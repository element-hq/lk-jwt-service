// Copyright 2025 Element Creations Ltd.
// Copyright 2023 - 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// handler.go: the HTTP entry point of the service.
//
// Handler owns the actor-model jobs map (only loop() reads/writes it),
// routes incoming requests, generates LiveKit JWTs, and delegates to the
// delayed-event manager for leave-event handling.  This file also contains
// LiveKitAuth (Handler's auth bundle) and getJoinToken (the JWT minter)
// because they only make sense as Handler's internals.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"

	"github.com/matrix-org/gomatrix"
)

type LiveKitAuth struct {
	key          string
	secret       string
	authProvider *auth.SimpleKeyProvider
	lkUrl        string
}

// writeMatrixError writes a Matrix-style error response to the HTTP response writer.
func writeMatrixError(w http.ResponseWriter, status int, errCode string, errMsg string) {
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(gomatrix.RespError{
		ErrCode: errCode,
		Err:     errMsg,
	}); err != nil {
		slog.Error("Handler: failed to encode json error message!", "err", err)
	}
}

func getJoinToken(apiKey string, apiSecret string, room LiveKitRoomAlias, identity LiveKitIdentity) (string, error) {
	at := auth.NewAccessToken(apiKey, apiSecret)

	canPublish := true
	canSubscribe := true
	grant := &auth.VideoGrant{
		RoomJoin:     true,
		RoomCreate:   false,
		CanPublish:   &canPublish,
		CanSubscribe: &canSubscribe,
		Room:         string(room),
	}

	at.SetVideoGrant(grant).
		SetIdentity(string(identity)).
		SetValidFor(time.Hour)

	return at.ToJWT()
}

// ── Handler ──────────────────────────────────────────────────────────────────

// Handler is the top-level HTTP handler.
//
// # Concurrency model
//
// Handler.loop() is the single goroutine that owns the jobs map — no mutex
// needed.  HTTP handler goroutines communicate with loop() exclusively through
// channels:
//
//   - addJobCh:   deliver new DelayedEventJobParams to loop(), which creates a
//     DelayedEventJob and starts its participant-lookup goroutine.
//   - sfuEventCh: deliver an SFU webhook event to loop(), which routes it
//     directly to the correct job by (room, identity) key.
//   - jobDoneCh:  jobs signal loop() when they enter a terminal state so loop()
//     can cancel and clean them up.
//
// Because all map mutations happen in a single goroutine there are no data
// races and no mutex is required.
type Handler struct {
	ctx                   context.Context
	cancel                context.CancelFunc
	liveKitAuth           LiveKitAuth
	fullAccessHomeservers []string
	skipVerifyTLS         bool
	// sanityCheckInterval is the period between room-worker sanity checks.
	// Zero disables the sanity check.
	sanityCheckInterval time.Duration
	// loopDone is closed when loop() has exited.
	loopDone   chan struct{}
	jobDoneCh  chan *DelayedEventJob
	addJobCh   chan addJobRequest
	sfuEventCh chan sfuEventRequest
}

// addJobRequest is sent by HTTP handlers to loop() to add a delayed-event job.
type addJobRequest struct {
	params DelayedEventJobParams
	// result receives the outcome; buffered so loop() never blocks.
	result chan addJobResult
}

type addJobResult struct {
	jobId UniqueID
	ok    bool
}

// sfuEventRequest is sent by handleSfuWebhook to loop() for routing.
type sfuEventRequest struct {
	roomAlias LiveKitRoomAlias
	msg       SFUMessage
}

func NewHandler(lkAuth LiveKitAuth, skipVerifyTLS bool, fullAccessHomeservers []string, sanityCheckInterval time.Duration) *Handler {
	ctx, cancel := context.WithCancel(context.Background())
	h := &Handler{
		ctx:                   ctx,
		cancel:                cancel,
		liveKitAuth:           lkAuth,
		skipVerifyTLS:         skipVerifyTLS,
		fullAccessHomeservers: fullAccessHomeservers,
		sanityCheckInterval:   sanityCheckInterval,
		loopDone:              make(chan struct{}),
		jobDoneCh:             make(chan *DelayedEventJob, 10),
		addJobCh:              make(chan addJobRequest),
		sfuEventCh:            make(chan sfuEventRequest, 200),
	}
	go h.loop()
	return h
}

// loop is the sole owner of the jobs map — no locking needed.
func (h *Handler) loop() {
	defer close(h.loopDone)

	jobs := make(map[jobKey]*DelayedEventJob)

	// loopWg tracks all job.loop() goroutines spawned by loop().
	// loopWg.Wait() in the ctx.Done() teardown path ensures no goroutine still
	// references global function variables (e.g. LiveKitListParticipants,
	// ExecuteDelayedEventAction) after Handler.Close() returns, which is
	// required for test safety.  Participant-lookup goroutines are tracked by
	// job.backgroundWg and are therefore also waited on (loop() calls backgroundWg.Wait()
	// before it returns).
	var loopWg sync.WaitGroup

	for {
		select {
		case <-h.ctx.Done():
			slog.Debug("Handler: loop exiting")
			// Cancel all job contexts first — unblocks goroutines waiting on
			// jobDoneCh so they take the ctx.Done() path and exit promptly.
			for _, job := range jobs {
				job.Stop()
			}
		drainDone: // Label the loop so we can break out of it from within the select block.
			// Drain any buffered terminal signals so no goroutine stays blocked
			// trying to send on the now-dead loop.
			for {
				select {
				case <-h.jobDoneCh:
				default:
					// Channel is empty. Break the for-loop, not just the select
					break drainDone
				}
			}
			// Wait for all job loop() goroutines to exit before returning.
			loopWg.Wait()
			return

		// ── add job ──────────────────────────────────────────────────────────
		case req := <-h.addJobCh:
			key := jobKey{Room: req.params.LiveKitRoom, Identity: req.params.LiveKitIdentity}

			job, err := NewDelayedEventJob(h.ctx, req.params, h.jobDoneCh)
			if err != nil {
				slog.Error("Handler: failed to create delayed event job",
					"room", req.params.LiveKitRoom,
					"lkId", req.params.LiveKitIdentity, "err", err)
				req.result <- addJobResult{ok: false}
				continue
			}

			if existing, ok := jobs[key]; ok {
				slog.Info("Handler: replacing existing job",
					"room", key.Room, "lkId", key.Identity,
					"oldJobId", existing.JobId, "newJobId", job.JobId)
				select {
				case existing.EventChannel <- JobReplaced:
				default:
				}
				existing.Stop()
				// No Close() goroutine needed — existing.loop() exits when its
				// context is cancelled; loopWg.Wait() handles the rest.
			}

			jobs[key] = job
			loopWg.Add(1)
			go func() {
				defer loopWg.Done()
				job.loop()
			}()
			// Pull-based lookup (additionally to SFU webhook)
			// Phase 1:
			// - required for `handleMembershipLeaveDelegation` as no SFU webhook is
			//   expected for this code path
			// - safeguard in case of `processLegacySFURequest` and `processSFURequest`
			//   to minimize impact of transient SFU webhook failures
			//
			// Phase 2 (if enabled: sanityCheckInterval > 0 seconds):
			// - Adds periodic lookups to ensure the participant is still present on the SFU,
			//   and cancels the job if not.
			// - Mitigates the risk of "zombie" jobs that never receive the SFU disconnect webhook
			//   (e.g. due transient SFU webhook failures)
			startParticipantLookup(job, h.liveKitAuth, h.sanityCheckInterval)
			slog.Debug("Handler: job created",
				"room", key.Room, "lkId", key.Identity, "jobId", job.JobId)
			req.result <- addJobResult{jobId: job.JobId, ok: true}

		// ── SFU webhook routing ──────────────────────────────────────────────
		case ev := <-h.sfuEventCh:
			key := jobKey{Room: ev.roomAlias, Identity: ev.msg.LiveKitIdentity}
			if job, ok := jobs[key]; ok {
				switch ev.msg.Type {
				case ParticipantConnected, ParticipantDisconnectedIntentionally,
					ParticipantConnectionAborted, SFUParticipantGone:
					select {
					case job.EventChannel <- ev.msg.Type:
					case <-job.ctx.Done():
					}
				default:
					slog.Warn("Handler: unexpected SFU event type",
						"type", ev.msg.Type, "room", ev.roomAlias, "lkId", ev.msg.LiveKitIdentity)
				}
			}

		// ── job lifecycle ─────────────────────────────────────────────────────
		case doneJob := <-h.jobDoneCh:
			key := jobKey{Room: doneJob.LiveKitRoom, Identity: doneJob.LiveKitIdentity}
			current, ok := jobs[key]
			if !ok || current != doneJob {
				// Stale signal from a replaced job — pointer equality guards this.
				slog.Debug("Handler: ignoring stale doneCh signal",
					"room", key.Room, "lkId", key.Identity, "jobId", doneJob.JobId)
				break
			}
			slog.Info("Handler: job done, cleaning up",
				"room", key.Room, "lkId", key.Identity, "jobId", doneJob.JobId)
			delete(jobs, key)
			doneJob.Stop()
			// No Close() goroutine needed — doneJob.loop() exits on its own;
			// loopWg.Wait() handles cleanup at shutdown.
		}
	}
}

// Close shuts down the handler and waits for loop() to exit.
func (h *Handler) Close() {
	h.cancel()
	select {
	case <-h.loopDone:
	case <-time.After(10 * time.Second):
		slog.Warn("Handler: Close() timed out")
	}
}

// addDelayedEventJob sends a set of job params to loop() and waits for the result.
// loop() performs the monitor lookup and HandoverJob atomically.
func (h *Handler) addDelayedEventJob(p DelayedEventJobParams) {
	slog.Debug("Handler: adding delayed event job",
		"room", p.LiveKitRoom,
		"lkId", p.LiveKitIdentity,
		"DelayId", p.DelayId)

	result := make(chan addJobResult, 1)
	select {
	case h.addJobCh <- addJobRequest{params: p, result: result}:
	case <-h.ctx.Done():
		slog.Warn("Handler: addDelayedEventJob called after shutdown",
			"room", p.LiveKitRoom)
		return
	}
	res := <-result
	if !res.ok {
		slog.Error("Handler: job handover failed",
			"room", p.LiveKitRoom,
			"lkId", p.LiveKitIdentity,
			"jobId", res.jobId)
	}
}

func (h *Handler) isFullAccessUser(matrixServerName string) bool {
	// Grant full access if wildcard '*' is present as the only entry
	if len(h.fullAccessHomeservers) == 1 && h.fullAccessHomeservers[0] == "*" {
		return true
	}

	// Check if the matrixServerName is in the list of full-access homeservers
	return slices.Contains(h.fullAccessHomeservers, matrixServerName)
}

// Deprecated: processLegacySFURequest serves the pre-Matrix-2.0 /sfu/get
// endpoint.  Remove once all in-the-wild clients have migrated to /get_token.
func (h *Handler) processLegacySFURequest(r *http.Request, req *LegacySFURequest) (*SFUResponse, error) {
	userInfo, err := exchangeOpenIdUserInfo(r.Context(), req.OpenIDToken, h.skipVerifyTLS)
	if err != nil {
		return nil, &MatrixErrorResponse{
			Status:  http.StatusInternalServerError,
			ErrCode: "M_LOOKUP_FAILED",
			Err:     "Failed to look up user info from homeserver",
		}
	}

	isFullAccessUser := h.isFullAccessUser(req.OpenIDToken.MatrixServerName)
	delayedEventDelegationRequested := req.DelayId != ""

	if delayedEventDelegationRequested && !isFullAccessUser {
		return nil, &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "Delegation of delayed events is only supported for full access users",
		}
	}

	slog.Debug("Handler: got Matrix user info",
		"userInfo.Sub", userInfo.Sub,
		"access", map[bool]string{true: "full access", false: "restricted access"}[isFullAccessUser])

	lkIdentity := LiveKitIdentity(userInfo.Sub + ":" + req.DeviceID)
	slotId := "m.call#ROOM"
	lkRoomAlias := LiveKitRoomAliasFor(req.Room, slotId)

	token, err := getJoinToken(h.liveKitAuth.key, h.liveKitAuth.secret, lkRoomAlias, lkIdentity)
	if err != nil {
		return nil, &MatrixErrorResponse{
			Status:  http.StatusInternalServerError,
			ErrCode: "M_UNKNOWN",
			Err:     "Internal Server Error",
		}
	}

	if isFullAccessUser {
		if err := CreateLiveKitRoom(r.Context(), &h.liveKitAuth, lkRoomAlias, userInfo.Sub, lkIdentity); err != nil {
			return nil, &MatrixErrorResponse{
				Status:  http.StatusInternalServerError,
				ErrCode: "M_UNKNOWN",
				Err:     "Unable to create room on SFU",
			}
		}
		if delayedEventDelegationRequested {
			slog.Info("Handler: scheduling delayed event job",
				"room", lkRoomAlias, "lkId", lkIdentity,
				"DelayId", req.DelayId, "CsApiUrl", req.DelayCsApiUrl)
			h.addDelayedEventJob(DelayedEventJobParams{
				CsApiUrl:        req.DelayCsApiUrl,
				DelayId:         req.DelayId,
				DelayTimeout:    time.Duration(req.DelayTimeout) * time.Millisecond,
				LiveKitRoom:     lkRoomAlias,
				LiveKitIdentity: lkIdentity,
			})
		}

	}

	slog.Info("Handler: generated Legacy SFU access token",
		"matrixId", userInfo.Sub,
		"ClaimedDeviceID", req.DeviceID,
		"access", map[bool]string{true: "full", false: "restricted"}[isFullAccessUser],
		"MatrixRoom", req.Room,
		"lkId", lkIdentity,
		"room", lkRoomAlias,
		"RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"))

	return &SFUResponse{URL: h.liveKitAuth.lkUrl, JWT: token}, nil
}

func (h *Handler) processSFURequest(r *http.Request, req *SFURequest) (*SFUResponse, error) {
	userInfo, err := exchangeOpenIdUserInfo(r.Context(), req.OpenIDToken, h.skipVerifyTLS)
	if err != nil {
		return nil, &MatrixErrorResponse{
			Status:  http.StatusUnauthorized,
			ErrCode: "M_UNAUTHORIZED",
			Err:     "The request could not be authorised.",
		}
	}

	if req.Member.ClaimedUserID != userInfo.Sub {
		slog.Warn("Handler: ClaimedUserID does not match token subject",
			"ClaimedUserID", req.Member.ClaimedUserID, "userInfo.Sub", userInfo.Sub)
		return nil, &MatrixErrorResponse{
			Status:  http.StatusUnauthorized,
			ErrCode: "M_UNAUTHORIZED",
			Err:     "The request could not be authorised.",
		}
	}

	isFullAccessUser := h.isFullAccessUser(req.OpenIDToken.MatrixServerName)
	delayedEventDelegationRequested := req.DelayId != ""

	if delayedEventDelegationRequested && !isFullAccessUser {
		return nil, &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "Delegation of delayed events is only supported for full access users",
		}
	}

	slog.Debug("Handler: got Matrix user info",
		"userInfo.Sub", userInfo.Sub,
		"access", map[bool]string{true: "full access", false: "restricted access"}[isFullAccessUser])

	lkIdentity := LiveKitIdentityFor(userInfo.Sub, req.Member.ClaimedDeviceID, req.Member.ID)
	lkRoomAlias := LiveKitRoomAliasFor(req.RoomID, req.SlotID)

	token, err := getJoinToken(h.liveKitAuth.key, h.liveKitAuth.secret, lkRoomAlias, lkIdentity)
	if err != nil {
		slog.Error("Handler: error getting LiveKit token", "userInfo.Sub", userInfo.Sub, "err", err)
		return nil, &MatrixErrorResponse{
			Status:  http.StatusInternalServerError,
			ErrCode: "M_UNKNOWN",
			Err:     "Internal Server Error",
		}
	}

	if isFullAccessUser {
		if err := CreateLiveKitRoom(r.Context(), &h.liveKitAuth, lkRoomAlias, userInfo.Sub, lkIdentity); err != nil {
			return nil, &MatrixErrorResponse{
				Status:  http.StatusInternalServerError,
				ErrCode: "M_UNKNOWN",
				Err:     "Unable to create room on SFU",
			}
		}

		if delayedEventDelegationRequested {
			slog.Info("Handler: scheduling delayed event job",
				"room", lkRoomAlias, "lkId", lkIdentity,
				"DelayId", req.DelayId, "CsApiUrl", req.DelayCsApiUrl)
			h.addDelayedEventJob(DelayedEventJobParams{
				CsApiUrl:        req.DelayCsApiUrl,
				DelayId:         req.DelayId,
				DelayTimeout:    time.Duration(req.DelayTimeout) * time.Millisecond,
				LiveKitRoom:     lkRoomAlias,
				LiveKitIdentity: lkIdentity,
			})
		}
	}

	slog.Info("Handler: generated SFU access token",
		"matrixId", userInfo.Sub,
		"ClaimedDeviceID", req.Member.ClaimedDeviceID,
		"access", map[bool]string{true: "full", false: "restricted"}[isFullAccessUser],
		"MatrixRoom", req.RoomID,
		"MatrixRTCSlot", req.SlotID,
		"lkId", lkIdentity,
		"room", lkRoomAlias,
		"RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"))

	return &SFUResponse{URL: h.liveKitAuth.lkUrl, JWT: token}, nil
}

// processMembershipLeaveDelegation handles the /membership_leave_delegation endpoint.
//
// Unlike processSFURequest it:
//   - Does NOT issue a JWT (the client is already connected to the SFU).
//   - Does NOT call CreateLiveKitRoom (the room already exists).
//   - Requires all three delayed-event parameters (they are mandatory here).
//
// The participant is assumed to be already present on the SFU. As the
// ParticipantConnected SFU webhook has already happened, the participant-lookup
// goroutine (startParticipantLookup) will use its
// backoff to confirm presence.
func (h *Handler) processMembershipLeaveDelegation(r *http.Request, req *MembershipLeaveDelegationRequest) error {
	userInfo, err := exchangeOpenIdUserInfo(r.Context(), req.OpenIDToken, h.skipVerifyTLS)
	if err != nil {
		return &MatrixErrorResponse{
			Status:  http.StatusUnauthorized,
			ErrCode: "M_UNAUTHORIZED",
			Err:     "The request could not be authorised.",
		}
	}

	if req.Member.ClaimedUserID != userInfo.Sub {
		slog.Warn("Handler: membership_leave_delegation: ClaimedUserID does not match token subject",
			"ClaimedUserID", req.Member.ClaimedUserID, "userInfo.Sub", userInfo.Sub)
		return &MatrixErrorResponse{
			Status:  http.StatusUnauthorized,
			ErrCode: "M_UNAUTHORIZED",
			Err:     "The request could not be authorised.",
		}
	}

	// Delayed event delegation is restricted to full-access homeservers.
	if !h.isFullAccessUser(req.OpenIDToken.MatrixServerName) {
		return &MatrixErrorResponse{
			Status:  http.StatusForbidden,
			ErrCode: "M_FORBIDDEN",
			Err:     "Delegation of delayed events is only supported for full access users",
		}
	}

	lkIdentity := LiveKitIdentityFor(userInfo.Sub, req.Member.ClaimedDeviceID, req.Member.ID)
	lkRoomAlias := LiveKitRoomAliasFor(req.RoomID, req.SlotID)

	slog.Info("Handler: scheduling delayed event job (membership_leave_delegation)",
		"room", lkRoomAlias, "lkId", lkIdentity,
		"DelayId", req.DelayId, "CsApiUrl", req.DelayCsApiUrl,
		"RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"))

	h.addDelayedEventJob(DelayedEventJobParams{
		CsApiUrl:        req.DelayCsApiUrl,
		DelayId:         req.DelayId,
		DelayTimeout:    time.Duration(req.DelayTimeout) * time.Millisecond,
		LiveKitRoom:     lkRoomAlias,
		LiveKitIdentity: lkIdentity,
	})

	return nil
}

func (h *Handler) handleMembershipLeaveDelegation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST")
	w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token")

	switch r.Method {
	case "OPTIONS":
		w.WriteHeader(http.StatusOK)
		return
	case "POST":
		slog.Debug("Handler: membership_leave_delegation request",
			"RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"))

		var req MembershipLeaveDelegationRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			slog.Error("Handler: membership_leave_delegation: error reading body",
				"RemoteAddr", r.RemoteAddr, "err", err)
			writeMatrixError(w, http.StatusBadRequest, "M_NOT_JSON", "Error reading request")
			return
		}

		if err := req.Validate(); err != nil {
			matrixErr := &MatrixErrorResponse{}
			if errors.As(err, &matrixErr) {
				writeMatrixError(w, matrixErr.Status, matrixErr.ErrCode, matrixErr.Err)
			}
			return
		}

		if err := h.processMembershipLeaveDelegation(r, &req); err != nil {
			matrixErr := &MatrixErrorResponse{}
			if errors.As(err, &matrixErr) {
				writeMatrixError(w, matrixErr.Status, matrixErr.ErrCode, matrixErr.Err)
			}
			return
		}

		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *Handler) prepareMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/sfu/get", h.handle_legacy) // Deprecated: pre-Matrix-2.0; remove once clients migrate to /get_token.
	mux.HandleFunc("/get_token", h.handle)
	mux.HandleFunc("/membership_leave_delegation", h.handleMembershipLeaveDelegation)
	mux.HandleFunc("/sfu_webhook", h.handleSfuWebhook)
	mux.HandleFunc("/healthz", h.healthcheck)
	return mux
}

func (h *Handler) healthcheck(w http.ResponseWriter, r *http.Request) {
	slog.Info("Handler: health check", "RemoteAddr", r.RemoteAddr)

	if r.Method == "GET" || r.Method == "HEAD" {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// Deprecated: handle_legacy serves the pre-Matrix-2.0 /sfu/get endpoint.
// Remove once all in-the-wild clients have migrated to /get_token.
func (h *Handler) handle_legacy(w http.ResponseWriter, r *http.Request) {
	slog.Debug("Handler (legacy): new request",
		"RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"))

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST")
	w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token")

	switch r.Method {
	case "OPTIONS":
		w.WriteHeader(http.StatusOK)
		return
	case "POST":
		var req LegacySFURequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			slog.Error("Handler (legacy): error reading body",
				"RemoteAddr", r.RemoteAddr, "err", err)
			writeMatrixError(w, http.StatusBadRequest, "M_NOT_JSON", "Error reading request")
			return
		}

		if err := req.Validate(); err != nil {
			matrixErr := &MatrixErrorResponse{}
			if errors.As(err, &matrixErr) {
				writeMatrixError(w, matrixErr.Status, matrixErr.ErrCode, matrixErr.Err)
			}
			return
		}

		sfuAccessResponse, err := h.processLegacySFURequest(r, &req)
		if err != nil {
			matrixErr := &MatrixErrorResponse{}
			if errors.As(err, &matrixErr) {
				writeMatrixError(w, matrixErr.Status, matrixErr.ErrCode, matrixErr.Err)
			}
			return
		}

		if err := json.NewEncoder(w).Encode(&sfuAccessResponse); err != nil {
			slog.Error("Handler (legacy): failed to encode response",
				"RemoteAddr", r.RemoteAddr, "err", err)
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST")
	w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token")

	switch r.Method {
	case "OPTIONS":
		slog.Debug("Handler: preflight", "RemoteAddr", r.RemoteAddr)
		w.WriteHeader(http.StatusOK)
		return
	case "POST":
		slog.Debug("Handler: new request", "RemoteAddr", r.RemoteAddr)
		var sfuAccessRequest SFURequest

		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&sfuAccessRequest); err != nil {
			slog.Error("Handler: error reading body",
				"RemoteAddr", r.RemoteAddr, "err", err)
			writeMatrixError(w, http.StatusBadRequest, "M_NOT_JSON", "Error reading request")
			return
		}

		if err := sfuAccessRequest.Validate(); err != nil {
			matrixErr := &MatrixErrorResponse{}
			if errors.As(err, &matrixErr) {
				writeMatrixError(w, matrixErr.Status, matrixErr.ErrCode, matrixErr.Err)
			}
			return
		}

		sfuAccessResponse, err := h.processSFURequest(r, &sfuAccessRequest)
		if err != nil {
			matrixErr := &MatrixErrorResponse{}
			if errors.As(err, &matrixErr) {
				writeMatrixError(w, matrixErr.Status, matrixErr.ErrCode, matrixErr.Err)
			}
			return
		}

		if err := json.NewEncoder(w).Encode(&sfuAccessResponse); err != nil {
			slog.Error("Handler: failed to encode response",
				"RemoteAddr", r.RemoteAddr, "err", err)
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// sfuEventFromWebhook translates a validated LiveKit webhook event into an
// (roomAlias, SFUMessage) pair.  Returns ok=false for event types that do
// not require routing (e.g. room events, unknown types).
func sfuEventFromWebhook(event *livekit.WebhookEvent) (LiveKitRoomAlias, SFUMessage, bool) {
	// https://docs.livekit.io/intro/basics/rooms-participants-tracks/webhooks-events/#webhook-events
	roomAlias := LiveKitRoomAlias(event.Room.Name)
	switch event.Event {
	case "participant_joined":
		slog.Debug("Handler: SFU participant joined",
			"lkId", event.Participant.Identity, "room", event.Room.Name)
		return roomAlias, SFUMessage{
			Type:            ParticipantConnected,
			LiveKitIdentity: LiveKitIdentity(event.Participant.Identity),
		}, true

	case "participant_left", "participant_connection_aborted":
		msgType := ParticipantConnectionAborted
		if event.Participant.DisconnectReason == livekit.DisconnectReason_CLIENT_INITIATED {
			msgType = ParticipantDisconnectedIntentionally
		}
		slog.Debug("Handler: SFU participant left",
			"lkId", event.Participant.Identity, "room", event.Room.Name,
			"DisconnectReason", event.Participant.DisconnectReason)
		return roomAlias, SFUMessage{
			Type:            msgType,
			LiveKitIdentity: LiveKitIdentity(event.Participant.Identity),
		}, true

	default:
		return "", SFUMessage{}, false
	}
}

func (h *Handler) handleSfuWebhook(w http.ResponseWriter, r *http.Request) {
	event, err := webhook.ReceiveWebhookEvent(r, h.liveKitAuth.authProvider)
	if err != nil {
		slog.Warn("Handler: SFU webhook error", "err", err)
		return
	}

	roomAlias, msg, ok := sfuEventFromWebhook(event)
	if !ok {
		return
	}

	// Route via loop() so the map is accessed by a single goroutine only.
	select {
	case h.sfuEventCh <- sfuEventRequest{roomAlias: roomAlias, msg: msg}:
	case <-h.ctx.Done():
	}
}

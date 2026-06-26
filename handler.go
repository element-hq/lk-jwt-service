// Copyright 2025 Element Creations Ltd.
// Copyright 2023 - 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// handler.go: the HTTP entry point of the service.
//
// Handler owns the actor-model jobs map (only loop() reads/writes it),
// routes incoming requests, generates LiveKit JWTs, and delegates to the
// delayed-event manager for leave-event handling. This file also contains
// LiveKitAuth (Handler's auth bundle) and getJoinToken (the JWT minter)
// because they only make sense as Handler's internals.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// writeIfMatrixError unwraps err as a *MatrixErrorResponse and writes the
// corresponding HTTP response; returns true if it wrote. Non-Matrix errors
// are silently dropped (response unwritten) — callers always return after
// calling this regardless of the bool.
func writeIfMatrixError(w http.ResponseWriter, err error) bool {
	var mErr *MatrixErrorResponse
	if errors.As(err, &mErr) {
		writeMatrixError(w, mErr.Status, mErr.ErrCode, mErr.Err)
		return true
	}
	return false
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
// needed. HTTP handler goroutines communicate with loop() exclusively through
// channels:
//
//   - addJobCh: deliver new DelayedEventJobParams to loop(), which creates a
//     DelayedEventJob and starts its participant-lookup goroutine.
//   - sfuEventCh: deliver an SFU webhook event to loop(), which routes it
//     directly to the correct job by (room, identity) key.
//   - jobDoneCh: jobs signal loop() when they enter a terminal state so loop()
//     can cancel and clean them up.
//   - jobRestartedCh: jobs signal loop() when they restart the delayed event
//     so loop() can update the stored job.
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
	csApiUrlOverrides   map[string]CsApiUrl
	csApiUrlCache       *csApiUrlCache
	store               store
	// loopDone is closed when loop() has exited.
	loopDone       chan struct{}
	jobDoneCh      chan *DelayedEventJob
	jobRestartedCh chan jobRestartedRequest
	addJobCh       chan addJobRequest
	sfuEventCh     chan sfuEventRequest
}

// addJobRequest is sent by HTTP handlers to loop() to add a delayed-event job.
type addJobRequest struct {
	params DelayedEventJobParams
	// result receives the outcome; buffered so loop() never blocks.
	result chan addJobResult
}

type addJobResult struct {
	jobId UniqueID
	err   error
}

type jobRestartedRequest struct {
	params      DelayedEventJobParams
	restartedAt time.Time
}

// sfuEventRequest is sent by handleSfuWebhook to loop() for routing.
type sfuEventRequest struct {
	roomAlias LiveKitRoomAlias
	msg       SFUMessage
}

var errStorePersistFailed = errors.New("store: failed to persist job")

func NewHandler(lkAuth LiveKitAuth, skipVerifyTLS bool, fullAccessHomeservers []string, sanityCheckInterval time.Duration, csApiUrlOverrides map[string]CsApiUrl, store store) *Handler {
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
		jobRestartedCh:        make(chan jobRestartedRequest, 10),
		addJobCh:              make(chan addJobRequest),
		sfuEventCh:            make(chan sfuEventRequest, 200),
		csApiUrlOverrides:     csApiUrlOverrides,
		csApiUrlCache:         newCsApiUrlCache(),
		store:                 store,
	}
	go h.loop()
	return h
}

// loop is the Handler's actor goroutine: the sole owner of the jobs map
// (no locking needed) and the single consumer of
//
//   - addJobCh: job creation, replacing any active job for the same key
//   - sfuEventCh: routing SFU webhook events to the job for (room, identity)
//   - jobDoneCh: cleaning up jobs that reached a terminal state
//   - jobRestartedCh: updating the stored job when the delayed event is restarted
//
// Started once by NewHandler; runs until h.ctx is cancelled (Handler.Close),
// then joins all job goroutines and closes h.loopDone.
func (h *Handler) loop() {
	defer close(h.loopDone)

	jobs := make(map[jobKey]*DelayedEventJob)

	// Concurrency model of this loop:
	//
	//   - loopWg: the single join point for all concurrency rooted in this loop
	//       - directly: each job.loop() goroutine
	//       - transitively:
	//           - the participant-lookup, ActionRestart and ActionSend goroutines
	//           - they funnel into job.backgroundWg, which job.loop() drains
	//             before returning
	//   - job teardown (replace / job done / shutdown) always via async
	//     job.Stop() as this actor must never block.
	//       - job.Stop() signals shutdown and is never directly acknowledged,
	//         job completion surfaces only via the transitive loopWg accounting
	//   - loop shutdown:
	//       - async via h.ctx cancellation: this loop then runs
	//         cancel all jobs → drain jobDoneCh → loopWg.Wait() → close
	//         loopDone
	//       - blocking via Handler.Close() (useful for tests)
	//          - cancels h.ctx and
	//          - waits for loopDone
	//         Note: after it returns, no goroutine still references
	//         swappable function pointers (e.g. LiveKitParticipantExists,
	//         ExecuteDelayedEventAction)
	var loopWg sync.WaitGroup

	// Load any existing jobs from the store.
	if storedJobs, err := h.store.allJobs(h.ctx); err != nil {
		slog.Error("Handler: failed to load stored jobs", "err", err)
	} else {
		for _, storedJob := range storedJobs {
			key := jobKey{Room: storedJob.Params.LiveKitRoom, Identity: storedJob.Params.LiveKitIdentity}

			// Check if the job's delay timeout has already exceeded. If so, delete it from the store.
			remaining := time.Until(storedJob.RestartedAt.Add(storedJob.Params.DelayTimeout))
			if remaining <= 0 {
				slog.Info("Handler: skipping expired stored job", "lkId", key.Identity)
				if err := h.store.deleteJob(h.ctx, key.Identity); err != nil {
					slog.Error("Handler: failed to delete expired stored job", "lkId", key.Identity, "err", err)
				}
				continue
			}

			// TODO johannes: verify that delayed event still exists?
			// TODO johannes: avoid waiting up to one hour for the participant to connect?

			// Create a new job.
			job, err := NewDelayedEventJob(h.ctx, storedJob.Params, func(ctx context.Context, serverName string) (CsApiUrl, error) {
				return resolveCsApiUrl(ctx, serverName, h.csApiUrlOverrides, h.csApiUrlCache)
			}, h.jobDoneCh, h.jobRestartedCh)

			// Gracefully ignore job creation failures but leave the job in the store for
			// a potential future restart.
			if err != nil {
				slog.Error("Handler: failed to create delayed event job from stored job",
					"room", storedJob.Params.LiveKitRoom, "lkId", storedJob.Params.LiveKitIdentity, "err", err)
				continue
			}

			// Store the job in the in-memory map and kick off its loop.
			jobs[key] = job
			startParticipantLookup(job, h.liveKitAuth, h.sanityCheckInterval)
			loopWg.Add(1)
			go func() {
				defer loopWg.Done()
				job.loop()
			}()
			slog.Debug("Handler: job created from stored job",
				"room", key.Room, "lkId", key.Identity, "jobId", job.JobId)
		}
	}

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

			// Create a new job.
			job, err := NewDelayedEventJob(h.ctx, req.params, func(ctx context.Context, serverName string) (CsApiUrl, error) {
				return resolveCsApiUrl(ctx, serverName, h.csApiUrlOverrides, h.csApiUrlCache)
			}, h.jobDoneCh, h.jobRestartedCh)
			if err != nil {
				slog.Error("Handler: failed to create delayed event job",
					"room", req.params.LiveKitRoom,
					"lkId", req.params.LiveKitIdentity, "err", err)
				req.result <- addJobResult{err: err}
				continue
			}

			// Save the job in the store. We assume the delayed event was restarted
			// just before the request came in because that is our best guess.
			storedJob := storedJob{Params: req.params, RestartedAt: time.Now()}
			if err := h.store.saveJob(h.ctx, key.Identity, storedJob); err != nil {
				slog.Error("Handler: failed to store job",
					"room", key.Room, "lkId", key.Identity, "err", err)
				req.result <- addJobResult{err: fmt.Errorf("%w: %w", errStorePersistFailed, err)}
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

			// Pull-based lookup (additionally to SFU webhook)
			// Phase 1:
			// - required for `handleDelegateDelayedLeave` as no SFU webhook is
			//   expected for this code path
			// - safeguard in case of `processLegacySFURequest` and `processSFURequest`
			//   to minimize impact of transient SFU webhook failures
			//
			// Phase 2 (if enabled: sanityCheckInterval > 0 seconds):
			// - Adds periodic lookups to ensure the participant is still present on the SFU,
			//   and cancels the job if not.
			// - Mitigates the risk of "zombie" jobs that never receive the SFU disconnect webhook
			//   (e.g. due transient SFU webhook failures)

			// Started before loop() so that its backgroundWg.Add happens before
			// loop() can reach backgroundWg.Wait.  Events it emits early are
			// buffered by EventChannel until loop() starts consuming.
			startParticipantLookup(job, h.liveKitAuth, h.sanityCheckInterval)

			loopWg.Add(1)
			go func() {
				defer loopWg.Done()
				job.loop()
			}()
			slog.Debug("Handler: job created",
				"room", key.Room, "lkId", key.Identity, "jobId", job.JobId)
			req.result <- addJobResult{jobId: job.JobId}

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
				slog.Debug("Handler: ignoring stale jobDoneCh signal",
					"room", key.Room, "lkId", key.Identity, "jobId", doneJob.JobId)
				break
			}
			slog.Info("Handler: job done, cleaning up",
				"room", key.Room, "lkId", key.Identity, "jobId", doneJob.JobId)
			delete(jobs, key)
			if err := h.store.deleteJob(h.ctx, key.Identity); err != nil {
				slog.Error("Handler: failed to delete persisted job",
					"room", key.Room, "lkId", key.Identity, "err", err)
			}
			doneJob.Stop()
			// No Close() goroutine needed — doneJob.loop() exits on its own;
			// loopWg.Wait() handles cleanup at shutdown.
		case req := <-h.jobRestartedCh:
			// Save the job in the store.
			key := jobKey{Room: req.params.LiveKitRoom, Identity: req.params.LiveKitIdentity}
			storedJob := storedJob{Params: req.params, RestartedAt: req.restartedAt}
			if err := h.store.saveJob(h.ctx, key.Identity, storedJob); err != nil {
				slog.Error("Handler: failed to update stored job",
					"room", key.Room, "lkId", key.Identity, "err", err)
			}
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

// addDelayedEventJob sends a set of job params to loop() and waits for the
// result. It returns:
//   - nil on success,
//   - the job-creation error on failure,
//   - or context.Canceled when the handler has already shut down.
func (h *Handler) addDelayedEventJob(p DelayedEventJobParams) error {
	slog.Debug("Handler: adding delayed event job",
		"room", p.LiveKitRoom,
		"lkId", p.LiveKitIdentity,
		"delayId", p.DelayId)

	result := make(chan addJobResult, 1)
	select {
	case h.addJobCh <- addJobRequest{params: p, result: result}:
	case <-h.ctx.Done():
		slog.Warn("Handler: addDelayedEventJob called after shutdown",
			"room", p.LiveKitRoom)
		return h.ctx.Err()
	}
	res := <-result
	if res.err != nil {
		slog.Error("Handler: job handover failed",
			"room", p.LiveKitRoom,
			"lkId", p.LiveKitIdentity,
			"err", res.err)
		return res.err
	}
	return nil
}

// matrixErrorForAddJob maps an addDelayedEventJob failure to the client
// response:
//   - shutdown → 503 M_UNKNOWN,
//   - store failure → 503 M_UNKNOWN,
//   - invalid job params → 400 M_BAD_JSON.
func matrixErrorForAddJob(err error) *MatrixErrorResponse {
	if errors.Is(err, context.Canceled) {
		return &MatrixErrorResponse{
			Status:  http.StatusServiceUnavailable,
			ErrCode: "M_UNKNOWN",
			Err:     "Service is shutting down",
		}
	}
	if errors.Is(err, errStorePersistFailed) {
		return &MatrixErrorResponse{
			Status:  http.StatusServiceUnavailable,
			ErrCode: "M_UNKNOWN",
			Err:     "Failed to persist delayed event",
		}
	}
	return &MatrixErrorResponse{
		Status:  http.StatusBadRequest,
		ErrCode: "M_BAD_JSON",
		Err:     err.Error(),
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

func (h *Handler) verifyOpenIDToken(ctx context.Context, token OpenIDTokenType, claimedUserID string) (string, error) {
	userInfo, err := exchangeOpenIdUserInfo(ctx, token, h.skipVerifyTLS)
	if err != nil {
		return "", &MatrixErrorResponse{
			Status:  http.StatusUnauthorized,
			ErrCode: "M_UNAUTHORIZED",
			Err:     "The request could not be authorised.",
		}
	}

	if claimedUserID != "" && claimedUserID != userInfo.Sub {
		slog.Warn("Handler: ClaimedUserID does not match token subject",
			"claimedUserId", claimedUserID, "matrixId", userInfo.Sub)
		return "", &MatrixErrorResponse{
			Status:  http.StatusUnauthorized,
			ErrCode: "M_UNAUTHORIZED",
			Err:     "The request could not be authorised.",
		}
	}

	return userInfo.Sub, nil
}

// Deprecated: processLegacySFURequest serves the pre-Matrix-2.0 /sfu/get
// endpoint. Remove once all in-the-wild clients have migrated to /get_token.
func (h *Handler) processLegacySFURequest(r *http.Request, req *LegacySFURequest) (*SFUResponse, error) {
	matrixID, err := h.verifyOpenIDToken(r.Context(), req.OpenIDToken, "")
	if err != nil {
		return nil, err
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
		"matrixId", matrixID,
		"access", map[bool]string{true: "full", false: "restricted"}[isFullAccessUser])

	lkIdentity := LiveKitIdentity(matrixID + ":" + req.DeviceID)
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
		// If delegation is requested, verify that we can resolve the Client-Server API and fail the request otherwise.
		// We do this before creating the LiveKit room so that we don't produce lingering rooms in the error case.
		//
		// TODO: This code is currently duplicated across the token endpoints and the delegation endpoint. This
		// will be resolved ones the already deprecated delay parameters are removed from the token endpoints.
		if delayedEventDelegationRequested {
			if url, _ := resolveCsApiUrl(r.Context(), req.OpenIDToken.MatrixServerName, h.csApiUrlOverrides, h.csApiUrlCache); url == "" {
				return nil, &MatrixErrorResponse{
					Status:  http.StatusBadRequest,
					ErrCode: "M_BAD_JSON",
					Err:     "Unable to resolve client-server API",
				}
			}
		}

		// Now create the LiveKit room.
		if err := CreateLiveKitRoom(r.Context(), &h.liveKitAuth, lkRoomAlias, matrixID, lkIdentity); err != nil {
			return nil, &MatrixErrorResponse{
				Status:  http.StatusInternalServerError,
				ErrCode: "M_UNKNOWN",
				Err:     "Unable to create room on SFU",
			}
		}
		if delayedEventDelegationRequested {
			slog.Info("Handler: scheduling delayed event job",
				"room", lkRoomAlias, "lkId", lkIdentity,
				"delayId", req.DelayId, "MatrixServerName", req.OpenIDToken.MatrixServerName)
			if err := h.addDelayedEventJob(DelayedEventJobParams{
				ServerName:      req.OpenIDToken.MatrixServerName,
				DelayId:         req.DelayId,
				DelayTimeout:    time.Duration(req.DelayTimeout) * time.Millisecond,
				LiveKitRoom:     lkRoomAlias,
				LiveKitIdentity: lkIdentity,
			}); err != nil {
				return nil, matrixErrorForAddJob(err)
			}
		}

	}

	slog.Info("Handler: generated Legacy SFU access token",
		"matrixId", matrixID,
		"claimedDeviceId", req.DeviceID,
		"access", map[bool]string{true: "full", false: "restricted"}[isFullAccessUser],
		"matrixRoom", req.Room,
		"lkId", lkIdentity,
		"room", lkRoomAlias,
		"RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"))

	return &SFUResponse{URL: h.liveKitAuth.lkUrl, JWT: token}, nil
}

func (h *Handler) processSFURequest(r *http.Request, req *SFURequest) (*SFUResponse, error) {
	matrixID, err := h.verifyOpenIDToken(r.Context(), req.OpenIDToken, req.Member.ClaimedUserID)
	if err != nil {
		return nil, err
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
		"matrixId", matrixID,
		"access", map[bool]string{true: "full", false: "restricted"}[isFullAccessUser])

	lkIdentity := LiveKitIdentityFor(matrixID, req.Member.ClaimedDeviceID, req.Member.ID)
	lkRoomAlias := LiveKitRoomAliasFor(req.RoomID, req.SlotID)

	token, err := getJoinToken(h.liveKitAuth.key, h.liveKitAuth.secret, lkRoomAlias, lkIdentity)
	if err != nil {
		slog.Error("Handler: error getting LiveKit token", "matrixId", matrixID, "err", err)
		return nil, &MatrixErrorResponse{
			Status:  http.StatusInternalServerError,
			ErrCode: "M_UNKNOWN",
			Err:     "Internal Server Error",
		}
	}

	if isFullAccessUser {
		// If delegation is requested, verify that we can resolve the Client-Server API and fail the request otherwise.
		// We do this before creating the LiveKit room so that we don't produce lingering rooms in the error case.
		//
		// TODO: This code is currently duplicated across the token endpoints and the delegation endpoint. This
		// will be resolved ones the already deprecated delay parameters are removed from the token endpoints.
		if delayedEventDelegationRequested {
			if url, _ := resolveCsApiUrl(r.Context(), req.OpenIDToken.MatrixServerName, h.csApiUrlOverrides, h.csApiUrlCache); url == "" {
				return nil, &MatrixErrorResponse{
					Status:  http.StatusBadRequest,
					ErrCode: "M_BAD_JSON",
					Err:     "Unable to resolve client-server API",
				}
			}
		}

		// Now create the LiveKit room.
		if err := CreateLiveKitRoom(r.Context(), &h.liveKitAuth, lkRoomAlias, matrixID, lkIdentity); err != nil {
			return nil, &MatrixErrorResponse{
				Status:  http.StatusInternalServerError,
				ErrCode: "M_UNKNOWN",
				Err:     "Unable to create room on SFU",
			}
		}

		if delayedEventDelegationRequested {
			slog.Info("Handler: scheduling delayed event job",
				"room", lkRoomAlias, "lkId", lkIdentity,
				"delayId", req.DelayId, "MatrixServerName", req.OpenIDToken.MatrixServerName)
			if err := h.addDelayedEventJob(DelayedEventJobParams{
				ServerName:      req.OpenIDToken.MatrixServerName,
				DelayId:         req.DelayId,
				DelayTimeout:    time.Duration(req.DelayTimeout) * time.Millisecond,
				LiveKitRoom:     lkRoomAlias,
				LiveKitIdentity: lkIdentity,
			}); err != nil {
				return nil, matrixErrorForAddJob(err)
			}
		}
	}

	slog.Info("Handler: generated SFU access token",
		"matrixId", matrixID,
		"claimedDeviceId", req.Member.ClaimedDeviceID,
		"access", map[bool]string{true: "full", false: "restricted"}[isFullAccessUser],
		"matrixRoom", req.RoomID,
		"MatrixRTCSlot", req.SlotID,
		"lkId", lkIdentity,
		"room", lkRoomAlias,
		"RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"))

	return &SFUResponse{URL: h.liveKitAuth.lkUrl, JWT: token}, nil
}

// processDelegateDelayedLeave handles the /delegate_delayed_leave endpoint.
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
func (h *Handler) processDelegateDelayedLeave(r *http.Request, req *DelegateDelayedLeaveRequest) (*DelegateDelayedLeaveResponse, error) {
	matrixID, err := h.verifyOpenIDToken(r.Context(), req.OpenIDToken, req.Member.ClaimedUserID)
	if err != nil {
		return nil, err
	}

	// Delayed event delegation is restricted to full-access homeservers.
	if !h.isFullAccessUser(req.OpenIDToken.MatrixServerName) {
		return nil, &MatrixErrorResponse{
			Status:  http.StatusForbidden,
			ErrCode: "M_FORBIDDEN",
			Err:     "Delegation of delayed events is only supported for full access users",
		}
	}

	lkIdentity := LiveKitIdentityFor(matrixID, req.Member.ClaimedDeviceID, req.Member.ID)
	lkRoomAlias := LiveKitRoomAliasFor(req.RoomID, req.SlotID)

	// Verify that the Client-Server API can be resolved and prime the cache.
	if url, _ := resolveCsApiUrl(r.Context(), req.OpenIDToken.MatrixServerName, h.csApiUrlOverrides, h.csApiUrlCache); url == "" {
		return nil, &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "Unable to resolve client-server API",
		}
	}

	slog.Info("Handler: scheduling delayed event job (delegate_delayed_leave)",
		"room", lkRoomAlias, "lkId", lkIdentity,
		"delayId", req.DelayId, "MatrixServerName", req.OpenIDToken.MatrixServerName,
		"RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"))

	if err := h.addDelayedEventJob(DelayedEventJobParams{
		ServerName:      req.OpenIDToken.MatrixServerName,
		DelayId:         req.DelayId,
		DelayTimeout:    time.Duration(req.DelayTimeout) * time.Millisecond,
		LiveKitRoom:     lkRoomAlias,
		LiveKitIdentity: lkIdentity,
	}); err != nil {
		return nil, matrixErrorForAddJob(err)
	}

	return &DelegateDelayedLeaveResponse{}, nil
}

func (h *Handler) handleDelegateDelayedLeave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	slog.Debug("Handler: delegate_delayed_leave request",
		"RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"))

	var req DelegateDelayedLeaveRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		slog.Error("Handler: delegate_delayed_leave: error reading body",
			"RemoteAddr", r.RemoteAddr, "err", err)
		writeMatrixError(w, http.StatusBadRequest, "M_NOT_JSON", "Error reading request")
		return
	}

	if err := req.Validate(); err != nil {
		writeIfMatrixError(w, err)
		return
	}

	response, err := h.processDelegateDelayedLeave(r, &req)
	if err != nil {
		writeIfMatrixError(w, err)
		return
	}

	if err := json.NewEncoder(w).Encode(&response); err != nil {
		slog.Error("Handler: delegate_delayed_leave: failed to encode response",
			"RemoteAddr", r.RemoteAddr, "err", err)
	}

	w.WriteHeader(http.StatusOK)
}

// corsJSON adds the four standard CORS + JSON headers used by every POST
// endpoint and short-circuits the CORS preflight (OPTIONS) with 200. Any
// other method falls through to next.
func corsJSON(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST")
		w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	}
}

func (h *Handler) prepareMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/sfu/get", corsJSON(h.handle_legacy)) // Deprecated: pre-Matrix-2.0; remove once clients migrate to /get_token.
	mux.HandleFunc("/get_token", corsJSON(h.handleGetToken))
	mux.HandleFunc("/delegate_delayed_leave", corsJSON(h.handleDelegateDelayedLeave))
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
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	slog.Debug("Handler (legacy): new request",
		"RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"))

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
		writeIfMatrixError(w, err)
		return
	}

	sfuAccessResponse, err := h.processLegacySFURequest(r, &req)
	if err != nil {
		writeIfMatrixError(w, err)
		return
	}

	if err := json.NewEncoder(w).Encode(&sfuAccessResponse); err != nil {
		slog.Error("Handler (legacy): failed to encode response",
			"RemoteAddr", r.RemoteAddr, "err", err)
	}
}

func (h *Handler) handleGetToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
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
		writeIfMatrixError(w, err)
		return
	}

	sfuAccessResponse, err := h.processSFURequest(r, &sfuAccessRequest)
	if err != nil {
		writeIfMatrixError(w, err)
		return
	}

	if err := json.NewEncoder(w).Encode(&sfuAccessResponse); err != nil {
		slog.Error("Handler: failed to encode response",
			"RemoteAddr", r.RemoteAddr, "err", err)
	}
}

// sfuEventFromWebhook translates a validated LiveKit webhook event into an
// (roomAlias, SFUMessage) pair. Returns ok=false for event types that do
// not require routing (e.g. room events, unknown types).
func sfuEventFromWebhook(event *livekit.WebhookEvent) (LiveKitRoomAlias, SFUMessage, bool) {
	// https://docs.livekit.io/intro/basics/rooms-participants-tracks/webhooks-events/#webhook-events
	roomAlias := LiveKitRoomAlias(event.Room.Name)
	switch event.Event {
	case webhook.EventParticipantJoined:
		slog.Debug("Handler: SFU participant joined",
			"lkId", event.Participant.Identity, "room", event.Room.Name)
		return roomAlias, SFUMessage{
			Type:            ParticipantConnected,
			LiveKitIdentity: LiveKitIdentity(event.Participant.Identity),
		}, true

	case webhook.EventParticipantLeft, webhook.EventParticipantConnectionAborted:
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

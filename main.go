// Copyright 2025 Element Creations Ltd.
// Copyright 2023 - 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MatusOllah/slogcolor"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"
	"github.com/mattn/go-isatty"

	"github.com/matrix-org/gomatrix"
)

type LiveKitAuth struct {
	key          string
	secret       string
	authProvider *auth.SimpleKeyProvider
	lkUrl        string
}

type RoomMonitorEvent int

const (
	NoJobsLeft RoomMonitorEvent = iota
)

type HandlerMessage struct {
	RoomAlias LiveKitRoomAlias
	Event     RoomMonitorEvent
	MonitorId UniqueID
}

type Config struct {
	Key                   string
	Secret                string
	LkUrl                 string
	SkipVerifyTLS         bool
	FullAccessHomeservers []string
	LkJwtBind             string
	// SanityCheckInterval is the period at which the participant-lookup
	// goroutine re-checks whether a connected participant is still present on
	// the SFU.  Zero (the default) disables the sanity check entirely.
	// Configure via LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS (unit: seconds).
	SanityCheckInterval   time.Duration
}

type MatrixRTCMemberType struct {
	ID              string `json:"id"`
	ClaimedUserID   string `json:"claimed_user_id"`
	ClaimedDeviceID string `json:"claimed_device_id"`
}

type OpenIDTokenType struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	MatrixServerName string `json:"matrix_server_name"`
	ExpiresIn        int    `json:"expires_in"`
}

type LegacySFURequest struct {
	Room        string          `json:"room"`
	OpenIDToken OpenIDTokenType `json:"openid_token"`
	DeviceID    string          `json:"device_id"`
}

type DelayedEventRequest struct {
	DelayId         string
	DelayTimeout    time.Duration
	DelayCsApiUrl   string
	LiveKitRoom     LiveKitRoomAlias
	LiveKitIdentity LiveKitIdentity
}

type SFURequest struct {
	RoomID        string              `json:"room_id"`
	SlotID        string              `json:"slot_id"`
	OpenIDToken   OpenIDTokenType     `json:"openid_token"`
	Member        MatrixRTCMemberType `json:"member"`
	DelayId       string              `json:"delay_id,omitempty"`
	DelayTimeout  int                 `json:"delay_timeout,omitempty"`
	DelayCsApiUrl string              `json:"delay_cs_api_url,omitempty"`
}

type SFUResponse struct {
	URL string `json:"url"`
	JWT string `json:"jwt"`
}

// MembershipLeaveDelegationRequest is the body of POST /membership_leave_delegation.
// It is used when the client is already connected to the SFU and wants to
// hand over the delayed disconnect event after the fact — i.e. no JWT is
// needed and the LiveKit room already exists.
//
// All three delayed-event fields are mandatory (unlike SFURequest where they
// are optional).  The participant is assumed to be already present on the SFU,
// so the monitor will use the participant-lookup backoff to confirm presence
// rather than waiting for the webhook (which has already fired).
type MembershipLeaveDelegationRequest struct {
	RoomID        string              `json:"room_id"`
	SlotID        string              `json:"slot_id"`
	OpenIDToken   OpenIDTokenType     `json:"openid_token"`
	Member        MatrixRTCMemberType `json:"member"`
	DelayId       string              `json:"delay_id"`
	DelayTimeout  int                 `json:"delay_timeout"`
	DelayCsApiUrl string              `json:"delay_cs_api_url"`
}

func (r *MembershipLeaveDelegationRequest) Validate() error {
	if r.RoomID == "" || r.SlotID == "" {
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "The request body is missing `room_id` or `slot_id`",
		}
	}
	if r.Member.ID == "" || r.Member.ClaimedUserID == "" || r.Member.ClaimedDeviceID == "" {
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "The request body `member` is missing `id`, `claimed_user_id` or `claimed_device_id`",
		}
	}
	if r.OpenIDToken.AccessToken == "" || r.OpenIDToken.MatrixServerName == "" {
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "The request body `openid_token` is missing `access_token` or `matrix_server_name`",
		}
	}
	if r.DelayId == "" || r.DelayTimeout <= 0 || r.DelayCsApiUrl == "" {
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "The request body is missing `delay_id`, `delay_timeout` or `delay_cs_api_url`",
		}
	}
	return nil
}

type MatrixErrorResponse struct {
	Status  int
	ErrCode string
	Err     string
}

type ValidatableSFURequest interface {
	Validate() error
}

func (e *MatrixErrorResponse) Error() string {
	return e.Err
}

func (r *SFURequest) Validate() error {
	if r.RoomID == "" || r.SlotID == "" {
		slog.Error("Missing room_id or slot_id", "room_id", r.RoomID, "slot_id", r.SlotID)
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "The request body is missing `room_id` or `slot_id`",
		}
	}
	if r.Member.ID == "" || r.Member.ClaimedUserID == "" || r.Member.ClaimedDeviceID == "" {
		slog.Error("Handler -> SFURequest: Missing member parameters", "Member", r.Member)
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "The request body `member` is missing a `id`, `claimed_user_id` or `claimed_device_id`",
		}
	}
	if r.OpenIDToken.AccessToken == "" || r.OpenIDToken.MatrixServerName == "" {
		slog.Error("Handler -> SFURequest: Missing OpenID token parameters:", "OpenIDToken", r.OpenIDToken)
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "The request body `openid_token` is missing a `access_token` or `matrix_server_name`",
		}
	}

	allDelayedEventParamsPresent := r.DelayId != "" && r.DelayTimeout > 0 && r.DelayCsApiUrl != ""
	atLeastOneDelayedEventParamPresent := r.DelayId != "" || r.DelayTimeout > 0 || r.DelayCsApiUrl != ""
	if atLeastOneDelayedEventParamPresent && !allDelayedEventParamsPresent {
		slog.Error("Handler -> SFURequest: Missing delayed event delegation parameters",
			"DelayId", r.DelayId,
			"DelayTimeout", r.DelayTimeout,
			"DelayCsApiUrl", r.DelayCsApiUrl,
		)
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "The request body is missing `delay_id`, `delay_timeout` or `delay_cs_api_url`",
		}
	}

	return nil
}

func (r *LegacySFURequest) Validate() error {
	if r.Room == "" {
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "Missing room parameter",
		}
	}
	if r.OpenIDToken.AccessToken == "" || r.OpenIDToken.MatrixServerName == "" {
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "Missing OpenID token parameters",
		}
	}
	return nil
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
// Handler.loop() is the single goroutine that owns the liveKitRoomMonitors
// map — no mutex needed.  HTTP handler goroutines communicate with loop()
// exclusively through channels:
//
//   - addJobCh:      deliver a new DelayedEventRequest to loop(), which
//                    creates/reuses a monitor and calls HandoverJob atomically.
//   - sfuEventCh:    deliver an SFU webhook event to loop(), which routes it
//                    to the correct monitor.
//   - monitorCommChan: monitors send NoJobsLeft back to loop() for cleanup.
//
// Because all map mutations happen in a single goroutine there are no data
// races and no mutex is required.
//
// # Atomicity of job handover
//
// loop() performs both steps atomically: it looks up (or creates) the
// monitor and calls HandoverJob in the same iteration, with no opportunity
// for the monitor to be removed in between.
type Handler struct {
	ctx                   context.Context
	cancel                context.CancelFunc
	liveKitAuth           LiveKitAuth
	fullAccessHomeservers []string
	skipVerifyTLS         bool
	// sanityCheckInterval is the period between re-checks of connected
	// participants.  Zero disables the sanity check.
	sanityCheckInterval time.Duration
	// loopDone is closed when loop() has exited.
	loopDone         chan struct{}
	monitorCommChan  chan HandlerMessage
	addJobCh         chan addJobRequest
	sfuEventCh       chan sfuEventRequest
}

// addJobRequest is sent by HTTP handlers to loop() to add a delayed-event job.
type addJobRequest struct {
	jobRequest *DelayedEventRequest
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
		monitorCommChan:       make(chan HandlerMessage, 10),
		addJobCh:              make(chan addJobRequest),
		sfuEventCh:            make(chan sfuEventRequest, 200),
	}
	go h.loop()
	return h
}

// loop is the sole owner of liveKitRoomMonitors — no locking needed.
func (h *Handler) loop() {
	defer close(h.loopDone)

	// The map lives entirely inside this goroutine.
	monitors := make(map[LiveKitRoomAlias]*LiveKitRoomMonitor)

	// monitorWg tracks all monitors started by loop() so that when loop()
	// exits it can wait for every monitor (and its lookup goroutines) to
	// finish before returning.  This ensures that background goroutines no
	// longer read global variables (e.g. LiveKitParticipantLookup) by the
	// time tests restore them.
	var monitorWg sync.WaitGroup

	// acquireMonitor returns the live monitor for a room, creating one if
	// needed.  Called only from within loop().
	acquireMonitor := func(lkRoom LiveKitRoomAlias) *LiveKitRoomMonitor {
		if m, ok := monitors[lkRoom]; ok {
			select {
			case <-m.ctx.Done():
				// Monitor is shutting down — replace it.
				slog.Info("Handler: replacing shutting-down monitor",
					"room", lkRoom, "MonitorId", m.MonitorId)
			default:
				return m
			}
		}
		m := NewLiveKitRoomMonitor(h.ctx, h.monitorCommChan, &h.liveKitAuth, lkRoom, h.sanityCheckInterval)
		monitorWg.Add(1)
		go func() {
			defer monitorWg.Done()
			m.Loop()
		}()
		monitors[lkRoom] = m
		slog.Debug("Handler: created LiveKitRoomMonitor",
			"room", lkRoom, "MonitorId", m.MonitorId)
		return m
	}

	for {
		select {
		case <-h.ctx.Done():
			slog.Debug("Handler: loop exiting")
			// Close all remaining monitors and wait for them — and their
			// lookup goroutines — to finish before loop() returns.  This
			// prevents background goroutines from reading global function
			// variables (e.g. LiveKitParticipantLookup) after tests have
			// restored them.
			for _, m := range monitors {
				if err := m.Close(); err != nil {
					slog.Error("Handler: error closing monitor on shutdown",
						"room", m.RoomAlias, "MonitorId", m.MonitorId, "err", err)
				}
			}
			monitorWg.Wait()
			return

		// ── job handover (atomic: lookup + HandoverJob in one step) ──────────
		case req := <-h.addJobCh:
			m := acquireMonitor(req.jobRequest.LiveKitRoom)
			jobId, ok := m.HandoverJob(req.jobRequest)
			if !ok {
				slog.Error("Handler: HandoverJob failed",
					"room", req.jobRequest.LiveKitRoom,
					"lkId", req.jobRequest.LiveKitIdentity)
			}
			req.result <- addJobResult{jobId: jobId, ok: ok}

		// ── SFU webhook routing ──────────────────────────────────────────────
		case ev := <-h.sfuEventCh:
			if m, ok := monitors[ev.roomAlias]; ok {
				select {
				case m.SFUCommChan <- ev.msg:
				case <-m.ctx.Done():
					// Monitor shutting down; event can be dropped safely.
				}
			}

		// ── monitor lifecycle ────────────────────────────────────────────────
		case event := <-h.monitorCommChan:
			switch event.Event {
			case NoJobsLeft:
				m, ok := monitors[event.RoomAlias]
				if !ok {
					break
				}
				if m.MonitorId != event.MonitorId {
					slog.Error("Handler: MonitorId mismatch on NoJobsLeft — ignoring",
						"room", event.RoomAlias,
						"mapMonitorId", m.MonitorId,
						"eventMonitorId", event.MonitorId)
					break
				}
				slog.Info("Handler: removing LiveKitRoomMonitor",
					"room", event.RoomAlias, "MonitorId", m.MonitorId)
				delete(monitors, event.RoomAlias)
				// m.Loop() has already cancelled itself and called monitorWg.Done()
				// before sending NoJobsLeft, so the monitorWg entry for this monitor
				// is already settled.  Close() is called in a goroutine purely to
				// avoid blocking loop() on the Close timeout — it does not need
				// separate tracking.
				go func(mon *LiveKitRoomMonitor) {
					if err := mon.Close(); err != nil {
						slog.Error("Handler: error closing monitor",
							"room", event.RoomAlias, "MonitorId", mon.MonitorId, "err", err)
					}
				}(m)
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

// addDelayedEventJob sends a job request to loop() and waits for the result.
// loop() performs the monitor lookup and HandoverJob atomically.
func (h *Handler) addDelayedEventJob(jobRequest *DelayedEventRequest) {
	slog.Debug("Handler: adding delayed event job",
		"room", jobRequest.LiveKitRoom,
		"lkId", jobRequest.LiveKitIdentity,
		"DelayId", jobRequest.DelayId)

	result := make(chan addJobResult, 1)
	select {
	case h.addJobCh <- addJobRequest{jobRequest: jobRequest, result: result}:
	case <-h.ctx.Done():
		slog.Warn("Handler: addDelayedEventJob called after shutdown",
			"room", jobRequest.LiveKitRoom)
		return
	}
	res := <-result
	if !res.ok {
		slog.Error("Handler: job handover failed",
			"room", jobRequest.LiveKitRoom,
			"lkId", jobRequest.LiveKitIdentity,
			"jobId", res.jobId)
	}
}

func (h *Handler) isFullAccessUser(matrixServerName string) bool {
	if len(h.fullAccessHomeservers) == 1 && h.fullAccessHomeservers[0] == "*" {
		return true
	}
	return slices.Contains(h.fullAccessHomeservers, matrixServerName)
}

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

	slog.Debug("Handler: got Matrix user info",
		"userInfo.Sub", userInfo.Sub,
		"access", map[bool]string{true: "full access", false: "restricted access"}[isFullAccessUser])

	lkIdentity := LiveKitIdentity(userInfo.Sub + ":" + req.DeviceID)
	slotId := "m.call#ROOM"
	lkRoomAlias := CreateLiveKitRoomAlias(req.Room, slotId)

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

	lkIdentity := CreateLiveKitIdentity(userInfo.Sub, req.Member.ClaimedDeviceID, req.Member.ID)
	lkRoomAlias := CreateLiveKitRoomAlias(req.RoomID, req.SlotID)

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
			h.addDelayedEventJob(&DelayedEventRequest{
				DelayCsApiUrl:   req.DelayCsApiUrl,
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
// The participant is assumed to be already present on the SFU.  The
// LiveKitRoomMonitor will use its participant-lookup backoff to confirm
// presence, which covers the case where the SFU webhook has already fired
// before this request arrived.
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

	lkIdentity := CreateLiveKitIdentity(userInfo.Sub, req.Member.ClaimedDeviceID, req.Member.ID)
	lkRoomAlias := CreateLiveKitRoomAlias(req.RoomID, req.SlotID)

	slog.Info("Handler: scheduling delayed event job (membership_leave_delegation)",
		"room", lkRoomAlias, "lkId", lkIdentity,
		"DelayId", req.DelayId, "CsApiUrl", req.DelayCsApiUrl,
		"RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"))

	h.addDelayedEventJob(&DelayedEventRequest{
		DelayCsApiUrl:   req.DelayCsApiUrl,
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
	mux.HandleFunc("/sfu/get", h.handle_legacy) // TODO: deprecated
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

// TODO: deprecated
func mapSFURequest(data *[]byte) (any, error) {
	requestTypes := []ValidatableSFURequest{&LegacySFURequest{}, &SFURequest{}}
	for _, req := range requestTypes {
		decoder := json.NewDecoder(strings.NewReader(string(*data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(req); err == nil {
			if err := req.Validate(); err != nil {
				return nil, err
			}
			return req, nil
		}
	}
	return nil, &MatrixErrorResponse{
		Status:  http.StatusBadRequest,
		ErrCode: "M_BAD_JSON",
		Err:     "The request body was malformed, missing required fields, or contained invalid values.",
	}
}

// TODO: deprecated
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
		body, err := io.ReadAll(r.Body)
		if err != nil {
			slog.Error("Handler (legacy): error reading body",
				"RemoteAddr", r.RemoteAddr, "err", err)
			writeMatrixError(w, http.StatusBadRequest, "M_NOT_JSON", "Error reading request")
			return
		}

		sfuAccessRequest, err := mapSFURequest(&body)
		if err != nil {
			matrixErr := &MatrixErrorResponse{}
			if errors.As(err, &matrixErr) {
				writeMatrixError(w, matrixErr.Status, matrixErr.ErrCode, matrixErr.Err)
			}
			return
		}

		var sfuAccessResponse *SFUResponse
		switch sfuReq := sfuAccessRequest.(type) {
		case *SFURequest:
			sfuAccessResponse, err = h.processSFURequest(r, sfuReq)
		case *LegacySFURequest:
			sfuAccessResponse, err = h.processLegacySFURequest(r, sfuReq)
		}

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

// ── config / main ─────────────────────────────────────────────────────────────

func readKeySecret() (string, string) {
	key := os.Getenv("LIVEKIT_KEY")
	secret := os.Getenv("LIVEKIT_SECRET")
	keyPath := os.Getenv("LIVEKIT_KEY_FROM_FILE")
	secretPath := os.Getenv("LIVEKIT_SECRET_FROM_FILE")
	keySecretPath := os.Getenv("LIVEKIT_KEY_FILE")

	if keySecretPath != "" {
		keySecretBytes, err := os.ReadFile(keySecretPath)
		if err != nil {
			log.Fatal(err)
		}
		parts := strings.Split(string(keySecretBytes), ":")
		if len(parts) != 2 {
			log.Fatalf("invalid key secret file format!")
		}
		slog.Info("Using LiveKit API key and secret from LIVEKIT_KEY_FILE", "keySecretPath", keySecretPath)
		key = parts[0]
		secret = parts[1]
	} else {
		if keyPath != "" {
			keyBytes, err := os.ReadFile(keyPath)
			if err != nil {
				log.Fatal(err)
			}
			slog.Info("Using LiveKit API key from LIVEKIT_KEY_FROM_FILE", "keyPath", keyPath)
			key = string(keyBytes)
		}
		if secretPath != "" {
			secretBytes, err := os.ReadFile(secretPath)
			if err != nil {
				log.Fatal(err)
			}
			slog.Info("Using LiveKit API secret from LIVEKIT_SECRET_FROM_FILE", "secretPath", secretPath)
			secret = string(secretBytes)
		}
	}

	return strings.Trim(key, " \r\n"), strings.Trim(secret, " \r\n")
}

func parseConfig() (*Config, error) {
	skipVerifyTLS := os.Getenv("LIVEKIT_INSECURE_SKIP_VERIFY_TLS") == "YES_I_KNOW_WHAT_I_AM_DOING"
	if skipVerifyTLS {
		slog.Warn("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!")
		slog.Warn("!!! WARNING !!!  LIVEKIT_INSECURE_SKIP_VERIFY_TLS        !!! WARNING !!!")
		slog.Warn("!!! WARNING !!!  Allow to skip invalid TLS certificates  !!! WARNING !!!")
		slog.Warn("!!! WARNING !!!  Use only for testing or debugging       !!! WARNING !!!")
		slog.Warn("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!\n")
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	key, secret := readKeySecret()
	lkUrl := os.Getenv("LIVEKIT_URL")

	if key == "" || secret == "" || lkUrl == "" {
		return nil, fmt.Errorf("LIVEKIT_KEY[_FILE], LIVEKIT_SECRET[_FILE] and LIVEKIT_URL must be set")
	}

	fullAccessHomeservers := os.Getenv("LIVEKIT_FULL_ACCESS_HOMESERVERS")
	if len(fullAccessHomeservers) == 0 {
		localHomeservers := os.Getenv("LIVEKIT_LOCAL_HOMESERVERS")
		if len(localHomeservers) > 0 {
			slog.Warn("!!! LIVEKIT_LOCAL_HOMESERVERS is deprecated, use LIVEKIT_FULL_ACCESS_HOMESERVERS !!!")
			fullAccessHomeservers = localHomeservers
		} else {
			slog.Warn("LIVEKIT_FULL_ACCESS_HOMESERVERS not set, defaulting to wildcard (*)")
			fullAccessHomeservers = "*"
		}
	}

	lkJwtBind := os.Getenv("LIVEKIT_JWT_BIND")
	lkJwtPort := os.Getenv("LIVEKIT_JWT_PORT")
	if lkJwtBind == "" {
		if lkJwtPort == "" {
			lkJwtPort = "8080"
		} else {
			slog.Warn("!!! LIVEKIT_JWT_PORT is deprecated, use LIVEKIT_JWT_BIND !!!")
		}
		lkJwtBind = fmt.Sprintf(":%s", lkJwtPort)
	} else if lkJwtPort != "" {
		return nil, fmt.Errorf("LIVEKIT_JWT_BIND and LIVEKIT_JWT_PORT must not be set together")
	}

	var sanityCheckInterval time.Duration
	if s := os.Getenv("LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS"); s != "" {
		if secs, err := strconv.Atoi(s); err != nil || secs <= 0 {
			return nil, fmt.Errorf("LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS must be a positive integer, got %q", s)
		} else {
			sanityCheckInterval = time.Duration(secs) * time.Second
			slog.Info("Sanity check enabled", "interval", sanityCheckInterval)
		}
	}

	return &Config{
		Key:                   key,
		Secret:                secret,
		LkUrl:                 lkUrl,
		SkipVerifyTLS:         skipVerifyTLS,
		FullAccessHomeservers: strings.Fields(strings.ReplaceAll(fullAccessHomeservers, ",", " ")),
		LkJwtBind:             lkJwtBind,
		SanityCheckInterval:   sanityCheckInterval,
	}, nil
}

func main() {
	opts := slogcolor.DefaultOptions
	opts.NoColor = !isatty.IsTerminal(os.Stderr.Fd())

	logLevelString := os.Getenv("LIVEKIT_LOG_LEVEL")
	switch strings.ToLower(logLevelString) {
	case "debug":
		opts.Level = slog.LevelDebug
	case "info":
	case "warn", "warning":
		opts.Level = slog.LevelWarn
	case "error":
		opts.Level = slog.LevelError
	case "":
		opts.Level = slog.LevelInfo
		slog.Info("log level defaulting to info")
	default:
		opts.Level = slog.LevelInfo
		slog.Warn("Invalid log level in LIVEKIT_LOG_LEVEL, defaulting to info",
			"invalidValue", logLevelString)
	}
	slog.SetDefault(slog.New(slogcolor.NewHandler(os.Stderr, opts)))

	config, err := parseConfig()
	if err != nil {
		log.Fatal(err)
	}

	handler := NewHandler(
		LiveKitAuth{
			key:          config.Key,
			secret:       config.Secret,
			authProvider: auth.NewSimpleKeyProvider(config.Key, config.Secret),
			lkUrl:        config.LkUrl,
		},
		config.SkipVerifyTLS,
		config.FullAccessHomeservers,
		config.SanityCheckInterval,
	)

	slog.Info("Starting service",
		"LIVEKIT_URL", config.LkUrl,
		"LIVEKIT_JWT_BIND", config.LkJwtBind,
		"LIVEKIT_FULL_ACCESS_HOMESERVERS", config.FullAccessHomeservers,
		"SkipVerifyTLS", config.SkipVerifyTLS,
		"LiveKit key", config.Key,
		"LiveKit secret", config.Secret,
		"SanityCheckInterval", config.SanityCheckInterval,
	)

	log.Fatal(http.ListenAndServe(config.LkJwtBind, handler.prepareMux()))
}

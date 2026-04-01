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
// HTTP handlers run in separate goroutines (one per request).  They call
// addDelayedEventJob() and read/write LiveKitRoomMonitors via a mutex-
// protected map.  The heavy work (job lifecycle, SFU event routing) is
// entirely delegated to LiveKitRoomMonitor.Loop() and DelayedEventJob.Loop().
//
// The Handler itself only needs a mutex to protect the monitors map.
//
// # Shutdown
//
// MonitorCommChan carries NoJobsLeft events from monitors back to the
// Handler's event loop (started in NewHandler).  When the handler context is
// cancelled (e.g. on server shutdown) the event loop exits cleanly.
type Handler struct {
	mu                    sync.Mutex // protects liveKitRoomMonitors only
	ctx                   context.Context
	cancel                context.CancelFunc
	liveKitAuth           LiveKitAuth
	fullAccessHomeservers []string
	skipVerifyTLS         bool
	liveKitRoomMonitors   map[LiveKitRoomAlias]*LiveKitRoomMonitor
	monitorCommChan       chan HandlerMessage
	// loopDone is closed when the internal event loop has exited.
	loopDone chan struct{}
}

func NewHandler(lkAuth LiveKitAuth, skipVerifyTLS bool, fullAccessHomeservers []string) *Handler {
	ctx, cancel := context.WithCancel(context.Background())
	h := &Handler{
		ctx:                   ctx,
		cancel:                cancel,
		liveKitAuth:           lkAuth,
		skipVerifyTLS:         skipVerifyTLS,
		fullAccessHomeservers: fullAccessHomeservers,
		monitorCommChan:       make(chan HandlerMessage, 10),
		liveKitRoomMonitors:   make(map[LiveKitRoomAlias]*LiveKitRoomMonitor),
		loopDone:              make(chan struct{}),
	}
	go h.loop()
	return h
}

// loop is the Handler's event-processing goroutine.
// It is the only place that removes monitors from the map.
func (h *Handler) loop() {
	defer close(h.loopDone)
	for {
		select {
		case <-h.ctx.Done():
			slog.Debug("Handler: loop exiting")
			return
		case event := <-h.monitorCommChan:
			switch event.Event {
			case NoJobsLeft:
				h.mu.Lock()
				monitor, ok := h.liveKitRoomMonitors[event.RoomAlias]
				if ok && monitor.MonitorId == event.MonitorId {
					slog.Info("Handler: removing LiveKitRoomMonitor",
						"room", event.RoomAlias, "MonitorId", monitor.MonitorId)
					delete(h.liveKitRoomMonitors, event.RoomAlias)
					h.mu.Unlock()
					// Close outside of the lock so Close() can take its time.
					if err := monitor.Close(); err != nil {
						slog.Error("Handler: error closing monitor",
							"room", event.RoomAlias, "MonitorId", monitor.MonitorId, "err", err)
					}
				} else if ok {
					slog.Error("Handler: MonitorId mismatch, not removing",
						"room", event.RoomAlias,
						"MonitorId", monitor.MonitorId,
						"requestedMonitorId", event.MonitorId)
					h.mu.Unlock()
				} else {
					h.mu.Unlock()
				}
			}
		}
	}
}

// Close shuts down the handler and waits for the internal loop to exit.
func (h *Handler) Close() {
	h.cancel()
	select {
	case <-h.loopDone:
	case <-time.After(10 * time.Second):
		slog.Warn("Handler: Close() timed out")
	}
}

func (h *Handler) addDelayedEventJob(jobRequest *DelayedEventRequest) {
	slog.Debug("Handler: adding delayed event job",
		"room", jobRequest.LiveKitRoom,
		"lkId", jobRequest.LiveKitIdentity,
		"DelayId", jobRequest.DelayId)

	monitor := h.acquireOrCreateMonitor(jobRequest.LiveKitRoom)

	jobId, ok := monitor.HandoverJob(jobRequest)
	if !ok {
		slog.Error("Handler: failed to handover job",
			"room", jobRequest.LiveKitRoom,
			"lkId", jobRequest.LiveKitIdentity,
			"jobId", jobId)
	}
}

// acquireOrCreateMonitor returns the existing LiveKitRoomMonitor for the
// given room, or creates and starts a new one.
//
// The returned monitor is guaranteed to be running (Loop() has been started).
func (h *Handler) acquireOrCreateMonitor(lkRoom LiveKitRoomAlias) *LiveKitRoomMonitor {
	h.mu.Lock()
	defer h.mu.Unlock()

	if monitor, ok := h.liveKitRoomMonitors[lkRoom]; ok {
		// Check whether the monitor is still alive by seeing if its context
		// is still active.
		select {
		case <-monitor.ctx.Done():
			// Monitor is shutting down — fall through to create a replacement.
			slog.Info("Handler: replacing shutting-down monitor",
				"room", lkRoom, "MonitorId", monitor.MonitorId)
		default:
			return monitor
		}
	}

	monitor := NewLiveKitRoomMonitor(h.ctx, h.monitorCommChan, &h.liveKitAuth, lkRoom)
	go monitor.Loop()
	h.liveKitRoomMonitors[lkRoom] = monitor
	slog.Debug("Handler: created new LiveKitRoomMonitor",
		"room", lkRoom, "MonitorId", monitor.MonitorId)
	return monitor
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

func (h *Handler) prepareMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/sfu/get", h.handle_legacy) // TODO: deprecated
	mux.HandleFunc("/get_token", h.handle)
	mux.HandleFunc("/disconnect_delegation", h.handle)
	mux.HandleFunc("/sfu_webhook", h.handleSfuWebhook)
	mux.HandleFunc("/healthz", h.healthcheck)
	return mux
}

func (h *Handler) healthcheck(w http.ResponseWriter, r *http.Request) {
	slog.Info("Handler: health check", "RemoteAddr", r.RemoteAddr)
	if r.Method == "GET" {
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

func (h *Handler) handleSfuWebhook(w http.ResponseWriter, r *http.Request) {
	event, err := webhook.ReceiveWebhookEvent(r, h.liveKitAuth.authProvider)
	if err != nil {
		slog.Warn("Handler: SFU webhook error", "err", err)
		return
	}

	h.mu.Lock()
	monitor, ok := h.liveKitRoomMonitors[LiveKitRoomAlias(event.Room.Name)]
	h.mu.Unlock()

	if !ok {
		return
	}

	switch event.Event {
	case "participant_joined":
		monitor.SFUCommChan <- SFUMessage{
			Type:            ParticipantConnected,
			LiveKitIdentity: LiveKitIdentity(event.Participant.Identity),
		}
		slog.Debug("Handler: SFU participant joined",
			"lkId", event.Participant.Identity, "room", event.Room.Name)

	case "participant_left", "participant_connection_aborted":
		msgType := ParticipantConnectionAborted
		if event.Participant.DisconnectReason == livekit.DisconnectReason_CLIENT_INITIATED {
			msgType = ParticipantDisconnectedIntentionally
		}
		monitor.SFUCommChan <- SFUMessage{
			Type:            msgType,
			LiveKitIdentity: LiveKitIdentity(event.Participant.Identity),
		}
		slog.Debug("Handler: SFU participant left",
			"lkId", event.Participant.Identity, "room", event.Room.Name,
			"DisconnectReason", event.Participant.DisconnectReason)
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
		return nil, fmt.Errorf("LIVEKIT_KEY[_FILE], LIVEKIT_SECRET[_FILE] and LIVEKIT_URL environment variables must be set")
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
		return nil, fmt.Errorf("LIVEKIT_JWT_BIND and LIVEKIT_JWT_PORT environment variables MUST NOT be set together")
	}

	return &Config{
		Key:                   key,
		Secret:                secret,
		LkUrl:                 lkUrl,
		SkipVerifyTLS:         skipVerifyTLS,
		FullAccessHomeservers: strings.Fields(strings.ReplaceAll(fullAccessHomeservers, ",", " ")),
		LkJwtBind:             lkJwtBind,
	}, nil
}

func main() {
	opts := slogcolor.DefaultOptions
	opts.Level = slog.LevelDebug
	opts.NoColor = !isatty.IsTerminal(os.Stderr.Fd())
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
	)

	slog.Info("Starting service",
		"LIVEKIT_URL", config.LkUrl,
		"LIVEKIT_JWT_BIND", config.LkJwtBind,
		"LIVEKIT_FULL_ACCESS_HOMESERVERS", config.FullAccessHomeservers,
		"SkipVerifyTLS", config.SkipVerifyTLS,
		"LiveKit key", config.Key,
		"LiveKit secret", config.Secret,
	)

	log.Fatal(http.ListenAndServe(config.LkJwtBind, handler.prepareMux()))
}

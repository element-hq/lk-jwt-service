// Copyright 2025 Element Creations Ltd.
// Copyright 2023 - 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package main

import (
	"context"
	"crypto/sha256"
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
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/mattn/go-isatty"

	"github.com/matrix-org/gomatrix"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

type LiveKitAuth struct {
	key          string
	secret       string
	authProvider *auth.SimpleKeyProvider
	lkUrl        string
}

type RoomMonitorEvent int
const(
	NoJobsLeft RoomMonitorEvent = iota
)
type HandlerMessage struct {
	RoomAlias LiveKitRoomAlias
	Event     RoomMonitorEvent
}
type Handler struct {
	sync.Mutex
	wg                       sync.WaitGroup
	liveKitAuth              LiveKitAuth
	fullAccessHomeservers    []string
	skipVerifyTLS            bool
	LiveKitRoomMonitors      map[LiveKitRoomAlias]*LiveKitRoomMonitor
	MonitorCommChan          chan HandlerMessage
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

type SFURequest struct {
	RoomID         string              `json:"room_id"`
	SlotID         string              `json:"slot_id"`
	OpenIDToken    OpenIDTokenType     `json:"openid_token"`
	Member         MatrixRTCMemberType `json:"member"`
	DelayID        string              `json:"delay_id,omitempty"`
	DelayTimeout   int                 `json:"delay_timeout,omitempty"`
	DelayCsApiUrl  string              `json:"delay_cs_api_url,omitempty"`
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

type LiveKitRoomAlias string
type LiveKitIdentity string


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
	
	all_delayed_event_params_present := r.DelayID != "" && r.DelayTimeout > 0 && r.DelayCsApiUrl != ""
	at_least_one_delayed_event_param_present := r.DelayID != "" || r.DelayTimeout > 0 || r.DelayCsApiUrl != ""
	if (at_least_one_delayed_event_param_present && !all_delayed_event_params_present) {
		slog.Error("Handler -> SFURequest: Missing delayed event delegation parameters", 
			"DelayID", r.DelayID, 
			"DelayTimeout", r.DelayTimeout, 
			"DelayCsApiUrl", r.DelayCsApiUrl,
		)
		return &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "The request body is missing `delayed_event_id` or `delay_timeout`",
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

var exchangeOpenIdUserInfo = func(
	ctx context.Context, token OpenIDTokenType, skipVerifyTLS bool,
) (*fclient.UserInfo, error) {
	if token.AccessToken == "" || token.MatrixServerName == "" {
		return nil, errors.New("missing parameters in openid token")
	}

	if skipVerifyTLS {
		slog.Warn("OpenIDUserInfo:: !!! WARNING !!! Skipping TLS verification", "MatrixServerName", token.MatrixServerName)
		// Disable TLS verification on the default HTTP Transport for the well-known lookup
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	client := fclient.NewClient(fclient.WithWellKnownSRVLookups(true), fclient.WithSkipVerify(skipVerifyTLS))

	// validate the openid token by getting the user's ID
	userinfo, err := client.LookupUserInfo(
		ctx, spec.ServerName(token.MatrixServerName), token.AccessToken,
	)
	if err != nil {
		slog.Error("OpenIDUserInfo: Failed to look up user info", "err", err)
		return nil, errors.New("failed to look up user info")
	}
	return &userinfo, nil
}

func (h *Handler) addDelayedEventJob(jobDescription *DelayedEventJob) {

	slog.Debug("Handler: Adding delayed event job", "room", jobDescription.LiveKitRoom, "lkId", jobDescription.LiveKitIdentity, "DelayID", jobDescription.DelayID)

	targetMonitor, ok := h.getRoomMonitor(LiveKitRoomAlias(jobDescription.LiveKitIdentity))	
	if !ok {
		slog.Info("Handler: Creating new LiveKitRoomMonitor", "room", jobDescription.LiveKitRoom, "lkId", jobDescription.LiveKitIdentity, "DelayID", jobDescription.DelayID)
		targetMonitor = NewLiveKitRoomMonitor(&h.liveKitAuth, jobDescription.LiveKitRoom)
		targetMonitor.HandlerCommChan = h.MonitorCommChan
		h.addRoomMonitor(targetMonitor)
	}

	job, err := NewDelayedEventJob(
		jobDescription.CsApiUrl,
		jobDescription.DelayID,
		jobDescription.DelayTimeout,
		jobDescription.LiveKitRoom,
		jobDescription.LiveKitIdentity,
		targetMonitor.JobCommChan,
	)
	slog.Debug("Handler: Created delayed event job", "room", jobDescription.LiveKitRoom, "lkId", jobDescription.LiveKitIdentity, "DelayID", jobDescription.DelayID)
	if err != nil {
		slog.Error("Error creating delayed event job",  "room", jobDescription.LiveKitRoom, "lkId", jobDescription.LiveKitIdentity, "err", err)
		return
	}
	targetMonitor.addDelayedEventJob(job)
}

func (h *Handler) isFullAccessUser(matrixServerName string) bool {
	// Grant full access if wildcard '*' is present as the only entry
	if len(h.fullAccessHomeservers) == 1 && h.fullAccessHomeservers[0] == "*" {
		return true
	}

	// Check if the matrixServerName is in the list of full-access homeservers
	return slices.Contains(h.fullAccessHomeservers, matrixServerName)
}

func (h *Handler) processLegacySFURequest(r *http.Request, req *LegacySFURequest) (*SFUResponse, error) {
	// Note LegacySFURequest has already been validated at this point

	userInfo, err := exchangeOpenIdUserInfo(r.Context(), req.OpenIDToken, h.skipVerifyTLS)
	if err != nil {
		return nil, &MatrixErrorResponse{
			Status:  http.StatusInternalServerError,
			ErrCode: "M_LOOKUP_FAILED",
			Err:     "Failed to look up user info from homeserver",
		}
	}

	isFullAccessUser := h.isFullAccessUser(req.OpenIDToken.MatrixServerName)

	log.Printf(
		"Got Matrix user info for %s (%s)",
		userInfo.Sub,
		map[bool]string{true: "full access", false: "restricted access"}[isFullAccessUser],
	)

	// TODO: is DeviceID required? If so then we should have validated at the start
	lkIdentity := LiveKitIdentity(userInfo.Sub + ":" + req.DeviceID)
	token, err := getJoinToken(h.liveKitAuth.key, h.liveKitAuth.secret, LiveKitRoomAlias(req.Room), lkIdentity)
	if err != nil {
		return nil, &MatrixErrorResponse{
			Status:  http.StatusInternalServerError,
			ErrCode: "M_UNKNOWN",
			Err:     "Internal Server Error",
		}
	}

	if isFullAccessUser {
		if err := helperCreateLiveKitRoom(r.Context(), &h.liveKitAuth, LiveKitRoomAlias(req.Room), userInfo.Sub, lkIdentity); err != nil {
			return nil, &MatrixErrorResponse{
				Status:  http.StatusInternalServerError,
				ErrCode: "M_UNKNOWN",
				Err:     "Unable to create room on SFU",
			}
		}
	}

	return &SFUResponse{URL: h.liveKitAuth.lkUrl, JWT: token}, nil
}

func (h *Handler) processSFURequest(r *http.Request, req *SFURequest) (*SFUResponse, error) {
	// Note SFURequest has already been validated at this point

	userInfo, err := exchangeOpenIdUserInfo(r.Context(), req.OpenIDToken, h.skipVerifyTLS)
	if err != nil {
		return nil, &MatrixErrorResponse{
			Status:  http.StatusUnauthorized,
			ErrCode: "M_UNAUTHORIZED",
			Err:     "The request could not be authorised.",
		}
	}

	// Check if validated userInfo.Sub matches req.Member.ClaimedUserID
	if req.Member.ClaimedUserID != userInfo.Sub {
		slog.Warn("Handler: Claimed user ID %s does not match token subject %s", "ClaimedUserID", req.Member.ClaimedUserID, "userInfo.Sub", userInfo.Sub)
		return nil, &MatrixErrorResponse{
			Status:  http.StatusUnauthorized,
			ErrCode: "M_UNAUTHORIZED",
			Err:     "The request could not be authorised.",
		}
	}

	// Does the user belong to homeservers granted full access
	isFullAccessUser := h.isFullAccessUser(req.OpenIDToken.MatrixServerName)

	delayedEventDelegationRequested := req.DelayID != ""
	if delayedEventDelegationRequested {
		// TODO: Check if homeserver supported delegation of delayed events
		// org.matrix.msc4140
	}

	// Use a valid DelayID as indicator for delegation of delayed events (fail early)
	if delayedEventDelegationRequested && !isFullAccessUser {
		return nil, &MatrixErrorResponse{
			Status:  http.StatusBadRequest,
			ErrCode: "M_BAD_JSON",
			Err:     "Delegation of delayed events is only supported for full access users",
		}
	}

	slog.Info(
		"Handler: Got Matrix user info", "userInfo.Sub", 
		userInfo.Sub, 
		"access", map[bool]string{true: "full access", false: "restricted access"}[isFullAccessUser],
	)

	lkIdentityRaw := userInfo.Sub + "|" + req.Member.ClaimedDeviceID + "|" + req.Member.ID
	lkIdentity := LiveKitIdentity(fmt.Sprintf("%x", sha256.Sum256([]byte(lkIdentityRaw))))
	lkRoomAlias := LiveKitRoomAlias(fmt.Sprintf("%x", sha256.Sum256([]byte(req.RoomID+"|"+req.SlotID))))
	token, err := getJoinToken(h.key, h.secret, lkRoomAlias, lkIdentity)
	if err != nil {
		slog.Error("Handler: Error getting LiveKit token", "userInfo.Sub", userInfo.Sub, "err", err)
		return nil, &MatrixErrorResponse{
			Status:  http.StatusInternalServerError,
			ErrCode: "M_UNKNOWN",
			Err:     "Internal Server Error",
		}
	}

	if isFullAccessUser {
		if err := helperCreateLiveKitRoom(r.Context(), &h.liveKitAuth, lkRoomAlias, userInfo.Sub, lkIdentity); err != nil {
			return nil, &MatrixErrorResponse{
				Status:  http.StatusInternalServerError,
				ErrCode: "M_UNKNOWN",
				Err:     "Unable to create room on SFU",
			}
		}

		if delayedEventDelegationRequested {
			// TODO verify support for delayed events
			slog.Info("Handler: Scheduling delayed event job", "room", lkRoomAlias, "lkId", lkIdentity, "DelayID", req.DelayID, "CsApiUrl", req.DelayCsApiUrl)
			h.addDelayedEventJob(&DelayedEventJob{
				CsApiUrl:         req.DelayCsApiUrl,
				DelayID:          req.DelayID,
				DelayTimeout:     time.Duration(req.DelayTimeout) * time.Millisecond,
				LiveKitRoom:      lkRoomAlias,
				LiveKitIdentity:  lkIdentity,
			},
			)
		}
	}

	return &SFUResponse{URL: h.liveKitAuth.lkUrl, JWT: token}, nil
}

var helperCreateLiveKitRoom = func(ctx context.Context, liveKitAuth *LiveKitAuth, room LiveKitRoomAlias, matrixUser string, lkIdentity LiveKitIdentity) error {
	roomClient := lksdk.NewRoomServiceClient(liveKitAuth.lkUrl, liveKitAuth.key, liveKitAuth.secret)
	creationStart := time.Now().Unix()
	lkRoom, err := roomClient.CreateRoom(
		ctx,
		&livekit.CreateRoomRequest{
			Name:             string(room),
			EmptyTimeout:     5 * 60, // 5 Minutes to keep the room open if no one joins
			DepartureTimeout: 20,     // number of seconds to keep the room after everyone leaves
			MaxParticipants:  0,      // 0 == no limitation
		},
	)

	if err != nil {
		return fmt.Errorf("unable to create room %s: %w", room, err)
	}

	// Log the room creation time and the user info
	isNewRoom := lkRoom.GetCreationTime() >= creationStart && lkRoom.GetCreationTime() <= time.Now().Unix()
	slog.Info(
		fmt.Sprintf("createLiveKitRoom: %s Room for full-access Matrix user", map[bool]string{true: "Created", false: "Using"}[isNewRoom]),
		"room", room,
		"roomSid", lkRoom.Sid,
		"lkId", lkIdentity,
		"matrixUser", matrixUser,
		"access", "full access",
	)

	return nil
}

func (h *Handler) addRoomMonitor(m *LiveKitRoomMonitor) {
	h.Lock()
	defer h.Unlock()
	h.LiveKitRoomMonitors[m.RoomAlias] = m
	h.wg.Add(1)
}

func (h* Handler) getRoomMonitor(name LiveKitRoomAlias) (*LiveKitRoomMonitor, bool) {
	h.Lock()
	defer h.Unlock()
	
	monitor, ok := h.LiveKitRoomMonitors[name]
	return monitor, ok
}

func (h* Handler) removeRoomMonitor(name LiveKitRoomAlias) {
	h.Lock()
	defer h.Unlock()
    if _, ok := h.LiveKitRoomMonitors[name]; ok {
        delete(h.LiveKitRoomMonitors, name)
        h.wg.Done()
    }	
}

func (h *Handler) prepareMux() *http.ServeMux {

	mux := http.NewServeMux()
	mux.HandleFunc("/sfu/get", h.handle_legacy) // TODO: This is deprecated and will be removed in future versions
	mux.HandleFunc("/get_token", h.handle)
	mux.HandleFunc("/healthz", h.healthcheck)

	return mux
}

func (h *Handler) healthcheck(w http.ResponseWriter, r *http.Request) {
	slog.Info("Handler: Health check", "RemoteAddr", r.RemoteAddr)

	if r.Method == "GET" {
		w.WriteHeader(http.StatusOK)
		return
	} else {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// TODO: This is deprecated and will be removed in future versions
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
		Err:     "The request body was malformed, missing required fields, or contained invalid values (e.g. missing `room_id`, `slot_id`, or `openid_token`).",
	}
}

// TODO: This is deprecated and will be removed in future versions
func (h *Handler) handle_legacy(w http.ResponseWriter, r *http.Request) {
	slog.Info("Handler (legacy endpoint): New Request", "RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"))

	w.Header().Set("Content-Type", "application/json")

	// Set the CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST")
	w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token")

	switch r.Method {
	case "OPTIONS":
		// Handle preflight request (CORS)
		w.WriteHeader(http.StatusOK)
		return
	case "POST":
		// Read request body once for later JSON parsing
		body, err := io.ReadAll(r.Body)
		if err != nil {
			slog.Error("Handler (legacy endpoint): Error reading request body", "RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"), "err", err)
			writeMatrixError(w, http.StatusBadRequest, "M_NOT_JSON", "Error reading request")
			return
		}

		var sfuAccessResponse *SFUResponse

		sfuAccessRequest, err := mapSFURequest(&body)
		if err != nil {
			matrixErr := &MatrixErrorResponse{}
			if errors.As(err, &matrixErr) {
				slog.Error("Handler (legacy endpoint): Error processing request", "RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"), "matrixErr", matrixErr.Err)
				writeMatrixError(w, matrixErr.Status, matrixErr.ErrCode, matrixErr.Err)
				return
			}
		}

		switch sfuReq := sfuAccessRequest.(type) {
		case *SFURequest:
			log.Printf("Processing SFU request")
			sfuAccessResponse, err = h.processSFURequest(r, sfuReq)
		case *LegacySFURequest:
			log.Printf("Processing legacy SFU request")
			sfuAccessResponse, err = h.processLegacySFURequest(r, sfuReq)
		}

		if err != nil {
			matrixErr := &MatrixErrorResponse{}
			if errors.As(err, &matrixErr) {
				slog.Error("Handler (legacy endpoint): Error reading request body", "RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"), "matrixErr", matrixErr.Err)
				writeMatrixError(w, matrixErr.Status, matrixErr.ErrCode, matrixErr.Err)
				return
			}
		}

		if err := json.NewEncoder(w).Encode(&sfuAccessResponse); err != nil {
			slog.Error("Handler (legacy endpoint): failed to encode json response!", "RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"), "err", err)
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handle(w http.ResponseWriter, r *http.Request) {
	log.Printf("Request from %s at \"%s\"", r.RemoteAddr, r.Header.Get("Origin"))

	w.Header().Set("Content-Type", "application/json")

	// Set the CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST")
	w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token")

	// Handle preflight request (CORS)
	switch r.Method {
	case "OPTIONS":
		slog.Debug("Handler: preflight request (CORS)", "RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"))
		w.WriteHeader(http.StatusOK)
		return
	case "POST":
		slog.Info("Handler: New Request", "RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"))
		var sfuAccessRequest SFURequest

		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&sfuAccessRequest); err == nil {
			if err := sfuAccessRequest.Validate(); err != nil {
				matrixErr := &MatrixErrorResponse{}
				if errors.As(err, &matrixErr) {
					slog.Error("Handler: Error processing request", "RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"), "err", matrixErr.Err)
					writeMatrixError(w, matrixErr.Status, matrixErr.ErrCode, matrixErr.Err)
					return
				}
			}
		} else {
			slog.Error("Handler: Error reading request body", "RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"), "err", err)
			writeMatrixError(w, http.StatusBadRequest, "M_NOT_JSON", "Error reading request")
			return
		}

		log.Printf("Processing SFU request")
		sfuAccessResponse, err := h.processSFURequest(r, &sfuAccessRequest)

		if err != nil {
			matrixErr := &MatrixErrorResponse{}
			if errors.As(err, &matrixErr) {
				slog.Error("Handler: Error reading request body", "RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"), "matrixErr", matrixErr.Err)
				writeMatrixError(w, matrixErr.Status, matrixErr.ErrCode, matrixErr.Err)
				return
			}
		}

		if err := json.NewEncoder(w).Encode(&sfuAccessResponse); err != nil {
			slog.Error("Handler: failed to encode json response!", "RemoteAddr", r.RemoteAddr, "Origin", r.Header.Get("Origin"), "err", err)
		}

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func readKeySecret() (string, string) {
	// We initialize keys & secrets from environment variables
	key := os.Getenv("LIVEKIT_KEY")
	secret := os.Getenv("LIVEKIT_SECRET")
	// We initialize potential key & secret path from environment variables
	keyPath := os.Getenv("LIVEKIT_KEY_FROM_FILE")
	secretPath := os.Getenv("LIVEKIT_SECRET_FROM_FILE")
	keySecretPath := os.Getenv("LIVEKIT_KEY_FILE")

	// If keySecretPath is set we read the file and split it into two parts
	// It takes over any other initialization
	if keySecretPath != "" {
		if keySecretBytes, err := os.ReadFile(keySecretPath); err != nil {
			log.Fatal(err)
		} else {
			keySecrets := strings.Split(string(keySecretBytes), ":")
			if len(keySecrets) != 2 {
				log.Fatalf("invalid key secret file format!")
			}
			slog.Info("Using LiveKit API key and API secret from LIVEKIT_KEY_FILE", "keySecretPath", keySecretPath)
			key = keySecrets[0]
			secret = keySecrets[1]
		}
	} else {
		// If keySecretPath is not set, we try to read the key and secret from files
		// If those files are not set, we return the key & secret from the environment variables
		if keyPath != "" {
			if keyBytes, err := os.ReadFile(keyPath); err != nil {
				log.Fatal(err)
			} else {
				slog.Info("Using LiveKit API key from LIVEKIT_KEY_FROM_FILE", "keyPath", keyPath)
				key = string(keyBytes)
			}
		}

		if secretPath != "" {
			if secretBytes, err := os.ReadFile(secretPath); err != nil {
				log.Fatal(err)
			} else {
				slog.Info("Using LiveKit API secret from LIVEKIT_SECRET_FROM_FILE", "secretPath", secretPath)
				secret = string(secretBytes)
			}
		}

	}

	// remove white spaces, new lines and carriage returns
	// from key and secret
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
			slog.Warn("!!! LIVEKIT_LOCAL_HOMESERVERS is deprecated, please use LIVEKIT_FULL_ACCESS_HOMESERVERS instead !!!")
			fullAccessHomeservers = localHomeservers
		} else {
			slog.Warn("LIVEKIT_FULL_ACCESS_HOMESERVERS not set, defaulting to wildcard (*) for full access")
			fullAccessHomeservers = "*"
		}
	}

	lkJwtBind := os.Getenv("LIVEKIT_JWT_BIND")
	lkJwtPort := os.Getenv("LIVEKIT_JWT_PORT")

	if lkJwtBind == "" {
		if lkJwtPort == "" {
			lkJwtPort = "8080"
		} else {
			slog.Warn("!!! LIVEKIT_JWT_PORT is deprecated, please use LIVEKIT_JWT_BIND instead !!!")
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

func NewHandler(lkAuth LiveKitAuth, skipVerifyTLS bool, fullAccessHomeservers []string) *Handler {

	//writeDelayedEventJobChan := make(chan writeDelayedEventJob)

	handler := &Handler{
		liveKitAuth:              lkAuth,
		skipVerifyTLS:            skipVerifyTLS,
		fullAccessHomeservers:    fullAccessHomeservers,
		MonitorCommChan: make(chan HandlerMessage),
		LiveKitRoomMonitors:   make(map[LiveKitRoomAlias]*LiveKitRoomMonitor),
	}

	go func() {
		for {
			select {
			case event := <- handler.MonitorCommChan:
				switch event.Event {
				case NoJobsLeft:
					_, ok := handler.getRoomMonitor(event.RoomAlias)
					if ok {
						slog.Info("Handler: Removing LiveKitRoomMonitor", "room", event.RoomAlias)
						handler.removeRoomMonitor(event.RoomAlias)
					}
				}
			}
		}
	}()

	return handler
}

func main() {
	// Set global logger with custom options
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
			// for LiveKit webhooks we need authProvider, for createLiveKitRoom we need 
			// key and secret which is not exposed by authProvider, hence redundancy
			authProvider: auth.NewSimpleKeyProvider(config.Key, config.Secret),
			lkUrl:        config.LkUrl,
		},
		config.SkipVerifyTLS,
		config.FullAccessHomeservers,
	)

	// TODO
	// check if we can create a proper connection to SFU in order to fail fast

	slog.Info(
		"Starting service", 
		"LIVEKIT_URL", config.LkUrl,
		"LIVEKIT_JWT_BIND", config.LkJwtBind,
		"LIVEKIT_FULL_ACCESS_HOMESERVERS", config.FullAccessHomeservers,
		"SkipVerifyTLS", config.SkipVerifyTLS,
		"LiveKit key", config.Key,
		"LiveKit secret", config.Secret,
	)

	log.Fatal(http.ListenAndServe(config.LkJwtBind, handler.prepareMux()))
}

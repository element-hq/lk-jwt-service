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
	"net/http"
	"os"
	"slices"
	"strings"

	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/matrix-org/gomatrix"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

type Handler struct {
	key, secret, lkUrl    string
	fullAccessHomeservers []string
	skipVerifyTLS         bool
}
type Config struct {
	Key                   string
	Secret                string
	LkUrl                 string
	SkipVerifyTLS         bool
	FullAccessHomeservers []string
	LkJwtBind               string
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
	DelayedEventID string              `json:"delayed_event_id"`
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
		log.Printf("Missing room_id or slot_id: room_id='%s', slot_id='%s'", r.RoomID, r.SlotID)
        return &MatrixErrorResponse{
            Status:  http.StatusBadRequest,
            ErrCode: "M_BAD_JSON",
            Err:     "The request body is missing `room_id` or `slot_id`",
        }
    }
    if r.Member.ID == "" || r.Member.ClaimedUserID == "" || r.Member.ClaimedDeviceID == "" {
        log.Printf("Missing member parameters: %+v", r.Member)
        return &MatrixErrorResponse{
            Status:  http.StatusBadRequest,
            ErrCode: "M_BAD_JSON",
            Err:     "The request body `member` is missing a `id`, `claimed_user_id` or `claimed_device_id`",
        }
    }
    if r.OpenIDToken.AccessToken == "" || r.OpenIDToken.MatrixServerName == "" {
		log.Printf("Missing OpenID token parameters: %+v", r.OpenIDToken)
        return &MatrixErrorResponse{
            Status:  http.StatusBadRequest,
            ErrCode: "M_BAD_JSON",
            Err:     "The request body `openid_token` is missing a `access_token` or `matrix_server_name`",
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
        log.Printf("failed to encode json error message! %v", err)
    }
}

func getJoinToken(apiKey, apiSecret, room, identity string) (string, error) {
	at := auth.NewAccessToken(apiKey, apiSecret)

	canPublish := true
	canSubscribe := true
	grant := &auth.VideoGrant{
		RoomJoin:     true,
		RoomCreate:   false,
		CanPublish:   &canPublish,
		CanSubscribe: &canSubscribe,
		Room:         room,
	}

	at.SetVideoGrant(grant).
		SetIdentity(identity).
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
		log.Printf("!!! WARNING !!! Skipping TLS verification for matrix client connection to %s", token.MatrixServerName)
		// Disable TLS verification on the default HTTP Transport for the well-known lookup
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	client := fclient.NewClient(fclient.WithWellKnownSRVLookups(true), fclient.WithSkipVerify(skipVerifyTLS))

	// validate the openid token by getting the user's ID
	hostOverride := os.Getenv("USER_INFO_HOST_OVERRIDE")
	serverName := spec.ServerName(token.MatrixServerName)
	if hostOverride != "" {
		serverName = spec.ServerName(hostOverride)
	}
	userinfo, err := client.LookupUserInfo(
		ctx, serverName, token.AccessToken,
	)
	if err != nil {
		log.Printf("Failed to look up user info: %v", err)
		return nil, errors.New("failed to look up user info")
	}
	return &userinfo, nil
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
			Status: http.StatusInternalServerError,
			ErrCode: "M_LOOKUP_FAILED",
			Err: "Failed to look up user info from homeserver",
		}
    }

    isFullAccessUser := h.isFullAccessUser(req.OpenIDToken.MatrixServerName)

    log.Printf(
        "Got Matrix user info for %s (%s)",
        userInfo.Sub,
        map[bool]string{true: "full access", false: "restricted access"}[isFullAccessUser],
    )

    // TODO: is DeviceID required? If so then we should have validated at the start
    lkIdentity := userInfo.Sub + ":" + req.DeviceID
    token, err := getJoinToken(h.key, h.secret, req.Room, lkIdentity)
    if err != nil {
		return nil, &MatrixErrorResponse{
			Status: http.StatusInternalServerError,
			ErrCode: "M_UNKNOWN",
			Err: "Internal Server Error",
		}
    }

    if isFullAccessUser {
        if err := createLiveKitRoom(r.Context(), h, req.Room, userInfo.Sub, lkIdentity); err != nil {
			return nil, &MatrixErrorResponse{
				Status: http.StatusInternalServerError,
				ErrCode: "M_UNKNOWN",
				Err: "Unable to create room on SFU",
			}
        }
    }

    return &SFUResponse{URL: h.lkUrl, JWT: token}, nil
}

func (h *Handler) processSFURequest(r *http.Request, req *SFURequest) (*SFUResponse, error) {
	// Note SFURequest has already been validated at this point
	
    userInfo, err := exchangeOpenIdUserInfo(r.Context(), req.OpenIDToken, h.skipVerifyTLS)
    if err != nil {
		return nil, &MatrixErrorResponse{
			Status: http.StatusUnauthorized,
			ErrCode: "M_UNAUTHORIZED",
			Err: "The request could not be authorised.",
		}
    }

	// Check if validated userInfo.Sub matches req.Member.ClaimedUserID
	if req.Member.ClaimedUserID != userInfo.Sub {
		log.Printf("Claimed user ID %s does not match token subject %s", req.Member.ClaimedUserID, userInfo.Sub)
		return nil, &MatrixErrorResponse{
			Status: http.StatusUnauthorized,
			ErrCode: "M_UNAUTHORIZED",
			Err: "The request could not be authorised.",
		}
	}

	// Does the user belong to homeservers granted full access
    isFullAccessUser := h.isFullAccessUser(req.OpenIDToken.MatrixServerName)

    log.Printf(
        "Got Matrix user info for %s (%s)",
        userInfo.Sub,
        map[bool]string{true: "full access", false: "restricted access"}[isFullAccessUser],
    )

    lkIdentity := req.Member.ID
	lkRoomAlias := fmt.Sprintf("%x", sha256.Sum256([]byte(req.RoomID + "|" + req.SlotID)))
    token, err := getJoinToken(h.key, h.secret, lkRoomAlias, lkIdentity)
    if err != nil {
		log.Printf("Error getting LiveKit token: %v", err)
		return nil, &MatrixErrorResponse{
			Status: http.StatusInternalServerError,
			ErrCode: "M_UNKNOWN",
			Err: "Internal Server Error",
		}
    }

    if isFullAccessUser {
        if err := createLiveKitRoom(r.Context(), h, lkRoomAlias, userInfo.Sub, lkIdentity); err != nil {
			return nil, &MatrixErrorResponse{
				Status: http.StatusInternalServerError,
				ErrCode: "M_UNKNOWN",
				Err: "Unable to create room on SFU",
			}
        }
    }

    return &SFUResponse{URL: h.lkUrl, JWT: token}, nil
}

var createLiveKitRoom = func(ctx context.Context, h *Handler, room, matrixUser, lkIdentity string) error {
    roomClient := lksdk.NewRoomServiceClient(h.lkUrl, h.key, h.secret)
    creationStart := time.Now().Unix()
    lkRoom, err := roomClient.CreateRoom(
        ctx,
        &livekit.CreateRoomRequest{
            Name:             room,
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
    log.Printf(
        "%s LiveKit room sid: %s (alias: %s) for full-access Matrix user %s (LiveKit identity: %s)",
        map[bool]string{true: "Created", false: "Using"}[isNewRoom],
        lkRoom.Sid, room, matrixUser, lkIdentity,
    )

    return nil
}

func (h *Handler) prepareMux() *http.ServeMux {

	mux := http.NewServeMux()
	mux.HandleFunc("/sfu/get", h.handle_legacy) // TODO: This is deprecated and will be removed in future versions
 	mux.HandleFunc("/get_token", h.handle)
	mux.HandleFunc("/healthz", h.healthcheck)

	return mux
}

func (h *Handler) healthcheck(w http.ResponseWriter, r *http.Request) {
	log.Printf("Health check from %s", r.RemoteAddr)

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
		Status: http.StatusBadRequest,
		ErrCode: "M_BAD_JSON",
		Err: "The request body was malformed, missing required fields, or contained invalid values (e.g. missing `room_id`, `slot_id`, or `openid_token`).",
	}
}

// TODO: This is deprecated and will be removed in future versions
func (h *Handler) handle_legacy(w http.ResponseWriter, r *http.Request) {
	log.Printf("Request from %s at \"%s\"", r.RemoteAddr, r.Header.Get("Origin"))

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
			log.Printf("Error reading request body: %v", err)
			writeMatrixError(w, http.StatusBadRequest, "M_NOT_JSON", "Error reading request")
			return
		}

		var sfuAccessResponse *SFUResponse

		sfuAccessRequest, err := mapSFURequest(&body)
		if err != nil {
			matrixErr := &MatrixErrorResponse{}
			if errors.As(err, &matrixErr) {
				log.Printf("Error processing request: %v", matrixErr.Err)
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
				log.Printf("Error processing request: %v", matrixErr.Err)
				writeMatrixError(w, matrixErr.Status, matrixErr.ErrCode, matrixErr.Err)
				return
			}
		}

		if err := json.NewEncoder(w).Encode(&sfuAccessResponse); err != nil {
			log.Printf("failed to encode json response! %v", err)
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
		w.WriteHeader(http.StatusOK)
		return
	case "POST":
		var sfuAccessRequest SFURequest

		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&sfuAccessRequest); err == nil {
			if err := sfuAccessRequest.Validate(); err != nil {
				matrixErr := &MatrixErrorResponse{}
				if errors.As(err, &matrixErr) {
					log.Printf("Error processing request: %v", matrixErr.Err)
					writeMatrixError(w, matrixErr.Status, matrixErr.ErrCode, matrixErr.Err)
					return
				}
			}
		} else {
            log.Printf("Error reading request body: %v", err)
            writeMatrixError(w, http.StatusBadRequest, "M_NOT_JSON", "Error reading request")
            return
        }			

		log.Printf("Processing SFU request")
		sfuAccessResponse, err := h.processSFURequest(r, &sfuAccessRequest)

		if err != nil {
			matrixErr := &MatrixErrorResponse{}
			if errors.As(err, &matrixErr) {
				log.Printf("Error processing request: %v", matrixErr.Err)
				writeMatrixError(w, matrixErr.Status, matrixErr.ErrCode, matrixErr.Err)
				return
			}
		}

		if err := json.NewEncoder(w).Encode(&sfuAccessResponse); err != nil {
			log.Printf("failed to encode json response! %v", err)
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
			log.Printf("Using LiveKit API key and API secret from LIVEKIT_KEY_FILE")
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
				log.Printf("Using LiveKit API key from LIVEKIT_KEY_FROM_FILE")
				key = string(keyBytes)
			}
		}

		if secretPath != "" {
			if secretBytes, err := os.ReadFile(secretPath); err != nil {
				log.Fatal(err)
			} else {
				log.Printf("Using LiveKit API secret from LIVEKIT_SECRET_FROM_FILE")
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
		log.Printf("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!")
		log.Printf("!!! WARNING !!!  LIVEKIT_INSECURE_SKIP_VERIFY_TLS        !!! WARNING !!!")
		log.Printf("!!! WARNING !!!  Allow to skip invalid TLS certificates  !!! WARNING !!!")
		log.Printf("!!! WARNING !!!  Use only for testing or debugging       !!! WARNING !!!")
		log.Println("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!")
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
			log.Printf("!!! LIVEKIT_LOCAL_HOMESERVERS is deprecated, please use LIVEKIT_FULL_ACCESS_HOMESERVERS instead !!!")
			fullAccessHomeservers = localHomeservers
		} else {
			log.Printf("LIVEKIT_FULL_ACCESS_HOMESERVERS not set, defaulting to wildcard (*) for full access")
			fullAccessHomeservers = "*"
		}
	}

	lkJwtBind := os.Getenv("LIVEKIT_JWT_BIND")
	lkJwtPort := os.Getenv("LIVEKIT_JWT_PORT")

	if lkJwtBind == "" {
		if lkJwtPort == "" {
			lkJwtPort = "8080"
		} else {
			log.Printf("!!! LIVEKIT_JWT_PORT is deprecated, please use LIVEKIT_JWT_BIND instead !!!")
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
	config, err := parseConfig()
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("LIVEKIT_URL: %s, LIVEKIT_JWT_BIND: %s", config.LkUrl, config.LkJwtBind)
	log.Printf("LIVEKIT_FULL_ACCESS_HOMESERVERS: %v", config.FullAccessHomeservers)

	handler := &Handler{
		key:                   config.Key,
		secret:                config.Secret,
		lkUrl:                 config.LkUrl,
		skipVerifyTLS:         config.SkipVerifyTLS,
		fullAccessHomeservers: config.FullAccessHomeservers,
	}

	log.Fatal(http.ListenAndServe(config.LkJwtBind, handler.prepareMux()))
}

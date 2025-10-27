// Copyright 2023 New Vector Ltd
// Copyright 2025 New Vector Ltd
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

type OpenIDTokenType struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	MatrixServerName string `json:"matrix_server_name"`
}

type SFURequest struct {
	Room        string          `json:"room"`
	OpenIDToken OpenIDTokenType `json:"openid_token"`
	DeviceID    string          `json:"device_id"`
}

type SFUResponse struct {
	URL string `json:"url"`
	JWT string `json:"jwt"`
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
				key = string(keyBytes)
			}
		}

		if secretPath != "" {
			if secretBytes, err := os.ReadFile(secretPath); err != nil {
				log.Fatal(err)
			} else {
				secret = string(secretBytes)
			}
		}

	}

	// remove white spaces, new lines and carriage returns
	// from key and secret
	return strings.Trim(key, " \r\n"), strings.Trim(secret, " \r\n")
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

func exchangeOpenIdUserInfo(
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
	userinfo, err := client.LookupUserInfo(
		ctx, spec.ServerName(token.MatrixServerName), token.AccessToken,
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

func (h *Handler) prepareMux() *http.ServeMux {

	mux := http.NewServeMux()
	mux.HandleFunc("/sfu/get", h.handle)
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

func (h *Handler) handle(w http.ResponseWriter, r *http.Request) {
	log.Printf("Request from %s at \"%s\"", r.RemoteAddr, r.Header.Get("Origin"))

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
		err := json.NewDecoder(r.Body).Decode(&sfuAccessRequest)
		if err != nil {
			log.Printf("Error decoding JSON: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			err = json.NewEncoder(w).Encode(gomatrix.RespError{
				ErrCode: "M_NOT_JSON",
				Err:     "Error decoding JSON",
			})
			if err != nil {
				log.Printf("failed to encode json error message! %v", err)
			}
			return
		}

		if sfuAccessRequest.Room == "" {
			log.Printf("Request missing room")
			w.WriteHeader(http.StatusBadRequest)
			err = json.NewEncoder(w).Encode(gomatrix.RespError{
				ErrCode: "M_BAD_JSON",
				Err:     "Missing parameters",
			})
			if err != nil {
				log.Printf("failed to encode json error message! %v", err)
			}
			return
		}

		// TODO: we should be sanitising the input here before using it
		// e.g. only allowing `https://` URL scheme
		userInfo, err := exchangeOpenIdUserInfo(r.Context(), sfuAccessRequest.OpenIDToken, h.skipVerifyTLS)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			err = json.NewEncoder(w).Encode(gomatrix.RespError{
				ErrCode: "M_LOOKUP_FAILED",
				Err:     "Failed to look up user info from homeserver",
			})
			if err != nil {
				log.Printf("failed to encode json error message! %v", err)
			}
			return
		}

		// Does the user belong to homeservers granted full access
		isFullAccessUser := h.isFullAccessUser(sfuAccessRequest.OpenIDToken.MatrixServerName)

		log.Printf(
			"Got Matrix user info for %s (%s)",
			userInfo.Sub,
			map[bool]string{true: "full access", false: "restricted access"}[isFullAccessUser],
		)

		// TODO: is DeviceID required? If so then we should have validated at the start of the request processing
		lkIdentity := userInfo.Sub + ":" + sfuAccessRequest.DeviceID
		token, err := getJoinToken(h.key, h.secret, sfuAccessRequest.Room, lkIdentity)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			err = json.NewEncoder(w).Encode(gomatrix.RespError{
				ErrCode: "M_UNKNOWN",
				Err:     "Internal Server Error",
			})
			if err != nil {
				log.Printf("failed to encode json error message! %v", err)
			}
			return
		}

		if isFullAccessUser {
			roomClient := lksdk.NewRoomServiceClient(h.lkUrl, h.key, h.secret)
			creationStart := time.Now().Unix()
			room, err := roomClient.CreateRoom(
				context.Background(), &livekit.CreateRoomRequest{
					Name:             sfuAccessRequest.Room,
					EmptyTimeout:     5 * 60, // 5 Minutes to keep the room open if no one joins
					DepartureTimeout: 20,     // number of seconds to keep the room after everyone leaves
					MaxParticipants:  0,      // 0 == no limitation
				},
			)

			if err != nil {
				log.Printf("Unable to create room %s. Error message: %v", sfuAccessRequest.Room, err)

				w.WriteHeader(http.StatusInternalServerError)
				err = json.NewEncoder(w).Encode(gomatrix.RespError{
					ErrCode: "M_UNKNOWN",
					Err:     "Unable to create room on SFU",
				})
				if err != nil {
					log.Printf("failed to encode json error message! %v", err)
				}
				return
			}

			// Log the room creation time and the user info
			isNewRoom := room.GetCreationTime() >= creationStart && room.GetCreationTime() <= time.Now().Unix()
			log.Printf(
				"%s LiveKit room sid: %s (alias: %s) for full-access Matrix user %s (LiveKit identity: %s)",
				map[bool]string{true: "Created", false: "Using"}[isNewRoom],
				room.Sid, sfuAccessRequest.Room, userInfo.Sub, lkIdentity,
			)
		}

		res := SFUResponse{URL: h.lkUrl, JWT: token}

		w.Header().Set("Content-Type", "application/json")
		err = json.NewEncoder(w).Encode(res)
		if err != nil {
			log.Printf("failed to encode json response! %v", err)
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func main() {
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

	// Check if the key, secret or url are empty.
	if key == "" || secret == "" || lkUrl == "" {
		log.Fatal("LIVEKIT_KEY[_FILE], LIVEKIT_SECRET[_FILE] and LIVEKIT_URL environment variables must be set")
	}

	fullAccessHomeservers := os.Getenv("LIVEKIT_FULL_ACCESS_HOMESERVERS")

	if len(fullAccessHomeservers) == 0 {
		// For backward compatibility we also check for LIVEKIT_LOCAL_HOMESERVERS
		// TODO: Remove this backward compatibility in the near future.
		localHomeservers := os.Getenv("LIVEKIT_LOCAL_HOMESERVERS")
		if len(localHomeservers) > 0 {
			log.Printf("!!! LIVEKIT_LOCAL_HOMESERVERS is deprecated, please use LIVEKIT_FULL_ACCESS_HOMESERVERS instead !!!")
			fullAccessHomeservers = localHomeservers
		} else {
			// If no full access homeservers are set, we default to wildcard '*' to mimic the previous behavior.
			// TODO: Remove defaulting to wildcard '*' (aka full-access for all users) in the near future.
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
		log.Fatal("LIVEKIT_JWT_BIND and LIVEKIT_JWT_PORT environment variables must not be set together")
	}

	log.Printf("LIVEKIT_URL: %s, LIVEKIT_JWT_BIND: %s", lkUrl, lkJwtBind)
	log.Printf("LIVEKIT_FULL_ACCESS_HOMESERVERS: %v", fullAccessHomeservers)

	handler := &Handler{
		key:                   key,
		secret:                secret,
		lkUrl:                 lkUrl,
		skipVerifyTLS:         skipVerifyTLS,
		fullAccessHomeservers: strings.Split(fullAccessHomeservers, ","),
	}

	log.Fatal(http.ListenAndServe(lkJwtBind, handler.prepareMux()))
}

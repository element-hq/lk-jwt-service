// Copyright 2023 New Vector Ltd

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.

// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"context"
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
	key, secret, lk_url string
	local_homeservers   []string
	skipVerifyTLS       bool
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

func exchangeOIDCToken(
	ctx context.Context, token OpenIDTokenType, skipVerifyTLS bool,
) (*fclient.UserInfo, error) {
	if token.AccessToken == "" || token.MatrixServerName == "" {
		return nil, errors.New("Missing parameters in OIDC token")
	}

	if skipVerifyTLS {
		log.Printf("!!! WARNING !!! Skipping TLS verification for matrix client connection to %s", token.MatrixServerName)
	}
	client := fclient.NewClient(fclient.WithWellKnownSRVLookups(true), fclient.WithSkipVerify(skipVerifyTLS))

	// validate the openid token by getting the user's ID
	userinfo, err := client.LookupUserInfo(
		ctx, spec.ServerName(token.MatrixServerName), token.AccessToken,
	)
	if err != nil {
		log.Printf("Failed to look up user info: %v", err)
		return nil, errors.New("Failed to look up user info")
	}
	return &userinfo, nil
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
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	} else if r.Method == "POST" {
		var sfu_access_request SFURequest
		err := json.NewDecoder(r.Body).Decode(&sfu_access_request)
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

		if sfu_access_request.Room == "" {
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
		userInfo, err := exchangeOIDCToken(r.Context(), sfu_access_request.OpenIDToken, h.skipVerifyTLS)
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

		// Does user belong to local homeservers alongside our SFU Cluster
		is_local_user := slices.Contains(h.local_homeservers, sfu_access_request.OpenIDToken.MatrixServerName)

		log.Printf(
			"Got Matrix user info for %s (%s)",
			userInfo.Sub,
			map[bool]string{true: "local", false: "remote"}[is_local_user],
		)

		// TODO: is DeviceID required? If so then we should have validated at the start of the request processing
		lk_identity := userInfo.Sub + ":" + sfu_access_request.DeviceID
		token, err := getJoinToken(is_local_user, h.key, h.secret, sfu_access_request.Room, lk_identity)		
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

		if is_local_user {
			roomClient := lksdk.NewRoomServiceClient(h.lk_url, h.key, h.secret)
			room, err := roomClient.CreateRoom(
				context.Background(), &livekit.CreateRoomRequest{
					Name: sfu_access_request.Room,
					EmptyTimeout: 5 * 60, // 5 Minutes to keep the room open if no one joins
					DepartureTimeout: 20, // number of seconds to keep the room after everyone leaves 
					MaxParticipants: 0,   // 0 == no limitation
				},
			)

			if err != nil {
				log.Printf("Unable to create room %s. Error message: %v", sfu_access_request.Room, err)
				
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

			log.Printf("Created LiveKit room sid: %s (alias: %s) for local Matrix user %s (LiveKit identity: %s)", room.Sid, sfu_access_request.Room, userInfo.Sub , lk_identity)
		}

		res := SFUResponse{URL: h.lk_url, JWT: token}

		w.Header().Set("Content-Type", "application/json")
		err = json.NewEncoder(w).Encode(res)
		if err != nil {
			log.Printf("failed to encode json response! %v", err)
		}
	} else {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *Handler) prepareMux() *http.ServeMux {

	mux := http.NewServeMux()
	mux.HandleFunc("/sfu/get", h.handle)
	mux.HandleFunc("/healthz", h.healthcheck)

	return mux
}

func readKeySecret() (string, string) {
	key := os.Getenv("LIVEKIT_KEY")
	secret := os.Getenv("LIVEKIT_SECRET")
	key_path := os.Getenv("LIVEKIT_KEY_FILE")
	secret_path := os.Getenv("LIVEKIT_SECRET_FILE")
	if key_path != "" {
		if keyBytes, err := os.ReadFile(key_path); err != nil {
			log.Fatal(err)
		} else {
			key = string(keyBytes)
		}
	}

	if secret_path != "" {
		if secretBytes, err := os.ReadFile(secret_path); err != nil {
			log.Fatal(err)
		} else {
			secret = string(secretBytes)
		}
	}

	return key, secret
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
	lk_url := os.Getenv("LIVEKIT_URL")

	// Check if the key, secret or url are empty.
	if key == "" || secret == "" || lk_url == "" {
		log.Fatal("LIVEKIT_KEY[_FILE], LIVEKIT_SECRET[_FILE] and LIVEKIT_URL environment variables must be set")
	}

	local_homeservers := os.Getenv("LIVEKIT_LOCAL_HOMESERVERS")
	if len(local_homeservers) == 0 {
		log.Fatal("LIVEKIT_LOCAL_HOMESERVERS environment variables must be set")
	}

	lk_jwt_port := os.Getenv("LIVEKIT_JWT_PORT")
	if lk_jwt_port == "" {
		lk_jwt_port = "8080"
	}

	log.Printf("LIVEKIT_URL: %s, LIVEKIT_JWT_PORT: %s", lk_url, lk_jwt_port)
	log.Printf("LIVEKIT_LOCAL_HOMESERVERS: %v", local_homeservers)

	handler := &Handler{
		key:               key,
		secret:            secret,
		lk_url:            lk_url,
		skipVerifyTLS:     skipVerifyTLS,
		local_homeservers: strings.Split(local_homeservers, ","),
	}

	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", lk_jwt_port), handler.prepareMux()))
}

func getJoinToken(is_local_user bool, apiKey, apiSecret, room, identity string) (string, error) {
	at := auth.NewAccessToken(apiKey, apiSecret)

	canPublish := true
	canSubscribe := true
	grant := &auth.VideoGrant{
		RoomJoin:     true,
		RoomCreate:   is_local_user,
		CanPublish:   &canPublish,
		CanSubscribe: &canSubscribe,
		Room:         room,
	}

	at.SetVideoGrant(grant).
		SetIdentity(identity).
		SetValidFor(time.Hour)

	return at.ToJWT()
}

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

	"time"

	"github.com/livekit/protocol/auth"

	"github.com/matrix-org/gomatrix"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

type Handler struct {
	key, secret, lk_url string
	skipVerifyTLS bool
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
		var body SFURequest
		err := json.NewDecoder(r.Body).Decode(&body)
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

		if body.Room == "" {
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
		userInfo, err := exchangeOIDCToken(r.Context(), body.OpenIDToken, h.skipVerifyTLS)
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

		log.Printf("Got user info for %s", userInfo.Sub)

		// TODO: is DeviceID required? If so then we should have validated at the start of the request processing
		token, err := getJoinToken(h.key, h.secret, body.Room, userInfo.Sub+":"+body.DeviceID)
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

func (h *Handler) prepareMux() (*http.ServeMux) {

	mux := http.NewServeMux()
	mux.HandleFunc("/sfu/get", h.handle)
	mux.HandleFunc("/healthz", h.healthcheck)

	return mux
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

	key := os.Getenv("LIVEKIT_KEY")
	secret := os.Getenv("LIVEKIT_SECRET")
	lk_url := os.Getenv("LIVEKIT_URL")

	// Check if the key, secret or url are empty.
	if key == "" || secret == "" || lk_url == "" {
		log.Fatal("LIVEKIT_KEY, LIVEKIT_SECRET and LIVEKIT_URL environment variables must be set")
	}

	lk_jwt_port := os.Getenv("LIVEKIT_JWT_PORT")
	if lk_jwt_port == "" {
		lk_jwt_port = "8080"
	}

	log.Printf("LIVEKIT_KEY: %s, LIVEKIT_SECRET: %s, LIVEKIT_URL: %s, LIVEKIT_JWT_PORT: %s", key, secret, lk_url, lk_jwt_port)

	handler := &Handler{
		key:    key,
		secret: secret,
		lk_url: lk_url,
		skipVerifyTLS: skipVerifyTLS,
	}

	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", lk_jwt_port), handler.prepareMux()))
}

func getJoinToken(apiKey, apiSecret, room, identity string) (string, error) {
	at := auth.NewAccessToken(apiKey, apiSecret)

	canPublish := true
	canSubscribe := true
	grant := &auth.VideoGrant{
		RoomJoin:     true,
		RoomCreate:   true,
		CanPublish:   &canPublish,
		CanSubscribe: &canSubscribe,
		Room:         room,
	}

	at.SetVideoGrant(grant).
		SetIdentity(identity).
		SetValidFor(time.Hour)

	return at.ToJWT()
}

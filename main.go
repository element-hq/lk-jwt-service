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
	"strings"

	"time"

	"github.com/livekit/protocol/auth"

	"github.com/matrix-org/gomatrix"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

type Handler struct {
	key, secret, lk_url string
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
		return nil, errors.New("missing parameters in OIDC token")
	}

	if skipVerifyTLS {
		log.Printf("!!! WARNING !!! Skipping TLS verification for matrix client connection to %s", token.MatrixServerName)
		// Disable TLS verification on the default HTTP Transport for the well-known lookup
		http.DefaultTransport.(*http.Transport).TLSClientConfig  = &tls.Config{ InsecureSkipVerify: true }
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

		log.Printf("Got user info for %s", userInfo.Sub)

		// TODO: is DeviceID required? If so then we should have validated at the start of the request processing
		token, err := getJoinToken(h.key, h.secret, sfu_access_request.Room, userInfo.Sub+":"+sfu_access_request.DeviceID)
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
	default:
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
			key_secrets := strings.Split(string(keySecretBytes), ":")
			if len(key_secrets) != 2 {
				log.Fatalf("invalid key secret file format!")
			}
			key = key_secrets[0]
			secret = key_secrets[1]
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

	return strings.Trim(key, " \r\n"), strings.Trim(secret, " \r\n")
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
	lk_url := os.Getenv("LIVEKIT_URL")

	lk_jwt_port := os.Getenv("LIVEKIT_JWT_PORT")
	if lk_jwt_port == "" {
		lk_jwt_port = "8080"
	}

	log.Printf("LIVEKIT_URL: %s, LIVEKIT_JWT_PORT: %s", lk_url, lk_jwt_port)
	key, secret := readKeySecret()

	// Check if the key, secret or url are empty.
	if key == "" || secret == "" || lk_url == "" {
		log.Fatal("LIVEKIT_KEY[_FILE], LIVEKIT_SECRET[_FILE] and LIVEKIT_URL environment variables must be set")
	}

	handler := &Handler{
		key:           key,
		secret:        secret,
		lk_url:        lk_url,
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

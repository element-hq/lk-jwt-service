// Copyright 2025 Element Creations Ltd.
// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/matrix-org/gomatrix"
	"github.com/matrix-org/gomatrixserverlib/fclient"
)

func TestHealthcheck(t *testing.T) {
	handler := &Handler{}
	req, err := http.NewRequest("GET", "/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}
}

func TestHandleOptions(t *testing.T) {
	handler := &Handler{}
	req, err := http.NewRequest("OPTIONS", "/sfu/get", nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code for OPTIONS: got %v want %v", status, http.StatusOK)
	}

	if accessControlAllowOrigin := rr.Header().Get("Access-Control-Allow-Origin"); accessControlAllowOrigin != "*" {
		t.Errorf("handler returned wrong Access-Control-Allow-Origin: got %v want %v", accessControlAllowOrigin, "*")
	}

	if accessControlAllowMethods := rr.Header().Get("Access-Control-Allow-Methods"); accessControlAllowMethods != "POST" {
		t.Errorf("handler returned wrong Access-Control-Allow-Methods: got %v want %v", accessControlAllowMethods, "POST")
	}
}

func TestHandlePostMissingParams(t *testing.T) {
	handler := &Handler{}

	testCases := []map[string]interface{}{
		{},
		{
			"room": "",
		},
	}

	for _, testCase := range testCases {
		jsonBody, _ := json.Marshal(testCase)

		req, err := http.NewRequest("POST", "/sfu/get", bytes.NewBuffer(jsonBody))
		if err != nil {
			t.Fatal(err)
		}

		rr := httptest.NewRecorder()
		handler.prepareMux().ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusBadRequest {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusBadRequest)
		}

		var resp gomatrix.RespError
		err = json.NewDecoder(rr.Body).Decode(&resp)
		if err != nil {
			t.Errorf("failed to decode response body %v", err)
		}

		if resp.ErrCode != "M_BAD_JSON" {
			t.Errorf("unexpected error code: got %v want %v", resp.ErrCode, "M_BAD_JSON")
		}
	}
}

func TestHandlePost(t *testing.T) {
	handler := NewHandler(
		LiveKitAuth{
			secret: "testSecret",
			key:    "testKey",
			lkUrl:  "wss://lk.local:8080/foo",
		},
		true,
		[]string{"example.com"},
	)

	var matrixServerName = ""

	testServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Log("Received request")
		// Inspect the request
		if r.URL.Path != "/_matrix/federation/v1/openid/userinfo" {
			t.Errorf("unexpected request path: got %v want %v", r.URL.Path, "/_matrix/federation/v1/openid/userinfo")
		}

		if accessToken := r.URL.Query().Get("access_token"); accessToken != "testAccessToken" {
			t.Errorf("unexpected access token: got %v want %v", accessToken, "testAccessToken")
		}

		// Mock response
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		_, err := fmt.Fprintf(w, `{"sub": "@user:%s"}`, matrixServerName)
		if err != nil {
			t.Fatalf("failed to write response: %v", err)
		}
	}))
	defer testServer.Close()

	u, _ := url.Parse(testServer.URL)

	matrixServerName = u.Host

	testCase := map[string]interface{}{
		"room_id": "!testRoom:example.com",
		"slot_id": "m.call#ROOM",
		"openid_token": map[string]interface{}{
			"access_token":       "testAccessToken",
			"token_type":         "testTokenType",
			"matrix_server_name": u.Host,
			"expires_in":         3600,
		},
		"member": map[string]interface{}{
			"id":                "member_test_id",
			"claimed_user_id":   "@user:" + matrixServerName,
			"claimed_device_id": "testDevice",
		},
	}

	jsonBody, _ := json.Marshal(testCase)

	req, err := http.NewRequest("POST", "/get_token", bytes.NewBuffer(jsonBody))
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	if contentType := rr.Header().Get("Content-Type"); contentType != "application/json" {
		t.Errorf("handler returned wrong Content-Type: got %v want %v", contentType, "application/json")
	}

	var resp SFUResponse
	err = json.NewDecoder(rr.Body).Decode(&resp)
	if err != nil {
		t.Errorf("failed to decode response body %v", err)
	}

	if resp.URL != "wss://lk.local:8080/foo" {
		t.Errorf("unexpected URL: got %v want %v", resp.URL, "wss://lk.local:8080/foo")
	}

	if resp.JWT == "" {
		t.Error("expected JWT to be non-empty")
	}

	// parse JWT checking the shared secret
	token, err := jwt.Parse(resp.JWT, func(token *jwt.Token) (interface{}, error) {
		return []byte(handler.liveKitAuth.secret), nil
	})

	if err != nil {
		t.Fatalf("failed to parse JWT: %v", err)
	}

	claims, ok := token.Claims.(jwt.MapClaims)

	if !ok || !token.Valid {
		t.Fatalf("failed to parse claims from JWT: %v", err)
	}

		want_sub_hash := sha256.Sum256([]byte("@user:"+ matrixServerName + "|testDevice|member_test_id"))
		want_sub := unpaddedBase64.EncodeToString(want_sub_hash[:])
		if claims["sub"] != want_sub {
			t.Errorf("unexpected sub: got %v want %v", claims["sub"], "member_test_id")
		}

		// should have permission for the room
		want_room_hash := sha256.Sum256([]byte("!testRoom:example.com" + "|" +  "m.call#ROOM"))
		want_room := unpaddedBase64.EncodeToString(want_room_hash[:])
		if claims["video"].(map[string]interface{})["room"] != want_room {
			t.Errorf("unexpected room: got %v want %v", claims["video"].(map[string]interface{})["room"], want_room)
		}
}

func TestLegacyHandlePost(t *testing.T) {
	handler := NewHandler(
		LiveKitAuth{
			secret: "testSecret",
			key:    "testKey",
			lkUrl:  "wss://lk.local:8080/foo",
		},
		true,
		[]string{"example.com"},
	)

	var matrixServerName = ""

	testServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Log("Received request")
		// Inspect the request
		if r.URL.Path != "/_matrix/federation/v1/openid/userinfo" {
			t.Errorf("unexpected request path: got %v want %v", r.URL.Path, "/_matrix/federation/v1/openid/userinfo")
		}

		if accessToken := r.URL.Query().Get("access_token"); accessToken != "testAccessToken" {
			t.Errorf("unexpected access token: got %v want %v", accessToken, "testAccessToken")
		}

		// Mock response
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		_, err := fmt.Fprintf(w, `{"sub": "@user:%s"}`, matrixServerName)
		if err != nil {
			t.Fatalf("failed to write response: %v", err)
		}
	}))
	defer testServer.Close()

	u, _ := url.Parse(testServer.URL)

	matrixServerName = u.Host
	matrixRoom := "testRoom"

	testCase := map[string]interface{}{
		"room": matrixRoom,
		"openid_token": map[string]interface{}{
			"access_token":       "testAccessToken",
			"token_type":         "testTokenType",
			"matrix_server_name": u.Host,
			"expires_in":         3600,
		},
		"device_id": "testDevice",
	}

	jsonBody, _ := json.Marshal(testCase)

	req, err := http.NewRequest("POST", "/sfu/get", bytes.NewBuffer(jsonBody))
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	if contentType := rr.Header().Get("Content-Type"); contentType != "application/json" {
		t.Errorf("handler returned wrong Content-Type: got %v want %v", contentType, "application/json")
	}

	var resp SFUResponse
	err = json.NewDecoder(rr.Body).Decode(&resp)
	if err != nil {
		t.Errorf("failed to decode response body %v", err)
	}

	if resp.URL != "wss://lk.local:8080/foo" {
		t.Errorf("unexpected URL: got %v want %v", resp.URL, "wss://lk.local:8080/foo")
	}

	if resp.JWT == "" {
		t.Error("expected JWT to be non-empty")
	}

	// parse JWT checking the shared secret
	token, err := jwt.Parse(resp.JWT, func(token *jwt.Token) (interface{}, error) {
		return []byte(handler.liveKitAuth.secret), nil
	})

	if err != nil {
		t.Fatalf("failed to parse JWT: %v", err)
	}

	claims, ok := token.Claims.(jwt.MapClaims)

	if !ok || !token.Valid {
		t.Fatalf("failed to parse claims from JWT: %v", err)
	}

	if claims["sub"] != "@user:"+matrixServerName+":testDevice" {
		t.Errorf("unexpected sub: got %v want %v", claims["sub"], "@user:"+matrixServerName+":testDevice")
	}

	// should have permission for the room
	if claims["video"].(map[string]interface{})["room"] != string(CreateLiveKitRoomAlias(matrixRoom, "m.call#ROOM")) {
		t.Errorf("unexpected room: got %v want %v", claims["room"], string(CreateLiveKitRoomAlias(matrixRoom, "m.call#ROOM")))
	}
}

func TestIsFullAccessUser(t *testing.T) {
	handler := NewHandler(
		LiveKitAuth{
			secret: "testSecret",
			key:    "testKey",
			lkUrl:  "wss://lk.local:8080/foo",
		},
		true,
		[]string{"example.com", "another.example.com"},
	)

	// Test cases for full access users
	if handler.isFullAccessUser("example.com") {
		t.Log("User has full access")
	} else {
		t.Error("User has restricted access")
	}

	if handler.isFullAccessUser("another.example.com") {
		t.Log("User has full access")
	} else {
		t.Error("User has restricted access")
	}

	// Test cases for restricted access users
	if handler.isFullAccessUser("aanother.example.com") {
		t.Error("User has full access")
	} else {
		t.Log("User has restricted access")
	}

	if handler.isFullAccessUser("matrix.example.com") {
		t.Error("User has full access")
	} else {
		t.Log("User has restricted access")
	}

	// test wildcard access
	handler.fullAccessHomeservers = []string{"*"}
	if handler.isFullAccessUser("other.com") {
		t.Log("User has full access")
	} else {
		t.Error("User has restricted access")
	}
}

func TestGetJoinToken(t *testing.T) {
	apiKey := "testKey"
	apiSecret := "testSecret"
	room := LiveKitRoomAlias("testRoom")
	identity := LiveKitIdentity("testIdentity@example.com")

	tokenString, err := getJoinToken(apiKey, apiSecret, room, identity)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if tokenString == "" {
		t.Error("expected token to be non-empty")
	}

	// parse JWT checking the shared secret
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		return []byte(apiSecret), nil
	})
	claims, ok := token.Claims.(jwt.MapClaims)

	if !ok || !token.Valid {
		t.Fatalf("failed to parse claims from JWT: %v", err)
	}

	claimRoomCreate := claims["video"].(map[string]interface{})["roomCreate"]
	if claimRoomCreate == nil {
		claimRoomCreate = false
	}

	if claimRoomCreate == true {
		t.Fatalf("roomCreate property needs to be false, since the lk-jwt-service creates the room")
	}
}

func TestReadKeySecret(t *testing.T) {
	testCases := []struct {
		name           string
		env            map[string]string
		expectedKey    string
		expectedSecret string
		err            bool
	}{
		{
			name: "Read from env",
			env: map[string]string{
				"LIVEKIT_KEY":    "from_env_pheethiewixohp9eecheeGhuayeeph4l",
				"LIVEKIT_SECRET": "from_env_ahb8eiwae0viey7gee4ieNgahgeeQuie",
			},
			expectedKey:    "from_env_pheethiewixohp9eecheeGhuayeeph4l",
			expectedSecret: "from_env_ahb8eiwae0viey7gee4ieNgahgeeQuie",
			err:            false,
		},
		{
			name: "Read from livekit keysecret",
			env: map[string]string{
				"LIVEKIT_KEY_FILE": "./tests/keysecret.yaml",
			},
			expectedKey:    "keysecret_iethuB2LeLiNuishiaKeephei9jaatio",
			expectedSecret: "keysecret_xefaingo4oos6ohla9phiMieBu3ohJi2",
		},
		{
			name: "Read from file",
			env: map[string]string{
				"LIVEKIT_KEY_FROM_FILE":    "./tests/key",
				"LIVEKIT_SECRET_FROM_FILE": "./tests/secret",
			},
			expectedKey:    "from_file_oquusheiheiw4Iegah8te3Vienguus5a",
			expectedSecret: "from_file_vohmahH3eeyieghohSh3kee8feuPhaim",
		},
		{
			name: "Read from file key only",
			env: map[string]string{
				"LIVEKIT_KEY_FROM_FILE": "./tests/key",
				"LIVEKIT_SECRET":        "from_env_ahb8eiwae0viey7gee4ieNgahgeeQuie",
			},
			expectedKey:    "from_file_oquusheiheiw4Iegah8te3Vienguus5a",
			expectedSecret: "from_env_ahb8eiwae0viey7gee4ieNgahgeeQuie",
		},
		{
			name: "Read from file secret only",
			env: map[string]string{
				"LIVEKIT_SECRET_FROM_FILE": "./tests/secret",
				"LIVEKIT_KEY":              "from_env_qui8aiTopiekiechah9oocbeimeew2O",
			},
			expectedKey:    "from_env_qui8aiTopiekiechah9oocbeimeew2O",
			expectedSecret: "from_file_vohmahH3eeyieghohSh3kee8feuPhaim",
		},
		{
			name:           "Empty if secret no env",
			env:            map[string]string{},
			expectedKey:    "",
			expectedSecret: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				if err := os.Setenv(k, v); err != nil {
					t.Errorf("Failed to set environment variable %s: %v", k, err)
				}
			}

			key, secret := readKeySecret()
			if secret != tc.expectedSecret || key != tc.expectedKey {
				t.Errorf("Expected secret and key to be %s and %s but got %s and %s",
					tc.expectedSecret,
					tc.expectedKey,
					secret,
					key)
			}
			for k := range tc.env {
				if err := os.Unsetenv(k); err != nil {
					t.Errorf("Failed to unset environment variable %s: %v", k, err)
				}
			}
		})
	}
}

func TestParseConfig(t *testing.T) {
	testCases := []struct {
		name       string
		env        map[string]string
		wantConfig *Config
		wantErrMsg string
	}{
		{
			name: "Minimal valid config",
			env: map[string]string{
				"LIVEKIT_KEY":    "test_key",
				"LIVEKIT_SECRET": "test_secret",
				"LIVEKIT_URL":    "wss://test.livekit.cloud",
			},
			wantConfig: &Config{
				Key:                   "test_key",
				Secret:                "test_secret",
				LkUrl:                 "wss://test.livekit.cloud",
				SkipVerifyTLS:         false,
				FullAccessHomeservers: []string{"*"},
				LkJwtBind:             ":8080",
			},
		},
		{
			name: "Full config with all options",
			env: map[string]string{
				"LIVEKIT_KEY":                      "test_key",
				"LIVEKIT_SECRET":                   "test_secret",
				"LIVEKIT_URL":                      "wss://test.livekit.cloud",
				"LIVEKIT_FULL_ACCESS_HOMESERVERS":  "example.com, test.com",
				"LIVEKIT_JWT_BIND":                 ":9090",
				"LIVEKIT_INSECURE_SKIP_VERIFY_TLS": "YES_I_KNOW_WHAT_I_AM_DOING",
			},
			wantConfig: &Config{
				Key:                   "test_key",
				Secret:                "test_secret",
				LkUrl:                 "wss://test.livekit.cloud",
				SkipVerifyTLS:         true,
				FullAccessHomeservers: []string{"example.com", "test.com"},
				LkJwtBind:             ":9090",
			},
		},
		{
			name: "Legacy port configuration",
			env: map[string]string{
				"LIVEKIT_KEY":      "test_key",
				"LIVEKIT_SECRET":   "test_secret",
				"LIVEKIT_URL":      "wss://test.livekit.cloud",
				"LIVEKIT_JWT_PORT": "9090",
			},
			wantConfig: &Config{
				Key:                   "test_key",
				Secret:                "test_secret",
				LkUrl:                 "wss://test.livekit.cloud",
				SkipVerifyTLS:         false,
				FullAccessHomeservers: []string{"*"},
				LkJwtBind:             ":9090",
			},
		},
		{
			name: "Legacy full-access homeservers configuration",
			env: map[string]string{
				"LIVEKIT_KEY":               "test_key",
				"LIVEKIT_SECRET":            "test_secret",
				"LIVEKIT_URL":               "wss://test.livekit.cloud",
				"LIVEKIT_LOCAL_HOMESERVERS": "legacy.com",
			},
			wantConfig: &Config{
				Key:                   "test_key",
				Secret:                "test_secret",
				LkUrl:                 "wss://test.livekit.cloud",
				SkipVerifyTLS:         false,
				FullAccessHomeservers: []string{"legacy.com"},
				LkJwtBind:             ":8080",
			},
		},
		{
			name: "Missing required config",
			env: map[string]string{
				"LIVEKIT_KEY": "test_key",
			},
			wantErrMsg: "LIVEKIT_KEY[_FILE], LIVEKIT_SECRET[_FILE] and LIVEKIT_URL environment variables must be set",
		},
		{
			name: "Conflicting bind configuration",
			env: map[string]string{
				"LIVEKIT_KEY":      "test_key",
				"LIVEKIT_SECRET":   "test_secret",
				"LIVEKIT_URL":      "wss://test.livekit.cloud",
				"LIVEKIT_JWT_BIND": ":9090",
				"LIVEKIT_JWT_PORT": "8080",
			},
			wantErrMsg: "LIVEKIT_JWT_BIND and LIVEKIT_JWT_PORT environment variables MUST NOT be set together",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Setup: set env variables
			for k, v := range tc.env {
				if err := os.Setenv(k, v); err != nil {
					t.Fatalf("Failed to set environment variable %s: %v", k, err)
				}
			}
			defer func() {
				// Cleanup: reset env variables after test
				for k := range tc.env {
					if err := os.Unsetenv(k); err != nil {
						t.Errorf("Failed to unset environment variable %s: %v", k, err)
					}
				}
			}()

			// parse config from env variables
			got, err := parseConfig()

			// Given error(s), check potential error messages
			if tc.wantErrMsg != "" {
				if err == nil {
					t.Errorf("parseConfig() error = nil, wantErr %q", tc.wantErrMsg)
					return
				}
				if err.Error() != tc.wantErrMsg {
					t.Errorf("parseConfig() error = %q, wantErr %q", err.Error(), tc.wantErrMsg)
				}
				return
			}

			// Given no error, check for unexpected error messages
			if err != nil {
				t.Errorf("parseConfig() unexpected error: %v", err)
				return
			}

			// Compare parsed (got) config with wanted config
			if got.Key != tc.wantConfig.Key {
				t.Errorf("Key = %q, want %q", got.Key, tc.wantConfig.Key)
			}
			if got.Secret != tc.wantConfig.Secret {
				t.Errorf("Secret = %q, want %q", got.Secret, tc.wantConfig.Secret)
			}
			if got.LkUrl != tc.wantConfig.LkUrl {
				t.Errorf("LkUrl = %q, want %q", got.LkUrl, tc.wantConfig.LkUrl)
			}
			if got.SkipVerifyTLS != tc.wantConfig.SkipVerifyTLS {
				t.Errorf("SkipVerifyTLS = %v, want %v", got.SkipVerifyTLS, tc.wantConfig.SkipVerifyTLS)
			}
			if !reflect.DeepEqual(got.FullAccessHomeservers, tc.wantConfig.FullAccessHomeservers) {
				t.Errorf("FullAccessHomeservers = %v, want %v", got.FullAccessHomeservers, tc.wantConfig.FullAccessHomeservers)
			}
			if got.LkJwtBind != tc.wantConfig.LkJwtBind {
				t.Errorf("JwtBind = %q, want %q", got.LkJwtBind, tc.wantConfig.LkJwtBind)
			}
		})
	}
}

func TestMapSFURequest(t *testing.T) {
	testCases := []struct {
		name        string
		input       string
		want        any
		wantErrCode string
	}{
		{
			name: "Valid legacy request",
			input: `{
				"room": "testRoom",
				"openid_token": {
					"access_token": "test_token",
					"token_type": "Bearer",
					"matrix_server_name": "example.com",
					"expires_in": 3600
				},
				"device_id": "testDevice"
			}`,
			want: &LegacySFURequest{
				Room: "testRoom",
				OpenIDToken: OpenIDTokenType{
					AccessToken:      "test_token",
					TokenType:        "Bearer",
					MatrixServerName: "example.com",
					ExpiresIn:        3600,
				},
				DeviceID: "testDevice",
			},
		},
		{
			name: "Valid Matrix 2.0 request",
			input: `{
				"room_id": "!testRoom:example.com",
				"slot_id": "123",
				"openid_token": {
					"access_token": "test_token",
					"token_type": "Bearer",
					"matrix_server_name": "example.com",
					"expires_in": 3600
				},
				"member": {
					"id": "test_id",
					"claimed_user_id": "@test:example.com",
					"claimed_device_id": "testDevice"
				}
			}`,
			want: &SFURequest{
				RoomID: "!testRoom:example.com",
				SlotID: "123",
				OpenIDToken: OpenIDTokenType{
					AccessToken:      "test_token",
					TokenType:        "Bearer",
					MatrixServerName: "example.com",
					ExpiresIn:        3600,
				},
				Member: MatrixRTCMemberType{
					ID:              "test_id",
					ClaimedUserID:   "@test:example.com",
					ClaimedDeviceID: "testDevice",
				},
			},
		},
		{
			name: "Valid Matrix 2.0 request with delayed events parameters",
			input: `{
				"room_id": "!testRoom:example.com",
				"slot_id": "123",
				"openid_token": {
					"access_token": "test_token",
					"token_type": "Bearer",
					"matrix_server_name": "example.com",
					"expires_in": 3600
				},
				"member": {
					"id": "test_id",
					"claimed_user_id": "@test:example.com",
					"claimed_device_id": "testDevice"
				},
				"delay_id": "delayed123",
				"delay_timeout": 30,
				"delay_cs_api_url": "https://example.com/api"
			}`,
			want: &SFURequest{
				RoomID: "!testRoom:example.com",
				SlotID: "123",
				OpenIDToken: OpenIDTokenType{
					AccessToken:      "test_token",
					TokenType:        "Bearer",
					MatrixServerName: "example.com",
					ExpiresIn:        3600,
				},
				Member: MatrixRTCMemberType{
					ID:              "test_id",
					ClaimedUserID:   "@test:example.com",
					ClaimedDeviceID: "testDevice",
				},
				DelayId:     "delayed123",
				DelayTimeout: 30,
				DelayCsApiUrl: "https://example.com/api",
			},
		},
		{
			name: "Valid Matrix 2.0 request with INVALID delayed events parameters",
			input: `{
				"room_id": "!testRoom:example.com",
				"slot_id": "123",
				"openid_token": {
					"access_token": "test_token",
					"token_type": "Bearer",
					"matrix_server_name": "example.com",
					"expires_in": 3600
				},
				"member": {
					"id": "test_id",
					"claimed_user_id": "@test:example.com",
					"claimed_device_id": "testDevice"
				},
				"delay_id": "delayed123",
				"delay_timeout": 30
			}`,
			want:        nil,
			wantErrCode: "M_BAD_JSON",

		},		{
			name:        "Invalid JSON",
			input:       `{"invalid": json}`,
			want:        nil,
			wantErrCode: "M_BAD_JSON",
		},
		{
			name:        "Empty request",
			input:       `{}`,
			want:        nil,
			wantErrCode: "M_BAD_JSON",
		},
		{
			name: "Invalid legacy request with extra field",
			input: `{
				"room": "testRoom",
				"openid_token": {
					"access_token": "test_token",
					"token_type": "Bearer",
					"matrix_server_name": "example.com",
					"expires_in": 3600
				},
				"device_id": "testDevice",
				"extra_field": "should_fail"
			}`,
			want:        nil,
			wantErrCode: "M_BAD_JSON",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Convert string to []byte for input
			input := []byte(tc.input)

			// Call mapSFURequest
			got, err := mapSFURequest(&input)

			// Check error cases
			if tc.wantErrCode != "" {
				matrixErr := &MatrixErrorResponse{}
				if !errors.As(err, &matrixErr) {
					t.Errorf("mapSFURequest() error = %v, want MatrixErrorResponse", err)
					return
				}
				if matrixErr.ErrCode != tc.wantErrCode {
					t.Errorf("mapSFURequest() error code = %v, want %v", matrixErr.ErrCode, tc.wantErrCode)
				}
				return
			}

			// Check success cases
			if err != nil {
				t.Errorf("mapSFURequest() unexpected error: %v", err)
				return
			}

			// Type-specific comparisons
			switch expected := tc.want.(type) {
			case *LegacySFURequest:
				actual, ok := got.(*LegacySFURequest)
				if !ok {
					t.Errorf("mapSFURequest() returned wrong type, got %T, want *LegacySFURequest", got)
					return
				}
				if !reflect.DeepEqual(actual, expected) {
					t.Errorf("mapSFURequest() = %+v, want %+v", actual, expected)
				}
			case *SFURequest:
				actual, ok := got.(*SFURequest)
				if !ok {
					t.Errorf("mapSFURequest() returned wrong type, got %T, want *SFURequest", got)
					return
				}
				if !reflect.DeepEqual(actual, expected) {
					t.Errorf("mapSFURequest() = %+v, want %+v", actual, expected)
				}
			}
		})
	}
}

func TestMapSFURequestMemoryLeak(t *testing.T) {
	const iterations = 100000

	input := []byte(`{
		"room_id": "!testRoom:example.com",
		"slot_id": "123",
		"openid_token": {
			"access_token": "test_token",
			"token_type": "Bearer",
			"matrix_server_name": "example.com",
			"expires_in": 3600
		},
		"member": {
			"id": "test_id",
			"claimed_user_id": "@test:example.com",
			"claimed_device_id": "testDevice"
		}
	}`)

	// Force a garbage collection to start from a clean slate.
	var mStart, mEnd runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&mStart)

	for i := 0; i < iterations; i++ {
		_, err := mapSFURequest(&input)
		if err != nil {
			t.Fatalf("unexpected error in mapSFURequest iteration %d: %v", i, err)
		}
	}

	// Force another GC to clear unreferenced memory
	runtime.GC()
	runtime.ReadMemStats(&mEnd)

	t.Logf("Start Alloc: %d bytes, End Alloc: %d bytes", mStart.Alloc, mEnd.Alloc)

	// Check that allocated heap hasn’t grown unboundedly
	if mEnd.Alloc > mStart.Alloc {
		allocDiff := mEnd.Alloc - mStart.Alloc
		t.Logf("Heap allocation growth after %d iterations: %d bytes", iterations, allocDiff)

		// Heuristic threshold: less than 100KB growth across 100k iterations is fine
		const leakThreshold uint64 = 100 * 1024 // 100KB
		if allocDiff > leakThreshold {
			t.Errorf("Potential memory leak: heap grew by %d bytes (> %d)", allocDiff, leakThreshold)
		}
	}
}

func TestProcessSFURequest(t *testing.T) {
	// mock createLiveKitRoom
	var called_createLiveKitRoom bool
	original_createLiveKitRoom := helperCreateLiveKitRoom
	helperCreateLiveKitRoom = func(ctx context.Context, liveKitAuth *LiveKitAuth, room LiveKitRoomAlias, matrixUser string, lkIdentity LiveKitIdentity) error {
		called_createLiveKitRoom = true
		if room == "" {
			t.Error("expected room name passed into mock")
		}
		return nil
	}
	t.Cleanup(func() { helperCreateLiveKitRoom = original_createLiveKitRoom })

	// mock OpenID lookup
	var failed_exchangeOpenIdUserInfo bool
	var exchangeOpenIdUserInfo_MatrixID string
	original_exchangeOpenIdUserInfo := exchangeOpenIdUserInfo
	exchangeOpenIdUserInfo = func(ctx context.Context, token OpenIDTokenType, skip bool) (*fclient.UserInfo, error) {
		if failed_exchangeOpenIdUserInfo {
			return nil, &MatrixErrorResponse{
				Status:  http.StatusUnauthorized,
				ErrCode: "M_UNAUTHORIZED",
				Err:     "The request could not be authorised.",
			}
		}
		return &fclient.UserInfo{Sub: exchangeOpenIdUserInfo_MatrixID}, nil
	}
	t.Cleanup(func() { exchangeOpenIdUserInfo = original_exchangeOpenIdUserInfo })

	type testCase struct {
		name                       string
		MatrixID                   string
		ClaimedMatrixID            string
		getJoinTokenErr            error
		expectJoinTokenError       bool
		expectExchangeOpendIdError bool
		expectCreateRoomCall       bool
		expectError                bool
		exchangeErr                error
	}

	tests := []testCase{
		{
			name:                 "Full access user + all OK",
			MatrixID:             "@user:example.com",
			ClaimedMatrixID:      "@user:example.com",
			expectCreateRoomCall: true,
			expectError:          false,
		},
		{
			name:                 "Restricted user + all OK",
			MatrixID:             "@user:otherdomain.com",
			ClaimedMatrixID:      "@user:otherdomain.com",
			expectCreateRoomCall: false,
			expectError:          false,
		},
		{
			name:                       "Full access user but exchangeOpenIdUserInfo fails",
			MatrixID:                   "@user:example.com",
			ClaimedMatrixID:            "@user:example.com",
			expectExchangeOpendIdError: true,
			exchangeErr:                &MatrixErrorResponse{},
			expectCreateRoomCall:       false,
			expectError:                true,
		},
		{
			name:                 "Full access user but getJoinToken fails",
			MatrixID:             "@user:example.com",
			ClaimedMatrixID:      "@user:example.com",
			expectJoinTokenError: true,
			getJoinTokenErr:      &MatrixErrorResponse{},
			expectCreateRoomCall: false,
			expectError:          true,
		},
		{
			name:                 "Full access user but claimed_matrix_id fails",
			MatrixID:             "@user:example.com",
			ClaimedMatrixID:      "@user:faked.com",
			expectJoinTokenError: false,
			getJoinTokenErr:      &MatrixErrorResponse{},
			expectCreateRoomCall: false,
			expectError:          true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// --- mock createLiveKitRoom ---
			called_createLiveKitRoom = false
			failed_exchangeOpenIdUserInfo = tc.expectExchangeOpendIdError
			exchangeOpenIdUserInfo_MatrixID = tc.MatrixID

			handler := NewHandler(
				LiveKitAuth{
					key: map[bool]string{true: "", false: "the_api_key"}[tc.expectJoinTokenError], 
					secret: "secret", 
					lkUrl: "wss://lk.local:8080/foo",
				}, 
				false, 
				[]string{"example.com"},
			)

			req := &SFURequest{
				RoomID: "!room:example.com",
				SlotID: "slot",
				OpenIDToken: OpenIDTokenType{
					AccessToken:      "token",
					MatrixServerName: strings.Split(tc.ClaimedMatrixID, ":")[1],
				},
				Member: MatrixRTCMemberType{
					ID:              "device",
					ClaimedUserID:   tc.ClaimedMatrixID,
					ClaimedDeviceID: "dev",
				},
			}

			_, err := handler.processSFURequest(&http.Request{}, req)
			if tc.expectError && err == nil {
				t.Fatalf("expected error but got nil")
			}
			if !tc.expectError && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if called_createLiveKitRoom != tc.expectCreateRoomCall {
				t.Errorf("expected createLiveKitRoom called=%v, got %v", tc.expectCreateRoomCall, called_createLiveKitRoom)
			}

		})
	}

}

func TestProcessLegacySFURequest(t *testing.T) {
	// mock createLiveKitRoom
	var called_createLiveKitRoom bool
	original_createLiveKitRoom := helperCreateLiveKitRoom
	
	helperCreateLiveKitRoom = func(ctx context.Context, liveKitAuth *LiveKitAuth, room LiveKitRoomAlias, matrixUser string, lkIdentity LiveKitIdentity) error {
		called_createLiveKitRoom = true
		if room == "" {
			t.Error("expected room name passed into mock")
		}
		return nil
	}
	t.Cleanup(func() { helperCreateLiveKitRoom = original_createLiveKitRoom })

	// mock OpenID lookup
	var failed_exchangeOpenIdUserInfo bool
	original_exchangeOpenIdUserInfo := exchangeOpenIdUserInfo
	exchangeOpenIdUserInfo = func(ctx context.Context, token OpenIDTokenType, skip bool) (*fclient.UserInfo, error) {
		if failed_exchangeOpenIdUserInfo {
			return nil, &MatrixErrorResponse{
				Status:  http.StatusUnauthorized,
				ErrCode: "M_UNAUTHORIZED",
				Err:     "The request could not be authorised.",
			}
		}
		return &fclient.UserInfo{Sub: "@mock:example.com"}, nil
	}
	t.Cleanup(func() { exchangeOpenIdUserInfo = original_exchangeOpenIdUserInfo })

	type testCase struct {
		name                       string
		MatrixID                   string
		getJoinTokenErr            error
		expectJoinTokenError       bool
		expectExchangeOpendIdError bool
		expectCreateRoomCall       bool
		expectError                bool
		exchangeErr                error
	}

	tests := []testCase{
		{
			name:                 "Full access user + all OK",
			MatrixID:             "@user:example.com",
			expectCreateRoomCall: true,
			expectError:          false,
		},
		{
			name:                 "Restricted user + all OK",
			MatrixID:             "@user:otherdomain.com",
			expectCreateRoomCall: false,
			expectError:          false,
		},
		{
			name:                       "Full access user but exchangeOpenIdUserInfo fails",
			MatrixID:                   "@user:example.com",
			expectExchangeOpendIdError: true,
			exchangeErr:                &MatrixErrorResponse{},
			expectCreateRoomCall:       false,
			expectError:                true,
		},
		{
			name:                 "Full access user but getJoinToken fails",
			MatrixID:             "@user:example.com",
			expectJoinTokenError: true,
			getJoinTokenErr:      &MatrixErrorResponse{},
			expectCreateRoomCall: false,
			expectError:          true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// --- mock createLiveKitRoom ---
			called_createLiveKitRoom = false
			failed_exchangeOpenIdUserInfo = tc.expectExchangeOpendIdError

			handler := NewHandler(
				LiveKitAuth{
					key:    map[bool]string{true: "", false: "the_api_key"}[tc.expectJoinTokenError],
					secret: "secret",
					lkUrl:  "wss://lk.local:8080/foo",
				},
				false,
				[]string{"example.com"},
			)

			req := &LegacySFURequest{
				Room: "!room:example.com",
				OpenIDToken: OpenIDTokenType{
					AccessToken:      "token",
					MatrixServerName: strings.Split(tc.MatrixID, ":")[1],
				},
				DeviceID: "dev",
			}

			_, err := handler.processLegacySFURequest(&http.Request{}, req)
			if tc.expectError && err == nil {
				t.Fatalf("expected error but got nil")
			}
			if !tc.expectError && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if called_createLiveKitRoom != tc.expectCreateRoomCall {
				t.Errorf("expected createLiveKitRoom called=%v, got %v", tc.expectCreateRoomCall, called_createLiveKitRoom)
			}

		})
	}

}

func TestLiveKitRoomMonitor(t *testing.T) {

	original_LiveKitParticipantLookup := helperLiveKitParticipantLookup
	helperLiveKitParticipantLookup = func(ctx context.Context, lkAuth LiveKitAuth, lkRoomAlias LiveKitRoomAlias, lkId LiveKitIdentity, ch chan SFUMessage) (bool, error) {
		return true, nil
	}
	t.Cleanup(func() { helperLiveKitParticipantLookup = original_LiveKitParticipantLookup })

	handler := NewHandler(
		LiveKitAuth{
			secret: "secret",
			key:    "devkey",
			lkUrl:  "ws://127.0.0.1:7880",
		},
		true,
		[]string{"example.com"},
	)

	lkAlias := LiveKitRoomAlias("aa1bc9d7b9344361474764ef632415003bd4e0e8696f93c34fcc7f8e9d123848")

	monitor := NewLiveKitRoomMonitor(context.TODO(), &handler.liveKitAuth, lkAlias)

	jobRequest := &DelayedEventRequest{
		DelayCsApiUrl:    "https://synapse.m.localhost",
		DelayId:          "syd_astTzXBzAazONpxHCqzW",
		DelayTimeout:     10 * time.Second,
		LiveKitRoom:      lkAlias,
		LiveKitIdentity:  "@azure-colonial-otter:synapse.m.localhostQQVMKEAUKY",
	}

	job, _ := NewDelayedEventJob(
				context.TODO(),
				jobRequest,
				monitor.JobCommChan,
	)

	monitor.AddJob(job)
	monitor.RemoveJob(LiveKitIdentity(lkAlias), job.JobId)
	monitor.Close()

	

	/*
	var matrixServerName string

	// Mock Matrix server
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/federation/v1/openid/userinfo" {
			t.Errorf("unexpected URL path: got %v want %v", r.URL.Path, "/_matrix/federation/v1/openid/userinfo")
		}

		accessToken := r.URL.Query().Get("access_token")
		if accessToken != "testAccessToken" {
			t.Errorf("unexpected access_token: got %v want %v", accessToken, "testAccessToken")
		}

		userInfo := fclient.UserInfo{
			Sub: "@user:" + matrixServerName,
		}

		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(userInfo)
		if err != nil {
			t.Errorf("failed to encode user info: %v", err)
		}
	}))
	defer testServer.Close()

	u, err := url.Parse(testServer.URL)
	if err != nil {
		t.Fatalf("failed to parse test server URL: %v", err)
	}
	matrixServerName = u.Host

	// Prepare request
	testCase := map[string]any{
		"room": "testRoom",
		"openid_token": map[string]any{
			"access_token":      "testAccessToken",
			"token_type":        "Bearer",
			"matrix_server_name": matrixServerName,
			"expires_in":        3600,
		},
		"device_id": "testDevice",
	}

	reqBody, err := json.Marshal(testCase)
	if err != nil {
		t.Fatalf("failed to marshal test case: %v", err)
	}

	req, err := http.NewRequest("POST", "/sfu-jwt", bytes.NewBuffer(reqBody))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	// Record response
	rr := httptest.NewRecorder()
	handlerFunc := http.HandlerFunc(handler.SFUJWTHandler)
	handlerFunc.ServeHTTP(rr, req)

	// Check response
	if status := rr.Code; status != http.StatusOK {
		t.Fatalf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	var resp SFUJWTResponse
	err = json.NewDecoder(rr.Body).Decode(&resp)
	if err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.URL != "wss://lk.local:8080/foo" {
		t.Errorf("unexpected URL: got %v want %v", resp.URL, "wss://lk.local:8080/foo")
	}

	// parse JWT checking the shared secret
	token, err := jwt.Parse(resp.Token, func(token *jwt.Token) (interface{}, error) {
		return []byte("testSecret"), nil
	})
	claims, ok := token.Claims.(jwt.MapClaims)

	if !ok || !token.Valid {
		t.Fatalf("failed to parse claims from JWT: %v", err)
	}

	if claims["iss"] != "testKey" {
		t.Errorf("unexpected iss: got %v want %v", claims["iss"], "testKey")
	}

	if claims["sub"] != "@user:"+matrixServerName {
		t.Errorf("unexpected sub: got %v want %v", claims["sub"], "@user:"+matrixServerName)
	}

	if claims["room"] != "testRoom" {
		t.Errorf("unexpected room: got %v want %v", claims["room"], "testRoom")
	}
	*/
}

// Copyright 2025 New Vector Ltd
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/matrix-org/gomatrix"
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
	handler := &Handler{
		secret:                "testSecret",
		key:                   "testKey",
		lkUrl:                 "wss://lk.local:8080/foo",
		fullAccessHomeservers: []string{"example.com"},
		skipVerifyTLS:     true,
	}

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
		"room": "testRoom",
		"openid_token": map[string]interface{}{
			"access_token":       "testAccessToken",
			"token_type":         "testTokenType",
			"matrix_server_name": u.Host,
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
		return []byte(handler.secret), nil
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
	if claims["video"].(map[string]interface{})["room"] != "testRoom" {
		t.Errorf("unexpected room: got %v want %v", claims["room"], "testRoom")
	}
}

func TestIsFullAccessUser(t *testing.T) {
	handler := &Handler{
		secret:                "testSecret",
		key:                   "testKey",
		lkUrl:                 "wss://lk.local:8080/foo",
		fullAccessHomeservers: []string{"example.com", "another.example.com"},
		skipVerifyTLS:     true,
	}

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
	room := "testRoom"
	identity := "testIdentity@example.com"
	
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
// Copyright 2025 Element Creations Ltd.
// Copyright 2023 - 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// sfu_get_test.go: end-to-end tests for the legacy /sfu/get endpoint —
// HTTP method/options handling, malformed/missing-params responses, the
// full POST happy-path (with JWT inspection), and the underlying
// Handler.processLegacySFURequest method (handler.go).
//
// Deprecated: this endpoint is pre-Matrix-2.0. When /sfu/get is removed
// (see // Deprecated comments on LegacySFURequest / handle_legacy /
// processLegacySFURequest), delete this file too.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/matrix-org/gomatrix"
	"github.com/matrix-org/gomatrixserverlib/fclient"
)

// TestHandleSfuGet_Options verifies that OPTIONS /sfu/get returns 200 with
// the expected CORS headers.
func TestHandleSfuGet_Options(t *testing.T) {
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
	if v := rr.Header().Get("Access-Control-Allow-Origin"); v != "*" {
		t.Errorf("wrong Access-Control-Allow-Origin: got %v want *", v)
	}
	if v := rr.Header().Get("Access-Control-Allow-Methods"); v != "POST" {
		t.Errorf("wrong Access-Control-Allow-Methods: got %v want POST", v)
	}
}

// TestHandleSfuGet_MissingParams verifies that POSTs missing required body
// fields return 400 / M_BAD_JSON via LegacySFURequest.Validate.
func TestHandleSfuGet_MissingParams(t *testing.T) {
	handler := &Handler{}
	for _, testCase := range []map[string]interface{}{{}, {"room": ""}} {
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
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Errorf("failed to decode response body: %v", err)
		}
		if resp.ErrCode != "M_BAD_JSON" {
			t.Errorf("unexpected error code: got %v want M_BAD_JSON", resp.ErrCode)
		}
	}
}

// TestHandleSfuGet_Success verifies the full-access happy-path POST /sfu/get:
// a valid request produces a 200 SFUResponse, CreateLiveKitRoom is invoked,
// and the JWT's sub/room claims encode the legacy (pre-Matrix-2.0) identity
// scheme (`<sub>:<device>` for sub, hashed (room, "m.call#ROOM") for room).
//
// The OpenID exchange and LiveKit room creation are mocked here — the real
// exchangeOpenIdUserInfo path is exercised by TestExchangeOpenIdUserInfo in
// helper_test.go.
func TestHandleSfuGet_Success(t *testing.T) {
	const matrixServerName = "example.com"
	const claimedUserSub = "@user:" + matrixServerName
	const deviceID = "testDevice"
	const matrixRoom = "testRoom"

	originalExchange := exchangeOpenIdUserInfo
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		return &fclient.UserInfo{Sub: claimedUserSub}, nil
	}

	originalCreate := CreateLiveKitRoom
	t.Cleanup(func() { CreateLiveKitRoom = originalCreate })
	var createCalled bool
	CreateLiveKitRoom = func(_ context.Context, _ *LiveKitAuth, _ LiveKitRoomAlias, _ string, _ LiveKitIdentity) error {
		createCalled = true
		return nil
	}

	handler := NewHandler(
		LiveKitAuth{secret: "testSecret", key: "testKey", lkUrl: "wss://lk.local:8080/foo"},
		false, []string{matrixServerName},
		0, // sanityCheckInterval disabled
		map[string]string{},
	)
	t.Cleanup(handler.Close)

	body, _ := json.Marshal(map[string]interface{}{
		"room": matrixRoom,
		"openid_token": map[string]interface{}{
			"access_token":       "testAccessToken",
			"token_type":         "testTokenType",
			"matrix_server_name": matrixServerName,
			"expires_in":         3600,
		},
		"device_id": deviceID,
	})
	req := httptest.NewRequest("POST", "/sfu/get", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if !createCalled {
		t.Error("expected CreateLiveKitRoom to be called for full-access user")
	}

	var resp SFUResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if resp.URL != "wss://lk.local:8080/foo" {
		t.Errorf("resp.URL = %q, want wss://lk.local:8080/foo", resp.URL)
	}
	if resp.JWT == "" {
		t.Error("expected JWT to be non-empty")
	}

	token, err := jwt.Parse(resp.JWT, func(token *jwt.Token) (interface{}, error) {
		return []byte(handler.liveKitAuth.secret), nil
	})
	if err != nil {
		t.Fatalf("failed to parse JWT: %v", err)
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		t.Fatalf("failed to parse claims from JWT")
	}
	wantSub := claimedUserSub + ":" + deviceID
	if claims["sub"] != wantSub {
		t.Errorf("sub = %v, want %v", claims["sub"], wantSub)
	}
	wantRoom := string(LiveKitRoomAliasFor(matrixRoom, "m.call#ROOM"))
	if got := claims["video"].(map[string]interface{})["room"]; got != wantRoom {
		t.Errorf("room = %v, want %v", got, wantRoom)
	}
}

func TestProcessLegacySFURequest(t *testing.T) {
	var calledCreateLiveKitRoom bool
	originalCreate := CreateLiveKitRoom
	t.Cleanup(func() { CreateLiveKitRoom = originalCreate })
	CreateLiveKitRoom = func(_ context.Context, _ *LiveKitAuth, room LiveKitRoomAlias, _ string, _ LiveKitIdentity) error {
		calledCreateLiveKitRoom = true
		if room == "" {
			t.Error("expected non-empty room name")
		}
		return nil
	}

	var failExchange bool
	originalExchange := exchangeOpenIdUserInfo
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		if failExchange {
			return nil, &MatrixErrorResponse{Status: http.StatusUnauthorized, ErrCode: "M_UNAUTHORIZED", Err: "unauthorised"}
		}
		return &fclient.UserInfo{Sub: "@mock:example.com"}, nil
	}

	for _, tc := range []struct {
		name                 string
		matrixID             string
		delayId              string
		delayTimeout         int
		expectJoinTokenError bool
		expectExchangeError  bool
		expectCreateRoom     bool
		expectError          bool
	}{
		{name: "Full access — all OK", matrixID: "@user:example.com", expectCreateRoom: true},
		{name: "Restricted — all OK", matrixID: "@user:other.com"},
		{name: "Exchange fails", matrixID: "@user:example.com", expectExchangeError: true, expectError: true},
		{name: "Token key empty", matrixID: "@user:example.com", expectJoinTokenError: true, expectError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calledCreateLiveKitRoom = false
			failExchange = tc.expectExchangeError

			apiKey := "the_api_key"
			if tc.expectJoinTokenError {
				apiKey = ""
			}
			handler := NewHandler(
				LiveKitAuth{key: apiKey, secret: "secret", lkUrl: "wss://lk.local:8080/foo"},
				false, []string{"example.com"},
				0, // sanityCheckInterval disabled
				map[string]string{},
			)
			req := &LegacySFURequest{
				Room:         "!room:example.com",
				OpenIDToken:  OpenIDTokenType{AccessToken: "token", MatrixServerName: strings.Split(tc.matrixID, ":")[1]},
				DeviceID:     "dev",
				DelayId:      tc.delayId,
				DelayTimeout: tc.delayTimeout,
			}
			_, err := handler.processLegacySFURequest(&http.Request{}, req)
			if tc.expectError && err == nil {
				t.Fatal("expected error but got nil")
			}
			if !tc.expectError && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if calledCreateLiveKitRoom != tc.expectCreateRoom {
				t.Errorf("createLiveKitRoom called=%v, want %v", calledCreateLiveKitRoom, tc.expectCreateRoom)
			}
		})
	}
}

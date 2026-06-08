// Copyright 2025 Element Creations Ltd.
// Copyright 2023 - 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// get_token_test.go: end-to-end tests for the /get_token endpoint —
// HTTP method/options/JSON handling, the full POST happy-path (with JWT
// inspection), the unauthorised-user case, and the underlying
// Handler.processSFURequest method (handler.go).

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/matrix-org/gomatrixserverlib/fclient"
)

// TestHandle_MethodNotAllowed verifies that non-POST/OPTIONS requests to
// /get_token return 405.
func TestHandle_MethodNotAllowed(t *testing.T) {
	handler := NewHandler(
		LiveKitAuth{key: "key", secret: "secret", lkUrl: "wss://lk.local"},
		false, []string{"*"},
		0, // sanityCheckInterval disabled
	)
	t.Cleanup(handler.Close)

	for _, method := range []string{"GET", "PUT", "DELETE", "PATCH"} {
		req := httptest.NewRequest(method, "/get_token", nil)
		rr := httptest.NewRecorder()
		handler.prepareMux().ServeHTTP(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /get_token: expected 405, got %d", method, rr.Code)
		}
	}
}

// TestHandle_Options verifies that OPTIONS /get_token returns 200.
func TestHandle_Options(t *testing.T) {
	handler := NewHandler(
		LiveKitAuth{key: "key", secret: "secret", lkUrl: "wss://lk.local"},
		false, []string{"*"},
		0, // sanityCheckInterval disabled
	)
	t.Cleanup(handler.Close)
	req := httptest.NewRequest("OPTIONS", "/get_token", nil)
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for OPTIONS, got %d", rr.Code)
	}
}

// TestHandle_InvalidJSON verifies that malformed JSON to /get_token returns 400.
func TestHandle_InvalidJSON(t *testing.T) {
	handler := NewHandler(
		LiveKitAuth{key: "key", secret: "secret", lkUrl: "wss://lk.local"},
		false, []string{"*"},
		0, // sanityCheckInterval disabled
	)
	t.Cleanup(handler.Close)
	req := httptest.NewRequest("POST", "/get_token", strings.NewReader("{invalid json}"))
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", rr.Code)
	}
}

func TestHandlePost(t *testing.T) {
	handler := NewHandler(
		LiveKitAuth{secret: "testSecret", key: "testKey", lkUrl: "wss://lk.local:8080/foo"},
		true, []string{"example.com"},
		0, // sanityCheckInterval disabled
	)

	var matrixServerName string
	testServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/federation/v1/openid/userinfo" {
			t.Errorf("unexpected path: got %v", r.URL.Path)
		}
		if accessToken := r.URL.Query().Get("access_token"); accessToken != "testAccessToken" {
			t.Errorf("unexpected access token: got %v", accessToken)
		}
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprintf(w, `{"sub": "@alice:%s"}`, matrixServerName); err != nil {
			t.Fatalf("failed to write response: %v", err)
		}
	}))
	defer testServer.Close()

	u, _ := url.Parse(testServer.URL)
	matrixServerName = u.Host

	testCase := map[string]interface{}{
		// from https://github.com/hughns/matrix-spec-proposals/blob/hughns/matrixrtc-livekit/proposals/4195-matrixrtc-livekit.md#test-vectors
		"room_id": "!roomid:example.com", "slot_id": "slot123",
		"openid_token": map[string]interface{}{
			"access_token": "testAccessToken", "token_type": "testTokenType",
			"matrix_server_name": u.Host, "expires_in": 3600,
		},
		"member": map[string]interface{}{
			"id": "memberABC", "claimed_user_id": "@alice:" + matrixServerName,
			"claimed_device_id": "DEVICE123",
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

	var resp SFUResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Errorf("failed to decode response body: %v", err)
	}
	if resp.URL != "wss://lk.local:8080/foo" {
		t.Errorf("unexpected URL: got %v", resp.URL)
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
	// from https://github.com/hughns/matrix-spec-proposals/blob/hughns/matrixrtc-livekit/proposals/4195-matrixrtc-livekit.md#test-vectors

	wantSubRawBytes, err := json.Marshal([]string{"@alice:" + matrixServerName, "DEVICE123", "memberABC"})
	if err != nil {
		panic("unreachable, probably")
	}
	wantSubIdentityRaw := string(wantSubRawBytes)
	wantSubIdentityHash := sha256.Sum256([]byte(wantSubIdentityRaw))
	wantSub := unpaddedBase64.EncodeToString(wantSubIdentityHash[:])
	if claims["sub"] != wantSub {
		t.Errorf("unexpected sub: got %v want %v", claims["sub"], wantSub)
	}

	// from https://github.com/hughns/matrix-spec-proposals/blob/hughns/matrixrtc-livekit/proposals/4195-matrixrtc-livekit.md#test-vectors
	wantRoom := "AUDmNDQiVHmWYRE+rKBvieWX8AUSzepenuj6u+d/n9c"
	if claims["video"].(map[string]interface{})["room"] != wantRoom {
		t.Errorf("unexpected room: got %v want %v", claims["video"].(map[string]interface{})["room"], wantRoom)
	}
}

func TestHandle_UnauthorizedUser(t *testing.T) {
	originalExchange := exchangeOpenIdUserInfo
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		return &fclient.UserInfo{Sub: "@real:example.com"}, nil
	}

	handler := NewHandler(
		LiveKitAuth{key: "key", secret: "secret", lkUrl: "wss://lk.local"},
		false, []string{"*"},
		0, // sanityCheckInterval disabled
	)
	t.Cleanup(handler.Close)

	body, _ := json.Marshal(map[string]interface{}{
		"room_id": "!room:example.com", "slot_id": "slot",
		"openid_token": map[string]interface{}{"access_token": "tok", "matrix_server_name": "example.com"},
		"member":       map[string]interface{}{"id": "mid", "claimed_user_id": "@attacker:example.com", "claimed_device_id": "dev"},
	})
	req := httptest.NewRequest("POST", "/get_token", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for mismatched user, got %d", rr.Code)
	}
}

func TestProcessSFURequest(t *testing.T) {
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
	var exchangeMatrixID string
	originalExchange := exchangeOpenIdUserInfo
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		if failExchange {
			return nil, &MatrixErrorResponse{Status: http.StatusUnauthorized, ErrCode: "M_UNAUTHORIZED", Err: "unauthorised"}
		}
		return &fclient.UserInfo{Sub: exchangeMatrixID}, nil
	}

	for _, tc := range []struct {
		name                 string
		matrixID             string
		claimedMatrixID      string
		expectJoinTokenError bool
		expectExchangeError  bool
		expectCreateRoom     bool
		expectError          bool
	}{
		{name: "Full access — all OK", matrixID: "@user:example.com", claimedMatrixID: "@user:example.com", expectCreateRoom: true},
		{name: "Restricted — all OK", matrixID: "@user:other.com", claimedMatrixID: "@user:other.com"},
		{name: "Exchange fails", matrixID: "@user:example.com", claimedMatrixID: "@user:example.com", expectExchangeError: true, expectError: true},
		{name: "Token key empty", matrixID: "@user:example.com", claimedMatrixID: "@user:example.com", expectJoinTokenError: true, expectError: true},
		{name: "ClaimedUserID mismatch", matrixID: "@user:example.com", claimedMatrixID: "@user:faked.com", expectError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calledCreateLiveKitRoom = false
			failExchange = tc.expectExchangeError
			exchangeMatrixID = tc.matrixID

			apiKey := "the_api_key"
			if tc.expectJoinTokenError {
				apiKey = ""
			}
			handler := NewHandler(
				LiveKitAuth{key: apiKey, secret: "secret", lkUrl: "wss://lk.local:8080/foo"},
				false, []string{"example.com"},
				0, // sanityCheckInterval disabled
			)
			req := &SFURequest{
				RoomID: "!room:example.com", SlotID: "slot",
				OpenIDToken: OpenIDTokenType{AccessToken: "token", MatrixServerName: strings.Split(tc.claimedMatrixID, ":")[1]},
				Member:      MatrixRTCMemberType{ID: "device", ClaimedUserID: tc.claimedMatrixID, ClaimedDeviceID: "dev"},
			}
			_, err := handler.processSFURequest(&http.Request{}, req)
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

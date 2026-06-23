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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/matrix-org/gomatrixserverlib/fclient"
)

// TestHandleGetToken_MethodNotAllowed verifies that non-POST/OPTIONS requests to
// /get_token return 405.
func TestHandleGetToken_MethodNotAllowed(t *testing.T) {
	handler := newGetTokenHandler(t)
	for _, method := range []string{"GET", "PUT", "DELETE", "PATCH"} {
		req := httptest.NewRequest(method, "/get_token", nil)
		rr := httptest.NewRecorder()
		handler.prepareMux().ServeHTTP(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /get_token: expected 405, got %d", method, rr.Code)
		}
	}
}

// TestHandleGetToken_Options verifies that OPTIONS /get_token returns 200.
func TestHandleGetToken_Options(t *testing.T) {
	handler := newGetTokenHandler(t)
	req := httptest.NewRequest("OPTIONS", "/get_token", nil)
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for OPTIONS, got %d", rr.Code)
	}
}

// TestHandleGetToken_InvalidJSON verifies that malformed JSON to /get_token returns 400.
func TestHandleGetToken_InvalidJSON(t *testing.T) {
	handler := newGetTokenHandler(t)
	req := httptest.NewRequest("POST", "/get_token", strings.NewReader("{invalid json}"))
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", rr.Code)
	}
}

// TestHandleGetToken_Success verifies the full-access happy-path POST
// /get_token: a valid request produces a 200 SFUResponse, CreateLiveKitRoom
// is invoked, and the JWT's sub/room claims match the MSC-4195 test vectors.
//
// The OpenID exchange and LiveKit room creation are mocked here — the real
// exchangeOpenIdUserInfo path is exercised by TestExchangeOpenIdUserInfo in
// helper_test.go.
func TestHandleGetToken_Success(t *testing.T) {
	const matrixServerName = "example.com"
	const claimedUserID = "@alice:" + matrixServerName

	originalExchange := exchangeOpenIdUserInfo
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		return &fclient.UserInfo{Sub: claimedUserID}, nil
	}

	originalCreate := CreateLiveKitRoom
	t.Cleanup(func() { CreateLiveKitRoom = originalCreate })
	var createCalled bool
	CreateLiveKitRoom = func(_ context.Context, _ *LiveKitAuth, _ LiveKitRoomAlias, _ string, _ LiveKitIdentity) error {
		createCalled = true
		return nil
	}

	handler := newGetTokenHandler(t)

	// Inputs match the MSC-4195 test vectors exactly:
	// https://github.com/hughns/matrix-spec-proposals/blob/hughns/matrixrtc-livekit/proposals/4195-matrixrtc-livekit.md#test-vectors
	body := marshalSFURequest(t, func(r *SFURequest) {
		r.RoomID = "!roomid:example.com"
		r.SlotID = "slot1234"
		r.OpenIDToken.MatrixServerName = matrixServerName
		r.Member.ID = "memberABC"
		r.Member.ClaimedUserID = claimedUserID
		r.Member.ClaimedDeviceID = "DEVICE123"
	})
	req := httptest.NewRequest("POST", "/get_token", body)
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
	if resp.URL != handler.liveKitAuth.lkUrl {
		t.Errorf("resp.URL = %q, want %q", resp.URL, handler.liveKitAuth.lkUrl)
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

	// MSC-4195 identity hash of (claimed_user_id, claimed_device_id, member.id).
	// https://github.com/hughns/matrix-spec-proposals/blob/hughns/matrixrtc-livekit/proposals/4195-matrixrtc-livekit.md#test-vectors
	wantSubRawBytes, _ := json.Marshal([]string{claimedUserID, "DEVICE123", "memberABC"})
	wantSubHash := sha256.Sum256(wantSubRawBytes)
	wantSub := unpaddedBase64.EncodeToString(wantSubHash[:])
	if claims["sub"] != wantSub {
		t.Errorf("sub = %v, want %v", claims["sub"], wantSub)
	}
	// MSC-4195 room hash of ("!roomid:example.com", "slot123").
	// https://github.com/hughns/matrix-spec-proposals/blob/hughns/matrixrtc-livekit/proposals/4195-matrixrtc-livekit.md#test-vectors
	const wantRoom = "O8437W3+jmzMVjoIP3tNwbm+XxHQk2iKpOA7aqw3qSc"
	if got := claims["video"].(map[string]interface{})["room"]; got != wantRoom {
		t.Errorf("room = %v, want %v", got, wantRoom)
	}
}

// TestHandleGetToken_UnauthorizedUser verifies that a mismatch between the
// OpenID-validated sub and the claimed_user_id in the request body returns
// 401.
func TestHandleGetToken_UnauthorizedUser(t *testing.T) {
	originalExchange := exchangeOpenIdUserInfo
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		return &fclient.UserInfo{Sub: "@real:example.com"}, nil
	}

	handler := newGetTokenHandler(t)
	body := marshalSFURequest(t, func(r *SFURequest) {
		r.Member.ClaimedUserID = "@attacker:example.com" // mismatch vs Sub
	})
	req := httptest.NewRequest("POST", "/get_token", body)
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for mismatched user, got %d", rr.Code)
	}
}

// TestHandleGetToken_RestrictedUser verifies that a request from a homeserver
// that is not on the full-access list, with delayed-event delegation params,
// returns 400 / M_BAD_JSON. Delegation is gated on full-access; restricted
// users may join existing rooms but not delegate delayed-event handling.
func TestHandleGetToken_RestrictedUser(t *testing.T) {
	originalExchange := exchangeOpenIdUserInfo
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		return &fclient.UserInfo{Sub: "@user:restricted.com"}, nil
	}

	// newGetTokenHandler configures full access only for example.com.
	handler := newGetTokenHandler(t)
	body := marshalSFURequest(t, func(r *SFURequest) {
		r.Member.ClaimedUserID = "@user:restricted.com"
		r.OpenIDToken.MatrixServerName = "restricted.com" // not in full-access list
		// Delegation params trigger the restricted-user reject path.
		r.DelayId = "delay-id"
		r.DelayTimeout = 30000 // 30 s in ms
		r.DelayCsApiUrl = "https://restricted.com"
	})
	req := httptest.NewRequest("POST", "/get_token", body)
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for restricted user requesting delegation, got %d", rr.Code)
	}
}

// TestHandleGetToken_ExchangeError verifies that an OpenID lookup failure
// surfaces as 401 / M_UNAUTHORIZED — the request couldn't be authorised.
func TestHandleGetToken_ExchangeError(t *testing.T) {
	originalExchange := exchangeOpenIdUserInfo
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		return nil, &MatrixErrorResponse{
			Status: http.StatusUnauthorized, ErrCode: "M_UNAUTHORIZED", Err: "no",
		}
	}

	handler := newGetTokenHandler(t)
	req := httptest.NewRequest("POST", "/get_token", marshalSFURequest(t, nil))
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 when OpenID exchange fails, got %d", rr.Code)
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
		delayId              string
		delayTimeout         int
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
				map[string]string{},
			)
			req := &SFURequest{
				RoomID: "!room:example.com", SlotID: "slot",
				OpenIDToken:  OpenIDTokenType{AccessToken: "token", MatrixServerName: strings.Split(tc.claimedMatrixID, ":")[1]},
				Member:       MatrixRTCMemberType{ID: "device", ClaimedUserID: tc.claimedMatrixID, ClaimedDeviceID: "dev"},
				DelayId:      tc.delayId,
				DelayTimeout: tc.delayTimeout,
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

// ── helpers ───────────────────────────────────────────────────────────────────

// validSFURequest returns a fully populated, valid request. The default
// homeserver is "example.com" (matches newGetTokenHandler's full-access list).
func validSFURequest() SFURequest {
	return SFURequest{
		RoomID: "!testRoom:example.com",
		SlotID: "m.call#ROOM",
		OpenIDToken: OpenIDTokenType{
			AccessToken:      "test-token",
			MatrixServerName: "example.com",
		},
		Member: MatrixRTCMemberType{
			ID:              "member-id",
			ClaimedUserID:   "@user:example.com",
			ClaimedDeviceID: "device-id",
		},
	}
}

// marshalSFURequest returns a valid request body with an optional mutation
// applied before marshalling.
func marshalSFURequest(t *testing.T, mutate func(*SFURequest)) *bytes.Reader {
	t.Helper()
	req := validSFURequest()
	if mutate != nil {
		mutate(&req)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}
	return bytes.NewReader(body)
}

// newGetTokenHandler creates a Handler configured for testing /get_token,
// with example.com as the only full-access homeserver and a LIFO cleanup.
func newGetTokenHandler(t *testing.T) *Handler {
	t.Helper()
	handler := NewHandler(
		LiveKitAuth{key: "key", secret: "secret", lkUrl: "wss://lk.local"},
		false,
		[]string{"example.com"},
		0, // sanityCheckInterval disabled
		map[string]string{},
	)
	t.Cleanup(handler.Close)
	return handler
}

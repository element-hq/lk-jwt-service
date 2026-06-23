// Copyright 2025 Element Creations Ltd.
// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// delegate_delayed_leave_test.go: tests for the
// POST /delegate_delayed_leave endpoint and its supporting types
// (handler.go: handleDelegateDelayedLeave, processDelegateDelayedLeave).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib/fclient"
)

// ── handleDelegateDelayedLeave HTTP handler ───────────────────────────────────

func TestHandleDelegateDelayedLeave_Options(t *testing.T) {
	handler := newDelegateDelayedLeaveHandler(t)
	req := httptest.NewRequest("OPTIONS", "/delegate_delayed_leave", nil)
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for OPTIONS, got %d", rr.Code)
	}
}

func TestHandleDelegateDelayedLeave_MethodNotAllowed(t *testing.T) {
	handler := newDelegateDelayedLeaveHandler(t)
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		req := httptest.NewRequest(method, "/delegate_delayed_leave", nil)
		rr := httptest.NewRecorder()
		handler.prepareMux().ServeHTTP(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: expected 405, got %d", method, rr.Code)
		}
	}
}

func TestHandleDelegateDelayedLeave_InvalidJSON(t *testing.T) {
	handler := newDelegateDelayedLeaveHandler(t)
	req := httptest.NewRequest("POST", "/delegate_delayed_leave", bytes.NewBufferString("{bad json}"))
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", rr.Code)
	}
}

func TestHandleDelegateDelayedLeave_MissingFields(t *testing.T) {
	handler := newDelegateDelayedLeaveHandler(t)
	body, _ := json.Marshal(map[string]interface{}{})
	req := httptest.NewRequest("POST", "/delegate_delayed_leave", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing fields, got %d", rr.Code)
	}
}

func TestHandleDelegateDelayedLeave_UnauthorizedUser(t *testing.T) {
	originalExchange := exchangeOpenIdUserInfo
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		return &fclient.UserInfo{Sub: "@real:example.com"}, nil
	}

	handler := newDelegateDelayedLeaveHandler(t)
	body := marshalDelegateDelayedLeaveRequest(t, func(r *DelegateDelayedLeaveRequest) {
		r.Member.ClaimedUserID = "@attacker:example.com" // mismatch
	})
	req := httptest.NewRequest("POST", "/delegate_delayed_leave", body)
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for mismatched user, got %d", rr.Code)
	}
}

func TestHandleDelegateDelayedLeave_RestrictedUser(t *testing.T) {
	originalExchange := exchangeOpenIdUserInfo
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		return &fclient.UserInfo{Sub: "@user:restricted.com"}, nil
	}

	// Handler configured with only "example.com" as full-access.
	handler := newDelegateDelayedLeaveHandler(t)
	body := marshalDelegateDelayedLeaveRequest(t, func(r *DelegateDelayedLeaveRequest) {
		r.Member.ClaimedUserID = "@user:restricted.com"
		r.OpenIDToken.MatrixServerName = "restricted.com" // not in full-access list
	})
	req := httptest.NewRequest("POST", "/delegate_delayed_leave", body)
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403 for restricted user, got %d", rr.Code)
	}
}

func TestHandleDelegateDelayedLeave_ExchangeError(t *testing.T) {
	originalExchange := exchangeOpenIdUserInfo
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		return nil, &MatrixErrorResponse{Status: http.StatusUnauthorized, ErrCode: "M_UNAUTHORIZED", Err: "no"}
	}

	handler := newDelegateDelayedLeaveHandler(t)
	body := marshalDelegateDelayedLeaveRequest(t, nil)
	req := httptest.NewRequest("POST", "/delegate_delayed_leave", body)
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 when exchange fails, got %d", rr.Code)
	}
}

func TestHandleDelegateDelayedLeave_Success(t *testing.T) {
	originalExchange := exchangeOpenIdUserInfo
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		return &fclient.UserInfo{Sub: "@user:example.com"}, nil
	}

	originalLookup := LiveKitParticipantExists
	t.Cleanup(func() { LiveKitParticipantExists = originalLookup })
	LiveKitParticipantExists = func(ctx context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, _ LiveKitIdentity) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}

	handler := newDelegateDelayedLeaveHandler(t) // registers handler.Close last → runs first
	body := marshalDelegateDelayedLeaveRequest(t, nil)
	req := httptest.NewRequest("POST", "/delegate_delayed_leave", body)
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for valid request, got %d", rr.Code)
	}
	// Response body should be empty (no JWT returned).
	if strings.TrimSpace(rr.Body.String()) != "{}" {
		t.Errorf("expected empty object in response body, got: %s", rr.Body.String())
	}
}

// TestHandleDelegateDelayedLeave_NoJWT verifies that the endpoint does NOT
// return a JWT — differentiating it from /get_token.
func TestHandleDelegateDelayedLeave_NoJWT(t *testing.T) {
	originalExchange := exchangeOpenIdUserInfo
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		return &fclient.UserInfo{Sub: "@user:example.com"}, nil
	}

	originalLookup := LiveKitParticipantExists
	t.Cleanup(func() { LiveKitParticipantExists = originalLookup })
	LiveKitParticipantExists = func(ctx context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, _ LiveKitIdentity) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}

	handler := newDelegateDelayedLeaveHandler(t) // registers handler.Close last → runs first
	body := marshalDelegateDelayedLeaveRequest(t, nil)
	req := httptest.NewRequest("POST", "/delegate_delayed_leave", body)
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)

	// Must not contain a JWT field.
	var respMap map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&respMap); err == nil {
		if _, hasJWT := respMap["jwt"]; hasJWT {
			t.Error("response must not contain a JWT")
		}
		if _, hasURL := respMap["url"]; hasURL {
			t.Error("response must not contain a url")
		}
	}
	// 200 with empty body is the correct success response.
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

// ── processDelegateDelayedLeave unit test ─────────────────────────────────────

// TestProcessDelegateDelayedLeave_CreatesJob verifies that a successful call
// to processDelegateDelayedLeave hands a job over to the monitor without
// creating a LiveKit room or token.
func TestProcessDelegateDelayedLeave_CreatesJob(t *testing.T) {
	originalCreate := CreateLiveKitRoom
	t.Cleanup(func() { CreateLiveKitRoom = originalCreate })
	createRoomCalled := false
	CreateLiveKitRoom = func(_ context.Context, _ *LiveKitAuth, _ LiveKitRoomAlias, _ string, _ LiveKitIdentity) error {
		createRoomCalled = true
		return nil
	}

	originalExchange := exchangeOpenIdUserInfo
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		return &fclient.UserInfo{Sub: "@user:example.com"}, nil
	}

	originalLookup := LiveKitParticipantExists
	t.Cleanup(func() { LiveKitParticipantExists = originalLookup })
	LiveKitParticipantExists = func(ctx context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, _ LiveKitIdentity) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}

	handler := newDelegateDelayedLeaveHandler(t) // registers handler.Close last → runs first
	req := validDelegateDelayedLeaveRequest()
	resp, err := handler.processDelegateDelayedLeave(&http.Request{}, &req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatalf("missing response")
	}

	if createRoomCalled {
		t.Error("processDelegateDelayedLeave must NOT call CreateLiveKitRoom")
	}

	// Give the job a moment to be registered, then verify the monitor exists.
	time.Sleep(50 * time.Millisecond)
}

// TestProcessDelegateDelayedLeave_InvalidDelayTimeout verifies that a job
// rejected by NewDelayedEventJob (DelayTimeout <= 0, bypassing request
// parsing) surfaces as 400 M_BAD_JSON carrying the creation error.
func TestProcessDelegateDelayedLeave_InvalidDelayTimeout(t *testing.T) {
	originalExchange := exchangeOpenIdUserInfo
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		return &fclient.UserInfo{Sub: "@user:example.com"}, nil
	}

	handler := newDelegateDelayedLeaveHandler(t)
	req := validDelegateDelayedLeaveRequest()
	req.DelayTimeout = 0 // invalid — would be rejected by request parsing, too

	resp, err := handler.processDelegateDelayedLeave(&http.Request{}, &req)
	if resp != nil {
		t.Errorf("expected no response, got %v", resp)
	}
	var mErr *MatrixErrorResponse
	if !errors.As(err, &mErr) {
		t.Fatalf("expected MatrixErrorResponse, got %v", err)
	}
	if mErr.Status != http.StatusBadRequest || mErr.ErrCode != "M_BAD_JSON" {
		t.Errorf("expected 400 M_BAD_JSON, got %d %s", mErr.Status, mErr.ErrCode)
	}
	if mErr.Err == "" {
		t.Error("expected the job-creation error as message, got empty string")
	}
}

// TestProcessDelegateDelayedLeave_AfterShutdown verifies that a delegation
// request hitting an already-shut-down handler surfaces as 503 M_UNKNOWN
// (not M_BAD_JSON — the request itself was fine).
func TestProcessDelegateDelayedLeave_AfterShutdown(t *testing.T) {
	originalExchange := exchangeOpenIdUserInfo
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		return &fclient.UserInfo{Sub: "@user:example.com"}, nil
	}

	handler := newDelegateDelayedLeaveHandler(t)
	handler.Close() // idempotent — the t.Cleanup Close is a no-op afterwards

	req := validDelegateDelayedLeaveRequest()
	resp, err := handler.processDelegateDelayedLeave(&http.Request{}, &req)
	if resp != nil {
		t.Errorf("expected no response, got %v", resp)
	}
	var mErr *MatrixErrorResponse
	if !errors.As(err, &mErr) {
		t.Fatalf("expected MatrixErrorResponse, got %v", err)
	}
	if mErr.Status != http.StatusServiceUnavailable || mErr.ErrCode != "M_UNKNOWN" {
		t.Errorf("expected 503 M_UNKNOWN, got %d %s", mErr.Status, mErr.ErrCode)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// validDelegateDelayedLeaveRequest returns a fully populated, valid request.
func validDelegateDelayedLeaveRequest() DelegateDelayedLeaveRequest {
	return DelegateDelayedLeaveRequest{
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
		DelayId:       "syd_delay123",
		DelayTimeout:  30000, // 30 s in ms
		DelayCsApiUrl: "https://matrix.example.com",
	}
}

// newDelegateDelayedLeaveHandler creates a Handler configured for testing
// the /delegate_delayed_leave endpoint, with a LIFO cleanup.
func newDelegateDelayedLeaveHandler(t *testing.T) *Handler {
	t.Helper()
	handler := NewHandler(
		LiveKitAuth{key: "key", secret: "secret", lkUrl: "ws://localhost:7880"},
		false,
		[]string{"example.com"},
		0, // sanityCheckInterval disabled
		map[string]string{},
	)
	t.Cleanup(handler.Close)
	return handler
}

// marshalDelegateDelayedLeaveRequest returns a valid request body with an
// optional mutation applied before marshalling.
func marshalDelegateDelayedLeaveRequest(t *testing.T, mutate func(*DelegateDelayedLeaveRequest)) *bytes.Reader {
	t.Helper()
	req := validDelegateDelayedLeaveRequest()
	if mutate != nil {
		mutate(&req)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}
	return bytes.NewReader(body)
}

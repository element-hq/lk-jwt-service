// Copyright 2025 Element Creations Ltd.
// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// membership_leave_delegation_test.go: tests for the
// POST /membership_leave_delegation endpoint and its supporting types
// (handler.go: handleMembershipLeaveDelegation, processMembershipLeaveDelegation).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib/fclient"
)

// ── handleMembershipLeaveDelegation HTTP handler ───────────────────────────────────

func TestHandleMembershipLeaveDelegation_Options(t *testing.T) {
	handler := newMembershipLeaveDelegationHandler(t)
	req := httptest.NewRequest("OPTIONS", "/membership_leave_delegation", nil)
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for OPTIONS, got %d", rr.Code)
	}
}

func TestHandleMembershipLeaveDelegation_MethodNotAllowed(t *testing.T) {
	handler := newMembershipLeaveDelegationHandler(t)
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		req := httptest.NewRequest(method, "/membership_leave_delegation", nil)
		rr := httptest.NewRecorder()
		handler.prepareMux().ServeHTTP(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: expected 405, got %d", method, rr.Code)
		}
	}
}

func TestHandleMembershipLeaveDelegation_InvalidJSON(t *testing.T) {
	handler := newMembershipLeaveDelegationHandler(t)
	req := httptest.NewRequest("POST", "/membership_leave_delegation", bytes.NewBufferString("{bad json}"))
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", rr.Code)
	}
}

func TestHandleMembershipLeaveDelegation_MissingFields(t *testing.T) {
	handler := newMembershipLeaveDelegationHandler(t)
	body, _ := json.Marshal(map[string]interface{}{})
	req := httptest.NewRequest("POST", "/membership_leave_delegation", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing fields, got %d", rr.Code)
	}
}

func TestHandleMembershipLeaveDelegation_UnauthorizedUser(t *testing.T) {
	originalExchange := exchangeOpenIdUserInfo
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		return &fclient.UserInfo{Sub: "@real:example.com"}, nil
	}

	handler := newMembershipLeaveDelegationHandler(t)
	body := marshalMembershipLeaveDelegationRequest(t, func(r *MembershipLeaveDelegationRequest) {
		r.Member.ClaimedUserID = "@attacker:example.com" // mismatch
	})
	req := httptest.NewRequest("POST", "/membership_leave_delegation", body)
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for mismatched user, got %d", rr.Code)
	}
}

func TestHandleMembershipLeaveDelegation_RestrictedUser(t *testing.T) {
	originalExchange := exchangeOpenIdUserInfo
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		return &fclient.UserInfo{Sub: "@user:restricted.com"}, nil
	}

	// Handler configured with only "example.com" as full-access.
	handler := newMembershipLeaveDelegationHandler(t)
	body := marshalMembershipLeaveDelegationRequest(t, func(r *MembershipLeaveDelegationRequest) {
		r.Member.ClaimedUserID = "@user:restricted.com"
		r.OpenIDToken.MatrixServerName = "restricted.com" // not in full-access list
	})
	req := httptest.NewRequest("POST", "/membership_leave_delegation", body)
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403 for restricted user, got %d", rr.Code)
	}
}

func TestHandleMembershipLeaveDelegation_ExchangeError(t *testing.T) {
	originalExchange := exchangeOpenIdUserInfo
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		return nil, &MatrixErrorResponse{Status: http.StatusUnauthorized, ErrCode: "M_UNAUTHORIZED", Err: "no"}
	}

	handler := newMembershipLeaveDelegationHandler(t)
	body := marshalMembershipLeaveDelegationRequest(t, nil)
	req := httptest.NewRequest("POST", "/membership_leave_delegation", body)
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 when exchange fails, got %d", rr.Code)
	}
}

func TestHandleMembershipLeaveDelegation_Success(t *testing.T) {
	// LIFO: register restores FIRST (run last), handler.Close LAST (runs first).
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

	handler := newMembershipLeaveDelegationHandler(t) // registers handler.Close last → runs first
	body := marshalMembershipLeaveDelegationRequest(t, nil)
	req := httptest.NewRequest("POST", "/membership_leave_delegation", body)
	rr := httptest.NewRecorder()
	handler.prepareMux().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for valid request, got %d", rr.Code)
	}
	// Response body should be empty (no JWT returned).
	if rr.Body.Len() > 0 {
		t.Errorf("expected empty response body, got: %s", rr.Body.String())
	}
}

// TestHandleMembershipLeaveDelegation_NoJWT verifies that the endpoint does NOT
// return a JWT — differentiating it from /get_token.
func TestHandleMembershipLeaveDelegation_NoJWT(t *testing.T) {
	// LIFO: register restores FIRST (run last), handler.Close LAST (runs first).
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

	handler := newMembershipLeaveDelegationHandler(t) // registers handler.Close last → runs first
	body := marshalMembershipLeaveDelegationRequest(t, nil)
	req := httptest.NewRequest("POST", "/membership_leave_delegation", body)
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

// ── processMembershipLeaveDelegation unit test ─────────────────────────────────────

// TestProcessMembershipLeaveDelegation_CreatesJob verifies that a successful call
// to processMembershipLeaveDelegation hands a job over to the monitor without
// creating a LiveKit room or token.
func TestProcessMembershipLeaveDelegation_CreatesJob(t *testing.T) {
	// LIFO: register restores FIRST (run last), handler.Close LAST (runs first).
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

	handler := newMembershipLeaveDelegationHandler(t) // registers handler.Close last → runs first
	req := validMembershipLeaveDelegationRequest()
	if err := handler.processMembershipLeaveDelegation(&http.Request{}, &req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if createRoomCalled {
		t.Error("processMembershipLeaveDelegation must NOT call CreateLiveKitRoom")
	}

	// Give the job a moment to be registered, then verify the monitor exists.
	time.Sleep(50 * time.Millisecond)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// validMembershipLeaveDelegationRequest returns a fully populated, valid request.
func validMembershipLeaveDelegationRequest() MembershipLeaveDelegationRequest {
	return MembershipLeaveDelegationRequest{
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

// newMembershipLeaveDelegationHandler creates a Handler configured for testing
// the /membership_leave_delegation endpoint, with a LIFO cleanup.
func newMembershipLeaveDelegationHandler(t *testing.T) *Handler {
	t.Helper()
	handler := NewHandler(
		LiveKitAuth{key: "key", secret: "secret", lkUrl: "ws://localhost:7880"},
		false,
		[]string{"example.com"},
		0, // sanityCheckInterval disabled
	)
	t.Cleanup(handler.Close)
	return handler
}

// marshalMembershipLeaveDelegationRequest returns a valid request body with an
// optional mutation applied before marshalling.
func marshalMembershipLeaveDelegationRequest(t *testing.T, mutate func(*MembershipLeaveDelegationRequest)) *bytes.Reader {
	t.Helper()
	req := validMembershipLeaveDelegationRequest()
	if mutate != nil {
		mutate(&req)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}
	return bytes.NewReader(body)
}

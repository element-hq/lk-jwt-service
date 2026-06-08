// Copyright 2025 Element Creations Ltd.
// Copyright 2023 - 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// requests_test.go: tests for the request / response DTOs and their
// Validate() / Error() methods (requests.go).

package main

import "testing"

// TestMatrixErrorResponse_Error verifies the Error() string method.
func TestMatrixErrorResponse_Error(t *testing.T) {
	err := &MatrixErrorResponse{Status: 400, ErrCode: "M_BAD_JSON", Err: "bad input"}
	if err.Error() != "bad input" {
		t.Errorf("Error() = %q, want %q", err.Error(), "bad input")
	}
}

// ── SFURequest / LegacySFURequest validation ──────────────────────────────────

// TestLegacySFURequest_Validate_DelayedEventPartialParams verifies that providing
// only some delayed-event parameters returns M_BAD_JSON.
func TestLegacySFURequest_Validate_DelayedEventPartialParams(t *testing.T) {
	for _, c := range []struct {
		name    string
		delayID string
		timeout int
		csURL   string
	}{
		{"only delay_id", "did", 0, ""},
		{"only timeout", "", 1000, ""},
		{"only cs_api_url", "", 0, "https://example.com"},
		{"delay_id + timeout", "did", 1000, ""},
		{"delay_id + cs_api_url", "did", 0, "https://example.com"},
		{"timeout + cs_api_url", "", 1000, "https://example.com"},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := &LegacySFURequest{
				Room: "!r:x", DeviceID: "DEVICE1234",
				OpenIDToken: OpenIDTokenType{AccessToken: "tok", MatrixServerName: "x"},
				DelayId:     c.delayID, DelayTimeout: c.timeout, DelayCsApiUrl: c.csURL,
			}
			if err := req.Validate(); err == nil {
				t.Error("expected validation error for partial delayed-event params, got nil")
			}
		})
	}
}

// TestSFURequest_Validate_DelayedEventPartialParams verifies that providing
// only some delayed-event parameters returns M_BAD_JSON.
func TestSFURequest_Validate_DelayedEventPartialParams(t *testing.T) {
	for _, c := range []struct {
		name    string
		delayID string
		timeout int
		csURL   string
	}{
		{"only delay_id", "did", 0, ""},
		{"only timeout", "", 1000, ""},
		{"only cs_api_url", "", 0, "https://example.com"},
		{"delay_id + timeout", "did", 1000, ""},
		{"delay_id + cs_api_url", "did", 0, "https://example.com"},
		{"timeout + cs_api_url", "", 1000, "https://example.com"},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := &SFURequest{
				RoomID: "!r:x", SlotID: "s",
				OpenIDToken: OpenIDTokenType{AccessToken: "tok", MatrixServerName: "x"},
				Member:      MatrixRTCMemberType{ID: "id", ClaimedUserID: "@u:x", ClaimedDeviceID: "d"},
				DelayId:     c.delayID, DelayTimeout: c.timeout, DelayCsApiUrl: c.csURL,
			}
			if err := req.Validate(); err == nil {
				t.Error("expected validation error for partial delayed-event params, got nil")
			}
		})
	}
}

// ── MembershipLeaveDelegationRequest.Validate() ───────────────────────────────
// Helpers (validMembershipLeaveDelegationRequest, assertValidationError) live
// in membership_leave_delegation_test.go and are visible because they're in
// the same package.

func TestMembershipLeaveDelegationRequest_Validate_Valid(t *testing.T) {
	req := validMembershipLeaveDelegationRequest()
	if err := req.Validate(); err != nil {
		t.Errorf("expected no error for valid request, got: %v", err)
	}
}

func TestMembershipLeaveDelegationRequest_Validate_MissingRoomID(t *testing.T) {
	req := validMembershipLeaveDelegationRequest()
	req.RoomID = ""
	assertValidationError(t, req.Validate(), "M_BAD_JSON")
}

func TestMembershipLeaveDelegationRequest_Validate_MissingSlotID(t *testing.T) {
	req := validMembershipLeaveDelegationRequest()
	req.SlotID = ""
	assertValidationError(t, req.Validate(), "M_BAD_JSON")
}

func TestMembershipLeaveDelegationRequest_Validate_MissingMemberFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*MembershipLeaveDelegationRequest)
	}{
		{"missing ID", func(r *MembershipLeaveDelegationRequest) { r.Member.ID = "" }},
		{"missing ClaimedUserID", func(r *MembershipLeaveDelegationRequest) { r.Member.ClaimedUserID = "" }},
		{"missing ClaimedDeviceID", func(r *MembershipLeaveDelegationRequest) { r.Member.ClaimedDeviceID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := validMembershipLeaveDelegationRequest()
			tc.mutate(&req)
			assertValidationError(t, req.Validate(), "M_BAD_JSON")
		})
	}
}

func TestMembershipLeaveDelegationRequest_Validate_MissingOpenIDToken(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*MembershipLeaveDelegationRequest)
	}{
		{"missing AccessToken", func(r *MembershipLeaveDelegationRequest) { r.OpenIDToken.AccessToken = "" }},
		{"missing MatrixServerName", func(r *MembershipLeaveDelegationRequest) { r.OpenIDToken.MatrixServerName = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := validMembershipLeaveDelegationRequest()
			tc.mutate(&req)
			assertValidationError(t, req.Validate(), "M_BAD_JSON")
		})
	}
}

func TestMembershipLeaveDelegationRequest_Validate_MissingDelayedEventParams(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*MembershipLeaveDelegationRequest)
	}{
		{"missing DelayId", func(r *MembershipLeaveDelegationRequest) { r.DelayId = "" }},
		{"zero DelayTimeout", func(r *MembershipLeaveDelegationRequest) { r.DelayTimeout = 0 }},
		{"negative DelayTimeout", func(r *MembershipLeaveDelegationRequest) { r.DelayTimeout = -1 }},
		{"missing DelayCsApiUrl", func(r *MembershipLeaveDelegationRequest) { r.DelayCsApiUrl = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := validMembershipLeaveDelegationRequest()
			tc.mutate(&req)
			assertValidationError(t, req.Validate(), "M_BAD_JSON")
		})
	}
}

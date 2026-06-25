// Copyright 2025 Element Creations Ltd.
// Copyright 2023 - 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// requests_test.go: tests for the request / response DTOs and their
// Validate() / Error() methods (requests.go).

package main

import (
	"errors"
	"testing"
)

// assertValidationError checks that err is a *MatrixErrorResponse with the
// expected error code.
func assertValidationError(t *testing.T, err error, wantErrCode string) {
	t.Helper()
	if err == nil {
		t.Error("expected validation error, got nil")
		return
	}
	var matrixErr *MatrixErrorResponse
	if !errors.As(err, &matrixErr) {
		t.Errorf("expected *MatrixErrorResponse, got %T: %v", err, err)
		return
	}
	if matrixErr.ErrCode != wantErrCode {
		t.Errorf("ErrCode = %q, want %q", matrixErr.ErrCode, wantErrCode)
	}
}

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

func TestLegacySFURequest_Validate_CsApiUrlIgnored(t *testing.T) {
	for _, c := range []struct {
		name    string
		delayID string
		timeout int
		csURL   string
	}{
		{"delay parameters but no url", "did", 1000, ""},
		{"no delay parameters and no url", "", 0, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := &LegacySFURequest{
				Room: "!r:x", DeviceID: "DEVICE1234",
				OpenIDToken: OpenIDTokenType{AccessToken: "tok", MatrixServerName: "x"},
				DelayId:     c.delayID, DelayTimeout: c.timeout, DelayCsApiUrl: c.csURL,
			}
			if err := req.Validate(); err != nil {
				t.Errorf("expected no validation error for delayed-event params without URL, got: %v", err)
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

func TestSFURequest_Validate_CsApiUrlIgnored(t *testing.T) {
	for _, c := range []struct {
		name    string
		delayID string
		timeout int
		csURL   string
	}{
		{"delay parameters but no url", "did", 1000, ""},
		{"no delay parameters and no url", "", 0, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := &SFURequest{
				RoomID: "!r:x", SlotID: "s",
				OpenIDToken: OpenIDTokenType{AccessToken: "tok", MatrixServerName: "x"},
				Member:      MatrixRTCMemberType{ID: "id", ClaimedUserID: "@u:x", ClaimedDeviceID: "d"},
				DelayId:     c.delayID, DelayTimeout: c.timeout, DelayCsApiUrl: c.csURL,
			}
			if err := req.Validate(); err != nil {
				t.Errorf("expected no validation error for delayed-event params without URL, got: %v", err)
			}
		})
	}
}

// ── DelegateDelayedLeaveRequest.Validate() ───────────────────────────────

func TestDelegateDelayedLeaveRequest_Validate_Valid(t *testing.T) {
	req := validDelegateDelayedLeaveRequest()
	if err := req.Validate(); err != nil {
		t.Errorf("expected no error for valid request, got: %v", err)
	}
}

func TestDelegateDelayedLeaveRequest_Validate_MissingRoomID(t *testing.T) {
	req := validDelegateDelayedLeaveRequest()
	req.RoomID = ""
	assertValidationError(t, req.Validate(), "M_BAD_JSON")
}

func TestDelegateDelayedLeaveRequest_Validate_MissingSlotID(t *testing.T) {
	req := validDelegateDelayedLeaveRequest()
	req.SlotID = ""
	assertValidationError(t, req.Validate(), "M_BAD_JSON")
}

func TestDelegateDelayedLeaveRequest_Validate_MissingMemberFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*DelegateDelayedLeaveRequest)
	}{
		{"missing ID", func(r *DelegateDelayedLeaveRequest) { r.Member.ID = "" }},
		{"missing ClaimedUserID", func(r *DelegateDelayedLeaveRequest) { r.Member.ClaimedUserID = "" }},
		{"missing ClaimedDeviceID", func(r *DelegateDelayedLeaveRequest) { r.Member.ClaimedDeviceID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := validDelegateDelayedLeaveRequest()
			tc.mutate(&req)
			assertValidationError(t, req.Validate(), "M_BAD_JSON")
		})
	}
}

func TestDelegateDelayedLeaveRequest_Validate_MissingOpenIDToken(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*DelegateDelayedLeaveRequest)
	}{
		{"missing AccessToken", func(r *DelegateDelayedLeaveRequest) { r.OpenIDToken.AccessToken = "" }},
		{"missing MatrixServerName", func(r *DelegateDelayedLeaveRequest) { r.OpenIDToken.MatrixServerName = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := validDelegateDelayedLeaveRequest()
			tc.mutate(&req)
			assertValidationError(t, req.Validate(), "M_BAD_JSON")
		})
	}
}

func TestDelegateDelayedLeaveRequest_Validate_MissingDelayedEventParams(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*DelegateDelayedLeaveRequest)
	}{
		{"missing DelayId", func(r *DelegateDelayedLeaveRequest) { r.DelayId = "" }},
		{"zero DelayTimeout", func(r *DelegateDelayedLeaveRequest) { r.DelayTimeout = 0 }},
		{"negative DelayTimeout", func(r *DelegateDelayedLeaveRequest) { r.DelayTimeout = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := validDelegateDelayedLeaveRequest()
			tc.mutate(&req)
			assertValidationError(t, req.Validate(), "M_BAD_JSON")
		})
	}
}

func TestDelegateDelayedLeaveRequest_Validate_MissingCsApiUrlIgnored(t *testing.T) {
	req := validDelegateDelayedLeaveRequest()
	req.DelayCsApiUrl = ""
	if err := req.Validate(); err != nil {
		t.Errorf("expected no error for valid request without CS API URL, got: %v", err)
	}
}

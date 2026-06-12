// Copyright 2025 Element Creations Ltd.
// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// helper_test.go: tests for the cross-cutting helpers in helper.go
// (ID generation, LiveKit SDK wrappers, Matrix CS-API calls).

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/livekit/protocol/livekit"
)

// ── NewUniqueID ───────────────────────────────────────────────────────────────

// TestNewUniqueIDUniqueness verifies that each call to NewUniqueID() returns a unique ID.
func TestNewUniqueIDUniqueness(t *testing.T) {
	const count = 1000
	ids := make(map[UniqueID]bool, count)
	for i := 0; i < count; i++ {
		id := NewUniqueID()
		if ids[id] {
			t.Errorf("duplicate ID generated: %s (iteration %d)", id, i)
		}
		ids[id] = true
	}
	if len(ids) != count {
		t.Errorf("expected %d unique IDs, got %d", count, len(ids))
	}
}

// TestNewUniqueIDChronologicalOrder verifies that subsequent IDs maintain
// chronological (lexicographic) order because the timestamp occupies the most
// significant bytes and Base32Hex preserves byte order.
func TestNewUniqueIDChronologicalOrder(t *testing.T) {
	const count = 100
	ids := make([]UniqueID, count)
	for i := 0; i < count; i++ {
		ids[i] = NewUniqueID()
		time.Sleep(1 * time.Millisecond)
	}
	for i := 1; i < count; i++ {
		if strings.Compare(string(ids[i-1]), string(ids[i])) >= 0 {
			t.Errorf("chronological order violated at index %d: %s >= %s",
				i, ids[i-1], ids[i])
		}
	}
}

// TestNewUniqueIDFormat verifies that generated IDs have the correct length
// and only contain valid Base32Hex characters (0-9, A-V, no padding).
func TestNewUniqueIDFormat(t *testing.T) {
	id := NewUniqueID()
	// 16 bytes → Base32Hex without padding: ceil(16*8/5) = 26 characters.
	const expectedLen = 26
	if len(id) != expectedLen {
		t.Errorf("expected ID length %d, got %d: %s", expectedLen, len(id), id)
	}
	const validChars = "0123456789ABCDEFGHIJKLMNOPQRSTUV"
	for _, ch := range id {
		if !strings.ContainsRune(validChars, ch) {
			t.Errorf("invalid character %q in ID %s", ch, id)
		}
	}
}

// TestNewUniqueIDNeverEmpty verifies that generated IDs are never empty.
func TestNewUniqueIDNeverEmpty(t *testing.T) {
	for i := 0; i < 100; i++ {
		if id := NewUniqueID(); len(id) == 0 {
			t.Error("generated empty UniqueID")
		}
	}
}

// TestNewUniqueIDStringConversion verifies that UniqueID round-trips through
// string conversion without loss.
func TestNewUniqueIDStringConversion(t *testing.T) {
	id := NewUniqueID()
	if idAgain := UniqueID(string(id)); id != idAgain {
		t.Errorf("round-trip conversion failed: %s != %s", id, idAgain)
	}
}

// ── LiveKitRoomAliasFor ────────────────────────────────────────────────────

// TestLiveKitRoomAliasFor_TestVector verifies against the test vector from the
// spec proposal to ensure compliance with the expected hashing and encoding scheme.
// https://github.com/hughns/matrix-spec-proposals/blob/hughns/matrixrtc-livekit/proposals/4195-matrixrtc-livekit.md#appendix-hash-derivation-test-vectors
func TestLiveKitRoomAliasFor_TestVector(t *testing.T) {
	id := string(LiveKitRoomAliasFor("!roomid:example.com", "slot1234"))
	wantId := "O8437W3+jmzMVjoIP3tNwbm+XxHQk2iKpOA7aqw3qSc"
	if id != wantId {
		t.Errorf("LiveKitRoomAliasFor test vector mismatch: got %s, want %s", id, wantId)
	}
}

// TestLiveKitRoomAliasFor_Deterministic verifies that the same inputs always
// produce the same alias.
func TestLiveKitRoomAliasFor_Deterministic(t *testing.T) {
	a1 := LiveKitRoomAliasFor("!room:example.com", "m.call#ROOM")
	a2 := LiveKitRoomAliasFor("!room:example.com", "m.call#ROOM")
	if a1 != a2 {
		t.Errorf("same inputs produced different aliases: %s vs %s", a1, a2)
	}
}

// TestLiveKitRoomAliasFor_SampleInputsDistinct verifies that different
// inputs produce different aliases.
func TestLiveKitRoomAliasFor_SampleInputsDistinct(t *testing.T) {
	cases := [][2]string{
		{"!room1:example.com", "m.call#ROOM"},
		{"!room2:example.com", "m.call#ROOM"},
		{"!room1:example.com", "m.call#OTHER"},
		{"", ""},
	}
	seen := make(map[LiveKitRoomAlias][2]string)
	for _, c := range cases {
		alias := LiveKitRoomAliasFor(c[0], c[1])
		for prev, prevKey := range seen {
			if alias == prev {
				t.Errorf("collision: (%v) and (%v) produced the same alias %s",
					c, prevKey, alias)
			}
		}
		seen[alias] = c
	}
}

// TestLiveKitRoomAliasFor_Format verifies that the alias is a non-empty
// unpadded Base64 string (no trailing '=').
func TestLiveKitRoomAliasFor_Format(t *testing.T) {
	alias := LiveKitRoomAliasFor("!room:example.com", "m.call#ROOM")
	if len(alias) == 0 {
		t.Error("alias is empty")
	}
	if strings.Contains(string(alias), "=") {
		t.Errorf("alias contains padding '=': %s", alias)
	}
}

// ── LiveKitIdentityFor ─────────────────────────────────────────────────────

// TestLiveKitIdentityFor_TestVector verifies against the test vector from the
// spec proposal to ensure compliance with the expected hashing and encoding scheme.
// https://github.com/hughns/matrix-spec-proposals/blob/hughns/matrixrtc-livekit/proposals/4195-matrixrtc-livekit.md#appendix-hash-derivation-test-vectors
func TestLiveKitIdentityFor_TestVector(t *testing.T) {
	id := string(LiveKitIdentityFor("@alice:example.com", "DEVICE123", "memberABC"))
	wantId := "J+T45tGruxc+HrUOqJJlyQSV33m728Cme4+vt8/SWrU"
	if id != wantId {
		t.Errorf("LiveKitIdentityFor test vector mismatch: got %s, want %s", id, wantId)
	}
}

// TestLiveKitIdentityFor_Deterministic verifies that the same inputs always
// produce the same identity.
func TestLiveKitIdentityFor_Deterministic(t *testing.T) {
	id1 := LiveKitIdentityFor("@user:example.com", "DEVICEID", "memberID")
	id2 := LiveKitIdentityFor("@user:example.com", "DEVICEID", "memberID")
	if id1 != id2 {
		t.Errorf("same inputs produced different identities: %s vs %s", id1, id2)
	}
}

// TestLiveKitIdentityFor_SampleInputsDistinct verifies that different inputs
// produce different identities.
func TestLiveKitIdentityFor_SampleInputsDistinct(t *testing.T) {
	cases := [][3]string{
		{"@alice:example.com", "DEV1", "mem1"},
		{"@bob:example.com", "DEV1", "mem1"},
		{"@alice:example.com", "DEV2", "mem1"},
		{"@alice:example.com", "DEV1", "mem2"},
	}
	seen := make(map[LiveKitIdentity][3]string)
	for _, c := range cases {
		id := LiveKitIdentityFor(c[0], c[1], c[2])
		for prev, prevInputs := range seen {
			if id == prev {
				t.Errorf("collision: %v and %v produced the same identity %s",
					c, prevInputs, id)
			}
		}
		seen[id] = c
	}
}

// TestLiveKitIdentityFor_Format verifies that the identity is a non-empty
// unpadded Base64 string.
func TestLiveKitIdentityFor_Format(t *testing.T) {
	id := LiveKitIdentityFor("@user:example.com", "DEVICEID", "memberID")
	if len(id) == 0 {
		t.Error("identity is empty")
	}
	if strings.Contains(string(id), "=") {
		t.Errorf("identity contains padding '=': %s", id)
	}
}

// ── ExecuteDelayedEventAction ─────────────────────────────────────────────────

// TestExecuteDelayedEventAction_Success verifies that a 200 OK response
// returns the status code without error.
func TestExecuteDelayedEventAction_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	status, err := ExecuteDelayedEventAction(ts.URL, "delay-id-1", ActionRestart)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("expected 200, got %d", status)
	}
}

// TestExecuteDelayedEventAction_URLConstruction verifies that the request URL
// is built correctly from base URL, delay ID, and action.
func TestExecuteDelayedEventAction_URLConstruction(t *testing.T) {
	var capturedPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	_, _ = ExecuteDelayedEventAction(ts.URL, "myDelayID", ActionSend)
	expected := DelayedEventsEndpoint + "/myDelayID/" + string(ActionSend)
	if capturedPath != expected {
		t.Errorf("expected path %q, got %q", expected, capturedPath)
	}
}

// TestExecuteDelayedEventAction_PathEscaping verifies that a delayID
// containing path-traversal sequences or special characters is escaped and
// does not alter the request path beyond the intended segment.
func TestExecuteDelayedEventAction_PathEscaping(t *testing.T) {
	var capturedPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	maliciousID := "../../../admin"
	_, _ = ExecuteDelayedEventAction(ts.URL, maliciousID, ActionSend)

	// The path must not contain an unescaped ".." segment that would traverse
	// outside the expected endpoint prefix.
	segments := strings.Split(capturedPath, "/")
	for _, seg := range segments {
		if seg == ".." {
			t.Errorf("path contains unescaped traversal segment: %q (full path: %s)",
				seg, capturedPath)
		}
	}
}

// TestExecuteDelayedEventAction_404OnSend verifies that a 404 for ActionSend
// is treated as success (delayed event already sent/cancelled) and returns
// the status code without error.
func TestExecuteDelayedEventAction_404OnSend(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	status, err := ExecuteDelayedEventAction(ts.URL, "gone-id", ActionSend)
	if err != nil {
		t.Fatalf("expected no error for ActionSend 404, got: %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("expected 404, got %d", status)
	}
}

// TestExecuteDelayedEventAction_404OnRestart verifies that a 404 for
// ActionRestart returns the errDelayedEventNotFound sentinel so callers
// can map it to DelayedEventNotFound (vs. the generic transient/permanent
// failure path). This is the contract differentiator from 404-on-Send,
// which is treated as success per MSC-4140.
func TestExecuteDelayedEventAction_404OnRestart(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	status, err := ExecuteDelayedEventAction(ts.URL, "gone-id", ActionRestart)
	if err == nil {
		t.Fatal("expected errDelayedEventNotFound for 404 on ActionRestart, got nil")
	}
	if !errors.Is(err, errDelayedEventNotFound) {
		t.Errorf("expected errDelayedEventNotFound, got %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", status)
	}
}

// TestExecuteDelayedEventAction_RetryAfter_Formats verifies that a 429
// response with a Retry-After header (both seconds and HTTP-date formats)
// is converted to a *backoff.RetryAfterError with the correct duration.
func TestExecuteDelayedEventAction_RetryAfter_Formats(t *testing.T) {
	futureDate := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	tests := []struct {
		name               string
		retryAfterValue    string
		expectedMinSeconds int
	}{
		{
			name:               "Retry-After as seconds",
			retryAfterValue:    "30",
			expectedMinSeconds: 30,
		},
		{
			name:               "Retry-After as HTTP date",
			retryAfterValue:    futureDate,
			expectedMinSeconds: 10,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", tt.retryAfterValue)
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer ts.Close()

			status, err := ExecuteDelayedEventAction(ts.URL, "id", ActionSend)
			if err == nil {
				t.Fatal("expected error for 429, got nil")
			}
			if status != http.StatusTooManyRequests {
				t.Errorf("expected status 429, got %d", status)
			}
			var retryAfterErr *backoff.RetryAfterError
			if !errors.As(err, &retryAfterErr) {
				t.Fatalf("expected *backoff.RetryAfterError, got %T: %v", err, err)
			}
			// Allow ±1s tolerance for clock jitter between header generation and assertion.
			diff := tt.expectedMinSeconds - int(retryAfterErr.Duration.Seconds())
			if diff > 1 {
				t.Errorf("retry duration too short: expected ~%ds, got %s",
					tt.expectedMinSeconds, retryAfterErr.Duration)
			}
		})
	}
}

// TestExecuteDelayedEventAction_429WithoutRetryAfter verifies that a 429
// without a usable Retry-After returns a plain transient error (so callers
// wrapping in backoff.Retry will retry with the default interval) and that
// the 429 status code is still surfaced to the caller for logging.
func TestExecuteDelayedEventAction_429WithoutRetryAfter(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	status, err := ExecuteDelayedEventAction(ts.URL, "id", ActionSend)
	if err == nil {
		t.Fatal("expected transient error for 429 without Retry-After, got nil")
	}
	var retryAfterErr *backoff.RetryAfterError
	if errors.As(err, &retryAfterErr) {
		t.Errorf("did not expect *backoff.RetryAfterError, got %v", err)
	}
	if status != http.StatusTooManyRequests {
		t.Errorf("expected status 429, got %d", status)
	}
}

// TestExecuteDelayedEventAction_429MatrixRetryAfterMs verifies the Matrix-
// specific body fallback: when a 429 omits the Retry-After header (or its
// value is unparseable) but the response body carries the M_LIMIT_EXCEEDED
// `retry_after_ms` field, the helper honours it.
// Matrix spec v1.10 spec deprecated this in favour of the standard
// Retry-After header but Synapse/Dendrite/Conduit still emit it for backwards
// compatibility.
func TestExecuteDelayedEventAction_429MatrixRetryAfterMs(t *testing.T) {
	for _, tc := range []struct {
		name           string
		header         string // value of Retry-After (empty = unset)
		body           string // raw response body
		wantRetryAfter bool   // expect *backoff.RetryAfterError
		wantMinSeconds int    // lower bound on the retry duration in seconds
	}{
		{
			name:           "body retry_after_ms — 2000 ms → 2 s",
			body:           `{"errcode":"M_LIMIT_EXCEEDED","retry_after_ms":2000}`,
			wantRetryAfter: true,
			wantMinSeconds: 2,
		},
		{
			name:           "body retry_after_ms — sub-second 500 ms ceils to 1 s",
			body:           `{"errcode":"M_LIMIT_EXCEEDED","retry_after_ms":500}`,
			wantRetryAfter: true,
			wantMinSeconds: 1,
		},
		{
			name:           "header wins when both present",
			header:         "10",
			body:           `{"errcode":"M_LIMIT_EXCEEDED","retry_after_ms":30000}`,
			wantRetryAfter: true,
			wantMinSeconds: 10, // header value, not the body's 30
		},
		{
			name:           "retry_after_ms = 0 → fall through to transient err",
			body:           `{"errcode":"M_LIMIT_EXCEEDED","retry_after_ms":0}`,
			wantRetryAfter: false,
		},
		{
			name:           "negative retry_after_ms → fall through to transient err",
			body:           `{"errcode":"M_LIMIT_EXCEEDED","retry_after_ms":-100}`,
			wantRetryAfter: false,
		},
		{
			name:           "invalid JSON body → fall through to transient err",
			body:           `not json {`,
			wantRetryAfter: false,
		},
		{
			name:           "body without retry_after_ms field → fall through",
			body:           `{"errcode":"M_LIMIT_EXCEEDED","error":"slow down"}`,
			wantRetryAfter: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()

			status, err := ExecuteDelayedEventAction(ts.URL, "id", ActionSend)
			if err == nil {
				t.Fatal("expected error for 429, got nil")
			}
			if status != http.StatusTooManyRequests {
				t.Errorf("expected status 429, got %d", status)
			}
			var retryAfterErr *backoff.RetryAfterError
			gotRetryAfter := errors.As(err, &retryAfterErr)
			if gotRetryAfter != tc.wantRetryAfter {
				t.Fatalf("RetryAfterError = %v, want %v (err: %v)",
					gotRetryAfter, tc.wantRetryAfter, err)
			}
			if tc.wantRetryAfter {
				if got := int(retryAfterErr.Duration.Seconds()); got < tc.wantMinSeconds {
					t.Errorf("retry duration too short: got %ds, want at least %ds",
						got, tc.wantMinSeconds)
				}
			}
		})
	}
}

// TestExecuteDelayedEventAction_502BadGateway verifies that a 502 returns a
// transient error and surfaces the status code for logging.
func TestExecuteDelayedEventAction_502BadGateway(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()

	status, err := ExecuteDelayedEventAction(ts.URL, "id", ActionSend)
	if err == nil {
		t.Fatal("expected error for 502, got nil")
	}
	if status != http.StatusBadGateway {
		t.Errorf("expected status 502, got %d", status)
	}
}

// TestExecuteDelayedEventAction_500InternalServerError verifies that a 500
// returns a transient error and surfaces the status code for logging.
func TestExecuteDelayedEventAction_500InternalServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	status, err := ExecuteDelayedEventAction(ts.URL, "id", ActionSend)
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}
	if status != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", status)
	}
}

// TestExecuteDelayedEventAction_5xxAreAllTransient verifies that the
// generalized "any 5xx" branch returns a transient error for the common
// retryable codes — 500, 502, 503, 504 — plus a less common one (507)
// to lock the contract in place. None of these is a *backoff.RetryAfterError;
// they all let backoff.Retry use its default schedule.
func TestExecuteDelayedEventAction_5xxAreAllTransient(t *testing.T) {
	for _, code := range []int{
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout,      // 504
		http.StatusInsufficientStorage, // 507
	} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer ts.Close()

			status, err := ExecuteDelayedEventAction(ts.URL, "id", ActionSend)
			if err == nil {
				t.Fatalf("expected transient error for %d, got nil", code)
			}
			var retryAfterErr *backoff.RetryAfterError
			if errors.As(err, &retryAfterErr) {
				t.Errorf("did not expect *backoff.RetryAfterError for %d, got %v", code, err)
			}
			if status != code {
				t.Errorf("expected status %d, got %d", code, status)
			}
		})
	}
}

// TestExecuteDelayedEventAction_UnknownStatusAreTransient verifies the
// catch-all branch: any status code the switch doesn't classify (genuine
// transients like 408 / 421 / 423 / 425, oddball 4xx like 400 / 418, …)
// returns a non-Permanent error so backoff.Retry keeps trying until
// WithMaxElapsedTime expires. Locks in two things at once:
//
//   - the explicit-success contract (nil err means success — these don't
//     silently slip through);
//   - the "transient by default" policy (only errDelayedEventNotFound is
//     wrapped in backoff.Permanent; everything else gets the benefit of
//     retry budget).
func TestExecuteDelayedEventAction_UnknownStatusAreTransient(t *testing.T) {
	for _, code := range []int{
		http.StatusBadRequest,         // 400 — permanent in spirit, but retry budget is cheap
		http.StatusUnauthorized,       // 401
		http.StatusForbidden,          // 403
		http.StatusRequestTimeout,     // 408 — actually transient
		http.StatusMisdirectedRequest, // 421 — actually transient
		http.StatusLocked,             // 423 — actually transient
		http.StatusTooEarly,           // 425 — actually transient
		http.StatusTeapot,             // 418
	} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer ts.Close()

			status, err := ExecuteDelayedEventAction(ts.URL, "id", ActionSend)
			if err == nil {
				t.Fatalf("expected unexpected-status error for %d, got nil", code)
			}
			if status != code {
				t.Errorf("expected status %d, got %d", code, status)
			}
			// Not a RetryAfter (only 429 with parseable Retry-After is).
			var retryAfterErr *backoff.RetryAfterError
			if errors.As(err, &retryAfterErr) {
				t.Errorf("did not expect *backoff.RetryAfterError for %d, got %v", code, err)
			}
			// Not Permanent — backoff should keep trying until the deadline.
			var permErr *backoff.PermanentError
			if errors.As(err, &permErr) {
				t.Errorf("did not expect *backoff.PermanentError for %d (let backoff retry), got %v", code, err)
			}
		})
	}
}

// TestExecuteDelayedEventAction_NetworkError verifies that a connection error
// is returned as a non-nil error.
func TestExecuteDelayedEventAction_NetworkError(t *testing.T) {
	// Use a closed server to provoke a connection error.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ts.Close() // close immediately

	_, err := ExecuteDelayedEventAction(ts.URL, "id", ActionSend)
	if err == nil {
		t.Error("expected a network error, got nil")
	}
}

// TestExecuteDelayedEventAction_ContentType verifies that requests carry the
// correct Content-Type header.
func TestExecuteDelayedEventAction_ContentType(t *testing.T) {
	var capturedContentType string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	_, _ = ExecuteDelayedEventAction(ts.URL, "id", ActionRestart)
	if capturedContentType != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", capturedContentType)
	}
}

// ── exchangeOpenIdUserInfo ────────────────────────────────────────────────────

// TestExchangeOpenIdUserInfo_Success verifies the end-to-end OpenID userinfo
// lookup: exchangeOpenIdUserInfo builds the correct federation request, parses
// the response body, and returns the sub. Endpoint happy-path tests mock
// this function to focus on endpoint behaviour — this is its dedicated home.
func TestExchangeOpenIdUserInfo_Success(t *testing.T) {
	const accessToken = "testAccessToken"
	var matrixServerName string

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/federation/v1/openid/userinfo" {
			t.Errorf("unexpected path: got %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("access_token"); got != accessToken {
			t.Errorf("access_token = %q, want %q", got, accessToken)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := fmt.Fprintf(w, `{"sub": "@alice:%s"}`, matrixServerName); err != nil {
			t.Fatalf("failed to write response: %v", err)
		}
	}))
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	matrixServerName = u.Host

	info, err := exchangeOpenIdUserInfo(
		context.Background(),
		OpenIDTokenType{
			AccessToken:      accessToken,
			MatrixServerName: matrixServerName,
		},
		true, // skipVerifyTLS — required for the httptest TLS server
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "@alice:" + matrixServerName; info.Sub != want {
		t.Errorf("Sub = %q, want %q", info.Sub, want)
	}
}

// ── CreateLiveKitRoom ─────────────────────────────────────────────────────────

// mockRoomServiceClient implements the RoomClient interface for testing
type mockRoomServiceClient struct {
	createRoomFunc     func(ctx context.Context, req *livekit.CreateRoomRequest) (*livekit.Room, error)
	getParticipantFunc func(ctx context.Context, req *livekit.RoomParticipantIdentity) (*livekit.ParticipantInfo, error)
}

func (m *mockRoomServiceClient) CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest) (*livekit.Room, error) {
	if m.createRoomFunc != nil {
		return m.createRoomFunc(ctx, req)
	}
	return &livekit.Room{}, nil
}

func (m *mockRoomServiceClient) GetParticipant(ctx context.Context, req *livekit.RoomParticipantIdentity) (*livekit.ParticipantInfo, error) {
	if m.getParticipantFunc != nil {
		return m.getParticipantFunc(ctx, req)
	}
	return &livekit.ParticipantInfo{}, nil
}

// TestCreateLiveKitRoom_SuccessCreatingNewRoom verifies that CreateLiveKitRoom
// successfully creates a new room and properly detects it as newly created.
func TestCreateLiveKitRoom_SuccessCreatingNewRoom(t *testing.T) {
	ctx := context.Background()
	liveKitAuth := &LiveKitAuth{
		key:    "test-key",
		secret: "test-secret",
		lkUrl:  "http://localhost:55002",
	}
	roomAlias := LiveKitRoomAlias("test-room-alias")
	identity := LiveKitIdentity("test-identity")
	matrixUser := "@user:example.com"

	creationStart := time.Now().Unix()
	mockRoom := &livekit.Room{
		Sid:          "room-sid-123",
		CreationTime: creationStart + 1, // Room created after our start marker
	}

	originalNewClient := newRoomServiceClient
	newRoomServiceClient = func(url, key, secret string) RoomClient {
		return &mockRoomServiceClient{
			createRoomFunc: func(ctx context.Context, req *livekit.CreateRoomRequest) (*livekit.Room, error) {
				if string(req.Name) != string(roomAlias) {
					t.Errorf("expected room name %q, got %q", roomAlias, req.Name)
				}
				if req.EmptyTimeout != 5*60 {
					t.Errorf("expected empty timeout 300s, got %ds", req.EmptyTimeout)
				}
				if req.DepartureTimeout != 20 {
					t.Errorf("expected departure timeout 20s, got %ds", req.DepartureTimeout)
				}
				if req.MaxParticipants != 0 {
					t.Errorf("expected max participants 0 (unlimited), got %d", req.MaxParticipants)
				}
				return mockRoom, nil
			},
		}
	}
	defer func() { newRoomServiceClient = originalNewClient }()

	err := CreateLiveKitRoom(ctx, liveKitAuth, roomAlias, matrixUser, identity)

	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

// TestCreateLiveKitRoom_SuccessUsingExistingRoom verifies that CreateLiveKitRoom
// succeeds when the room already exists (creation time is before our start marker).
func TestCreateLiveKitRoom_SuccessUsingExistingRoom(t *testing.T) {
	ctx := context.Background()
	liveKitAuth := &LiveKitAuth{
		key:    "test-key",
		secret: "test-secret",
		lkUrl:  "http://localhost:55002",
	}
	roomAlias := LiveKitRoomAlias("existing-room")
	identity := LiveKitIdentity("test-identity")
	matrixUser := "@user:example.com"

	creationStart := time.Now().Unix()
	mockRoom := &livekit.Room{
		Sid:          "room-sid-456",
		CreationTime: creationStart - 100, // Room created long ago
	}

	originalNewClient := newRoomServiceClient
	newRoomServiceClient = func(url, key, secret string) RoomClient {
		return &mockRoomServiceClient{
			createRoomFunc: func(ctx context.Context, req *livekit.CreateRoomRequest) (*livekit.Room, error) {
				return mockRoom, nil
			},
		}
	}
	defer func() { newRoomServiceClient = originalNewClient }()

	err := CreateLiveKitRoom(ctx, liveKitAuth, roomAlias, matrixUser, identity)

	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

// TestCreateLiveKitRoom_ErrorHandling verifies that errors from the LiveKit SDK
// are properly wrapped and returned with the room alias in the message.
func TestCreateLiveKitRoom_ErrorHandling(t *testing.T) {
	ctx := context.Background()
	liveKitAuth := &LiveKitAuth{
		key:    "test-key",
		secret: "test-secret",
		lkUrl:  "http://localhost:55002",
	}
	roomAlias := LiveKitRoomAlias("failed-room")
	identity := LiveKitIdentity("test-identity")
	matrixUser := "@user:example.com"

	sdkErr := errors.New("SDK connection failed")
	originalNewClient := newRoomServiceClient
	newRoomServiceClient = func(url, key, secret string) RoomClient {
		return &mockRoomServiceClient{
			createRoomFunc: func(ctx context.Context, req *livekit.CreateRoomRequest) (*livekit.Room, error) {
				return nil, sdkErr
			},
		}
	}
	defer func() { newRoomServiceClient = originalNewClient }()

	err := CreateLiveKitRoom(ctx, liveKitAuth, roomAlias, matrixUser, identity)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), string(roomAlias)) {
		t.Errorf("error should mention room alias, got: %v", err)
	}
}

// TestCreateLiveKitRoom_RoomConfigurationParameters verifies that the room
// is created with correct timeout configuration.
func TestCreateLiveKitRoom_RoomConfigurationParameters(t *testing.T) {
	ctx := context.Background()
	liveKitAuth := &LiveKitAuth{
		key:    "test-key",
		secret: "test-secret",
		lkUrl:  "http://localhost:55002",
	}

	var capturedRequest *livekit.CreateRoomRequest
	originalNewClient := newRoomServiceClient
	newRoomServiceClient = func(url, key, secret string) RoomClient {
		return &mockRoomServiceClient{
			createRoomFunc: func(ctx context.Context, req *livekit.CreateRoomRequest) (*livekit.Room, error) {
				capturedRequest = req
				return &livekit.Room{Sid: "test", CreationTime: time.Now().Unix()}, nil
			},
		}
	}
	defer func() { newRoomServiceClient = originalNewClient }()

	_ = CreateLiveKitRoom(ctx, liveKitAuth, LiveKitRoomAlias("room"), "@user:example.com", LiveKitIdentity("id"))

	if capturedRequest == nil {
		t.Fatal("CreateRoom was not called")
	}
	if capturedRequest.EmptyTimeout != 5*60 {
		t.Errorf("expected empty timeout 300s, got %ds", capturedRequest.EmptyTimeout)
	}
	if capturedRequest.DepartureTimeout != 20 {
		t.Errorf("expected departure timeout 20s, got %ds", capturedRequest.DepartureTimeout)
	}
	if capturedRequest.MaxParticipants != 0 {
		t.Errorf("expected max participants 0 (unlimited), got %d", capturedRequest.MaxParticipants)
	}
}

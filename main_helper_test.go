// Copyright 2025 Element Creations Ltd.
// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

// ── CreateLiveKitRoomAlias ────────────────────────────────────────────────────

// TestCreateLiveKitRoomAlias_TestVector verifies against the test vector from the
// spec proposal to ensure compliance with the expected hashing and encoding scheme.
// https://github.com/hughns/matrix-spec-proposals/blob/hughns/matrixrtc-livekit/proposals/4195-matrixrtc-livekit.md#appendix-hash-derivation-test-vectors
func TestCreateLiveKitRoomAlias_TestVector(t *testing.T) {
	id := string(CreateLiveKitRoomAlias("!roomid:example.com", "slot123"))
	wantId := "AUDmNDQiVHmWYRE+rKBvieWX8AUSzepenuj6u+d/n9c"
	if id != wantId {
		t.Errorf("CreateLiveKitRoomAlias test vector mismatch: got %s, want %s", id, wantId)
	}
}

// TestCreateLiveKitRoomAlias_Deterministic verifies that the same inputs always
// produce the same alias.
func TestCreateLiveKitRoomAlias_Deterministic(t *testing.T) {
	a1 := CreateLiveKitRoomAlias("!room:example.com", "m.call#ROOM")
	a2 := CreateLiveKitRoomAlias("!room:example.com", "m.call#ROOM")
	if a1 != a2 {
		t.Errorf("same inputs produced different aliases: %s vs %s", a1, a2)
	}
}

// TestCreateLiveKitRoomAlias_SampleInputsDistinct verifies that different
// inputs produce different aliases.
func TestCreateLiveKitRoomAlias_SampleInputsDistinct(t *testing.T) {
	cases := [][2]string{
		{"!room1:example.com", "m.call#ROOM"},
		{"!room2:example.com", "m.call#ROOM"},
		{"!room1:example.com", "m.call#OTHER"},
		{"", ""},
	}
	seen := make(map[LiveKitRoomAlias][2]string)
	for _, c := range cases {
		alias := CreateLiveKitRoomAlias(c[0], c[1])
		for prev, prevKey := range seen {
			if alias == prev {
				t.Errorf("collision: (%v) and (%v) produced the same alias %s",
					c, prevKey, alias)
			}
		}
		seen[alias] = c
	}
}

// TestCreateLiveKitRoomAlias_Format verifies that the alias is a non-empty
// unpadded Base64 string (no trailing '=').
func TestCreateLiveKitRoomAlias_Format(t *testing.T) {
	alias := CreateLiveKitRoomAlias("!room:example.com", "m.call#ROOM")
	if len(alias) == 0 {
		t.Error("alias is empty")
	}
	if strings.Contains(string(alias), "=") {
		t.Errorf("alias contains padding '=': %s", alias)
	}
}

// ── CreateLiveKitIdentity ─────────────────────────────────────────────────────

// TestCreateLiveKitIdentity_TestVector verifies against the test vector from the
// spec proposal to ensure compliance with the expected hashing and encoding scheme.
// https://github.com/hughns/matrix-spec-proposals/blob/hughns/matrixrtc-livekit/proposals/4195-matrixrtc-livekit.md#appendix-hash-derivation-test-vectors
func TestCreateLiveKitIdentity_TestVector(t *testing.T) {
	id := string(CreateLiveKitIdentity("@alice:example.com", "DEVICE123", "memberABC"))
	wantId := "J+T45tGruxc+HrUOqJJlyQSV33m728Cme4+vt8/SWrU"
	if id != wantId {
		t.Errorf("CreateLiveKitIdentity test vector mismatch: got %s, want %s", id, wantId)
	}
}

// TestCreateLiveKitIdentity_Deterministic verifies that the same inputs always
// produce the same identity.
func TestCreateLiveKitIdentity_Deterministic(t *testing.T) {
	id1 := CreateLiveKitIdentity("@user:example.com", "DEVICEID", "memberID")
	id2 := CreateLiveKitIdentity("@user:example.com", "DEVICEID", "memberID")
	if id1 != id2 {
		t.Errorf("same inputs produced different identities: %s vs %s", id1, id2)
	}
}

// TestCreateLiveKitIdentity_SampleInputsDistinct verifies that different inputs 
// produce different identities.
func TestCreateLiveKitIdentity_SampleInputsDistinct(t *testing.T) {
	cases := [][3]string{
		{"@alice:example.com", "DEV1", "mem1"},
		{"@bob:example.com", "DEV1", "mem1"},
		{"@alice:example.com", "DEV2", "mem1"},
		{"@alice:example.com", "DEV1", "mem2"},
	}
	seen := make(map[LiveKitIdentity][3]string)
	for _, c := range cases {
		id := CreateLiveKitIdentity(c[0], c[1], c[2])
		for prev, prevInputs := range seen {
			if id == prev {
				t.Errorf("collision: %v and %v produced the same identity %s",
					c, prevInputs, id)
			}
		}
		seen[id] = c
	}
}

// TestCreateLiveKitIdentity_Format verifies that the identity is a non-empty
// unpadded Base64 string.
func TestCreateLiveKitIdentity_Format(t *testing.T) {
	id := CreateLiveKitIdentity("@user:example.com", "DEVICEID", "memberID")
	if len(id) == 0 {
		t.Error("identity is empty")
	}
	if strings.Contains(string(id), "=") {
		t.Errorf("identity contains padding '=': %s", id)
	}
}

// ── ExecuteDelayedEventAction ─────────────────────────────────────────────────

// TestExecuteDelayedEventAction_Success verifies that a 200 OK response is
// returned as-is without error.
func TestExecuteDelayedEventAction_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	resp, err := ExecuteDelayedEventAction(ts.URL, "delay-id-1", ActionRestart)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
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
// the response without error.
func TestExecuteDelayedEventAction_404OnSend(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	resp, err := ExecuteDelayedEventAction(ts.URL, "gone-id", ActionSend)
	if err != nil {
		t.Fatalf("expected no error for ActionSend 404, got: %v", err)
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 response, got %v", resp)
	}
}

// TestExecuteDelayedEventAction_404OnRestart verifies that a 404 for
// ActionRestart is passed through normally (not special-cased).
func TestExecuteDelayedEventAction_404OnRestart(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	resp, err := ExecuteDelayedEventAction(ts.URL, "gone-id", ActionRestart)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 response, got %v", resp)
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

			resp, err := ExecuteDelayedEventAction(ts.URL, "id", ActionSend)
			if err == nil {
				t.Fatal("expected error for 429, got nil")
			}
			if resp != nil {
				t.Error("expected nil response for 429 with Retry-After")
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
// without a Retry-After header returns the response without a RetryAfterError.
func TestExecuteDelayedEventAction_429WithoutRetryAfter(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	resp, err := ExecuteDelayedEventAction(ts.URL, "id", ActionSend)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429 response, got %v", resp)
	}
}

// TestExecuteDelayedEventAction_502BadGateway verifies that a 502 error is handled as error
func TestExecuteDelayedEventAction_502BadGateway(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()

	resp, err := ExecuteDelayedEventAction(ts.URL, "id", ActionSend)
	if err == nil {
		t.Fatalf("Expected error: CS API temporarily unavailable (http status code 502)")
	}
	if resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Errorf("expected 502 response, got %v", resp)
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

// ── CreateLiveKitRoom ─────────────────────────────────────────────────────────

// mockRoomServiceClient implements the RoomClient interface for testing
type mockRoomServiceClient struct {
	createRoomFunc      func(ctx context.Context, req *livekit.CreateRoomRequest) (*livekit.Room, error)
	getParticipantFunc  func(ctx context.Context, req *livekit.RoomParticipantIdentity) (*livekit.ParticipantInfo, error)
	listParticipantsFunc func(ctx context.Context, req *livekit.ListParticipantsRequest) (*livekit.ListParticipantsResponse, error)
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

func (m *mockRoomServiceClient) ListParticipants(ctx context.Context, req *livekit.ListParticipantsRequest) (*livekit.ListParticipantsResponse, error) {
	if m.listParticipantsFunc != nil {
		return m.listParticipantsFunc(ctx, req)
	}
	return &livekit.ListParticipantsResponse{}, nil
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

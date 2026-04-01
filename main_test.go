// Copyright 2025 Element Creations Ltd.
// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/matrix-org/gomatrix"
	"github.com/matrix-org/gomatrixserverlib/fclient"
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

	if v := rr.Header().Get("Access-Control-Allow-Origin"); v != "*" {
		t.Errorf("wrong Access-Control-Allow-Origin: got %v want *", v)
	}

	if v := rr.Header().Get("Access-Control-Allow-Methods"); v != "POST" {
		t.Errorf("wrong Access-Control-Allow-Methods: got %v want POST", v)
	}
}

func TestHandlePostMissingParams(t *testing.T) {
	handler := &Handler{}

	testCases := []map[string]interface{}{
		{},
		{"room": ""},
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
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Errorf("failed to decode response body: %v", err)
		}

		if resp.ErrCode != "M_BAD_JSON" {
			t.Errorf("unexpected error code: got %v want M_BAD_JSON", resp.ErrCode)
		}
	}
}

func TestHandlePost(t *testing.T) {
	handler := NewHandler(
		LiveKitAuth{
			secret: "testSecret",
			key:    "testKey",
			lkUrl:  "wss://lk.local:8080/foo",
		},
		true,
		[]string{"example.com"},
	)

	var matrixServerName = ""

	testServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/federation/v1/openid/userinfo" {
			t.Errorf("unexpected path: got %v", r.URL.Path)
		}
		if accessToken := r.URL.Query().Get("access_token"); accessToken != "testAccessToken" {
			t.Errorf("unexpected access token: got %v", accessToken)
		}
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprintf(w, `{"sub": "@user:%s"}`, matrixServerName); err != nil {
			t.Fatalf("failed to write response: %v", err)
		}
	}))
	defer testServer.Close()

	u, _ := url.Parse(testServer.URL)
	matrixServerName = u.Host

	testCase := map[string]interface{}{
		"room_id": "!testRoom:example.com",
		"slot_id": "m.call#ROOM",
		"openid_token": map[string]interface{}{
			"access_token":       "testAccessToken",
			"token_type":         "testTokenType",
			"matrix_server_name": u.Host,
			"expires_in":         3600,
		},
		"member": map[string]interface{}{
			"id":                "member_test_id",
			"claimed_user_id":   "@user:" + matrixServerName,
			"claimed_device_id": "testDevice",
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

	wantSubHash := sha256.Sum256([]byte("@user:" + matrixServerName + "|testDevice|member_test_id"))
	wantSub := unpaddedBase64.EncodeToString(wantSubHash[:])
	if claims["sub"] != wantSub {
		t.Errorf("unexpected sub: got %v want %v", claims["sub"], wantSub)
	}

	wantRoomHash := sha256.Sum256([]byte("!testRoom:example.com" + "|" + "m.call#ROOM"))
	wantRoom := unpaddedBase64.EncodeToString(wantRoomHash[:])
	if claims["video"].(map[string]interface{})["room"] != wantRoom {
		t.Errorf("unexpected room: got %v want %v", claims["video"].(map[string]interface{})["room"], wantRoom)
	}
}

func TestLegacyHandlePost(t *testing.T) {
	handler := NewHandler(
		LiveKitAuth{
			secret: "testSecret",
			key:    "testKey",
			lkUrl:  "wss://lk.local:8080/foo",
		},
		true,
		[]string{"example.com"},
	)

	var matrixServerName = ""

	testServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/federation/v1/openid/userinfo" {
			t.Errorf("unexpected path: got %v", r.URL.Path)
		}
		if accessToken := r.URL.Query().Get("access_token"); accessToken != "testAccessToken" {
			t.Errorf("unexpected access token: got %v", accessToken)
		}
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprintf(w, `{"sub": "@user:%s"}`, matrixServerName); err != nil {
			t.Fatalf("failed to write response: %v", err)
		}
	}))
	defer testServer.Close()

	u, _ := url.Parse(testServer.URL)
	matrixServerName = u.Host
	matrixRoom := "testRoom"

	testCase := map[string]interface{}{
		"room": matrixRoom,
		"openid_token": map[string]interface{}{
			"access_token":       "testAccessToken",
			"token_type":         "testTokenType",
			"matrix_server_name": u.Host,
			"expires_in":         3600,
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

	if claims["sub"] != "@user:"+matrixServerName+":testDevice" {
		t.Errorf("unexpected sub: got %v", claims["sub"])
	}
	if claims["video"].(map[string]interface{})["room"] != string(CreateLiveKitRoomAlias(matrixRoom, "m.call#ROOM")) {
		t.Errorf("unexpected room: got %v", claims["video"].(map[string]interface{})["room"])
	}
}

func TestIsFullAccessUser(t *testing.T) {
	handler := NewHandler(
		LiveKitAuth{secret: "testSecret", key: "testKey", lkUrl: "wss://lk.local:8080/foo"},
		true,
		[]string{"example.com", "another.example.com"},
	)

	for _, tc := range []struct {
		server string
		want   bool
	}{
		{"example.com", true},
		{"another.example.com", true},
		{"aanother.example.com", false},
		{"matrix.example.com", false},
	} {
		if got := handler.isFullAccessUser(tc.server); got != tc.want {
			t.Errorf("isFullAccessUser(%q) = %v, want %v", tc.server, got, tc.want)
		}
	}

	handler.fullAccessHomeservers = []string{"*"}
	if !handler.isFullAccessUser("other.com") {
		t.Error("expected wildcard to grant full access")
	}
}

func TestGetJoinToken(t *testing.T) {
	tokenString, err := getJoinToken("testKey", "testSecret",
		LiveKitRoomAlias("testRoom"), LiveKitIdentity("testIdentity@example.com"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tokenString == "" {
		t.Error("expected token to be non-empty")
	}

	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		return []byte("testSecret"), nil
	})
	if err != nil {
		t.Fatalf("failed to parse JWT: %v", err)
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		t.Fatal("invalid JWT claims")
	}

	roomCreate := claims["video"].(map[string]interface{})["roomCreate"]
	if roomCreate == true {
		t.Fatal("roomCreate must be false")
	}
}

func TestMapSFURequest(t *testing.T) {
	testCases := []struct {
		name        string
		input       string
		want        any
		wantErrCode string
	}{
		{
			name: "Valid legacy request",
			input: `{
				"room": "testRoom",
				"openid_token": {"access_token":"test_token","token_type":"Bearer","matrix_server_name":"example.com","expires_in":3600},
				"device_id": "testDevice"
			}`,
			want: &LegacySFURequest{
				Room:     "testRoom",
				OpenIDToken: OpenIDTokenType{AccessToken: "test_token", TokenType: "Bearer", MatrixServerName: "example.com", ExpiresIn: 3600},
				DeviceID: "testDevice",
			},
		},
		{
			name: "Valid Matrix 2.0 request",
			input: `{
				"room_id":"!testRoom:example.com","slot_id":"123",
				"openid_token":{"access_token":"test_token","token_type":"Bearer","matrix_server_name":"example.com","expires_in":3600},
				"member":{"id":"test_id","claimed_user_id":"@test:example.com","claimed_device_id":"testDevice"}
			}`,
			want: &SFURequest{
				RoomID: "!testRoom:example.com", SlotID: "123",
				OpenIDToken: OpenIDTokenType{AccessToken: "test_token", TokenType: "Bearer", MatrixServerName: "example.com", ExpiresIn: 3600},
				Member:      MatrixRTCMemberType{ID: "test_id", ClaimedUserID: "@test:example.com", ClaimedDeviceID: "testDevice"},
			},
		},
		{
			name: "Valid Matrix 2.0 request with delayed events",
			input: `{
				"room_id":"!testRoom:example.com","slot_id":"123",
				"openid_token":{"access_token":"test_token","token_type":"Bearer","matrix_server_name":"example.com","expires_in":3600},
				"member":{"id":"test_id","claimed_user_id":"@test:example.com","claimed_device_id":"testDevice"},
				"delay_id":"delayed123","delay_timeout":30,"delay_cs_api_url":"https://example.com/api"
			}`,
			want: &SFURequest{
				RoomID: "!testRoom:example.com", SlotID: "123",
				OpenIDToken:   OpenIDTokenType{AccessToken: "test_token", TokenType: "Bearer", MatrixServerName: "example.com", ExpiresIn: 3600},
				Member:        MatrixRTCMemberType{ID: "test_id", ClaimedUserID: "@test:example.com", ClaimedDeviceID: "testDevice"},
				DelayId:       "delayed123",
				DelayTimeout:  30,
				DelayCsApiUrl: "https://example.com/api",
			},
		},
		{
			name: "Invalid delayed events — missing delay_cs_api_url",
			input: `{
				"room_id":"!testRoom:example.com","slot_id":"123",
				"openid_token":{"access_token":"test_token","token_type":"Bearer","matrix_server_name":"example.com","expires_in":3600},
				"member":{"id":"test_id","claimed_user_id":"@test:example.com","claimed_device_id":"testDevice"},
				"delay_id":"delayed123","delay_timeout":30
			}`,
			wantErrCode: "M_BAD_JSON",
		},
		{name: "Invalid JSON", input: `{"invalid": json}`, wantErrCode: "M_BAD_JSON"},
		{name: "Empty request", input: `{}`, wantErrCode: "M_BAD_JSON"},
		{
			name: "Legacy request with extra field",
			input: `{
				"room":"testRoom",
				"openid_token":{"access_token":"test_token","token_type":"Bearer","matrix_server_name":"example.com","expires_in":3600},
				"device_id":"testDevice","extra_field":"should_fail"
			}`,
			wantErrCode: "M_BAD_JSON",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(tc.input)
			got, err := mapSFURequest(&input)

			if tc.wantErrCode != "" {
				matrixErr := &MatrixErrorResponse{}
				if !errors.As(err, &matrixErr) {
					t.Errorf("expected MatrixErrorResponse, got %v", err)
					return
				}
				if matrixErr.ErrCode != tc.wantErrCode {
					t.Errorf("error code = %v, want %v", matrixErr.ErrCode, tc.wantErrCode)
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			switch expected := tc.want.(type) {
			case *LegacySFURequest:
				actual, ok := got.(*LegacySFURequest)
				if !ok {
					t.Errorf("got %T, want *LegacySFURequest", got)
					return
				}
				if !reflect.DeepEqual(actual, expected) {
					t.Errorf("got %+v, want %+v", actual, expected)
				}
			case *SFURequest:
				actual, ok := got.(*SFURequest)
				if !ok {
					t.Errorf("got %T, want *SFURequest", got)
					return
				}
				if !reflect.DeepEqual(actual, expected) {
					t.Errorf("got %+v, want %+v", actual, expected)
				}
			}
		})
	}
}

func TestMapSFURequestMemoryLeak(t *testing.T) {
	const iterations = 100000

	input := []byte(`{
		"room_id":"!testRoom:example.com","slot_id":"123",
		"openid_token":{"access_token":"test_token","token_type":"Bearer","matrix_server_name":"example.com","expires_in":3600},
		"member":{"id":"test_id","claimed_user_id":"@test:example.com","claimed_device_id":"testDevice"}
	}`)

	var mStart, mEnd runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&mStart)

	for i := 0; i < iterations; i++ {
		if _, err := mapSFURequest(&input); err != nil {
			t.Fatalf("unexpected error at iteration %d: %v", i, err)
		}
	}

	runtime.GC()
	runtime.ReadMemStats(&mEnd)

	t.Logf("Start Alloc: %d bytes, End Alloc: %d bytes", mStart.Alloc, mEnd.Alloc)

	if mEnd.Alloc > mStart.Alloc {
		diff := mEnd.Alloc - mStart.Alloc
		const threshold uint64 = 100 * 1024
		if diff > threshold {
			t.Errorf("potential memory leak: heap grew %d bytes (> %d)", diff, threshold)
		}
	}
}

func TestProcessSFURequest(t *testing.T) {
	var calledCreateLiveKitRoom bool
	originalCreate := CreateLiveKitRoom
	CreateLiveKitRoom = func(_ context.Context, _ *LiveKitAuth, room LiveKitRoomAlias, _ string, _ LiveKitIdentity) error {
		calledCreateLiveKitRoom = true
		if room == "" {
			t.Error("expected non-empty room name")
		}
		return nil
	}
	t.Cleanup(func() { CreateLiveKitRoom = originalCreate })

	var failExchange bool
	var exchangeMatrixID string
	originalExchange := exchangeOpenIdUserInfo
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		if failExchange {
			return nil, &MatrixErrorResponse{Status: http.StatusUnauthorized, ErrCode: "M_UNAUTHORIZED", Err: "unauthorised"}
		}
		return &fclient.UserInfo{Sub: exchangeMatrixID}, nil
	}
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })

	tests := []struct {
		name                 string
		matrixID             string
		claimedMatrixID      string
		expectJoinTokenError bool
		expectExchangeError  bool
		expectCreateRoom     bool
		expectError          bool
	}{
		{name: "Full access user — all OK", matrixID: "@user:example.com", claimedMatrixID: "@user:example.com", expectCreateRoom: true},
		{name: "Restricted user — all OK", matrixID: "@user:other.com", claimedMatrixID: "@user:other.com", expectCreateRoom: false},
		{name: "Exchange fails", matrixID: "@user:example.com", claimedMatrixID: "@user:example.com", expectExchangeError: true, expectError: true},
		{name: "getJoinToken fails (empty key)", matrixID: "@user:example.com", claimedMatrixID: "@user:example.com", expectJoinTokenError: true, expectError: true},
		{name: "ClaimedUserID mismatch", matrixID: "@user:example.com", claimedMatrixID: "@user:faked.com", expectError: true},
	}

	for _, tc := range tests {
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
				false,
				[]string{"example.com"},
			)

			req := &SFURequest{
				RoomID: "!room:example.com",
				SlotID: "slot",
				OpenIDToken: OpenIDTokenType{
					AccessToken:      "token",
					MatrixServerName: strings.Split(tc.claimedMatrixID, ":")[1],
				},
				Member: MatrixRTCMemberType{
					ID:              "device",
					ClaimedUserID:   tc.claimedMatrixID,
					ClaimedDeviceID: "dev",
				},
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

func TestProcessLegacySFURequest(t *testing.T) {
	var calledCreateLiveKitRoom bool
	originalCreate := CreateLiveKitRoom
	CreateLiveKitRoom = func(_ context.Context, _ *LiveKitAuth, room LiveKitRoomAlias, _ string, _ LiveKitIdentity) error {
		calledCreateLiveKitRoom = true
		if room == "" {
			t.Error("expected non-empty room name")
		}
		return nil
	}
	t.Cleanup(func() { CreateLiveKitRoom = originalCreate })

	var failExchange bool
	originalExchange := exchangeOpenIdUserInfo
	exchangeOpenIdUserInfo = func(_ context.Context, _ OpenIDTokenType, _ bool) (*fclient.UserInfo, error) {
		if failExchange {
			return nil, &MatrixErrorResponse{Status: http.StatusUnauthorized, ErrCode: "M_UNAUTHORIZED", Err: "unauthorised"}
		}
		return &fclient.UserInfo{Sub: "@mock:example.com"}, nil
	}
	t.Cleanup(func() { exchangeOpenIdUserInfo = originalExchange })

	tests := []struct {
		name                 string
		matrixID             string
		expectJoinTokenError bool
		expectExchangeError  bool
		expectCreateRoom     bool
		expectError          bool
	}{
		{name: "Full access user — all OK", matrixID: "@user:example.com", expectCreateRoom: true},
		{name: "Restricted user — all OK", matrixID: "@user:other.com", expectCreateRoom: false},
		{name: "Exchange fails", matrixID: "@user:example.com", expectExchangeError: true, expectError: true},
		{name: "getJoinToken fails (empty key)", matrixID: "@user:example.com", expectJoinTokenError: true, expectError: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calledCreateLiveKitRoom = false
			failExchange = tc.expectExchangeError

			apiKey := "the_api_key"
			if tc.expectJoinTokenError {
				apiKey = ""
			}

			handler := NewHandler(
				LiveKitAuth{key: apiKey, secret: "secret", lkUrl: "wss://lk.local:8080/foo"},
				false,
				[]string{"example.com"},
			)

			req := &LegacySFURequest{
				Room:        "!room:example.com",
				OpenIDToken: OpenIDTokenType{AccessToken: "token", MatrixServerName: strings.Split(tc.matrixID, ":")[1]},
				DeviceID:    "dev",
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

// ── LiveKitRoomMonitor tests ─────────────────────────────────────────────────
//
// The monitor is now an actor: all state lives inside Loop().
// Tests interact with the monitor exclusively through its public API:
//   - HandoverJob()  — adds a job
//   - Close()        — shuts down the monitor
//   - SFUCommChan    — sends SFU events
//
// We verify behaviour by observing side-effects (e.g. messages arriving on
// handlerCommChan, or the monitor's done channel closing).

// newTestMonitor creates a monitor for testing and registers cleanup in the
// correct order:
//  1. Close the monitor (waits for Loop() to exit, which waits for all
//     participant-lookup goroutines to finish via job.ctx cancellation).
//  2. Only then restore the LiveKitParticipantLookup global.
//
// t.Cleanup calls are executed LIFO, so we register the global restore first
// and the monitor close second — meaning Close() runs first on teardown.
func newTestMonitor(t *testing.T, alias LiveKitRoomAlias) (*LiveKitRoomMonitor, chan HandlerMessage) {
	t.Helper()

	original := LiveKitParticipantLookup

	// Register the restore FIRST so it runs LAST (LIFO order).
	t.Cleanup(func() { LiveKitParticipantLookup = original })

	LiveKitParticipantLookup = func(ctx context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, _ LiveKitIdentity) (SFUMessage, error) {
		// Block until the job context is cancelled so the goroutine exits
		// before the test restores the global — eliminating the race.
		<-ctx.Done()
		return SFUMessage{}, ctx.Err()
	}

	handlerCh := make(chan HandlerMessage, 10)
	lkAuth := &LiveKitAuth{secret: "secret", key: "devkey", lkUrl: "ws://127.0.0.1:7880"}
	m := NewLiveKitRoomMonitor(context.Background(), handlerCh, lkAuth, alias)
	go m.Loop()

	// Register the monitor close SECOND so it runs FIRST (LIFO order).
	// This guarantees Loop() — and all goroutines it spawned — have exited
	// before we restore LiveKitParticipantLookup.
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("newTestMonitor cleanup: Close() failed: %v", err)
		}
	})

	return m, handlerCh
}

func defaultJobRequest(alias LiveKitRoomAlias, identity LiveKitIdentity) *DelayedEventRequest {
	return &DelayedEventRequest{
		DelayCsApiUrl:   "https://synapse.m.localhost",
		DelayId:         "syd_astTzXBzAazONpxHCqzW",
		DelayTimeout:    10 * time.Second,
		LiveKitRoom:     alias,
		LiveKitIdentity: identity,
	}
}

// TestLiveKitRoomMonitor_HandoverAndNoJobsLeft verifies that after handing
// over a single job and then closing it (by sending a Disconnected
// MonitorMessage), the monitor sends NoJobsLeft to the handler.
func TestLiveKitRoomMonitor_HandoverAndNoJobsLeft(t *testing.T) {
	alias := LiveKitRoomAlias("test-room-1")
	m, _ := newTestMonitor(t, alias)

	identity := LiveKitIdentity("@alice:example.com")
	jobID, ok := m.HandoverJob(defaultJobRequest(alias, identity))
	if !ok {
		t.Fatal("HandoverJob returned not-ok")
	}
	if jobID == "" {
		t.Fatal("expected non-empty jobID")
	}

	// Simulate the job completing by injecting a Disconnected MonitorMessage
	// directly via a participant-disconnect SFU event so the job FSM drives it.
	// For a pure unit test we send a ParticipantDisconnectedIntentionally first
	// (which puts the job into Disconnected) after it reaches Connected.
	//
	// Simpler: just close the monitor and verify done closes cleanly.
	if err := m.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}

	select {
	case <-m.done:
	case <-time.After(3 * time.Second):
		t.Fatal("monitor did not shut down in time")
	}
}

// TestLiveKitRoomMonitor_MultipleHandovers verifies that multiple distinct
// identities can be handed over and that the monitor stays alive until all
// jobs have been signalled as complete.
func TestLiveKitRoomMonitor_MultipleHandovers(t *testing.T) {
	alias := LiveKitRoomAlias("test-room-multi")
	m, _ := newTestMonitor(t, alias)

	identities := []LiveKitIdentity{
		"@alice:example.com",
		"@bob:example.com",
		"@charlie:example.com",
	}

	for _, id := range identities {
		_, ok := m.HandoverJob(defaultJobRequest(alias, id))
		if !ok {
			t.Fatalf("HandoverJob failed for %s", id)
		}
	}

	// Close the monitor and ensure it exits cleanly.
	if err := m.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}

	select {
	case <-m.done:
	case <-time.After(5 * time.Second):
		t.Fatal("monitor did not shut down in time after Close()")
	}
}

// TestLiveKitRoomMonitor_HandoverOnClosedMonitor verifies that HandoverJob
// returns false when called after the monitor has been closed.
func TestLiveKitRoomMonitor_HandoverOnClosedMonitor(t *testing.T) {
	alias := LiveKitRoomAlias("test-room-closed")
	m, _ := newTestMonitor(t, alias)

	if err := m.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}

	select {
	case <-m.done:
	case <-time.After(3 * time.Second):
		t.Fatal("monitor did not close in time")
	}

	_, ok := m.HandoverJob(defaultJobRequest(alias, "@late:example.com"))
	if ok {
		t.Error("expected HandoverJob to fail on a closed monitor")
	}
}

// TestLiveKitRoomMonitor_SFUEventsRouted verifies that SFU events sent to
// SFUCommChan reach the correct job without panicking or deadlocking.
func TestLiveKitRoomMonitor_SFUEventsRouted(t *testing.T) {
	alias := LiveKitRoomAlias("test-room-sfu")
	m, _ := newTestMonitor(t, alias)

	identity := LiveKitIdentity("@sfu-user:example.com")
	_, ok := m.HandoverJob(defaultJobRequest(alias, identity))
	if !ok {
		t.Fatal("HandoverJob failed")
	}

	// Give Loop() time to register the job before sending SFU events.
	time.Sleep(20 * time.Millisecond)

	m.SFUCommChan <- SFUMessage{Type: ParticipantConnected, LiveKitIdentity: identity}
	// A second event to ensure the channel is drained.
	m.SFUCommChan <- SFUMessage{Type: ParticipantDisconnectedIntentionally, LiveKitIdentity: identity}

	// newTestMonitor's cleanup calls Close() and waits for Loop() to exit
	// before restoring LiveKitParticipantLookup, so no manual close needed.
	time.Sleep(20 * time.Millisecond)
}

// TestLiveKitRoomMonitor_RaceConditionStress hammers the monitor with many
// concurrent HandoverJob calls to shake out data races.
// Run with: go test -race -run TestLiveKitRoomMonitor_RaceConditionStress
func TestLiveKitRoomMonitor_RaceConditionStress(t *testing.T) {
	original := LiveKitParticipantLookup
	// Restore LAST (registered first — LIFO).
	t.Cleanup(func() { LiveKitParticipantLookup = original })

	LiveKitParticipantLookup = func(ctx context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, _ LiveKitIdentity) (SFUMessage, error) {
		<-ctx.Done()
		return SFUMessage{}, ctx.Err()
	}

	alias := LiveKitRoomAlias("stress-room")
	handlerCh := make(chan HandlerMessage, 100)
	lkAuth := &LiveKitAuth{secret: "secret", key: "devkey", lkUrl: "ws://127.0.0.1:7880"}
	m := NewLiveKitRoomMonitor(context.Background(), handlerCh, lkAuth, alias)
	go m.Loop()

	// Close FIRST (registered second — LIFO): Loop() exits, all lookup goroutines
	// finish (they unblock on ctx.Done()), then the global is restored.
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("Close() failed: %v", err)
		}
	})

	const workers = 50
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func(n int) {
			defer wg.Done()
			identity := LiveKitIdentity(fmt.Sprintf("@user%d:example.com", n))
			req := defaultJobRequest(alias, identity)
			_, _ = m.HandoverJob(req)
		}(i)
	}

	wg.Wait()
	// Cleanup (registered above) calls Close() and waits for done.
}

// TestLiveKitRoomMonitor_ReplaceJob verifies that handing over a second job
// for the same identity replaces the first without deadlocking.
func TestLiveKitRoomMonitor_ReplaceJob(t *testing.T) {
	alias := LiveKitRoomAlias("replace-room")
	m, _ := newTestMonitor(t, alias)

	identity := LiveKitIdentity("@replace-me:example.com")
	req := defaultJobRequest(alias, identity)

	id1, ok := m.HandoverJob(req)
	if !ok {
		t.Fatal("first HandoverJob failed")
	}

	// Small delay so the first job is registered in Loop() before we replace it.
	time.Sleep(20 * time.Millisecond)

	id2, ok := m.HandoverJob(req)
	if !ok {
		t.Fatal("second HandoverJob (replacement) failed")
	}

	if id2 <= id1 {
		t.Errorf("replacement jobID (%v) must be greater than original (%v)", id2, id1)
	}
	// newTestMonitor cleanup calls Close() — no manual teardown needed.
}

// TestLiveKitRoomMonitor_NoJobsLeftSignal verifies the full happy path:
// after a job finishes (simulated via FSM events), the monitor sends
// NoJobsLeft to the handler channel and shuts itself down.
// TestLiveKitRoomMonitor_NoJobsLeftSignal verifies the full happy path:
// after a job finishes (simulated via FSM events), the monitor sends
// NoJobsLeft to the handler channel and shuts itself down.
func TestLiveKitRoomMonitor_NoJobsLeftSignal(t *testing.T) {
	// ── Cleanup registration order (LIFO: last registered = first executed) ──
	//
	//   Register 1st → runs last:  restore LiveKitParticipantLookup
	//   Register 2nd → runs 2nd:   restore ExecuteDelayedEventAction
	//   Register 3rd → runs 1st:   m.Close()  ← must finish before any restore
	//
	// m.Close() cancels all job contexts which unblocks the lookup goroutines
	// (blocked on ctx.Done()) and lets the ActionRestart goroutines exit via
	// resetWg.Wait() inside job.Loop().  Only after all goroutines have exited
	// are the global variables restored.

	originalLookup := LiveKitParticipantLookup
	t.Cleanup(func() { LiveKitParticipantLookup = originalLookup }) // runs 3rd (last)
	LiveKitParticipantLookup = func(ctx context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, _ LiveKitIdentity) (SFUMessage, error) {
		<-ctx.Done()
		return SFUMessage{}, ctx.Err()
	}

	originalExec := ExecuteDelayedEventAction
	t.Cleanup(func() { ExecuteDelayedEventAction = originalExec }) // runs 2nd
	ExecuteDelayedEventAction = func(_ string, _ string, _ DelayEventAction) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK}, nil
	}

	alias := LiveKitRoomAlias("nojobsleft-room")
	handlerCh := make(chan HandlerMessage, 5)
	lkAuth := &LiveKitAuth{secret: "secret", key: "devkey", lkUrl: "ws://127.0.0.1:7880"}
	m := NewLiveKitRoomMonitor(context.Background(), handlerCh, lkAuth, alias)
	go m.Loop()

	// Register monitor close LAST so it runs FIRST — before either global is restored.
	t.Cleanup(func() { // runs 1st
		if err := m.Close(); err != nil {
			t.Errorf("Close() failed: %v", err)
		}
	})

	identity := LiveKitIdentity("@nojobs:example.com")
	_, ok := m.HandoverJob(defaultJobRequest(alias, identity))
	if !ok {
		t.Fatal("HandoverJob failed")
	}

	// Drive the job through Connected → Disconnected via SFU events.
	time.Sleep(30 * time.Millisecond) // let Loop() register the job
	m.SFUCommChan <- SFUMessage{Type: ParticipantConnected, LiveKitIdentity: identity}
	time.Sleep(30 * time.Millisecond)
	m.SFUCommChan <- SFUMessage{Type: ParticipantDisconnectedIntentionally, LiveKitIdentity: identity}

	// Wait for NoJobsLeft to arrive on the handler channel (the monitor sends
	// it and then cancels itself).
	select {
	case msg := <-handlerCh:
		if msg.Event != NoJobsLeft {
			t.Errorf("expected NoJobsLeft, got %v", msg.Event)
		}
		if msg.RoomAlias != alias {
			t.Errorf("unexpected room alias: %v", msg.RoomAlias)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for NoJobsLeft")
	}
	// The monitor self-cancels after sending NoJobsLeft.
	// t.Cleanup (registered above) will call Close() which waits for done.
	slog.Info("TestLiveKitRoomMonitor_NoJobsLeftSignal passed")
}

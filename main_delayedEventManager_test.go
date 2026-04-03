// Copyright 2025 Element Creations Ltd.
// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// main_delayedEventManager_test.go contains unit tests for DelayedEventJob
// (FSM transitions, timer behaviour) and the String() methods of
// DelayEventState and DelayedEventSignal defined in delayedEventManager.go.

package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ── test helpers ──────────────────────────────────────────────────────────────

// newTestJob creates a DelayedEventJob wired to a buffered monitor channel.
func newTestJob(t *testing.T, timeout time.Duration) *DelayedEventJob {
	t.Helper()
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	job, err := NewDelayedEventJob(
		context.Background(),
		&DelayedEventRequest{
			DelayCsApiUrl:   "https://matrix.example.com",
			DelayId:         "test-delay-id",
			DelayTimeout:    timeout,
			LiveKitRoom:     LiveKitRoomAlias("test-room"),
			LiveKitIdentity: LiveKitIdentity("@test:example.com"),
		},
		make(chan MonitorMessage, 20),
	)
	if err != nil {
		t.Fatalf("NewDelayedEventJob: %v", err)
	}
	return job
}

// driveJobEvents starts Loop() and sends events sequentially, pausing briefly
// between each so Loop() has time to process them.
func driveJobEvents(t *testing.T, job *DelayedEventJob, events ...DelayedEventSignal) {
	t.Helper()
	go job.Loop()
	for _, ev := range events {
		select {
		case job.EventChannel <- ev:
		case <-time.After(time.Second):
			t.Fatalf("timed out sending event %v", ev)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newJobWithMonitorCh is a convenience wrapper that creates a job and returns
// both the job and the monitor channel so tests can observe outgoing messages.
func newJobWithMonitorCh(timeout time.Duration) (*DelayedEventJob, chan MonitorMessage) {
	monitorCh := make(chan MonitorMessage, 5)
	job, _ := NewDelayedEventJob(
		context.Background(),
		&DelayedEventRequest{
			DelayCsApiUrl:   "https://example.com",
			DelayId:         "id",
			DelayTimeout:    timeout,
			LiveKitRoom:     "room",
			LiveKitIdentity: "identity",
		},
		monitorCh,
	)
	return job, monitorCh
}

// mockExecOK replaces ExecuteDelayedEventAction with a stub that always
// returns 200 OK and restores the original in t.Cleanup.
func mockExecOK(t *testing.T) {
	t.Helper()
	original := ExecuteDelayedEventAction
	ExecuteDelayedEventAction = func(_ string, _ string, _ DelayEventAction) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	}
	t.Cleanup(func() { ExecuteDelayedEventAction = original })
}

// ── construction ──────────────────────────────────────────────────────────────

// TestDelayedEventJob_InvalidTimeout verifies that a zero timeout is rejected.
func TestDelayedEventJob_InvalidTimeout(t *testing.T) {
	_, err := NewDelayedEventJob(
		context.Background(),
		&DelayedEventRequest{
			DelayCsApiUrl: "https://example.com", DelayId: "id",
			DelayTimeout: 0, LiveKitRoom: "room", LiveKitIdentity: "identity",
		},
		make(chan MonitorMessage, 1),
	)
	if err == nil {
		t.Error("expected error for zero timeout, got nil")
	}
}

// TestDelayedEventJob_String verifies that String() returns a non-empty
// description that includes the type name.
func TestDelayedEventJob_String(t *testing.T) {
	job := newTestJob(t, 10*time.Second)
	t.Cleanup(func() { job.Close() })
	s := job.String()
	if s == "" || !strings.Contains(s, "DelayedEventJob") {
		t.Errorf("String() = %q, want non-empty string containing 'DelayedEventJob'", s)
	}
}

// ── FSM: WaitingForInitialConnect transitions ─────────────────────────────────

// TestDelayedEventJob_ParticipantConnected verifies
// WaitingForInitialConnect → Connected via ParticipantConnected.
func TestDelayedEventJob_ParticipantConnected(t *testing.T) {
	mockExecOK(t)
	job := newTestJob(t, 10*time.Second)
	driveJobEvents(t, job, ParticipantConnected)
	job.Cancel()
	job.Close()
}

// TestDelayedEventJob_ParticipantLookupSuccessful verifies
// WaitingForInitialConnect → Connected via ParticipantLookupSuccessful.
func TestDelayedEventJob_ParticipantLookupSuccessful(t *testing.T) {
	mockExecOK(t)
	job := newTestJob(t, 10*time.Second)
	driveJobEvents(t, job, ParticipantLookupSuccessful)
	if err := job.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestDelayedEventJob_ConnectionAborted verifies
// WaitingForInitialConnect → Completed via ParticipantConnectionAborted,
// and that the monitor channel receives a Completed message.
func TestDelayedEventJob_ConnectionAborted(t *testing.T) {
	job, monitorCh := newJobWithMonitorCh(10 * time.Second)
	go job.Loop()

	job.EventChannel <- ParticipantConnectionAborted

	select {
	case msg := <-monitorCh:
		if msg.State != Completed {
			t.Errorf("expected Completed, got %v", msg.State)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Completed MonitorMessage")
	}
	job.Close()
}

// TestDelayedEventJob_WaitingStateTimedOut verifies that a short DelayTimeout
// causes the waiting-state timer to fire and transition
// WaitingForInitialConnect → Disconnected automatically.
func TestDelayedEventJob_WaitingStateTimedOut(t *testing.T) {
	mockExecOK(t)
	job, monitorCh := newJobWithMonitorCh(50 * time.Millisecond) // fires quickly
	go job.Loop()

	select {
	case msg := <-monitorCh:
		if msg.State != Disconnected {
			t.Errorf("expected Disconnected, got %v", msg.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for WaitingStateTimedOut → Disconnected")
	}
	job.Close()
}

// ── FSM: Connected transitions ────────────────────────────────────────────────

// TestDelayedEventJob_ParticipantDisconnected verifies
// Connected → Disconnected via ParticipantDisconnectedIntentionally,
// and that the monitor channel receives a Disconnected message.
func TestDelayedEventJob_ParticipantDisconnected(t *testing.T) {
	mockExecOK(t)
	job, monitorCh := newJobWithMonitorCh(10 * time.Second)
	go job.Loop()

	job.EventChannel <- ParticipantConnected
	time.Sleep(20 * time.Millisecond)
	job.EventChannel <- ParticipantDisconnectedIntentionally

	select {
	case msg := <-monitorCh:
		if msg.State != Disconnected {
			t.Errorf("expected Disconnected, got %v", msg.State)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Disconnected MonitorMessage")
	}
	job.Close()
}

// TestDelayedEventJob_DelayedEventTimedOut verifies Connected → Disconnected
// via DelayedEventTimedOut (cancelling before ActionSend can block).
func TestDelayedEventJob_DelayedEventTimedOut(t *testing.T) {
	mockExecOK(t)
	job, _ := newJobWithMonitorCh(10 * time.Second)
	go job.Loop()

	job.EventChannel <- ParticipantConnected
	time.Sleep(20 * time.Millisecond)
	job.Cancel() // unblocks any in-progress HTTP calls
	job.Close()
}

// TestDelayedEventJob_DelayedEventNotFound verifies Connected → Disconnected
// via an explicit DelayedEventNotFound signal.
func TestDelayedEventJob_DelayedEventNotFound(t *testing.T) {
	mockExecOK(t)
	job, _ := newJobWithMonitorCh(10 * time.Second)
	go job.Loop()

	job.EventChannel <- ParticipantConnected
	time.Sleep(20 * time.Millisecond)
	job.EventChannel <- DelayedEventNotFound
	time.Sleep(20 * time.Millisecond)
	job.Cancel()
	job.Close()
}

// ── FSM: guard conditions ─────────────────────────────────────────────────────

// TestDelayedEventJob_FSM_IgnoresWrongStateTransitions verifies that events
// received in the wrong state are silently ignored (guard conditions hold).
func TestDelayedEventJob_FSM_IgnoresWrongStateTransitions(t *testing.T) {
	mockExecOK(t)
	job := newTestJob(t, 10*time.Second)
	go job.Loop()

	// ParticipantDisconnectedIntentionally in WaitingForInitialConnect → no-op.
	job.EventChannel <- ParticipantDisconnectedIntentionally
	time.Sleep(20 * time.Millisecond)

	// ParticipantConnectionAborted in Connected → no-op.
	job.EventChannel <- ParticipantConnected
	time.Sleep(20 * time.Millisecond)
	job.EventChannel <- ParticipantConnectionAborted
	time.Sleep(20 * time.Millisecond)

	job.Cancel()
	job.Close()
}

// ── FSM: ActionRestart outcomes ───────────────────────────────────────────────

// TestDelayedEventJob_ActionRestart_404 verifies that a 404 response from
// ActionRestart causes DelayedEventNotFound to be fed back into the FSM,
// resulting in a Disconnected MonitorMessage.
func TestDelayedEventJob_ActionRestart_404(t *testing.T) {
	original := ExecuteDelayedEventAction
	ExecuteDelayedEventAction = func(_ string, _ string, action DelayEventAction) (*http.Response, error) {
		if action == ActionRestart {
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	}
	t.Cleanup(func() { ExecuteDelayedEventAction = original })

	job, monitorCh := newJobWithMonitorCh(10 * time.Second)
	go job.Loop()

	// Connected triggers an immediate DelayedEventReset.
	// Reset goroutine gets 404 → DelayedEventNotFound → Disconnected.
	job.EventChannel <- ParticipantConnected

	select {
	case msg := <-monitorCh:
		if msg.State != Disconnected {
			t.Errorf("expected Disconnected after ActionRestart 404, got %v", msg.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Disconnected after ActionRestart 404")
	}
	job.Close()
}

// TestDelayedEventJob_ActionRestart_Error verifies that an HTTP error from
// ActionRestart causes DelayedEventTimedOut to be fed back into the FSM,
// resulting in a Disconnected MonitorMessage.
func TestDelayedEventJob_ActionRestart_Error(t *testing.T) {
	original := ExecuteDelayedEventAction
	ExecuteDelayedEventAction = func(_ string, _ string, action DelayEventAction) (*http.Response, error) {
		if action == ActionRestart {
			return nil, context.DeadlineExceeded
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	}
	t.Cleanup(func() { ExecuteDelayedEventAction = original })

	// Short timeout so backoff gives up quickly after the error.
	job, monitorCh := newJobWithMonitorCh(200 * time.Millisecond)
	go job.Loop()

	job.EventChannel <- ParticipantConnected

	select {
	case msg := <-monitorCh:
		if msg.State != Disconnected {
			t.Errorf("expected Disconnected after ActionRestart error, got %v", msg.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Disconnected after ActionRestart error")
	}
	job.Close()
}

// ── String() methods ──────────────────────────────────────────────────────────

func TestDelayEventState_String(t *testing.T) {
	for _, c := range []struct {
		state DelayEventState
		want  string
	}{
		{WaitingForInitialConnect, "WaitingForInitialConnect"},
		{Connected, "Connected"},
		{Disconnected, "Disconnected"},
		{Completed, "Completed"},
		{Replaced, "Replaced"},
		{DelayEventState(99), "DelayEventState(99)"},
	} {
		if got := c.state.String(); got != c.want {
			t.Errorf("DelayEventState(%d).String() = %q, want %q", int(c.state), got, c.want)
		}
	}
}

func TestDelayedEventSignal_String(t *testing.T) {
	for _, c := range []struct {
		sig  DelayedEventSignal
		want string
	}{
		{ParticipantConnected, "ParticipantConnected"},
		{ParticipantLookupSuccessful, "ParticipantLookupSuccessful"},
		{ParticipantDisconnectedIntentionally, "ParticipantDisconnectedIntentionally"},
		{ParticipantConnectionAborted, "ParticipantConnectionAborted"},
		{DelayedEventReset, "DelayedEventReset"},
		{DelayedEventTimedOut, "DelayedEventTimedOut"},
		{DelayedEventNotFound, "DelayedEventNotFound"},
		{WaitingStateTimedOut, "WaitingStateTimedOut"},
		{SFUNotAvailable, "SFUNotAvailable"},
		{DelayedEventSignal(99), "DelayedEventSignal(99)"},
	} {
		if got := c.sig.String(); got != c.want {
			t.Errorf("DelayedEventSignal(%d).String() = %q, want %q", int(c.sig), got, c.want)
		}
	}
}

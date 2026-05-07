// Copyright 2025 Element Creations Ltd.
// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// main_delayedEventManager_test.go contains unit tests for DelayedEventJob
// (FSM transitions, timer behaviour), LiveKitRoomWorker,
// and the String() methods of DelayEventState and DelayedEventSignal.

package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ── test helpers ──────────────────────────────────────────────────────────────

// newTestJob creates a DelayedEventJob wired to a buffered done channel.
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
		make(chan *DelayedEventJob, 20),
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

// newJobWithDoneCh is a convenience wrapper that creates a job and returns
// both the job and the done channel so tests can observe terminal signals.
func newJobWithDoneCh(timeout time.Duration) (*DelayedEventJob, chan *DelayedEventJob) {
	doneCh := make(chan *DelayedEventJob, 5)
	job, _ := NewDelayedEventJob(
		context.Background(),
		&DelayedEventRequest{
			DelayCsApiUrl:   "https://example.com",
			DelayId:         "id",
			DelayTimeout:    timeout,
			LiveKitRoom:     "room",
			LiveKitIdentity: "identity",
		},
		doneCh,
	)
	return job, doneCh
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
		make(chan *DelayedEventJob, 1),
	)
	if err == nil {
		t.Error("expected error for zero timeout, got nil")
	}
}

// TestDelayedEventJob_String verifies that String() returns a non-empty
// description that includes the type name.
func TestDelayedEventJob_String(t *testing.T) {
	job := newTestJob(t, 10*time.Second)
	s := job.String()
	if s == "" || !strings.Contains(s, "DelayedEventJob") {
		t.Errorf("String() = %q, want non-empty string containing 'DelayedEventJob'", s)
	}
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// ── FSM: WaitingForInitialConnect transitions ─────────────────────────────────

// TestDelayedEventJob_ParticipantConnected verifies
// WaitingForInitialConnect → Connected via ParticipantConnected.
func TestDelayedEventJob_ParticipantConnected(t *testing.T) {
	mockExecOK(t)
	job := newTestJob(t, 10*time.Second)
	driveJobEvents(t, job, ParticipantConnected)
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if job.state != DelayEventState(Connected) {
		t.Errorf("expected state %v, got %v", DelayEventState(Connected), job.state)
	}
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
	if job.state != DelayEventState(Connected) {
		t.Errorf("expected state %v, got %v", DelayEventState(Connected), job.state)
	}
}

// TestDelayedEventJob_ConnectionAborted verifies
// WaitingForInitialConnect → Completed via ParticipantConnectionAborted,
// and that the done channel receives the job pointer.
func TestDelayedEventJob_ConnectionAborted(t *testing.T) {
	job, doneCh := newJobWithDoneCh(10 * time.Second)
	go job.Loop()

	job.EventChannel <- ParticipantConnectionAborted

	select {
	case doneJob := <-doneCh:
		if doneJob.state != Completed {
			t.Errorf("expected Completed, got %v", doneJob.state)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Completed signal on doneCh")
	}
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestDelayedEventJob_WaitingStateTimedOut verifies that a short DelayTimeout
// causes the waiting-state timer to fire and transition
// WaitingForInitialConnect → Disconnected automatically.
func TestDelayedEventJob_WaitingStateTimedOut(t *testing.T) {
	mockExecOK(t)
	job, doneCh := newJobWithDoneCh(50 * time.Millisecond) // fires quickly
	go job.Loop()

	select {
	case doneJob := <-doneCh:
		if doneJob.state != Disconnected {
			t.Errorf("expected Disconnected, got %v", doneJob.state)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for WaitingStateTimedOut → Disconnected")
	}
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// ── FSM: Connected transitions ────────────────────────────────────────────────

// TestDelayedEventJob_ParticipantDisconnected verifies
// Connected → Disconnected via ParticipantDisconnectedIntentionally,
// and that the done channel receives the job pointer with state Disconnected.
func TestDelayedEventJob_ParticipantDisconnected(t *testing.T) {
	mockExecOK(t)
	job, doneCh := newJobWithDoneCh(10 * time.Second)
	go job.Loop()

	job.EventChannel <- ParticipantConnected
	time.Sleep(20 * time.Millisecond)
	job.EventChannel <- ParticipantDisconnectedIntentionally

	select {
	case doneJob := <-doneCh:
		if doneJob.state != Disconnected {
			t.Errorf("expected Disconnected, got %v", doneJob.state)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Disconnected signal on doneCh")
	}
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestDelayedEventJob_DelayedEventTimedOut verifies Connected → Disconnected
// via DelayedEventTimedOut
func TestDelayedEventJob_DelayedEventTimedOut(t *testing.T) {
	mockExecOK(t)
	job := newTestJob(t, 10*time.Millisecond)
	go job.Loop()

	job.EventChannel <- ParticipantConnected
	time.Sleep(20 * time.Millisecond)
	job.EventChannel <- DelayedEventTimedOut
	job.Cancel() // unblocks any in-progress HTTP calls
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if job.state != DelayEventState(Disconnected) {
		t.Errorf("expected state %v, got %v", DelayEventState(Disconnected), job.state)
	}
}

// TestDelayedEventJob_DelayedEventNotFound verifies Connected → Disconnected
// via an explicit DelayedEventNotFound signal.
func TestDelayedEventJob_DelayedEventNotFound(t *testing.T) {
	mockExecOK(t)
	job := newTestJob(t, 10*time.Second)
	go job.Loop()

	job.EventChannel <- ParticipantConnected
	time.Sleep(20 * time.Millisecond)
	job.EventChannel <- DelayedEventNotFound
	time.Sleep(20 * time.Millisecond)
	job.Cancel()
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if job.state != DelayEventState(Disconnected) {
		t.Errorf("expected state %v, got %v", DelayEventState(Disconnected), job.state)
	}
}

// ── FSM: guard conditions ─────────────────────────────────────────────────────

// TestDelayedEventJob_FSM_IgnoresWrongStateTransitions verifies that events
// received in the wrong state are silently ignored (guard conditions hold).
func TestDelayedEventJob_FSM_IgnoresWrongStateTransitions(t *testing.T) {
	mockExecOK(t)

	// Test WaitingForInitialConnect state
	job := newTestJob(t, 10*time.Second)
	go job.Loop()

	// SFUParticipantGone in WaitingForInitialConnect → no-op.
	job.EventChannel <- SFUParticipantGone
	time.Sleep(20 * time.Millisecond)
	// ParticipantDisconnectedIntentionally in WaitingForInitialConnect → no-op.
	job.EventChannel <- ParticipantDisconnectedIntentionally
	time.Sleep(20 * time.Millisecond)

	job.Cancel()
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if job.state != DelayEventState(WaitingForInitialConnect) {
		t.Errorf("expected state %v, got %v", DelayEventState(WaitingForInitialConnect), job.state)
	}

	// Test Connected state
	job = newTestJob(t, 10*time.Second)
	go job.Loop()
	job.EventChannel <- ParticipantConnected // Transitioning to Connected
	time.Sleep(20 * time.Millisecond)

	// WaitingStateTimedOut in Connected → no-op.
	job.EventChannel <- WaitingStateTimedOut
	time.Sleep(20 * time.Millisecond)

	job.Cancel()
	err = job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if job.state != DelayEventState(Connected) {
		t.Errorf("expected state %v, got %v", DelayEventState(Connected), job.state)
	}

	// Test Completed state
	job = newTestJob(t, 10*time.Second)
	go job.Loop()
	job.EventChannel <- ParticipantConnectionAborted // Transitioning to Completed
	time.Sleep(20 * time.Millisecond)

	// WaitingStateTimedOut in Completed → no-op.
	job.EventChannel <- WaitingStateTimedOut
	time.Sleep(20 * time.Millisecond)

	// ParticipantConnected in Completed → no-op.
	job.EventChannel <- ParticipantConnected
	time.Sleep(20 * time.Millisecond)

	// ParticipantLookupSuccessful in Completed → no-op.
	job.EventChannel <- ParticipantLookupSuccessful
	time.Sleep(20 * time.Millisecond)

	// DelayedEventTimedOut in Completed → no-op.
	job.EventChannel <- DelayedEventTimedOut
	time.Sleep(20 * time.Millisecond)

	// DelayedEventNotFound in Completed → no-op.
	job.EventChannel <- DelayedEventNotFound
	time.Sleep(20 * time.Millisecond)

	// DelayedEventReset in Completed → no-op.
	job.EventChannel <- DelayedEventReset
	time.Sleep(20 * time.Millisecond)

	// ParticipantDisconnectedIntentionally in Completed → no-op.
	job.EventChannel <- ParticipantDisconnectedIntentionally
	time.Sleep(20 * time.Millisecond)

	// ParticipantConnectionAborted in Completed → no-op.
	job.EventChannel <- ParticipantConnectionAborted
	time.Sleep(20 * time.Millisecond)

	// SFUParticipantGone in Completed → no-op.
	job.EventChannel <- SFUParticipantGone
	time.Sleep(20 * time.Millisecond)

	job.Cancel()
	err = job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if job.state != DelayEventState(Completed) {
		t.Errorf("expected state %v, got %v", DelayEventState(Completed), job.state)
	}

	// Test Replaced state
	job = newTestJob(t, 10*time.Second)
	go job.Loop()
	job.EventChannel <- JobReplaced // Transitioning to Replaced
	time.Sleep(20 * time.Millisecond)

	// WaitingStateTimedOut in Replaced → no-op.
	job.EventChannel <- WaitingStateTimedOut
	time.Sleep(20 * time.Millisecond)

	// ParticipantConnected in Replaced → no-op.
	job.EventChannel <- ParticipantConnected
	time.Sleep(20 * time.Millisecond)

	// ParticipantLookupSuccessful in Replaced → no-op.
	job.EventChannel <- ParticipantLookupSuccessful
	time.Sleep(20 * time.Millisecond)

	// DelayedEventTimedOut in Replaced → no-op.
	job.EventChannel <- DelayedEventTimedOut
	time.Sleep(20 * time.Millisecond)

	// DelayedEventNotFound in Replaced → no-op.
	job.EventChannel <- DelayedEventNotFound
	time.Sleep(20 * time.Millisecond)

	// DelayedEventReset in Replaced → no-op.
	job.EventChannel <- DelayedEventReset
	time.Sleep(20 * time.Millisecond)

	// ParticipantDisconnectedIntentionally in Replaced → no-op.
	job.EventChannel <- ParticipantDisconnectedIntentionally
	time.Sleep(20 * time.Millisecond)

	// ParticipantConnectionAborted in Replaced → no-op.
	job.EventChannel <- ParticipantConnectionAborted
	time.Sleep(20 * time.Millisecond)

	// SFUParticipantGone in Replaced → no-op.
	job.EventChannel <- SFUParticipantGone
	time.Sleep(20 * time.Millisecond)

	job.Cancel()
	err = job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if job.state != DelayEventState(Replaced) {
		t.Errorf("expected state %v, got %v", DelayEventState(Replaced), job.state)
	}

	// Test Disconnected state
	job = newTestJob(t, 10*time.Second)
	go job.Loop()
	job.EventChannel <- ParticipantConnected // Transitioning to Connected
	time.Sleep(20 * time.Millisecond)
	job.EventChannel <- ParticipantConnectionAborted // Transitioning to Disconnected
	time.Sleep(20 * time.Millisecond)

	// WaitingStateTimedOut in Disconnected → no-op.
	job.EventChannel <- WaitingStateTimedOut
	time.Sleep(20 * time.Millisecond)

	// ParticipantConnected in Disconnected → no-op.
	job.EventChannel <- ParticipantConnected
	time.Sleep(20 * time.Millisecond)

	// ParticipantLookupSuccessful in Disconnected → no-op.
	job.EventChannel <- ParticipantLookupSuccessful
	time.Sleep(20 * time.Millisecond)

	// DelayedEventTimedOut in Disconnected → no-op.
	job.EventChannel <- DelayedEventTimedOut
	time.Sleep(20 * time.Millisecond)

	// DelayedEventNotFound in Disconnected → no-op.
	job.EventChannel <- DelayedEventNotFound
	time.Sleep(20 * time.Millisecond)

	// DelayedEventReset in Disconnected → no-op.
	job.EventChannel <- DelayedEventReset
	time.Sleep(20 * time.Millisecond)

	// ParticipantDisconnectedIntentionally in Disconnected → no-op.
	job.EventChannel <- ParticipantDisconnectedIntentionally
	time.Sleep(20 * time.Millisecond)

	// ParticipantConnectionAborted in Disconnected → no-op.
	job.EventChannel <- ParticipantConnectionAborted
	time.Sleep(20 * time.Millisecond)

	// SFUParticipantGone in Disconnected → no-op.
	job.EventChannel <- SFUParticipantGone
	time.Sleep(20 * time.Millisecond)

	job.Cancel()
	err = job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if job.state != DelayEventState(Disconnected) {
		t.Errorf("expected state %v, got %v", DelayEventState(Disconnected), job.state)
	}
}

// ── FSM: ActionRestart outcomes ───────────────────────────────────────────────

// TestDelayedEventJob_ActionRestart_404 verifies that a 404 response from
// ActionRestart causes DelayedEventNotFound to be fed back into the FSM,
// resulting in a Disconnected signal on doneCh.
func TestDelayedEventJob_ActionRestart_404(t *testing.T) {
	original := ExecuteDelayedEventAction
	ExecuteDelayedEventAction = func(_ string, _ string, action DelayEventAction) (*http.Response, error) {
		if action == ActionRestart {
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	}
	t.Cleanup(func() { ExecuteDelayedEventAction = original })

	job, doneCh := newJobWithDoneCh(10 * time.Second)
	go job.Loop()

	// Connected triggers an immediate DelayedEventReset.
	// Reset goroutine gets 404 → DelayedEventNotFound → Disconnected.
	job.EventChannel <- ParticipantConnected

	select {
	case doneJob := <-doneCh:
		if doneJob.state != Disconnected {
			t.Errorf("expected Disconnected after ActionRestart 404, got %v", doneJob.state)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Disconnected after ActionRestart 404")
	}
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestDelayedEventJob_ActionRestart_Error verifies that an HTTP error from
// ActionRestart causes DelayedEventTimedOut to be fed back into the FSM,
// resulting in a Disconnected signal on doneCh.
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
	job, doneCh := newJobWithDoneCh(200 * time.Millisecond)
	go job.Loop()

	job.EventChannel <- ParticipantConnected

	select {
	case doneJob := <-doneCh:
		if doneJob.state != Disconnected {
			t.Errorf("expected Disconnected after ActionRestart error, got %v", doneJob.state)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Disconnected after ActionRestart error")
	}
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
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
		{JobReplaced, "JobReplaced"},
		{SFUParticipantGone, "SFUParticipantGone"},
		{DelayedEventSignal(99), "DelayedEventSignal(99)"},
	} {
		if got := c.sig.String(); got != c.want {
			t.Errorf("DelayedEventSignal(%d).String() = %q, want %q", int(c.sig), got, c.want)
		}
	}
}

// ── JobReplaced signal ────────────────────────────────────────────────────────

// TestDelayedEventJob_JobReplaced_SignalReceived verifies the normal path:
// when JobReplaced is sent on EventChannel while Loop() is running, the job
// transitions to Replaced state and then exits cleanly via Cancel().
func TestDelayedEventJob_JobReplaced_SignalReceived(t *testing.T) {
	job := newTestJob(t, 10*time.Second)
	go job.Loop()

	// Send JobReplaced — Loop() processes it and sets state = Replaced.
	select {
	case job.EventChannel <- JobReplaced:
	case <-time.After(time.Second):
		t.Fatal("timed out sending JobReplaced")
	}

	// Small delay so Loop() can process the signal before we check the state.
	time.Sleep(20 * time.Millisecond)

	// Now cancel and close — Loop() should exit promptly.
	job.Cancel()
	done := make(chan struct{})
	go func() {
		err := job.Close()
		if err != nil {
			t.Errorf("Close: %v", err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("job did not close in time after JobReplaced")
	}
	if job.state != DelayEventState(Replaced) {
		t.Errorf("expected state %v, got %v", DelayEventState(Replaced), job.state)
	}
}

// TestDelayedEventJob_JobReplaced_FullChannel verifies the default (drop)
// path of the non-blocking JobReplaced send:
//
//	select {
//	case existing.EventChannel <- JobReplaced:
//	default:   ← this branch
//	}
//
// When EventChannel is full the signal is dropped, but the job must still be
// cancelled and closed cleanly — no deadlock, no goroutine leak.
func TestDelayedEventJob_JobReplaced_FullChannel(t *testing.T) {
	// Create a job but do NOT start Loop() yet, so the EventChannel fills up.
	job := newTestJob(t, 10*time.Second)

	// Fill EventChannel to capacity (buffer = 10) with no-op signals.
	for i := 0; i < cap(job.EventChannel); i++ {
		job.EventChannel <- SFUNotAvailable
	}

	// Now attempt the non-blocking JobReplaced send — must take the default branch.
	sent := false
	select {
	case job.EventChannel <- JobReplaced:
		sent = true // should NOT happen: channel is full
	default:
		// Expected: signal dropped because channel is full.
	}
	if sent {
		t.Error("expected JobReplaced to be dropped (default branch), but it was sent")
	}

	// Even without the signal, Cancel() + Close() must complete cleanly.
	go job.Loop() // start Loop() so it can drain and exit
	job.Cancel()
	done := make(chan struct{})
	go func() {
		if err := job.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("job did not close cleanly after full-channel JobReplaced drop")
	}
	// We are still in state WaitingForInitialConnect because the JobReplaced signal was dropped,
	// but that's fine — the key is that we didn't deadlock and Loop() exited cleanly.
	if job.state != DelayEventState(WaitingForInitialConnect) {
		t.Errorf("expected state %v, got %v", DelayEventState(WaitingForInitialConnect), job.state)
	}
}

// ── Sanity check (SFUParticipantGone) ────────────────────────────────────────

// TestDelayedEventJob_SFUParticipantGone_Connected verifies that
// SFUParticipantGone in the Connected state triggers → Disconnected.
func TestDelayedEventJob_SFUParticipantGone_Connected(t *testing.T) {
	mockExecOK(t)
	job, doneCh := newJobWithDoneCh(10 * time.Second)
	go job.Loop()

	job.EventChannel <- ParticipantConnected
	time.Sleep(20 * time.Millisecond)
	job.EventChannel <- SFUParticipantGone

	select {
	case doneJob := <-doneCh:
		if doneJob.state != Disconnected {
			t.Errorf("expected Disconnected after SFUParticipantGone, got %v", doneJob.state)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Disconnected after SFUParticipantGone")
	}
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestDelayedEventJob_SFUParticipantGone_WrongState verifies that
// SFUParticipantGone in WaitingForInitialConnect is a no-op (guard condition).
func TestDelayedEventJob_SFUParticipantGone_WrongState(t *testing.T) {
	mockExecOK(t)
	job := newTestJob(t, 10*time.Second)
	go job.Loop()

	job.EventChannel <- SFUParticipantGone // should be ignored in WaitingForInitialConnect
	time.Sleep(20 * time.Millisecond)

	job.Cancel()
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// ── LiveKitRoomWorker ─────────────────────────────────────────────────────────

// ── startParticipantLookup ────────────────────────────────────────────────────

// TestParticipantLookup_Phase1_FindsParticipant verifies that startParticipantLookup
// immediately calls LiveKitGetParticipant and delivers ParticipantLookupSuccessful
// to the job when the participant is present (sanityInterval == 0: one attempt only).
func TestParticipantLookup_Phase1_FindsParticipant(t *testing.T) {
	const identity LiveKitIdentity = "@alice:example.com"
	const room LiveKitRoomAlias = "phase1-room"

	phase1Done := make(chan struct{})
	original := LiveKitGetParticipant
	t.Cleanup(func() { LiveKitGetParticipant = original })
	LiveKitGetParticipant = func(_ context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, _ LiveKitIdentity) error {
		select {
		case <-phase1Done:
		default:
			close(phase1Done)
		}
		return nil // participant present
	}

	mockExecOK(t)

	doneCh := make(chan *DelayedEventJob, 5)
	job, err := NewDelayedEventJob(
		context.Background(),
		&DelayedEventRequest{
			DelayCsApiUrl:   "https://example.com",
			DelayId:         "phase1-id",
			DelayTimeout:    10 * time.Second,
			LiveKitRoom:     room,
			LiveKitIdentity: identity,
		},
		doneCh,
	)
	if err != nil {
		t.Fatalf("NewDelayedEventJob: %v", err)
	}
	go job.Loop()
	// sanityInterval == 0: one attempt only, no Phase 2.
	startParticipantLookup(job, LiveKitAuth{secret: "secret", key: "devkey", lkUrl: "ws://127.0.0.1:7880"}, 0)

	select {
	case <-phase1Done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Phase 1 ListParticipants call")
	}

	job.Cancel()
	if err := job.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestParticipantLookup_Phase2_DetectsGoneParticipant verifies the Phase 2 path:
// after Phase 1 confirms the participant, the periodic ticker fires a
// ListParticipants that no longer includes the identity, causing
// startParticipantLookup to send SFUParticipantGone and the job to enter Disconnected.
func TestParticipantLookup_Phase2_DetectsGoneParticipant(t *testing.T) {
	const identity LiveKitIdentity = "@bob:example.com"
	const room LiveKitRoomAlias = "phase2-room"

	callCount := 0
	original := LiveKitGetParticipant
	t.Cleanup(func() { LiveKitGetParticipant = original })
	LiveKitGetParticipant = func(_ context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, _ LiveKitIdentity) error {
		callCount++
		if callCount == 1 {
			return nil // Phase 1: participant present.
		}
		return errors.New("not found") // Phase 2: participant gone.
	}

	mockExecOK(t)

	const sanityInterval = 50 * time.Millisecond
	doneCh := make(chan *DelayedEventJob, 5)
	job, err := NewDelayedEventJob(
		context.Background(),
		&DelayedEventRequest{
			DelayCsApiUrl:   "https://example.com",
			DelayId:         "phase2-id",
			DelayTimeout:    10 * time.Second,
			LiveKitRoom:     room,
			LiveKitIdentity: identity,
		},
		doneCh,
	)
	if err != nil {
		t.Fatalf("NewDelayedEventJob: %v", err)
	}
	go job.Loop()
	startParticipantLookup(job, LiveKitAuth{secret: "secret", key: "devkey", lkUrl: "ws://127.0.0.1:7880"}, sanityInterval)

	select {
	case doneJob := <-doneCh:
		if doneJob.state != Disconnected {
			t.Errorf("expected Disconnected after Phase 2 detected gone participant, got %v", doneJob.state)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Disconnected after Phase 2 sanity check")
	}
	if err := job.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestParticipantLookup_Phase2_Disabled verifies that when sanityInterval is 0,
// startParticipantLookup makes exactly one ListParticipants call (Phase 1) and
// then the goroutine exits — no Phase 2 ticker fires.
func TestParticipantLookup_Phase2_Disabled(t *testing.T) {
	const identity LiveKitIdentity = "@carol:example.com"
	const room LiveKitRoomAlias = "phase2-disabled-room"

	// phase1Done is closed on the first (and only) GetParticipant call.
	// callCount is read only after job.Close() ensures the lookup goroutine
	// has exited (backgroundWg.Wait()), so there is no concurrent access.
	phase1Done := make(chan struct{})
	callCount := 0
	original := LiveKitGetParticipant
	t.Cleanup(func() { LiveKitGetParticipant = original })
	LiveKitGetParticipant = func(_ context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, _ LiveKitIdentity) error {
		callCount++
		select {
		case <-phase1Done:
		default:
			close(phase1Done)
		}
		return nil // participant present
	}

	mockExecOK(t)

	doneCh := make(chan *DelayedEventJob, 5)
	job, err := NewDelayedEventJob(
		context.Background(),
		&DelayedEventRequest{
			DelayCsApiUrl:   "https://example.com",
			DelayId:         "disabled-id",
			DelayTimeout:    10 * time.Second,
			LiveKitRoom:     room,
			LiveKitIdentity: identity,
		},
		doneCh,
	)
	if err != nil {
		t.Fatalf("NewDelayedEventJob: %v", err)
	}
	go job.Loop()
	// sanityInterval == 0 disables Phase 2.
	startParticipantLookup(job, LiveKitAuth{secret: "secret", key: "devkey", lkUrl: "ws://127.0.0.1:7880"}, 0)

	// Wait for Phase 1 to complete.
	select {
	case <-phase1Done:
	case <-time.After(5 * time.Second):
		job.Cancel()
		t.Fatal("timed out waiting for Phase 1")
	}

	// Cancel the job and wait for Close() — this waits for backgroundWg which
	// includes the lookup goroutine.  After this, callCount is safe to read.
	job.Cancel()
	if err := job.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected exactly 1 ListParticipants call (Phase 2 disabled), got %d", callCount)
	}
}

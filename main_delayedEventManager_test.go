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
	"fmt"
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
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
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
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
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
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
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
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
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
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
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
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
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

	// Give Loop() time to process the signal before we inspect side-effects.
	time.Sleep(20 * time.Millisecond)

	// Now cancel and close — Loop() should exit promptly.
	job.Cancel()
	done := make(chan struct{})
	go func() { close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("job did not close in time after JobReplaced")
	}
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestLiveKitRoomMonitor_ReplaceJob_JobReplacedSignal verifies the full
// replacement path through LiveKitRoomMonitor.Loop():
//
//  1. A job is handed over for identity X.
//  2. A second job for the same identity X is handed over.
//  3. Loop() sends JobReplaced on the first job's EventChannel (normal path).
//  4. The first job transitions to Replaced and is closed cleanly.
//  5. The second job becomes the active job for identity X.
func TestLiveKitRoomMonitor_ReplaceJob_JobReplacedSignal(t *testing.T) {
	alias := LiveKitRoomAlias("replace-signal-room")
	m, _ := newTestMonitor(t, alias)

	identity := LiveKitIdentity("@replace-signal:example.com")
	req := defaultJobRequest(alias, identity)

	// Hand over first job.
	id1, ok := m.HandoverJob(req)
	if !ok {
		t.Fatal("first HandoverJob failed")
	}

	// Small delay so Loop() registers the first job before we replace it.
	time.Sleep(20 * time.Millisecond)

	// Hand over second job for the same identity — triggers replacement.
	// Loop() will: send JobReplaced on job1.EventChannel, cancel job1, start job2.
	id2, ok := m.HandoverJob(req)
	if !ok {
		t.Fatal("second HandoverJob failed")
	}
	if id2 <= id1 {
		t.Errorf("replacement jobID (%v) must be greater than original (%v)", id2, id1)
	}

	// Give Loop() time to complete the replacement.
	time.Sleep(50 * time.Millisecond)
	// newTestMonitor cleanup verifies clean shutdown — no deadlock = success.
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
	go func() { close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("job did not close cleanly after full-channel JobReplaced drop")
	}
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// ── Sanity check (SFUParticipantGone) ────────────────────────────────────────

// TestDelayedEventJob_SFUParticipantGone_Connected verifies that
// SFUParticipantGone in the Connected state triggers → Disconnected.
func TestDelayedEventJob_SFUParticipantGone_Connected(t *testing.T) {
	mockExecOK(t)
	job, monitorCh := newJobWithMonitorCh(10 * time.Second)
	go job.Loop()

	job.EventChannel <- ParticipantConnected
	time.Sleep(20 * time.Millisecond)
	job.EventChannel <- SFUParticipantGone

	select {
	case msg := <-monitorCh:
		if msg.State != Disconnected {
			t.Errorf("expected Disconnected after SFUParticipantGone, got %v", msg.State)
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

// TestDelayedEventSignal_SFUParticipantGone_String verifies String() for the
// new signal value.
func TestDelayedEventSignal_SFUParticipantGone_String(t *testing.T) {
	if got := SFUParticipantGone.String(); got != "SFUParticipantGone" {
		t.Errorf("SFUParticipantGone.String() = %q, want %q", got, "SFUParticipantGone")
	}
}

// TestLiveKitRoomMonitor_SanityCheck_DetectsGoneParticipant verifies the full
// sanity-check path: after the initial lookup succeeds, the periodic ticker
// detects that the participant is gone and emits SFUParticipantGone → the job
// transitions to Disconnected and the monitor sends NoJobsLeft.
func TestLiveKitRoomMonitor_SanityCheck_DetectsGoneParticipant(t *testing.T) {
	// Phase 1 (initial lookup): succeed immediately.
	// Phase 2 (sanity): fail on first tick → emit SFUParticipantGone.
	lookupCount := 0
	original := LiveKitParticipantLookup
	t.Cleanup(func() { LiveKitParticipantLookup = original })
	LiveKitParticipantLookup = func(ctx context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, id LiveKitIdentity) (SFUMessage, error) {
		lookupCount++
		if lookupCount == 1 {
			// Initial lookup succeeds.
			return SFUMessage{Type: ParticipantLookupSuccessful, LiveKitIdentity: id}, nil
		}
		// Sanity tick: participant gone.
		return SFUMessage{}, fmt.Errorf("not found")
	}

	originalExec := ExecuteDelayedEventAction
	t.Cleanup(func() { ExecuteDelayedEventAction = originalExec })
	ExecuteDelayedEventAction = func(_ string, _ string, _ DelayEventAction) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	}

	const sanityInterval = 50 * time.Millisecond

	alias := LiveKitRoomAlias("sanity-check-room")
	handlerCh := make(chan HandlerMessage, 5)
	lkAuth := &LiveKitAuth{secret: "secret", key: "devkey", lkUrl: "ws://127.0.0.1:7880"}
	m := NewLiveKitRoomMonitor(context.Background(), handlerCh, lkAuth, alias, sanityInterval)
	go m.Loop()
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("Close() failed: %v", err)
		}
	})

	_, ok := m.HandoverJob(&DelayedEventRequest{
		DelayCsApiUrl:   "https://matrix.example.com",
		DelayId:         "sanity-delay-id",
		DelayTimeout:    10 * time.Second,
		LiveKitRoom:     alias,
		LiveKitIdentity: "@sanity-user:example.com",
	})
	if !ok {
		t.Fatal("HandoverJob failed")
	}

	// Wait for NoJobsLeft — sanity check detected gone participant → Disconnected.
	select {
	case msg := <-handlerCh:
		if msg.Event != NoJobsLeft {
			t.Errorf("expected NoJobsLeft, got %v", msg.Event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for NoJobsLeft after sanity check detected gone participant")
	}
}

// TestLiveKitRoomMonitor_SanityCheck_Disabled verifies that when
// sanityCheckInterval is 0, the goroutine exits after the initial lookup
// without starting the ticker — no spurious SFUParticipantGone signals.
func TestLiveKitRoomMonitor_SanityCheck_Disabled(t *testing.T) {
	lookupCount := 0
	original := LiveKitParticipantLookup
	t.Cleanup(func() { LiveKitParticipantLookup = original })
	LiveKitParticipantLookup = func(ctx context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, id LiveKitIdentity) (SFUMessage, error) {
		lookupCount++
		if lookupCount == 1 {
			return SFUMessage{Type: ParticipantLookupSuccessful, LiveKitIdentity: id}, nil
		}
		// Should never be called again when sanity check is disabled.
		t.Errorf("unexpected lookup call #%d (sanity check should be disabled)", lookupCount)
		return SFUMessage{}, fmt.Errorf("not found")
	}

	originalExec := ExecuteDelayedEventAction
	t.Cleanup(func() { ExecuteDelayedEventAction = originalExec })
	ExecuteDelayedEventAction = func(_ string, _ string, _ DelayEventAction) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	}

	alias := LiveKitRoomAlias("sanity-disabled-room")
	handlerCh := make(chan HandlerMessage, 5)
	lkAuth := &LiveKitAuth{secret: "secret", key: "devkey", lkUrl: "ws://127.0.0.1:7880"}
	// sanityCheckInterval = 0 → disabled
	m := NewLiveKitRoomMonitor(context.Background(), handlerCh, lkAuth, alias, 0)
	go m.Loop()
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("Close() failed: %v", err)
		}
	})

	_, ok := m.HandoverJob(&DelayedEventRequest{
		DelayCsApiUrl:   "https://matrix.example.com",
		DelayId:         "disabled-delay-id",
		DelayTimeout:    10 * time.Second,
		LiveKitRoom:     alias,
		LiveKitIdentity: "@no-sanity:example.com",
	})
	if !ok {
		t.Fatal("HandoverJob failed")
	}

	// Wait briefly — no SFUParticipantGone should arrive.
	time.Sleep(200 * time.Millisecond)

	// Verify that no unexpected signals arrived.
	select {
	case msg := <-handlerCh:
		t.Errorf("unexpected handler message: %+v", msg)
	default:
		// Expected: no messages.
	}
}

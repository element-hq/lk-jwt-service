// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// delayedEventManager_test.go: unit tests for DelayedEventJob (FSM
// transitions, timer behaviour), startParticipantLookup (Phase 1 / Phase 2),
// and the String() methods of DelayEventState and DelayedEventSignal
// (delayedEventManager.go).

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v5"
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
		DelayedEventJobParams{
			ServerName:      "example.com",
			DelayId:         "test-delay-id",
			DelayTimeout:    timeout,
			LiveKitRoom:     LiveKitRoomAlias("test-room"),
			LiveKitIdentity: LiveKitIdentity("@test:example.com"),
		},
		lookUpCsApiUrlFromOverrideOnly("example.com", "https://matrix-client.example.com"),
		make(chan *DelayedEventJob, 20),
	)
	if err != nil {
		t.Fatalf("NewDelayedEventJob: %v", err)
	}
	return job
}

func lookUpCsApiUrlFromOverrideOnly(serverNameOverride string, urlOverride string) func(context.Context, string) (CsApiUrl, error) {
	return func(_ context.Context, serverName string) (CsApiUrl, error) {
		if serverName == serverNameOverride {
			return CsApiUrl(urlOverride), nil
		}
		return "", fmt.Errorf("trying to resolve CS-API URL for unexpected server name %s", serverName)
	}
}

// driveJobEvents starts loop() and sends events sequentially. The
// select+timeout bounds a send that would otherwise block on a full
// EventChannel.
func driveJobEvents(t *testing.T, job *DelayedEventJob, events ...DelayedEventSignal) {
	t.Helper()
	go job.loop()
	for _, ev := range events {
		select {
		case job.EventChannel <- ev:
		case <-time.After(time.Second):
			t.Fatalf("timed out sending event %v", ev)
		}
		// Let the event settle: "whatever comes next" can act on the
		// changes this event produced, not race past it.
		time.Sleep(10 * time.Millisecond)
	}
}

// newJobWithDoneCh is a convenience wrapper that creates a job and returns
// both the job and the done channel so tests can observe terminal signals.
func newJobWithDoneCh(timeout time.Duration) (*DelayedEventJob, chan *DelayedEventJob) {
	doneCh := make(chan *DelayedEventJob, 5)
	job, _ := NewDelayedEventJob(
		context.Background(),
		DelayedEventJobParams{
			ServerName:      "example.com",
			DelayId:         "id",
			DelayTimeout:    timeout,
			LiveKitRoom:     "room",
			LiveKitIdentity: "identity",
		},
		lookUpCsApiUrlFromOverrideOnly("example.com", "https://matrix-client.example.com"),
		doneCh,
	)
	return job, doneCh
}

// mockExecOK replaces ExecuteDelayedEventAction with a stub that always
// returns 200 OK and restores the original in t.Cleanup.
func mockExecOK(t *testing.T) {
	t.Helper()
	original := ExecuteDelayedEventAction
	ExecuteDelayedEventAction = func(_ CsApiUrl, _ string, _ DelayEventAction) (int, error) {
		return http.StatusOK, nil
	}
	t.Cleanup(func() { ExecuteDelayedEventAction = original })
}

// ── construction ──────────────────────────────────────────────────────────────

// TestDelayedEventJob_InvalidTimeout verifies that a zero timeout is rejected.
func TestDelayedEventJob_InvalidTimeout(t *testing.T) {
	_, err := NewDelayedEventJob(
		context.Background(),
		DelayedEventJobParams{
			ServerName:      "example.com",
			DelayId:         "id",
			DelayTimeout:    0,
			LiveKitRoom:     "room",
			LiveKitIdentity: "identity",
		},
		func(_ context.Context, _ string) (CsApiUrl, error) { return "", nil },
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
	job.Stop() // loop() was never started — Close() would time out on job.done
}

// ── FSM: table invariants ─────────────────────────────────────────────────────

// TestFSMTables_InternalAndTransitionDisjoint asserts the FSM dispatch
// invariant: no (state, event) pair may be registered both as an internal
// action and as a transition — otherwise the internal-first dispatch order
// in handleEvent would silently shadow the transition.
func TestFSMTables_InternalAndTransitionDisjoint(t *testing.T) {
	for state, events := range fsmInternalActions {
		for event := range events {
			if _, ok := fsmTransitions[state][event]; ok {
				t.Errorf("FSM: (%s, %s) registered as both internal action and transition",
					state, event)
			}
		}
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
// WaitingForInitialConnect → Aborted via ParticipantConnectionAborted,
// and that the done channel receives the job pointer.
func TestDelayedEventJob_ConnectionAborted(t *testing.T) {
	job, doneCh := newJobWithDoneCh(10 * time.Second)
	go job.loop()

	job.EventChannel <- ParticipantConnectionAborted

	select {
	case doneJob := <-doneCh:
		if doneJob.state != Aborted {
			t.Errorf("expected Aborted, got %v", doneJob.state)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Aborted signal on doneCh")
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
	go job.loop()

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
	go job.loop()

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
	go job.loop()

	job.EventChannel <- ParticipantConnected
	time.Sleep(20 * time.Millisecond)
	job.EventChannel <- DelayedEventTimedOut
	time.Sleep(20 * time.Millisecond)
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
	go job.loop()

	job.EventChannel <- ParticipantConnected
	time.Sleep(20 * time.Millisecond)
	job.EventChannel <- DelayedEventNotFound
	time.Sleep(20 * time.Millisecond)
	err := job.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if job.state != DelayEventState(Disconnected) {
		t.Errorf("expected state %v, got %v", DelayEventState(Disconnected), job.state)
	}
}

// TestDelayedEventJob_ResetWithExhaustedDeadline verifies the
// startDelayedEventRestart contract for an already-expired deadline: no
// homeserver call is made and DelayedEventTimedOut is emitted instead.
// The method is called directly — loop() is not running — so the test is
// fully deterministic.
func TestDelayedEventJob_ResetWithExhaustedDeadline(t *testing.T) {
	var calls atomic.Int32
	original := ExecuteDelayedEventAction
	ExecuteDelayedEventAction = func(csApiUrl CsApiUrl, _ string, _ DelayEventAction) (int, error) {
		if csApiUrl != "https://matrix-client.example.com" {
			t.Errorf("got unexpected client-server API URL %v", csApiUrl)
		}
		calls.Add(1)
		return http.StatusOK, nil
	}
	t.Cleanup(func() { ExecuteDelayedEventAction = original })

	job := newTestJob(t, 10*time.Second)
	job.restartDeadline = time.Now().Add(-time.Second) // already expired

	job.startDelayedEventRestart()

	select {
	case ev := <-job.EventChannel:
		if ev != DelayedEventTimedOut {
			t.Errorf("expected DelayedEventTimedOut, got %v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for DelayedEventTimedOut")
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("expected no homeserver call, got %d", n)
	}
	job.Stop() // loop() was never started — Close() would time out on job.done
}

func TestDelayedEventJob_ResetWithFailingCsApiUrlResolution(t *testing.T) {
	job, err := NewDelayedEventJob(
		context.Background(),
		DelayedEventJobParams{
			ServerName:      "example.com",
			DelayId:         "id",
			DelayTimeout:    10 * time.Second,
			LiveKitRoom:     "room",
			LiveKitIdentity: "identity",
		},
		func(_ context.Context, _ string) (CsApiUrl, error) { return "", fmt.Errorf("M_NOT_FOUND") },
		make(chan *DelayedEventJob, 5),
	)
	if err != nil {
		t.Fatalf("NewDelayedEventJob: %v", err)
	}
	job.restartDeadline = time.Now().Add(time.Second)

	job.startDelayedEventRestart()

	select {
	case ev := <-job.EventChannel:
		if ev != DelayedEventNotFound {
			t.Errorf("expected DelayedEventNotFound, got %v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for DelayedEventNotFound")
	}
	job.Stop()
}

// TestDelayedEventJob_ActionSendBoundedWhenNoTimeRemains verifies that when
// Disconnected is reached without a restart deadline (waiting state timed out
// before any connect), ActionSend is bounded by the one-second floor — even
// against a failing homeserver — and still notifies doneCh instead of
// retrying forever (backoff v5 treats MaxElapsedTime(0) as "retry forever").
func TestDelayedEventJob_ActionSendBoundedWhenNoTimeRemains(t *testing.T) {
	var calls atomic.Int32
	original := ExecuteDelayedEventAction
	ExecuteDelayedEventAction = func(csApiUrl CsApiUrl, _ string, _ DelayEventAction) (int, error) {
		calls.Add(1)
		if csApiUrl != "https://matrix-client.example.com" {
			t.Errorf("got unexpected client-server API URL %v", csApiUrl)
		}
		return http.StatusInternalServerError, errors.New("homeserver down")
	}
	t.Cleanup(func() { ExecuteDelayedEventAction = original })

	job, doneCh := newJobWithDoneCh(50 * time.Millisecond) // waiting timer fires quickly
	go job.loop()

	select {
	case doneJob := <-doneCh:
		if doneJob.state != Disconnected {
			t.Errorf("expected Disconnected, got %v", doneJob.state)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for doneCh — ActionSend may be retrying forever")
	}
	// The one-second window allows 1-2 attempts (randomized first backoff
	// interval); anything more means the elapsed-time bound is not applied.
	if n := calls.Load(); n < 1 || n > 2 {
		t.Errorf("expected 1-2 ActionSend attempts, got %d", n)
	}
	if err := job.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestDelayedEventJob_ActionSendWithFailingCsApiUrlResolution(t *testing.T) {
	var calls atomic.Int32
	original := ExecuteDelayedEventAction
	ExecuteDelayedEventAction = func(_ CsApiUrl, _ string, _ DelayEventAction) (int, error) {
		calls.Add(1)
		return http.StatusInternalServerError, errors.New("homeserver down")
	}
	t.Cleanup(func() { ExecuteDelayedEventAction = original })

	doneCh := make(chan *DelayedEventJob, 5)
	job, _ := NewDelayedEventJob(
		context.Background(),
		DelayedEventJobParams{
			ServerName:      "example.com",
			DelayId:         "id",
			DelayTimeout:    50 * time.Millisecond, // waiting timer fires quickly
			LiveKitRoom:     "room",
			LiveKitIdentity: "identity",
		},
		func(_ context.Context, _ string) (CsApiUrl, error) { return "", fmt.Errorf("M_NOT_FOUND") },
		doneCh,
	)
	go job.loop()

	select {
	case doneJob := <-doneCh:
		if doneJob.state != Disconnected {
			t.Errorf("expected Disconnected, got %v", doneJob.state)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for doneCh — ActionSend may be retrying forever")
	}
	if n := calls.Load(); n > 0 {
		t.Errorf("expected no ActionSend attempts, got %d", n)
	}
	if err := job.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// ── FSM: guard conditions ─────────────────────────────────────────────────────

// TestDelayedEventJob_FSM_IgnoresWrongStateTransitions verifies every
// (state, event) pair that is registered neither as a transition nor as an
// internal action: the event must be ignored and the state stay unchanged.
// Each pair runs against a fresh job, so every no-op is verified
// individually — not just the sum of several no-ops.
func TestDelayedEventJob_FSM_IgnoresWrongStateTransitions(t *testing.T) {
	mockExecOK(t)

	// Event sequences that drive a fresh job into each state.
	statePaths := map[DelayEventState][]DelayedEventSignal{
		WaitingForInitialConnect: {},
		Connected:                {ParticipantConnected},
		Disconnected:             {ParticipantConnected, ParticipantDisconnectedIntentionally},
		Aborted:                  {ParticipantConnectionAborted},
		Replaced:                 {JobReplaced},
	}
	allEvents := []DelayedEventSignal{
		ParticipantConnected, ParticipantLookupSuccessful,
		ParticipantDisconnectedIntentionally, ParticipantConnectionAborted,
		DelayedEventReset, DelayedEventTimedOut, DelayedEventNotFound,
		WaitingStateTimedOut, SFUNotAvailable, JobReplaced, SFUParticipantGone,
	}

	for state, path := range statePaths {
		for _, event := range allEvents {
			if _, ok := fsmTransitions[state][event]; ok {
				continue // legal transition — covered by dedicated tests
			}
			if _, ok := fsmInternalActions[state][event]; ok {
				continue // internal action — not a no-op
			}
			t.Run(fmt.Sprintf("%v in %v", event, state), func(t *testing.T) {
				job := newTestJob(t, 10*time.Second)
				events := append([]DelayedEventSignal{}, path...)
				events = append(events, event)
				driveJobEvents(t, job, events...)
				if err := job.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				if job.state != state {
					t.Errorf("expected state %v after %v, got %v", state, event, job.state)
				}
			})
		}
	}
}

// ── FSM: ActionRestart outcomes ───────────────────────────────────────────────

// TestDelayedEventJob_ActionRestart_404 verifies that the
// errDelayedEventNotFound sentinel (returned by the helper for a 404 on
// ActionRestart, wrapped in backoff.Permanent so backoff.Retry stops immediately)
// causes DelayedEventNotFound to be fed back into the FSM, resulting in a
// Disconnected signal on doneCh.
func TestDelayedEventJob_ActionRestart_404(t *testing.T) {
	original := ExecuteDelayedEventAction
	ExecuteDelayedEventAction = func(csApiUrl CsApiUrl, _ string, action DelayEventAction) (int, error) {
		if csApiUrl != "https://matrix-client.example.com" {
			t.Errorf("got unexpected client-server API URL %v", csApiUrl)
		}
		if action == ActionRestart {
			return http.StatusNotFound, backoff.Permanent(errDelayedEventNotFound)
		}
		return http.StatusOK, nil
	}
	t.Cleanup(func() { ExecuteDelayedEventAction = original })

	job, doneCh := newJobWithDoneCh(10 * time.Second)
	go job.loop()

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
	ExecuteDelayedEventAction = func(csApiUrl CsApiUrl, _ string, action DelayEventAction) (int, error) {
		if csApiUrl != "https://matrix-client.example.com" {
			t.Errorf("got unexpected client-server API URL %v", csApiUrl)
		}
		if action == ActionRestart {
			return 0, context.DeadlineExceeded
		}
		return http.StatusOK, nil
	}
	t.Cleanup(func() { ExecuteDelayedEventAction = original })

	// Short timeout so backoff gives up quickly after the error.
	job, doneCh := newJobWithDoneCh(200 * time.Millisecond)
	go job.loop()

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
		{Aborted, "Aborted"},
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
// when JobReplaced is sent on EventChannel while loop() is running, the job
// transitions to Replaced state and then exits cleanly via Stop().
func TestDelayedEventJob_JobReplaced_SignalReceived(t *testing.T) {
	job := newTestJob(t, 10*time.Second)
	go job.loop()

	// Send JobReplaced — loop() processes it and sets state = Replaced.
	select {
	case job.EventChannel <- JobReplaced:
	case <-time.After(time.Second):
		t.Fatal("timed out sending JobReplaced")
	}

	// Small delay so loop() can process the signal before we check the state.
	time.Sleep(20 * time.Millisecond)

	// Now cancel and close — loop() should exit promptly.
	job.Stop()
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
	// Create a job but do NOT start loop() yet, so the EventChannel fills up.
	job := newTestJob(t, 10*time.Second)

	// Fill EventChannel to capacity (buffer = 10) with no-op signals.
	for i := 0; i < cap(job.EventChannel); i++ {
		job.EventChannel <- SFUNotAvailable
	}

	// Now attempt the non-blocking JobReplaced send — must take the default branch.
	select {
	case job.EventChannel <- JobReplaced:
		t.Error("expected JobReplaced to be dropped (default branch), but it was sent")
	default:
		// Expected: signal dropped because channel is full.
	}

	// Even without the signal, Stop() + Close() must complete cleanly.
	go job.loop() // start loop() so it can drain and exit
	job.Stop()
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
	// but that's fine — the key is that we didn't deadlock and loop() exited cleanly.
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
	go job.loop()

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

// ── LiveKitRoomWorker ─────────────────────────────────────────────────────────

// ── startParticipantLookup ────────────────────────────────────────────────────

// TestParticipantLookup_Phase1_FindsParticipant verifies that startParticipantLookup
// immediately calls LiveKitParticipantExists and delivers ParticipantLookupSuccessful
// to the job when the participant is present (sanityInterval == 0: one attempt only).
func TestParticipantLookup_Phase1_FindsParticipant(t *testing.T) {
	const identity LiveKitIdentity = "@alice:example.com"
	const room LiveKitRoomAlias = "phase1-room"

	phase1Done := make(chan struct{})
	original := LiveKitParticipantExists
	t.Cleanup(func() { LiveKitParticipantExists = original })
	LiveKitParticipantExists = func(_ context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, _ LiveKitIdentity) (bool, error) {
		select {
		case <-phase1Done:
		default:
			close(phase1Done)
		}
		return true, nil // participant present
	}

	mockExecOK(t)

	doneCh := make(chan *DelayedEventJob, 5)
	job, err := NewDelayedEventJob(
		context.Background(),
		DelayedEventJobParams{
			ServerName:      "example.com",
			DelayId:         "phase1-id",
			DelayTimeout:    10 * time.Second,
			LiveKitRoom:     room,
			LiveKitIdentity: identity,
		},
		lookUpCsApiUrlFromOverrideOnly("example.com", "https://matrix-client.example.com"),
		doneCh,
	)
	if err != nil {
		t.Fatalf("NewDelayedEventJob: %v", err)
	}
	go job.loop()
	// Joins loop() and the lookup goroutine on every exit path — including
	// t.Fatal — before the LIFO cleanup above restores the real
	// LiveKitParticipantExists, so no goroutine can race on the global mock.
	t.Cleanup(func() { _ = job.Close() })
	// sanityInterval == 0: one attempt only, no Phase 2.
	startParticipantLookup(job, LiveKitAuth{secret: "secret", key: "devkey", lkUrl: "ws://127.0.0.1:7880"}, 0)

	select {
	case <-phase1Done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Phase 1 ListParticipants call")
	}

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
	original := LiveKitParticipantExists
	t.Cleanup(func() { LiveKitParticipantExists = original })
	LiveKitParticipantExists = func(_ context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, _ LiveKitIdentity) (bool, error) {
		callCount++
		if callCount == 1 {
			return true, nil // Phase 1: participant present.
		}
		return false, nil // Phase 2: participant confirmed absent.
	}

	mockExecOK(t)

	const sanityInterval = 50 * time.Millisecond
	doneCh := make(chan *DelayedEventJob, 5)
	job, err := NewDelayedEventJob(
		context.Background(),
		DelayedEventJobParams{
			ServerName:      "example.com",
			DelayId:         "phase2-id",
			DelayTimeout:    10 * time.Second,
			LiveKitRoom:     room,
			LiveKitIdentity: identity,
		},
		lookUpCsApiUrlFromOverrideOnly("example.com", "https://matrix-client.example.com"),
		doneCh,
	)
	if err != nil {
		t.Fatalf("NewDelayedEventJob: %v", err)
	}
	go job.loop()
	// Joins all job goroutines on every exit path before the mock-restore
	// cleanup runs (LIFO) — see TestParticipantLookup_Phase1_FindsParticipant.
	t.Cleanup(func() { _ = job.Close() })
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
	original := LiveKitParticipantExists
	t.Cleanup(func() { LiveKitParticipantExists = original })
	LiveKitParticipantExists = func(_ context.Context, _ LiveKitAuth, _ LiveKitRoomAlias, _ LiveKitIdentity) (bool, error) {
		callCount++
		select {
		case <-phase1Done:
		default:
			close(phase1Done)
		}
		return true, nil // participant present
	}

	mockExecOK(t)

	doneCh := make(chan *DelayedEventJob, 5)
	job, err := NewDelayedEventJob(
		context.Background(),
		DelayedEventJobParams{
			ServerName:      "example.com",
			DelayId:         "disabled-id",
			DelayTimeout:    10 * time.Second,
			LiveKitRoom:     room,
			LiveKitIdentity: identity,
		},
		lookUpCsApiUrlFromOverrideOnly("example.com", "https://matrix-client.example.com"),
		doneCh,
	)
	if err != nil {
		t.Fatalf("NewDelayedEventJob: %v", err)
	}
	go job.loop()
	// Joins all job goroutines on every exit path before the mock-restore
	// cleanup runs (LIFO) — see TestParticipantLookup_Phase1_FindsParticipant.
	t.Cleanup(func() { _ = job.Close() })
	// sanityInterval == 0 disables Phase 2.
	startParticipantLookup(job, LiveKitAuth{secret: "secret", key: "devkey", lkUrl: "ws://127.0.0.1:7880"}, 0)

	// Wait for Phase 1 to complete.
	select {
	case <-phase1Done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Phase 1")
	}

	// Close() cancels the job and waits for backgroundWg, which includes the
	// lookup goroutine. After this, callCount is safe to read.
	if err := job.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected exactly 1 ListParticipants call (Phase 2 disabled), got %d", callCount)
	}
}

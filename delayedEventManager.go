// Copyright 2025 Element Creations Ltd.
// Copyright 2023 - 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v5"
)

var DelayedEventsEndpoint = "/_matrix/client/unstable/org.matrix.msc4140/delayed_events"

//var DelayedEventsEndpoint =  "/_matrix/client/v1/delayed_events"

type DelayEventAction string

const (
	ActionRestart DelayEventAction = "restart"
	ActionSend    DelayEventAction = "send"
)

// go:generate stringer -type=DelayEventState
type DelayEventState int

const (
	WaitingForInitialConnect DelayEventState = iota
	Connected
	Disconnected
	Completed
	Replaced
)

func (s DelayEventState) String() string {
	switch s {
	case WaitingForInitialConnect:
		return "WaitingForInitialConnect"
	case Connected:
		return "Connected"
	case Disconnected:
		return "Disconnected"
	case Completed:
		return "Completed"
	case Replaced:
		return "Replaced"
	default:
		return fmt.Sprintf("DelayEventState(%d)", int(s))
	}
}

// go:generate stringer -type=DelayedEventSignal
type DelayedEventSignal int

const (
	ParticipantConnected DelayedEventSignal = iota
	ParticipantLookupSuccessful
	ParticipantDisconnectedIntentionally
	ParticipantConnectionAborted
	DelayedEventReset
	DelayedEventTimedOut
	DelayedEventNotFound
	WaitingStateTimedOut
	SFUNotAvailable
)

func (s DelayedEventSignal) String() string {
	switch s {
	case ParticipantConnected:
		return "ParticipantConnected"
	case ParticipantLookupSuccessful:
		return "ParticipantLookupSuccessful"
	case ParticipantDisconnectedIntentionally:
		return "ParticipantDisconnectedIntentionally"
	case ParticipantConnectionAborted:
		return "ParticipantConnectionAborted"
	case DelayedEventReset:
		return "DelayedEventReset"
	case DelayedEventTimedOut:
		return "DelayedEventTimedOut"
	case DelayedEventNotFound:
		return "DelayedEventNotFound"
	case WaitingStateTimedOut:
		return "WaitingStateTimedOut"
	case SFUNotAvailable:
		return "SFUNotAvailable"
	default:
		return fmt.Sprintf("DelayedEventSignal(%d)", int(s))
	}
}

type LiveKitRoomAlias string
type LiveKitIdentity string

type SFUMessage struct {
	Type            DelayedEventSignal
	LiveKitIdentity LiveKitIdentity
}

// MonitorMessage is sent from DelayedEventJob to LiveKitRoomMonitor.
type MonitorMessage struct {
	LiveKitIdentity LiveKitIdentity
	State           DelayEventState
	Event           DelayedEventSignal
	JobId           UniqueID
}

// ── DelayedEventJob ──────────────────────────────────────────────────────────

// DelayedEventJob models the complete lifecycle of a MatrixRTC cancellable
// delayed disconnect event for a single participant in a LiveKit room.
//
// # Actor model
//
// A single goroutine — started via Loop() — is the sole owner of all mutable
// state (current FSM state, timers, …).  No mutex is needed for that state
// because nothing outside that goroutine touches it.
//
// External callers communicate with the job exclusively through its
// EventChannel (write-only from the outside).  Loop() reads from that channel
// and drives the FSM.
//
// # Lifecycle
//
//  1. Created by LiveKitRoomMonitor.HandoverJob().
//  2. Caller starts Loop() — typically in a goroutine: go job.Loop().
//  3. Events arrive via EventChannel (SFU webhooks, internal timers).
//  4. Loop() exits when ctx is cancelled (via Close() or parent cancellation).
//  5. Close() cancels the context and blocks until Loop() has returned.
type DelayedEventJob struct {
	// Immutable after construction — safe to read without a lock.
	JobId           UniqueID
	CsApiUrl        string
	DelayId         string
	DelayTimeout    time.Duration
	LiveKitRoom     LiveKitRoomAlias
	LiveKitIdentity LiveKitIdentity

	// EventChannel is the only way to send input to the job from the outside.
	// It is buffered so that senders are unlikely to block.
	EventChannel chan DelayedEventSignal

	ctx    context.Context
	cancel context.CancelFunc

	// monitorCh is the channel back to the owning LiveKitRoomMonitor.
	monitorCh chan<- MonitorMessage

	// done is closed by Loop() when it exits, allowing Close() to wait.
	done chan struct{}

	// resetWg tracks background ActionRestart goroutines started by
	// handleEventDelayedEventReset.  Loop() waits for them before returning
	// so that Close() guarantees no goroutine still holds a reference to
	// the ExecuteDelayedEventAction function variable after it returns.
	resetWg sync.WaitGroup

	// ── FSM state — owned exclusively by Loop() ──────────────────────────
	state                DelayEventState
	fsmTimerWaitingState *time.Timer
	fsmTimerDelayedEvent *delayedEventTimer
}

func NewDelayedEventJob(
	parentCtx context.Context,
	jobRequest *DelayedEventRequest,
	monitorCh chan<- MonitorMessage,
) (*DelayedEventJob, error) {
	if jobRequest.DelayTimeout <= 0 {
		return nil, fmt.Errorf("invalid delay timeout for delayed event job: %v", jobRequest.DelayTimeout)
	}

	ctx, cancel := context.WithCancel(parentCtx)
	job := &DelayedEventJob{
		JobId:           NewUniqueID(),
		CsApiUrl:        jobRequest.DelayCsApiUrl,
		DelayId:         jobRequest.DelayId,
		DelayTimeout:    jobRequest.DelayTimeout,
		LiveKitRoom:     jobRequest.LiveKitRoom,
		LiveKitIdentity: jobRequest.LiveKitIdentity,
		EventChannel:    make(chan DelayedEventSignal, 10),
		ctx:             ctx,
		cancel:          cancel,
		monitorCh:       monitorCh,
		done:            make(chan struct{}),
		state:           WaitingForInitialConnect,
	}
	return job, nil
}

// Loop is the single goroutine that owns all mutable job state.
// Start it exactly once: go job.Loop()
// It returns when the job's context is cancelled.
func (job *DelayedEventJob) Loop() {
	defer close(job.done)

	// waitingDuration is bounded to at most one hour (sticky-event timeout).
	waitingDuration := min(time.Hour, job.DelayTimeout)
	job.fsmTimerWaitingState = time.AfterFunc(waitingDuration, func() {
		slog.Debug("Job: FSM WaitingState -> WaitingStateTimedOut",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity)
		// Non-blocking send: if the job is already shutting down the channel
		// will be drained and the timer event is simply discarded.
		select {
		case job.EventChannel <- WaitingStateTimedOut:
		case <-job.ctx.Done():
		}
	})

	for {
		select {
		case <-job.ctx.Done():
			job.stopTimers()
			// Wait for all ActionRestart goroutines to finish.  They all use
			// job.ctx which is now cancelled, so they will exit promptly.
			job.resetWg.Wait()
			slog.Debug("Job: Loop exiting", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity)
			return

		case event, ok := <-job.EventChannel:
			if !ok {
				return
			}
			slog.Debug("Job: dispatching event", "event", event,
				"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity)
			if job.handleEvent(event) {
				job.handleStateEntryAction(event)
			}
		}
	}
}

// Cancel cancels the job context without waiting for Loop() to exit.
// Use this to unblock any goroutines that are sending on channels
// before calling Close() in a separate goroutine.
func (job *DelayedEventJob) Cancel() {
	job.cancel()
}

// Close cancels the job context and waits until Loop() has exited.
// It is safe to call from any goroutine.
func (job *DelayedEventJob) Close() error {
	job.cancel()
	select {
	case <-job.done:
	case <-time.After(10 * time.Second):
		slog.Warn("Job: Close() timed out waiting for Loop() to exit",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "jobId", job.JobId)
	}
	slog.Debug("Job: closed", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity)
	return nil
}

func (job *DelayedEventJob) String() string {
	return fmt.Sprintf(
		"DelayedEventJob{CSAPI: %s, DelayId: %s, DelayTimeout: %s, LiveKitRoom: %s, LiveKitIdentity: %s, State: %s}",
		job.CsApiUrl, job.DelayId, job.DelayTimeout,
		job.LiveKitRoom, job.LiveKitIdentity, job.state,
	)
}

// delayRestartDuration returns 80 % of the original timeout.
// Called only from Loop().
func (job *DelayedEventJob) delayRestartDuration() time.Duration {
	return job.DelayTimeout * 8 / 10
}

// stopTimers stops both internal timers.
// MUST only be called from Loop().
func (job *DelayedEventJob) stopTimers() {
	if job.fsmTimerWaitingState != nil {
		job.fsmTimerWaitingState.Stop()
	}
	if job.fsmTimerDelayedEvent != nil {
		job.fsmTimerDelayedEvent.stop()
	}
}

// ── FSM ──────────────────────────────────────────────────────────────────────
// All methods below are called exclusively from Loop() and therefore need no
// additional synchronisation.

func (job *DelayedEventJob) handleEvent(event DelayedEventSignal) (stateChanged bool) {
	slog.Debug("Job: FSM event", "event", event,
		"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
		"delayId", job.DelayId, "jobId", job.JobId)

	switch event {
	case ParticipantConnected:
		return job.handleEventParticipantConnected()
	case ParticipantLookupSuccessful:
		return job.handleEventParticipantLookupSuccessful()
	case ParticipantDisconnectedIntentionally:
		return job.handleEventParticipantDisconnected()
	case ParticipantConnectionAborted:
		return job.handleEventParticipantConnectionAborted()
	case DelayedEventReset:
		return job.handleEventDelayedEventReset()
	case DelayedEventTimedOut:
		return job.handleEventDelayedEventTimedOut()
	case DelayedEventNotFound:
		return job.handleEventDelayedEventNotFound()
	case WaitingStateTimedOut:
		return job.handleEventWaitingStateTimedOut()
	case SFUNotAvailable:
		// noop
	default:
		slog.Error("Job: FSM unknown event",
			"event", event, "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity)
	}
	return false
}

func (job *DelayedEventJob) handleStateEntryAction(event DelayedEventSignal) {
	switch job.state {
	case WaitingForInitialConnect:
		// nothing to do on entry

	case Connected:
		// No longer need the waiting-state guard timer.
		if job.fsmTimerWaitingState != nil {
			job.fsmTimerWaitingState.Stop()
		}

		// Start the delayed-event reset timer.
		// The timer fires at 80 % of the original timeout so we have headroom
		// to call Restart on the homeserver before the event is emitted.
		restartDur := job.delayRestartDuration()
		job.fsmTimerDelayedEvent = newDelayedEventTimer(
			job.DelayTimeout,
			func() {
				select {
				case job.EventChannel <- DelayedEventReset:
				case <-job.ctx.Done():
				}
			},
		)
		_ = restartDur // restartDur is used inside newDelayedEventTimer via closure — kept for clarity

		// Immediately trigger a reset to sync the timer with the homeserver
		// (we don't know how long we took between token creation and now).
		job.fsmTimerDelayedEvent.stop()
		select {
		case job.EventChannel <- DelayedEventReset:
		case <-job.ctx.Done():
		}

	case Disconnected:
		remaining := time.Duration(0)
		if job.fsmTimerDelayedEvent != nil {
			remaining = job.fsmTimerDelayedEvent.timeRemaining()
		}
		job.stopTimers()
		snapshotRemaining := remaining
		snapshotEvent := event

		// Notify the monitor immediately so it can proceed with teardown
		// without waiting for the ActionSend HTTP call to complete.
		// ActionSend runs concurrently and is a best-effort operation.
		//
		// Sending first also breaks the circular dependency:
		//   monitor.Loop() receives → job.Cancel() → ActionSend goroutine exits
		// If we sent after ActionSend, the monitor could never cancel the job
		// context to unblock the goroutine. Deadlock.
		select {
		case job.monitorCh <- MonitorMessage{
			LiveKitIdentity: job.LiveKitIdentity,
			State:           Disconnected,
			Event:           snapshotEvent,
			JobId:           job.JobId,
		}:
		case <-job.ctx.Done():
			// Context cancelled — monitor will clean up via teardown path.
			return
		}

		// ActionSend is best-effort: fire and forget, bounded by job.ctx.
		// The monitor has already been notified above, so this goroutine
		// completing (or not) does not affect the job lifecycle.
		job.resetWg.Add(1)
		go func() {
			defer job.resetWg.Done()

			expBackOff := backoff.NewExponentialBackOff()
			expBackOff.InitialInterval = 1000 * time.Millisecond
			expBackOff.Multiplier = 1.5
			expBackOff.RandomizationFactor = 0.5
			expBackOff.MaxInterval = 60 * time.Second

			resp, err := backoff.Retry(
				job.ctx,
				func() (*http.Response, error) {
					return ExecuteDelayedEventAction(job.CsApiUrl, job.DelayId, ActionSend)
				},
				backoff.WithBackOff(expBackOff),
				backoff.WithMaxElapsedTime(snapshotRemaining),
			)
			if err != nil {
				slog.Warn("Job: ActionSend failed",
					"state", Disconnected, "event", snapshotEvent,
					"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "jobId", job.JobId,
					"err", err)
			} else if resp == nil || resp.StatusCode < 200 || (resp.StatusCode >= 300 && resp.StatusCode != 404) {
				slog.Warn("Job: ActionSend unexpected status",
					"state", Disconnected, "event", snapshotEvent,
					"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "jobId", job.JobId)
			} else if resp != nil && resp.StatusCode == 404 {
				slog.Info("Job: ActionSend — delayed event already sent or cancelled",
					"state", Disconnected, "event", snapshotEvent,
					"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "jobId", job.JobId)
			}
		}()

	case Completed:
		job.stopTimers()
		// Notify monitor immediately (same pattern as Disconnected).
		select {
		case job.monitorCh <- MonitorMessage{
			LiveKitIdentity: job.LiveKitIdentity,
			State:           Completed,
			Event:           event,
			JobId:           job.JobId,
		}:
		case <-job.ctx.Done():
		}
	}
}

func (job *DelayedEventJob) handleEventParticipantConnected() bool {
	if job.state == WaitingForInitialConnect {
		job.state = Connected
		slog.Info("Job: → Connected (ParticipantConnected)",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		return true
	}
	return false
}

func (job *DelayedEventJob) handleEventParticipantLookupSuccessful() bool {
	if job.state == WaitingForInitialConnect {
		job.state = Connected
		slog.Info("Job: → Connected (ParticipantLookupSuccessful)",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		return true
	}
	return false
}

func (job *DelayedEventJob) handleEventParticipantDisconnected() bool {
	if job.state == Connected {
		job.state = Disconnected
		slog.Info("Job: → Disconnected (ParticipantDisconnected)",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		return true
	}
	return false
}

func (job *DelayedEventJob) handleEventParticipantConnectionAborted() bool {
	if job.state == WaitingForInitialConnect {
		job.state = Completed
		slog.Info("Job: → Completed (ParticipantConnectionAborted)",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		return true
	}
	return false
}

func (job *DelayedEventJob) handleEventDelayedEventTimedOut() bool {
	if job.state == WaitingForInitialConnect || job.state == Connected {
		job.state = Disconnected
		slog.Info("Job: → Disconnected (DelayedEventTimedOut)",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		return true
	}
	return false
}

func (job *DelayedEventJob) handleEventDelayedEventNotFound() bool {
	if job.state == WaitingForInitialConnect || job.state == Connected {
		job.state = Disconnected
		slog.Info("Job: → Disconnected (DelayedEventNotFound)",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		return true
	}
	return false
}

func (job *DelayedEventJob) handleEventDelayedEventReset() bool {
	if (job.state != Connected && job.state != WaitingForInitialConnect) ||
		job.fsmTimerDelayedEvent == nil {
		return false
	}

	remaining := job.fsmTimerDelayedEvent.timeRemaining()
	if remaining <= 0 {
		job.state = Disconnected
		slog.Info("Job: → Disconnected (DelayedEventReset, remaining=0)",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		return true
	}

	// Issue the restart call in a background goroutine so Loop() stays
	// responsive.  The goroutine must not touch any job state directly — it
	// only sends back a signal on EventChannel.
	timer := job.fsmTimerDelayedEvent
	timeout := job.DelayTimeout
	nextReset := job.delayRestartDuration()
	lkRm := job.LiveKitRoom
	lkId := job.LiveKitIdentity
	ch := job.EventChannel
	ctx := job.ctx

	job.resetWg.Add(1)
	go func() {
		defer job.resetWg.Done()
		expBackOff := backoff.NewExponentialBackOff()
		expBackOff.InitialInterval = 1000 * time.Millisecond
		expBackOff.Multiplier = 1.5
		expBackOff.RandomizationFactor = 0.5
		expBackOff.MaxInterval = 60 * time.Second

		resp, err := backoff.Retry(
			ctx,
			func() (*http.Response, error) {
				return ExecuteDelayedEventAction(job.CsApiUrl, job.DelayId, ActionRestart)
			},
			backoff.WithBackOff(expBackOff),
			backoff.WithMaxElapsedTime(remaining),
		)

		var signal DelayedEventSignal
		switch {
		case err != nil:
			slog.Warn("Job: ActionRestart failed — emitting DelayedEventTimedOut",
				"room", lkRm, "lkId", lkId, "jobId", job.JobId, "err", err)
			signal = DelayedEventTimedOut
		case resp == nil || resp.StatusCode == 404:
			slog.Warn("Job: ActionRestart not found — emitting DelayedEventNotFound",
				"room", lkRm, "lkId", lkId, "jobId", job.JobId)
			signal = DelayedEventNotFound
		case resp.StatusCode < 200 || resp.StatusCode >= 300:
			slog.Warn("Job: ActionRestart bad status — emitting DelayedEventTimedOut",
				"room", lkRm, "lkId", lkId, "jobId", job.JobId)
			signal = DelayedEventTimedOut
		default:
			// Only reschedule if the job context is still active.  If ctx is
			// already done the timer callback would try to send on EventChannel
			// which Loop() is no longer reading, causing resetWg.Done() to
			// never be called and resetWg.Wait() to block forever.
			select {
			case <-ctx.Done():
				// Job is shutting down — do not re-arm the timer.
				return
			default:
			}
			timer.reset(nextReset, timeout)
			slog.Debug(fmt.Sprintf("Job: ActionRestart ok, next reset in %s", nextReset),
				"room", lkRm, "lkId", lkId)
			return
		}

		select {
		case ch <- signal:
		case <-ctx.Done():
		}
	}()

	return false
}

func (job *DelayedEventJob) handleEventWaitingStateTimedOut() bool {
	if job.state == WaitingForInitialConnect {
		job.state = Disconnected
		slog.Info("Job: → Disconnected (WaitingStateTimedOut)",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		return true
	}
	return false
}

// ── delayedEventTimer ────────────────────────────────────────────────────────

// delayedEventTimer is a thin wrapper around time.Timer that also tracks the
// absolute deadline so that callers can query the remaining time.
//
// Thread-safe: reset() may be called from a background goroutine while
// timeRemaining() is called from Loop().
type delayedEventTimer struct {
	mu       sync.Mutex
	timer    *time.Timer
	deadline time.Time
}

func newDelayedEventTimer(timeoutDuration time.Duration, f func()) *delayedEventTimer {
	dt := &delayedEventTimer{
		deadline: time.Now().Add(timeoutDuration),
	}
	dt.timer = time.AfterFunc(timeoutDuration, f)
	return dt
}

func (dt *delayedEventTimer) reset(restartDuration, timeoutDuration time.Duration) bool {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	if dt.timer == nil {
		return false
	}
	dt.deadline = time.Now().Add(timeoutDuration)
	dt.timer.Reset(restartDuration)
	return true
}

func (dt *delayedEventTimer) stop() bool {
	if dt.timer != nil {
		return dt.timer.Stop()
	}
	return false
}

func (dt *delayedEventTimer) timeRemaining() time.Duration {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	r := time.Until(dt.deadline)
	if r < 0 {
		return 0
	}
	return r
}

// ── LiveKitRoomMonitor ───────────────────────────────────────────────────────

// LiveKitRoomMonitor manages DelayedEventJobs for a specific LiveKit room.
//
// # Actor model
//
// Like DelayedEventJob, the monitor owns all its mutable state in a single
// goroutine started via Loop().  The only way to communicate with it from
// the outside is through SFUCommChan (SFU webhook events) and the
// HandlerCommChan it was given at construction time.
//
// # Job handover / replacement
//
// See HandoverJob() for the replacement strategy.
//
// # Shutdown
//
// Call Close() to cancel the context.  Loop() will drain pending jobs and
// return.  Close() blocks until Loop() has exited (with a timeout guard).
type LiveKitRoomMonitor struct {
	// Immutable after construction.
	MonitorId       UniqueID
	RoomAlias       LiveKitRoomAlias
	SFUCommChan     chan SFUMessage
	handoverCh      chan handoverRequest

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// handlerCommChan sends events to the owning Handler.
	handlerCommChan chan HandlerMessage
	lkAuth          *LiveKitAuth
}

// handoverRequest is the message sent through handoverCh to add a new job
// atomically inside the monitor's Loop().
type handoverRequest struct {
	jobRequest *DelayedEventRequest
	result     chan handoverResult
}

type handoverResult struct {
	jobId UniqueID
	ok    bool
}

func NewLiveKitRoomMonitor(
	parentCtx context.Context,
	handlerCommChan chan HandlerMessage,
	lkAuth *LiveKitAuth,
	roomAlias LiveKitRoomAlias,
) *LiveKitRoomMonitor {
	ctx, cancel := context.WithCancel(parentCtx)
	m := &LiveKitRoomMonitor{
		MonitorId:       NewUniqueID(),
		RoomAlias:       roomAlias,
		SFUCommChan:     make(chan SFUMessage, 100),
		handoverCh:      make(chan handoverRequest),
		ctx:             ctx,
		cancel:          cancel,
		done:            make(chan struct{}),
		handlerCommChan: handlerCommChan,
		lkAuth:          lkAuth,
	}
	slog.Info("LiveKitRoomMonitor: created", "room", roomAlias, "MonitorId", m.MonitorId)
	return m
}

// Loop is the single goroutine that owns all monitor state.
// Start it exactly once: go monitor.Loop()
func (m *LiveKitRoomMonitor) Loop() {
	defer close(m.done)

	// All mutable state lives here — no mutex needed.
	jobs := make(map[LiveKitIdentity]*DelayedEventJob)
	// upcomingJobs counts handover requests that are in flight (i.e. the
	// caller has called HandoverJob but the job goroutine has not yet been
	// registered in jobs).  We track this so we don't tear down while a
	// handover is still in progress.
	upcomingJobs := 0
	jobCommCh := make(chan MonitorMessage, 20)

	// lookupWg tracks all participant-lookup goroutines.  We must wait for
	// them before Loop() returns so that any goroutine still holding a
	// reference to the LiveKitParticipantLookup function variable has exited
	// before test cleanup (or production teardown) replaces/restores it.
	var lookupWg sync.WaitGroup

	// teardown cancels all running jobs, drains pending messages, then
	// waits for all background goroutines (lookups, Close calls) to exit.
	teardown := func() {
		// Cancel all job contexts first so their goroutines stop sending
		// on jobCommCh.  This prevents Close() from deadlocking while we
		// are no longer draining jobCommCh.
		for _, job := range jobs {
			job.Cancel()
		}
		// Drain messages that arrived before cancellation.
		for len(jobCommCh) > 0 {
			<-jobCommCh
		}
		// Now close all jobs (waits for resetWg inside each job).
		for _, job := range jobs {
			if err := job.Close(); err != nil {
				slog.Error("RoomMonitor: error closing job on teardown",
					"room", m.RoomAlias, "lkId", job.LiveKitIdentity, "jobId", job.JobId)
			}
		}
		// Block until every lookup goroutine and async Close goroutine exits.
		lookupWg.Wait()
	}

	// maybeInitiateTeardown sends NoJobsLeft to the handler and stops the loop
	// when there is nothing left to do.
	maybeInitiateTeardown := func() {
		if len(jobs) == 0 && upcomingJobs == 0 {
			slog.Debug("RoomMonitor: no jobs left, notifying handler",
				"room", m.RoomAlias, "MonitorId", m.MonitorId)
			// Send synchronously — we're already in the loop goroutine and the
			// handler's channel is unbuffered-tolerant (it runs its own loop).
			m.handlerCommChan <- HandlerMessage{
				RoomAlias: m.RoomAlias,
				Event:     NoJobsLeft,
				MonitorId: m.MonitorId,
			}
			// Cancel ourselves; next iteration hits ctx.Done.
			m.cancel()
		}
	}

	for {
		select {
		case <-m.ctx.Done():
			teardown()
			slog.Debug("RoomMonitor: Loop exiting", "room", m.RoomAlias)
			return

		// ── SFU webhook events ────────────────────────────────────────────
		case event := <-m.SFUCommChan:
			job, ok := jobs[event.LiveKitIdentity]
			if !ok {
				slog.Debug("RoomMonitor: SFU event for unknown identity — ignoring",
					"room", m.RoomAlias, "lkId", event.LiveKitIdentity, "type", event.Type)
				continue
			}
			switch event.Type {
			case ParticipantConnected:
				job.EventChannel <- ParticipantConnected
			case ParticipantLookupSuccessful:
				job.EventChannel <- ParticipantLookupSuccessful
			case ParticipantDisconnectedIntentionally:
				job.EventChannel <- ParticipantDisconnectedIntentionally
			case ParticipantConnectionAborted:
				job.EventChannel <- ParticipantConnectionAborted
			default:
				slog.Warn("RoomMonitor: unhandled SFU event type",
					"room", m.RoomAlias, "lkId", event.LiveKitIdentity, "type", event.Type)
			}

		// ── Messages from jobs ────────────────────────────────────────────
		case msg := <-jobCommCh:
			job, ok := jobs[msg.LiveKitIdentity]
			if !ok {
				slog.Warn("RoomMonitor: MonitorMessage for unknown identity",
					"room", m.RoomAlias, "lkId", msg.LiveKitIdentity)
				continue
			}
			if job.JobId != msg.JobId {
				slog.Warn("RoomMonitor: MonitorMessage JobId mismatch — ignoring",
					"room", m.RoomAlias,
					"lkId", msg.LiveKitIdentity,
					"expectedJobId", job.JobId,
					"gotJobId", msg.JobId)
				continue
			}
			switch msg.State {
			case Disconnected, Completed:
				slog.Debug("RoomMonitor: removing job",
					"room", m.RoomAlias, "lkId", msg.LiveKitIdentity, "state", msg.State)
				// Cancel the job context immediately so any goroutines that are
				// trying to send on jobCommCh (= monitorCh) will take the
				// ctx.Done() branch and exit — preventing a deadlock where
				// job.Close() blocks waiting for those goroutines while we are
				// no longer draining jobCommCh.
				job.Cancel()
				// Wait for full cleanup in a separate goroutine so Loop() keeps
				// draining all channels (including jobCommCh) while we wait.
				lookupWg.Add(1)
				go func(j *DelayedEventJob) {
					defer lookupWg.Done()
					if err := j.Close(); err != nil {
						slog.Error("RoomMonitor: error closing job",
							"room", m.RoomAlias, "lkId", j.LiveKitIdentity)
					}
				}(job)
				delete(jobs, msg.LiveKitIdentity)
				maybeInitiateTeardown()
			default:
				slog.Warn("RoomMonitor: unhandled MonitorMessage state",
					"room", m.RoomAlias, "lkId", msg.LiveKitIdentity,
					"JobId", msg.JobId, "state", msg.State)
			}

		// ── New job handover ──────────────────────────────────────────────
		case req := <-m.handoverCh:
			// Count this request as in-flight so maybeInitiateTeardown() does
			// not fire before we have registered the job in the jobs map.
			upcomingJobs++
			job, err := NewDelayedEventJob(m.ctx, req.jobRequest, jobCommCh)
			if err != nil {
				slog.Error("RoomMonitor: failed to create job",
					"room", m.RoomAlias,
					"lkId", req.jobRequest.LiveKitIdentity,
					"err", err)
				req.result <- handoverResult{}
				upcomingJobs--
				maybeInitiateTeardown()
				continue
			}

			// Replace an existing job for the same identity if present.
			if existing, ok := jobs[job.LiveKitIdentity]; ok {
				if job.JobId <= existing.JobId {
					slog.Error("RoomMonitor: new JobId not greater than existing",
						"room", m.RoomAlias,
						"existingJobId", existing.JobId,
						"newJobId", job.JobId)
					panic("invariant violated: JobId must be monotonically increasing")
				}
				slog.Warn("RoomMonitor: replacing existing job",
					"room", m.RoomAlias, "lkId", existing.LiveKitIdentity,
					"existingJobId", existing.JobId, "newJobId", job.JobId)
				existing.setState(Replaced)
				// Cancel immediately so any goroutines sending on jobCommCh exit,
				// then close in background tracked by lookupWg so teardown() waits.
				existing.Cancel()
				lookupWg.Add(1)
				go func(j *DelayedEventJob) {
					defer lookupWg.Done()
					if err := j.Close(); err != nil {
						slog.Error("RoomMonitor: error closing replaced job",
							"room", m.RoomAlias, "lkId", j.LiveKitIdentity, "jobId", j.JobId)
					}
				}(existing)
			}

			jobs[job.LiveKitIdentity] = job
			go job.Loop()

			// Start participant lookup with exponential backoff in a separate
			// goroutine.  It sends its result as a SFUMessage so it goes
			// through the normal event path.
			// lookupWg tracks this goroutine so teardown() can wait for it to
			// exit before Loop() returns — preventing races on the global
			// LiveKitParticipantLookup variable in tests.
			waitingDuration := min(time.Hour, job.DelayTimeout)
			lookupWg.Add(1)
			go func(j *DelayedEventJob) {
				defer lookupWg.Done()
				expBackOff := backoff.NewExponentialBackOff()
				expBackOff.InitialInterval = 1000 * time.Millisecond
				expBackOff.Multiplier = 1.5
				expBackOff.RandomizationFactor = 0.5
				expBackOff.MaxInterval = 60 * time.Second

				_, err := backoff.Retry(
					j.ctx,
					func() (bool, error) {
						return LiveKitParticipantLookup(j.ctx, *m.lkAuth, j.LiveKitRoom, j.LiveKitIdentity, m.SFUCommChan)
					},
					backoff.WithBackOff(expBackOff),
					backoff.WithMaxElapsedTime(waitingDuration),
				)
				if err != nil {
					slog.Warn("RoomMonitor: participant lookup failed",
						"room", j.LiveKitRoom, "lkId", j.LiveKitIdentity,
						"jobId", j.JobId, "err", err)
				}
			}(job)

			upcomingJobs--
			req.result <- handoverResult{jobId: job.JobId, ok: true}
			slog.Info("RoomMonitor: job added",
				"room", m.RoomAlias, "lkId", job.LiveKitIdentity,
				"delayId", job.DelayId, "jobId", job.JobId)
		}
	}
}

// Close cancels the monitor and blocks until Loop() has exited.
func (m *LiveKitRoomMonitor) Close() error {
	m.cancel()
	select {
	case <-m.done:
	case <-time.After(15 * time.Second):
		slog.Warn("RoomMonitor: Close() timed out",
			"room", m.RoomAlias, "MonitorId", m.MonitorId)
	}
	slog.Debug("RoomMonitor: closed", "room", m.RoomAlias, "MonitorId", m.MonitorId)
	return nil
}

// HandoverJob sends a new job request to Loop() and blocks until Loop() has
// registered it.  This replaces the old StartJobHandover/HandoverJob pair.
//
// Returns the new job's ID and whether the handover succeeded.
func (m *LiveKitRoomMonitor) HandoverJob(jobRequest *DelayedEventRequest) (UniqueID, bool) {
	result := make(chan handoverResult, 1)
	select {
	case m.handoverCh <- handoverRequest{jobRequest: jobRequest, result: result}:
	case <-m.ctx.Done():
		slog.Warn("RoomMonitor: HandoverJob called on closed monitor",
			"room", m.RoomAlias)
		return UniqueID(""), false
	}
	res := <-result
	return res.jobId, res.ok
}

// setState is called from outside Loop() only in the "replacement" path where
// we want to mark the old job before closing it.  Because this runs in a
// goroutine spawned by Loop() (which already moved the job out of the jobs
// map), it's safe — no other goroutine will touch the job's state anymore.
func (job *DelayedEventJob) setState(state DelayEventState) {
	// This is the one case where we write state outside of Loop().
	// It is safe because it is called only after the job has been removed from
	// the jobs map and Loop() will never see it again.
	job.state = state
	slog.Debug("Job: state forced",
		"newState", state, "room", job.LiveKitRoom,
		"lkId", job.LiveKitIdentity, "jobId", job.JobId)
}

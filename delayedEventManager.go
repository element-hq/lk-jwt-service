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
	"sync"
	"time"

	"github.com/cenkalti/backoff/v5"
)

// DelayedEventsEndpoint is the Matrix CS-API path for delayed events.
//
// TODO(msc4140): switch to the stable path once MSC-4140 is merged into
// the Matrix spec:
//
//	/_matrix/client/v1/delayed_events
//
// See https://github.com/matrix-org/matrix-spec-proposals/pull/4140
var DelayedEventsEndpoint = "/_matrix/client/unstable/org.matrix.msc4140/delayed_events"

type DelayEventAction string

const (
	ActionRestart DelayEventAction = "restart"
	ActionSend    DelayEventAction = "send"
)

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
	JobReplaced
	// SFUParticipantGone is emitted by the sanity-check phase of the
	// participant-lookup goroutine when a Connected participant can no longer
	// be found on the SFU — indicating a missed disconnect webhook.
	SFUParticipantGone
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
	case JobReplaced:
		return "JobReplaced"
	case SFUParticipantGone:
		return "SFUParticipantGone"
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

// jobKey is the map key used by Handler.loop() to track active delayed-event jobs.
// LiveKit identity encodes the Matrix user ID (which includes the homeserver domain),
// so (room, identity) is unique — no need for CsApiUrl in the key.
type jobKey struct {
	Room     LiveKitRoomAlias
	Identity LiveKitIdentity
}

// startParticipantLookup spawns a goroutine (tracked by job.backgroundWg) that
// performs Phase-1 and Phase-2 participant checks for the given job.
//
//   - Phase 1: calls LiveKitParticipantExists with exponential backoff until
//     the identity is present or job.DelayTimeout elapses.  On success sends
//     ParticipantLookupSuccessful to job.EventChannel.
//   - Phase 2: at every sanityInterval tick, calls LiveKitParticipantExists;
//     if the identity is confirmed absent (SFU returned NotFound), sends
//     SFUParticipantGone.  Transport errors are logged and ignored.
//     Disabled when sanityInterval == 0.
//
// Communication is identical to SFU webhooks — signals are sent on
// job.EventChannel — so no new concurrency primitives are needed.
func startParticipantLookup(job *DelayedEventJob, lkAuth LiveKitAuth, sanityInterval time.Duration) {
	lkRoom := job.LiveKitRoom
	lkId := job.LiveKitIdentity

	job.backgroundWg.Add(1)
	go func() {
		defer job.backgroundWg.Done()
		ctx := job.ctx

		// Phase 1: retry with exponential backoff until the participant appears.

		// waitingDuration is bounded to at most one hour (sticky-event timeout).
		waitingDuration := min(time.Hour, job.DelayTimeout)

		expBackOff := backoff.NewExponentialBackOff()
		expBackOff.InitialInterval = 1 * time.Second
		expBackOff.Multiplier = 1.5
		expBackOff.RandomizationFactor = 0.5
		expBackOff.MaxInterval = 60 * time.Second
		_, err := backoff.Retry(
			ctx,
			func() (struct{}, error) {
				ok, err := LiveKitParticipantExists(ctx, lkAuth, lkRoom, lkId)
				switch {
				case err != nil:
					return struct{}{}, err // transport error — retry
				case !ok:
					return struct{}{}, errParticipantAbsent // confirmed absent — retry
				default:
					return struct{}{}, nil // present — success
				}
			},
			backoff.WithBackOff(expBackOff),
			backoff.WithMaxElapsedTime(waitingDuration),
		)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("participantLookup: Phase 1 failed", "room", lkRoom, "lkId", lkId, "err", err)
			}
			return
		}
		slog.Debug("participantLookup: Phase 1 succeeded", "room", lkRoom, "lkId", lkId)
		select {
		case job.EventChannel <- ParticipantLookupSuccessful:
		case <-ctx.Done():
			return
		}

		// Phase 2: periodic sanity checks (disabled when sanityInterval == 0).
		//
		// Only emit SFUParticipantGone when the SFU confirms absence
		// ((false, nil) from LiveKitParticipantExists).  Transport errors
		// are logged and ignored — a transient blip should not tear the
		// job down prematurely.
		if sanityInterval == 0 {
			return
		}
		ticker := time.NewTicker(sanityInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ok, err := LiveKitParticipantExists(ctx, lkAuth, lkRoom, lkId)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					slog.Warn("participantLookup: Phase 2: transport error (ignored)",
						"room", lkRoom, "lkId", lkId, "err", err)
					continue
				}
				if !ok {
					slog.Warn("participantLookup: Phase 2: participant no longer on SFU",
						"room", lkRoom, "lkId", lkId)
					select {
					case job.EventChannel <- SFUParticipantGone:
					case <-ctx.Done():
					}
					return
				}
			}
		}
	}()
}

// ── DelayedEventJob ──────────────────────────────────────────────────────────

// DelayedEventJobParams is the immutable bundle of inputs needed to construct
// a DelayedEventJob.  Built by request handlers in main.go and handed to
// Handler.addDelayedEventJob, which forwards it to Handler.loop() and
// NewDelayedEventJob.  Embedded into DelayedEventJob so its fields are
// promoted (job.LiveKitRoom etc. resolve here without going through .Params).
type DelayedEventJobParams struct {
	DelayId         string
	CsApiUrl        string
	DelayTimeout    time.Duration
	LiveKitRoom     LiveKitRoomAlias
	LiveKitIdentity LiveKitIdentity
}

// DelayedEventJob models the complete lifecycle of a MatrixRTC cancellable
// delayed disconnect event for a single participant in a LiveKit room.
//
// # Actor model
//
// A single goroutine — started via loop() — is the sole owner of all mutable
// state (current FSM state, timers, …).  No mutex is needed for that state
// because nothing outside that goroutine touches it.
//
// External callers communicate with the job exclusively through its
// EventChannel (write-only from the outside).  loop() reads from that channel
// and drives the FSM.
//
// # Lifecycle
//
//  1. Created by Handler.loop() when a new delayed-event request arrives.
//  2. Caller starts loop() — typically in a goroutine: go job.loop().
//  3. Events arrive via EventChannel (SFU webhooks, internal FSM timers).
//  4. loop() exits when ctx is cancelled (via Close() or parent cancellation).
//  5. Close() cancels the context and blocks until loop() has returned.
//
// # Finite State Machine (FSM) — Main Job Lifecycle
//
//	                        ParticipantConnected,            DelayedEventReset /
//	   (Start)              ParticipantLookupSuccessful    (Execute ActionRestart)
//	      |                ┌───────────────────────────┐          ┌──────┐
//	      ▼                |                           ▼          |      |
//	 ┌──────────────────────────┐        ┌───────────────────────────┐   |
//	 │ WaitingForInitialConnect │        │ Connected                 │◄──┘
//	 │                          │        │                           │
//	 │ On Entry: (none)         │        │ On Entry:                 │
//	 └──────────────────────────┘        │ • Stop waiting timer      │
//	     |                 |             │ • Setup delayed timer     │
//	     |                 |             │ • Emit: DelayedEventReset │
//	     |                 |             └───────────────────────────┘
//	     |                 |                           │
//	     |                 | DelayedEventTimedOut,     │ DelayedEventTimedOut,
//	     |                 | DelayedEventNotFound,     │ DelayedEventNotFound,
//	     |                 | WaitingStateTimedOut      │ ParticipantDisconnectedIntentionally,
//	     |                 |                           │ ParticipantConnectionAborted,
//	     |                 └───────────────────────────│ SFUParticipantGone
//	     |                                             │
//	     | ParticipantConnectionAborted                │
//	     ▼                                             ▼
//	 ┌──────────────────────┐              ┌──────────────────────┐
//	 │ Completed            │              │ Disconnected         │
//	 │                      |              │                      │
//	 │ On Entry:            │              │ On Entry:            │
//	 │ • Stop FSM timers    │              │ • Stop timers        │
//	 │ • Notify handler     │              │ • Execute ActionSend │
//	 └──────────────────────┘              │ • Notify handler     │
//	                                       └──────────────────────┘
//
//	     (from any state)
//	            |
//	            │ JobReplaced
//	            ▼
//	 ┌──────────────────────┐
//	 │ Replaced             │
//	 │                      │
//	 │ On Entry: (none)     │
//	 └──────────────────────┘
//
// # FSM — Waiting-State Timer
//
//	┌──────────────────────────────┐               ┌────────────────────────┐
//	│ Active                       │               │ Fired                  │
//	│                              │ Timer elapses │                        │
//	│ On Entry:                    │──────────────►│ On Entry:              │
//	│ • Start timer                │               │ • Emit:                |
//	│   (≤ 1 hour or DelayTimeout) │               |   WaitingStateTimedOut |
//	└──────────────────────────────┘               └────────────────────────┘
//
// # FSM — Delayed-Event Restart Timer
//
//	┌──────────────────────────────┐
//	│ Created                      │
//	│                              │
//	│ On Entry:                    │
//	│ • Create AfterFunc timer     │
//	│   (timeoutDuration)          │
//	└──────────────────────────────┘
//	               |
//	               ▼
//	┌──────────────────────────────┐               ┌───────────────────────────┐
//	│ Active                       │               │ Fired                     │
//	│                              │ Timer elapses │                           │
//	│ On Entry: (none)             │──────────────►│ On Entry:                 │
//	│                              │               │ • Emit: DelayedEventReset |
//	└──────────────────────────────┘               └───────────────────────────┘
//	               ▲
//	               │ Restart / (restart timer)
//	               |
//	   (from Active / Fired state)
type DelayedEventJob struct {
	// Immutable after construction — safe to read without a lock.  The
	// embedded DelayedEventJobParams promotes DelayId, CsApiUrl, DelayTimeout,
	// LiveKitRoom and LiveKitIdentity onto DelayedEventJob, so callers can
	// keep using job.LiveKitRoom etc. without reaching through .Params.
	JobId UniqueID
	DelayedEventJobParams

	// EventChannel is the only way to send input to the job from the outside.
	// It is buffered so that senders are unlikely to block.
	EventChannel chan DelayedEventSignal

	ctx    context.Context
	cancel context.CancelFunc

	// doneCh signals Handler.loop() when the job enters a terminal state
	// (Disconnected after ActionSend completes, or Completed).  The job sends
	// a pointer to itself so the Handler can verify it is still the active job
	// for this (room, identity) before cancelling and cleaning it up.
	doneCh chan<- *DelayedEventJob

	// done is closed by loop() when it exits, allowing Close() to wait.
	done chan struct{}

	// backgroundWg tracks all background goroutines started by or for this job:
	// the participant-lookup goroutine, ActionRestart goroutines, and the
	// ActionSend goroutine.  loop() waits for them before returning so that
	// Close() guarantees no goroutine still holds a reference to the
	// ExecuteDelayedEventAction or LiveKitParticipantExists function variables
	// after it returns.
	backgroundWg sync.WaitGroup

	// restartResultCh carries the new deadline back to loop() (sized 1 so the
	// producer never blocks).  The deadline is captured at the moment
	// ActionRestart's HTTP success returns — not when loop() reads the signal —
	// so scheduler latency in loop() cannot shrink the remaining ActionSend
	// window.  Owner: loop().
	restartResultCh chan time.Time

	// ── FSM state — owned exclusively by loop() ──────────────────────────
	state                DelayEventState
	fsmTimerWaitingState *time.Timer
	// fsmTimerRestart fires DelayedEventReset; re-armed by loop() each time an
	// ActionRestart goroutine reports success via restartResultCh.
	fsmTimerRestart *time.Timer
	// restartDeadline is the absolute time by which ActionSend must complete.
	// Updated by loop() on each successful restart (via restartResultCh).
	restartDeadline time.Time
}

func NewDelayedEventJob(
	parentCtx context.Context,
	p DelayedEventJobParams,
	doneCh chan<- *DelayedEventJob,
) (*DelayedEventJob, error) {
	if p.DelayTimeout <= 0 {
		return nil, fmt.Errorf("invalid delay timeout for delayed event job: %v", p.DelayTimeout)
	}

	ctx, cancel := context.WithCancel(parentCtx)
	job := &DelayedEventJob{
		JobId:                 NewUniqueID(),
		DelayedEventJobParams: p,
		EventChannel:          make(chan DelayedEventSignal, 10),
		ctx:                   ctx,
		cancel:                cancel,
		doneCh:                doneCh,
		done:                  make(chan struct{}),
		restartResultCh:       make(chan time.Time, 1),
		state:                 WaitingForInitialConnect,
	}
	return job, nil
}

// loop is the single goroutine that owns all mutable job state.
// Started exactly once by NewDelayedEventJob's caller: go job.loop().
// It returns when the job's context is cancelled.
//
// Unexported: callers outside this package must never invoke loop directly;
// it has strict preconditions (run exactly once, in a goroutine, on a
// freshly constructed job).
func (job *DelayedEventJob) loop() {
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
			// Wait for all background goroutines to finish.  They all use
			// job.ctx which is now cancelled, so they will exit promptly.
			job.backgroundWg.Wait()
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

		case newDeadline := <-job.restartResultCh:
			// ActionRestart succeeded: extend the deadline and re-arm the timer.
			// Only apply when still Connected — ignore stale completions after a
			// state transition.
			if job.state == Connected && job.fsmTimerRestart != nil {
				job.restartDeadline = newDeadline
				job.fsmTimerRestart.Reset(job.delayRestartDuration())
			}
		}
	}
}

// Stop cancels the job's context.  Non-blocking.
//
// Use Stop when signalling teardown to many jobs in parallel; afterwards wait
// on the parent WaitGroup (e.g. loopWg in Handler.loop) for all loop()
// goroutines to drain.  For a single-job synchronous teardown — typically
// tests — use Close instead.  Safe to call concurrently and idempotent.
func (job *DelayedEventJob) Stop() {
	job.cancel()
}

// Close cancels the job's context and waits for loop() to exit (bounded by a
// 10-second safety timeout that logs a warning on overrun).  Idempotent and
// safe to call from any goroutine.
//
// The error return exists for io.Closer compatibility; this implementation
// always returns nil.  Prefer Stop + a shared WaitGroup when tearing down
// many jobs — N concurrent Closes serialize on their own 10-second timeouts.
func (job *DelayedEventJob) Close() error {
	job.cancel()
	select {
	case <-job.done:
	case <-time.After(10 * time.Second):
		slog.Warn("Job: Close() timed out waiting for loop() to exit",
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
// Called only from loop().
func (job *DelayedEventJob) delayRestartDuration() time.Duration {
	return job.DelayTimeout * 8 / 10
}

// stopTimers stops both internal timers.
// MUST only be called from loop().
func (job *DelayedEventJob) stopTimers() {
	if job.fsmTimerWaitingState != nil {
		job.fsmTimerWaitingState.Stop()
	}
	if job.fsmTimerRestart != nil {
		job.fsmTimerRestart.Stop()
	}
}

// ── FSM ──────────────────────────────────────────────────────────────────────
// All methods below are called exclusively from loop() and therefore need no
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
	case SFUParticipantGone:
		return job.handleEventSFUParticipantGone()
	case JobReplaced:
		job.state = Replaced
		slog.Info("Job: → Replaced (JobReplaced)",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "jobId", job.JobId)
		return false // no state change: no state-entry action for Replaced
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

		// Set up fsmTimerRestart so that handleEventDelayedEventReset fires
		// after each restart interval.  The deadline tracks the absolute time by
		// which ActionSend must complete; it is extended on each successful restart.
		restartDuration := job.delayRestartDuration()
		job.restartDeadline = time.Now().Add(job.DelayTimeout)
		job.fsmTimerRestart = time.AfterFunc(restartDuration, func() {
			select {
			case job.EventChannel <- DelayedEventReset:
			case <-job.ctx.Done():
			}
		})
		// We stop the timer immediately and trigger the first reset manually so the
		// homeserver timer is synced right away — we don't know how long elapsed
		// between submitting the delayed event to the homeserver and handing over
		// the delegation to this service.
		job.fsmTimerRestart.Stop()
		select {
		case job.EventChannel <- DelayedEventReset:
		case <-job.ctx.Done():
		}

	case Disconnected:
		remaining := time.Duration(0)
		if !job.restartDeadline.IsZero() {
			if r := time.Until(job.restartDeadline); r > 0 {
				remaining = r
			}
		}
		job.stopTimers()
		snapshotRemaining := remaining
		snapshotEvent := event

		// ActionSend runs in a background goroutine so loop() stays responsive.
		// It retries with exponential backoff until snapshotRemaining elapses —
		// this is the core of the delegation: we keep trying to send the leave
		// event until the original delayed-event timeout would have fired anyway.
		//
		// The handler is notified AFTER ActionSend completes (or times out) so
		// that job.ctx is NOT cancelled prematurely by handler teardown.
		// Cancelling job.ctx would abort ActionSend before the leave event is
		// sent — defeating the purpose of the delegation.
		//
		// Teardown order:
		//   ActionSend completes → doneCh notified → Handler calls job.Stop()
		//   → job.Close() → backgroundWg.Wait() → loop() exits cleanly.
		job.backgroundWg.Add(1)
		go func() {
			defer job.backgroundWg.Done()

			expBackOff := backoff.NewExponentialBackOff()
			expBackOff.InitialInterval = 1000 * time.Millisecond
			expBackOff.Multiplier = 1.5
			expBackOff.RandomizationFactor = 0.5
			expBackOff.MaxInterval = 60 * time.Second

			status, err := backoff.Retry(
				job.ctx,
				func() (int, error) {
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
			} else if status < 200 || (status >= 300 && status != 404) {
				slog.Warn("Job: ActionSend unexpected status",
					"state", Disconnected, "event", snapshotEvent,
					"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "jobId", job.JobId,
					"status", status)
			} else if status == 404 {
				slog.Info("Job: ActionSend — delayed event already sent or cancelled",
					"state", Disconnected, "event", snapshotEvent,
					"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "jobId", job.JobId)
			}

			// Notify the handler only after ActionSend has completed (or timed
			// out).  This is intentional: the handler must not cancel job.ctx
			// before ActionSend finishes, or the leave event would never be sent.
			select {
			case job.doneCh <- job:
			case <-job.ctx.Done():
				// Context cancelled externally (e.g. handler shutdown) before
				// ActionSend completed — teardown path handles cleanup.
			}
		}()

	case Completed:
		job.stopTimers()
		// Notify handler immediately (same pattern as Disconnected).
		select {
		case job.doneCh <- job:
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
		return true // new state: trigger state-entry action
	}
	return false // no state change: no state-entry action
}

func (job *DelayedEventJob) handleEventParticipantLookupSuccessful() bool {
	if job.state == WaitingForInitialConnect {
		job.state = Connected
		slog.Info("Job: → Connected (ParticipantLookupSuccessful)",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		return true // new state: trigger state-entry action
	}
	return false // no state change: no state-entry action
}

func (job *DelayedEventJob) handleEventParticipantDisconnected() bool {
	if job.state == Connected {
		job.state = Disconnected
		slog.Info("Job: → Disconnected (ParticipantDisconnected)",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		return true // new state: trigger state-entry action
	}
	return false // no state change: no state-entry action
}

func (job *DelayedEventJob) handleEventParticipantConnectionAborted() bool {
	if job.state == WaitingForInitialConnect {
		job.state = Completed
		slog.Info("Job: → Completed (ParticipantConnectionAborted)",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		return true // new state: trigger state-entry action
	}
	if job.state == Connected {
		job.state = Disconnected
		slog.Info("Job: → Disconnected (ParticipantConnectionAborted)",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		return true // new state: trigger state-entry action
	}
	return false // no state change: no state-entry action
}

func (job *DelayedEventJob) handleEventDelayedEventTimedOut() bool {
	if job.state == WaitingForInitialConnect || job.state == Connected {
		job.state = Disconnected
		slog.Info("Job: → Disconnected (DelayedEventTimedOut)",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		return true
	}
	return false // no state change: no state-entry action
}

func (job *DelayedEventJob) handleEventDelayedEventNotFound() bool {
	if job.state == WaitingForInitialConnect || job.state == Connected {
		job.state = Disconnected
		slog.Info("Job: → Disconnected (DelayedEventNotFound)",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		return true // new state: trigger state-entry action
	}
	return false // no state change: no state-entry action
}

func (job *DelayedEventJob) handleEventDelayedEventReset() bool {
	if (job.state != Connected && job.state != WaitingForInitialConnect) ||
		job.fsmTimerRestart == nil {
		return false // no state change: no state-entry action
	}

	remaining := time.Duration(0)
	if r := time.Until(job.restartDeadline); r > 0 {
		remaining = r
	}
	if remaining <= 0 {
		job.state = Disconnected
		slog.Info("Job: → Disconnected (DelayedEventReset, remaining=0)",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		return true // new state: trigger state-entry action
	}

	// Issue the restart call in a background goroutine so loop() stays
	// responsive.  On success the goroutine sends the new deadline to
	// restartResultCh; loop() reads it and re-arms fsmTimerRestart without
	// any mutex — loop() is the sole owner of both fields.
	timeout := job.DelayTimeout
	lkRm := job.LiveKitRoom
	lkId := job.LiveKitIdentity
	ch := job.EventChannel
	resCh := job.restartResultCh
	ctx := job.ctx

	job.backgroundWg.Add(1)
	go func() {
		defer job.backgroundWg.Done()
		expBackOff := backoff.NewExponentialBackOff()
		expBackOff.InitialInterval = 1000 * time.Millisecond
		expBackOff.Multiplier = 1.5
		expBackOff.RandomizationFactor = 0.5
		expBackOff.MaxInterval = 60 * time.Second

		status, err := backoff.Retry(
			ctx,
			func() (int, error) {
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
		case status == 404:
			slog.Warn("Job: ActionRestart not found — emitting DelayedEventNotFound",
				"room", lkRm, "lkId", lkId, "jobId", job.JobId)
			signal = DelayedEventNotFound
		case status < 200 || status >= 300:
			slog.Warn("Job: ActionRestart bad status — emitting DelayedEventTimedOut",
				"room", lkRm, "lkId", lkId, "jobId", job.JobId, "status", status)
			signal = DelayedEventTimedOut
		default:
			// Report success to loop() via restartResultCh; loop() will extend
			// the deadline and re-arm fsmTimerRestart.
			newDeadline := time.Now().Add(timeout)
			slog.Debug(fmt.Sprintf("Job: ActionRestart ok, next reset in %s", job.delayRestartDuration()),
				"room", lkRm, "lkId", lkId)
			select {
			case resCh <- newDeadline:
			case <-ctx.Done():
			}
			return
		}

		select {
		case ch <- signal:
		case <-ctx.Done():
		}
	}()

	return false // no state change: no state-entry action
}

func (job *DelayedEventJob) handleEventWaitingStateTimedOut() bool {
	if job.state == WaitingForInitialConnect {
		job.state = Disconnected
		slog.Info("Job: → Disconnected (WaitingStateTimedOut)",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		return true // new state: trigger state-entry action
	}
	return false // no state change: no state-entry action
}

func (job *DelayedEventJob) handleEventSFUParticipantGone() bool {
	if job.state == Connected {
		job.state = Disconnected
		slog.Info("Job: → Disconnected (SFUParticipantGone — missed disconnect webhook)",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		return true // new state: trigger state-entry action
	}
	return false // no state change: no state-entry action
}


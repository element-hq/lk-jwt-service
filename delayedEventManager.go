// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
	Aborted
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
	case Aborted:
		return "Aborted"
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
//     the identity is present or job.DelayTimeout elapses. On success sends
//     ParticipantLookupSuccessful to job.EventChannel.
//   - Phase 2: at every sanityInterval tick, calls LiveKitParticipantExists;
//     if the identity is confirmed absent (SFU returned NotFound), sends
//     SFUParticipantGone. Transport errors are logged and ignored.
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
		// ((false, nil) from LiveKitParticipantExists). Transport errors
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
// a DelayedEventJob. Built by request handlers in main.go and handed to
// Handler.addDelayedEventJob, which forwards it to Handler.loop() and
// NewDelayedEventJob. Embedded into DelayedEventJob so its fields are
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
// state (current FSM state, timers, …). No mutex is needed for that state
// because nothing outside that goroutine touches it.
//
// External callers communicate with the job exclusively through its
// EventChannel (write-only from the outside). loop() reads from that channel
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
// # Finite State Machine (FSM)
//
// The FSM itself — state diagrams, transition/internal-action tables, timer
// FSMs and their coupling — is defined and documented in the FSM section of
// this file (see fsmTransitions).
type DelayedEventJob struct {
	// Immutable after construction — safe to read without a lock. The
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
	// (Disconnected after ActionSend completes, or Aborted). The job sends
	// a pointer to itself so the Handler can verify it is still the active job
	// for this (room, identity) before cancelling and cleaning it up.
	doneCh chan<- *DelayedEventJob

	// done is closed by loop() when it exits, allowing Close() to wait.
	done chan struct{}

	// backgroundWg tracks all background goroutines started by or for this job:
	// the participant-lookup goroutine, ActionRestart goroutines, and the
	// ActionSend goroutine. loop() waits for them before returning so that
	// Close() guarantees no goroutine still holds a reference to the
	// ExecuteDelayedEventAction or LiveKitParticipantExists function variables
	// after it returns.
	backgroundWg sync.WaitGroup

	// restartResultCh carries the new deadline back to loop() (its owner).
	// Sends never block: at most one restart is in flight; the next reset
	// fires only after the previous result was consumed —> so the buffer
	// always has room.
	restartResultCh chan time.Time

	// ── FSM state — owned exclusively by loop() ──────────────────────────
	state                DelayEventState
	fsmTimerWaitingState *time.Timer
	// fsmTimerRestart fires DelayedEventReset. Created lazily by loop() when
	// the first ActionRestart result arrives via restartResultCh, re-armed on
	// each subsequent result. nil encodes "not running"; the Connected exit
	// action restores nil so re-arming a stopped timer is impossible.
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
			// Wait for all background goroutines to finish. They all use
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
			job.handleEvent(event)

		case newDeadline := <-job.restartResultCh:
			// ActionRestart succeeded. Only apply while still Connected:
			// ActionRestart retries with backoff, so a result can arrive long
			// after the reset was triggered, possibly after the FSM already left
			// Connected (the Connected exit action has nil'ed the timer by then).
			if job.state != Connected {
				break
			}
			job.restartDeadline = newDeadline
			if job.fsmTimerRestart == nil {
				// First successful restart after entering Connected: create the
				// timer (nil encodes "not running" — see # Timers above).
				job.fsmTimerRestart = time.AfterFunc(job.delayRestartDuration(), func() {
					select {
					case job.EventChannel <- DelayedEventReset:
					case <-job.ctx.Done():
					}
				})
			} else {
				job.fsmTimerRestart.Reset(job.delayRestartDuration())
			}
		}
	}
}

// Stop cancels the job's context. Non-blocking.
//
// Use Stop when signalling teardown to many jobs in parallel; afterwards wait
// on the parent WaitGroup (e.g. loopWg in Handler.loop) for all loop()
// goroutines to drain. For a single-job synchronous teardown — typically
// tests — use Close instead. Safe to call concurrently and idempotent.
func (job *DelayedEventJob) Stop() {
	job.cancel()
}

// Close cancels the job's context and waits for loop() to exit (bounded by a
// 10-second safety timeout that logs a warning on overrun). Idempotent and
// safe to call from any goroutine.
//
// The error return exists for io.Closer compatibility; this implementation
// always returns nil. Prefer Stop + a shared WaitGroup when tearing down
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
// Pure function of the immutable DelayTimeout. Safe from any goroutine.
func (job *DelayedEventJob) delayRestartDuration() time.Duration {
	return job.DelayTimeout * 8 / 10
}

// ── FSM ──────────────────────────────────────────────────────────────────────
// All methods below are called exclusively from loop() and therefore need no
// additional synchronisation.

// stopTimers stops both internal timers (shutdown path: ctx cancelled while
// the FSM may still be in a state that owns a running timer).
func (job *DelayedEventJob) stopTimers() {
	if job.fsmTimerWaitingState != nil {
		job.fsmTimerWaitingState.Stop()
	}
	if job.fsmTimerRestart != nil {
		job.fsmTimerRestart.Stop()
		// nil is the "not running" encoding — keep it consistent even on the
		// shutdown path.
		job.fsmTimerRestart = nil
	}
}

// The FSM uses the UML state-machine action taxonomy:
//
//   - Transition (table fsmTransitions):
//     state × event → next state
//   - Internal action (table fsmInternalActions):
//     state × event → action func; runs without a state change; no
//     exit/entry actions fire
//   - Entry action (switch in handleStateEntryAction):
//     runs when a state is entered, regardless of transition
//   - Exit action (switch in handleStateExitAction):
//     runs when a state is left, regardless of transition
//
// Invariant: a (state, event) pair must not appear in both tables: the
// internal action would silently shadow the transition. Enforced by
// TestFSMTables_InternalAndTransitionDisjoint.
//
// # Main Job Lifecycle
//
// Diagram notation: internal transitions are listed inside the state box and
// deliberately NOT drawn as self-loop arrows — a self-loop would denote an
// external self-transition (exit/entry actions fire).
//
//	                       ParticipantConnected,
//	  (Start)              ParticipantLookupSuccessful
//	     |                ┌───────────────────────────────┐
//	     ▼                |                               ▼
//	┌────────────────────────────┐      ┌─────────────────────────────────┐
//	│ WaitingForInitialConnect   │      │ Connected                       │
//	│                            │      │                                 │
//	│ On Entry: (none)           │      │ On Entry:                       │
//	│ On Exit:                   │      │ • Emit: DelayedEventReset       │
//	│ • Stop waiting timer       │      │                                 │
//	└────────────────────────────┘      │ On Internal:                    │
//	    |                 |             │ • DelayedEventReset:            │
//	    |                 |             │     Execute ActionRestart       │
//	    |                 |             │                                 │
//	    |                 |             │ On Exit:                        │
//	    |                 |             │ • Stop delayed-event timer      │
//	    |                 |             └─────────────────────────────────┘
//	    |                 |                          │
//	    |                 | DelayedEventTimedOut,    │ DelayedEventTimedOut,
//	    |                 | DelayedEventNotFound,    │ DelayedEventNotFound,
//	    |                 | WaitingStateTimedOut     │ ParticipantDisconnectedIntentionally,
//	    |                 |                          │ ParticipantConnectionAborted,
//	    |                 └──────────────────────────│ SFUParticipantGone
//	    |                                            │
//	    | ParticipantConnectionAborted               │
//	    ▼                                            ▼
//	┌──────────────────────┐              ┌──────────────────────┐
//	│ Aborted              │              │ Disconnected         │
//	│                      │              │                      │
//	│ On Entry:            │              │ On Entry:            │
//	│ • Notify handler     │              │ • Execute ActionSend │
//	│                      │              │ • Notify handler     │
//	└──────────────────────┘              └──────────────────────┘
//
//	    (from any state)
//	           |
//	           │ JobReplaced
//	           ▼
//	┌──────────────────────┐
//	│ Replaced             │
//	│                      │
//	│ On Entry: (none)     │
//	└──────────────────────┘
//
// # Timers
//
// Properties shared by both timers:
//
//   - concurrency: the timer objects are owned by loop(); no other goroutine
//     touches them
//   - lifecycle: each timer belongs to exactly one FSM state and may only
//     run while the FSM is in that state
//       - stopped by the owning state's exit action
//         (stopTimers covers shutdown)
//       - armed only by loop() itself: the waiting timer at loop start, the
//         delayed-event timer on a restartResultCh message
//   - FSM coupling: purely event-based — a firing timer emits its event into
//     EventChannel like any other event source
//
// The two timers:
//
//   - fsmTimerWaitingState (waiting timer):
//       - belongs to: WaitingForInitialConnect
//       - purpose: bounds how long the job waits for the initial connect
//       - fires WaitingStateTimedOut
//       - armed exactly once, at loop start
//   - fsmTimerRestart (delayed-event timer):
//       - belongs to: Connected
//       - purpose: triggers periodically the next reset of the delayed leave
//         event (at 80 % of DelayTimeout)
//       - fires DelayedEventReset
//       - created lazily on the first successful restart (via restartResultCh
//         emitted by startDelayedEventRestart), then re-armed after each
//         subsequent restart
//       - nil is its only "not running" encoding (no separate stopped flag),
//         so every Stop also restores nil to keep loop()'s nil check truthful
//
// # fsmTimerRestart — the restart cycle
//
//	Connected: On Entry
//	    │  Emit DelayedEventReset
//	    ▼
//	job.EventChannel <- DelayedEventReset
//	    │
//	    ▼
//	Connected: On internal (DelayedEventReset): startDelayedEventRestart
//	    │  Emit newDeadline
//	    ▼
//	job.restartResultCh <- newDeadline          (success only)
//	    │
//	    ▼
//	loop(): create timer (if nil) / Reset
//	    │
//	    │  (timer fires after 80 % of DelayTimeout)
//	    ▼
//	job.EventChannel <- DelayedEventReset       (cycle repeats)
//
// The cycle ends when ActionRestart fails terminally: DelayedEventTimedOut
// or DelayedEventNotFound is emitted instead of a new deadline, transitioning
// Connected → Disconnected; the exit action stops the timer. Late results on
// restartResultCh are discarded by loop()'s state != Connected guard.

// fsmTransitions maps state × event to the next state. Pairs not listed
// here cause no state change (and therefore no exit/entry actions) — this
// also makes stale events after a transition harmless, e.g. a second
// DelayedEventTimedOut arriving once the job is already Disconnected.
var fsmTransitions = map[DelayEventState]map[DelayedEventSignal]DelayEventState{
	WaitingForInitialConnect: {
		ParticipantConnected:         Connected,
		ParticipantLookupSuccessful:  Connected,
		ParticipantConnectionAborted: Aborted,
		DelayedEventTimedOut:         Disconnected,
		DelayedEventNotFound:         Disconnected,
		WaitingStateTimedOut:         Disconnected,
		JobReplaced:                  Replaced,
	},
	Connected: {
		ParticipantDisconnectedIntentionally: Disconnected,
		ParticipantConnectionAborted:         Disconnected,
		DelayedEventTimedOut:                 Disconnected,
		DelayedEventNotFound:                 Disconnected,
		SFUParticipantGone:                   Disconnected,
		JobReplaced:                          Replaced,
	},
	Disconnected: {
		JobReplaced: Replaced,
	},
	Aborted: {
		JobReplaced: Replaced,
	},
}

// fsmInternalActions maps state × event to an input action that runs without
// leaving the state — no exit/entry actions fire.
var fsmInternalActions = map[DelayEventState]map[DelayedEventSignal]func(*DelayedEventJob){
	Connected: {
		DelayedEventReset: (*DelayedEventJob).startDelayedEventRestart,
	},
}

// handleEvent processes one event completely according to the FSM rules:
// internal actions take precedence (mutually exclusive with transitions per
// the table invariant above); a transition runs the exit action of the state
// being left and the entry action of the state being entered.
func (job *DelayedEventJob) handleEvent(event DelayedEventSignal) {
	if job.handleStateInternalAction(event) {
		return // input action consumed the event, no state change
	}
	old := job.state
	next, changed := job.handleStateTransition(event)
	if !changed {
		// Expected in normal operation (stale results after a transition,
		// duplicate webhooks, late timer fires) — hence Debug, not Warn.
		slog.Debug("Job: FSM event ignored in current state",
			"state", old, "event", event,
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		return
	}
	job.handleStateExitAction(old, event)
	job.handleStateEntryAction(next, event)
}

// handleStateInternalAction runs the input action registered for the current
// state and the given event, if any. Returns whether the event was consumed.
func (job *DelayedEventJob) handleStateInternalAction(event DelayedEventSignal) bool {
	action, ok := fsmInternalActions[job.state][event]
	if !ok {
		return false
	}
	slog.Debug("Job: FSM internal action", "state", job.state, "event", event,
		"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
		"delayId", job.DelayId, "jobId", job.JobId)
	action(job)
	return true
}

// handleStateTransition determines and performs the state change for the
// given event. Unknown (state, event) pairs cause no change.
func (job *DelayedEventJob) handleStateTransition(event DelayedEventSignal) (DelayEventState, bool) {
	next, ok := fsmTransitions[job.state][event]
	if !ok {
		return job.state, false
	}
	slog.Info("Job: FSM transition", "from", job.state, "to", next, "event", event,
		"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
		"delayId", job.DelayId, "jobId", job.JobId)
	job.state = next
	return next, true
}

// handleStateExitAction runs the exit action of the state being left. Timer
// cleanup lives here — exit actions cover every outgoing path (including
// JobReplaced, which fires from any state), so no path can leak a timer.
func (job *DelayedEventJob) handleStateExitAction(state DelayEventState, event DelayedEventSignal) {
	switch state {
	case WaitingForInitialConnect:
		// The waiting-state guard timer belongs to this state.
		if job.fsmTimerWaitingState != nil {
			job.fsmTimerWaitingState.Stop()
		}

	case Connected:
		// The delayed-event timer belongs to this state. Restore the
		// "not running" encoding (nil) so loop()'s restartResultCh case never
		// Resets a stopped timer; a late ActionRestart result is additionally
		// discarded there by the state != Connected guard.
		if job.fsmTimerRestart != nil {
			job.fsmTimerRestart.Stop()
			job.fsmTimerRestart = nil
		}
	}
}

// handleStateEntryAction runs the entry action of the state being entered.
func (job *DelayedEventJob) handleStateEntryAction(state DelayEventState, event DelayedEventSignal) {
	switch state {
	case Connected:
		// The deadline tracks the absolute time by which ActionSend must
		// complete; the full DelayTimeout is available until the first
		// successful restart extends it (via restartResultCh).
		job.restartDeadline = time.Now().Add(job.DelayTimeout)
		// Trigger the first reset right away so the homeserver timer is synced
		// immediately — we don't know how long elapsed between submitting the
		// delayed event to the homeserver and handing over the delegation to
		// this service. The delayed-event timer itself is created lazily by
		// loop() once the first ActionRestart result arrives on restartResultCh.
		select {
		case job.EventChannel <- DelayedEventReset:
		case <-job.ctx.Done():
		}

	case Disconnected:
		// Clamp to [1 s, 1 h]
		//  - At least one second
		//     - so ActionSend gets a real attempt even
		//       when the deadline has passed or was never set
		//       (Disconnected reached straight from WaitingForInitialConnect)
		//     - WithMaxElapsedTime(0) would mean "retry forever" (backoff v5).
		//  - At most one hour: the sticky-event timeout; Once the membership
		//    event has expired, retrying the leave-send is pointless.
		remaining := min(time.Hour, max(time.Second, time.Until(job.restartDeadline)))

		// ActionSend runs in a background goroutine so loop() stays responsive.
		// It retries with exponential backoff until remaining elapses — this is
		// the core of the delegation: we keep trying to send the leave event
		// until the original delayed-event timeout would have fired anyway.
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
				backoff.WithMaxElapsedTime(remaining),
			)
			// Per ExecuteDelayedEventAction's explicit-success contract:
			// err != nil covers transient (5xx, 429, transport) and permanent
			// (4xx other than 404) failures. err == nil + status == 404 means
			// MSC-4140 already-sent. err == nil + 200/204 is silent success.
			switch {
			case err != nil:
				slog.Warn("Job: ActionSend failed",
					"state", Disconnected, "event", event,
					"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "jobId", job.JobId,
					"status", status, "err", err)
			case status == http.StatusNotFound:
				slog.Info("Job: ActionSend — delayed event already sent or cancelled",
					"state", Disconnected, "event", event,
					"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "jobId", job.JobId)
			}

			// Notify the handler only after ActionSend has completed (or timed
			// out). This is intentional: the handler must not cancel job.ctx
			// before ActionSend finishes, or the leave event would never be sent.
			select {
			case job.doneCh <- job:
			case <-job.ctx.Done():
				// Context cancelled externally (e.g. handler shutdown) before
				// ActionSend completed — teardown path handles cleanup.
			}
		}()

	case Aborted:
		// Timers are stopped by the exit actions of the states that own them.
		// Notify handler immediately (same pattern as Disconnected).
		select {
		case job.doneCh <- job:
		case <-job.ctx.Done():
		}
	}
}

// startDelayedEventRestart restarts the delayed event on the homeserver
// (ActionRestart) in a background goroutine tracked by backgroundWg, retrying
// with exponential backoff for as long as the restart deadline allows.
// It reports exactly one outcome — unless the job shuts down first; every
// send is ctx.Done()-guarded:
//
//   - success: new deadline on restartResultCh captured when
//     ActionRestart succeeds (not when loop() reads it) so loop() latency
//     cannot shrink the ActionSend window
//   - delayed event gone on the homeserver (404): DelayedEventNotFound
//     on EventChannel
//   - any other failure, including an already-exhausted deadline:
//     DelayedEventTimedOut on EventChannel
//
// See the restart cycle (# fsmTimerRestart) next to the FSM diagram above.
func (job *DelayedEventJob) startDelayedEventRestart() {
	remaining := time.Until(job.restartDeadline)
	if remaining <= 0 {
		slog.Info("Job: ActionRestart deadline exhausted — emitting DelayedEventTimedOut",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		select {
		case job.EventChannel <- DelayedEventTimedOut:
		case <-job.ctx.Done():
		}
		return
	}

	// Issue the restart call in a background goroutine so loop() stays
	// responsive. On success the goroutine sends the new deadline to
	// restartResultCh; loop() reads it and creates/re-arms fsmTimerRestart
	// without any mutex — loop() is the sole owner of both fields.
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
				return ExecuteDelayedEventAction(job.CsApiUrl, job.DelayId, ActionRestart)
			},
			backoff.WithBackOff(expBackOff),
			backoff.WithMaxElapsedTime(remaining),
		)

		// Per ExecuteDelayedEventAction's explicit-success contract:
		// errDelayedEventNotFound is the only "soft" failure we treat specially
		// (the event is gone, not a transient blip). Any other err falls into
		// the generic timed-out bucket. err == nil is the success path.
		var signal DelayedEventSignal
		switch {
		case errors.Is(err, errDelayedEventNotFound):
			slog.Warn("Job: ActionRestart not found — emitting DelayedEventNotFound",
				"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "jobId", job.JobId)
			signal = DelayedEventNotFound
		case err != nil:
			slog.Warn("Job: ActionRestart failed — emitting DelayedEventTimedOut",
				"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "jobId", job.JobId,
				"status", status, "err", err)
			signal = DelayedEventTimedOut
		default:
			// Report success to loop() via restartResultCh; loop() will extend
			// the deadline and re-arm fsmTimerRestart.
			newDeadline := time.Now().Add(job.DelayTimeout)
			slog.Debug("Job: ActionRestart ok", "room", job.LiveKitRoom,
				"lkId", job.LiveKitIdentity,
				"next reset in", job.delayRestartDuration())
			select {
			case job.restartResultCh <- newDeadline:
			case <-job.ctx.Done():
			}
			return
		}

		select {
		case job.EventChannel <- signal:
		case <-job.ctx.Done():
		}
	}()
}

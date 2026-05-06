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

// ── LiveKitRoomWorker ────────────────────────────────────────────────────────

// liveKitJobRegistration registers or unregisters a job identity with a
// LiveKitRoomWorker.
type liveKitJobRegistration struct {
	identity LiveKitIdentity
	eventCh  chan<- DelayedEventSignal
	remove   bool
}

// LiveKitRoomWorker manages participant lookups for all jobs in a single
// LiveKit room.  A single goroutine (run()) owns all state; external callers
// communicate exclusively via channels — no mutex needed.
//
//   - Phase 1 (initial lookup): when a job is registered the worker immediately
//     calls LiveKitListParticipants; if the participant is present it sends
//     ParticipantLookupSuccessful to the job's EventChannel.
//   - Phase 2 (sanity check): at every sanityInterval tick the worker calls
//     LiveKitListParticipants for the room and sends SFUParticipantGone to any
//     job whose identity is no longer listed.  Disabled when sanityInterval == 0.
//
// Communication with jobs is identical to SFU webhooks — both send
// DelayedEventSignal values on the job's EventChannel — so no new concurrency
// primitives are introduced.
type LiveKitRoomWorker struct {
	roomAlias      LiveKitRoomAlias
	lkAuth         LiveKitAuth
	sanityInterval time.Duration
	registerCh     chan liveKitJobRegistration
}

func NewLiveKitRoomWorker(roomAlias LiveKitRoomAlias, lkAuth LiveKitAuth, sanityInterval time.Duration) *LiveKitRoomWorker {
	return &LiveKitRoomWorker{
		roomAlias:      roomAlias,
		lkAuth:         lkAuth,
		sanityInterval: sanityInterval,
		registerCh:     make(chan liveKitJobRegistration, 20),
	}
}

// Register tells the worker to start Phase-1 lookup for identity and to include
// it in Phase-2 sanity checks thereafter.  Results are delivered on eventCh.
func (w *LiveKitRoomWorker) Register(ctx context.Context, identity LiveKitIdentity, eventCh chan<- DelayedEventSignal) {
	select {
	case w.registerCh <- liveKitJobRegistration{identity: identity, eventCh: eventCh}:
	case <-ctx.Done():
	}
}

// Unregister tells the worker to stop tracking identity.
func (w *LiveKitRoomWorker) Unregister(ctx context.Context, identity LiveKitIdentity) {
	select {
	case w.registerCh <- liveKitJobRegistration{identity: identity, remove: true}:
	case <-ctx.Done():
	}
}

// run is the worker's main goroutine.  It exits when ctx is cancelled.
// Must be started exactly once, tracked in Handler's loopWg.
func (w *LiveKitRoomWorker) run(ctx context.Context) {
	// pendingLookup: identities awaiting Phase-1 confirmation.
	// active: identities that have passed Phase-1 (ParticipantLookupSuccessful sent).
	pendingLookup := make(map[LiveKitIdentity]chan<- DelayedEventSignal)
	active := make(map[LiveKitIdentity]chan<- DelayedEventSignal)

	doCheck := func() {
		if len(pendingLookup) == 0 && len(active) == 0 {
			return
		}
		resp, err := LiveKitListParticipants(ctx, w.lkAuth, w.roomAlias)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Debug("LiveKitRoomWorker: ListParticipants failed",
				"room", w.roomAlias, "err", err)
			return
		}
		present := make(map[LiveKitIdentity]bool, len(resp.GetParticipants()))
		for _, p := range resp.GetParticipants() {
			present[LiveKitIdentity(p.GetIdentity())] = true
		}
		// Phase 1: promote any pending identity that is now present.
		for identity, eventCh := range pendingLookup {
			if present[identity] {
				active[identity] = eventCh
				delete(pendingLookup, identity)
				slog.Debug("LiveKitRoomWorker: participant found (Phase 1)",
					"room", w.roomAlias, "lkId", identity)
				select {
				case eventCh <- ParticipantLookupSuccessful:
				case <-ctx.Done():
					return
				}
			}
		}
		// Phase 2: detect gone participants (only when sanity check is enabled).
		if w.sanityInterval == 0 {
			return
		}
		for identity, eventCh := range active {
			if !present[identity] {
				slog.Warn("LiveKitRoomWorker: sanity check: participant no longer on SFU",
					"room", w.roomAlias, "lkId", identity)
				select {
				case eventCh <- SFUParticipantGone:
				case <-ctx.Done():
					return
				}
				delete(active, identity)
			}
		}
	}

	var tickerC <-chan time.Time
	if w.sanityInterval > 0 {
		ticker := time.NewTicker(w.sanityInterval)
		defer ticker.Stop()
		tickerC = ticker.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case reg := <-w.registerCh:
			if reg.remove {
				delete(pendingLookup, reg.identity)
				delete(active, reg.identity)
			} else {
				pendingLookup[reg.identity] = reg.eventCh
				doCheck()
			}
		case <-tickerC:
			doCheck()
		}
	}
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
//  1. Created by Handler.loop() when a new delayed-event request arrives.
//  2. Caller starts Loop() — typically in a goroutine: go job.Loop().
//  3. Events arrive via EventChannel (SFU webhooks, internal FSM timers).
//  4. Loop() exits when ctx is cancelled (via Close() or parent cancellation).
//  5. Close() cancels the context and blocks until Loop() has returned.
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

	// doneCh signals Handler.loop() when the job enters a terminal state
	// (Disconnected after ActionSend completes, or Completed).  The job sends
	// a pointer to itself so the Handler can verify it is still the active job
	// for this (room, identity) before cancelling and cleaning it up.
	doneCh chan<- *DelayedEventJob

	// done is closed by Loop() when it exits, allowing Close() to wait.
	done chan struct{}

	// resetWg tracks all background goroutines started by or for this job:
	// ActionRestart goroutines, the ActionSend goroutine, and the participant
	// lookup goroutine.  Loop() waits for them before returning so that Close()
	// guarantees no goroutine still holds a reference to the
	// ExecuteDelayedEventAction or LiveKitParticipantLookup function variables
	// after it returns.
	resetWg sync.WaitGroup

	// ── FSM state — owned exclusively by Loop() ──────────────────────────
	state                DelayEventState
	fsmTimerWaitingState *time.Timer
	fsmTimerDelayedEvent *delayedEventTimer
}

func NewDelayedEventJob(
	parentCtx context.Context,
	jobRequest *DelayedEventRequest,
	doneCh chan<- *DelayedEventJob,
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
		doneCh:          doneCh,
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
			// Wait for all background goroutines to finish.  They all use
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

		// Set up fsmTimerDelayedEvent so that handleEventDelayedEventReset can
		// call timer.reset() on subsequent restarts.  The AfterFunc fires
		// DelayedEventReset into the FSM when the restart interval elapses.
		// We stop it immediately and trigger the first reset manually so the
		// homeserver timer is synced right away — we don't know how long elapsed
		// between submitting the delayed event to the homeserver and handing over
		// the delegation to this service.
		job.fsmTimerDelayedEvent = newDelayedEventTimer(
			job.delayRestartDuration(),
			func() {
				select {
				case job.EventChannel <- DelayedEventReset:
				case <-job.ctx.Done():
				}
			},
		)
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

		// ActionSend runs in a background goroutine so Loop() stays responsive.
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
		//   ActionSend completes → doneCh notified → Handler calls job.Cancel()
		//   → job.Close() → resetWg.Wait() → Loop() exits cleanly.
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
		job.fsmTimerDelayedEvent == nil {
		return false // no state change: no state-entry action
	}

	remaining := job.fsmTimerDelayedEvent.timeRemaining()
	if remaining <= 0 {
		job.state = Disconnected
		slog.Info("Job: → Disconnected (DelayedEventReset, remaining=0)",
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity,
			"delayId", job.DelayId, "jobId", job.JobId)
		return true // new state: trigger state-entry action
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

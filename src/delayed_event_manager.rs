// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//! The DelayedEventJob actor: a finite state machine modelling the
//! lifecycle of a MatrixRTC cancellable delayed disconnect event for a
//! single participant in a LiveKit room.

use std::fmt;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime};

use futures::future::BoxFuture;
use serde::{Deserialize, Serialize};
use tokio::sync::{mpsc, watch, Notify};
use tokio::time::Instant;
use tokio_util::sync::CancellationToken;
use tracing::{debug, info, warn};

use crate::helper::{
    new_unique_id, CsApiUrl, Deps, LiveKitAuth, LiveKitIdentity, LiveKitRoomAlias, UniqueId,
};
use crate::retry::{retry, Classify, ErrorClass, ExponentialBackoff, RetryError};

/// The Matrix CS-API path for delayed events.
///
/// TODO(MSC4140): Add a `/versions` check so that we can use the unstable or
/// the stable path (/_matrix/client/v1/delayed_events) depending on what the
/// homeserver supports.
///
/// See https://github.com/matrix-org/matrix-spec-proposals/pull/4140
pub const DELAYED_EVENTS_ENDPOINT: &str =
    "/_matrix/client/unstable/org.matrix.msc4140/delayed_events";

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DelayEventAction {
    Restart,
    Send,
}

impl DelayEventAction {
    pub fn as_str(&self) -> &'static str {
        match self {
            DelayEventAction::Restart => "restart",
            DelayEventAction::Send => "send",
        }
    }
}

impl fmt::Display for DelayEventAction {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum DelayEventState {
    WaitingForInitialConnect,
    Connected,
    Disconnected,
    Aborted,
    Replaced,
}

impl fmt::Display for DelayEventState {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let s = match self {
            DelayEventState::WaitingForInitialConnect => "WaitingForInitialConnect",
            DelayEventState::Connected => "Connected",
            DelayEventState::Disconnected => "Disconnected",
            DelayEventState::Aborted => "Aborted",
            DelayEventState::Replaced => "Replaced",
        };
        f.write_str(s)
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum DelayedEventSignal {
    ParticipantConnected,
    ParticipantLookupSuccessful,
    ParticipantDisconnectedIntentionally,
    ParticipantConnectionAborted,
    DelayedEventReset,
    DelayedEventTimedOut,
    DelayedEventNotFound,
    CsApiUrlNotFound,
    WaitingStateTimedOut,
    SfuNotAvailable,
    JobReplaced,
    /// Emitted by the sanity-check phase of the participant-lookup task when
    /// a Connected participant can no longer be found on the SFU — indicating
    /// a missed disconnect webhook.
    SfuParticipantGone,
}

impl fmt::Display for DelayedEventSignal {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let s = match self {
            DelayedEventSignal::ParticipantConnected => "ParticipantConnected",
            DelayedEventSignal::ParticipantLookupSuccessful => "ParticipantLookupSuccessful",
            DelayedEventSignal::ParticipantDisconnectedIntentionally => {
                "ParticipantDisconnectedIntentionally"
            }
            DelayedEventSignal::ParticipantConnectionAborted => "ParticipantConnectionAborted",
            DelayedEventSignal::DelayedEventReset => "DelayedEventReset",
            DelayedEventSignal::DelayedEventTimedOut => "DelayedEventTimedOut",
            DelayedEventSignal::DelayedEventNotFound => "DelayedEventNotFound",
            DelayedEventSignal::CsApiUrlNotFound => "CsApiUrlNotFound",
            DelayedEventSignal::WaitingStateTimedOut => "WaitingStateTimedOut",
            DelayedEventSignal::SfuNotAvailable => "SFUNotAvailable",
            DelayedEventSignal::JobReplaced => "JobReplaced",
            DelayedEventSignal::SfuParticipantGone => "SFUParticipantGone",
        };
        f.write_str(s)
    }
}

/// An SFU webhook event routed to a job.
#[derive(Debug, Clone)]
pub struct SfuMessage {
    pub signal: DelayedEventSignal,
    pub livekit_identity: LiveKitIdentity,
}

/// The map key used by the handler loop to track active delayed-event jobs.
/// LiveKit identity encodes the Matrix user ID (which includes the homeserver
/// domain), so (room, identity) is unique — no need for CsApiUrl in the key.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub struct JobKey {
    pub room: LiveKitRoomAlias,
    pub identity: LiveKitIdentity,
}

/// Resolves the Client-Server API base URL for a homeserver.
pub type LookupCsApiUrlFn =
    Arc<dyn Fn(String) -> BoxFuture<'static, Result<CsApiUrl, String>> + Send + Sync>;

// ── WaitGroup ────────────────────────────────────────────────────────────────

/// A Go-style wait group for tracking background tasks. Guards decrement the
/// count when dropped; `wait` resolves once the count reaches zero.
#[derive(Clone, Default)]
pub(crate) struct WaitGroup(Arc<WaitGroupInner>);

#[derive(Default)]
struct WaitGroupInner {
    count: AtomicUsize,
    notify: Notify,
}

pub(crate) struct WaitGroupGuard(Arc<WaitGroupInner>);

impl WaitGroup {
    pub fn add(&self) -> WaitGroupGuard {
        self.0.count.fetch_add(1, Ordering::SeqCst);
        WaitGroupGuard(self.0.clone())
    }

    pub async fn wait(&self) {
        loop {
            let notified = self.0.notify.notified();
            tokio::pin!(notified);
            // Register with the notifier BEFORE re-checking the count —
            // notify_waiters() only reaches registered waiters, so checking
            // first would race with a concurrent final decrement.
            notified.as_mut().enable();
            if self.0.count.load(Ordering::SeqCst) == 0 {
                return;
            }
            notified.await;
        }
    }
}

impl Drop for WaitGroupGuard {
    fn drop(&mut self) {
        if self.0.count.fetch_sub(1, Ordering::SeqCst) == 1 {
            self.0.notify.notify_waiters();
        }
    }
}

// ── participant lookup ───────────────────────────────────────────────────────

/// The error side of a single Phase-1 lookup attempt.
#[derive(Debug, thiserror::Error)]
enum LookupError {
    /// The SFU confirmed the participant is not present yet — keep polling.
    /// (Not a transport failure.)
    #[error("livekit: participant not currently present")]
    Absent,
    /// Transport / auth / server error — presence unknown, retry.
    #[error("{0}")]
    Transport(String),
    /// The job shut down mid-attempt.
    #[error("lookup cancelled")]
    Cancelled,
}

impl Classify for LookupError {
    fn classify(&self) -> ErrorClass {
        match self {
            LookupError::Cancelled => ErrorClass::Permanent,
            _ => ErrorClass::Transient,
        }
    }
}

/// Spawns a task (tracked by the job's background wait group) that performs
/// Phase-1 and Phase-2 participant checks for the given job.
///
///   - Phase 1: calls `deps.participant_exists` with exponential backoff until
///     the identity is present or the job's delay timeout elapses. On success
///     sends ParticipantLookupSuccessful to the job's event channel.
///   - Phase 2: at every `sanity_interval` tick, calls
///     `deps.participant_exists`; if the identity is confirmed absent (SFU
///     returned NotFound), sends SfuParticipantGone. Transport errors are
///     logged and ignored. Disabled when `sanity_interval` is zero.
pub fn start_participant_lookup(
    job: &Arc<DelayedEventJob>,
    deps: Arc<dyn Deps>,
    lk_auth: LiveKitAuth,
    sanity_interval: Duration,
) {
    let lk_room = job.params.livekit_room.clone();
    let lk_id = job.params.livekit_identity.clone();

    let guard = job.background.add();
    let job = job.clone();
    tokio::spawn(async move {
        let _guard = guard;
        let cancel = job.cancel.clone();

        // Phase 1: retry with exponential backoff until the participant
        // appears. The waiting duration is bounded to at most one hour
        // (sticky-event timeout).
        let waiting_duration = Duration::from_secs(60 * 60).min(job.params.delay_timeout);

        let result = retry(
            &cancel,
            ExponentialBackoff::service_default(),
            waiting_duration,
            || {
                let deps = deps.clone();
                let lk_auth = lk_auth.clone();
                let lk_room = lk_room.clone();
                let lk_id = lk_id.clone();
                let cancel = cancel.clone();
                async move {
                    // Race the attempt against job shutdown.
                    let outcome = tokio::select! {
                        _ = cancel.cancelled() => return Err(LookupError::Cancelled),
                        r = deps.participant_exists(&lk_auth, &lk_room, &lk_id) => r,
                    };
                    match outcome {
                        Err(e) => Err(LookupError::Transport(e)), // transport error — retry
                        Ok(false) => Err(LookupError::Absent),    // confirmed absent — retry
                        Ok(true) => Ok(()),                       // present — success
                    }
                }
            },
        )
        .await;

        if let Err(err) = result {
            if !cancel.is_cancelled() {
                warn!(room = %lk_room, lk_id = %lk_id, err = %err, "participantLookup: Phase 1 failed");
            }
            return;
        }
        debug!(room = %lk_room, lk_id = %lk_id, "participantLookup: Phase 1 succeeded");
        tokio::select! {
            _ = cancel.cancelled() => return,
            _ = job.event_tx.send(DelayedEventSignal::ParticipantLookupSuccessful) => {}
        }

        // Phase 2: periodic sanity checks (disabled when sanity_interval == 0).
        //
        // Only emit SfuParticipantGone when the SFU confirms absence
        // (Ok(false) from participant_exists). Transport errors are logged
        // and ignored — a transient blip should not tear the job down
        // prematurely.
        if sanity_interval.is_zero() {
            return;
        }
        let mut ticker =
            tokio::time::interval_at(Instant::now() + sanity_interval, sanity_interval);
        ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            tokio::select! {
                _ = cancel.cancelled() => return,
                _ = ticker.tick() => {
                    let outcome = tokio::select! {
                        _ = cancel.cancelled() => return,
                        r = deps.participant_exists(&lk_auth, &lk_room, &lk_id) => r,
                    };
                    match outcome {
                        Err(err) => {
                            if cancel.is_cancelled() {
                                return;
                            }
                            warn!(room = %lk_room, lk_id = %lk_id, err = %err,
                                "participantLookup: Phase 2: transport error (ignored)");
                            continue;
                        }
                        Ok(true) => {}
                        Ok(false) => {
                            warn!(room = %lk_room, lk_id = %lk_id,
                                "participantLookup: Phase 2: participant no longer on SFU");
                            tokio::select! {
                                _ = cancel.cancelled() => {}
                                _ = job.event_tx.send(DelayedEventSignal::SfuParticipantGone) => {}
                            }
                            return;
                        }
                    }
                }
            }
        }
    });
}

// ── DelayedEventJob ──────────────────────────────────────────────────────────

mod duration_ns {
    //! Serializes Duration as an integer nanosecond count, the JSON shape
    //! expected by existing stores.
    use super::*;
    use serde::{Deserializer, Serializer};

    pub fn serialize<S: Serializer>(d: &Duration, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_i64(d.as_nanos() as i64)
    }

    pub fn deserialize<'de, D: Deserializer<'de>>(d: D) -> Result<Duration, D::Error> {
        let ns = i64::deserialize(d)?;
        Ok(Duration::from_nanos(ns.max(0) as u64))
    }
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct DelayedEventJobParams {
    #[serde(rename = "DelayId")]
    pub delay_id: String,
    #[serde(rename = "DelayTimeout", with = "duration_ns")]
    pub delay_timeout: Duration,
    /// The Matrix server name from which to resolve the client-server API
    /// for making delayed event requests.
    #[serde(rename = "ServerName")]
    pub server_name: String,
    #[serde(rename = "LiveKitRoom")]
    pub livekit_room: LiveKitRoomAlias,
    #[serde(rename = "LiveKitIdentity")]
    pub livekit_identity: LiveKitIdentity,
}

/// Sent to the handler loop when a job successfully restarted its delayed
/// event, so the stored job can be updated.
pub struct JobRestartedRequest {
    pub job: Arc<DelayedEventJob>,
    pub restarted_at: SystemTime,
}

/// DelayedEventJob models the complete lifecycle of a MatrixRTC cancellable
/// delayed disconnect event for a single participant in a LiveKit room.
///
/// # Actor model
///
/// A single task — started via [`DelayedEventJob::spawn_loop`] — is the sole
/// owner of all FSM decisions. External callers communicate with the job
/// exclusively through its event channel (`event_tx`). The loop reads from
/// that channel and drives the FSM.
///
/// # Lifecycle
///
///  1. `spawn_loop` starts the actor loop.
///  2. Events arrive via the event channel (SFU webhooks, internal timers).
///  3. The loop exits when the job is cancelled (`stop`, `close` or parent
///     cancellation); `close` additionally waits for the exit.
///
/// The FSM — state diagram, transition/internal-action tables, timers — is
/// documented at [`fsm_transitions`] and the FSM section below.
pub struct DelayedEventJob {
    // Immutable after construction.
    pub job_id: UniqueId,
    pub params: DelayedEventJobParams,
    lookup_cs_api_url: LookupCsApiUrlFn,
    deps: Arc<dyn Deps>,

    /// The only way to send input to the job from the outside. Buffered so
    /// that senders are unlikely to block.
    pub event_tx: mpsc::Sender<DelayedEventSignal>,
    event_rx: Mutex<Option<mpsc::Receiver<DelayedEventSignal>>>,

    pub(crate) cancel: CancellationToken,

    /// Signals a terminal state (Disconnected after ActionSend completed, or
    /// Aborted). Carries the job itself so stale signals from a replaced job
    /// can be told apart from the active one.
    done_tx: mpsc::Sender<Arc<DelayedEventJob>>,

    /// Signals a successful restart of the delayed event.
    restarted_tx: mpsc::Sender<JobRestartedRequest>,

    /// Set to true by the loop when it exits, allowing `close` to wait.
    loop_done_tx: watch::Sender<bool>,

    /// Tracks all background tasks of this job (participant lookup,
    /// ActionRestart, ActionSend). The loop drains it before exiting, so
    /// after `close` no task holds a reference to the deps.
    pub(crate) background: WaitGroup,

    /// Carries the new deadline back to the loop (its owner). Sends never
    /// block: at most one restart is in flight; the next reset fires only
    /// after the previous result was consumed — so the buffer always has room.
    restart_result_tx: mpsc::Sender<Instant>,
    restart_result_rx: Mutex<Option<mpsc::Receiver<Instant>>>,

    // ── FSM state — owned by the loop ─────────────────────────────────────
    state: Mutex<DelayEventState>,
    /// The absolute time by which ActionSend must complete. None means
    /// "never set" and is treated as already expired.
    restart_deadline: Mutex<Option<Instant>>,
}

impl fmt::Display for DelayedEventJob {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "DelayedEventJob{{ServerName: {}, DelayId: {}, DelayTimeout: {:?}, LiveKitRoom: {}, LiveKitIdentity: {}, State: {}}}",
            self.params.server_name,
            self.params.delay_id,
            self.params.delay_timeout,
            self.params.livekit_room,
            self.params.livekit_identity,
            self.state(),
        )
    }
}

/// The per-state timers owned by the loop. Each timer is a spawned sleep task
/// guarded by its own cancellation token; "stopped" is encoded by cancelling
/// the token and clearing the slot.
#[derive(Default)]
struct LoopTimers {
    waiting: Option<CancellationToken>,
    restart: Option<CancellationToken>,
}

impl LoopTimers {
    /// Stops both internal timers (shutdown path: cancelled while the FSM may
    /// still be in a state that owns a running timer).
    fn stop_all(&mut self) {
        if let Some(t) = self.waiting.take() {
            t.cancel();
        }
        if let Some(t) = self.restart.take() {
            t.cancel();
        }
    }
}

impl DelayedEventJob {
    pub fn new(
        parent_cancel: &CancellationToken,
        params: DelayedEventJobParams,
        deps: Arc<dyn Deps>,
        lookup_cs_api_url: LookupCsApiUrlFn,
        done_tx: mpsc::Sender<Arc<DelayedEventJob>>,
        restarted_tx: mpsc::Sender<JobRestartedRequest>,
    ) -> Result<Arc<Self>, String> {
        if params.delay_timeout.is_zero() {
            return Err(format!(
                "invalid delay timeout for delayed event job: {:?}",
                params.delay_timeout
            ));
        }

        let (event_tx, event_rx) = mpsc::channel(10);
        let (restart_result_tx, restart_result_rx) = mpsc::channel(1);
        let (loop_done_tx, _) = watch::channel(false);
        Ok(Arc::new(Self {
            job_id: new_unique_id(),
            params,
            lookup_cs_api_url,
            deps,
            event_tx,
            event_rx: Mutex::new(Some(event_rx)),
            cancel: parent_cancel.child_token(),
            done_tx,
            restarted_tx,
            loop_done_tx,
            background: WaitGroup::default(),
            restart_result_tx,
            restart_result_rx: Mutex::new(Some(restart_result_rx)),
            state: Mutex::new(DelayEventState::WaitingForInitialConnect),
            restart_deadline: Mutex::new(None),
        }))
    }

    pub fn state(&self) -> DelayEventState {
        *self.state.lock().unwrap()
    }

    fn set_state(&self, state: DelayEventState) {
        *self.state.lock().unwrap() = state;
    }

    pub(crate) fn set_restart_deadline(&self, deadline: Instant) {
        *self.restart_deadline.lock().unwrap() = Some(deadline);
    }

    fn restart_deadline(&self) -> Option<Instant> {
        *self.restart_deadline.lock().unwrap()
    }

    /// Starts the actor loop. Must be called exactly once, on a freshly
    /// constructed job — the loop is the single owner of all FSM decisions.
    /// Panics when called a second time.
    pub fn spawn_loop(self: &Arc<Self>) {
        let event_rx = self
            .event_rx
            .lock()
            .unwrap()
            .take()
            .expect("DelayedEventJob loop started twice");
        let restart_result_rx = self
            .restart_result_rx
            .lock()
            .unwrap()
            .take()
            .expect("DelayedEventJob loop started twice");
        let job = self.clone();
        tokio::spawn(async move { job.run_loop(event_rx, restart_result_rx).await });
    }

    /// The actor loop. Runs until the job is cancelled.
    async fn run_loop(
        self: Arc<Self>,
        mut event_rx: mpsc::Receiver<DelayedEventSignal>,
        mut restart_result_rx: mpsc::Receiver<Instant>,
    ) {
        let mut timers = LoopTimers::default();

        // Arm the waiting-state guard timer, bounded to at most one hour
        // (sticky-event timeout).
        let waiting_duration = Duration::from_secs(60 * 60).min(self.params.delay_timeout);
        let waiting_token = self.cancel.child_token();
        timers.waiting = Some(waiting_token.clone());
        {
            let job = self.clone();
            tokio::spawn(async move {
                tokio::select! {
                    _ = waiting_token.cancelled() => {}
                    _ = tokio::time::sleep(waiting_duration) => {
                        debug!(room = %job.params.livekit_room, lk_id = %job.params.livekit_identity,
                            "Job: FSM WaitingState -> WaitingStateTimedOut");
                        tokio::select! {
                            _ = job.cancel.cancelled() => {}
                            _ = job.event_tx.send(DelayedEventSignal::WaitingStateTimedOut) => {}
                        }
                    }
                }
            });
        }

        loop {
            tokio::select! {
                _ = self.cancel.cancelled() => {
                    timers.stop_all();
                    // Wait for all background tasks to finish. They all watch
                    // the job's cancellation token, which is now cancelled, so
                    // they will exit promptly.
                    self.background.wait().await;
                    debug!(room = %self.params.livekit_room, lk_id = %self.params.livekit_identity,
                        "Job: Loop exiting");
                    break;
                }

                Some(event) = event_rx.recv() => {
                    debug!(event = %event, room = %self.params.livekit_room,
                        lk_id = %self.params.livekit_identity, "Job: dispatching event");
                    self.handle_event(event, &mut timers).await;
                }

                Some(new_deadline) = restart_result_rx.recv() => {
                    // ActionRestart succeeded. Only apply while still
                    // Connected: ActionRestart retries with backoff, so a
                    // result can arrive long after the reset was triggered,
                    // possibly after the FSM already left Connected (the
                    // Connected exit action has cleared the timer by then).
                    if self.state() != DelayEventState::Connected {
                        continue;
                    }
                    self.set_restart_deadline(new_deadline);
                    // (Re-)arm the delayed-event timer by cancelling any
                    // running sleep task and spawning a fresh one.
                    if let Some(t) = timers.restart.take() {
                        t.cancel();
                    }
                    let restart_token = self.cancel.child_token();
                    timers.restart = Some(restart_token.clone());
                    let job = self.clone();
                    let delay = self.delay_restart_duration();
                    tokio::spawn(async move {
                        tokio::select! {
                            _ = restart_token.cancelled() => {}
                            _ = tokio::time::sleep(delay) => {
                                tokio::select! {
                                    _ = job.cancel.cancelled() => {}
                                    _ = job.event_tx.send(DelayedEventSignal::DelayedEventReset) => {}
                                }
                            }
                        }
                    });
                }
            }
        }

        self.loop_done_tx.send_replace(true);
    }

    /// Tears the job down asynchronously: signals shutdown and returns
    /// immediately, without waiting for the job's tasks to exit.
    /// Idempotent and safe to call from anywhere.
    /// For a synchronous teardown, use [`Self::close`].
    pub fn stop(&self) {
        self.cancel.cancel();
    }

    /// Tears the job down synchronously: signals shutdown and waits until all
    /// job tasks have exited, bounded by a 10-second timeout. Returns an
    /// error when that wait times out — also the case when the loop was never
    /// started. Idempotent and safe to call from anywhere.
    /// For an asynchronous teardown, use [`Self::stop`].
    pub async fn close(&self) -> Result<(), String> {
        self.cancel.cancel();
        let mut done = self.loop_done_tx.subscribe();
        let wait = async {
            while !*done.borrow_and_update() {
                if done.changed().await.is_err() {
                    break;
                }
            }
        };
        if tokio::time::timeout(Duration::from_secs(10), wait)
            .await
            .is_err()
        {
            return Err(format!(
                "job {} (room {}, lkId {}): close timed out waiting for the loop to exit",
                self.job_id, self.params.livekit_room, self.params.livekit_identity
            ));
        }
        debug!(room = %self.params.livekit_room, lk_id = %self.params.livekit_identity, "Job: closed");
        Ok(())
    }

    /// Returns 80 % of the original timeout. Pure function of the immutable
    /// delay timeout.
    fn delay_restart_duration(&self) -> Duration {
        self.params.delay_timeout * 8 / 10
    }

    // ── FSM ──────────────────────────────────────────────────────────────────
    // All methods below are called exclusively from the loop (or directly by
    // tests) and follow the UML state-machine action taxonomy:
    //
    //   - Transition (table fsm_transitions):
    //     state × event → next state
    //   - Internal action (table fsm_internal_actions):
    //     state × event → action; runs without a state change; no
    //     exit/entry actions fire
    //   - Entry action (handle_state_entry_action):
    //     runs when a state is entered, regardless of transition
    //   - Exit action (handle_state_exit_action):
    //     runs when a state is left, regardless of transition
    //
    // Invariant: a (state, event) pair must not appear in both tables — the
    // internal action would silently shadow the transition.
    //
    // # Main Job Lifecycle
    //
    // Diagram notation: internal transitions are listed inside the state box
    // and deliberately NOT drawn as self-loop arrows — a self-loop would
    // denote an external self-transition (exit/entry actions fire).
    //
    //                                           ┌──────────────────────┐
    //                                           │ Disconnected         │
    //                                           │                      │
    //                      ┌───────────────────►│ On Entry:            │
    //                      |                    │ • Execute ActionSend │
    //                      |                    │ • Notify handler     │
    //                      |                    └──────────────────────┘
    //                      |                          ▲
    //                      | WaitingStateTimedOut     | ParticipantDisconnectedIntentionally,
    //      (Start)         |                          | ParticipantConnectionAborted,
    //         |            |                          | SFUParticipantGone
    //         ▼            |                          |
    //	┌────────────────────────────┐      ┌─────────────────────────────────┐
    //	│ WaitingForInitialConnect   │      │ Connected                       │
    //	│                            │      │                                 │
    //	│ On Entry: (none)           │      │ On Entry:                       │
    //	│ On Exit:                   │      │ • Emit: DelayedEventReset       │
    //	│ • Stop waiting timer       │      │                                 │
    //	└────────────────────────────┘      │ On Internal:                    │
    //	  |   |                             │ • DelayedEventReset:            │
    //	  |   | ParticipantConnected,       │     Execute ActionRestart       │
    //	  |   | ParticipantLookupSuccessful │                                 │
    //	  |   └────────────────────────────►│ On Exit:                        │
    //	  |                                 │ • Stop delayed-event timer      │
    //	  |                                 └─────────────────────────────────┘
    //	  |                                            |
    //	  | ParticipantConnectionAborted               | CsApiUrlNotFound,
    //	  ▼                                            | DelayedEventTimedOut,
    //	┌──────────────────────┐                       | DelayedEventNotFound
    //	│ Aborted              │                       |
    //	│                      │                       |
    //	│ On Entry:            │◄──────────────────────╯
    //	│ • Notify handler     │
    //	│                      │
    //	└──────────────────────┘
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
    //   - concurrency: the timer slots are owned by the loop; no other task
    //     touches them
    //   - lifecycle: each timer belongs to exactly one FSM state and may only
    //     run while the FSM is in that state
    //       - stopped by the owning state's exit action
    //         (stop_all covers shutdown)
    //       - armed only by the loop itself: the waiting timer at loop start,
    //         the delayed-event timer on a restart_result message
    //   - FSM coupling: purely event-based — a firing timer emits its event
    //     into the event channel like any other event source
    //
    // The two timers:
    //
    //   - waiting timer:
    //       - belongs to: WaitingForInitialConnect
    //       - purpose: bounds how long the job waits for the initial connect
    //       - fires WaitingStateTimedOut
    //       - armed exactly once, at loop start
    //   - restart (delayed-event) timer:
    //       - belongs to: Connected
    //       - purpose: triggers periodically the next reset of the delayed
    //         leave event (at 80 % of the delay timeout)
    //       - fires DelayedEventReset
    //       - created lazily on the first successful restart (via the
    //         restart_result channel fed by start_delayed_event_restart),
    //         then re-armed after each subsequent restart
    //       - None is its only "not running" encoding, so every stop also
    //         restores None to keep the loop's slot check truthful
    //
    // # The restart cycle
    //
    //	Connected: On Entry
    //	    │  Emit DelayedEventReset
    //	    ▼
    //	event channel <- DelayedEventReset
    //	    │
    //	    ▼
    //	Connected: On internal (DelayedEventReset): start_delayed_event_restart
    //	    │  Emit new deadline
    //	    ▼
    //	restart_result <- new deadline               (success only)
    //	    │
    //	    ▼
    //	loop: create timer (if None) / re-arm
    //	    │
    //	    │  (timer fires after 80 % of the delay timeout)
    //	    ▼
    //	event channel <- DelayedEventReset           (cycle repeats)
    //
    // The cycle ends when ActionRestart fails terminally: CsApiUrlNotFound,
    // DelayedEventTimedOut or DelayedEventNotFound is emitted instead of a
    // new deadline, transitioning Connected → Aborted; the exit action stops
    // the timer. Late results on the restart_result channel are discarded by
    // the loop's state != Connected guard.

    /// Processes one event: internal actions take precedence; a transition
    /// runs the exit action of the state being left and the entry action of
    /// the state being entered.
    async fn handle_event(self: &Arc<Self>, event: DelayedEventSignal, timers: &mut LoopTimers) {
        if self.handle_state_internal_action(event).await {
            return; // input action consumed the event, no state change
        }
        let old = self.state();
        let Some(next) = self.handle_state_transition(event) else {
            // Expected in normal operation (stale results after a transition,
            // duplicate webhooks, late timer fires) — hence debug, not warn.
            debug!(state = %old, event = %event,
                room = %self.params.livekit_room, lk_id = %self.params.livekit_identity,
                delay_id = %self.params.delay_id, job_id = %self.job_id,
                "Job: FSM event ignored in current state");
            return;
        };
        self.handle_state_exit_action(old, timers);
        self.handle_state_entry_action(next, event).await;
    }

    /// Runs the input action registered for the current state and the given
    /// event, if any. Returns whether the event was consumed.
    async fn handle_state_internal_action(self: &Arc<Self>, event: DelayedEventSignal) -> bool {
        let state = self.state();
        if !fsm_internal_actions().contains(&(state, event)) {
            return false;
        }
        debug!(state = %state, event = %event,
            room = %self.params.livekit_room, lk_id = %self.params.livekit_identity,
            delay_id = %self.params.delay_id, job_id = %self.job_id,
            "Job: FSM internal action");
        // The only registered internal action: (Connected, DelayedEventReset).
        self.start_delayed_event_restart().await;
        true
    }

    /// Determines and performs the state change for the given event. Unknown
    /// (state, event) pairs cause no change.
    fn handle_state_transition(&self, event: DelayedEventSignal) -> Option<DelayEventState> {
        let state = self.state();
        let next = fsm_transitions()
            .iter()
            .find(|(s, e, _)| *s == state && *e == event)
            .map(|(_, _, next)| *next)?;
        info!(from = %state, to = %next, event = %event,
            room = %self.params.livekit_room, lk_id = %self.params.livekit_identity,
            delay_id = %self.params.delay_id, job_id = %self.job_id,
            "Job: FSM transition");
        self.set_state(next);
        Some(next)
    }

    /// Runs the exit action of the state being left. Timer cleanup lives here
    /// — exit actions cover every outgoing path (including JobReplaced, which
    /// fires from any state), so no path can leak a timer.
    fn handle_state_exit_action(&self, state: DelayEventState, timers: &mut LoopTimers) {
        match state {
            DelayEventState::WaitingForInitialConnect => {
                // The waiting-state guard timer belongs to this state.
                if let Some(t) = timers.waiting.take() {
                    t.cancel();
                }
            }
            DelayEventState::Connected => {
                // The delayed-event timer belongs to this state; None encodes
                // "not running".
                if let Some(t) = timers.restart.take() {
                    t.cancel();
                }
            }
            _ => {}
        }
    }

    /// Runs the entry action of the state being entered.
    async fn handle_state_entry_action(
        self: &Arc<Self>,
        state: DelayEventState,
        event: DelayedEventSignal,
    ) {
        match state {
            DelayEventState::Connected => {
                // The deadline tracks the absolute time by which ActionSend
                // must complete; the full delay timeout is available until the
                // first successful restart extends it (via restart_result).
                self.set_restart_deadline(Instant::now() + self.params.delay_timeout);
                // Trigger the first reset right away so the homeserver timer
                // is synced immediately — we don't know how long elapsed
                // between submitting the delayed event to the homeserver and
                // handing over the delegation to this service. The
                // delayed-event timer itself is created lazily by the loop
                // once the first ActionRestart result arrives.
                tokio::select! {
                    _ = self.cancel.cancelled() => {}
                    _ = self.event_tx.send(DelayedEventSignal::DelayedEventReset) => {}
                }
            }

            DelayEventState::Disconnected => {
                // Clamp to [1 s, 1 h]
                //  - At least one second
                //     - so ActionSend gets a real attempt even when the
                //       deadline has passed or was never set (Disconnected
                //       reached straight from WaitingForInitialConnect)
                //     - a zero elapsed budget would mean "retry forever".
                //  - At most one hour: the sticky-event timeout; once the
                //    membership event has expired, retrying the leave-send is
                //    pointless.
                let until_deadline = self
                    .restart_deadline()
                    .map(|d| d.saturating_duration_since(Instant::now()))
                    .unwrap_or(Duration::ZERO);
                let remaining = until_deadline
                    .max(Duration::from_secs(1))
                    .min(Duration::from_secs(60 * 60));

                // ActionSend runs in a background task so the loop stays
                // responsive, retrying until `remaining` elapses — the leave
                // event keeps being attempted until the original delayed-event
                // timeout would have fired anyway. done_tx is only notified
                // AFTER ActionSend completes; notifying earlier would let
                // cleanup cancel the job and abort the send.
                let guard = self.background.add();
                let job = self.clone();
                tokio::spawn(async move {
                    let _guard = guard;

                    // Resolve the URL of the Client-Server API.
                    let cs_api_url = match (job.lookup_cs_api_url)(job.params.server_name.clone())
                        .await
                    {
                        Ok(url) => url,
                        Err(err) => {
                            warn!(state = %DelayEventState::Disconnected, event = %event,
                                room = %job.params.livekit_room, lk_id = %job.params.livekit_identity,
                                job_id = %job.job_id, err = %err,
                                "Job: ActionSend could not resolve Client-Server API URL");
                            tokio::select! {
                                _ = job.cancel.cancelled() => {}
                                _ = job.done_tx.send(job.clone()) => {}
                            }
                            return;
                        }
                    };

                    let result = retry(
                        &job.cancel,
                        ExponentialBackoff::service_default(),
                        remaining,
                        || {
                            let job = job.clone();
                            let cs_api_url = cs_api_url.clone();
                            async move {
                                job.deps
                                    .execute_delayed_event_action(
                                        &cs_api_url,
                                        &job.params.delay_id,
                                        DelayEventAction::Send,
                                    )
                                    .await
                            }
                        },
                    )
                    .await;

                    // Ok(404) means MSC4140 already-sent; Ok(200/204) is
                    // silent success.
                    match &result {
                        Err(RetryError::Error(err)) => {
                            warn!(state = %DelayEventState::Disconnected, event = %event,
                                room = %job.params.livekit_room, lk_id = %job.params.livekit_identity,
                                job_id = %job.job_id, status = err.status(), err = %err,
                                "Job: ActionSend failed");
                        }
                        Err(RetryError::Cancelled) => {}
                        Ok(404) => {
                            info!(state = %DelayEventState::Disconnected, event = %event,
                                room = %job.params.livekit_room, lk_id = %job.params.livekit_identity,
                                job_id = %job.job_id,
                                "Job: ActionSend — delayed event already sent or cancelled");
                        }
                        Ok(_) => {}
                    }

                    // Notify the handler only after ActionSend has completed
                    // (or timed out). This is intentional: the handler must
                    // not cancel the job before ActionSend finishes, or the
                    // leave event would never be sent.
                    tokio::select! {
                        // Cancelled externally (e.g. handler shutdown) before
                        // ActionSend completed — the teardown path handles
                        // cleanup.
                        _ = job.cancel.cancelled() => {}
                        _ = job.done_tx.send(job.clone()) => {}
                    }
                });
            }

            DelayEventState::Aborted => {
                // Timers are stopped by the exit actions of the states that
                // own them. Notify the handler immediately (same pattern as
                // Disconnected).
                tokio::select! {
                    _ = self.cancel.cancelled() => {}
                    _ = self.done_tx.send(self.clone()) => {}
                }
            }

            _ => {}
        }
    }

    /// Restarts the delayed event on the homeserver (ActionRestart) in a
    /// background task tracked by the background wait group, retrying with
    /// exponential backoff for as long as the restart deadline allows.
    /// It reports exactly one outcome — unless the job shuts down first;
    /// every send is cancellation-guarded:
    ///
    ///   - success: new deadline on the restart_result channel, captured when
    ///     ActionRestart succeeds (not when the loop reads it) so loop latency
    ///     cannot shrink the ActionSend window
    ///   - client-server API resolution error: CsApiUrlNotFound on the event
    ///     channel
    ///   - delayed event gone on the homeserver (404): DelayedEventNotFound
    ///     on the event channel
    ///   - any other failure, including an already-exhausted deadline:
    ///     DelayedEventTimedOut on the event channel
    pub(crate) async fn start_delayed_event_restart(self: &Arc<Self>) {
        let remaining = self
            .restart_deadline()
            .map(|d| d.saturating_duration_since(Instant::now()))
            .unwrap_or(Duration::ZERO);
        if remaining.is_zero() {
            info!(room = %self.params.livekit_room, lk_id = %self.params.livekit_identity,
                delay_id = %self.params.delay_id, job_id = %self.job_id,
                "Job: ActionRestart deadline exhausted — emitting DelayedEventTimedOut");
            tokio::select! {
                _ = self.cancel.cancelled() => {}
                _ = self.event_tx.send(DelayedEventSignal::DelayedEventTimedOut) => {}
            }
            return;
        }

        // Issue the restart call in a background task so the loop stays
        // responsive. On success the task sends the new deadline to the
        // restart_result channel; the loop reads it and creates/re-arms the
        // restart timer.
        let guard = self.background.add();
        let job = self.clone();
        tokio::spawn(async move {
            let _guard = guard;

            // Resolve the URL of the Client-Server API.
            let cs_api_url = match (job.lookup_cs_api_url)(job.params.server_name.clone()).await {
                Ok(url) => url,
                Err(err) => {
                    info!(room = %job.params.livekit_room, lk_id = %job.params.livekit_identity,
                        delay_id = %job.params.delay_id, job_id = %job.job_id, err = %err,
                        "Job: ActionRestart could not resolve Client-Server API URL — emitting CsApiUrlNotFound");
                    tokio::select! {
                        _ = job.cancel.cancelled() => {}
                        _ = job.event_tx.send(DelayedEventSignal::CsApiUrlNotFound) => {}
                    }
                    return;
                }
            };

            let result = retry(
                &job.cancel,
                ExponentialBackoff::service_default(),
                remaining,
                || {
                    let job = job.clone();
                    let cs_api_url = cs_api_url.clone();
                    async move {
                        job.deps
                            .execute_delayed_event_action(
                                &cs_api_url,
                                &job.params.delay_id,
                                DelayEventAction::Restart,
                            )
                            .await
                    }
                },
            )
            .await;

            // DelayedEventNotFound is the only failure treated specially
            // (the event is gone, not a transient blip); everything else
            // falls into the generic timed-out bucket.
            let signal = match result {
                Err(RetryError::Error(err)) if err.is_delayed_event_not_found() => {
                    warn!(room = %job.params.livekit_room, lk_id = %job.params.livekit_identity,
                        job_id = %job.job_id,
                        "Job: ActionRestart not found — emitting DelayedEventNotFound");
                    DelayedEventSignal::DelayedEventNotFound
                }
                Err(RetryError::Error(err)) => {
                    warn!(room = %job.params.livekit_room, lk_id = %job.params.livekit_identity,
                        job_id = %job.job_id, status = err.status(), err = %err,
                        "Job: ActionRestart failed — emitting DelayedEventTimedOut");
                    DelayedEventSignal::DelayedEventTimedOut
                }
                Err(RetryError::Cancelled) => {
                    // The job shut down mid-retry — nothing to report.
                    return;
                }
                Ok(_) => {
                    let restarted_at = SystemTime::now();
                    let new_deadline = Instant::now() + job.params.delay_timeout;

                    // Report success so the stored job gets updated.
                    tokio::select! {
                        _ = job.cancel.cancelled() => return,
                        _ = job.restarted_tx.send(JobRestartedRequest {
                            job: job.clone(),
                            restarted_at,
                        }) => {}
                    }

                    // Hand the new deadline to the loop, which extends the
                    // deadline and re-arms the restart timer.
                    debug!(room = %job.params.livekit_room, lk_id = %job.params.livekit_identity,
                        next_reset_in = ?job.delay_restart_duration(), "Job: ActionRestart ok");
                    tokio::select! {
                        _ = job.cancel.cancelled() => {}
                        _ = job.restart_result_tx.send(new_deadline) => {}
                    }
                    return;
                }
            };

            tokio::select! {
                _ = job.cancel.cancelled() => {}
                _ = job.event_tx.send(signal) => {}
            }
        });
    }
}

// ── FSM tables ───────────────────────────────────────────────────────────────

/// Maps state × event to the next state. Pairs not listed here cause no state
/// change (and therefore no exit/entry actions) — this also makes stale events
/// after a transition harmless, e.g. a second DelayedEventTimedOut arriving
/// once the job is already Disconnected.
pub(crate) fn fsm_transitions() -> &'static [(DelayEventState, DelayedEventSignal, DelayEventState)]
{
    use DelayEventState::*;
    use DelayedEventSignal::*;
    &[
        (WaitingForInitialConnect, ParticipantConnected, Connected),
        (
            WaitingForInitialConnect,
            ParticipantLookupSuccessful,
            Connected,
        ),
        (
            WaitingForInitialConnect,
            ParticipantConnectionAborted,
            Aborted,
        ),
        (WaitingForInitialConnect, WaitingStateTimedOut, Disconnected),
        (WaitingForInitialConnect, JobReplaced, Replaced),
        (
            Connected,
            ParticipantDisconnectedIntentionally,
            Disconnected,
        ),
        (Connected, ParticipantConnectionAborted, Disconnected),
        (Connected, DelayedEventTimedOut, Aborted),
        (Connected, DelayedEventNotFound, Aborted),
        (Connected, CsApiUrlNotFound, Aborted),
        (Connected, SfuParticipantGone, Disconnected),
        (Connected, JobReplaced, Replaced),
        (Disconnected, JobReplaced, Replaced),
        (Aborted, JobReplaced, Replaced),
    ]
}

/// Maps state × event to an input action that runs without leaving the state
/// — no exit/entry actions fire. The only registered action is
/// (Connected, DelayedEventReset) → start_delayed_event_restart.
pub(crate) fn fsm_internal_actions() -> &'static [(DelayEventState, DelayedEventSignal)] {
    &[(
        DelayEventState::Connected,
        DelayedEventSignal::DelayedEventReset,
    )]
}

// ─────────────────────────────────────────────────────────────────────────────

#[cfg(test)]
pub(crate) mod test_support {
    use super::*;
    use crate::helper::ActionError;

    type ExecFn =
        Box<dyn Fn(&CsApiUrl, &str, DelayEventAction) -> Result<u16, ActionError> + Send + Sync>;
    type ExistsFn = Box<
        dyn Fn(&LiveKitRoomAlias, &LiveKitIdentity) -> BoxFuture<'static, Result<bool, String>>
            + Send
            + Sync,
    >;

    /// A [`Deps`] implementation for job tests with swappable behaviours.
    pub(crate) struct TestJobDeps {
        pub exec: ExecFn,
        pub exists: ExistsFn,
    }

    impl Default for TestJobDeps {
        fn default() -> Self {
            Self {
                exec: Box::new(|_, _, _| Ok(200)),
                // Pends forever; lookups resolve via cancellation.
                exists: Box::new(|_, _| Box::pin(std::future::pending())),
            }
        }
    }

    #[async_trait::async_trait]
    impl Deps for TestJobDeps {
        async fn participant_exists(
            &self,
            _lk_auth: &LiveKitAuth,
            room: &LiveKitRoomAlias,
            identity: &LiveKitIdentity,
        ) -> Result<bool, String> {
            (self.exists)(room, identity).await
        }

        async fn execute_delayed_event_action(
            &self,
            cs_api_url: &CsApiUrl,
            delay_id: &str,
            action: DelayEventAction,
        ) -> Result<u16, ActionError> {
            (self.exec)(cs_api_url, delay_id, action)
        }
    }

    /// A deps whose exec always returns 200 OK.
    pub(crate) fn mock_exec_ok() -> Arc<TestJobDeps> {
        Arc::new(TestJobDeps::default())
    }

    /// Resolves the CS-API URL only for the given server name.
    pub(crate) fn lookup_cs_api_url_from_override_only(
        server_name_override: &str,
        url_override: &str,
    ) -> LookupCsApiUrlFn {
        let server_name_override = server_name_override.to_owned();
        let url_override = url_override.to_owned();
        Arc::new(move |server_name| {
            let server_name_override = server_name_override.clone();
            let url_override = url_override.clone();
            Box::pin(async move {
                if server_name == server_name_override {
                    Ok(CsApiUrl(url_override))
                } else {
                    Err(format!(
                        "trying to resolve CS-API URL for unexpected server name {server_name}"
                    ))
                }
            })
        })
    }

    /// A lookup function that always fails (for CsApiUrlNotFound paths).
    pub(crate) fn lookup_cs_api_url_failing(message: &str) -> LookupCsApiUrlFn {
        let message = message.to_owned();
        Arc::new(move |_| {
            let message = message.clone();
            Box::pin(async move { Err(message) })
        })
    }
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::AtomicI32;

    use super::test_support::*;
    use super::*;
    use crate::helper::ActionError;

    // ── test helpers ──────────────────────────────────────────────────────────

    /// Creates a DelayedEventJob wired to buffered done channels.
    fn new_test_job(deps: Arc<dyn Deps>, timeout: Duration) -> Arc<DelayedEventJob> {
        let timeout = if timeout.is_zero() {
            Duration::from_secs(10)
        } else {
            timeout
        };
        let (done_tx, _done_rx) = mpsc::channel(20);
        let (restarted_tx, _restarted_rx) = mpsc::channel(20);
        // Leak the receivers so the channels stay open but unread.
        std::mem::forget(_done_rx);
        std::mem::forget(_restarted_rx);
        DelayedEventJob::new(
            &CancellationToken::new(),
            DelayedEventJobParams {
                server_name: "example.com".into(),
                delay_id: "test-delay-id".into(),
                delay_timeout: timeout,
                livekit_room: LiveKitRoomAlias("test-room".into()),
                livekit_identity: LiveKitIdentity("@test:example.com".into()),
            },
            deps,
            lookup_cs_api_url_from_override_only(
                "example.com",
                "https://matrix-client.example.com",
            ),
            done_tx,
            restarted_tx,
        )
        .expect("DelayedEventJob::new")
    }

    /// Starts the loop and sends events sequentially. The timeout bounds a
    /// send that would otherwise block on a full event channel.
    async fn drive_job_events(job: &Arc<DelayedEventJob>, events: &[DelayedEventSignal]) {
        job.spawn_loop();
        for ev in events {
            tokio::time::timeout(Duration::from_secs(1), job.event_tx.send(*ev))
                .await
                .unwrap_or_else(|_| panic!("timed out sending event {ev}"))
                .expect("event channel closed");
            // Let the event settle: "whatever comes next" can act on the
            // changes this event produced, not race past it.
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    }

    /// Convenience wrapper that creates a job and returns both the job and
    /// the done channel so tests can observe terminal signals.
    fn new_job_with_done_ch(
        deps: Arc<dyn Deps>,
        timeout: Duration,
    ) -> (Arc<DelayedEventJob>, mpsc::Receiver<Arc<DelayedEventJob>>) {
        new_job_with_done_ch_and_lookup(
            deps,
            timeout,
            lookup_cs_api_url_from_override_only(
                "example.com",
                "https://matrix-client.example.com",
            ),
        )
    }

    fn new_job_with_done_ch_and_lookup(
        deps: Arc<dyn Deps>,
        timeout: Duration,
        lookup: LookupCsApiUrlFn,
    ) -> (Arc<DelayedEventJob>, mpsc::Receiver<Arc<DelayedEventJob>>) {
        let (done_tx, done_rx) = mpsc::channel(5);
        let (restarted_tx, restarted_rx) = mpsc::channel(20);
        std::mem::forget(restarted_rx);
        let job = DelayedEventJob::new(
            &CancellationToken::new(),
            DelayedEventJobParams {
                server_name: "example.com".into(),
                delay_id: "id".into(),
                delay_timeout: timeout,
                livekit_room: LiveKitRoomAlias("room".into()),
                livekit_identity: LiveKitIdentity("identity".into()),
            },
            deps,
            lookup,
            done_tx,
            restarted_tx,
        )
        .expect("DelayedEventJob::new");
        (job, done_rx)
    }

    async fn expect_done_state(
        done_rx: &mut mpsc::Receiver<Arc<DelayedEventJob>>,
        want: DelayEventState,
        wait: Duration,
        what: &str,
    ) {
        match tokio::time::timeout(wait, done_rx.recv()).await {
            Ok(Some(done_job)) => {
                assert_eq!(done_job.state(), want, "{what}: unexpected state");
            }
            Ok(None) => panic!("{what}: done channel closed"),
            Err(_) => panic!("timed out waiting for {what}"),
        }
    }

    // ── construction ──────────────────────────────────────────────────────────

    /// Verifies that a zero timeout is rejected.
    #[tokio::test]
    async fn test_delayed_event_job_invalid_timeout() {
        let (done_tx, _done_rx) = mpsc::channel(1);
        let (restarted_tx, _restarted_rx) = mpsc::channel(1);
        let result = DelayedEventJob::new(
            &CancellationToken::new(),
            DelayedEventJobParams {
                server_name: "example.com".into(),
                delay_id: "id".into(),
                delay_timeout: Duration::ZERO,
                livekit_room: LiveKitRoomAlias("room".into()),
                livekit_identity: LiveKitIdentity("identity".into()),
            },
            mock_exec_ok(),
            lookup_cs_api_url_from_override_only("x", "y"),
            done_tx,
            restarted_tx,
        );
        assert!(result.is_err(), "expected error for zero timeout, got Ok");
    }

    /// Verifies that Display returns a non-empty description that includes
    /// the type name.
    #[tokio::test]
    async fn test_delayed_event_job_string() {
        let job = new_test_job(mock_exec_ok(), Duration::from_secs(10));
        let s = job.to_string();
        assert!(
            !s.is_empty() && s.contains("DelayedEventJob"),
            "to_string() = {s:?}, want non-empty string containing 'DelayedEventJob'"
        );
        job.stop(); // the loop was never started — close() would time out
    }

    // ── FSM: table invariants ─────────────────────────────────────────────────

    /// Asserts the FSM dispatch invariant: no (state, event) pair may be
    /// registered both as an internal action and as a transition — otherwise
    /// the internal-first dispatch order in handle_event would silently
    /// shadow the transition.
    #[test]
    fn test_fsm_tables_internal_and_transition_disjoint() {
        for (state, event) in fsm_internal_actions() {
            assert!(
                !fsm_transitions()
                    .iter()
                    .any(|(s, e, _)| s == state && e == event),
                "FSM: ({state}, {event}) registered as both internal action and transition"
            );
        }
    }

    // ── FSM: WaitingForInitialConnect transitions ─────────────────────────────

    /// Verifies WaitingForInitialConnect → Connected via ParticipantConnected.
    #[tokio::test]
    async fn test_delayed_event_job_participant_connected() {
        let job = new_test_job(mock_exec_ok(), Duration::from_secs(10));
        drive_job_events(&job, &[DelayedEventSignal::ParticipantConnected]).await;
        job.close().await.expect("close");
        assert_eq!(job.state(), DelayEventState::Connected);
    }

    /// Verifies WaitingForInitialConnect → Connected via
    /// ParticipantLookupSuccessful.
    #[tokio::test]
    async fn test_delayed_event_job_participant_lookup_successful() {
        let job = new_test_job(mock_exec_ok(), Duration::from_secs(10));
        drive_job_events(&job, &[DelayedEventSignal::ParticipantLookupSuccessful]).await;
        job.close().await.expect("close");
        assert_eq!(job.state(), DelayEventState::Connected);
    }

    /// Verifies WaitingForInitialConnect → Aborted via
    /// ParticipantConnectionAborted, and that the done channel receives the
    /// job.
    #[tokio::test]
    async fn test_delayed_event_job_connection_aborted() {
        let (job, mut done_rx) = new_job_with_done_ch(mock_exec_ok(), Duration::from_secs(10));
        job.spawn_loop();

        job.event_tx
            .send(DelayedEventSignal::ParticipantConnectionAborted)
            .await
            .unwrap();

        expect_done_state(
            &mut done_rx,
            DelayEventState::Aborted,
            Duration::from_secs(3),
            "Aborted signal on done channel",
        )
        .await;
        job.close().await.expect("close");
    }

    /// Verifies that a short delay timeout causes the waiting-state timer to
    /// fire and transition WaitingForInitialConnect → Disconnected
    /// automatically.
    #[tokio::test]
    async fn test_delayed_event_job_waiting_state_timed_out() {
        let (job, mut done_rx) = new_job_with_done_ch(mock_exec_ok(), Duration::from_millis(50)); // fires quickly
        job.spawn_loop();

        expect_done_state(
            &mut done_rx,
            DelayEventState::Disconnected,
            Duration::from_secs(5),
            "WaitingStateTimedOut → Disconnected",
        )
        .await;
        job.close().await.expect("close");
    }

    // ── FSM: Connected transitions ────────────────────────────────────────────

    /// Verifies Connected → Disconnected via
    /// ParticipantDisconnectedIntentionally, and that the done channel
    /// receives the job with state Disconnected.
    #[tokio::test]
    async fn test_delayed_event_job_participant_disconnected() {
        let (job, mut done_rx) = new_job_with_done_ch(mock_exec_ok(), Duration::from_secs(10));
        job.spawn_loop();

        job.event_tx
            .send(DelayedEventSignal::ParticipantConnected)
            .await
            .unwrap();
        tokio::time::sleep(Duration::from_millis(20)).await;
        job.event_tx
            .send(DelayedEventSignal::ParticipantDisconnectedIntentionally)
            .await
            .unwrap();

        expect_done_state(
            &mut done_rx,
            DelayEventState::Disconnected,
            Duration::from_secs(3),
            "Disconnected signal on done channel",
        )
        .await;
        job.close().await.expect("close");
    }

    #[tokio::test]
    async fn test_delayed_event_job_delayed_event_timed_out() {
        let (job, mut done_rx) = new_job_with_done_ch(mock_exec_ok(), Duration::from_secs(1));

        // Prime the channel with our test events before starting the job's
        // loop. This prevents the events added by the job itself from
        // interfering with the test. They'll simply be discarded in the end.
        job.event_tx
            .send(DelayedEventSignal::ParticipantConnected)
            .await
            .unwrap();
        job.event_tx
            .send(DelayedEventSignal::DelayedEventTimedOut)
            .await
            .unwrap();

        // Have the job process the events.
        job.spawn_loop();

        // Wait for the job to abort.
        expect_done_state(
            &mut done_rx,
            DelayEventState::Aborted,
            Duration::from_secs(2),
            "done channel",
        )
        .await;
    }

    #[tokio::test]
    async fn test_delayed_event_job_delayed_event_not_found() {
        let (job, mut done_rx) = new_job_with_done_ch(mock_exec_ok(), Duration::from_secs(1));

        // Prime the channel with our test events before starting the job's
        // loop.
        job.event_tx
            .send(DelayedEventSignal::ParticipantConnected)
            .await
            .unwrap();
        job.event_tx
            .send(DelayedEventSignal::DelayedEventNotFound)
            .await
            .unwrap();

        job.spawn_loop();

        expect_done_state(
            &mut done_rx,
            DelayEventState::Aborted,
            Duration::from_secs(2),
            "done channel",
        )
        .await;
    }

    #[tokio::test]
    async fn test_delayed_event_job_cs_api_url_not_found() {
        let (job, mut done_rx) = new_job_with_done_ch(mock_exec_ok(), Duration::from_secs(1));

        // Prime the channel with our test events before starting the job's
        // loop.
        job.event_tx
            .send(DelayedEventSignal::ParticipantConnected)
            .await
            .unwrap();
        job.event_tx
            .send(DelayedEventSignal::CsApiUrlNotFound)
            .await
            .unwrap();

        job.spawn_loop();

        expect_done_state(
            &mut done_rx,
            DelayEventState::Aborted,
            Duration::from_secs(2),
            "done channel",
        )
        .await;
    }

    /// Verifies the start_delayed_event_restart contract for an
    /// already-expired deadline: no homeserver call is made and
    /// DelayedEventTimedOut is emitted instead. The method is called directly
    /// — the loop is not running — so the test is fully deterministic.
    #[tokio::test]
    async fn test_delayed_event_job_reset_with_exhausted_deadline() {
        let calls = Arc::new(AtomicI32::new(0));
        let calls_clone = calls.clone();
        let deps = Arc::new(TestJobDeps {
            exec: Box::new(move |cs_api_url, _, _| {
                assert_eq!(
                    cs_api_url.as_str(),
                    "https://matrix-client.example.com",
                    "got unexpected client-server API URL"
                );
                calls_clone.fetch_add(1, Ordering::SeqCst);
                Ok(200)
            }),
            ..TestJobDeps::default()
        });

        let job = new_test_job(deps, Duration::from_secs(10));
        job.set_restart_deadline(Instant::now() - Duration::from_secs(1)); // already expired

        job.start_delayed_event_restart().await;

        let mut event_rx = job.event_rx.lock().unwrap().take().unwrap();
        match tokio::time::timeout(Duration::from_secs(1), event_rx.recv()).await {
            Ok(Some(ev)) => assert_eq!(
                ev,
                DelayedEventSignal::DelayedEventTimedOut,
                "expected DelayedEventTimedOut"
            ),
            _ => panic!("timed out waiting for DelayedEventTimedOut"),
        }
        assert_eq!(
            calls.load(Ordering::SeqCst),
            0,
            "expected no homeserver call"
        );
        job.stop(); // the loop was never started — close() would time out
    }

    #[tokio::test]
    async fn test_delayed_event_job_reset_with_failing_cs_api_url_resolution() {
        let (job, _done_rx) = new_job_with_done_ch_and_lookup(
            mock_exec_ok(),
            Duration::from_secs(10),
            lookup_cs_api_url_failing("M_NOT_FOUND"),
        );
        job.set_restart_deadline(Instant::now() + Duration::from_secs(1));

        job.start_delayed_event_restart().await;

        let mut event_rx = job.event_rx.lock().unwrap().take().unwrap();
        match tokio::time::timeout(Duration::from_secs(1), event_rx.recv()).await {
            Ok(Some(ev)) => assert_eq!(
                ev,
                DelayedEventSignal::CsApiUrlNotFound,
                "expected CsApiUrlNotFound"
            ),
            _ => panic!("timed out waiting for CsApiUrlNotFound"),
        }
        job.stop();
    }

    /// Verifies that when Disconnected is reached without a restart deadline
    /// (waiting state timed out before any connect), ActionSend is bounded by
    /// the one-second floor — even against a failing homeserver — and still
    /// notifies the done channel instead of retrying forever.
    #[tokio::test]
    async fn test_delayed_event_job_action_send_bounded_when_no_time_remains() {
        let calls = Arc::new(AtomicI32::new(0));
        let calls_clone = calls.clone();
        let deps = Arc::new(TestJobDeps {
            exec: Box::new(move |cs_api_url, _, _| {
                calls_clone.fetch_add(1, Ordering::SeqCst);
                assert_eq!(
                    cs_api_url.as_str(),
                    "https://matrix-client.example.com",
                    "got unexpected client-server API URL"
                );
                Err(ActionError::Transient {
                    status: 500,
                    msg: "homeserver down".into(),
                })
            }),
            ..TestJobDeps::default()
        });

        let (job, mut done_rx) = new_job_with_done_ch(deps, Duration::from_millis(50)); // waiting timer fires quickly
        job.spawn_loop();

        expect_done_state(
            &mut done_rx,
            DelayEventState::Disconnected,
            Duration::from_secs(5),
            "done channel — ActionSend may be retrying forever",
        )
        .await;
        // The one-second window allows 1-2 attempts (randomized first backoff
        // interval); anything more means the elapsed-time bound is not
        // applied.
        let n = calls.load(Ordering::SeqCst);
        assert!(
            (1..=2).contains(&n),
            "expected 1-2 ActionSend attempts, got {n}"
        );
        job.close().await.expect("close");
    }

    #[tokio::test]
    async fn test_delayed_event_job_action_send_with_failing_cs_api_url_resolution() {
        let calls = Arc::new(AtomicI32::new(0));
        let calls_clone = calls.clone();
        let deps = Arc::new(TestJobDeps {
            exec: Box::new(move |_, _, _| {
                calls_clone.fetch_add(1, Ordering::SeqCst);
                Err(ActionError::Transient {
                    status: 500,
                    msg: "homeserver down".into(),
                })
            }),
            ..TestJobDeps::default()
        });

        let (job, mut done_rx) = new_job_with_done_ch_and_lookup(
            deps,
            Duration::from_millis(50), // waiting timer fires quickly
            lookup_cs_api_url_failing("M_NOT_FOUND"),
        );
        job.spawn_loop();

        expect_done_state(
            &mut done_rx,
            DelayEventState::Disconnected,
            Duration::from_secs(5),
            "done channel — ActionSend may be retrying forever",
        )
        .await;
        assert_eq!(
            calls.load(Ordering::SeqCst),
            0,
            "expected no ActionSend attempts"
        );
        job.close().await.expect("close");
    }

    // ── FSM: guard conditions ─────────────────────────────────────────────────

    /// Verifies every (state, event) pair that is registered neither as a
    /// transition nor as an internal action: the event must be ignored and
    /// the state stay unchanged. Each pair runs against a fresh job, so every
    /// no-op is verified individually — not just the sum of several no-ops.
    #[tokio::test]
    async fn test_delayed_event_job_fsm_ignores_wrong_state_transitions() {
        use DelayEventState::*;
        use DelayedEventSignal::*;

        // Event sequences that drive a fresh job into each state.
        let state_paths: Vec<(DelayEventState, Vec<DelayedEventSignal>)> = vec![
            (WaitingForInitialConnect, vec![]),
            (Connected, vec![ParticipantConnected]),
            (
                Disconnected,
                vec![ParticipantConnected, ParticipantDisconnectedIntentionally],
            ),
            (Aborted, vec![ParticipantConnectionAborted]),
            (Replaced, vec![JobReplaced]),
        ];
        let all_events = [
            ParticipantConnected,
            ParticipantLookupSuccessful,
            ParticipantDisconnectedIntentionally,
            ParticipantConnectionAborted,
            DelayedEventReset,
            DelayedEventTimedOut,
            DelayedEventNotFound,
            WaitingStateTimedOut,
            SfuNotAvailable,
            JobReplaced,
            SfuParticipantGone,
        ];

        for (state, path) in &state_paths {
            for event in all_events {
                if fsm_transitions()
                    .iter()
                    .any(|(s, e, _)| s == state && e == &event)
                {
                    continue; // legal transition — covered by dedicated tests
                }
                if fsm_internal_actions().contains(&(*state, event)) {
                    continue; // internal action — not a no-op
                }
                let job = new_test_job(mock_exec_ok(), Duration::from_secs(10));
                let mut events = path.clone();
                events.push(event);
                drive_job_events(&job, &events).await;
                job.close()
                    .await
                    .unwrap_or_else(|e| panic!("{event} in {state}: close: {e}"));
                assert_eq!(
                    job.state(),
                    *state,
                    "expected state {state} after {event}, got {}",
                    job.state()
                );
            }
        }
    }

    // ── FSM: ActionRestart outcomes ───────────────────────────────────────────

    #[tokio::test]
    async fn test_delayed_event_job_action_restart_cs_api_url_not_found() {
        let lookup: LookupCsApiUrlFn = Arc::new(|server_name| {
            Box::pin(async move {
                assert_eq!(
                    server_name, "example.com",
                    "trying to resolve unexpected server name"
                );
                Err("M_NOT_FOUND: no".to_owned())
            })
        });
        let (job, mut done_rx) =
            new_job_with_done_ch_and_lookup(mock_exec_ok(), Duration::from_secs(10), lookup);
        job.spawn_loop();

        // Connected triggers an immediate DelayedEventReset.
        // The reset task fails to resolve the CS API URL → CsApiUrlNotFound →
        // Aborted.
        job.event_tx
            .send(DelayedEventSignal::ParticipantConnected)
            .await
            .unwrap();

        expect_done_state(
            &mut done_rx,
            DelayEventState::Aborted,
            Duration::from_secs(5),
            "Aborted after ActionRestart 404",
        )
        .await;
        job.close().await.expect("close");
    }

    #[tokio::test]
    async fn test_delayed_event_job_action_restart_404() {
        let deps = Arc::new(TestJobDeps {
            exec: Box::new(|cs_api_url, _, action| {
                assert_eq!(
                    cs_api_url.as_str(),
                    "https://matrix-client.example.com",
                    "got unexpected client-server API URL"
                );
                if action == DelayEventAction::Restart {
                    return Err(ActionError::DelayedEventNotFound { status: 404 });
                }
                Ok(200)
            }),
            ..TestJobDeps::default()
        });
        let (job, mut done_rx) = new_job_with_done_ch(deps, Duration::from_secs(10));
        job.spawn_loop();

        // Connected triggers an immediate DelayedEventReset.
        // The reset task gets 404 → DelayedEventNotFound → Aborted.
        job.event_tx
            .send(DelayedEventSignal::ParticipantConnected)
            .await
            .unwrap();

        expect_done_state(
            &mut done_rx,
            DelayEventState::Aborted,
            Duration::from_secs(5),
            "Aborted after ActionRestart 404",
        )
        .await;
        job.close().await.expect("close");
    }

    #[tokio::test]
    async fn test_delayed_event_job_action_restart_error() {
        let deps = Arc::new(TestJobDeps {
            exec: Box::new(|cs_api_url, _, action| {
                assert_eq!(
                    cs_api_url.as_str(),
                    "https://matrix-client.example.com",
                    "got unexpected client-server API URL"
                );
                if action == DelayEventAction::Restart {
                    return Err(ActionError::Transient {
                        status: 0,
                        msg: "context deadline exceeded".into(),
                    });
                }
                Ok(200)
            }),
            ..TestJobDeps::default()
        });

        // Short timeout so the retry loop gives up quickly after the error.
        let (job, mut done_rx) = new_job_with_done_ch(deps, Duration::from_millis(200));
        job.spawn_loop();

        job.event_tx
            .send(DelayedEventSignal::ParticipantConnected)
            .await
            .unwrap();

        expect_done_state(
            &mut done_rx,
            DelayEventState::Aborted,
            Duration::from_secs(5),
            "Aborted after ActionRestart error",
        )
        .await;
        job.close().await.expect("close");
    }

    /// Starting the loop twice is a programming error and panics.
    #[tokio::test]
    #[should_panic(expected = "loop started twice")]
    async fn test_delayed_event_job_spawn_loop_twice_panics() {
        let job = new_test_job(mock_exec_ok(), Duration::from_secs(10));
        job.spawn_loop();
        job.spawn_loop();
    }

    // ── Display impls ─────────────────────────────────────────────────────────

    #[test]
    fn test_delay_event_state_string() {
        for (state, want) in [
            (
                DelayEventState::WaitingForInitialConnect,
                "WaitingForInitialConnect",
            ),
            (DelayEventState::Connected, "Connected"),
            (DelayEventState::Disconnected, "Disconnected"),
            (DelayEventState::Aborted, "Aborted"),
            (DelayEventState::Replaced, "Replaced"),
        ] {
            assert_eq!(state.to_string(), want);
        }
    }

    #[test]
    fn test_delayed_event_signal_string() {
        for (sig, want) in [
            (
                DelayedEventSignal::ParticipantConnected,
                "ParticipantConnected",
            ),
            (
                DelayedEventSignal::ParticipantLookupSuccessful,
                "ParticipantLookupSuccessful",
            ),
            (
                DelayedEventSignal::ParticipantDisconnectedIntentionally,
                "ParticipantDisconnectedIntentionally",
            ),
            (
                DelayedEventSignal::ParticipantConnectionAborted,
                "ParticipantConnectionAborted",
            ),
            (DelayedEventSignal::DelayedEventReset, "DelayedEventReset"),
            (
                DelayedEventSignal::DelayedEventTimedOut,
                "DelayedEventTimedOut",
            ),
            (
                DelayedEventSignal::DelayedEventNotFound,
                "DelayedEventNotFound",
            ),
            (
                DelayedEventSignal::WaitingStateTimedOut,
                "WaitingStateTimedOut",
            ),
            (DelayedEventSignal::SfuNotAvailable, "SFUNotAvailable"),
            (DelayedEventSignal::JobReplaced, "JobReplaced"),
            (DelayedEventSignal::SfuParticipantGone, "SFUParticipantGone"),
        ] {
            assert_eq!(sig.to_string(), want);
        }
    }

    // ── JobReplaced signal ────────────────────────────────────────────────────

    /// Verifies the normal path: when JobReplaced is sent on the event
    /// channel while the loop is running, the job transitions to Replaced
    /// state and then exits cleanly via stop().
    #[tokio::test]
    async fn test_delayed_event_job_job_replaced_signal_received() {
        let job = new_test_job(mock_exec_ok(), Duration::from_secs(10));
        job.spawn_loop();

        // Send JobReplaced — the loop processes it and sets state = Replaced.
        tokio::time::timeout(
            Duration::from_secs(1),
            job.event_tx.send(DelayedEventSignal::JobReplaced),
        )
        .await
        .expect("timed out sending JobReplaced")
        .unwrap();

        // Small delay so the loop can process the signal before we check the
        // state.
        tokio::time::sleep(Duration::from_millis(20)).await;

        // Now cancel and close — the loop should exit promptly.
        job.stop();
        tokio::time::timeout(Duration::from_secs(3), job.close())
            .await
            .expect("job did not close in time after JobReplaced")
            .expect("close");
        assert_eq!(job.state(), DelayEventState::Replaced);
    }

    /// Verifies the drop path of the non-blocking JobReplaced send: when the
    /// event channel is full the signal is dropped, but the job must still be
    /// cancelled and closed cleanly — no deadlock, no task leak.
    #[tokio::test]
    async fn test_delayed_event_job_job_replaced_full_channel() {
        // Create a job but do NOT start the loop yet, so the event channel
        // fills up.
        let job = new_test_job(mock_exec_ok(), Duration::from_secs(10));

        // Fill the event channel to capacity (buffer = 10) with no-op signals.
        for _ in 0..job.event_tx.max_capacity() {
            job.event_tx
                .try_send(DelayedEventSignal::SfuNotAvailable)
                .unwrap();
        }

        // Now attempt the non-blocking JobReplaced send — must fail with Full.
        assert!(
            job.event_tx
                .try_send(DelayedEventSignal::JobReplaced)
                .is_err(),
            "expected JobReplaced to be dropped (full channel), but it was sent"
        );

        // Even without the signal, stop() + close() must complete cleanly.
        job.spawn_loop(); // start the loop so it can drain and exit
        job.stop();
        tokio::time::timeout(Duration::from_secs(3), job.close())
            .await
            .expect("job did not close cleanly after full-channel JobReplaced drop")
            .expect("close");
        // We are still in state WaitingForInitialConnect because the
        // JobReplaced signal was dropped, but that's fine — the key is that we
        // didn't deadlock and the loop exited cleanly.
        assert_eq!(job.state(), DelayEventState::WaitingForInitialConnect);
    }

    // ── Sanity check (SfuParticipantGone) ─────────────────────────────────────

    /// Verifies that SfuParticipantGone in the Connected state triggers →
    /// Disconnected.
    #[tokio::test]
    async fn test_delayed_event_job_sfu_participant_gone_connected() {
        let (job, mut done_rx) = new_job_with_done_ch(mock_exec_ok(), Duration::from_secs(10));
        job.spawn_loop();

        job.event_tx
            .send(DelayedEventSignal::ParticipantConnected)
            .await
            .unwrap();
        tokio::time::sleep(Duration::from_millis(20)).await;
        job.event_tx
            .send(DelayedEventSignal::SfuParticipantGone)
            .await
            .unwrap();

        expect_done_state(
            &mut done_rx,
            DelayEventState::Disconnected,
            Duration::from_secs(3),
            "Disconnected after SFUParticipantGone",
        )
        .await;
        job.close().await.expect("close");
    }

    // ── start_participant_lookup ──────────────────────────────────────────────

    /// Verifies that start_participant_lookup immediately calls
    /// participant_exists and delivers ParticipantLookupSuccessful to the job
    /// when the participant is present (sanity_interval == 0: one attempt
    /// only).
    #[tokio::test]
    async fn test_participant_lookup_phase1_finds_participant() {
        let (phase1_done_tx, mut phase1_done_rx) = mpsc::channel::<()>(1);
        let deps = Arc::new(TestJobDeps {
            exists: Box::new(move |_, _| {
                let _ = phase1_done_tx.try_send(());
                Box::pin(async { Ok(true) }) // participant present
            }),
            ..TestJobDeps::default()
        });

        let (done_tx, _done_rx) = mpsc::channel(5);
        let (restarted_tx, restarted_rx) = mpsc::channel(5);
        std::mem::forget(_done_rx);
        std::mem::forget(restarted_rx);
        let job = DelayedEventJob::new(
            &CancellationToken::new(),
            DelayedEventJobParams {
                server_name: "example.com".into(),
                delay_id: "phase1-id".into(),
                delay_timeout: Duration::from_secs(10),
                livekit_room: LiveKitRoomAlias("phase1-room".into()),
                livekit_identity: LiveKitIdentity("@alice:example.com".into()),
            },
            deps.clone(),
            lookup_cs_api_url_from_override_only(
                "example.com",
                "https://matrix-client.example.com",
            ),
            done_tx,
            restarted_tx,
        )
        .unwrap();
        job.spawn_loop();
        // sanity_interval == 0: one attempt only, no Phase 2.
        start_participant_lookup(
            &job,
            deps,
            LiveKitAuth {
                secret: "secret".into(),
                key: "devkey".into(),
                lk_url: "ws://127.0.0.1:7880".into(),
            },
            Duration::ZERO,
        );

        tokio::time::timeout(Duration::from_secs(5), phase1_done_rx.recv())
            .await
            .expect("timed out waiting for Phase 1 participant lookup call");

        job.close().await.expect("close");
    }

    /// Verifies the Phase 2 path: after Phase 1 confirms the participant, the
    /// periodic ticker fires a lookup that no longer includes the identity,
    /// causing start_participant_lookup to send SfuParticipantGone and the
    /// job to enter Disconnected.
    #[tokio::test]
    async fn test_participant_lookup_phase2_detects_gone_participant() {
        let call_count = Arc::new(AtomicI32::new(0));
        let call_count_clone = call_count.clone();
        let deps = Arc::new(TestJobDeps {
            exists: Box::new(move |_, _| {
                let n = call_count_clone.fetch_add(1, Ordering::SeqCst);
                Box::pin(async move {
                    if n == 0 {
                        Ok(true) // Phase 1: participant present.
                    } else {
                        Ok(false) // Phase 2: participant confirmed absent.
                    }
                })
            }),
            ..TestJobDeps::default()
        });

        const SANITY_INTERVAL: Duration = Duration::from_millis(50);
        let (done_tx, mut done_rx) = mpsc::channel(5);
        let (restarted_tx, restarted_rx) = mpsc::channel(5);
        std::mem::forget(restarted_rx);
        let job = DelayedEventJob::new(
            &CancellationToken::new(),
            DelayedEventJobParams {
                server_name: "example.com".into(),
                delay_id: "phase2-id".into(),
                delay_timeout: Duration::from_secs(10),
                livekit_room: LiveKitRoomAlias("phase2-room".into()),
                livekit_identity: LiveKitIdentity("@bob:example.com".into()),
            },
            deps.clone(),
            lookup_cs_api_url_from_override_only(
                "example.com",
                "https://matrix-client.example.com",
            ),
            done_tx,
            restarted_tx,
        )
        .unwrap();
        job.spawn_loop();
        start_participant_lookup(
            &job,
            deps,
            LiveKitAuth {
                secret: "secret".into(),
                key: "devkey".into(),
                lk_url: "ws://127.0.0.1:7880".into(),
            },
            SANITY_INTERVAL,
        );

        expect_done_state(
            &mut done_rx,
            DelayEventState::Disconnected,
            Duration::from_secs(5),
            "Disconnected after Phase 2 sanity check",
        )
        .await;
        job.close().await.expect("close");
    }

    /// Verifies that when sanity_interval is 0, start_participant_lookup
    /// makes exactly one lookup call (Phase 1) and then the task exits — no
    /// Phase 2 ticker fires.
    #[tokio::test]
    async fn test_participant_lookup_phase2_disabled() {
        // phase1_done fires on the first (and only) lookup call. call_count is
        // read only after job.close() ensures the lookup task has exited (the
        // background wait group), so there is no concurrent access.
        let (phase1_done_tx, mut phase1_done_rx) = mpsc::channel::<()>(1);
        let call_count = Arc::new(AtomicI32::new(0));
        let call_count_clone = call_count.clone();
        let deps = Arc::new(TestJobDeps {
            exists: Box::new(move |_, _| {
                call_count_clone.fetch_add(1, Ordering::SeqCst);
                let _ = phase1_done_tx.try_send(());
                Box::pin(async { Ok(true) }) // participant present
            }),
            ..TestJobDeps::default()
        });

        let (done_tx, _done_rx) = mpsc::channel(5);
        let (restarted_tx, restarted_rx) = mpsc::channel(5);
        std::mem::forget(_done_rx);
        std::mem::forget(restarted_rx);
        let job = DelayedEventJob::new(
            &CancellationToken::new(),
            DelayedEventJobParams {
                server_name: "example.com".into(),
                delay_id: "disabled-id".into(),
                delay_timeout: Duration::from_secs(10),
                livekit_room: LiveKitRoomAlias("phase2-disabled-room".into()),
                livekit_identity: LiveKitIdentity("@carol:example.com".into()),
            },
            deps.clone(),
            lookup_cs_api_url_from_override_only(
                "example.com",
                "https://matrix-client.example.com",
            ),
            done_tx,
            restarted_tx,
        )
        .unwrap();
        job.spawn_loop();
        // sanity_interval == 0 disables Phase 2.
        start_participant_lookup(
            &job,
            deps,
            LiveKitAuth {
                secret: "secret".into(),
                key: "devkey".into(),
                lk_url: "ws://127.0.0.1:7880".into(),
            },
            Duration::ZERO,
        );

        // Wait for Phase 1 to complete.
        tokio::time::timeout(Duration::from_secs(5), phase1_done_rx.recv())
            .await
            .expect("timed out waiting for Phase 1");

        // close() cancels the job and waits for the background wait group,
        // which includes the lookup task. After this, call_count is stable.
        job.close().await.expect("close");
        assert_eq!(
            call_count.load(Ordering::SeqCst),
            1,
            "expected exactly 1 lookup call (Phase 2 disabled)"
        );
    }
}

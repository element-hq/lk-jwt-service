// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//! The MembershipMonitor actor: removes participants from the SFU once they
//! are no longer members of the Matrix room their token was issued for, as
//! recommended by MSC4195 ("Kicking users from the SFU on room leave/ban").
//!
//! # How it works
//!
//! Every token this service mints is registered with the monitor together
//! with the Matrix room and user it was issued for. SFU webhooks then tell
//! the monitor when the participant actually connects (or leaves), and a
//! periodic sweep reconciles the tracked participants against the
//! homeserver:
//!
//!   - One `GET /rooms/{roomId}/joined_members` request per Matrix room with
//!     tracked participants. Every *connected* participant whose user is no
//!     longer in the returned member list is removed from the SFU.
//!   - A 403 from that endpoint means none of the application service's users
//!     is in the room any more. Since the service's user namespace covers the
//!     homeserver's local users, that is the homeserver having left the room.
//!     The monitor double-checks that via MSC4502's `/is_joined` and, when
//!     confirmed, deletes the affected LiveKit rooms outright — nothing
//!     legitimate is left in them because only local users can publish on
//!     this SFU. When *not* confirmed, the 403 is treated as a
//!     misconfiguration (most likely the namespace regex) and nothing is
//!     removed.
//!   - Any other failure to determine membership fails open: a homeserver
//!     outage must not drop every call.
//!
//! Participants that were issued a token but have not shown up on the SFU
//! yet ("pending") are never kicked, since there is nothing to kick. Their
//! presence is verified against the SFU on every sweep — so a missed
//! `participant_joined` webhook only delays enforcement — and they are
//! forgotten once the token they were issued has expired without a connect.
//!
//! Tracked participants are persisted through the [`Store`] so enforcement
//! resumes after a restart; recovered entries start out as pending and are
//! verified against the SFU by the first sweep.
//!
//! # Concurrency model
//!
//! `run_loop` is the single task that owns the participant map. Everything
//! else communicates with it through channels. Sweeps run as separate tasks
//! so a slow homeserver never stalls webhook processing; at most one sweep
//! is in flight at any time.

use std::collections::{HashMap, HashSet};
use std::sync::Arc;
use std::time::Duration;

use chrono::{DateTime, Utc};
use futures::StreamExt;
#[cfg(test)]
use tokio::sync::oneshot;
use tokio::sync::{mpsc, watch};
use tokio_util::sync::CancellationToken;
use tracing::{debug, error, info, warn};

use crate::delayed_event_manager::{LookupCsApiUrlFn, WaitGroup};
use crate::helper::{
    CsApiUrl, Deps, LiveKitAuth, LiveKitIdentity, LiveKitRoomAlias, MembershipQueryError,
    ParticipantKey,
};
use crate::retry::{Classify, ErrorClass, ExponentialBackoff, retry};
use crate::store::{Store, StoredParticipant};

/// How many homeserver / SFU requests a sweep has in flight at once.
const SWEEP_CONCURRENCY: usize = 8;

/// How long a participant who was issued a token but never showed up on the
/// SFU stays tracked. Matches the lifetime of the tokens this service mints
/// (see `get_join_token`): once the token has expired, it cannot be used to
/// connect any more.
pub const PENDING_TTL: Duration = Duration::from_secs(60 * 60);

/// How long the monitor keeps retrying a single SFU removal before giving up
/// on it (the next sweep will try again if the participant is still there).
const SFU_ACTION_BUDGET: Duration = Duration::from_secs(60);

/// A participant to start tracking, i.e. a token that was just minted.
#[derive(Debug, Clone, PartialEq)]
pub struct Registration {
    /// The Matrix room the token was issued for.
    pub matrix_room_id: String,
    /// The Matrix user the token was issued to.
    pub matrix_user_id: String,
    /// The LiveKit room the token grants access to.
    pub livekit_room: LiveKitRoomAlias,
    /// The LiveKit identity the token was issued for.
    pub livekit_identity: LiveKitIdentity,
}

impl Registration {
    fn key(&self) -> ParticipantKey {
        ParticipantKey {
            room: self.livekit_room.clone(),
            identity: self.livekit_identity.clone(),
        }
    }
}

/// An SFU-side lifecycle event the monitor tracks.
#[derive(Debug, Clone, PartialEq)]
pub enum SfuEvent {
    /// The participant connected to the room.
    ParticipantJoined(ParticipantKey),
    /// The participant left the room, for whatever reason.
    ParticipantLeft(ParticipantKey),
    /// The room was closed; every participant in it is gone.
    RoomFinished(LiveKitRoomAlias),
}

/// A tracked participant, as seen by the monitor.
#[derive(Debug, Clone, PartialEq)]
pub struct TrackedParticipant {
    pub stored: StoredParticipant,
    /// Whether the participant is known to be connected to the SFU.
    pub connected: bool,
}

/// Configuration of a [`MembershipMonitor`].
#[derive(Debug, Clone)]
pub struct MembershipMonitorConfig {
    /// The period between sweeps.
    pub check_interval: Duration,
    /// The server name of the homeserver this service is registered with.
    pub hs_server_name: String,
    /// The token authenticating requests to the homeserver.
    pub as_token: String,
}

/// A message to the actor loop.
enum Msg {
    Register(Registration),
    Sfu(SfuEvent),
    /// Runs a sweep now (in addition to the periodic ones) and replies once
    /// its outcome has been applied.
    #[cfg(test)]
    Sweep(oneshot::Sender<()>),
    /// Replies with the current participant map.
    #[cfg(test)]
    Snapshot(oneshot::Sender<Vec<TrackedParticipant>>),
}

/// The membership monitor actor. See the module documentation.
pub struct MembershipMonitor {
    cancel: CancellationToken,
    msg_tx: mpsc::Sender<Msg>,
    /// Set to true when run_loop has exited.
    loop_done_tx: watch::Sender<bool>,
    /// Set to true once start-up recovery of stored participants is done.
    recovery_done_tx: watch::Sender<bool>,
}

/// The receiving ends consumed by the actor loop, plus everything else the
/// loop needs that is not shared with the outside.
pub(crate) struct LoopContext {
    msg_rx: mpsc::Receiver<Msg>,
    config: MembershipMonitorConfig,
    deps: Arc<dyn Deps>,
    lk_auth: LiveKitAuth,
    lookup_cs_api_url: LookupCsApiUrlFn,
    store: Option<Arc<dyn Store>>,
    /// Keys of participants that were removed from the SFU because they lost
    /// their room membership. The handler stops any delayed-event job for
    /// them.
    revoked_tx: mpsc::Sender<ParticipantKey>,
}

impl MembershipMonitor {
    /// Constructs a monitor and starts its actor loop.
    ///
    /// `parent_cancel` ties the monitor's lifetime to its owner: cancelling
    /// the parent shuts the monitor down too. [`Self::close`] additionally
    /// waits for the loop to exit.
    pub fn new(
        parent_cancel: &CancellationToken,
        config: MembershipMonitorConfig,
        deps: Arc<dyn Deps>,
        lk_auth: LiveKitAuth,
        lookup_cs_api_url: LookupCsApiUrlFn,
        store: Option<Arc<dyn Store>>,
        revoked_tx: mpsc::Sender<ParticipantKey>,
    ) -> Arc<Self> {
        let (monitor, ctx) = Self::new_without_loop(
            parent_cancel,
            config,
            deps,
            lk_auth,
            lookup_cs_api_url,
            store,
            revoked_tx,
        );
        let looped = monitor.clone();
        tokio::spawn(async move { looped.run_loop(ctx).await });
        monitor
    }

    /// Constructs a monitor without starting its actor loop, handing the
    /// loop's context to the caller.
    pub(crate) fn new_without_loop(
        parent_cancel: &CancellationToken,
        config: MembershipMonitorConfig,
        deps: Arc<dyn Deps>,
        lk_auth: LiveKitAuth,
        lookup_cs_api_url: LookupCsApiUrlFn,
        store: Option<Arc<dyn Store>>,
        revoked_tx: mpsc::Sender<ParticipantKey>,
    ) -> (Arc<Self>, LoopContext) {
        let (msg_tx, msg_rx) = mpsc::channel(256);
        let (loop_done_tx, _) = watch::channel(false);
        let (recovery_done_tx, _) = watch::channel(false);
        let monitor = Arc::new(Self {
            cancel: parent_cancel.child_token(),
            msg_tx,
            loop_done_tx,
            recovery_done_tx,
        });
        let ctx = LoopContext {
            msg_rx,
            config,
            deps,
            lk_auth,
            lookup_cs_api_url,
            store,
            revoked_tx,
        };
        (monitor, ctx)
    }

    /// A watch receiver that flips to true once start-up recovery completed.
    #[cfg(test)]
    pub(crate) fn recovery_done(&self) -> watch::Receiver<bool> {
        self.recovery_done_tx.subscribe()
    }

    /// Starts tracking a participant a token was just minted for. Replaces
    /// any earlier registration for the same (room, identity).
    pub async fn register(&self, registration: Registration) {
        self.send(Msg::Register(registration)).await;
    }

    /// Feeds an SFU webhook event to the monitor.
    pub async fn notify_sfu_event(&self, event: SfuEvent) {
        self.send(Msg::Sfu(event)).await;
    }

    /// Runs a sweep now and waits until its outcome has been applied.
    #[cfg(test)]
    pub(crate) async fn sweep_now(&self) {
        let (tx, rx) = oneshot::channel();
        self.send(Msg::Sweep(tx)).await;
        let _ = rx.await;
    }

    /// The participants currently tracked.
    #[cfg(test)]
    pub(crate) async fn snapshot(&self) -> Vec<TrackedParticipant> {
        let (tx, rx) = oneshot::channel();
        self.send(Msg::Snapshot(tx)).await;
        rx.await.unwrap_or_default()
    }

    async fn send(&self, msg: Msg) {
        tokio::select! {
            _ = self.cancel.cancelled() => {
                debug!("MembershipMonitor: dropping message after shutdown");
            }
            sent = self.msg_tx.send(msg) => {
                if sent.is_err() {
                    debug!("MembershipMonitor: dropping message, loop is gone");
                }
            }
        }
    }

    /// Shuts the monitor down and waits for the loop to exit.
    pub async fn close(&self) {
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
            warn!("MembershipMonitor: close() timed out");
        }
    }

    /// The actor task owning the participant map. Runs until cancelled.
    pub(crate) async fn run_loop(self: Arc<Self>, mut ctx: LoopContext) {
        let mut participants: HashMap<ParticipantKey, TrackedParticipant> = HashMap::new();
        let background = WaitGroup::default();

        // Store writes go through a dedicated writer task so a slow store
        // cannot stall the loop, while a single writer preserves the order the
        // loop decided on (a save followed by a delete must stay a delete).
        let (store_tx, store_writer) = match &ctx.store {
            Some(store) => {
                let store = store.clone();
                let (tx, mut op_rx) = mpsc::channel::<StoreOp>(256);
                let writer = tokio::spawn(async move {
                    while let Some(op) = op_rx.recv().await {
                        match op {
                            StoreOp::Save(participant) => {
                                let key = participant.key();
                                if let Err(err) = store.save_participant(&key, &participant).await {
                                    error!(key = ?key, err = %err,
                                        "MembershipMonitor: failed to store participant");
                                }
                            }
                            StoreOp::Delete(key) => {
                                if let Err(err) = store.delete_participant(&key).await {
                                    error!(key = ?key, err = %err,
                                        "MembershipMonitor: failed to delete stored participant");
                                }
                            }
                        }
                    }
                });
                (Some(tx), Some(writer))
            }
            None => (None, None),
        };
        let enqueue_store_op = |op: StoreOp| async {
            if let Some(tx) = &store_tx
                && tx.send(op).await.is_err()
            {
                error!("MembershipMonitor: store writer is gone, dropping store operation");
            }
        };

        // Recover the participants tracked before the last shutdown. They
        // start out as pending: the first sweep verifies who is actually still
        // connected.
        if let Some(store) = &ctx.store {
            match store.all_participants().await {
                Err(err) => {
                    error!(err = %err, "MembershipMonitor: failed to load stored participants");
                }
                Ok(stored) => {
                    for participant in stored {
                        let key = participant.key();
                        debug!(matrix_room = %participant.matrix_room_id,
                            matrix_id = %participant.matrix_user_id, room = %key.room,
                            lk_id = %key.identity, "MembershipMonitor: recovered stored participant");
                        participants.insert(
                            key,
                            TrackedParticipant {
                                stored: participant,
                                connected: false,
                            },
                        );
                    }
                }
            }
        }
        self.recovery_done_tx.send_replace(true);

        // Sweeps report back through this channel. `sweep_in_flight` keeps it
        // to one sweep at a time; `sweep_waiters` are answered once the next
        // outcome has been applied.
        let (sweep_tx, mut sweep_rx) = mpsc::channel::<SweepOutcome>(1);
        let mut sweep_in_flight = false;
        #[cfg(test)]
        let mut sweep_waiters: Vec<oneshot::Sender<()>> = Vec::new();

        let mut ticker = tokio::time::interval_at(
            tokio::time::Instant::now() + ctx.config.check_interval,
            ctx.config.check_interval,
        );
        ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);

        // Start reconciling recovered participants right away rather than a
        // full interval later.
        let mut sweep_requested = !participants.is_empty();

        loop {
            if sweep_requested && !sweep_in_flight {
                sweep_requested = false;
                sweep_in_flight = true;
                let input = SweepInput::from_participants(&participants);
                let sweep_tx = sweep_tx.clone();
                let deps = ctx.deps.clone();
                let lk_auth = ctx.lk_auth.clone();
                let lookup = ctx.lookup_cs_api_url.clone();
                let config = ctx.config.clone();
                let cancel = self.cancel.clone();
                let guard = background.add();
                tokio::spawn(async move {
                    let _guard = guard;
                    let outcome = tokio::select! {
                        _ = cancel.cancelled() => return,
                        outcome = sweep(deps, lk_auth, lookup, config, input) => outcome,
                    };
                    let _ = sweep_tx.send(outcome).await;
                });
            }

            tokio::select! {
                _ = self.cancel.cancelled() => {
                    debug!("MembershipMonitor: loop exiting");
                    break;
                }

                _ = ticker.tick() => {
                    if sweep_in_flight {
                        warn!("MembershipMonitor: previous sweep still running, skipping this one");
                    } else if !participants.is_empty() {
                        sweep_requested = true;
                    }
                }

                Some(outcome) = sweep_rx.recv() => {
                    sweep_in_flight = false;
                    let actions = apply_sweep_outcome(&mut participants, outcome, Utc::now());
                    for key in &actions.forgotten {
                        enqueue_store_op(StoreOp::Delete(key.clone())).await;
                    }
                    for key in &actions.revoked {
                        enqueue_store_op(StoreOp::Delete(key.clone())).await;
                        tokio::select! {
                            _ = self.cancel.cancelled() => {}
                            _ = ctx.revoked_tx.send(key.clone()) => {}
                        }
                    }
                    for key in actions.kick {
                        spawn_sfu_action(
                            &background,
                            &self.cancel,
                            ctx.deps.clone(),
                            ctx.lk_auth.clone(),
                            SfuAction::RemoveParticipant(key),
                        );
                    }
                    for room in actions.delete_rooms {
                        spawn_sfu_action(
                            &background,
                            &self.cancel,
                            ctx.deps.clone(),
                            ctx.lk_auth.clone(),
                            SfuAction::DeleteRoom(room),
                        );
                    }
                    #[cfg(test)]
                    for waiter in sweep_waiters.drain(..) {
                        let _ = waiter.send(());
                    }
                }

                Some(msg) = ctx.msg_rx.recv() => {
                    match msg {
                        Msg::Register(registration) => {
                            let key = registration.key();
                            // A re-issued token for a connected participant
                            // keeps its connected state; only the timestamp
                            // moves.
                            let connected = participants.get(&key).is_some_and(|p| p.connected);
                            let stored = StoredParticipant {
                                matrix_room_id: registration.matrix_room_id,
                                matrix_user_id: registration.matrix_user_id,
                                livekit_room: registration.livekit_room,
                                livekit_identity: registration.livekit_identity,
                                registered_at: Utc::now(),
                            };
                            debug!(matrix_room = %stored.matrix_room_id,
                                matrix_id = %stored.matrix_user_id, room = %key.room,
                                lk_id = %key.identity, connected,
                                "MembershipMonitor: tracking participant");
                            enqueue_store_op(StoreOp::Save(stored.clone())).await;
                            participants.insert(key, TrackedParticipant { stored, connected });
                        }

                        Msg::Sfu(SfuEvent::ParticipantJoined(key)) => {
                            match participants.get_mut(&key) {
                                Some(participant) => {
                                    debug!(room = %key.room, lk_id = %key.identity,
                                        "MembershipMonitor: participant connected");
                                    participant.connected = true;
                                }
                                None => {
                                    debug!(room = %key.room, lk_id = %key.identity,
                                        "MembershipMonitor: ignoring connect of untracked participant");
                                }
                            }
                        }

                        Msg::Sfu(SfuEvent::ParticipantLeft(key)) => {
                            if participants.remove(&key).is_some() {
                                debug!(room = %key.room, lk_id = %key.identity,
                                    "MembershipMonitor: participant left, no longer tracking");
                                enqueue_store_op(StoreOp::Delete(key)).await;
                            }
                        }

                        Msg::Sfu(SfuEvent::RoomFinished(room)) => {
                            let gone: Vec<ParticipantKey> = participants
                                .keys()
                                .filter(|key| key.room == room)
                                .cloned()
                                .collect();
                            if !gone.is_empty() {
                                debug!(room = %room, count = gone.len(),
                                    "MembershipMonitor: room finished, no longer tracking its participants");
                            }
                            for key in gone {
                                participants.remove(&key);
                                enqueue_store_op(StoreOp::Delete(key)).await;
                            }
                        }

                        #[cfg(test)]
                        Msg::Sweep(reply) => {
                            sweep_waiters.push(reply);
                            sweep_requested = true;
                        }

                        #[cfg(test)]
                        Msg::Snapshot(reply) => {
                            let _ = reply.send(participants.values().cloned().collect());
                        }
                    }
                }
            }
        }

        // Wait for in-flight sweeps and SFU actions, then let the writer
        // drain its queue before signalling completion.
        background.wait().await;
        drop(store_tx);
        if let Some(writer) = store_writer {
            let _ = writer.await;
        }
        self.loop_done_tx.send_replace(true);
    }
}

/// A persistence operation queued for the store-writer task.
enum StoreOp {
    Save(StoredParticipant),
    Delete(ParticipantKey),
}

// ── sweeps ───────────────────────────────────────────────────────────────────

/// What a sweep needs to know, snapshotted from the participant map.
#[derive(Debug, Default, PartialEq)]
struct SweepInput {
    /// Participants not (yet) known to be connected, whose presence on the
    /// SFU is to be verified.
    pending: Vec<ParticipantKey>,
    /// The Matrix rooms with tracked participants.
    rooms: Vec<String>,
}

impl SweepInput {
    fn from_participants(participants: &HashMap<ParticipantKey, TrackedParticipant>) -> Self {
        let pending = participants
            .iter()
            .filter(|(_, p)| !p.connected)
            .map(|(key, _)| key.clone())
            .collect();
        let rooms: HashSet<&str> = participants
            .values()
            .map(|p| p.stored.matrix_room_id.as_str())
            .collect();
        let mut rooms: Vec<String> = rooms.into_iter().map(str::to_owned).collect();
        rooms.sort();
        Self { pending, rooms }
    }
}

/// The homeserver's answer about one Matrix room.
#[derive(Debug, PartialEq)]
enum RoomMembership {
    /// The users currently joined to the room.
    Joined(HashSet<String>),
    /// The homeserver itself is no longer in the room.
    ServerLeft,
    /// Membership could not be determined; leave the room's participants be.
    Unknown,
}

/// What a sweep found out.
#[derive(Debug, Default)]
struct SweepOutcome {
    /// Pending participants found present on the SFU.
    present: Vec<ParticipantKey>,
    /// Pending participants confirmed absent from the SFU.
    absent: Vec<ParticipantKey>,
    /// Membership per Matrix room.
    rooms: Vec<(String, RoomMembership)>,
}

/// Queries the SFU and the homeserver for everything in `input`.
async fn sweep(
    deps: Arc<dyn Deps>,
    lk_auth: LiveKitAuth,
    lookup_cs_api_url: LookupCsApiUrlFn,
    config: MembershipMonitorConfig,
    input: SweepInput,
) -> SweepOutcome {
    let mut outcome = SweepOutcome::default();

    // Presence of pending participants.
    let mut presence = futures::stream::iter(input.pending)
        .map(|key| {
            let deps = deps.clone();
            let lk_auth = lk_auth.clone();
            async move {
                let result = deps
                    .participant_exists(&lk_auth, &key.room, &key.identity)
                    .await;
                (key, result)
            }
        })
        .buffer_unordered(SWEEP_CONCURRENCY);
    while let Some((key, result)) = presence.next().await {
        match result {
            Ok(true) => outcome.present.push(key),
            Ok(false) => outcome.absent.push(key),
            Err(err) => {
                warn!(room = %key.room, lk_id = %key.identity, err = %err,
                    "MembershipMonitor: could not verify presence on the SFU");
            }
        }
    }

    if input.rooms.is_empty() {
        return outcome;
    }

    // Room membership. Everything below needs the homeserver's C-S API.
    let cs_api_url = match lookup_cs_api_url(config.hs_server_name.clone()).await {
        Ok(url) => url,
        Err(err) => {
            warn!(server_name = %config.hs_server_name, err = %err,
                "MembershipMonitor: could not resolve the Client-Server API, skipping membership checks");
            outcome.rooms = input
                .rooms
                .into_iter()
                .map(|room| (room, RoomMembership::Unknown))
                .collect();
            return outcome;
        }
    };

    let mut memberships = futures::stream::iter(input.rooms)
        .map(|room_id| {
            let deps = deps.clone();
            let cs_api_url = cs_api_url.clone();
            let config = config.clone();
            async move {
                let membership =
                    query_room_membership(&*deps, &cs_api_url, &config, &room_id).await;
                (room_id, membership)
            }
        })
        .buffer_unordered(SWEEP_CONCURRENCY);
    while let Some(entry) = memberships.next().await {
        outcome.rooms.push(entry);
    }
    outcome
}

/// Determines the membership of one Matrix room, see [`RoomMembership`].
async fn query_room_membership(
    deps: &dyn Deps,
    cs_api_url: &CsApiUrl,
    config: &MembershipMonitorConfig,
    room_id: &str,
) -> RoomMembership {
    match deps
        .get_joined_members(cs_api_url, room_id, &config.as_token)
        .await
    {
        Ok(members) => RoomMembership::Joined(members),
        Err(MembershipQueryError::NotInRoom) => {
            // Confirm before doing anything drastic: a 403 with the server
            // still joined points at a misconfigured user namespace.
            match deps
                .is_server_joined(
                    cs_api_url,
                    room_id,
                    &config.hs_server_name,
                    &config.as_token,
                )
                .await
            {
                Ok(false) => RoomMembership::ServerLeft,
                Ok(true) => {
                    error!(matrix_room = %room_id,
                        "MembershipMonitor: joined_members was refused although the homeserver is \
                         in the room; check that the application service's user namespace covers \
                         the homeserver's local users. Leaving the room's participants untouched.");
                    RoomMembership::Unknown
                }
                Err(err) => {
                    warn!(matrix_room = %room_id, err = %err,
                        "MembershipMonitor: joined_members was refused and the homeserver's own \
                         membership could not be verified, leaving the room's participants untouched");
                    RoomMembership::Unknown
                }
            }
        }
        Err(MembershipQueryError::Other(err)) => {
            warn!(matrix_room = %room_id, err = %err,
                "MembershipMonitor: could not fetch joined members, leaving the room's participants untouched");
            RoomMembership::Unknown
        }
    }
}

/// What the loop has to do after applying a sweep outcome.
#[derive(Debug, Default, PartialEq)]
struct SweepActions {
    /// Participants to remove from the SFU (they lost their room membership).
    kick: Vec<ParticipantKey>,
    /// LiveKit rooms to delete (the homeserver left their Matrix room).
    delete_rooms: Vec<LiveKitRoomAlias>,
    /// Participants no longer tracked because they lost their room
    /// membership: `kick` plus everyone in `delete_rooms`. The handler stops
    /// their delayed-event jobs.
    revoked: Vec<ParticipantKey>,
    /// Participants no longer tracked for other reasons (their token expired
    /// without a connect).
    forgotten: Vec<ParticipantKey>,
}

/// Applies a sweep outcome to the participant map and returns the actions to
/// take. Pure, so it can be tested without the actor.
fn apply_sweep_outcome(
    participants: &mut HashMap<ParticipantKey, TrackedParticipant>,
    outcome: SweepOutcome,
    now: DateTime<Utc>,
) -> SweepActions {
    let mut actions = SweepActions::default();

    for key in outcome.present {
        if let Some(participant) = participants.get_mut(&key)
            && !participant.connected
        {
            debug!(room = %key.room, lk_id = %key.identity,
                "MembershipMonitor: participant found on the SFU");
            participant.connected = true;
        }
    }

    let pending_ttl = chrono::Duration::from_std(PENDING_TTL).unwrap_or_default();
    for key in outcome.absent {
        let expired = participants
            .get(&key)
            .is_some_and(|p| !p.connected && p.stored.registered_at + pending_ttl <= now);
        if expired {
            debug!(room = %key.room, lk_id = %key.identity,
                "MembershipMonitor: token expired without a connect, no longer tracking");
            participants.remove(&key);
            actions.forgotten.push(key);
        }
    }

    for (room_id, membership) in outcome.rooms {
        match membership {
            RoomMembership::Joined(members) => {
                let kicked: Vec<ParticipantKey> = participants
                    .iter()
                    .filter(|(_, p)| {
                        p.stored.matrix_room_id == room_id
                            && p.connected
                            && !members.contains(&p.stored.matrix_user_id)
                    })
                    .map(|(key, _)| key.clone())
                    .collect();
                for key in kicked {
                    if let Some(participant) = participants.remove(&key) {
                        info!(matrix_room = %room_id, matrix_id = %participant.stored.matrix_user_id,
                            room = %key.room, lk_id = %key.identity,
                            "MembershipMonitor: user is no longer a room member, removing from the SFU");
                    }
                    actions.revoked.push(key.clone());
                    actions.kick.push(key);
                }
            }
            RoomMembership::ServerLeft => {
                let gone: Vec<ParticipantKey> = participants
                    .iter()
                    .filter(|(_, p)| p.stored.matrix_room_id == room_id)
                    .map(|(key, _)| key.clone())
                    .collect();
                let mut rooms: Vec<LiveKitRoomAlias> = Vec::new();
                for key in gone {
                    participants.remove(&key);
                    if !rooms.contains(&key.room) {
                        rooms.push(key.room.clone());
                    }
                    actions.revoked.push(key);
                }
                for room in rooms {
                    info!(matrix_room = %room_id, room = %room,
                        "MembershipMonitor: homeserver left the room, deleting the LiveKit room");
                    actions.delete_rooms.push(room);
                }
            }
            RoomMembership::Unknown => {}
        }
    }

    actions
}

// ── SFU actions ──────────────────────────────────────────────────────────────

/// A removal to perform on the SFU.
#[derive(Debug, Clone)]
enum SfuAction {
    RemoveParticipant(ParticipantKey),
    DeleteRoom(LiveKitRoomAlias),
}

/// A failed SFU action. Always retried: the SFU is local infrastructure, so
/// failures are expected to be transient.
#[derive(Debug, thiserror::Error)]
#[error("{0}")]
struct SfuActionError(String);

impl Classify for SfuActionError {
    fn classify(&self) -> ErrorClass {
        ErrorClass::Transient
    }
}

/// Performs `action` in a background task, retrying transient failures for
/// up to [`SFU_ACTION_BUDGET`].
fn spawn_sfu_action(
    background: &WaitGroup,
    cancel: &CancellationToken,
    deps: Arc<dyn Deps>,
    lk_auth: LiveKitAuth,
    action: SfuAction,
) {
    let guard = background.add();
    let cancel = cancel.clone();
    tokio::spawn(async move {
        let _guard = guard;
        let result = retry(
            &cancel,
            ExponentialBackoff::service_default(),
            SFU_ACTION_BUDGET,
            || {
                let deps = deps.clone();
                let lk_auth = lk_auth.clone();
                let action = action.clone();
                let cancel = cancel.clone();
                async move {
                    let attempt = async {
                        match &action {
                            SfuAction::RemoveParticipant(key) => {
                                deps.remove_participant(&lk_auth, &key.room, &key.identity)
                                    .await
                            }
                            SfuAction::DeleteRoom(room) => {
                                deps.delete_livekit_room(&lk_auth, room).await
                            }
                        }
                    };
                    tokio::select! {
                        _ = cancel.cancelled() => Err(SfuActionError("cancelled".into())),
                        result = attempt => result.map_err(SfuActionError),
                    }
                }
            },
        )
        .await;
        match (&action, result) {
            (SfuAction::RemoveParticipant(key), Ok(())) => {
                info!(room = %key.room, lk_id = %key.identity,
                    "MembershipMonitor: removed participant from the SFU");
            }
            (SfuAction::DeleteRoom(room), Ok(())) => {
                info!(room = %room, "MembershipMonitor: deleted LiveKit room");
            }
            (SfuAction::RemoveParticipant(key), Err(err)) => {
                if !cancel.is_cancelled() {
                    error!(room = %key.room, lk_id = %key.identity, err = %err,
                        "MembershipMonitor: failed to remove participant from the SFU");
                }
            }
            (SfuAction::DeleteRoom(room), Err(err)) => {
                if !cancel.is_cancelled() {
                    error!(room = %room, err = %err,
                        "MembershipMonitor: failed to delete LiveKit room");
                }
            }
        }
    });
}

#[cfg(test)]
#[path = "membership_monitor_tests.rs"]
mod tests;

// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//! MembershipMonitor tests: registration and webhook bookkeeping, the sweep
//! decisions, persistence and recovery, and the periodic schedule.

use std::sync::Mutex;
use std::sync::atomic::{AtomicBool, Ordering};

use tokio::sync::mpsc;

use super::*;
use crate::helper::{livekit_identity_for, livekit_room_alias_for};
use crate::store::test_support::new_in_memory_store;

// ── test deps ─────────────────────────────────────────────────────────────────

type ParticipantExistsFn =
    Box<dyn Fn(&LiveKitRoomAlias, &LiveKitIdentity) -> Result<bool, String> + Send + Sync>;
type JoinedMembersFn =
    Box<dyn Fn(&str) -> Result<HashSet<String>, MembershipQueryError> + Send + Sync>;
type IsServerJoinedFn = Box<dyn Fn(&str, &str) -> Result<bool, String> + Send + Sync>;

/// A [`Deps`] implementation for the monitor: membership and presence are
/// scripted per test, removals are recorded and reported through channels.
/// Un-mocked SFU presence checks report the participant present.
#[derive(Default)]
struct MonitorTestDeps {
    participant_exists_fn: Option<ParticipantExistsFn>,
    get_joined_members_fn: Option<JoinedMembersFn>,
    is_server_joined_fn: Option<IsServerJoinedFn>,
    /// Every room `get_joined_members` was called for.
    joined_members_calls: Arc<Mutex<Vec<String>>>,
    /// Every participant removed from the SFU.
    removed_tx: Option<mpsc::UnboundedSender<ParticipantKey>>,
    /// Every LiveKit room deleted.
    deleted_tx: Option<mpsc::UnboundedSender<LiveKitRoomAlias>>,
}

#[async_trait::async_trait]
impl Deps for MonitorTestDeps {
    fn new_room_service_client(
        &self,
        _url: &str,
        _key: &str,
        _secret: &str,
    ) -> Arc<dyn crate::helper::RoomServiceClient> {
        panic!("new_room_service_client must not be called by the monitor");
    }

    async fn participant_exists(
        &self,
        _lk_auth: &LiveKitAuth,
        room: &LiveKitRoomAlias,
        identity: &LiveKitIdentity,
    ) -> Result<bool, String> {
        match &self.participant_exists_fn {
            Some(f) => f(room, identity),
            None => Ok(true),
        }
    }

    async fn remove_participant(
        &self,
        _lk_auth: &LiveKitAuth,
        room: &LiveKitRoomAlias,
        identity: &LiveKitIdentity,
    ) -> Result<(), String> {
        if let Some(tx) = &self.removed_tx {
            let _ = tx.send(ParticipantKey {
                room: room.clone(),
                identity: identity.clone(),
            });
        }
        Ok(())
    }

    async fn delete_livekit_room(
        &self,
        _lk_auth: &LiveKitAuth,
        room: &LiveKitRoomAlias,
    ) -> Result<(), String> {
        if let Some(tx) = &self.deleted_tx {
            let _ = tx.send(room.clone());
        }
        Ok(())
    }

    async fn get_joined_members(
        &self,
        _cs_api_url: &CsApiUrl,
        room_id: &str,
        _as_token: &str,
    ) -> Result<HashSet<String>, MembershipQueryError> {
        self.joined_members_calls
            .lock()
            .unwrap()
            .push(room_id.to_owned());
        match &self.get_joined_members_fn {
            Some(f) => f(room_id),
            None => panic!("get_joined_members not mocked"),
        }
    }

    async fn is_server_joined(
        &self,
        _cs_api_url: &CsApiUrl,
        room_id: &str,
        server_name: &str,
        _as_token: &str,
    ) -> Result<bool, String> {
        match &self.is_server_joined_fn {
            Some(f) => f(room_id, server_name),
            None => panic!("is_server_joined not mocked"),
        }
    }
}

/// A `get_joined_members` mock reporting the given users joined to every room.
fn members(users: &[&str]) -> Option<JoinedMembersFn> {
    let users: HashSet<String> = users.iter().map(|u| (*u).to_owned()).collect();
    Some(Box::new(move |_| Ok(users.clone())))
}

/// A `get_joined_members` mock refusing every room with a 403.
fn not_in_room() -> Option<JoinedMembersFn> {
    Some(Box::new(|_| Err(MembershipQueryError::NotInRoom)))
}

/// An `is_server_joined` mock with a fixed answer.
fn server_joined(joined: bool) -> Option<IsServerJoinedFn> {
    Some(Box::new(move |_, _| Ok(joined)))
}

// ── fixtures ──────────────────────────────────────────────────────────────────

const HS_SERVER_NAME: &str = "example.com";
const ROOM_A: &str = "!a:example.com";
const ROOM_B: &str = "!b:example.com";
const ALICE: &str = "@alice:example.com";
const BOB: &str = "@bob:remote.example.org";

/// The handles a test needs to drive and observe a monitor.
struct Harness {
    monitor: Arc<MembershipMonitor>,
    cancel: CancellationToken,
    revoked_rx: mpsc::Receiver<ParticipantKey>,
    removed_rx: mpsc::UnboundedReceiver<ParticipantKey>,
    deleted_rx: mpsc::UnboundedReceiver<LiveKitRoomAlias>,
    joined_members_calls: Arc<Mutex<Vec<String>>>,
}

/// A CS-API lookup that always resolves.
fn resolving_lookup() -> LookupCsApiUrlFn {
    Arc::new(|_| Box::pin(async { Ok(CsApiUrl("https://matrix.example.com".into())) }))
}

/// A CS-API lookup that always fails.
fn failing_lookup() -> LookupCsApiUrlFn {
    Arc::new(|_| Box::pin(async { Err("no such server".to_owned()) }))
}

fn new_harness(deps: MonitorTestDeps, store: Option<Arc<dyn Store>>) -> Harness {
    new_harness_with(
        deps,
        store,
        Duration::from_secs(60 * 60),
        resolving_lookup(),
    )
}

fn new_harness_with(
    mut deps: MonitorTestDeps,
    store: Option<Arc<dyn Store>>,
    check_interval: Duration,
    lookup: LookupCsApiUrlFn,
) -> Harness {
    let (removed_tx, removed_rx) = mpsc::unbounded_channel();
    let (deleted_tx, deleted_rx) = mpsc::unbounded_channel();
    deps.removed_tx = Some(removed_tx);
    deps.deleted_tx = Some(deleted_tx);
    let joined_members_calls = deps.joined_members_calls.clone();

    let (revoked_tx, revoked_rx) = mpsc::channel(64);
    let cancel = CancellationToken::new();
    let monitor = MembershipMonitor::new(
        &cancel,
        MembershipMonitorConfig {
            check_interval,
            hs_server_name: HS_SERVER_NAME.into(),
            as_token: "as_token".into(),
        },
        Arc::new(deps),
        LiveKitAuth::default(),
        lookup,
        store,
        revoked_tx,
    );
    Harness {
        monitor,
        cancel,
        revoked_rx,
        removed_rx,
        deleted_rx,
        joined_members_calls,
    }
}

/// A registration for `mxid` in `room_id`, in the given slot and with the
/// given member ID.
fn registration(room_id: &str, mxid: &str, slot_id: &str, member_id: &str) -> Registration {
    Registration {
        matrix_room_id: room_id.to_owned(),
        matrix_user_id: mxid.to_owned(),
        livekit_room: livekit_room_alias_for(room_id, slot_id),
        livekit_identity: livekit_identity_for(mxid, "DEVICE", member_id),
    }
}

/// Registers the participant and reports them connected.
async fn register_connected(monitor: &MembershipMonitor, reg: &Registration) {
    monitor.register(reg.clone()).await;
    monitor
        .notify_sfu_event(SfuEvent::ParticipantJoined(reg.key()))
        .await;
}

/// Receives from a channel, panicking when nothing arrives in time.
async fn recv_timeout<T>(rx: &mut mpsc::UnboundedReceiver<T>, what: &str) -> T {
    tokio::time::timeout(Duration::from_secs(5), rx.recv())
        .await
        .unwrap_or_else(|_| panic!("timed out waiting for {what}"))
        .unwrap_or_else(|| panic!("channel closed while waiting for {what}"))
}

/// Receives from a bounded channel, panicking when nothing arrives in time.
async fn recv_bounded_timeout<T>(rx: &mut mpsc::Receiver<T>, what: &str) -> T {
    tokio::time::timeout(Duration::from_secs(5), rx.recv())
        .await
        .unwrap_or_else(|_| panic!("timed out waiting for {what}"))
        .unwrap_or_else(|| panic!("channel closed while waiting for {what}"))
}

/// Asserts that nothing arrives on the channel for a little while.
async fn assert_quiet<T: std::fmt::Debug>(rx: &mut mpsc::UnboundedReceiver<T>, what: &str) {
    if let Ok(Some(item)) = tokio::time::timeout(Duration::from_millis(200), rx.recv()).await {
        panic!("unexpected {what}: {item:?}");
    }
}

fn keys(snapshot: &[TrackedParticipant]) -> HashSet<ParticipantKey> {
    snapshot.iter().map(|p| p.stored.key()).collect()
}

// ── kicking ───────────────────────────────────────────────────────────────────

/// A connected participant whose user is no longer in the room is removed
/// from the SFU, dropped from tracking and reported as revoked.
#[tokio::test]
async fn connected_non_member_is_kicked() {
    let mut h = new_harness(
        MonitorTestDeps {
            get_joined_members_fn: members(&[]),
            ..Default::default()
        },
        None,
    );
    let alice = registration(ROOM_A, ALICE, "m.call#", "m1");
    register_connected(&h.monitor, &alice).await;

    h.monitor.sweep_now().await;

    assert_eq!(
        recv_timeout(&mut h.removed_rx, "removal").await,
        alice.key()
    );
    assert_eq!(
        recv_bounded_timeout(&mut h.revoked_rx, "revocation").await,
        alice.key()
    );
    assert!(h.monitor.snapshot().await.is_empty());
    assert_quiet(&mut h.deleted_rx, "room deletion").await;
    h.cancel.cancel();
    h.monitor.close().await;
}

/// A connected participant whose user is still in the room stays.
#[tokio::test]
async fn member_is_left_alone() {
    let mut h = new_harness(
        MonitorTestDeps {
            get_joined_members_fn: members(&[ALICE]),
            ..Default::default()
        },
        None,
    );
    let alice = registration(ROOM_A, ALICE, "m.call#", "m1");
    register_connected(&h.monitor, &alice).await;

    h.monitor.sweep_now().await;

    assert_quiet(&mut h.removed_rx, "removal").await;
    let snapshot = h.monitor.snapshot().await;
    assert_eq!(keys(&snapshot), HashSet::from([alice.key()]));
    assert!(snapshot[0].connected);
    h.cancel.cancel();
    h.monitor.close().await;
}

/// A participant who has not connected yet cannot be kicked: they stay
/// tracked while absent from the SFU, and are removed on the first sweep
/// after they show up — whether that is learnt from a webhook or from the
/// SFU itself.
#[tokio::test]
async fn pending_participant_is_kicked_once_connected() {
    let present = Arc::new(AtomicBool::new(false));
    let present_clone = present.clone();
    let mut h = new_harness(
        MonitorTestDeps {
            get_joined_members_fn: members(&[]),
            participant_exists_fn: Some(Box::new(move |_, _| {
                Ok(present_clone.load(Ordering::SeqCst))
            })),
            ..Default::default()
        },
        None,
    );
    let alice = registration(ROOM_A, ALICE, "m.call#", "m1");
    h.monitor.register(alice.clone()).await;

    // Absent from the SFU: nothing to kick, but still tracked.
    h.monitor.sweep_now().await;
    assert_quiet(&mut h.removed_rx, "removal").await;
    let snapshot = h.monitor.snapshot().await;
    assert_eq!(keys(&snapshot), HashSet::from([alice.key()]));
    assert!(!snapshot[0].connected);

    // Present on the SFU (the join webhook was missed): the sweep notices
    // and, in the same pass, kicks.
    present.store(true, Ordering::SeqCst);
    h.monitor.sweep_now().await;
    assert_eq!(
        recv_timeout(&mut h.removed_rx, "removal").await,
        alice.key()
    );
    assert!(h.monitor.snapshot().await.is_empty());
    h.cancel.cancel();
    h.monitor.close().await;
}

/// One membership query serves every participant of a Matrix room, across
/// users, devices and slots; only the users who left are removed.
#[tokio::test]
async fn one_membership_query_per_room() {
    let mut h = new_harness(
        MonitorTestDeps {
            get_joined_members_fn: members(&[BOB]),
            ..Default::default()
        },
        None,
    );
    let alice_1 = registration(ROOM_A, ALICE, "m.call#", "m1");
    let alice_2 = registration(ROOM_A, ALICE, "m.call#other", "m2");
    let bob = registration(ROOM_A, BOB, "m.call#", "m3");
    for reg in [&alice_1, &alice_2, &bob] {
        register_connected(&h.monitor, reg).await;
    }

    h.monitor.sweep_now().await;

    assert_eq!(
        h.joined_members_calls.lock().unwrap().as_slice(),
        [ROOM_A.to_owned()],
        "expected exactly one membership query for the room"
    );
    let removed = HashSet::from([
        recv_timeout(&mut h.removed_rx, "first removal").await,
        recv_timeout(&mut h.removed_rx, "second removal").await,
    ]);
    assert_eq!(removed, HashSet::from([alice_1.key(), alice_2.key()]));
    assert_eq!(
        keys(&h.monitor.snapshot().await),
        HashSet::from([bob.key()])
    );
    h.cancel.cancel();
    h.monitor.close().await;
}

/// Rooms are queried independently: a user who left one room keeps their
/// participation in another.
#[tokio::test]
async fn rooms_are_independent() {
    let mut h = new_harness(
        MonitorTestDeps {
            get_joined_members_fn: Some(Box::new(|room_id| {
                Ok(if room_id == ROOM_A {
                    HashSet::new()
                } else {
                    HashSet::from([ALICE.to_owned()])
                })
            })),
            ..Default::default()
        },
        None,
    );
    let in_a = registration(ROOM_A, ALICE, "m.call#", "m1");
    let in_b = registration(ROOM_B, ALICE, "m.call#", "m2");
    register_connected(&h.monitor, &in_a).await;
    register_connected(&h.monitor, &in_b).await;

    h.monitor.sweep_now().await;

    assert_eq!(recv_timeout(&mut h.removed_rx, "removal").await, in_a.key());
    assert_quiet(&mut h.removed_rx, "second removal").await;
    assert_eq!(
        keys(&h.monitor.snapshot().await),
        HashSet::from([in_b.key()])
    );
    h.cancel.cancel();
    h.monitor.close().await;
}

// ── the homeserver leaving ───────────────────────────────────────────────────

/// Once the homeserver has left a room — the membership query is refused
/// and MSC4502 confirms the server is gone — every LiveKit room of that
/// Matrix room is deleted and all of its participants, pending ones
/// included, are dropped and revoked.
#[tokio::test]
async fn server_left_deletes_livekit_rooms() {
    let mut h = new_harness(
        MonitorTestDeps {
            get_joined_members_fn: not_in_room(),
            is_server_joined_fn: server_joined(false),
            participant_exists_fn: Some(Box::new(|_, _| Ok(false))),
            ..Default::default()
        },
        None,
    );
    let bob_1 = registration(ROOM_A, BOB, "m.call#", "m1");
    let bob_2 = registration(ROOM_A, BOB, "m.call#other", "m2");
    let pending = registration(ROOM_A, ALICE, "m.call#", "m3");
    let elsewhere = registration(ROOM_B, ALICE, "m.call#", "m4");
    register_connected(&h.monitor, &bob_1).await;
    register_connected(&h.monitor, &bob_2).await;
    h.monitor.register(pending.clone()).await;
    h.monitor.register(elsewhere.clone()).await;

    h.monitor.sweep_now().await;

    let deleted = HashSet::from([
        recv_timeout(&mut h.deleted_rx, "first room deletion").await,
        recv_timeout(&mut h.deleted_rx, "second room deletion").await,
    ]);
    assert_eq!(
        deleted,
        HashSet::from([bob_1.livekit_room.clone(), bob_2.livekit_room.clone()])
    );
    assert_quiet(&mut h.removed_rx, "individual removal").await;

    let mut revoked = HashSet::new();
    for _ in 0..3 {
        revoked.insert(recv_bounded_timeout(&mut h.revoked_rx, "revocation").await);
    }
    assert_eq!(
        revoked,
        HashSet::from([bob_1.key(), bob_2.key(), pending.key()])
    );

    // Room B was also refused, so its LiveKit room goes too.
    assert_eq!(
        recv_timeout(&mut h.deleted_rx, "third room deletion").await,
        elsewhere.livekit_room
    );
    assert!(h.monitor.snapshot().await.is_empty());
    h.cancel.cancel();
    h.monitor.close().await;
}

/// A refused membership query while the homeserver is still in the room is
/// a misconfiguration, not a departure: nothing is removed.
#[tokio::test]
async fn refusal_with_server_joined_fails_open() {
    let mut h = new_harness(
        MonitorTestDeps {
            get_joined_members_fn: not_in_room(),
            is_server_joined_fn: server_joined(true),
            ..Default::default()
        },
        None,
    );
    let alice = registration(ROOM_A, ALICE, "m.call#", "m1");
    register_connected(&h.monitor, &alice).await;

    h.monitor.sweep_now().await;

    assert_quiet(&mut h.removed_rx, "removal").await;
    assert_quiet(&mut h.deleted_rx, "room deletion").await;
    assert_eq!(
        keys(&h.monitor.snapshot().await),
        HashSet::from([alice.key()])
    );
    h.cancel.cancel();
    h.monitor.close().await;
}

/// A refused membership query whose confirmation fails leaves the room
/// alone as well.
#[tokio::test]
async fn refusal_with_unverifiable_server_membership_fails_open() {
    let mut h = new_harness(
        MonitorTestDeps {
            get_joined_members_fn: not_in_room(),
            is_server_joined_fn: Some(Box::new(|_, _| Err("boom".into()))),
            ..Default::default()
        },
        None,
    );
    let alice = registration(ROOM_A, ALICE, "m.call#", "m1");
    register_connected(&h.monitor, &alice).await;

    h.monitor.sweep_now().await;

    assert_quiet(&mut h.removed_rx, "removal").await;
    assert_quiet(&mut h.deleted_rx, "room deletion").await;
    assert_eq!(
        keys(&h.monitor.snapshot().await),
        HashSet::from([alice.key()])
    );
    h.cancel.cancel();
    h.monitor.close().await;
}

// ── failing open ──────────────────────────────────────────────────────────────

/// A homeserver that cannot answer the membership query leaves everyone
/// connected.
#[tokio::test]
async fn homeserver_error_fails_open() {
    let mut h = new_harness(
        MonitorTestDeps {
            get_joined_members_fn: Some(Box::new(|_| {
                Err(MembershipQueryError::Other("503".into()))
            })),
            ..Default::default()
        },
        None,
    );
    let alice = registration(ROOM_A, ALICE, "m.call#", "m1");
    register_connected(&h.monitor, &alice).await;

    h.monitor.sweep_now().await;

    assert_quiet(&mut h.removed_rx, "removal").await;
    assert_eq!(
        keys(&h.monitor.snapshot().await),
        HashSet::from([alice.key()])
    );
    h.cancel.cancel();
    h.monitor.close().await;
}

/// Without a resolvable Client-Server API no membership query is even
/// attempted and everyone stays.
#[tokio::test]
async fn unresolvable_cs_api_fails_open() {
    let mut h = new_harness_with(
        MonitorTestDeps {
            get_joined_members_fn: members(&[]),
            ..Default::default()
        },
        None,
        Duration::from_secs(60 * 60),
        failing_lookup(),
    );
    let alice = registration(ROOM_A, ALICE, "m.call#", "m1");
    register_connected(&h.monitor, &alice).await;

    h.monitor.sweep_now().await;

    assert!(h.joined_members_calls.lock().unwrap().is_empty());
    assert_quiet(&mut h.removed_rx, "removal").await;
    assert_eq!(
        keys(&h.monitor.snapshot().await),
        HashSet::from([alice.key()])
    );
    h.cancel.cancel();
    h.monitor.close().await;
}

// ── webhook bookkeeping ───────────────────────────────────────────────────────

/// A participant who left the SFU is no longer tracked, so a later loss of
/// membership has nothing to act on.
#[tokio::test]
async fn participant_left_stops_tracking() {
    let mut h = new_harness(
        MonitorTestDeps {
            get_joined_members_fn: members(&[]),
            ..Default::default()
        },
        None,
    );
    let alice = registration(ROOM_A, ALICE, "m.call#", "m1");
    register_connected(&h.monitor, &alice).await;
    h.monitor
        .notify_sfu_event(SfuEvent::ParticipantLeft(alice.key()))
        .await;

    assert!(h.monitor.snapshot().await.is_empty());
    h.monitor.sweep_now().await;
    assert!(h.joined_members_calls.lock().unwrap().is_empty());
    assert_quiet(&mut h.removed_rx, "removal").await;
    h.cancel.cancel();
    h.monitor.close().await;
}

/// A finished LiveKit room takes its participants with it, and only them.
#[tokio::test]
async fn room_finished_stops_tracking_that_room_only() {
    let h = new_harness(MonitorTestDeps::default(), None);
    let in_slot_1 = registration(ROOM_A, ALICE, "m.call#", "m1");
    let in_slot_2 = registration(ROOM_A, BOB, "m.call#other", "m2");
    register_connected(&h.monitor, &in_slot_1).await;
    register_connected(&h.monitor, &in_slot_2).await;

    h.monitor
        .notify_sfu_event(SfuEvent::RoomFinished(in_slot_1.livekit_room.clone()))
        .await;

    assert_eq!(
        keys(&h.monitor.snapshot().await),
        HashSet::from([in_slot_2.key()])
    );
    h.cancel.cancel();
    h.monitor.close().await;
}

/// Connect events for participants the monitor never heard of are ignored.
#[tokio::test]
async fn untracked_join_is_ignored() {
    let h = new_harness(MonitorTestDeps::default(), None);
    let alice = registration(ROOM_A, ALICE, "m.call#", "m1");

    h.monitor
        .notify_sfu_event(SfuEvent::ParticipantJoined(alice.key()))
        .await;

    assert!(h.monitor.snapshot().await.is_empty());
    h.cancel.cancel();
    h.monitor.close().await;
}

/// Re-minting a token for a connected participant keeps them connected.
#[tokio::test]
async fn re_registration_keeps_connected_state() {
    let h = new_harness(MonitorTestDeps::default(), None);
    let alice = registration(ROOM_A, ALICE, "m.call#", "m1");
    register_connected(&h.monitor, &alice).await;

    h.monitor.register(alice.clone()).await;

    let snapshot = h.monitor.snapshot().await;
    assert_eq!(snapshot.len(), 1);
    assert!(snapshot[0].connected);
    h.cancel.cancel();
    h.monitor.close().await;
}

// ── apply_sweep_outcome ───────────────────────────────────────────────────────

fn tracked(
    reg: &Registration,
    connected: bool,
    registered_at: DateTime<Utc>,
) -> TrackedParticipant {
    TrackedParticipant {
        stored: StoredParticipant {
            matrix_room_id: reg.matrix_room_id.clone(),
            matrix_user_id: reg.matrix_user_id.clone(),
            livekit_room: reg.livekit_room.clone(),
            livekit_identity: reg.livekit_identity.clone(),
            registered_at,
        },
        connected,
    }
}

/// A pending participant confirmed absent from the SFU is forgotten once
/// their token has expired, and kept while it is still valid. Connected
/// participants are never expired this way.
#[test]
fn absent_pending_participants_expire_with_their_token() {
    let now = Utc::now();
    let ttl = chrono::Duration::from_std(PENDING_TTL).unwrap();
    let old = registration(ROOM_A, ALICE, "m.call#", "old");
    let young = registration(ROOM_A, ALICE, "m.call#", "young");
    let connected = registration(ROOM_A, ALICE, "m.call#", "connected");
    let mut participants = HashMap::from([
        (old.key(), tracked(&old, false, now - ttl)),
        (young.key(), tracked(&young, false, now - ttl / 2)),
        (connected.key(), tracked(&connected, true, now - ttl * 2)),
    ]);

    let actions = apply_sweep_outcome(
        &mut participants,
        SweepOutcome {
            present: vec![],
            absent: vec![old.key(), young.key(), connected.key()],
            rooms: vec![(ROOM_A.into(), RoomMembership::Unknown)],
        },
        now,
    );

    assert_eq!(
        actions,
        SweepActions {
            forgotten: vec![old.key()],
            ..Default::default()
        }
    );
    assert_eq!(
        participants.keys().cloned().collect::<HashSet<_>>(),
        HashSet::from([young.key(), connected.key()])
    );
}

/// Presence learnt in the same sweep counts for the kick decision, and a
/// pending non-member is not kicked.
#[test]
fn presence_learnt_in_sweep_enables_kick() {
    let now = Utc::now();
    let seen = registration(ROOM_A, ALICE, "m.call#", "seen");
    let unseen = registration(ROOM_A, ALICE, "m.call#", "unseen");
    let mut participants = HashMap::from([
        (seen.key(), tracked(&seen, false, now)),
        (unseen.key(), tracked(&unseen, false, now)),
    ]);

    let actions = apply_sweep_outcome(
        &mut participants,
        SweepOutcome {
            present: vec![seen.key()],
            absent: vec![unseen.key()],
            rooms: vec![(ROOM_A.into(), RoomMembership::Joined(HashSet::new()))],
        },
        now,
    );

    assert_eq!(
        actions,
        SweepActions {
            kick: vec![seen.key()],
            revoked: vec![seen.key()],
            ..Default::default()
        }
    );
    assert_eq!(
        participants.keys().cloned().collect::<HashSet<_>>(),
        HashSet::from([unseen.key()])
    );
}

/// The sweep input covers every Matrix room once and lists exactly the
/// pending participants.
#[test]
fn sweep_input_is_deduplicated() {
    let now = Utc::now();
    let a1 = registration(ROOM_A, ALICE, "m.call#", "m1");
    let a2 = registration(ROOM_A, BOB, "m.call#", "m2");
    let b = registration(ROOM_B, ALICE, "m.call#", "m3");
    let participants = HashMap::from([
        (a1.key(), tracked(&a1, true, now)),
        (a2.key(), tracked(&a2, false, now)),
        (b.key(), tracked(&b, false, now)),
    ]);

    let input = SweepInput::from_participants(&participants);

    assert_eq!(input.rooms, vec![ROOM_A.to_owned(), ROOM_B.to_owned()]);
    assert_eq!(
        input.pending.into_iter().collect::<HashSet<_>>(),
        HashSet::from([a2.key(), b.key()])
    );
}

// ── persistence ───────────────────────────────────────────────────────────────

/// Registrations are persisted and recovered by a fresh monitor, which then
/// verifies them against the SFU and enforces membership as usual. Kicked
/// participants are removed from the store.
#[tokio::test]
async fn participants_are_persisted_and_recovered() {
    let store = new_in_memory_store();
    let alice = registration(ROOM_A, ALICE, "m.call#", "m1");

    {
        let h = new_harness(MonitorTestDeps::default(), Some(store.clone()));
        register_connected(&h.monitor, &alice).await;
        // Make sure the registration was processed before shutting down.
        assert_eq!(h.monitor.snapshot().await.len(), 1);
        h.cancel.cancel();
        h.monitor.close().await;
    }
    let stored = store.all_participants().await.unwrap();
    assert_eq!(stored.len(), 1);
    assert_eq!(stored[0].key(), alice.key());
    assert_eq!(stored[0].matrix_room_id, ROOM_A);
    assert_eq!(stored[0].matrix_user_id, ALICE);

    let mut h = new_harness(
        MonitorTestDeps {
            get_joined_members_fn: members(&[]),
            ..Default::default()
        },
        Some(store.clone()),
    );
    let mut recovered = h.monitor.recovery_done();
    while !*recovered.borrow_and_update() {
        recovered.changed().await.unwrap();
    }

    // Recovery triggers a sweep of its own; with the participant present on
    // the SFU (the deps' default) and no longer a member, they are kicked.
    assert_eq!(
        recv_timeout(&mut h.removed_rx, "removal").await,
        alice.key()
    );
    assert_eq!(
        recv_bounded_timeout(&mut h.revoked_rx, "revocation").await,
        alice.key()
    );
    h.monitor.sweep_now().await; // Flushes the store writer's queue.
    assert!(h.monitor.snapshot().await.is_empty());
    h.cancel.cancel();
    h.monitor.close().await;
    assert!(store.all_participants().await.unwrap().is_empty());
}

/// Participants that leave the SFU are removed from the store too.
#[tokio::test]
async fn departed_participants_are_removed_from_the_store() {
    let store = new_in_memory_store();
    let alice = registration(ROOM_A, ALICE, "m.call#", "m1");
    let h = new_harness(MonitorTestDeps::default(), Some(store.clone()));
    register_connected(&h.monitor, &alice).await;

    h.monitor
        .notify_sfu_event(SfuEvent::ParticipantLeft(alice.key()))
        .await;
    // The snapshot round-trip guarantees the leave has been processed (and
    // its store delete queued) before the loop is shut down; `close` then
    // lets the store writer drain.
    assert!(h.monitor.snapshot().await.is_empty());
    h.cancel.cancel();
    h.monitor.close().await;

    assert!(store.all_participants().await.unwrap().is_empty());
}

// ── schedule ──────────────────────────────────────────────────────────────────

/// Sweeps run on the configured interval without being asked.
#[tokio::test(start_paused = true)]
async fn sweeps_run_periodically() {
    let mut h = new_harness_with(
        MonitorTestDeps {
            get_joined_members_fn: members(&[]),
            ..Default::default()
        },
        None,
        Duration::from_secs(10),
        resolving_lookup(),
    );
    let alice = registration(ROOM_A, ALICE, "m.call#", "m1");
    register_connected(&h.monitor, &alice).await;

    // Paused time auto-advances to the next timer once everything is idle,
    // so this resolves as soon as the first tick has fired and been acted on.
    let removed = tokio::time::timeout(Duration::from_secs(60), h.removed_rx.recv())
        .await
        .expect("expected the periodic sweep to kick the participant")
        .expect("channel closed");
    assert_eq!(removed, alice.key());
    h.cancel.cancel();
    h.monitor.close().await;
}

/// Cancelling the parent token shuts the monitor down; `close` waits for it.
#[tokio::test]
async fn close_after_parent_cancellation() {
    let h = new_harness(MonitorTestDeps::default(), None);
    h.cancel.cancel();
    tokio::time::timeout(Duration::from_secs(5), h.monitor.close())
        .await
        .expect("close should return promptly");
    // Messages after shutdown are dropped rather than blocking.
    h.monitor
        .register(registration(ROOM_A, ALICE, "m.call#", "m1"))
        .await;
}

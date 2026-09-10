// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//! Membership enforcement (MSC4195, "Kicking users from the SFU on room
//! leave/ban"): the service periodically reconciles the participants it
//! issued tokens for against the homeserver's `/joined_members` and removes
//! those who lost their room membership from the SFU.

use std::collections::HashMap;
use std::time::Duration;

use lk_jwt_service_integration_tests::{
    FakeHomeserver, FakeRedis, FakeSfu, Service, ServiceConfig, expect_joined_members_request,
    expect_no_delete_room_requests, expect_no_joined_members_requests,
    expect_no_remove_participant_requests, livekit_identity, livekit_room_alias, send_sfu_webhook,
    wait_for_delete_room_request, wait_for_joined_members_request, wait_for_participant_persisted,
    wait_for_participant_removed, wait_for_remove_participant_request,
};
use serde_json::{Value, json};

const AS_TOKEN: &str = "as_token";
const HS_TOKEN: &str = "hs_token";
const ORIGIN_SERVER: &str = "origin.example.org";

const ROOM_ID: &str = "!room:example.com";
const SLOT_ID: &str = "m.call#";

const GET_TOKEN_CS_PATH: &str = "/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token";
const GET_TOKEN_SS_PATH: &str =
    "/_matrix/federation/unstable/io.element.msc4195/rtc/livekit/get_token";

/// How long to wait for the service to act on a membership change. The
/// check interval in these tests is one second, so this leaves plenty of
/// headroom for CI.
const ENFORCEMENT_TIMEOUT: Duration = Duration::from_secs(8);

/// How long to wait before concluding that the service did *not* act. Covers
/// two check intervals.
const QUIET_PERIOD: Duration = Duration::from_millis(2500);

/// App-service configuration with the membership check running every
/// `interval_secs` seconds, as extra_env.
fn app_service_env(hs_server_name: &str, interval_secs: u64) -> HashMap<String, String> {
    HashMap::from([
        ("LIVEKIT_AS_TOKEN".to_owned(), AS_TOKEN.to_owned()),
        ("LIVEKIT_HS_TOKEN".to_owned(), HS_TOKEN.to_owned()),
        (
            "LIVEKIT_HS_SERVER_NAME".to_owned(),
            hs_server_name.to_owned(),
        ),
        (
            "LIVEKIT_MEMBERSHIP_CHECK_INTERVAL_SECONDS".to_owned(),
            interval_secs.to_string(),
        ),
    ])
}

/// The full service configuration these tests use: a fake homeserver, a fake
/// SFU and the membership check every second.
fn service_config(hs: &FakeHomeserver, sfu: &FakeSfu, redis: Option<&FakeRedis>) -> ServiceConfig {
    ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        livekit_url: Some(sfu.url().to_owned()),
        redis_url: redis.map(|r| r.url().to_owned()),
        extra_env: app_service_env(hs.server_name(), 1),
    }
}

/// Mints a token for `user_id` via the C-S endpoint (a local user) and
/// returns the LiveKit (room, identity) it was issued for.
async fn mint_local_token(
    svc: &Service,
    sfu: &FakeSfu,
    user_id: &str,
    member_id: &str,
) -> (String, String) {
    let body: Value = json!({
        "url": sfu.url(),
        "room_id": ROOM_ID,
        "slot_id": SLOT_ID,
        "member": {
            "id": member_id,
            "claimed_user_id": user_id,
            "claimed_device_id": "DEVICE",
        },
    });
    let resp = reqwest::Client::new()
        .post(format!("{}{GET_TOKEN_CS_PATH}", svc.base_url))
        .header("Content-Type", "application/json")
        .header("X-Matrix-User-Identifier", user_id)
        .header("Authorization", format!("Bearer {HS_TOKEN}"))
        .body(body.to_string())
        .send()
        .await
        .expect("request failed");
    let status = resp.status().as_u16();
    let text = resp.text().await.expect("failed to read response body");
    assert_eq!(status, 200, "minting a local token failed: {text}");
    (
        livekit_room_alias(ROOM_ID, SLOT_ID),
        livekit_identity(user_id, "DEVICE", member_id),
    )
}

/// Mints a token for the remote `user_id` via the S-S endpoint and returns
/// the LiveKit (room, identity) it was issued for.
async fn mint_remote_token(
    svc: &Service,
    sfu: &FakeSfu,
    user_id: &str,
    member_id: &str,
) -> (String, String) {
    let body: Value = json!({
        "url": sfu.url(),
        "user_id": user_id,
        "room_id": ROOM_ID,
        "slot_id": SLOT_ID,
        "member": {
            "id": member_id,
            "claimed_device_id": "DEVICE",
        },
    });
    let resp = reqwest::Client::new()
        .post(format!("{}{GET_TOKEN_SS_PATH}", svc.base_url))
        .header("Content-Type", "application/json")
        .header("X-Matrix-Origin", ORIGIN_SERVER)
        .header("Authorization", format!("Bearer {HS_TOKEN}"))
        .body(body.to_string())
        .send()
        .await
        .expect("request failed");
    let status = resp.status().as_u16();
    let text = resp.text().await.expect("failed to read response body");
    assert_eq!(status, 200, "minting a remote token failed: {text}");
    (
        livekit_room_alias(ROOM_ID, SLOT_ID),
        livekit_identity(user_id, "DEVICE", member_id),
    )
}

/// Reports `identity` as connected to `room`: present on the SFU and
/// announced through a webhook, like a real connect.
async fn connect(svc: &Service, sfu: &FakeSfu, room: &str, identity: &str) {
    sfu.set_participant_present(room, identity);
    send_sfu_webhook(svc, "participant_joined", room, identity, None).await;
}

/// A connected local participant whose user left the room is removed from
/// the SFU. The membership query authenticates as the application service
/// itself.
#[tokio::test]
async fn non_member_is_removed_from_sfu() {
    let hs = FakeHomeserver::new().await;
    let alice = hs.new_user("alice");
    let sfu = FakeSfu::new().await;
    hs.set_joined_members(ROOM_ID, &[&alice.user_id]);

    let svc = Service::start(service_config(&hs, &sfu, None)).await;
    let (room, identity) = mint_local_token(&svc, &sfu, &alice.user_id, "member-1").await;
    connect(&svc, &sfu, &room, &identity).await;

    // Still a member: the service checks, but leaves the participant be.
    wait_for_joined_members_request(&hs, ROOM_ID, ENFORCEMENT_TIMEOUT).await;
    expect_joined_members_request(&hs, ROOM_ID, AS_TOKEN);
    expect_no_remove_participant_requests(&sfu);

    // Alice leaves the room.
    hs.set_joined_members(ROOM_ID, &[]);
    wait_for_remove_participant_request(&sfu, &room, &identity, ENFORCEMENT_TIMEOUT).await;
    expect_no_delete_room_requests(&sfu);
}

/// A connected participant whose user stays in the room is left alone.
#[tokio::test]
async fn member_stays_connected() {
    let hs = FakeHomeserver::new().await;
    let alice = hs.new_user("alice");
    let sfu = FakeSfu::new().await;
    hs.set_joined_members(ROOM_ID, &[&alice.user_id, "@someone-else:example.com"]);

    let svc = Service::start(service_config(&hs, &sfu, None)).await;
    let (room, identity) = mint_local_token(&svc, &sfu, &alice.user_id, "member-1").await;
    connect(&svc, &sfu, &room, &identity).await;

    tokio::time::sleep(QUIET_PERIOD).await;
    expect_joined_members_request(&hs, ROOM_ID, AS_TOKEN);
    expect_no_remove_participant_requests(&sfu);
    expect_no_delete_room_requests(&sfu);
}

/// A federated participant, whose token was minted via the S-S endpoint, is
/// removed from the SFU once they leave the room — even while local users
/// remain.
#[tokio::test]
async fn remote_participant_is_removed_when_leaving() {
    let hs = FakeHomeserver::new().await;
    let alice = hs.new_user("alice");
    let sfu = FakeSfu::new().await;
    let bob = format!("@bob:{ORIGIN_SERVER}");
    hs.set_joined_members(ROOM_ID, &[&alice.user_id, &bob]);

    let svc = Service::start(service_config(&hs, &sfu, None)).await;
    let (room, alice_identity) = mint_local_token(&svc, &sfu, &alice.user_id, "member-1").await;
    let (_, bob_identity) = mint_remote_token(&svc, &sfu, &bob, "member-2").await;
    connect(&svc, &sfu, &room, &alice_identity).await;
    connect(&svc, &sfu, &room, &bob_identity).await;

    hs.set_joined_members(ROOM_ID, &[&alice.user_id]);
    wait_for_remove_participant_request(&sfu, &room, &bob_identity, ENFORCEMENT_TIMEOUT).await;

    let removed = sfu.remove_participant_requests();
    assert!(
        removed.iter().all(|r| r.identity == bob_identity),
        "expected only bob to be removed, got {removed:?}"
    );
    expect_no_delete_room_requests(&sfu);
}

/// Once the homeserver itself has left the room — the membership query is
/// refused and MSC4502 confirms the server is gone — the LiveKit room is
/// deleted, taking every remaining participant with it.
#[tokio::test]
async fn homeserver_leaving_room_deletes_livekit_room() {
    let hs = FakeHomeserver::new().await;
    let alice = hs.new_user("alice");
    let sfu = FakeSfu::new().await;
    let bob = format!("@bob:{ORIGIN_SERVER}");
    hs.set_joined_members(ROOM_ID, &[&alice.user_id, &bob]);

    let svc = Service::start(service_config(&hs, &sfu, None)).await;
    let (room, alice_identity) = mint_local_token(&svc, &sfu, &alice.user_id, "member-1").await;
    let (_, bob_identity) = mint_remote_token(&svc, &sfu, &bob, "member-2").await;
    connect(&svc, &sfu, &room, &alice_identity).await;
    connect(&svc, &sfu, &room, &bob_identity).await;

    // Alice, the last local user, leaves: the homeserver is out of the room.
    hs.set_room_left(ROOM_ID);
    hs.set_not_joined(ROOM_ID, hs.server_name());
    wait_for_delete_room_request(&sfu, &room, ENFORCEMENT_TIMEOUT).await;
    expect_no_remove_participant_requests(&sfu);
}

/// A refused membership query while the homeserver is still in the room
/// points at a misconfigured user namespace, not at anyone having left:
/// nothing is removed.
#[tokio::test]
async fn refused_membership_query_with_server_joined_fails_open() {
    let hs = FakeHomeserver::new().await;
    let alice = hs.new_user("alice");
    let sfu = FakeSfu::new().await;
    hs.set_room_left(ROOM_ID);
    // /is_joined reports every subject joined by default, the server too.

    let svc = Service::start(service_config(&hs, &sfu, None)).await;
    let (room, identity) = mint_local_token(&svc, &sfu, &alice.user_id, "member-1").await;
    connect(&svc, &sfu, &room, &identity).await;

    wait_for_joined_members_request(&hs, ROOM_ID, ENFORCEMENT_TIMEOUT).await;
    tokio::time::sleep(QUIET_PERIOD).await;
    expect_no_remove_participant_requests(&sfu);
    expect_no_delete_room_requests(&sfu);
}

/// A homeserver that cannot answer the membership query leaves everyone
/// connected.
#[tokio::test]
async fn homeserver_error_fails_open() {
    let hs = FakeHomeserver::new().await;
    let alice = hs.new_user("alice");
    let sfu = FakeSfu::new().await;
    hs.set_joined_members_status(503);

    let svc = Service::start(service_config(&hs, &sfu, None)).await;
    let (room, identity) = mint_local_token(&svc, &sfu, &alice.user_id, "member-1").await;
    connect(&svc, &sfu, &room, &identity).await;

    wait_for_joined_members_request(&hs, ROOM_ID, ENFORCEMENT_TIMEOUT).await;
    tokio::time::sleep(QUIET_PERIOD).await;
    expect_no_remove_participant_requests(&sfu);
    expect_no_delete_room_requests(&sfu);
}

/// A participant the SFU reported as having left is no longer tracked, so a
/// later loss of membership has nothing to act on — and nothing left to
/// query for.
#[tokio::test]
async fn participant_left_webhook_stops_tracking() {
    let hs = FakeHomeserver::new().await;
    let alice = hs.new_user("alice");
    let sfu = FakeSfu::new().await;
    hs.set_joined_members(ROOM_ID, &[]);

    let svc = Service::start(service_config(&hs, &sfu, None)).await;
    let (room, identity) = mint_local_token(&svc, &sfu, &alice.user_id, "member-1").await;
    // Left before the first check ran, as reported by the SFU.
    send_sfu_webhook(
        &svc,
        "participant_left",
        &room,
        &identity,
        Some("CLIENT_INITIATED"),
    )
    .await;
    // Still on the SFU as far as GetParticipant is concerned, to prove that
    // the service does not go looking.
    sfu.set_participant_present(&room, &identity);

    tokio::time::sleep(QUIET_PERIOD).await;
    expect_no_joined_members_requests(&hs);
    expect_no_remove_participant_requests(&sfu);
}

/// A participant who was issued a token but never connected is not removed;
/// the removal happens once the SFU reports them present — a missed join
/// webhook only delays it.
#[tokio::test]
async fn pending_participant_is_removed_once_connected() {
    let hs = FakeHomeserver::new().await;
    let alice = hs.new_user("alice");
    let sfu = FakeSfu::new().await;
    hs.set_joined_members(ROOM_ID, &[]);

    let svc = Service::start(service_config(&hs, &sfu, None)).await;
    let (room, identity) = mint_local_token(&svc, &sfu, &alice.user_id, "member-1").await;

    // Absent from the SFU: nothing to remove.
    wait_for_joined_members_request(&hs, ROOM_ID, ENFORCEMENT_TIMEOUT).await;
    tokio::time::sleep(QUIET_PERIOD).await;
    expect_no_remove_participant_requests(&sfu);

    // Present on the SFU, without any webhook saying so.
    sfu.set_participant_present(&room, &identity);
    wait_for_remove_participant_request(&sfu, &room, &identity, ENFORCEMENT_TIMEOUT).await;
}

/// Tracked participants are persisted and picked up again by a restarted
/// service, which then enforces membership for them as usual and removes
/// them from the store once done.
#[tokio::test]
async fn tracked_participants_survive_restart() {
    let hs = FakeHomeserver::new().await;
    let alice = hs.new_user("alice");
    let sfu = FakeSfu::new().await;
    let redis = FakeRedis::new().await;
    hs.set_joined_members(ROOM_ID, &[&alice.user_id]);

    let (room, identity) = {
        let svc = Service::start(service_config(&hs, &sfu, Some(&redis))).await;
        let (room, identity) = mint_local_token(&svc, &sfu, &alice.user_id, "member-1").await;
        connect(&svc, &sfu, &room, &identity).await;
        wait_for_participant_persisted(&redis, &room, &identity, ENFORCEMENT_TIMEOUT).await;
        (room, identity)
        // The service is killed here.
    };

    // Alice leaves while the service is down.
    hs.set_joined_members(ROOM_ID, &[]);

    let _svc = Service::start(service_config(&hs, &sfu, Some(&redis))).await;
    wait_for_remove_participant_request(&sfu, &room, &identity, ENFORCEMENT_TIMEOUT).await;
    wait_for_participant_removed(&redis, &room, &identity, ENFORCEMENT_TIMEOUT).await;
}

/// A zero check interval disables membership enforcement entirely.
#[tokio::test]
async fn zero_interval_disables_enforcement() {
    let hs = FakeHomeserver::new().await;
    let alice = hs.new_user("alice");
    let sfu = FakeSfu::new().await;
    hs.set_joined_members(ROOM_ID, &[]);

    let mut config = service_config(&hs, &sfu, None);
    config.extra_env = app_service_env(hs.server_name(), 0);
    let svc = Service::start(config).await;
    let (room, identity) = mint_local_token(&svc, &sfu, &alice.user_id, "member-1").await;
    connect(&svc, &sfu, &room, &identity).await;

    tokio::time::sleep(QUIET_PERIOD).await;
    expect_no_joined_members_requests(&hs);
    expect_no_remove_participant_requests(&sfu);
}

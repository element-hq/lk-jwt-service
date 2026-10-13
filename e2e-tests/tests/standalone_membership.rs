// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use lk_jwt_service_e2e_tests::{
    AUTH_SERVICE_A_URL, AUTH_SERVICE_B_URL, LIVEKIT_A_SFU_ADDR, LIVEKIT_A_URL, LIVEKIT_B_SFU_ADDR,
    MatrixUser, SYNAPSE_A_CS_API_URL, SYNAPSE_A_SERVER_NAME, SYNAPSE_B_CS_API_URL,
    assert_stack_is_up, create_and_join_room, get_livekit_token, join_room_via, register_user,
    request_openid_token, verify_livekit_token_is_usable,
};

const SLOT_ID: &str = "m.call#ROOM";
const MEMBER_ID: &str = "e2e-member";
const DEVICE_ID: &str = "e2e-device";

/// POSTs `body` to the standalone `path` on the service at `service_url`
/// and returns the raw status and JSON body, so callers can check either a
/// token or an expected error.
async fn post_standalone(
    service_url: &str,
    path: &str,
    body: serde_json::Value,
) -> (reqwest::StatusCode, serde_json::Value) {
    let resp = reqwest::Client::new()
        .post(format!("{service_url}{path}"))
        .json(&body)
        .send()
        .await
        .unwrap_or_else(|e| panic!("request to {path} failed: {e}"));
    let status = resp.status();
    let body: serde_json::Value = resp
        .json()
        .await
        .unwrap_or_else(|e| panic!("{path} response was not valid JSON: {e}"));
    (status, body)
}

/// Calls the deprecated `/sfu/get` on `service_url` as `user` (of the
/// homeserver behind `cs_api_url`) for `room_id`.
async fn sfu_get(
    service_url: &str,
    cs_api_url: &str,
    user: &MatrixUser,
    room_id: &str,
) -> (reqwest::StatusCode, serde_json::Value) {
    let openid_token = request_openid_token(cs_api_url, user).await;
    post_standalone(
        service_url,
        "/sfu/get",
        serde_json::json!({
            "room": room_id,
            "openid_token": openid_token,
            "device_id": DEVICE_ID,
        }),
    )
    .await
}

/// Calls the standalone `/get_token` on `service_url` as `user` (of the
/// homeserver behind `cs_api_url`) for `room_id`.
async fn get_token(
    service_url: &str,
    cs_api_url: &str,
    user: &MatrixUser,
    room_id: &str,
) -> (reqwest::StatusCode, serde_json::Value) {
    let openid_token = request_openid_token(cs_api_url, user).await;
    post_standalone(
        service_url,
        "/get_token",
        serde_json::json!({
            "room_id": room_id,
            "slot_id": SLOT_ID,
            "openid_token": openid_token,
            "member": {
                "id": MEMBER_ID,
                "claimed_user_id": user.user_id,
                "claimed_device_id": DEVICE_ID,
            },
        }),
    )
    .await
}

/// Calls the standalone `/delegate_delayed_leave` on `service_url` as
/// `user` (of the homeserver behind `cs_api_url`) for `room_id`, handing
/// over `delay_id`.
async fn delegate_delayed_leave(
    service_url: &str,
    cs_api_url: &str,
    user: &MatrixUser,
    room_id: &str,
    delay_id: &str,
) -> (reqwest::StatusCode, serde_json::Value) {
    let openid_token = request_openid_token(cs_api_url, user).await;
    post_standalone(
        service_url,
        "/delegate_delayed_leave",
        serde_json::json!({
            "room_id": room_id,
            "slot_id": SLOT_ID,
            "openid_token": openid_token,
            "member": {
                "id": MEMBER_ID,
                "claimed_user_id": user.user_id,
                "claimed_device_id": DEVICE_ID,
            },
            "delay_id": delay_id,
            "delay_timeout": 10_000,
        }),
    )
    .await
}

/// Schedules a delayed `m.room.message` in `room_id` (MSC4140) as `user`
/// and returns its delay_id.
async fn schedule_delayed_message(cs_api_url: &str, user: &MatrixUser, room_id: &str) -> String {
    let mut url = reqwest::Url::parse(cs_api_url).expect("invalid CS API URL");
    url.path_segments_mut()
        .expect("cs_api_url cannot be a base")
        .extend([
            "_matrix",
            "client",
            "unstable",
            "org.matrix.msc4140",
            "rooms",
            room_id,
            "delayed_event",
            "m.room.message",
            "e2e-standalone-delayed-leave-txn",
        ]);

    let resp = reqwest::Client::new()
        .put(url)
        .bearer_auth(&user.access_token)
        .json(&serde_json::json!({
            "delay_ms": 10_000,
            "content": {"msgtype": "m.text", "body": "e2e-standalone-delayed-leave"},
        }))
        .send()
        .await
        .expect("request to schedule a delayed event failed");
    let status = resp.status();
    let body: serde_json::Value = resp
        .json()
        .await
        .expect("schedule-delayed-event response was not valid JSON");
    assert!(
        status.is_success(),
        "scheduling the delayed event failed: {status}: {body}"
    );
    body["delay_id"]
        .as_str()
        .expect("schedule-delayed-event response is missing `delay_id`")
        .to_owned()
}

#[track_caller]
fn expect_jwt(status: reqwest::StatusCode, body: &serde_json::Value) -> String {
    assert!(
        status.is_success(),
        "expected a token for a member, got {status}: {body}"
    );
    body["jwt"]
        .as_str()
        .unwrap_or_else(|| panic!("response is missing `jwt`: {body}"))
        .to_owned()
}

#[track_caller]
fn expect_forbidden(status: reqwest::StatusCode, body: &serde_json::Value) {
    assert_eq!(
        status.as_u16(),
        403,
        "expected 403 for a non-member, got {status}: {body}"
    );
    assert_eq!(
        body["errcode"].as_str(),
        Some("M_FORBIDDEN"),
        "body: {body}"
    );
}

// ── Stack A: membership is enforced ──────────────────────────────────────────
//
// On stack A, the service is the application service of its only full-access
// homeserver. Thus, room membership is enforced on the standalone endpoints.

// ── /sfu/get ─────────────────────────────────────────────────────────────────

/// A member of the room gets a token from `/sfu/get`, and the SFU accepts it.
#[tokio::test]
async fn membership_sfu_get_serves_member() {
    assert_stack_is_up();

    let alice = register_user(SYNAPSE_A_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_A_CS_API_URL, &alice).await;

    let (status, body) = sfu_get(AUTH_SERVICE_A_URL, SYNAPSE_A_CS_API_URL, &alice, &room_id).await;

    let jwt = expect_jwt(status, &body);
    verify_livekit_token_is_usable(LIVEKIT_A_SFU_ADDR, &jwt).await;
}

/// A valid OpenID token alone is not enough for `/sfu/get`: a user who is
/// not in the room is refused.
#[tokio::test]
async fn membership_sfu_get_rejects_non_member() {
    assert_stack_is_up();

    // Alice creates (and thus joins) a room; Bob never joins it.
    let alice = register_user(SYNAPSE_A_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_A_CS_API_URL, &alice).await;
    let bob = register_user(SYNAPSE_A_CS_API_URL, "bob", "e2e-test-password").await;

    let (status, body) = sfu_get(AUTH_SERVICE_A_URL, SYNAPSE_A_CS_API_URL, &bob, &room_id).await;

    expect_forbidden(status, &body);
}

// ── /get_token ───────────────────────────────────────────────────────────────

/// A member of the room gets a token from `/get_token`, and the SFU accepts
/// it.
#[tokio::test]
async fn membership_get_token_serves_member() {
    assert_stack_is_up();

    let alice = register_user(SYNAPSE_A_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_A_CS_API_URL, &alice).await;

    let (status, body) =
        get_token(AUTH_SERVICE_A_URL, SYNAPSE_A_CS_API_URL, &alice, &room_id).await;

    let jwt = expect_jwt(status, &body);
    verify_livekit_token_is_usable(LIVEKIT_A_SFU_ADDR, &jwt).await;
}

/// A valid OpenID token alone is not enough for `/get_token`: a user who is
/// not in the room is refused.
#[tokio::test]
async fn membership_get_token_rejects_non_member() {
    assert_stack_is_up();

    // Alice creates (and thus joins) a room; Bob never joins it.
    let alice = register_user(SYNAPSE_A_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_A_CS_API_URL, &alice).await;
    let bob = register_user(SYNAPSE_A_CS_API_URL, "bob", "e2e-test-password").await;

    let (status, body) = get_token(AUTH_SERVICE_A_URL, SYNAPSE_A_CS_API_URL, &bob, &room_id).await;

    expect_forbidden(status, &body);
}

/// A user of another homeserver (restricted for stack A's service) who has
/// joined the room over federation is served: their membership is read
/// from hs A's room state, which saw the join.
#[tokio::test]
async fn membership_get_token_serves_joined_remote_user() {
    assert_stack_is_up();

    // Alice creates (and thus joins) a room on hs A and gets a token first,
    // which is what creates the LiveKit room a restricted user can join.
    let alice = register_user(SYNAPSE_A_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_A_CS_API_URL, &alice).await;
    get_livekit_token(
        SYNAPSE_A_CS_API_URL,
        &alice,
        LIVEKIT_A_URL,
        &room_id,
        SLOT_ID,
        "e2e-member-alice",
    )
    .await;

    // Bob, on hs B, joins the room over federation and asks stack A's
    // service directly, with an OpenID token from his own homeserver.
    let bob = register_user(SYNAPSE_B_CS_API_URL, "bob", "e2e-test-password").await;
    join_room_via(SYNAPSE_B_CS_API_URL, &bob, &room_id, SYNAPSE_A_SERVER_NAME).await;

    let (status, body) = get_token(AUTH_SERVICE_A_URL, SYNAPSE_B_CS_API_URL, &bob, &room_id).await;

    let jwt = expect_jwt(status, &body);
    verify_livekit_token_is_usable(LIVEKIT_A_SFU_ADDR, &jwt).await;
}

/// A user of another homeserver who never joined the room is refused, even
/// though their OpenID token is valid.
#[tokio::test]
async fn membership_get_token_rejects_remote_non_member() {
    assert_stack_is_up();

    let alice = register_user(SYNAPSE_A_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_A_CS_API_URL, &alice).await;
    let bob = register_user(SYNAPSE_B_CS_API_URL, "bob", "e2e-test-password").await;

    let (status, body) = get_token(AUTH_SERVICE_A_URL, SYNAPSE_B_CS_API_URL, &bob, &room_id).await;

    expect_forbidden(status, &body);
}

// ── /delegate_delayed_leave ──────────────────────────────────────────────────

/// A member of the room can hand a delayed leave to the service.
#[tokio::test]
async fn membership_delegate_delayed_leave_serves_member() {
    assert_stack_is_up();

    let alice = register_user(SYNAPSE_A_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_A_CS_API_URL, &alice).await;
    let delay_id = schedule_delayed_message(SYNAPSE_A_CS_API_URL, &alice, &room_id).await;

    let (status, body) = delegate_delayed_leave(
        AUTH_SERVICE_A_URL,
        SYNAPSE_A_CS_API_URL,
        &alice,
        &room_id,
        &delay_id,
    )
    .await;

    assert!(
        status.is_success(),
        "expected a successful response for a member, got {status}: {body}"
    );
    assert_eq!(
        body.as_object().map(|o| o.len()),
        Some(0),
        "expected an empty response object, got {body}"
    );
}

/// A valid OpenID token alone is not enough for `/delegate_delayed_leave`:
/// a user who is not in the room is refused before the service touches any
/// delayed event.
#[tokio::test]
async fn membership_delegate_delayed_leave_rejects_non_member() {
    assert_stack_is_up();

    // Alice creates (and thus joins) a room; Bob never joins it, so he
    // cannot even have a delayed event there: the membership check refuses
    // him before the delay_id is looked at.
    let alice = register_user(SYNAPSE_A_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_A_CS_API_URL, &alice).await;
    let bob = register_user(SYNAPSE_A_CS_API_URL, "bob", "e2e-test-password").await;

    let (status, body) = delegate_delayed_leave(
        AUTH_SERVICE_A_URL,
        SYNAPSE_A_CS_API_URL,
        &bob,
        &room_id,
        "syd_e2e_non_member",
    )
    .await;

    expect_forbidden(status, &body);
}

// ── Stack B: membership not enforced ─────────────────────────────────────────
//
// Stack B's service lists a second full-access homeserver, so its homeserver
// cannot vouch for every room with a call on its SFU and the standalone
// endpoints are served on the OpenID token alone, as they were before the
// check existed.

/// On stack B a non-member gets a token from `/sfu/get`, and the SFU
/// accepts it.
#[tokio::test]
async fn membership_not_enforced_on_stack_b_sfu_get_serves_non_member() {
    assert_stack_is_up();

    // Alice creates (and thus joins) a room on hs B; Bob never joins it.
    let alice = register_user(SYNAPSE_B_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_B_CS_API_URL, &alice).await;
    let bob = register_user(SYNAPSE_B_CS_API_URL, "bob", "e2e-test-password").await;

    let (status, body) = sfu_get(AUTH_SERVICE_B_URL, SYNAPSE_B_CS_API_URL, &bob, &room_id).await;

    let jwt = expect_jwt(status, &body);
    verify_livekit_token_is_usable(LIVEKIT_B_SFU_ADDR, &jwt).await;
}

/// On stack B a non-member gets a token from `/get_token`, and the SFU
/// accepts it.
#[tokio::test]
async fn membership_not_enforced_on_stack_b_get_token_serves_non_member() {
    assert_stack_is_up();

    // Alice creates (and thus joins) a room on hs B; Bob never joins it.
    let alice = register_user(SYNAPSE_B_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_B_CS_API_URL, &alice).await;
    let bob = register_user(SYNAPSE_B_CS_API_URL, "bob", "e2e-test-password").await;

    let (status, body) = get_token(AUTH_SERVICE_B_URL, SYNAPSE_B_CS_API_URL, &bob, &room_id).await;

    let jwt = expect_jwt(status, &body);
    verify_livekit_token_is_usable(LIVEKIT_B_SFU_ADDR, &jwt).await;
}

/// On stack B a non-member can hand a delayed leave to the service: the
/// request is accepted on the OpenID token alone.
#[tokio::test]
async fn membership_not_enforced_on_stack_b_delegate_delayed_leave_serves_non_member() {
    assert_stack_is_up();

    // Alice creates (and thus joins) a room on hs B; Bob never joins it, so
    // he cannot have a delayed event there. The standalone endpoint does not
    // look the delay_id up when registering the job, so a made-up one shows
    // the request itself getting through; the job then finds no participant
    // on the SFU, fails to send the non-existent event and gives up.
    let alice = register_user(SYNAPSE_B_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_B_CS_API_URL, &alice).await;
    let bob = register_user(SYNAPSE_B_CS_API_URL, "bob", "e2e-test-password").await;

    let (status, body) = delegate_delayed_leave(
        AUTH_SERVICE_B_URL,
        SYNAPSE_B_CS_API_URL,
        &bob,
        &room_id,
        "syd_e2e_non_member",
    )
    .await;

    assert!(
        status.is_success(),
        "expected the request to be accepted without a membership check, got {status}: {body}"
    );
    assert_eq!(
        body.as_object().map(|o| o.len()),
        Some(0),
        "expected an empty response object, got {body}"
    );
}

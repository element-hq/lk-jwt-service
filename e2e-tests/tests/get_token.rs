// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use lk_jwt_service_e2e_tests::{
    LIVEKIT_A_SFU_ADDR, LIVEKIT_A_URL, MatrixUser, SYNAPSE_A_CS_API_URL, SYNAPSE_A_SERVER_NAME,
    SYNAPSE_B_CS_API_URL, assert_stack_is_up, attempt_publish_track, create_and_join_room,
    get_livekit_token, join_room_via, register_user, verify_livekit_token_is_usable,
};

/// Low-level POST to `/rtc/livekit/get_token` through `cs_api_url` as `user`,
/// optionally naming `server_name` as the SFU-hosting homeserver to relay
/// through via the MSC4512 federation proxy — the way a federated
/// participant does. Returns the raw status and JSON body without asserting
/// on success, so callers can check either an issued token or an expected
/// error.
async fn request_get_token(
    cs_api_url: &str,
    user: &MatrixUser,
    server_name: Option<&str>,
    livekit_url: &str,
    room_id: &str,
    member_id: &str,
    device_id: &str,
) -> (reqwest::StatusCode, serde_json::Value) {
    let mut json = serde_json::json!({
        "room_id": room_id,
        "slot_id": "m.call#ROOM",
        "url": livekit_url,
        "member": {
            "id": member_id,
            "claimed_device_id": device_id,
        },
    });
    if let Some(server_name) = server_name {
        json["server_name"] = server_name.into();
    }

    let resp = reqwest::Client::new()
        .post(format!(
            "{cs_api_url}/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token"
        ))
        .bearer_auth(&user.access_token)
        .json(&json)
        .send()
        .await
        .expect("request to /rtc/livekit/get_token failed");
    let status = resp.status();
    let body: serde_json::Value = resp.json().await.expect("response was not valid JSON");
    (status, body)
}

/// Like [`request_get_token`], but names `target_server_name` as the
/// SFU-hosting homeserver and asserts that the request succeeds, returning
/// the issued JWT.
async fn get_relayed_livekit_token(
    cs_api_url: &str,
    user: &MatrixUser,
    target_server_name: &str,
    livekit_url: &str,
    room_id: &str,
    member_id: &str,
    device_id: &str,
) -> String {
    let (status, body) = request_get_token(
        cs_api_url,
        user,
        Some(target_server_name),
        livekit_url,
        room_id,
        member_id,
        device_id,
    )
    .await;
    assert!(
        status.is_success(),
        "expected a successful get_token response, got {status}: {body}"
    );
    body["jwt"]
        .as_str()
        .unwrap_or_else(|| panic!("get_token response is missing `jwt`: {body}"))
        .to_owned()
}

/// A joined user succeeds in getting a token for the local SFU.
#[tokio::test]
async fn get_token_local_sfu_succeeds() {
    assert_stack_is_up();

    // Register a user and have them create (and thus join) a room.
    let user = register_user(SYNAPSE_A_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_A_CS_API_URL, &user).await;

    // Request a LiveKit token for that room through Synapse's C-S API. Synapse
    // proxies the request to its lk-jwt-service running as an application
    // service.
    let jwt = get_livekit_token(
        SYNAPSE_A_CS_API_URL,
        &user,
        LIVEKIT_A_URL,
        &room_id,
        "m.call#ROOM",
        "e2e-member",
        "E2EDEVICE",
    )
    .await;

    // The proof that matters: a real LiveKit SFU actually accepts the
    // issued token and admits the participant into the room.
    verify_livekit_token_is_usable(LIVEKIT_A_SFU_ADDR, &jwt).await;
}

/// A joined user succeeds in getting a token for a remote SFU.
#[tokio::test]
async fn get_token_remote_sfu_succeeds() {
    assert_stack_is_up();

    // Alice creates (and thus joins) a room on her own homeserver (hs A).
    let alice = register_user(SYNAPSE_A_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_A_CS_API_URL, &alice).await;

    // Alice requests a token locally first, as the real MSC4195 flow expects:
    // this is what actually creates the LiveKit room (room.auto_create is
    // disabled), which the relayed request below never does on its own.
    get_livekit_token(
        SYNAPSE_A_CS_API_URL,
        &alice,
        LIVEKIT_A_URL,
        &room_id,
        "m.call#ROOM",
        "e2e-member-alice",
        "E2EDEVICEALICE",
    )
    .await;

    // Bob, on a different, federated homeserver (hs B), joins the same room.
    let bob = register_user(SYNAPSE_B_CS_API_URL, "bob", "e2e-test-password").await;
    join_room_via(SYNAPSE_B_CS_API_URL, &bob, &room_id, SYNAPSE_A_SERVER_NAME).await;

    // Bob requests a token through his own homeserver's C-S API, naming hs A
    // as the MatrixRTC session's SFU-hosting homeserver. hs B relays this to
    // hs A via the MSC4512 federation proxy, which in turn calls the S-S
    // endpoint on hs A's app service.
    let jwt = get_relayed_livekit_token(
        SYNAPSE_B_CS_API_URL,
        &bob,
        SYNAPSE_A_SERVER_NAME,
        LIVEKIT_A_URL,
        &room_id,
        "e2e-member",
        "E2EDEVICE",
    )
    .await;

    // The proof that matters: hs A's real LiveKit SFU actually accepts the
    // issued token, even though it was requested through hs B.
    verify_livekit_token_is_usable(LIVEKIT_A_SFU_ADDR, &jwt).await;
}

/// A remote user's token only grants subscribe rights: unlike a local
/// user's, it cannot be used to publish into the room.
#[tokio::test]
async fn get_token_remote_sfu_cannot_publish() {
    assert_stack_is_up();

    // Alice creates (and thus joins) a room on her own homeserver (hs A) and
    // requests a token locally first, as the real MSC4195 flow expects: this
    // is what actually creates the LiveKit room (room.auto_create is
    // disabled), which the relayed request below never does on its own.
    let alice = register_user(SYNAPSE_A_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_A_CS_API_URL, &alice).await;
    let alice_jwt = get_livekit_token(
        SYNAPSE_A_CS_API_URL,
        &alice,
        LIVEKIT_A_URL,
        &room_id,
        "m.call#ROOM",
        "e2e-member-alice",
        "E2EDEVICEALICE",
    )
    .await;

    // Bob, on a different, federated homeserver (hs B), joins the same room
    // and requests a token through his own homeserver's C-S API, naming hs A
    // as the MatrixRTC session's SFU-hosting homeserver. hs B relays this to
    // hs A via the MSC4512 federation proxy, which in turn calls the S-S
    // endpoint on hs A's app service — the path that never grants publish
    // rights.
    let bob = register_user(SYNAPSE_B_CS_API_URL, "bob", "e2e-test-password").await;
    join_room_via(SYNAPSE_B_CS_API_URL, &bob, &room_id, SYNAPSE_A_SERVER_NAME).await;
    let bob_jwt = get_relayed_livekit_token(
        SYNAPSE_B_CS_API_URL,
        &bob,
        SYNAPSE_A_SERVER_NAME,
        LIVEKIT_A_URL,
        &room_id,
        "e2e-member-bob",
        "E2EDEVICEBOB",
    )
    .await;

    // The proof that matters: the real LiveKit SFU actually enforces the
    // difference. As a positive control, Alice's local token can publish...
    assert!(
        attempt_publish_track(LIVEKIT_A_SFU_ADDR, &alice_jwt).await,
        "expected alice's local token to be usable for publishing"
    );
    // ...while Bob's remote token, issued for the exact same room, cannot.
    assert!(
        !attempt_publish_track(LIVEKIT_A_SFU_ADDR, &bob_jwt).await,
        "expected bob's remote token to be rejected for publishing"
    );
}

/// A non-joined user is rejected.
#[tokio::test]
async fn get_token_rejects_non_member() {
    assert_stack_is_up();

    // Alice creates (and thus joins) a room; Bob never joins it.
    let alice = register_user(SYNAPSE_A_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_A_CS_API_URL, &alice).await;
    let bob = register_user(SYNAPSE_A_CS_API_URL, "bob", "e2e-test-password").await;

    let (status, body) = request_get_token(
        SYNAPSE_A_CS_API_URL,
        &bob,
        None,
        LIVEKIT_A_URL,
        &room_id,
        "e2e-member",
        "E2EDEVICE",
    )
    .await;

    assert_eq!(
        status.as_u16(),
        403,
        "expected 403 for a non-member, got {status}: {body}"
    );
    assert_eq!(body["errcode"].as_str(), Some("M_FORBIDDEN"));
}

/// A joined user is rejected if the remote server isn't joined.
#[tokio::test]
async fn get_token_rejects_when_remote_server_is_not_joined() {
    assert_stack_is_up();

    // Bob creates (and thus joins) a room on hs B. hs A never joins it.
    let bob = register_user(SYNAPSE_B_CS_API_URL, "bob", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_B_CS_API_URL, &bob).await;

    // Bob requests a token naming hs A as the SFU-hosting homeserver, even
    // though hs A has never seen this room.
    let (status, body) = request_get_token(
        SYNAPSE_B_CS_API_URL,
        &bob,
        Some(SYNAPSE_A_SERVER_NAME),
        LIVEKIT_A_URL,
        &room_id,
        "e2e-member",
        "E2EDEVICE",
    )
    .await;

    assert_eq!(
        status.as_u16(),
        403,
        "expected the remote homeserver's 403 to be relayed, got {status}: {body}"
    );
    assert_eq!(body["errcode"].as_str(), Some("M_FORBIDDEN"));
}

// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use lk_jwt_service_e2e_tests::{
    LIVEKIT_A_SFU_ADDR, LIVEKIT_A_URL, SYNAPSE_A_CS_API_URL, SYNAPSE_A_SERVER_NAME,
    SYNAPSE_B_CS_API_URL, assert_stack_is_up, create_and_join_room, join_room_via, register_user,
    verify_livekit_token_is_usable,
};

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
    let resp = reqwest::Client::new()
        .post(format!(
            "{SYNAPSE_A_CS_API_URL}/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token"
        ))
        .bearer_auth(&alice.access_token)
        .json(&serde_json::json!({
            "room_id": room_id,
            "slot_id": "m.call#ROOM",
            "url": LIVEKIT_A_URL,
            "member": {
                "id": "e2e-member-alice",
                "claimed_device_id": "E2EDEVICEALICE",
            },
        }))
        .send()
        .await
        .expect("alice's request to /rtc/livekit/get_token failed");
    assert!(
        resp.status().is_success(),
        "expected alice's local get_token request to succeed, got {}",
        resp.status()
    );

    // Bob, on a different, federated homeserver (hs B), joins the same room.
    let bob = register_user(SYNAPSE_B_CS_API_URL, "bob", "e2e-test-password").await;
    join_room_via(SYNAPSE_B_CS_API_URL, &bob, &room_id, SYNAPSE_A_SERVER_NAME).await;

    // Bob requests a token through his own homeserver's C-S API, naming hs A
    // as the MatrixRTC session's SFU-hosting homeserver. hs B relays this to
    // hs A via the MSC4512 federation proxy, which in turn calls the S-S
    // endpoint on hs A's app service.
    let resp = reqwest::Client::new()
        .post(format!(
            "{SYNAPSE_B_CS_API_URL}/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token"
        ))
        .bearer_auth(&bob.access_token)
        .json(&serde_json::json!({
            "server_name": SYNAPSE_A_SERVER_NAME,
            "room_id": room_id,
            "slot_id": "m.call#ROOM",
            "url": LIVEKIT_A_URL,
            "member": {
                "id": "e2e-member",
                "claimed_device_id": "E2EDEVICE",
            },
        }))
        .send()
        .await
        .expect("request to /rtc/livekit/get_token failed");

    let status = resp.status();
    let body: serde_json::Value = resp.json().await.expect("response was not valid JSON");
    assert!(
        status.is_success(),
        "expected a successful response, got {status}: {body}"
    );

    let jwt = body["jwt"]
        .as_str()
        .unwrap_or_else(|| panic!("expected a `jwt` field in the response, got {body}"));
    assert!(!jwt.is_empty(), "expected a non-empty JWT");

    // The proof that matters: hs A's real LiveKit SFU actually accepts the
    // issued token, even though it was requested through hs B.
    verify_livekit_token_is_usable(LIVEKIT_A_SFU_ADDR, jwt).await;
}

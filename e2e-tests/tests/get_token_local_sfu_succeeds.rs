// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use lk_jwt_service_e2e_tests::{
    LIVEKIT_A_SFU_ADDR, LIVEKIT_A_URL, SYNAPSE_A_CS_API_URL, create_and_join_room, register_user,
    require_stack, verify_livekit_token_is_usable,
};

/// A joined user succeeds in getting a token for the local SFU.
#[tokio::test]
async fn get_token_local_sfu_succeeds() {
    require_stack();

    // Register a user and have them create (and thus join) a room.
    let user = register_user(SYNAPSE_A_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_A_CS_API_URL, &user).await;

    // Request a LiveKit token for that room through Synapse's C-S API. Synapse
    // proxies the request to its lk-jwt-service running as an application service.
    let resp = reqwest::Client::new()
        .post(format!(
            "{SYNAPSE_A_CS_API_URL}/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token"
        ))
        .bearer_auth(&user.access_token)
        .json(&serde_json::json!({
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

    // The proof that matters: a real LiveKit SFU actually accepts the
    // issued token and admits the participant into the room.
    verify_livekit_token_is_usable(LIVEKIT_A_SFU_ADDR, jwt).await;
}

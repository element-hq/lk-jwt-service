// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use lk_jwt_service_e2e_tests::{
    LIVEKIT_A_URL, SYNAPSE_A_SERVER_NAME, SYNAPSE_B_CS_API_URL, create_and_join_room,
    register_user, require_stack,
};

/// A joined user is rejected if the remote server isn't joined.
#[tokio::test]
async fn get_token_rejects_when_remote_server_is_not_joined() {
    require_stack();

    // Bob creates (and thus joins) a room on hs B. hs A never joins it.
    let bob = register_user(SYNAPSE_B_CS_API_URL, "bob", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_B_CS_API_URL, &bob).await;

    // Bob requests a token naming hs A as the SFU-hosting homeserver, even
    // though hs A has never seen this room.
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
    assert_eq!(
        status.as_u16(),
        502,
        "expected 502 when the remote homeserver isn't in the room, got {status}: {body}"
    );
    assert_eq!(body["errcode"].as_str(), Some("M_CONNECTION_FAILED"));
}

// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use lk_jwt_service_e2e_tests::{
    LIVEKIT_A_URL, SYNAPSE_A_CS_API_URL, assert_stack_is_up, create_and_join_room, register_user,
};

/// A non-joined user is rejected.
#[tokio::test]
async fn get_token_rejects_non_member() {
    assert_stack_is_up();

    // Alice creates (and thus joins) a room; Bob never joins it.
    let alice = register_user(SYNAPSE_A_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_A_CS_API_URL, &alice).await;
    let bob = register_user(SYNAPSE_A_CS_API_URL, "bob", "e2e-test-password").await;

    let resp = reqwest::Client::new()
        .post(format!(
            "{SYNAPSE_A_CS_API_URL}/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token"
        ))
        .bearer_auth(&bob.access_token)
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
    assert_eq!(
        status.as_u16(),
        403,
        "expected 403 for a non-member, got {status}: {body}"
    );
    assert_eq!(body["errcode"].as_str(), Some("M_FORBIDDEN"));
}

// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use std::time::{Duration, Instant};

use lk_jwt_service_e2e_tests::{SYNAPSE_CS_API_URL, Stack, create_and_join_room, register_user};

/// Schedules a delayed `m.room.message` in `room_id` (MSC4140) and returns
/// its delay_id.
async fn schedule_delayed_message(
    cs_api_url: &str,
    access_token: &str,
    room_id: &str,
    body: &str,
    delay_ms: u64,
) -> String {
    let mut url = reqwest::Url::parse(cs_api_url).expect("invalid CS API URL");
    url.path_segments_mut()
        .expect("cs_api_url cannot be a base")
        .extend([
            "_matrix",
            "client",
            "v3",
            "rooms",
            room_id,
            "send",
            "m.room.message",
            "e2e-delayed-leave-txn",
        ]);
    url.query_pairs_mut()
        .append_pair("org.matrix.msc4140.delay", &delay_ms.to_string());

    let resp = reqwest::Client::new()
        .put(url)
        .bearer_auth(access_token)
        .json(&serde_json::json!({"msgtype": "m.text", "body": body}))
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

/// Polls the room's message timeline until a message with the given body
/// appears, or panics once the timeout elapses.
async fn wait_for_message(
    cs_api_url: &str,
    access_token: &str,
    room_id: &str,
    body: &str,
    timeout: Duration,
) {
    let client = reqwest::Client::new();
    let deadline = Instant::now() + timeout;
    loop {
        let resp = client
            .get(format!(
                "{cs_api_url}/_matrix/client/v3/rooms/{room_id}/messages?dir=b&limit=20"
            ))
            .bearer_auth(access_token)
            .send()
            .await
            .expect("request to list room messages failed");
        let messages: serde_json::Value = resp
            .json()
            .await
            .expect("messages response was not valid JSON");
        let found = messages["chunk"]
            .as_array()
            .into_iter()
            .flatten()
            .any(|event| event["content"]["body"].as_str() == Some(body));
        if found {
            return;
        }
        if Instant::now() > deadline {
            panic!("timed out waiting for a message with body {body:?} to appear in {room_id}");
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}

/// With no participant ever connecting to the SFU, the service's waiting-state
/// timeout triggers sending the delayed event.
#[tokio::test]
async fn delegate_delayed_leave_cs_succeeds() {
    let _stack = Stack::start().await;

    // Register a user and have them create (and thus join) a room.
    let user = register_user(SYNAPSE_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_CS_API_URL, &user).await;

    // Schedule the delayed event directly against Synapse with a long
    // delay, as the client normally would before delegating.
    const DELAY_MS: u64 = 30_000;
    // Pass a much shorter delay to the service when delegating. This
    // will make the service trigger the send action once the shorter
    // delay elapses.
    const DELEGATED_DELAY_MS: u64 = 3_000;
    const MESSAGE_BODY: &str = "e2e-delegate-delayed-leave-proof";
    let delay_id = schedule_delayed_message(
        SYNAPSE_CS_API_URL,
        &user.access_token,
        &room_id,
        MESSAGE_BODY,
        DELAY_MS,
    )
    .await;

    // Delegate the delayed event's lifecycle to the service, through
    // Synapse's C-S API. Synapse proxies the request to its lk-jwt-service
    // running as an application service.
    let resp = reqwest::Client::new()
        .post(format!(
            "{SYNAPSE_CS_API_URL}/_matrix/client/unstable/io.element.msc4195/rtc/livekit/delegate_delayed_leave"
        ))
        .bearer_auth(&user.access_token)
        .json(&serde_json::json!({
            "room_id": room_id,
            "slot_id": "m.call#ROOM",
            "member": {
                "id": "e2e-member",
                "claimed_device_id": "E2EDEVICE",
            },
            "delay_id": delay_id,
            "delay_timeout": DELEGATED_DELAY_MS,
        }))
        .send()
        .await
        .expect("request to delegate_delayed_leave failed");
    let status = resp.status();
    let body: serde_json::Value = resp
        .json()
        .await
        .expect("delegate_delayed_leave response was not valid JSON");
    assert!(
        status.is_success(),
        "expected a successful response, got {status}: {body}"
    );
    assert_eq!(
        body.as_object().map(|o| o.len()),
        Some(0),
        "expected an empty response object, got {body}"
    );

    // No participant ever connects to the SFU. Once the service's own
    // waiting-state timeout (bounded by the faked DELEGATED_DELAY_MS) elapses,
    // it should trigger the send action.
    wait_for_message(
        SYNAPSE_CS_API_URL,
        &user.access_token,
        &room_id,
        MESSAGE_BODY,
        // The timeout is significantly below DELAY_MS so that we can be sure
        // that the event was triggered by the service.
        Duration::from_millis(DELEGATED_DELAY_MS) + Duration::from_secs(10),
    )
    .await;
}

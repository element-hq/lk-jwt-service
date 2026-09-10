// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use std::time::{Duration, Instant};

use lk_jwt_service_e2e_tests::{
    LIVEKIT_A_SFU_ADDR, LIVEKIT_A_URL, LiveKitParticipant, SYNAPSE_A_CS_API_URL,
    assert_stack_is_up, create_and_join_room, get_livekit_token, register_user,
};

/// Schedules a delayed `m.room.message` in `room_id` (MSC4140) and returns
/// its delay_id.
async fn schedule_delayed_message(
    cs_api_url: &str,
    access_token: &str,
    room_id: &str,
    txn_id: &str,
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
            txn_id,
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

/// Reports whether a message with the given body is in the last 20 messages of the room's timeline.
async fn has_message(cs_api_url: &str, access_token: &str, room_id: &str, body: &str) -> bool {
    let resp = reqwest::Client::new()
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
    messages["chunk"]
        .as_array()
        .into_iter()
        .flatten()
        .any(|event| event["content"]["body"].as_str() == Some(body))
}

/// Polls the end of the room's timeline until a message with the given body
/// appears, or panics once the timeout elapses.
async fn wait_for_message(
    cs_api_url: &str,
    access_token: &str,
    room_id: &str,
    body: &str,
    timeout: Duration,
) {
    let deadline = Instant::now() + timeout;
    loop {
        if has_message(cs_api_url, access_token, room_id, body).await {
            return;
        }
        if Instant::now() > deadline {
            panic!("timed out waiting for a message with body {body:?} to appear in {room_id}");
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}

/// The parts of a delayed event's state — MSC4140's
/// `GET /delayed_events/{delay_id}` — this suite cares about.
struct DelayedEvent {
    /// The timestamp its current countdown started from. Reset to "now" by
    /// every `restart` action, so a later lookup returning a greater value
    /// than an earlier one proves a restart landed in between.
    delayed_since_ts: u64,
}

/// Looks up a delayed event by ID, returning `None` once it is no longer
/// pending: Synapse serves a 404 for a `delay_id` that has already fired or
/// been cancelled.
async fn get_delayed_event(
    cs_api_url: &str,
    access_token: &str,
    delay_id: &str,
) -> Option<DelayedEvent> {
    let mut url = reqwest::Url::parse(cs_api_url).expect("invalid CS API URL");
    url.path_segments_mut()
        .expect("cs_api_url cannot be a base")
        .extend([
            "_matrix",
            "client",
            "unstable",
            "org.matrix.msc4140",
            "delayed_events",
            delay_id,
        ]);

    let resp = reqwest::Client::new()
        .get(url)
        .bearer_auth(access_token)
        .send()
        .await
        .expect("request to look up the delayed event failed");
    if resp.status().as_u16() == 404 {
        return None;
    }
    let status = resp.status();
    let body: serde_json::Value = resp
        .json()
        .await
        .expect("delayed-event lookup response was not valid JSON");
    assert!(
        status.is_success(),
        "looking up the delayed event failed: {status}: {body}"
    );
    Some(DelayedEvent {
        delayed_since_ts: body["delayed_since_ts"]
            .as_u64()
            .expect("delayed-event lookup response is missing `delayed_since_ts`"),
    })
}

/// The service takes over a delegated delayed event: it keeps it alive on the
/// homeserver while the participant is connected to the SFU, and sends it once
/// the participant leaves.
///
/// Both actions are observed directly through MSC4140's
/// `GET /delayed_events/{delay_id}`:
///
///   - `restart` — a lookup right after scheduling captures the event's
///     initial `delayed_since_ts`. A second lookup, made after waiting
///     comfortably past the original delay, must see both a pending event
///     and a *later* `delayed_since_ts` — the only way it could still be
///     pending at that point at all is a restart the homeserver never
///     performs on its own.
///   - `send` — once the participant disconnects, the delayed event's
///     message must land almost immediately, far faster than a homeserver
///     countdown that's just been reset would ever fire it on its own.
#[tokio::test]
async fn delegate_delayed_leave_cs_succeeds() {
    assert_stack_is_up();

    // Register a user and have them create (and thus join) a room.
    let user = register_user(SYNAPSE_A_CS_API_URL, "alice", "e2e-test-password").await;
    let room_id = create_and_join_room(SYNAPSE_A_CS_API_URL, &user).await;

    // Short enough to keep the test fast, but long enough that the service's
    // restarts (every ~80% of the delay) have clearly landed at least once
    // before the "still pending" check below runs, even under CI-level
    // jitter.
    const DELAY_MS: u64 = 5_000;
    const SLOT_ID: &str = "m.call#ROOM";
    const MEMBER_ID: &str = "e2e-member";
    const DEVICE_ID: &str = "E2EDEVICE";
    const MESSAGE_BODY: &str = "e2e-delegate-delayed-leave-proof";

    // Schedule the delayed event the client will hand over.
    let delay_id = schedule_delayed_message(
        SYNAPSE_A_CS_API_URL,
        &user.access_token,
        &room_id,
        "e2e-delayed-leave-txn",
        MESSAGE_BODY,
        DELAY_MS,
    )
    .await;
    let delayed_since_ts = get_delayed_event(SYNAPSE_A_CS_API_URL, &user.access_token, &delay_id)
        .await
        .expect("the delayed event should still be pending right after scheduling")
        .delayed_since_ts;

    // Connect to the SFU as the participant the delegation will name. The
    // token is issued for the same member fields, so it carries the LiveKit
    // identity the service will watch.
    let jwt = get_livekit_token(
        SYNAPSE_A_CS_API_URL,
        &user,
        LIVEKIT_A_URL,
        &room_id,
        SLOT_ID,
        MEMBER_ID,
        DEVICE_ID,
    )
    .await;
    let participant = LiveKitParticipant::connect(LIVEKIT_A_SFU_ADDR, &jwt).await;

    // Delegate the delayed event's lifecycle to the service, through
    // Synapse's C-S API. Synapse proxies the request to its lk-jwt-service
    // running as an application service.
    let resp = reqwest::Client::new()
        .post(format!(
            "{SYNAPSE_A_CS_API_URL}/_matrix/client/unstable/io.element.msc4195/rtc/livekit/delegate_delayed_leave"
        ))
        .bearer_auth(&user.access_token)
        .json(&serde_json::json!({
            "url": LIVEKIT_A_URL,
            "room_id": room_id,
            "slot_id": SLOT_ID,
            "member": {
                "id": MEMBER_ID,
                "claimed_device_id": DEVICE_ID,
            },
            "delay_id": delay_id,
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

    // First action: having found the participant on the SFU, the service
    // starts restarting the delayed event to keep the homeserver's timer
    // from ever reaching zero. Wait comfortably past the original delay —
    // long enough that, left alone, the homeserver would already have fired
    // it — then look it up again.
    tokio::time::sleep(Duration::from_millis(DELAY_MS * 2)).await;
    let restarted_since_ts = get_delayed_event(SYNAPSE_A_CS_API_URL, &user.access_token, &delay_id)
        .await
        .expect(
            "the delegated delayed event was sent or cancelled while the participant was \
             still connected",
        )
        .delayed_since_ts;

    // Only a restart the homeserver never performs on its own explains the
    // event still being pending at this point: its `delayed_since_ts` must
    // have moved forward from the original schedule.
    assert!(
        restarted_since_ts > delayed_since_ts,
        "expected the service to have restarted the delayed event at least once, but \
         delayed_since_ts stayed at {delayed_since_ts}"
    );

    // Second action: the participant hangs up. The SFU reports the departure
    // to the service, which sends the delayed event right away — far sooner
    // than the homeserver's own, repeatedly-reset countdown would ever fire
    // it on its own. The bound here is deliberately tight relative to
    // DELAY_MS: a send that's merely eventual rather than immediate (e.g. the
    // service falling back to waiting out its own countdown) should show up
    // as a timeout, not get masked by a generous one.
    participant.disconnect().await;

    wait_for_message(
        SYNAPSE_A_CS_API_URL,
        &user.access_token,
        &room_id,
        MESSAGE_BODY,
        Duration::from_secs(10),
    )
    .await;
}

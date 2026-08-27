// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use std::time::{Duration, Instant};

use lk_jwt_service_e2e_tests::{
    APPSERVICE_ID, AUTH_SERVICE_URL, AUTH_SERVICE2_URL, LIVEKIT_SFU_ADDR, LIVEKIT_URL,
    LiveKitParticipant, SYNAPSE_CS_API_URL, SYNAPSE_SERVER_NAME, SYNAPSE2_CS_API_URL,
    SYNAPSE2_SERVER_NAME, create_and_join_room, ensure_stack, get_livekit_token, join_room_via,
    register_user, unique_localpart, verify_livekit_token_is_usable,
};

/// Triggers the app-service ping roundtrip against `auth_service_url` and
/// asserts it succeeds.
async fn assert_appservice_ping_succeeds(auth_service_url: &str, server_name: &str) {
    let resp = reqwest::Client::new()
        .post(format!("{auth_service_url}/appservice-ping"))
        .json(&serde_json::json!({
            "server_name": server_name,
            "appservice_id": APPSERVICE_ID,
        }))
        .send()
        .await
        .expect("request to /appservice-ping failed");

    let status = resp.status();
    let body: serde_json::Value = resp.json().await.expect("response was not valid JSON");
    assert!(
        status.is_success(),
        "expected a successful response, got {status}: {body}"
    );
    assert!(
        body.get("duration_ms").and_then(|v| v.as_u64()).is_some(),
        "expected a `duration_ms` field in the homeserver's response, got {body}"
    );
}

/// Triggers the app-service ping roundtrip to ensure each service and its
/// homeserver can reach each other, on both stacks, back to back.
#[tokio::test]
async fn appservice_ping_round_trip_succeeds() {
    ensure_stack().await;

    assert_appservice_ping_succeeds(AUTH_SERVICE_URL, SYNAPSE_SERVER_NAME).await;
    assert_appservice_ping_succeeds(AUTH_SERVICE2_URL, SYNAPSE2_SERVER_NAME).await;
}

/// A joined user succeeds in getting a token for the local SFU.
#[tokio::test]
async fn get_token_local_sfu_succeeds() {
    ensure_stack().await;

    // Register a user and have them create (and thus join) a room.
    let user = register_user(
        SYNAPSE_CS_API_URL,
        &unique_localpart("alice"),
        "e2e-test-password",
    )
    .await;
    let room_id = create_and_join_room(SYNAPSE_CS_API_URL, &user).await;

    // Request a LiveKit token for that room through Synapse's C-S API. Synapse
    // proxies the request to its lk-jwt-service running as an application service.
    let resp = reqwest::Client::new()
        .post(format!(
            "{SYNAPSE_CS_API_URL}/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token"
        ))
        .bearer_auth(&user.access_token)
        .json(&serde_json::json!({
            "room_id": room_id,
            "slot_id": "m.call#ROOM",
            "url": LIVEKIT_URL,
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
    verify_livekit_token_is_usable(LIVEKIT_SFU_ADDR, jwt).await;
}

/// A joined user succeeds in getting a token for a remote SFU.
#[tokio::test]
async fn get_token_remote_sfu_succeeds() {
    ensure_stack().await;

    // Alice creates (and thus joins) a room on her own homeserver (hs1).
    let alice = register_user(
        SYNAPSE_CS_API_URL,
        &unique_localpart("alice"),
        "e2e-test-password",
    )
    .await;
    let room_id = create_and_join_room(SYNAPSE_CS_API_URL, &alice).await;

    // Alice requests a token locally first, as the real MSC4195 flow expects:
    // this is what actually creates the LiveKit room (room.auto_create is
    // disabled), which the relayed request below never does on its own.
    let resp = reqwest::Client::new()
        .post(format!(
            "{SYNAPSE_CS_API_URL}/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token"
        ))
        .bearer_auth(&alice.access_token)
        .json(&serde_json::json!({
            "room_id": room_id,
            "slot_id": "m.call#ROOM",
            "url": LIVEKIT_URL,
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

    // Bob, on a different, federated homeserver (hs2), joins the same room.
    let bob = register_user(
        SYNAPSE2_CS_API_URL,
        &unique_localpart("bob"),
        "e2e-test-password",
    )
    .await;
    join_room_via(SYNAPSE2_CS_API_URL, &bob, &room_id, SYNAPSE_SERVER_NAME).await;

    // Bob requests a token through his own homeserver's C-S API, naming hs1
    // as the MatrixRTC session's SFU-hosting homeserver. hs2 relays this to
    // hs1 via the MSC4512 federation proxy, which in turn calls the S-S
    // endpoint on hs1's app service.
    let resp = reqwest::Client::new()
        .post(format!(
            "{SYNAPSE2_CS_API_URL}/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token"
        ))
        .bearer_auth(&bob.access_token)
        .json(&serde_json::json!({
            "server_name": SYNAPSE_SERVER_NAME,
            "room_id": room_id,
            "slot_id": "m.call#ROOM",
            "url": LIVEKIT_URL,
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

    // The proof that matters: hs1's real LiveKit SFU actually accepts the
    // issued token, even though it was requested through hs2.
    verify_livekit_token_is_usable(LIVEKIT_SFU_ADDR, jwt).await;
}

/// A non-joined user is rejected.
#[tokio::test]
async fn get_token_rejects_non_member() {
    ensure_stack().await;

    // Alice creates (and thus joins) a room; Bob never joins it.
    let alice = register_user(
        SYNAPSE_CS_API_URL,
        &unique_localpart("alice"),
        "e2e-test-password",
    )
    .await;
    let room_id = create_and_join_room(SYNAPSE_CS_API_URL, &alice).await;
    let bob = register_user(
        SYNAPSE_CS_API_URL,
        &unique_localpart("bob"),
        "e2e-test-password",
    )
    .await;

    let resp = reqwest::Client::new()
        .post(format!(
            "{SYNAPSE_CS_API_URL}/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token"
        ))
        .bearer_auth(&bob.access_token)
        .json(&serde_json::json!({
            "room_id": room_id,
            "slot_id": "m.call#ROOM",
            "url": LIVEKIT_URL,
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

/// A joined user is rejected if the remote server isn't joined.
#[tokio::test]
async fn get_token_rejects_when_remote_server_is_not_joined() {
    ensure_stack().await;

    // Bob creates (and thus joins) a room on hs2. hs1 never joins it.
    let bob = register_user(
        SYNAPSE2_CS_API_URL,
        &unique_localpart("bob"),
        "e2e-test-password",
    )
    .await;
    let room_id = create_and_join_room(SYNAPSE2_CS_API_URL, &bob).await;

    // Bob requests a token naming hs1 as the SFU-hosting homeserver, even
    // though hs1 has never seen this room.
    let resp = reqwest::Client::new()
        .post(format!(
            "{SYNAPSE2_CS_API_URL}/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token"
        ))
        .bearer_auth(&bob.access_token)
        .json(&serde_json::json!({
            "server_name": SYNAPSE_SERVER_NAME,
            "room_id": room_id,
            "slot_id": "m.call#ROOM",
            "url": LIVEKIT_URL,
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

/// The service takes over a delegated delayed event: it keeps it alive on the
/// homeserver while the participant is connected to the SFU, and sends it once
/// the participant leaves.
///
/// Synapse doesn't yet implement `GET /delayed_events/{delay_id}`, so the
/// service's `restart` and `send` actions can't be observed directly here.
/// Instead both are inferred from timing, using an undelegated control event
/// — scheduled with the same delay — as the reference for what the homeserver's
/// own countdown does on its own:
///
///   - `restart` — the control, never delegated, fires on the homeserver's
///     own schedule once its delay elapses. By that point the delegated
///     event, kept alive by the service's periodic restarts (which land
///     every ~80% of the delay — see `delay_restart_duration`), must still
///     be pending: only a restart the homeserver never performs on its own
///     explains that.
///   - `send` — once the participant disconnects, the delegated event's
///     message must land almost immediately, far faster than a homeserver
///     countdown that's just been reset would ever fire it on its own.
///
/// This is weaker than directly observing `running_since`: it trusts that
/// the service's restarts land roughly on schedule rather than proving it,
/// so a sufficiently starved CI runner could in principle produce a false
/// failure. Change to reading `running_since` via `GET /delayed_events/{id}`
/// once that endpoint is available here.
#[tokio::test]
async fn delegate_delayed_leave_cs_succeeds() {
    ensure_stack().await;

    // Register a user and have them create (and thus join) a room.
    let user = register_user(
        SYNAPSE_CS_API_URL,
        &unique_localpart("alice"),
        "e2e-test-password",
    )
    .await;
    let room_id = create_and_join_room(SYNAPSE_CS_API_URL, &user).await;

    // Short enough to keep the test fast, but long enough that the service's
    // restarts (every ~80% of the delay) have clearly landed at least twice
    // — comfortably extending the homeserver deadline past DELAY_MS — before
    // the "still pending" check below runs, even under CI-level jitter.
    const DELAY_MS: u64 = 5_000;
    const SLOT_ID: &str = "m.call#ROOM";
    const MEMBER_ID: &str = "e2e-member";
    const DEVICE_ID: &str = "E2EDEVICE";
    const MESSAGE_BODY: &str = "e2e-delegate-delayed-leave-proof";
    const CONTROL_MESSAGE_BODY: &str = "e2e-delegate-delayed-leave-control";

    // Schedule the delayed event the client will hand over, plus an identical
    // one it keeps to itself as a control.
    let delay_id = schedule_delayed_message(
        SYNAPSE_CS_API_URL,
        &user.access_token,
        &room_id,
        "e2e-delayed-leave-txn",
        MESSAGE_BODY,
        DELAY_MS,
    )
    .await;
    schedule_delayed_message(
        SYNAPSE_CS_API_URL,
        &user.access_token,
        &room_id,
        "e2e-delayed-leave-control-txn",
        CONTROL_MESSAGE_BODY,
        DELAY_MS,
    )
    .await;

    // Connect to the SFU as the participant the delegation will name. The
    // token is issued for the same member fields, so it carries the LiveKit
    // identity the service will watch.
    let jwt = get_livekit_token(
        SYNAPSE_CS_API_URL,
        &user,
        LIVEKIT_URL,
        &room_id,
        SLOT_ID,
        MEMBER_ID,
        DEVICE_ID,
    )
    .await;
    let participant = LiveKitParticipant::connect(LIVEKIT_SFU_ADDR, &jwt).await;

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
            "slot_id": SLOT_ID,
            "member": {
                "id": MEMBER_ID,
                "claimed_device_id": DEVICE_ID,
            },
            "delay_id": delay_id,
            // Optional: the service looks the delay up itself when it is
            // absent. Synapse does not implement MSC4140's
            // `GET /delayed_events/{delay_id}` yet, so it is still sent here.
            "delay_timeout": DELAY_MS,
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
    // from ever reaching zero. The control, never delegated, has no such
    // help — waiting for it to fire is the reference point for how long
    // DELAY_MS actually takes on this homeserver.
    wait_for_message(
        SYNAPSE_CS_API_URL,
        &user.access_token,
        &room_id,
        CONTROL_MESSAGE_BODY,
        Duration::from_millis(DELAY_MS * 3),
    )
    .await;

    // Give the delegated event a further DELAY_MS past that point. If the
    // service's restarts were failing or landing late rather than actually
    // preventing the send — i.e. merely delaying it a little instead of
    // indefinitely — this is roughly when a leak like that would show up, so
    // checking right as the control fires would let it slip through.
    tokio::time::sleep(Duration::from_millis(DELAY_MS)).await;

    // The delegated event must still be pending: the same delay has now
    // demonstrably elapsed (the control just proved it) with a further
    // DELAY_MS to spare, but the service has kept restarting it on the
    // homeserver while the participant stays connected, so only a
    // disconnect — not elapsed time — can trigger it.
    assert!(
        !has_message(
            SYNAPSE_CS_API_URL,
            &user.access_token,
            &room_id,
            MESSAGE_BODY
        )
        .await,
        "the delegated delayed event was sent while the participant was still connected"
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
        SYNAPSE_CS_API_URL,
        &user.access_token,
        &room_id,
        MESSAGE_BODY,
        Duration::from_secs(10),
    )
    .await;
}

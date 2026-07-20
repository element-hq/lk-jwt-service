// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use base64::Engine;
use base64::engine::general_purpose::{STANDARD, STANDARD_NO_PAD};
use jsonwebtoken::{Algorithm, DecodingKey, EncodingKey, Header, Validation};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};

use crate::fake_homeserver::FakeHomeserver;
use crate::fake_redis::FakeRedis;
use crate::harness::{LIVEKIT_KEY, LIVEKIT_SECRET, Service};

const REDIS_JOBS_HASH_KEY: &str = "lk-jwt:jobs";

/// Decode a LiveKit JWT issued with the service's API secret and return
/// its claims.
#[track_caller]
pub fn decode_livekit_jwt(jwt: &str) -> Value {
    let key = DecodingKey::from_secret(LIVEKIT_SECRET.as_bytes());
    let validation = Validation::new(Algorithm::HS256);
    let data = jsonwebtoken::decode::<Value>(jwt, &key, &validation)
        .unwrap_or_else(|e| panic!("failed to decode LiveKit JWT: {e} (jwt: {jwt})"));
    data.claims
}

/// Assert status and errcode of a Matrix error response.
#[track_caller]
pub fn expect_matrix_error(status: u16, body: &str, want_status: u16, want_errcode: &str) {
    assert_eq!(
        status, want_status,
        "unexpected status (expected {want_status}, got {status}, body: {body})"
    );
    let error: Value = serde_json::from_str(body)
        .unwrap_or_else(|e| panic!("response body is not JSON: {e} (body: {body})"));
    let errcode = error["errcode"].as_str();
    let errcode_str = errcode.unwrap_or("None");
    assert_eq!(
        errcode,
        Some(want_errcode),
        "unexpected errcode (expected: {want_errcode}, got: {errcode_str}, body: {body})"
    );
}

/// Assert that no /openid/userinfo lookups were made.
#[track_caller]
pub fn expect_no_user_info_lookups(hs: &FakeHomeserver) {
    assert!(
        hs.user_info_requests().is_empty(),
        "expected no user info lookups, got {:?}",
        hs.user_info_requests()
    );
}

/// Assert that exactly one /openid/userinfo lookup was made, carrying the
/// given access token.
#[track_caller]
pub fn expect_user_info_lookup(hs: &FakeHomeserver, access_token: &str) {
    let requests = hs.user_info_requests();
    assert_eq!(
        requests.len(),
        1,
        "expected exactly one user info lookup, got {:?}",
        requests
    );
    assert_eq!(requests[0].access_token, access_token);
}

/// Assert that no /delayed_events request has been recorded.
#[track_caller]
pub fn expect_no_delayed_event_requests(hs: &FakeHomeserver) {
    let requests = hs.delayed_event_requests();
    assert!(
        requests.is_empty(),
        "expected no delayed event actions, got {:?}",
        requests
    );
}

/// Assert that no /delayed_events request with the given delay ID and
/// action has been recorded.
#[track_caller]
pub fn expect_no_delayed_event_request(hs: &FakeHomeserver, delay_id: &str, action: &str) {
    let requests = hs.delayed_event_requests();
    assert!(
        !requests
            .iter()
            .any(|r| r.delay_id == delay_id && r.action == action),
        "expected no {action:?} request with delay_id {delay_id:?}, got {requests:?}"
    );
}

/// Assert that a /delayed_events request with the given delay ID and action
/// has been recorded.
#[track_caller]
pub fn expect_delayed_event_request(hs: &FakeHomeserver, delay_id: &str, action: &str) {
    let requests = hs.delayed_event_requests();
    assert!(
        requests
            .iter()
            .any(|r| r.delay_id == delay_id && r.action == action),
        "expected a {action:?} request with delay_id {delay_id:?}, got {requests:?}"
    );
}

/// Poll until a /delayed_events request with the given delay ID and action
/// has been recorded, or panic once the timeout elapses.
pub async fn wait_for_delayed_event_request(
    hs: &FakeHomeserver,
    delay_id: &str,
    action: &str,
    timeout: Duration,
) {
    let deadline = Instant::now() + timeout;
    loop {
        let requests = hs.delayed_event_requests();
        if requests
            .iter()
            .any(|r| r.delay_id == delay_id && r.action == action)
        {
            return;
        }
        if Instant::now() >= deadline {
            panic!(
                "timed out waiting for a {action:?} request with delay_id {delay_id:?}, got {requests:?}"
            );
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
}

/// Assert that exactly `count` /delayed_events requests with the given
/// delay ID and action have been recorded.
#[track_caller]
pub fn expect_delayed_event_request_count(
    hs: &FakeHomeserver,
    delay_id: &str,
    action: &str,
    count: usize,
) {
    let requests = hs.delayed_event_requests();
    let matching = requests
        .iter()
        .filter(|r| r.delay_id == delay_id && r.action == action)
        .count();
    assert_eq!(
        matching, count,
        "expected {count} {action:?} request(s) with delay_id {delay_id:?}, got {matching} (all requests: {requests:?})"
    );
}

/// Poll until at least `count` /delayed_events requests with the given
/// delay ID and action have been recorded, or panic once the timeout
/// elapses.
pub async fn wait_for_delayed_event_request_count(
    hs: &FakeHomeserver,
    delay_id: &str,
    action: &str,
    count: usize,
    timeout: Duration,
) {
    let deadline = Instant::now() + timeout;
    loop {
        let requests = hs.delayed_event_requests();
        let matching = requests
            .iter()
            .filter(|r| r.delay_id == delay_id && r.action == action)
            .count();
        if matching >= count {
            return;
        }
        if Instant::now() >= deadline {
            panic!(
                "timed out waiting for {count} {action:?} request(s) with delay_id {delay_id:?}, got {matching} (all requests: {requests:?})"
            );
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
}

pub fn livekit_room_alias(matrix_room: &str, slot_id: &str) -> String {
    let marshalled =
        serde_json::to_vec(&[matrix_room, slot_id]).expect("string arrays always serialize");
    STANDARD_NO_PAD.encode(Sha256::digest(marshalled))
}

pub fn livekit_identity(matrix_id: &str, device_id: &str, member_id: &str) -> String {
    let marshalled = serde_json::to_vec(&[matrix_id, device_id, member_id])
        .expect("string arrays always serialize");
    STANDARD_NO_PAD.encode(Sha256::digest(marshalled))
}

fn redis_job_field(room: &str, identity: &str) -> String {
    serde_json::to_string(&[room, identity]).expect("string arrays always serialize")
}

#[track_caller]
pub fn expect_job_persisted(redis: &FakeRedis, room: &str, identity: &str) {
    assert!(
        redis.hash_field_exists(REDIS_JOBS_HASH_KEY, &redis_job_field(room, identity)),
        "expected a persisted job for room {room:?}, identity {identity:?}"
    );
}

#[track_caller]
pub fn expect_job_not_persisted(redis: &FakeRedis, room: &str, identity: &str) {
    assert!(
        !redis.hash_field_exists(REDIS_JOBS_HASH_KEY, &redis_job_field(room, identity)),
        "expected no persisted job for room {room:?}, identity {identity:?}"
    );
}

pub async fn wait_for_job_removed(
    redis: &FakeRedis,
    room: &str,
    identity: &str,
    timeout: Duration,
) {
    let field = redis_job_field(room, identity);
    let deadline = Instant::now() + timeout;
    loop {
        if !redis.hash_field_exists(REDIS_JOBS_HASH_KEY, &field) {
            return;
        }
        if Instant::now() >= deadline {
            panic!(
                "timed out waiting for the job (room {room:?}, identity {identity:?}) to be removed from the store"
            );
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
}

/// Send a signed LiveKit SFU webhook to the service's /sfu_webhook endpoint.
pub async fn send_sfu_webhook(
    svc: &Service,
    event: &str,
    room: &str,
    identity: &str,
    disconnect_reason: Option<&str>,
) {
    let mut participant = json!({ "identity": identity });
    if let Some(reason) = disconnect_reason {
        participant["disconnectReason"] = json!(reason);
    }
    let body = json!({
        "event": event,
        "room": {"name": room},
        "participant": participant,
        "id": "evt_integration_test",
        "createdAt": 0,
    })
    .to_string();

    let sha256_claim = STANDARD.encode(Sha256::digest(body.as_bytes()));
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs();
    let claims = json!({
        "iss": LIVEKIT_KEY,
        "iat": now,
        "nbf": now,
        "exp": now + 60,
        "sha256": sha256_claim,
    });
    let token = jsonwebtoken::encode(
        &Header::new(Algorithm::HS256),
        &claims,
        &EncodingKey::from_secret(LIVEKIT_SECRET.as_bytes()),
    )
    .expect("failed to sign webhook token");

    let resp = reqwest::Client::new()
        .post(format!("{}/sfu_webhook", svc.base_url))
        .header("Authorization", &token)
        .header("Content-Type", "application/json")
        .body(body)
        .send()
        .await
        .expect("webhook request failed");
    assert_eq!(resp.status().as_u16(), 200, "webhook POST was not accepted");
}

// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use std::collections::HashMap;
use std::time::Duration;

use lk_jwt_service_integration_tests::{
    DEFAULT_LK_URL, FakeHomeserver, FakeRedis, Service, ServiceConfig,
    expect_delayed_event_request_identity, expect_job_persisted, expect_matrix_error,
    expect_no_delayed_event_requests, livekit_identity, livekit_room_alias, send_sfu_webhook,
    wait_for_delayed_event_request, wait_for_job_removed,
};
use serde_json::{Value, json};

const AS_TOKEN: &str = "as_token";
const HS_TOKEN: &str = "hs_token";

const DELEGATE_DELAYED_LEAVE_CS_PATH: &str =
    "/_matrix/client/unstable/io.element.msc4195/rtc/livekit/delegate_delayed_leave";

/// App-service configuration as extra_env.
fn app_service_env_with_hs_server_name(hs_server_name: &str) -> HashMap<String, String> {
    HashMap::from([
        ("LIVEKIT_AS_TOKEN".to_owned(), AS_TOKEN.to_owned()),
        ("LIVEKIT_HS_TOKEN".to_owned(), HS_TOKEN.to_owned()),
        (
            "LIVEKIT_HS_SERVER_NAME".to_owned(),
            hs_server_name.to_owned(),
        ),
    ])
}

/// Return a valid delegate_delayed_leave C-S request body.
fn delegate_request(lk_url: &str) -> Value {
    json!({
        "url": lk_url,
        "room_id": "!room:example.com",
        "slot_id": "m.call#",
        "member": {
            "id": "member-1",
            "claimed_device_id": "DEVICE",
        },
        "delay_id": "syd_cs_integration_1",
        "delay_timeout": 8000,
    })
}

/// POST the given body to the delegate_delayed_leave C-S endpoint and return
/// the status code and raw response body.
async fn post_delegate_cs(
    svc: &Service,
    body: impl Into<reqwest::Body>,
    header_mxid: Option<&str>,
) -> (u16, String) {
    post_delegate_cs_as(svc, body, header_mxid, Some(HS_TOKEN)).await
}

/// POST the given body to the delegate_delayed_leave C-S endpoint, with
/// explicit control over the homeserver access token, and return the status
/// code and raw response body.
async fn post_delegate_cs_as(
    svc: &Service,
    body: impl Into<reqwest::Body>,
    header_mxid: Option<&str>,
    hs_token: Option<&str>,
) -> (u16, String) {
    let mut req = reqwest::Client::new()
        .post(format!("{}{DELEGATE_DELAYED_LEAVE_CS_PATH}", svc.base_url))
        .header("Content-Type", "application/json");
    if let Some(mxid) = header_mxid {
        req = req.header("X-Matrix-User-Identifier", mxid);
    }
    if let Some(token) = hs_token {
        req = req.header("Authorization", format!("Bearer {token}"));
    }
    let resp = req.body(body).send().await.expect("request failed");
    let status = resp.status().as_u16();
    let body = resp.text().await.expect("failed to read response body");
    (status, body)
}

/// A missing homeserver token is rejected.
#[tokio::test]
async fn missing_hs_token() {
    let hs = FakeHomeserver::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let (status, body) = post_delegate_cs_as(
        &svc,
        delegate_request(DEFAULT_LK_URL).to_string(),
        Some("@alice:example.com"),
        None,
    )
    .await;

    expect_matrix_error(status, &body, 401, "M_MISSING_TOKEN");
    expect_no_delayed_event_requests(&hs);
}

/// A wrong homeserver token is rejected.
#[tokio::test]
async fn wrong_hs_token() {
    let hs = FakeHomeserver::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let (status, body) = post_delegate_cs_as(
        &svc,
        delegate_request(DEFAULT_LK_URL).to_string(),
        Some("@alice:example.com"),
        Some("not_the_hs_token"),
    )
    .await;

    expect_matrix_error(status, &body, 401, "M_UNKNOWN_TOKEN");
    expect_no_delayed_event_requests(&hs);
}

/// A missing X-Matrix-User-Identifier header is rejected.
#[tokio::test]
async fn missing_header() {
    let hs = FakeHomeserver::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let (status, body) =
        post_delegate_cs(&svc, delegate_request(DEFAULT_LK_URL).to_string(), None).await;

    expect_matrix_error(status, &body, 401, "M_UNAUTHORIZED");
    expect_no_delayed_event_requests(&hs);
}

/// A url not matching the service's configured LIVEKIT_URL is rejected.
#[tokio::test]
async fn url_mismatch() {
    let hs = FakeHomeserver::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let (status, body) = post_delegate_cs(
        &svc,
        delegate_request("wss://not-the-configured-sfu.example.com").to_string(),
        Some("@alice:example.com"),
    )
    .await;

    expect_matrix_error(status, &body, 400, "M_INVALID_PARAM");
    expect_no_delayed_event_requests(&hs);
}

/// A missing url is rejected.
#[tokio::test]
async fn missing_url() {
    let hs = FakeHomeserver::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let mut request = delegate_request(DEFAULT_LK_URL);
    request["url"] = json!("");
    let (status, body) =
        post_delegate_cs(&svc, request.to_string(), Some("@alice:example.com")).await;

    expect_matrix_error(status, &body, 400, "M_BAD_JSON");
    expect_no_delayed_event_requests(&hs);
}

/// A request missing mandatory fields is rejected.
#[tokio::test]
async fn missing_fields() {
    let hs = FakeHomeserver::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let mut request = delegate_request(DEFAULT_LK_URL);
    request.as_object_mut().unwrap().remove("delay_id");
    let (status, body) =
        post_delegate_cs(&svc, request.to_string(), Some("@alice:example.com")).await;

    expect_matrix_error(status, &body, 400, "M_BAD_JSON");
    expect_no_delayed_event_requests(&hs);
}

/// Malformed JSON is rejected.
#[tokio::test]
async fn malformed_json() {
    let hs = FakeHomeserver::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let (status, body) = post_delegate_cs(&svc, "{not json", Some("@alice:example.com")).await;

    expect_matrix_error(status, &body, 400, "M_NOT_JSON");
    expect_no_delayed_event_requests(&hs);
}

/// GET requests are rejected.
#[tokio::test]
async fn get_instead_of_post() {
    let hs = FakeHomeserver::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let url = format!("{}{DELEGATE_DELAYED_LEAVE_CS_PATH}", svc.base_url);
    let resp = reqwest::Client::new()
        .get(&url)
        .send()
        .await
        .expect("GET failed");

    assert_eq!(resp.status().as_u16(), 405);
    expect_no_delayed_event_requests(&hs);
}

/// A C-S API resolution failure triggers rejection.
#[tokio::test]
async fn unresolvable_cs_api() {
    let hs = FakeHomeserver::new().await;

    // No CS API override, so it should fall back to .well-known discovery
    // against the fake homeserver, which doesn't serve it.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let (status, body) = post_delegate_cs(
        &svc,
        delegate_request(DEFAULT_LK_URL).to_string(),
        Some("@alice:example.com"),
    )
    .await;

    expect_matrix_error(status, &body, 400, "M_BAD_JSON");
    expect_no_delayed_event_requests(&hs);
}

/// A valid request produces a 200 response with an empty JSON object body.
#[tokio::test]
async fn success() {
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let (status, body) = post_delegate_cs(
        &svc,
        delegate_request(DEFAULT_LK_URL).to_string(),
        Some(&user.user_id),
    )
    .await;

    assert_eq!(status, 200, "body: {body}");
    let response: Value = serde_json::from_str(&body).expect("response is not JSON");
    assert_eq!(
        response.as_object().map(|o| o.len()),
        Some(0),
        "expected empty response object, got {body}"
    );
}

/// End-to-end: the scheduled job restarts and sends the delayed event
/// against the configured local homeserver, authenticating both calls via
/// application-service identity assertion — the configured as_token in the
/// Authorization header, and the caller's MXID as `user_id`.
#[tokio::test]
async fn restart_and_send_use_identity_assertion() {
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    let redis = FakeRedis::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        redis_url: Some(redis.url().to_owned()),
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let (status, body) = post_delegate_cs(
        &svc,
        delegate_request(DEFAULT_LK_URL).to_string(),
        Some(&user.user_id),
    )
    .await;
    assert_eq!(status, 200, "body: {body}");

    // The job should be persisted.
    let room = livekit_room_alias("!room:example.com", "m.call#");
    let identity = livekit_identity(&user.user_id, "DEVICE", "member-1");
    expect_job_persisted(&redis, &room, &identity);

    // Report that the participant connected.
    send_sfu_webhook(&svc, "participant_joined", &room, &identity, None).await;

    // The service should restart the delayed event, authenticated as the
    // caller via application-service identity assertion.
    tokio::time::sleep(Duration::from_millis(200)).await;
    expect_delayed_event_request_identity(
        &hs,
        "syd_cs_integration_1",
        "restart",
        AS_TOKEN,
        &user.user_id,
    );

    // Report that the participant disconnected intentionally.
    send_sfu_webhook(
        &svc,
        "participant_left",
        &room,
        &identity,
        Some("CLIENT_INITIATED"),
    )
    .await;

    // The service should trigger the send action, also authenticated.
    wait_for_job_removed(&redis, &room, &identity, Duration::from_secs(2)).await;
    expect_delayed_event_request_identity(
        &hs,
        "syd_cs_integration_1",
        "send",
        AS_TOKEN,
        &user.user_id,
    );
}

// ── delay look-up (MSC4140) ───────────────────────────────────────────────────

/// A request may omit the delay timeout. The service then reads the delay off
/// the delayed event itself, asserting the caller's identity as it does so.
#[tokio::test]
async fn delay_timeout_looked_up_when_absent() {
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");
    hs.set_delay("syd_cs_integration_1", 8000, "!room:example.com");

    let redis = FakeRedis::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        redis_url: Some(redis.url().to_owned()),
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let mut request = delegate_request(DEFAULT_LK_URL);
    request.as_object_mut().unwrap().remove("delay_timeout");
    let (status, body) = post_delegate_cs(&svc, request.to_string(), Some(&user.user_id)).await;
    assert_eq!(status, 200, "body: {body}");

    let lookups = hs.delay_lookups();
    assert_eq!(
        lookups.len(),
        1,
        "expected exactly one lookup, got {lookups:?}"
    );
    assert_eq!(lookups[0].delay_id, "syd_cs_integration_1");
    assert_eq!(
        lookups[0].authorization,
        format!("Bearer {AS_TOKEN}"),
        "expected the lookup to authenticate with the as_token"
    );
    assert_eq!(
        lookups[0].user_id, user.user_id,
        "expected the lookup to assert the caller's identity"
    );

    // The job is scheduled off the looked-up delay.
    let room = livekit_room_alias("!room:example.com", "m.call#");
    let identity = livekit_identity(&user.user_id, "DEVICE", "member-1");
    expect_job_persisted(&redis, &room, &identity);
}

/// A request that carries a delay timeout is taken at its word — the service
/// does not look the delay up.
#[tokio::test]
async fn delay_timeout_not_looked_up_when_given() {
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let (status, body) = post_delegate_cs(
        &svc,
        delegate_request(DEFAULT_LK_URL).to_string(),
        Some(&user.user_id),
    )
    .await;
    assert_eq!(status, 200, "body: {body}");

    let lookups = hs.delay_lookups();
    assert!(lookups.is_empty(), "expected no lookups, got {lookups:?}");
}

/// The looked-up delay becomes the job's timeout: with a short delay and no
/// participant ever showing up on the SFU, the waiting-state timeout fires and
/// the leave event is sent.
#[tokio::test]
async fn looked_up_delay_drives_the_job() {
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");
    hs.set_delay("syd_cs_integration_1", 300, "!room:example.com");

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let mut request = delegate_request(DEFAULT_LK_URL);
    request.as_object_mut().unwrap().remove("delay_timeout");
    let (status, body) = post_delegate_cs(&svc, request.to_string(), Some(&user.user_id)).await;
    assert_eq!(status, 200, "body: {body}");

    wait_for_delayed_event_request(&hs, "syd_cs_integration_1", "send", Duration::from_secs(5))
        .await;
}

/// An unknown delay ID is rejected with 400 M_INVALID_PARAM per MSC4195, and
/// no job is scheduled for it.
#[tokio::test]
async fn unknown_delay_id_rejected() {
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");
    // No delay scripted for the request's delay ID.

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let mut request = delegate_request(DEFAULT_LK_URL);
    request.as_object_mut().unwrap().remove("delay_timeout");
    let (status, body) = post_delegate_cs(&svc, request.to_string(), Some(&user.user_id)).await;

    expect_matrix_error(status, &body, 400, "M_INVALID_PARAM");
    expect_no_delayed_event_requests(&hs);
}

/// A delay ID that exists but was scheduled for a different room than the
/// one being delegated for is rejected.
#[tokio::test]
async fn delay_id_for_another_room_rejected() {
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");
    hs.set_delay("syd_cs_integration_1", 300, "!other-room:example.com");

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let mut request = delegate_request(DEFAULT_LK_URL);
    request.as_object_mut().unwrap().remove("delay_timeout");
    let (status, body) = post_delegate_cs(&svc, request.to_string(), Some(&user.user_id)).await;

    expect_matrix_error(status, &body, 400, "M_INVALID_PARAM");
    expect_no_delayed_event_requests(&hs);
}

/// A homeserver that cannot answer the look-up makes the request fail with
/// 503 M_UNKNOWN, so the client knows it may retry.
#[tokio::test]
async fn delay_lookup_failure_rejected() {
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");
    hs.set_delay("syd_cs_integration_1", 8000, "!room:example.com");
    hs.set_delay_lookup_status(500);

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let mut request = delegate_request(DEFAULT_LK_URL);
    request.as_object_mut().unwrap().remove("delay_timeout");
    let (status, body) = post_delegate_cs(&svc, request.to_string(), Some(&user.user_id)).await;

    expect_matrix_error(status, &body, 503, "M_UNKNOWN");
    expect_no_delayed_event_requests(&hs);
}

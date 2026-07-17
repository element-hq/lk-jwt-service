// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use std::collections::HashMap;
use std::time::Duration;

use lk_jwt_service_integration_tests::{
    FakeHomeserver, FakeRedis, FakeSfu, FakeUser, Service, ServiceConfig,
    expect_delayed_event_request, expect_delayed_event_request_count, expect_job_not_persisted,
    expect_job_persisted, expect_matrix_error, expect_no_delayed_event_request,
    expect_no_delayed_event_requests, expect_no_user_info_lookups, expect_user_info_lookup,
    livekit_identity, livekit_room_alias, send_sfu_webhook, wait_for_delayed_event_request,
    wait_for_delayed_event_request_count, wait_for_job_removed,
};
use serde_json::{Value, json};

/// Return a valid /delegate_delayed_leave body for the given user.
fn delegate_request(hs: &FakeHomeserver, user: &FakeUser) -> Value {
    json!({
        "room_id": "!room:example.com",
        "slot_id": "m.call#",
        "openid_token": {
            "access_token": user.openid_token,
            "token_type": "Bearer",
            "matrix_server_name": hs.server_name(),
            "expires_in": 3600,
        },
        "member": {
            "id": "member-1",
            "claimed_user_id": user.user_id,
            "claimed_device_id": "DEVICE",
        },
        "delay_id": "syd_integration_1",
        "delay_timeout": 8000,
    })
}

/// POST the given body to /delegate_delayed_leave and return the status
/// code and raw response body.
async fn post_delegate(svc: &Service, body: impl Into<reqwest::Body>) -> (u16, String) {
    let resp = reqwest::Client::new()
        .post(format!("{}/delegate_delayed_leave", svc.base_url))
        .header("Content-Type", "application/json")
        .body(body)
        .send()
        .await
        .expect("request failed");
    let status = resp.status().as_u16();
    let body = resp.text().await.expect("failed to read response body");
    (status, body)
}

#[tokio::test]
async fn success() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Start the service.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        ..Default::default()
    })
    .await;

    // Post a valid /delegate_delayed_leave request.
    let (status, body) = post_delegate(&svc, delegate_request(&hs, &user).to_string()).await;

    // The request should succeed.
    assert_eq!(status, 200, "body: {body}");
    let response: Value = serde_json::from_str(&body).expect("response is not JSON");
    assert_eq!(
        response.as_object().map(|o| o.len()),
        Some(0),
        "expected empty response object, got {body}"
    );

    // The service should have called /openid/userinfo.
    expect_user_info_lookup(&hs, &user.openid_token);
}

#[tokio::test]
async fn unknown_token() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Start the service.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        ..Default::default()
    })
    .await;

    // Post a /delegate_delayed_leave request with an invalid OpenID token.
    let mut request = delegate_request(&hs, &user);
    request["openid_token"]["access_token"] = json!("syt_forged_token");
    let (status, body) = post_delegate(&svc, request.to_string()).await;

    // The request should fail.
    expect_matrix_error(status, &body, 401, "M_UNAUTHORIZED");

    // The service should have called /openid/userinfo.
    expect_user_info_lookup(&hs, "syt_forged_token");

    // The service should not have called /delayed_events.
    expect_no_delayed_event_requests(&hs);
}

#[tokio::test]
async fn claimed_user_mismatch() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Start the service.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        ..Default::default()
    })
    .await;

    // Post a /delegate_delayed_leave request with a different MXID.
    let mut request = delegate_request(&hs, &user);
    request["member"]["claimed_user_id"] = json!(format!("@bob:{}", hs.server_name()));

    // The request should fail.
    let (status, body) = post_delegate(&svc, request.to_string()).await;
    expect_matrix_error(status, &body, 401, "M_UNAUTHORIZED");

    // The service should have called /openid/userinfo.
    expect_user_info_lookup(&hs, &user.openid_token);

    // The service should not have called /delayed_events.
    expect_no_delayed_event_requests(&hs);
}

#[tokio::test]
async fn restricted_homeserver() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Start the service. The fake homeserver is NOT in the full-access list.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["trusted.example.com".to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        ..Default::default()
    })
    .await;

    // Post a /delegate_delayed_leave request.
    let (status, body) = post_delegate(&svc, delegate_request(&hs, &user).to_string()).await;

    // The request should fail.
    expect_matrix_error(status, &body, 403, "M_FORBIDDEN");

    // The service should have called /openid/userinfo.
    expect_user_info_lookup(&hs, &user.openid_token);

    // The service should not have called /delayed_events.
    expect_no_delayed_event_requests(&hs);
}

#[tokio::test]
async fn unresolvable_cs_api() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Start the service. No CS API override, so it should fall back to
    // .well-known discovery against the fake homeserver, which doesn't
    // serve it.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        ..Default::default()
    })
    .await;

    // Post a /delegate_delayed_leave request.
    let (status, body) = post_delegate(&svc, delegate_request(&hs, &user).to_string()).await;

    // The request should fail.
    expect_matrix_error(status, &body, 400, "M_BAD_JSON");

    // The service should have called /openid/userinfo.
    expect_user_info_lookup(&hs, &user.openid_token);

    // The service should not have called /delayed_events.
    expect_no_delayed_event_requests(&hs);
}

#[tokio::test]
async fn missing_delay_params() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Start the service.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        ..Default::default()
    })
    .await;

    // Post a /delegate_delayed_leave request without delay_id.
    let mut request = delegate_request(&hs, &user);
    request.as_object_mut().unwrap().remove("delay_id");
    let (status, body) = post_delegate(&svc, request.to_string()).await;

    // The request should fail.
    expect_matrix_error(status, &body, 400, "M_BAD_JSON");

    // Post a /delegate_delayed_leave request without delay_timeout.
    let mut request = delegate_request(&hs, &user);
    request.as_object_mut().unwrap().remove("delay_timeout");
    let (status, body) = post_delegate(&svc, request.to_string()).await;

    // The request should fail.
    expect_matrix_error(status, &body, 400, "M_BAD_JSON");

    // The service should not have called /openid/userinfo.
    expect_no_user_info_lookups(&hs);

    // The service should not have called /delayed_events.
    expect_no_delayed_event_requests(&hs);
}

#[tokio::test]
async fn malformed_json() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;

    // Start the service.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        ..Default::default()
    })
    .await;

    // Post a /delegate_delayed_leave request with malformed JSON.
    let (status, body) = post_delegate(&svc, "{not json").await;

    // The request should fail.
    expect_matrix_error(status, &body, 400, "M_NOT_JSON");

    // The service should not have called /openid/userinfo.
    expect_no_user_info_lookups(&hs);

    // The service should not have called /delayed_events.
    expect_no_delayed_event_requests(&hs);
}

#[tokio::test]
async fn get_instead_of_post() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;

    // Start the service.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        ..Default::default()
    })
    .await;

    // Send a /delegate_delayed_leave request using GET instead of POST.
    let url = format!("{}/delegate_delayed_leave", svc.base_url);
    let client = reqwest::Client::new();
    let resp = client.get(&url).send().await.expect("GET failed");

    // The request should fail.
    assert_eq!(resp.status().as_u16(), 405);

    // The service should not have called /openid/userinfo.
    expect_no_user_info_lookups(&hs);

    // The service should not have called /delayed_events.
    expect_no_delayed_event_requests(&hs);
}

#[tokio::test]
async fn aborts_and_sends_when_connection_is_never_established() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Set up Redis.
    let redis = FakeRedis::new().await;

    // Start the service.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        redis_url: Some(redis.url().to_owned()),

        ..Default::default()
    })
    .await;

    // Post a valid /delegate_delayed_leave request with a short timeout.
    let delay_timeout_ms = 1000;
    let mut request = delegate_request(&hs, &user);
    request["delay_timeout"] = json!(delay_timeout_ms);
    let (status, body) = post_delegate(&svc, request.to_string()).await;

    // The request should succeed.
    assert_eq!(status, 200, "body: {body}");

    // The service should have called /openid/userinfo.
    expect_user_info_lookup(&hs, &user.openid_token);

    // The job should be persisted.
    let room = livekit_room_alias("!room:example.com", "m.call#");
    let identity = livekit_identity(&user.user_id, "DEVICE", "member-1");
    expect_job_persisted(&redis, &room, &identity);

    // No SFU webhook ever arrives.

    // The service should trigger the send action and abort the job.
    wait_for_job_removed(&redis, &room, &identity, Duration::from_secs(2)).await;
    expect_delayed_event_request(&hs, "syd_integration_1", "send");
}

#[tokio::test]
async fn connects_via_participant_lookup_when_already_present() {
    // Set up the homeserver and SFU.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");
    let sfu = FakeSfu::new().await;

    // Set up Redis.
    let redis = FakeRedis::new().await;

    // Start the service.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        livekit_url: Some(sfu.url().to_owned()),
        redis_url: Some(redis.url().to_owned()),
        ..Default::default()
    })
    .await;

    // The participant is already on the SFU before the request even arrives.
    let room = livekit_room_alias("!room:example.com", "m.call#");
    let identity = livekit_identity(&user.user_id, "DEVICE", "member-1");
    sfu.set_participant_present(&room, &identity);

    // Post a valid /delegate_delayed_leave request.
    let (status, body) = post_delegate(&svc, delegate_request(&hs, &user).to_string()).await;

    // The request should succeed.
    assert_eq!(status, 200, "body: {body}");

    // The job should be persisted.
    expect_job_persisted(&redis, &room, &identity);

    // No SFU webhook ever arrives so the job should connect via participant-lookup polling alone.

    // The service should restart the delayed event once the lookup succeeds.
    wait_for_delayed_event_request(&hs, "syd_integration_1", "restart", Duration::from_secs(5))
        .await;

    // Report that the participant disconnected intentionally.
    send_sfu_webhook(
        &svc,
        "participant_left",
        &room,
        &identity,
        Some("CLIENT_INITIATED"),
    )
    .await;

    // The service should trigger the send action and abort the job.
    wait_for_job_removed(&redis, &room, &identity, Duration::from_secs(2)).await;
    expect_delayed_event_request(&hs, "syd_integration_1", "send");
}

#[tokio::test]
async fn aborts_when_connection_is_aborted_before_being_established() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Set up Redis.
    let redis = FakeRedis::new().await;

    // Start the service.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        redis_url: Some(redis.url().to_owned()),
        ..Default::default()
    })
    .await;

    // Post a valid /delegate_delayed_leave request.
    let (status, body) = post_delegate(&svc, delegate_request(&hs, &user).to_string()).await;

    // The request should succeed.
    assert_eq!(status, 200, "body: {body}");

    // The service should have called /openid/userinfo.
    expect_user_info_lookup(&hs, &user.openid_token);

    // The job should be persisted.
    let room = livekit_room_alias("!room:example.com", "m.call#");
    let identity = livekit_identity(&user.user_id, "DEVICE", "member-1");
    expect_job_persisted(&redis, &room, &identity);

    // Report that the participant aborted their connection.
    send_sfu_webhook(
        &svc,
        "participant_connection_aborted",
        &room,
        &identity,
        Some("JOIN_FAILURE"),
    )
    .await;

    // The service should not trigger the send action and abort the job.
    wait_for_job_removed(&redis, &room, &identity, Duration::from_secs(2)).await;
    expect_no_delayed_event_requests(&hs);
}

#[tokio::test]
async fn aborts_and_sends_when_connection_is_aborted_after_being_established() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Set up Redis.
    let redis = FakeRedis::new().await;

    // Start the service.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        redis_url: Some(redis.url().to_owned()),
        ..Default::default()
    })
    .await;

    // Post a valid /delegate_delayed_leave request.
    let (status, body) = post_delegate(&svc, delegate_request(&hs, &user).to_string()).await;

    // The request should succeed.
    assert_eq!(status, 200, "body: {body}");

    // The job should be persisted.
    let room = livekit_room_alias("!room:example.com", "m.call#");
    let identity = livekit_identity(&user.user_id, "DEVICE", "member-1");
    expect_job_persisted(&redis, &room, &identity);

    // Report that the participant connected.
    send_sfu_webhook(&svc, "participant_joined", &room, &identity, None).await;

    // The service should restart the delayed event.
    wait_for_delayed_event_request(&hs, "syd_integration_1", "restart", Duration::from_secs(2))
        .await;

    // Report that the participant's connection was aborted
    send_sfu_webhook(
        &svc,
        "participant_connection_aborted",
        &room,
        &identity,
        Some("SIGNAL_CLOSE"),
    )
    .await;

    // The service should trigger the send action and abort the job.
    wait_for_job_removed(&redis, &room, &identity, Duration::from_secs(2)).await;
    expect_delayed_event_request(&hs, "syd_integration_1", "send");
}

#[tokio::test]
async fn aborts_and_sends_when_connection_is_disconnected() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Set up Redis.
    let redis = FakeRedis::new().await;

    // Start the service.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        redis_url: Some(redis.url().to_owned()),
        ..Default::default()
    })
    .await;

    // Post a valid /delegate_delayed_leave request.
    let (status, body) = post_delegate(&svc, delegate_request(&hs, &user).to_string()).await;

    // The request should succeed.
    assert_eq!(status, 200, "body: {body}");

    // The job should be persisted.
    let room = livekit_room_alias("!room:example.com", "m.call#");
    let identity = livekit_identity(&user.user_id, "DEVICE", "member-1");
    expect_job_persisted(&redis, &room, &identity);

    // Report that the participant connected.
    send_sfu_webhook(&svc, "participant_joined", &room, &identity, None).await;

    // The service should restart the delayed event.
    wait_for_delayed_event_request(&hs, "syd_integration_1", "restart", Duration::from_secs(2))
        .await;

    // Report that the participant has having left.
    send_sfu_webhook(
        &svc,
        "participant_left",
        &room,
        &identity,
        Some("CLIENT_INITIATED"),
    )
    .await;

    // The service should trigger the send action and abort the job.
    wait_for_job_removed(&redis, &room, &identity, Duration::from_secs(2)).await;
    expect_delayed_event_request(&hs, "syd_integration_1", "send");
}

#[tokio::test]
async fn aborts_and_sends_when_participant_goes_missing_undetected() {
    // Set up the homeserver and SFU.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");
    let sfu = FakeSfu::new().await;

    // Set up Redis.
    let redis = FakeRedis::new().await;

    // Start the service with a short sanity check interval.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        livekit_url: Some(sfu.url().to_owned()),
        redis_url: Some(redis.url().to_owned()),
        extra_env: HashMap::from([(
            "LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS".to_owned(),
            "1".to_owned(),
        )]),
    })
    .await;

    // The participant is already on the SFU before the request arrives.
    let room = livekit_room_alias("!room:example.com", "m.call#");
    let identity = livekit_identity(&user.user_id, "DEVICE", "member-1");
    sfu.set_participant_present(&room, &identity);

    // Post a valid /delegate_delayed_leave request.
    let (status, body) = post_delegate(&svc, delegate_request(&hs, &user).to_string()).await;

    // The request should succeed.
    assert_eq!(status, 200, "body: {body}");

    // The job should be persisted.
    expect_job_persisted(&redis, &room, &identity);

    // No SFU webhook ever arrives so the job should connect via participant-lookup polling alone.

    // The service should restart the delayed event once the lookup succeeds.
    wait_for_delayed_event_request(&hs, "syd_integration_1", "restart", Duration::from_secs(5))
        .await;

    // Simulate a missed disconnect webhook: the participant leaves the SFU
    // but no participant_left event is ever sent.
    sfu.set_participant_absent(&room, &identity);

    // The service should detect the absence via its sanity check, trigger
    // the send action and abort the job.
    wait_for_job_removed(&redis, &room, &identity, Duration::from_secs(6)).await;
    expect_delayed_event_request(&hs, "syd_integration_1", "send");
}

#[tokio::test]
async fn restarts_the_delayed_event_repeatedly() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Set up Redis.
    let redis = FakeRedis::new().await;

    // Start the service.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        redis_url: Some(redis.url().to_owned()),
        ..Default::default()
    })
    .await;

    // Post a valid /delegate_delayed_leave request with a short timeout, so
    // the periodic restart cycle (80% of delay_timeout) fires quickly.
    let delay_timeout_ms = 1500;
    let mut request = delegate_request(&hs, &user);
    request["delay_timeout"] = json!(delay_timeout_ms);
    let (status, body) = post_delegate(&svc, request.to_string()).await;

    // The request should succeed.
    assert_eq!(status, 200, "body: {body}");

    // The job should be persisted.
    let room = livekit_room_alias("!room:example.com", "m.call#");
    let identity = livekit_identity(&user.user_id, "DEVICE", "member-1");
    expect_job_persisted(&redis, &room, &identity);

    // Report that the participant connected.
    send_sfu_webhook(&svc, "participant_joined", &room, &identity, None).await;

    // The service should restart the delayed event at least twice.
    wait_for_delayed_event_request_count(
        &hs,
        "syd_integration_1",
        "restart",
        2,
        Duration::from_millis(delay_timeout_ms) + Duration::from_secs(2),
    )
    .await;

    // Report that the participant disconnected intentionally.
    send_sfu_webhook(
        &svc,
        "participant_left",
        &room,
        &identity,
        Some("CLIENT_INITIATED"),
    )
    .await;

    // The service should trigger the send action and abort the job.
    wait_for_job_removed(&redis, &room, &identity, Duration::from_secs(2)).await;
    expect_delayed_event_request(&hs, "syd_integration_1", "send");
}

#[tokio::test]
async fn aborts_when_delayed_event_is_gone() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Set up Redis.
    let redis = FakeRedis::new().await;

    // Start the service.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        redis_url: Some(redis.url().to_owned()),
        ..Default::default()
    })
    .await;

    // Post a valid /delegate_delayed_leave request with a short timeout, so
    // the periodic restart cycle (80% of delay_timeout) fires quickly.
    let delay_timeout_ms = 1000;
    let mut request = delegate_request(&hs, &user);
    request["delay_timeout"] = json!(delay_timeout_ms);
    let (status, body) = post_delegate(&svc, request.to_string()).await;

    // The request should succeed.
    assert_eq!(status, 200, "body: {body}");

    // The job should be persisted.
    let room = livekit_room_alias("!room:example.com", "m.call#");
    let identity = livekit_identity(&user.user_id, "DEVICE", "member-1");
    expect_job_persisted(&redis, &room, &identity);

    // Report that the participant connected.
    send_sfu_webhook(&svc, "participant_joined", &room, &identity, None).await;

    // The service should restart the delayed event.
    wait_for_delayed_event_request(&hs, "syd_integration_1", "restart", Duration::from_secs(2))
        .await;

    // Now make the homeserver report the delayed event as gone.
    hs.set_delayed_event_status(404);

    // The service should not trigger the send action and abort the job.
    wait_for_job_removed(
        &redis,
        &room,
        &identity,
        Duration::from_millis(delay_timeout_ms) + Duration::from_secs(2),
    )
    .await;
    expect_no_delayed_event_request(&hs, "syd_integration_1", "send");
}

#[tokio::test]
async fn completes_when_delayed_event_already_sent() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Set up Redis.
    let redis = FakeRedis::new().await;

    // Start the service.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        redis_url: Some(redis.url().to_owned()),
        ..Default::default()
    })
    .await;

    // Post a valid /delegate_delayed_leave request.
    let (status, body) = post_delegate(&svc, delegate_request(&hs, &user).to_string()).await;

    // The request should succeed.
    assert_eq!(status, 200, "body: {body}");

    // The job should be persisted.
    let room = livekit_room_alias("!room:example.com", "m.call#");
    let identity = livekit_identity(&user.user_id, "DEVICE", "member-1");
    expect_job_persisted(&redis, &room, &identity);

    // Report that the participant connected.
    send_sfu_webhook(&svc, "participant_joined", &room, &identity, None).await;

    // The service should restart the delayed event.
    wait_for_delayed_event_request(&hs, "syd_integration_1", "restart", Duration::from_secs(2))
        .await;

    // Now make the homeserver report the delayed event as already gone.
    hs.set_delayed_event_status(404);

    // Report that the participant disconnected intentionally.
    send_sfu_webhook(
        &svc,
        "participant_left",
        &room,
        &identity,
        Some("CLIENT_INITIATED"),
    )
    .await;

    // The service should attempt to send the delayed event, ignore the error and abort the job.
    wait_for_job_removed(&redis, &room, &identity, Duration::from_secs(2)).await;
    expect_delayed_event_request_count(&hs, "syd_integration_1", "send", 1);
}

#[tokio::test]
async fn job_replacement_only_affects_latest_job() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Set up Redis.
    let redis = FakeRedis::new().await;

    // Start the service.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        redis_url: Some(redis.url().to_owned()),
        ..Default::default()
    })
    .await;

    // Post a first /delegate_delayed_leave request.
    let mut first_request = delegate_request(&hs, &user);
    first_request["delay_id"] = json!("syd_first");
    let (status, body) = post_delegate(&svc, first_request.to_string()).await;

    // The request should succeed.
    assert_eq!(status, 200, "body: {body}");

    // The job should be persisted.
    let room = livekit_room_alias("!room:example.com", "m.call#");
    let identity = livekit_identity(&user.user_id, "DEVICE", "member-1");
    expect_job_persisted(&redis, &room, &identity);

    // Post a second /delegate_delayed_leave request for the same room and
    // member, but with a different delay_id.
    let mut second_request = delegate_request(&hs, &user);
    second_request["delay_id"] = json!("syd_second");
    let (status, body) = post_delegate(&svc, second_request.to_string()).await;

    // The request should succeed.
    assert_eq!(status, 200, "body: {body}");

    // The job should still be persisted.
    let room = livekit_room_alias("!room:example.com", "m.call#");
    let identity = livekit_identity(&user.user_id, "DEVICE", "member-1");
    expect_job_persisted(&redis, &room, &identity);

    // Report that the participant connected.
    send_sfu_webhook(&svc, "participant_joined", &room, &identity, None).await;

    // The service should restart the second delayed event.
    wait_for_delayed_event_request(&hs, "syd_second", "restart", Duration::from_secs(2)).await;

    // Report that the participant disconnected intentionally.
    send_sfu_webhook(
        &svc,
        "participant_left",
        &room,
        &identity,
        Some("CLIENT_INITIATED"),
    )
    .await;

    // The service should trigger the send action for the second delayed event and abort the job.
    wait_for_job_removed(&redis, &room, &identity, Duration::from_secs(2)).await;
    expect_delayed_event_request(&hs, "syd_second", "send");

    // The service should not have triggered any requests for the first delayed event.
    expect_no_delayed_event_request(&hs, "syd_first", "restart");
    expect_no_delayed_event_request(&hs, "syd_first", "send");
}

#[tokio::test]
async fn independent_jobs_do_not_cross_talk() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let alice = hs.new_user("alice");
    let bob = hs.new_user("bob");

    // Set up Redis.
    let redis = FakeRedis::new().await;

    // Start the service.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        redis_url: Some(redis.url().to_owned()),
        ..Default::default()
    })
    .await;

    // Post a /delegate_delayed_leave request for Alice, in one room.
    let mut alice_request = delegate_request(&hs, &alice);
    alice_request["room_id"] = json!("!room-a:example.com");
    alice_request["delay_id"] = json!("syd_alice");
    let (status, body) = post_delegate(&svc, alice_request.to_string()).await;

    // The request should succeed.
    assert_eq!(status, 200, "body: {body}");

    // The job should be persisted.
    let alice_room = livekit_room_alias("!room-a:example.com", "m.call#");
    let alice_identity = livekit_identity(&alice.user_id, "DEVICE", "member-1");
    expect_job_persisted(&redis, &alice_room, &alice_identity);

    // Post a /delegate_delayed_leave request for Bob, in a different room.
    let mut bob_request = delegate_request(&hs, &bob);
    bob_request["room_id"] = json!("!room-b:example.com");
    bob_request["delay_id"] = json!("syd_bob");
    let (status, body) = post_delegate(&svc, bob_request.to_string()).await;

    // The request should succeed.
    assert_eq!(status, 200, "body: {body}");

    // The job should be persisted.
    let bob_room = livekit_room_alias("!room-b:example.com", "m.call#");
    let bob_identity = livekit_identity(&bob.user_id, "DEVICE", "member-1");
    expect_job_persisted(&redis, &bob_room, &bob_identity);

    // Report that both participants connected.
    send_sfu_webhook(
        &svc,
        "participant_joined",
        &alice_room,
        &alice_identity,
        None,
    )
    .await;
    send_sfu_webhook(&svc, "participant_joined", &bob_room, &bob_identity, None).await;

    // The service should restart both delayed events.
    wait_for_delayed_event_request(&hs, "syd_alice", "restart", Duration::from_secs(2)).await;
    wait_for_delayed_event_request(&hs, "syd_bob", "restart", Duration::from_secs(2)).await;

    // Report that Alice disconnected intentionally.
    send_sfu_webhook(
        &svc,
        "participant_left",
        &alice_room,
        &alice_identity,
        Some("CLIENT_INITIATED"),
    )
    .await;

    // The service should trigger the send action for Alice's delayed event and abort the job.
    wait_for_job_removed(&redis, &alice_room, &alice_identity, Duration::from_secs(2)).await;
    expect_delayed_event_request(&hs, "syd_alice", "send");

    // Bob's delayed event should not have been sent and the job should still be persisted.
    expect_no_delayed_event_request(&hs, "syd_bob", "send");
    expect_job_persisted(&redis, &bob_room, &bob_identity);

    // Report that Bob disconnected intentionally.
    send_sfu_webhook(
        &svc,
        "participant_left",
        &bob_room,
        &bob_identity,
        Some("CLIENT_INITIATED"),
    )
    .await;

    // Now the service should trigger the send action for Bob's delayed event and abort the job, too.
    wait_for_job_removed(&redis, &bob_room, &bob_identity, Duration::from_secs(2)).await;
    expect_delayed_event_request(&hs, "syd_bob", "send");
}

#[tokio::test]
async fn job_survives_a_service_restart() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Set up Redis.
    let redis = FakeRedis::new().await;

    // Start the service.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        redis_url: Some(redis.url().to_owned()),
        ..Default::default()
    })
    .await;

    // Post a valid /delegate_delayed_leave request.
    let (status, body) = post_delegate(&svc, delegate_request(&hs, &user).to_string()).await;

    // The request should succeed.
    assert_eq!(status, 200, "body: {body}");

    // The job should be persisted.
    let room = livekit_room_alias("!room:example.com", "m.call#");
    let identity = livekit_identity(&user.user_id, "DEVICE", "member-1");
    expect_job_persisted(&redis, &room, &identity);

    // Kill and restart the service.
    drop(svc);
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        redis_url: Some(redis.url().to_owned()),
        ..Default::default()
    })
    .await;

    // The restored job should still be persisted.
    expect_job_persisted(&redis, &room, &identity);

    // Report that the participant connected.
    send_sfu_webhook(&svc, "participant_joined", &room, &identity, None).await;

    // The service should restart the delayed event.
    wait_for_delayed_event_request(&hs, "syd_integration_1", "restart", Duration::from_secs(2))
        .await;

    // Report that the participant disconnected intentionally.
    send_sfu_webhook(
        &svc,
        "participant_left",
        &room,
        &identity,
        Some("CLIENT_INITIATED"),
    )
    .await;

    // The service should trigger the send action and abort the job.
    wait_for_job_removed(&redis, &room, &identity, Duration::from_secs(2)).await;
    expect_delayed_event_request(&hs, "syd_integration_1", "send");
}

#[tokio::test]
async fn expired_jobs_are_purged_on_restart() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Set up Redis.
    let redis = FakeRedis::new().await;

    // Start the service.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        redis_url: Some(redis.url().to_owned()),
        ..Default::default()
    })
    .await;

    // Post a valid /delegate_delayed_leave request with a short timeout.
    let delay_timeout_ms = 500;
    let mut request = delegate_request(&hs, &user);
    request["delay_timeout"] = json!(delay_timeout_ms);
    let (status, body) = post_delegate(&svc, request.to_string()).await;

    // The request should succeed.
    assert_eq!(status, 200, "body: {body}");

    // The job should be persisted.
    let room = livekit_room_alias("!room:example.com", "m.call#");
    let identity = livekit_identity(&user.user_id, "DEVICE", "member-1");
    expect_job_persisted(&redis, &room, &identity);

    // Kill the service.
    drop(svc);

    // Wait for the job's delay_timeout to elapse.
    tokio::time::sleep(Duration::from_millis(delay_timeout_ms) + Duration::from_millis(500)).await;

    // Start a new service against the same store.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        redis_url: Some(redis.url().to_owned()),
        ..Default::default()
    })
    .await;

    // The expired job should have been purged rather than restored.
    expect_job_not_persisted(&redis, &room, &identity);

    // The service should never have called /delayed_events.
    expect_no_delayed_event_requests(&hs);

    drop(svc);
}

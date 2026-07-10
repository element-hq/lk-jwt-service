// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use lk_jwt_service_integration_tests::{
    FakeHomeserver, FakeUser, Service, ServiceConfig, expect_matrix_error,
};
use serde_json::{Value, json};

/// Return a valid /get_token body for the given user.
fn get_token_request(hs: &FakeHomeserver, user: &FakeUser) -> Value {
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
    })
}

/// POST the given body to /get_token and return the status code and raw
/// response body.
async fn post_get_token(svc: &Service, body: impl Into<reqwest::Body>) -> (u16, String) {
    let resp = reqwest::Client::new()
        .post(format!("{}/get_token", svc.base_url))
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

    // Start the service. The fake homeserver is NOT in the full-access list, so no
    // no LiveKit room should be created.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["trusted.example.com".to_owned()],
        livekit_url: Some("wss://sfu.example.com".to_owned()),
        ..Default::default()
    })
    .await;

    // Post a valid /get_token request.
    let (status, body) = post_get_token(&svc, get_token_request(&hs, &user).to_string()).await;

    // The request should succeed.
    assert_eq!(status, 200, "body: {body}");

    // The response should contain the configured SFU URL and a LiveKit JWT.
    let response: Value = serde_json::from_str(&body).expect("response is not JSON");
    assert_eq!(response["url"].as_str(), Some("wss://sfu.example.com"));
    assert!(
        !response["jwt"].as_str().unwrap_or_default().is_empty(),
        "expected a non-empty jwt, got {body}"
    );

    // The service should have called /openid/userinfo.
    let requests = hs.user_info_requests();
    assert_eq!(requests.len(), 1, "expected exactly one user info lookup");
    assert_eq!(requests[0].access_token, user.openid_token);

    // The service should not have called /delayed_events.
    assert!(
        hs.delayed_event_requests().is_empty(),
        "expected no delayed event actions, got {:?}",
        hs.delayed_event_requests()
    );
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

    // Post a /get_token request with an invalid OpenID token.
    let mut request = get_token_request(&hs, &user);
    request["openid_token"]["access_token"] = json!("syt_forged_token");
    let (status, body) = post_get_token(&svc, request.to_string()).await;

    // The request should fail.
    expect_matrix_error(status, &body, 401, "M_UNAUTHORIZED");

    // The service should have called /openid/userinfo.
    let requests = hs.user_info_requests();
    assert_eq!(requests.len(), 1, "expected exactly one user info lookup");
    assert_eq!(requests[0].access_token, "syt_forged_token");

    // The service should not have called /delayed_events.
    assert!(
        hs.delayed_event_requests().is_empty(),
        "expected no delayed event actions, got {:?}",
        hs.delayed_event_requests()
    );
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

    // Post a /get_token request with a different MXID.
    let mut request = get_token_request(&hs, &user);
    request["member"]["claimed_user_id"] = json!(format!("@bob:{}", hs.server_name()));

    // The request should fail.
    let (status, body) = post_get_token(&svc, request.to_string()).await;
    expect_matrix_error(status, &body, 401, "M_UNAUTHORIZED");

    // The service should have called /openid/userinfo.
    let requests = hs.user_info_requests();
    assert_eq!(requests.len(), 1, "expected exactly one user info lookup");
    assert_eq!(requests[0].access_token, user.openid_token);

    // The service should not have called /delayed_events.
    assert!(
        hs.delayed_event_requests().is_empty(),
        "expected no delayed event actions, got {:?}",
        hs.delayed_event_requests()
    );
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

    // Post a /get_token request with delay parameters. Delegation of
    // delayed events is only supported for full-access users.
    let mut request = get_token_request(&hs, &user);
    request["delay_id"] = json!("syd_integration_1");
    request["delay_timeout"] = json!(8000);
    let (status, body) = post_get_token(&svc, request.to_string()).await;

    // The request should fail.
    expect_matrix_error(status, &body, 400, "M_BAD_JSON");

    // The service should have called /openid/userinfo.
    let requests = hs.user_info_requests();
    assert_eq!(requests.len(), 1, "expected exactly one user info lookup");
    assert_eq!(requests[0].access_token, user.openid_token);

    // The service should not have called /delayed_events.
    assert!(
        hs.delayed_event_requests().is_empty(),
        "expected no delayed event actions, got {:?}",
        hs.delayed_event_requests()
    );
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

    // Post a /get_token request with delay parameters.
    let mut request = get_token_request(&hs, &user);
    request["delay_id"] = json!("syd_integration_1");
    request["delay_timeout"] = json!(8000);
    let (status, body) = post_get_token(&svc, request.to_string()).await;

    // The request should fail.
    expect_matrix_error(status, &body, 400, "M_BAD_JSON");

    // The service should have called /openid/userinfo.
    let requests = hs.user_info_requests();
    assert_eq!(requests.len(), 1, "expected exactly one user info lookup");
    assert_eq!(requests[0].access_token, user.openid_token);

    // The service should not have called /delayed_events.
    assert!(
        hs.delayed_event_requests().is_empty(),
        "expected no delayed event actions, got {:?}",
        hs.delayed_event_requests()
    );
}

#[tokio::test]
async fn partial_delay_params() {
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

    // Post a /get_token request with delay_id but without delay_timeout.
    let mut request = get_token_request(&hs, &user);
    request["delay_id"] = json!("syd_integration_1");
    let (status, body) = post_get_token(&svc, request.to_string()).await;

    // The request should fail.
    expect_matrix_error(status, &body, 400, "M_BAD_JSON");

    // Post a /get_token request with delay_timeout but without delay_id.
    let mut request = get_token_request(&hs, &user);
    request["delay_timeout"] = json!(8000);
    let (status, body) = post_get_token(&svc, request.to_string()).await;

    // The request should fail.
    expect_matrix_error(status, &body, 400, "M_BAD_JSON");

    // The service should not have called /openid/userinfo.
    assert!(
        hs.user_info_requests().is_empty(),
        "expected no user info lookups, got {:?}",
        hs.user_info_requests()
    );

    // The service should not have called /delayed_events.
    assert!(
        hs.delayed_event_requests().is_empty(),
        "expected no delayed event actions, got {:?}",
        hs.delayed_event_requests()
    );
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

    // Post a /get_token request with malformed JSON.
    let (status, body) = post_get_token(&svc, "{not json").await;

    // The request should fail.
    expect_matrix_error(status, &body, 400, "M_NOT_JSON");

    // The service should not have called /openid/userinfo.
    assert!(
        hs.user_info_requests().is_empty(),
        "expected no user info lookups, got {:?}",
        hs.user_info_requests()
    );

    // The service should not have called /delayed_events.
    assert!(
        hs.delayed_event_requests().is_empty(),
        "expected no delayed event actions, got {:?}",
        hs.delayed_event_requests()
    );
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

    // Send a /get_token request using GET instead of POST.
    let url = format!("{}/get_token", svc.base_url);
    let client = reqwest::Client::new();
    let resp = client.get(&url).send().await.expect("GET failed");

    // The request should fail.
    assert_eq!(resp.status().as_u16(), 405);

    // The service should not have called /openid/userinfo.
    assert!(
        hs.user_info_requests().is_empty(),
        "expected no user info lookups, got {:?}",
        hs.user_info_requests()
    );

    // The service should not have called /delayed_events.
    assert!(
        hs.delayed_event_requests().is_empty(),
        "expected no delayed event actions, got {:?}",
        hs.delayed_event_requests()
    );
}

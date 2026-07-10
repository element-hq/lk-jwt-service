// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use std::collections::HashMap;

use lk_jwt_service_integration_tests::{
    FakeHomeserver, FakeUser, Service, ServiceConfig, expect_matrix_error,
    expect_no_user_info_lookups, expect_user_info_lookup,
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

/// Start the service with LIVEKIT_INSECURE_SKIP_VERIFY_TLS overridden to the
/// given value. No homeserver is in the full-access list so that a successful
/// token request does require an SFU.
async fn start_service_with_skip_verify_tls(value: &str) -> Service {
    Service::start(ServiceConfig {
        full_access_homeservers: vec!["trusted.example.com".to_owned()],
        extra_env: HashMap::from([(
            "LIVEKIT_INSECURE_SKIP_VERIFY_TLS".to_owned(),
            value.to_owned(),
        )]),
        ..Default::default()
    })
    .await
}

#[tokio::test]
async fn enabled_accepts_self_signed_tls() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Start the service with skipping explicitly enabled.
    let svc = start_service_with_skip_verify_tls("YES_I_KNOW_WHAT_I_AM_DOING").await;

    // Post a valid /get_token request.
    let (status, body) = post_get_token(&svc, get_token_request(&hs, &user).to_string()).await;

    // The request should succeed.
    assert_eq!(status, 200, "body: {body}");

    // The service should have called /openid/userinfo.
    expect_user_info_lookup(&hs, &user.openid_token);
}

#[tokio::test]
async fn unset_rejects_self_signed_tls() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Start the service with skipping disabled.
    let svc = start_service_with_skip_verify_tls("").await;

    // Post a valid /get_token request.
    let (status, body) = post_get_token(&svc, get_token_request(&hs, &user).to_string()).await;

    // The request should fail.
    expect_matrix_error(status, &body, 401, "M_UNAUTHORIZED");

    // The service should not have called /openid/userinfo.
    expect_no_user_info_lookups(&hs);
}

#[tokio::test]
async fn wrong_value_rejects_self_signed_tls() {
    // Set up the homeserver.
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Start the service with an invalid value.
    let svc = start_service_with_skip_verify_tls("true").await;

    // Post a valid /get_token request.
    let (status, body) = post_get_token(&svc, get_token_request(&hs, &user).to_string()).await;

    // The request should fail.
    expect_matrix_error(status, &body, 401, "M_UNAUTHORIZED");

    // The service should not have called /openid/userinfo.
    expect_no_user_info_lookups(&hs);
}

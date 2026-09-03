// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use std::collections::HashMap;

use lk_jwt_service_integration_tests::{
    DEFAULT_LK_URL, FakeHomeserver, FakeSfu, Service, ServiceConfig, decode_livekit_jwt,
    expect_is_joined_request, expect_matrix_error,
};
use serde_json::{Value, json};

const AS_TOKEN: &str = "as_token";
const HS_TOKEN: &str = "hs_token";
const ORIGIN_SERVER: &str = "origin.example.org";

const GET_TOKEN_SS_PATH: &str =
    "/_matrix/federation/unstable/io.element.msc4195/rtc/livekit/get_token";

/// App-service configuration as extra_env.
fn app_service_env(hs_server_name: &str) -> HashMap<String, String> {
    HashMap::from([
        ("LIVEKIT_AS_TOKEN".to_owned(), AS_TOKEN.to_owned()),
        ("LIVEKIT_HS_TOKEN".to_owned(), HS_TOKEN.to_owned()),
        (
            "LIVEKIT_HS_SERVER_NAME".to_owned(),
            hs_server_name.to_owned(),
        ),
    ])
}

/// Return a valid /rtc/livekit/get_token S-S request body.
fn get_token_ss_request(user_id: &str, lk_url: &str) -> Value {
    json!({
        "url": lk_url,
        "user_id": user_id,
        "room_id": "!room:example.com",
        "slot_id": "m.call#",
        "member": {
            "id": "member-1",
            "claimed_device_id": "DEVICE",
        },
    })
}

/// POST the given body to the /rtc/livekit/get_token S-S endpoint and
/// return the status code and raw response body.
async fn post_get_token_ss(
    svc: &Service,
    body: impl Into<reqwest::Body>,
    origin: Option<&str>,
    hs_token: Option<&str>,
) -> (u16, String) {
    let mut req = reqwest::Client::new()
        .post(format!("{}{GET_TOKEN_SS_PATH}", svc.base_url))
        .header("Content-Type", "application/json");
    if let Some(origin) = origin {
        req = req.header("X-Matrix-Origin", origin);
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
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        extra_env: app_service_env("example.com"),
        ..Default::default()
    })
    .await;

    let (status, body) = post_get_token_ss(
        &svc,
        get_token_ss_request("@alice:origin.example.org", DEFAULT_LK_URL).to_string(),
        Some(ORIGIN_SERVER),
        None,
    )
    .await;

    expect_matrix_error(status, &body, 401, "M_MISSING_TOKEN");
}

/// A wrong homeserver token is rejected.
#[tokio::test]
async fn wrong_hs_token() {
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        extra_env: app_service_env("example.com"),
        ..Default::default()
    })
    .await;

    let (status, body) = post_get_token_ss(
        &svc,
        get_token_ss_request("@alice:origin.example.org", DEFAULT_LK_URL).to_string(),
        Some(ORIGIN_SERVER),
        Some("not_the_hs_token"),
    )
    .await;

    expect_matrix_error(status, &body, 401, "M_UNKNOWN_TOKEN");
}

/// A missing X-Matrix-Origin header is rejected.
#[tokio::test]
async fn missing_origin_header() {
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        extra_env: app_service_env("example.com"),
        ..Default::default()
    })
    .await;

    let (status, body) = post_get_token_ss(
        &svc,
        get_token_ss_request("@alice:origin.example.org", DEFAULT_LK_URL).to_string(),
        None,
        Some(HS_TOKEN),
    )
    .await;

    expect_matrix_error(status, &body, 401, "M_UNAUTHORIZED");
}

/// A missing url is rejected.
#[tokio::test]
async fn missing_url() {
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        extra_env: app_service_env("example.com"),
        ..Default::default()
    })
    .await;

    let mut request = get_token_ss_request("@alice:origin.example.org", DEFAULT_LK_URL);
    request["url"] = json!("");
    let (status, body) = post_get_token_ss(
        &svc,
        request.to_string(),
        Some(ORIGIN_SERVER),
        Some(HS_TOKEN),
    )
    .await;

    expect_matrix_error(status, &body, 400, "M_BAD_JSON");
}

/// Malformed JSON is rejected.
#[tokio::test]
async fn malformed_json() {
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        extra_env: app_service_env("example.com"),
        ..Default::default()
    })
    .await;

    let (status, body) =
        post_get_token_ss(&svc, "{not json", Some(ORIGIN_SERVER), Some(HS_TOKEN)).await;

    expect_matrix_error(status, &body, 400, "M_NOT_JSON");
}

/// GET requests are rejected.
#[tokio::test]
async fn get_instead_of_post() {
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        extra_env: app_service_env("example.com"),
        ..Default::default()
    })
    .await;

    let url = format!("{}{GET_TOKEN_SS_PATH}", svc.base_url);
    let resp = reqwest::Client::new()
        .get(&url)
        .send()
        .await
        .expect("GET failed");

    assert_eq!(resp.status().as_u16(), 405);
}

/// A valid request from a joined user of the origin server succeeds, the
/// user's membership is verified, and the LiveKit room is created.
#[tokio::test]
async fn success() {
    let hs = FakeHomeserver::new().await;
    let sfu = FakeSfu::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        livekit_url: Some(sfu.url().to_owned()),
        extra_env: app_service_env(hs.server_name()),
        ..Default::default()
    })
    .await;

    let (status, body) = post_get_token_ss(
        &svc,
        get_token_ss_request("@alice:origin.example.org", sfu.url()).to_string(),
        Some(ORIGIN_SERVER),
        Some(HS_TOKEN),
    )
    .await;

    assert_eq!(status, 200, "body: {body}");
    let response: Value = serde_json::from_str(&body).expect("response is not JSON");
    let jwt = response["jwt"].as_str().unwrap_or_default();

    let claims = decode_livekit_jwt(jwt);
    assert_eq!(claims["video"]["roomJoin"].as_bool(), Some(true));
    assert_eq!(claims["video"]["roomCreate"].as_bool(), Some(false));
    assert_eq!(claims["video"]["canPublish"].as_bool(), Some(false));
    assert_eq!(claims["video"]["canSubscribe"].as_bool(), Some(true));

    expect_is_joined_request(
        &hs,
        "!room:example.com",
        "@alice:origin.example.org",
        AS_TOKEN,
    );

    let rooms = sfu.create_room_requests();
    assert_eq!(rooms.len(), 1, "expected exactly one room creation");
    assert_eq!(
        claims["video"]["room"].as_str(),
        Some(rooms[0].name.as_str())
    );
}

/// A request is rejected when the requesting user is not joined to the room.
#[tokio::test]
async fn user_not_a_member() {
    let hs = FakeHomeserver::new().await;
    hs.set_not_joined("!room:example.com", "@alice:origin.example.org");

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env(hs.server_name()),
        ..Default::default()
    })
    .await;

    let (status, body) = post_get_token_ss(
        &svc,
        get_token_ss_request("@alice:origin.example.org", DEFAULT_LK_URL).to_string(),
        Some(ORIGIN_SERVER),
        Some(HS_TOKEN),
    )
    .await;

    expect_matrix_error(status, &body, 403, "M_FORBIDDEN");
}

/// A request is rejected when `user_id` doesn't belong to the origin
/// server, without ever checking room membership.
#[tokio::test]
async fn user_id_domain_mismatch() {
    let hs = FakeHomeserver::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env(hs.server_name()),
        ..Default::default()
    })
    .await;

    let (status, body) = post_get_token_ss(
        &svc,
        get_token_ss_request("@alice:not-the-origin.example.org", DEFAULT_LK_URL).to_string(),
        Some(ORIGIN_SERVER),
        Some(HS_TOKEN),
    )
    .await;

    expect_matrix_error(status, &body, 403, "M_FORBIDDEN");
}

/// A `url` not matching the configured LIVEKIT_URL is rejected.
#[tokio::test]
async fn url_mismatch() {
    let hs = FakeHomeserver::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env(hs.server_name()),
        ..Default::default()
    })
    .await;

    let (status, body) = post_get_token_ss(
        &svc,
        get_token_ss_request(
            "@alice:origin.example.org",
            "wss://not-the-configured-sfu.example.com",
        )
        .to_string(),
        Some(ORIGIN_SERVER),
        Some(HS_TOKEN),
    )
    .await;

    expect_matrix_error(status, &body, 400, "M_INVALID_PARAM");
}

/// A C-S API resolution failure for our own homeserver triggers rejection.
#[tokio::test]
async fn unresolvable_cs_api() {
    let hs = FakeHomeserver::new().await;

    // No CS API override, so it should fall back to .well-known discovery
    // against the fake homeserver, which doesn't serve it.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["*".to_owned()],
        extra_env: app_service_env(hs.server_name()),
        ..Default::default()
    })
    .await;

    let (status, body) = post_get_token_ss(
        &svc,
        get_token_ss_request("@alice:origin.example.org", DEFAULT_LK_URL).to_string(),
        Some(ORIGIN_SERVER),
        Some(HS_TOKEN),
    )
    .await;

    expect_matrix_error(status, &body, 400, "M_BAD_JSON");
}

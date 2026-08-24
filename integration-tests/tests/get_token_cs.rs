// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use std::collections::HashMap;

use lk_jwt_service_integration_tests::{
    DEFAULT_LK_URL, FakeHomeserver, FakeSfu, FakeUser, Msc4502Support, Service, ServiceConfig,
    decode_livekit_jwt, expect_is_joined_request, expect_matrix_error, expect_no_is_joined_request,
    expect_no_is_joined_requests, expect_no_user_info_lookups,
};
use serde_json::{Value, json};

const AS_TOKEN: &str = "as_token";
const HS_TOKEN: &str = "hs_token";

const GET_TOKEN_CS_PATH: &str = "/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token";

/// App-service tokens as extra_env.
fn app_service_env() -> HashMap<String, String> {
    HashMap::from([
        ("LIVEKIT_AS_TOKEN".to_owned(), AS_TOKEN.to_owned()),
        ("LIVEKIT_HS_TOKEN".to_owned(), HS_TOKEN.to_owned()),
    ])
}

/// Return a valid /rtc/livekit/get_token C-S request body.
fn get_token_cs_request(user: &FakeUser, lk_url: &str) -> Value {
    json!({
        "room_id": "!room:example.com",
        "slot_id": "m.call#",
        "url": lk_url,
        "member": {
            "id": "member-1",
            "claimed_user_id": user.user_id,
            "claimed_device_id": "DEVICE",
        },
    })
}

/// POST the given body to the /rtc/livekit/get_token C-S
/// endpoint and return the status code and raw response body.
async fn post_get_token_cs(
    svc: &Service,
    body: impl Into<reqwest::Body>,
    header_mxid: Option<&str>,
) -> (u16, String) {
    post_get_token_cs_as(svc, body, header_mxid, Some(HS_TOKEN)).await
}

/// POST the given body to the /rtc/livekit/get_token C-S
/// endpoint and return the status code and raw response body.
async fn post_get_token_cs_as(
    svc: &Service,
    body: impl Into<reqwest::Body>,
    header_mxid: Option<&str>,
    hs_token: Option<&str>,
) -> (u16, String) {
    let mut req = reqwest::Client::new()
        .post(format!("{}{GET_TOKEN_CS_PATH}", svc.base_url))
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
    let user = hs.new_user("alice");

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env(),
        ..Default::default()
    })
    .await;

    let (status, body) = post_get_token_cs_as(
        &svc,
        get_token_cs_request(&user, DEFAULT_LK_URL).to_string(),
        Some(&user.user_id),
        None,
    )
    .await;

    expect_matrix_error(status, &body, 401, "M_MISSING_TOKEN");
    expect_no_is_joined_requests(&hs);
}

/// A wrong homeserver token is rejected.
#[tokio::test]
async fn wrong_hs_token() {
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env(),
        ..Default::default()
    })
    .await;

    let (status, body) = post_get_token_cs_as(
        &svc,
        get_token_cs_request(&user, DEFAULT_LK_URL).to_string(),
        Some(&user.user_id),
        Some("not_the_hs_token"),
    )
    .await;

    expect_matrix_error(status, &body, 401, "M_UNKNOWN_TOKEN");
    expect_no_is_joined_requests(&hs);
}

/// A missing X-Matrix-User-Identifierheader is rejected.
#[tokio::test]
async fn missing_header() {
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env(),
        ..Default::default()
    })
    .await;

    let (status, body) = post_get_token_cs(
        &svc,
        get_token_cs_request(&user, DEFAULT_LK_URL).to_string(),
        None,
    )
    .await;

    expect_matrix_error(status, &body, 401, "M_UNAUTHORIZED");
    expect_no_user_info_lookups(&hs);
    expect_no_is_joined_requests(&hs);
}

/// The endpoint derives identity solely from the X-Matrix-User-Identifier header.
#[tokio::test]
async fn claimed_user_id_mismatch_is_ignored() {
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // Restricted (non-full-access), so the test doesn't need a live SFU to
    // exercise room creation.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["trusted.example.com".to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env(),
        ..Default::default()
    })
    .await;

    // The body claims to be alice, but the (trusted) header says bob.
    let request = get_token_cs_request(&user, DEFAULT_LK_URL);
    let header_mxid = format!("@bob:{}", hs.server_name());
    let (status, body) = post_get_token_cs(&svc, request.to_string(), Some(&header_mxid)).await;

    // The request succeeds, using bob's identity from the header rather
    // than alice's claimed_user_id in the body.
    assert_eq!(status, 200, "body: {body}");
    expect_is_joined_request(&hs, "!room:example.com", &header_mxid);
    expect_no_is_joined_request(&hs, "!room:example.com", &user.user_id);
}

/// A malformed MXID header is rejected.
#[tokio::test]
async fn malformed_header() {
    let hs = FakeHomeserver::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env(),
        ..Default::default()
    })
    .await;

    let request = json!({
        "room_id": "!room:example.com",
        "slot_id": "m.call#",
        "url": DEFAULT_LK_URL,
        "member": {
            "id": "member-1",
            "claimed_user_id": "not-an-mxid",
            "claimed_device_id": "DEVICE",
        },
    });
    let (status, body) = post_get_token_cs(&svc, request.to_string(), Some("not-an-mxid")).await;
    expect_matrix_error(status, &body, 400, "M_INVALID_PARAM");
}

/// A url not matching the service's configured LIVEKIT_URL is rejected.
#[tokio::test]
async fn url_mismatch() {
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env(),
        ..Default::default()
    })
    .await;

    let (status, body) = post_get_token_cs(
        &svc,
        get_token_cs_request(&user, "wss://not-the-configured-sfu.example.com").to_string(),
        Some(&user.user_id),
    )
    .await;

    expect_matrix_error(status, &body, 400, "M_INVALID_PARAM");
    expect_no_is_joined_requests(&hs);
}

/// A missing url is rejected.
#[tokio::test]
async fn missing_url() {
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env(),
        ..Default::default()
    })
    .await;

    let mut request = get_token_cs_request(&user, DEFAULT_LK_URL);
    request["url"] = json!("");
    let (status, body) = post_get_token_cs(&svc, request.to_string(), Some(&user.user_id)).await;

    expect_matrix_error(status, &body, 400, "M_BAD_JSON");
    expect_no_is_joined_requests(&hs);
}

/// An unjoined user is rejected.
#[tokio::test]
async fn not_a_room_member() {
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");
    hs.set_not_joined("!room:example.com", &user.user_id);

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env(),
        ..Default::default()
    })
    .await;

    let (status, body) = post_get_token_cs(
        &svc,
        get_token_cs_request(&user, DEFAULT_LK_URL).to_string(),
        Some(&user.user_id),
    )
    .await;

    expect_matrix_error(status, &body, 403, "M_FORBIDDEN");
    expect_is_joined_request(&hs, "!room:example.com", &user.user_id);
}

/// A C-S API resolution failure triggers rejection.
#[tokio::test]
async fn unresolvable_cs_api() {
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");

    // No CS API override, so it should fall back to .well-known discovery
    // against the fake homeserver, which doesn't serve it.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        extra_env: app_service_env(),
        ..Default::default()
    })
    .await;

    let (status, body) = post_get_token_cs(
        &svc,
        get_token_cs_request(&user, DEFAULT_LK_URL).to_string(),
        Some(&user.user_id),
    )
    .await;

    expect_matrix_error(status, &body, 400, "M_BAD_JSON");
    expect_no_is_joined_requests(&hs);
}

/// Malformed JSON is rejected.
#[tokio::test]
async fn malformed_json() {
    let hs = FakeHomeserver::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env(),
        ..Default::default()
    })
    .await;

    let (status, body) = post_get_token_cs(&svc, "{not json", Some("@alice:example.com")).await;

    expect_matrix_error(status, &body, 400, "M_NOT_JSON");
    expect_no_is_joined_requests(&hs);
}

/// GET requests are rejected.
#[tokio::test]
async fn get_instead_of_post() {
    let hs = FakeHomeserver::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        extra_env: app_service_env(),
        ..Default::default()
    })
    .await;

    let url = format!("{}{GET_TOKEN_CS_PATH}", svc.base_url);
    let resp = reqwest::Client::new()
        .get(&url)
        .send()
        .await
        .expect("GET failed");

    assert_eq!(resp.status().as_u16(), 405);
    expect_no_is_joined_requests(&hs);
}

/// A full-access, joined user gets a token and triggers room creation,
/// using the unstable is_joined endpoint.
#[tokio::test]
async fn full_access_token_unstable_is_joined() {
    let hs = FakeHomeserver::new().await;
    hs.set_msc4502_support(Msc4502Support::Unstable);
    let user = hs.new_user("alice");
    let sfu = FakeSfu::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        livekit_url: Some(sfu.url().to_owned()),
        extra_env: app_service_env(),
        ..Default::default()
    })
    .await;

    let (status, body) = post_get_token_cs(
        &svc,
        get_token_cs_request(&user, sfu.url()).to_string(),
        Some(&user.user_id),
    )
    .await;

    assert_eq!(status, 200, "body: {body}");

    let response: Value = serde_json::from_str(&body).expect("response is not JSON");
    let jwt = response["jwt"].as_str().unwrap_or_default();

    let claims = decode_livekit_jwt(jwt);
    assert_eq!(claims["video"]["roomJoin"].as_bool(), Some(true));
    assert_eq!(claims["video"]["canPublish"].as_bool(), Some(true));
    assert_eq!(claims["video"]["canSubscribe"].as_bool(), Some(true));
    assert_eq!(
        claims["video"]["canUpdateOwnMetadata"].as_bool(),
        Some(true)
    );

    expect_is_joined_request(&hs, "!room:example.com", &user.user_id);

    let rooms = sfu.create_room_requests();
    assert_eq!(rooms.len(), 1, "expected exactly one room creation");
    assert_eq!(
        claims["video"]["room"].as_str(),
        Some(rooms[0].name.as_str())
    );
}

/// A full-access, joined user gets a token and triggers room creation,
/// using the stable is_joined endpoint.
#[tokio::test]
async fn full_access_token_stable_is_joined() {
    let hs = FakeHomeserver::new().await;
    hs.set_msc4502_support(Msc4502Support::Stable);
    let user = hs.new_user("alice");
    let sfu = FakeSfu::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        livekit_url: Some(sfu.url().to_owned()),
        extra_env: app_service_env(),
        ..Default::default()
    })
    .await;

    let (status, body) = post_get_token_cs(
        &svc,
        get_token_cs_request(&user, sfu.url()).to_string(),
        Some(&user.user_id),
    )
    .await;

    assert_eq!(status, 200, "body: {body}");

    let response: Value = serde_json::from_str(&body).expect("response is not JSON");
    let jwt = response["jwt"].as_str().unwrap_or_default();

    let claims = decode_livekit_jwt(jwt);
    assert_eq!(claims["video"]["roomJoin"].as_bool(), Some(true));
    assert_eq!(claims["video"]["canPublish"].as_bool(), Some(true));
    assert_eq!(claims["video"]["canSubscribe"].as_bool(), Some(true));
    assert_eq!(
        claims["video"]["canUpdateOwnMetadata"].as_bool(),
        Some(true)
    );

    expect_is_joined_request(&hs, "!room:example.com", &user.user_id);

    let rooms = sfu.create_room_requests();
    assert_eq!(rooms.len(), 1, "expected exactly one room creation");
    assert_eq!(
        claims["video"]["room"].as_str(),
        Some(rooms[0].name.as_str())
    );
}

/// A restricted (non-full-access) but joined user gets a token for an
/// existing room, without triggering room creation.
#[tokio::test]
async fn restricted_homeserver_joined_user() {
    let hs = FakeHomeserver::new().await;
    let user = hs.new_user("alice");
    let sfu = FakeSfu::new().await;

    // The fake homeserver is NOT in the full-access list.
    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec!["trusted.example.com".to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        livekit_url: Some(sfu.url().to_owned()),
        extra_env: app_service_env(),
        ..Default::default()
    })
    .await;

    let (status, body) = post_get_token_cs(
        &svc,
        get_token_cs_request(&user, sfu.url()).to_string(),
        Some(&user.user_id),
    )
    .await;

    assert_eq!(status, 200, "body: {body}");

    let response: Value = serde_json::from_str(&body).expect("response is not JSON");
    let jwt = response["jwt"].as_str().unwrap_or_default();
    let claims = decode_livekit_jwt(jwt);
    assert_eq!(claims["video"]["roomJoin"].as_bool(), Some(true));

    expect_is_joined_request(&hs, "!room:example.com", &user.user_id);

    assert!(
        sfu.create_room_requests().is_empty(),
        "expected no room creations, got {:?}",
        sfu.create_room_requests()
    );
}

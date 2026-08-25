// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use std::collections::HashMap;

use lk_jwt_service_integration_tests::{
    DEFAULT_LK_URL, FakeHomeserver, FakeSfu, FakeUser, Msc4502Support, Service, ServiceConfig,
    decode_livekit_jwt, expect_fed_proxy_request, expect_is_joined_request, expect_matrix_error,
    expect_no_fed_proxy_requests, expect_no_is_joined_requests, expect_no_user_info_lookups,
};
use serde_json::{Value, json};

const AS_TOKEN: &str = "as_token";
const HS_TOKEN: &str = "hs_token";

const GET_TOKEN_CS_PATH: &str = "/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token";

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

/// Return a valid /rtc/livekit/get_token C-S request body.
fn get_token_cs_request(user: &FakeUser, lk_url: &str) -> Value {
    json!({
        "url": lk_url,
        "room_id": "!room:example.com",
        "slot_id": "m.call#",
        "member": {
            "id": "member-1",
            "claimed_user_id": user.user_id,
            "claimed_device_id": "DEVICE",
        },
    })
}

/// The GetTokenSsRequest body expected to be relayed via the federation
/// proxy for a get_token_cs_request from `user` targeting `lk_url`.
fn expected_relayed_body(user: &FakeUser, lk_url: &str) -> Value {
    json!({
        "user_id": user.user_id,
        "url": lk_url,
        "room_id": "!room:example.com",
        "slot_id": "m.call#",
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
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
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
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
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
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
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

/// A malformed MXID header is rejected.
#[tokio::test]
async fn malformed_header() {
    let hs = FakeHomeserver::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
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
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
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
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
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
    hs.set_msc4502_support(Msc4502Support::Unstable);
    let user = hs.new_user("alice");
    hs.set_not_joined("!room:example.com", &user.user_id);

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
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
    expect_is_joined_request(&hs, "!room:example.com", &user.user_id, AS_TOKEN);
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
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
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
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
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
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
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

/// Without a `server_name` the target server is assumed to be the local one.
/// The user gets a token and triggers room creation. This test uses the unstable
/// /is_joined endpoint.
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
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
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

    expect_is_joined_request(&hs, "!room:example.com", &user.user_id, AS_TOKEN);

    let rooms = sfu.create_room_requests();
    assert_eq!(rooms.len(), 1, "expected exactly one room creation");
    assert_eq!(
        claims["video"]["room"].as_str(),
        Some(rooms[0].name.as_str())
    );
}

/// A full-access, joined user gets a token and triggers room creation,
/// using the stable is_joined endpoint.
/// Without a `server_name` the target server is assumed to be the local one.
/// The user gets a token and triggers room creation. This test uses the stable
/// /is_joined endpoint.
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
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
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

    expect_is_joined_request(&hs, "!room:example.com", &user.user_id, AS_TOKEN);

    let rooms = sfu.create_room_requests();
    assert_eq!(rooms.len(), 1, "expected exactly one room creation");
    assert_eq!(
        claims["video"]["room"].as_str(),
        Some(rooms[0].name.as_str())
    );
}

/// A `server_name` equal to this deployment's own LIVEKIT_HS_SERVER_NAME is
/// handled locally, just like an absent `server_name`.
#[tokio::test]
async fn server_name_matching_own_hs_server_name_is_local() {
    let hs = FakeHomeserver::new().await;
    hs.set_msc4502_support(Msc4502Support::Unstable);
    let user = hs.new_user("alice");
    let sfu = FakeSfu::new().await;

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        livekit_url: Some(sfu.url().to_owned()),
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let mut request = get_token_cs_request(&user, sfu.url());
    request["server_name"] = json!(hs.server_name());
    let (status, body) = post_get_token_cs(&svc, request.to_string(), Some(&user.user_id)).await;

    assert_eq!(status, 200, "body: {body}");
    expect_no_fed_proxy_requests(&hs);
    assert_eq!(
        sfu.create_room_requests().len(),
        1,
        "expected local room creation"
    );
}

// ── server_name / federation proxy routing ─────────────────────────────────────

/// A `server_name` naming a different homeserver than this deployment's own
/// LIVEKIT_HS_SERVER_NAME is relayed via the MSC4512 federation proxy.
#[tokio::test]
async fn foreign_server_name_is_relayed_via_federation_proxy() {
    let hs = FakeHomeserver::new().await;
    hs.set_msc4502_support(Msc4502Support::Unstable);
    let user = hs.new_user("alice");
    let destination_hs = FakeHomeserver::new().await;
    let sfu = FakeSfu::new().await;

    let mut cs_api_url_overrides = hs.cs_api_url_override();
    cs_api_url_overrides.extend(destination_hs.cs_api_url_override());

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides,
        livekit_url: Some(sfu.url().to_owned()),
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    // The `url` deliberately does not match this deployment's own SFU: the
    // URL check only applies to locally-minted tokens.
    let mut request = get_token_cs_request(&user, "wss://not-our-configured-sfu.example.com");
    request["server_name"] = json!(destination_hs.server_name());
    let (status, body) = post_get_token_cs(&svc, request.to_string(), Some(&user.user_id)).await;

    assert_eq!(status, 200, "body: {body}");
    let response: Value = serde_json::from_str(&body).expect("response is not JSON");
    assert_eq!(
        response["jwt"].as_str(),
        Some("remote-jwt"),
        "expected the JWT relayed from the federation proxy"
    );
    assert!(
        response.get("url").is_none(),
        "expected no `url` field in the response, got {response}"
    );

    // Membership is still checked against the requesting user's own
    // homeserver before relaying.
    expect_is_joined_request(&hs, "!room:example.com", &user.user_id, AS_TOKEN);

    expect_fed_proxy_request(
        &hs,
        destination_hs.server_name(),
        AS_TOKEN,
        &expected_relayed_body(&user, "wss://not-our-configured-sfu.example.com"),
    );

    assert!(
        sfu.create_room_requests().is_empty(),
        "expected no local room creation when relaying to another homeserver"
    );
}

/// A non-member is rejected before ever reaching the federation proxy.
#[tokio::test]
async fn non_member_is_rejected_even_with_foreign_server_name() {
    let hs = FakeHomeserver::new().await;
    hs.set_msc4502_support(Msc4502Support::Unstable);
    let user = hs.new_user("alice");
    hs.set_not_joined("!room:example.com", &user.user_id);

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides: hs.cs_api_url_override(),
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let mut request = get_token_cs_request(&user, DEFAULT_LK_URL);
    request["server_name"] = json!("other.example.org");
    let (status, body) = post_get_token_cs(&svc, request.to_string(), Some(&user.user_id)).await;

    expect_matrix_error(status, &body, 403, "M_FORBIDDEN");
    expect_no_fed_proxy_requests(&hs);
}

/// A destination-side error relayed through the federation proxy surfaces
/// as a 502 to the original caller.
#[tokio::test]
async fn federation_proxy_destination_error_surfaces_as_502() {
    let hs = FakeHomeserver::new().await;
    hs.set_msc4502_support(Msc4502Support::Unstable);
    let user = hs.new_user("alice");
    hs.set_fed_proxy_response(403, None);
    let destination_hs = FakeHomeserver::new().await;

    let mut cs_api_url_overrides = hs.cs_api_url_override();
    cs_api_url_overrides.extend(destination_hs.cs_api_url_override());

    let svc = Service::start(ServiceConfig {
        full_access_homeservers: vec![hs.server_name().to_owned()],
        cs_api_url_overrides,
        extra_env: app_service_env_with_hs_server_name(hs.server_name()),
        ..Default::default()
    })
    .await;

    let mut request = get_token_cs_request(&user, DEFAULT_LK_URL);
    request["server_name"] = json!(destination_hs.server_name());
    let (status, body) = post_get_token_cs(&svc, request.to_string(), Some(&user.user_id)).await;

    expect_matrix_error(status, &body, 502, "M_CONNECTION_FAILED");
    expect_fed_proxy_request(
        &hs,
        destination_hs.server_name(),
        AS_TOKEN,
        &expected_relayed_body(&user, DEFAULT_LK_URL),
    );
}

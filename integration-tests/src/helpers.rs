// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use jsonwebtoken::{Algorithm, DecodingKey, Validation};
use serde_json::Value;

use crate::fake_homeserver::FakeHomeserver;
use crate::harness::LIVEKIT_SECRET;

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

/// Assert that no /delayed_events requests were made.
#[track_caller]
pub fn expect_no_delayed_event_requests(hs: &FakeHomeserver) {
    let requests = hs.delayed_event_requests();
    assert!(
        requests.is_empty(),
        "expected no delayed event actions, got {:?}",
        requests
    );
}

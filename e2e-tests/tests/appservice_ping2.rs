// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use lk_jwt_service_e2e_tests::{APPSERVICE_ID, AUTH_SERVICE2_URL, SYNAPSE2_SERVER_NAME, Stack};

/// Triggers the app-service ping roundtrip against the second service and
/// homeserver to ensure they can reach each other.
#[tokio::test]
async fn appservice_ping_round_trip_succeeds_on_second_stack() {
    let _stack = Stack::start().await;

    let resp = reqwest::Client::new()
        .post(format!("{AUTH_SERVICE2_URL}/appservice-ping"))
        .json(&serde_json::json!({
            "server_name": SYNAPSE2_SERVER_NAME,
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

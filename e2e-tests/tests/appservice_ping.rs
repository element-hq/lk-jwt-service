// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use lk_jwt_service_e2e_tests::{
    APPSERVICE_ID, AUTH_SERVICE_A_URL, AUTH_SERVICE_B_URL, SYNAPSE_A_SERVER_NAME,
    SYNAPSE_B_SERVER_NAME, require_stack,
};

/// Triggers the app-service ping roundtrip to ensure the service and the homeserver
/// can reach each other.
#[tokio::test]
async fn appservice_ping_round_trip_succeeds() {
    require_stack();

    let resp = reqwest::Client::new()
        .post(format!("{AUTH_SERVICE_A_URL}/appservice-ping"))
        .json(&serde_json::json!({
            "server_name": SYNAPSE_A_SERVER_NAME,
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

    let resp = reqwest::Client::new()
        .post(format!("{AUTH_SERVICE_B_URL}/appservice-ping"))
        .json(&serde_json::json!({
            "server_name": SYNAPSE_B_SERVER_NAME,
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

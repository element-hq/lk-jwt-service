// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use serde_json::Value;

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

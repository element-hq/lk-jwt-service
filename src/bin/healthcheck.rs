// Copyright 2025 Element Creations Ltd.
// Copyright 2023 - 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//! Container health check: GETs /healthz on the local service and exits
//! non-zero on failure.

#[tokio::main]
async fn main() {
    let mut lk_jwt_bind = std::env::var("LIVEKIT_JWT_BIND").unwrap_or_default();
    if lk_jwt_bind.is_empty() {
        lk_jwt_bind = "8080".into();
    }

    let resp = match reqwest::get(format!("http://localhost:{lk_jwt_bind}/healthz")).await {
        Ok(resp) => resp,
        Err(err) => {
            println!("Connection error: {err}");
            std::process::exit(1);
        }
    };

    if resp.status().as_u16() != 200 {
        println!(
            "Healthcheck failed with status code {}",
            resp.status().as_u16()
        );
        std::process::exit(1);
    }
}

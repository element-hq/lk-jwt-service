// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//! Container health check: GETs /healthz on the local service and exits
//! non-zero on failure.

use lk_jwt_service::config::{bind_addresses, parse_config};

#[tokio::main]
async fn main() -> Result<(), String> {
    let config = parse_config()?;
    let addrs = bind_addresses(&config.lk_jwt_bind);

    for addr in addrs {
        let resp = match reqwest::get(format!("http://{addr}/healthz")).await {
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

    Ok(())
}

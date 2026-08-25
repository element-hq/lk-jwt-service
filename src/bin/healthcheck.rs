// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//! Container health check: GETs /healthz on the local service and exits
//! non-zero on failure.

use lk_jwt_service::config::{bind_addresses, parse_bind};

#[inline]
fn get_healthz_url(host_and_port: &str) -> String {
    format!("http://{host_and_port}/healthz")
}

#[tokio::main]
async fn main() -> Result<(), String> {
    let lk_jwt_bind = parse_bind()?;
    let addrs = bind_addresses(&lk_jwt_bind);

    for addr in addrs {
        let resp = match reqwest::get(get_healthz_url(&addr)).await {
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

#[cfg(test)]
mod tests {
    use super::*;
    use url::Url;

    fn validate_url(url_str: &str, expected_host: &str, expected_port: u16) {
        let url_res = Url::parse(url_str);
        assert!(url_res.is_ok());

        let url = url_res.unwrap();
        assert_eq!(url.host_str().unwrap_or("0.0.0.0"), expected_host);
        assert_eq!(url.port_or_known_default().unwrap_or_default(), expected_port);
    }

    fn test_healthz_url(lk_jwt_bind: &str, expected_host: &str, expected_port: u16) {
        let addrs = bind_addresses(lk_jwt_bind);
        let last = addrs.last().expect("bind_addresses returned empty Vec");
        let url_str = get_healthz_url(last);
        validate_url(&url_str, expected_host, expected_port);
    }

    #[test]
    fn bind_all_valid() {
        test_healthz_url(":8080", "0.0.0.0", 8080);
        test_healthz_url(":1234", "0.0.0.0", 1234);
        test_healthz_url(":443", "0.0.0.0", 443);
    }

    #[test]
    fn bind_specific_valid() {
        test_healthz_url("127.0.0.1:8080", "127.0.0.1", 8080);
        test_healthz_url("127.0.0.1:443", "127.0.0.1", 443);
    }
    
    #[test]
    fn bind_all_invalid() {
        let addrs = bind_addresses("8080");
        let last = addrs.last().expect("bind_addresses returned empty Vec");
        assert!(Url::parse(last).is_err());
    }
}
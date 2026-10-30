// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//! Container health check: GETs /healthz on the local service and exits
//! non-zero on failure.

use lk_jwt_service::config::{BindAddresses, bind_addresses, parse_bind};

#[inline]
fn get_healthz_url(host_and_port: &str) -> String {
    format!("http://{host_and_port}/healthz")
}

#[tokio::main]
async fn main() -> Result<(), String> {
    check(&parse_bind()?).await
}

async fn check(lk_jwt_bind: &str) -> Result<(), String> {
    let (client, urls) = match bind_addresses(lk_jwt_bind) {
        BindAddresses::Tcp(addrs) => (
            reqwest::Client::new(),
            addrs
                .iter()
                .map(|addr| get_healthz_url(addr))
                .collect::<Vec<_>>(),
        ),
        BindAddresses::Unix(path) => (
            reqwest::Client::builder()
                .unix_socket(path)
                .build()
                .map_err(|err| err.to_string())?,
            vec![get_healthz_url("localhost")],
        ),
    };

    for url in &urls {
        if let Ok(resp) = client.get(url).send().await {
            if resp.status().as_u16() != 200 {
                return Err(format!(
                    "Healthcheck failed with status code {}",
                    resp.status().as_u16()
                ));
            }

            return Ok(());
        }
    }

    Err(format!(
        "No configured address ({lk_jwt_bind}) could be accessed"
    ))
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
        assert_eq!(
            url.port_or_known_default().unwrap_or_default(),
            expected_port
        );
    }

    fn tcp_addresses(lk_jwt_bind: &str) -> Vec<String> {
        match bind_addresses(lk_jwt_bind) {
            BindAddresses::Tcp(addrs) => addrs,
            BindAddresses::Unix(path) => panic!("unexpected unix socket {}", path.display()),
        }
    }

    fn test_healthz_url(lk_jwt_bind: &str, expected_host: &str, expected_port: u16) {
        let addrs = tcp_addresses(lk_jwt_bind);
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
        let addrs = tcp_addresses("8080");
        let last = addrs.last().expect("bind_addresses returned empty Vec");
        assert!(Url::parse(last).is_err());
    }

    #[tokio::test]
    async fn checks_unix_socket() {
        let dir = std::env::temp_dir().join(format!("lk-jwt-healthcheck-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("lk.sock");
        let listener = tokio::net::UnixListener::bind(&path).unwrap();
        let router = axum::Router::new().route("/healthz", axum::routing::get(|| async { "" }));
        tokio::spawn(async move { axum::serve(listener, router).await });

        let healthy = check(path.to_str().unwrap()).await;
        std::fs::remove_dir_all(&dir).unwrap();
        assert_eq!(healthy, Ok(()));
    }

    #[tokio::test]
    async fn fails_on_missing_unix_socket() {
        assert!(check("/nonexistent/lk.sock").await.is_err());
    }
}

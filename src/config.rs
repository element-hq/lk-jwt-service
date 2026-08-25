// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//! Environment-driven configuration parsing.

use std::collections::HashMap;
use std::time::Duration;

use rust_yaml::{Value, Yaml};
use tracing::{info, warn};

use crate::helper::CsApiUrl;

#[derive(Debug, Clone, PartialEq)]
pub struct Config {
    pub key: String,
    pub secret: String,
    pub lk_url: String,
    pub skip_verify_tls: bool,
    pub full_access_homeservers: Vec<String>,
    pub lk_jwt_bind: String,
    /// The period at which the job worker re-checks whether connected
    /// participants are still present on the SFU. Zero (the default) disables
    /// the sanity check entirely.
    /// Configure via LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS (unit: seconds).
    pub sanity_check_interval: Duration,
    /// Map of URLs for the Client-Server API keyed by server name. These will
    /// be preferred over .well-known resolution for the contained server names.
    pub cs_api_url_overrides: HashMap<String, CsApiUrl>,
    /// Connection URL for the Redis store.
    pub redis_url: String,
    /// Tokens used for authenticating requests to and from the homeserver.
    pub app_service_config: AppServiceConfig,
}

/// Configuration options used for running as an application service.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct AppServiceConfig {
    /// A secret token that will be used to authenticate requests to the homeserver.
    pub as_token: String,
    /// A secret token that the homeserver will use to authenticate requests to this service.
    pub hs_token: String,
    /// The associated homeserver's server_name.
    pub hs_server_name: String,
}

impl AppServiceConfig {
    /// Whether the config is fully empty.
    pub fn is_empty(&self) -> bool {
        self.as_token.is_empty() && self.hs_token.is_empty() && self.hs_server_name.is_empty()
    }

    /// Whether the config has been set up or is (partially) empty.
    pub fn is_set_up(&self) -> bool {
        !self.as_token.is_empty() && !self.hs_token.is_empty() && !self.hs_server_name.is_empty()
    }
}

fn env_var(name: &str) -> String {
    std::env::var(name).unwrap_or_default()
}

pub fn read_key_secret() -> Result<(String, String), String> {
    let mut key = env_var("LIVEKIT_KEY");
    let mut secret = env_var("LIVEKIT_SECRET");
    let key_path = env_var("LIVEKIT_KEY_FROM_FILE");
    let secret_path = env_var("LIVEKIT_SECRET_FROM_FILE");
    let key_secret_path = env_var("LIVEKIT_KEY_FILE");

    if !key_secret_path.is_empty() {
        let key_secret = std::fs::read_to_string(&key_secret_path).map_err(|e| e.to_string())?;
        let parts: Vec<&str> = key_secret.split(':').collect();
        if parts.len() != 2 {
            return Err("invalid key secret file format!".into());
        }
        info!(
            key_secret_path,
            "Using LiveKit API key and secret from LIVEKIT_KEY_FILE"
        );
        key = parts[0].to_owned();
        secret = parts[1].to_owned();
    } else {
        if !key_path.is_empty() {
            key = std::fs::read_to_string(&key_path).map_err(|e| e.to_string())?;
            info!(key_path, "Using LiveKit API key from LIVEKIT_KEY_FROM_FILE");
        } else {
            info!("Using LiveKit API key from LIVEKIT_KEY");
        }
        if !secret_path.is_empty() {
            secret = std::fs::read_to_string(&secret_path).map_err(|e| e.to_string())?;
            info!(
                secret_path,
                "Using LiveKit API secret from LIVEKIT_SECRET_FROM_FILE"
            );
        } else {
            info!("Using LiveKit API secret from LIVEKIT_SECRET");
        }
    }

    let trim_chars: &[char] = &[' ', '\r', '\n'];
    Ok((
        key.trim_matches(trim_chars).to_owned(),
        secret.trim_matches(trim_chars).to_owned(),
    ))
}

/// Read the application service configuration from environment variables.
pub fn read_app_service_config() -> Result<AppServiceConfig, String> {
    let hs_server_name = env_var("LIVEKIT_HS_SERVER_NAME");
    let path = env_var("LIVEKIT_AS_REGISTRATION_FILE");

    let config = if !path.is_empty() {
        let content = std::fs::read_to_string(&path).map_err(|e| e.to_string())?;
        let yaml = Yaml::new();
        let parsed = yaml.load_str(&content).map_err(|e| e.to_string())?;
        if parsed.as_mapping().is_none() {
            return Err(
                "Could not parse app service registration file: invalid root element".into(),
            );
        }
        let as_token = read_app_service_registration_field(&parsed, "as_token")?;
        let hs_token = read_app_service_registration_field(&parsed, "hs_token")?;
        info!(
            path,
            "Using application service configuration from LIVEKIT_AS_REGISTRATION_FILE"
        );
        AppServiceConfig {
            as_token,
            hs_token,
            hs_server_name,
        }
    } else {
        let as_token = env_var("LIVEKIT_AS_TOKEN").trim().to_owned();
        let hs_token = env_var("LIVEKIT_HS_TOKEN").trim().to_owned();
        info!("Using application service configuration from LIVEKIT_AS_TOKEN and LIVEKIT_HS_TOKEN");
        AppServiceConfig {
            as_token,
            hs_token,
            hs_server_name,
        }
    };

    if !config.is_empty() && !config.is_set_up() {
        return Err(
            "The app service token(s) (LIVEKIT_AS_TOKEN/LIVEKIT_HS_TOKEN or \
             LIVEKIT_AS_REGISTRATION_FILE) and LIVEKIT_HS_SERVER_NAME must either all be set or \
             all be left empty"
                .into(),
        );
    }

    Ok(config)
}

/// Reads a required, trimmed string field from a parsed app service
/// registration file.
fn read_app_service_registration_field(parsed: &Value, field: &str) -> Result<String, String> {
    Ok(parsed
        .get_str(field)
        .ok_or_else(|| format!("Could not parse app service registration file: no {field} property"))?
        .as_str()
        .ok_or_else(|| {
            format!("Could not parse app service registration file: invalid value for {field} property")
        })?
        .trim()
        .to_owned())
}

pub fn read_cs_api_url_overrides(raw: &str) -> Result<HashMap<String, CsApiUrl>, String> {
    let mut m = HashMap::new();
    if !raw.is_empty() {
        for entry in raw.split(',') {
            let Some((server, url)) = entry.split_once('=') else {
                return Err(format!("invalid entry {entry:?}, expected server_name=url"));
            };
            let server = server.trim();
            let url = url.trim();
            if server.is_empty() || url.is_empty() {
                return Err(format!("invalid entry {entry:?}, expected server_name=url"));
            }
            m.insert(server.to_owned(), CsApiUrl(url.to_owned()));
        }
    }
    Ok(m)
}

pub fn parse_config() -> Result<Config, String> {
    let skip_verify_tls =
        env_var("LIVEKIT_INSECURE_SKIP_VERIFY_TLS") == "YES_I_KNOW_WHAT_I_AM_DOING";
    if skip_verify_tls {
        warn!("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!");
        warn!("!!! WARNING !!!  LIVEKIT_INSECURE_SKIP_VERIFY_TLS        !!! WARNING !!!");
        warn!("!!! WARNING !!!  Allow to skip invalid TLS certificates  !!! WARNING !!!");
        warn!("!!! WARNING !!!  Use only for testing or debugging       !!! WARNING !!!");
        warn!("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!");
    }

    let (key, secret) = read_key_secret()?;
    let lk_url = env_var("LIVEKIT_URL");

    if key.is_empty() || secret.is_empty() || lk_url.is_empty() {
        return Err("LIVEKIT_KEY* / LIVEKIT_SECRET* and LIVEKIT_URL must be set".into());
    }

    let full_access_homeservers = env_var("LIVEKIT_FULL_ACCESS_HOMESERVERS");
    if full_access_homeservers.is_empty() {
        return Err("LIVEKIT_FULL_ACCESS_HOMESERVERS environment variable must be set to the homeserver(s) you intend to serve — see README for guidance".into());
    }

    let mut lk_jwt_bind = env_var("LIVEKIT_JWT_BIND");
    let mut lk_jwt_port = env_var("LIVEKIT_JWT_PORT");
    if lk_jwt_bind.is_empty() {
        if lk_jwt_port.is_empty() {
            lk_jwt_port = "8080".into();
        } else {
            warn!("!!! LIVEKIT_JWT_PORT is deprecated, use LIVEKIT_JWT_BIND !!!");
        }
        lk_jwt_bind = format!(":{lk_jwt_port}");
    } else if !lk_jwt_port.is_empty() {
        return Err("LIVEKIT_JWT_BIND and LIVEKIT_JWT_PORT must not be set together".into());
    }

    let mut sanity_check_interval = Duration::ZERO;
    let sanity_check_raw = env_var("LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS");
    if !sanity_check_raw.is_empty() {
        match sanity_check_raw.parse::<i64>() {
            Ok(secs) if secs >= 0 => {
                sanity_check_interval = Duration::from_secs(secs as u64);
            }
            _ => {
                return Err(format!(
                    "LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS must be a non-negative integer, got {sanity_check_raw:?}"
                ));
            }
        }
    }

    let cs_api_url_overrides = read_cs_api_url_overrides(&env_var("LIVEKIT_CS_API_URL_OVERRIDES"))
        .map_err(|e| format!("failed parsing LIVEKIT_CS_API_URL_OVERRIDES: {e}"))?;

    let app_service_config = read_app_service_config()?;

    Ok(Config {
        key,
        secret,
        lk_url,
        skip_verify_tls,
        full_access_homeservers: full_access_homeservers
            .replace(',', " ")
            .split_whitespace()
            .map(str::to_owned)
            .collect(),
        lk_jwt_bind,
        sanity_check_interval,
        cs_api_url_overrides,
        redis_url: env_var("LIVEKIT_REDIS_URL"),
        app_service_config,
    })
}

/// Expands a bind address into the socket addresses to try, in order. A bare
/// ":port" means "all interfaces": the IPv6 wildcard is tried first (dual
/// stack on most systems) with the IPv4 wildcard as fallback.
pub fn bind_addresses(lk_jwt_bind: &str) -> Vec<String> {
    match lk_jwt_bind.strip_prefix(':') {
        Some(port) => vec![format!("[::]:{port}"), format!("0.0.0.0:{port}")],
        None => vec![lk_jwt_bind.to_owned()],
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_read_key_secret() {
        struct Case {
            name: &'static str,
            env: Vec<(&'static str, &'static str)>,
            expected_key: &'static str,
            expected_secret: &'static str,
        }
        let cases = [
            Case {
                name: "Read from env",
                env: vec![
                    ("LIVEKIT_KEY", "from_env_pheethiewixohp9eecheeGhuayeeph4l"),
                    (
                        "LIVEKIT_SECRET",
                        "from_env_ahb8eiwae0viey7gee4ieNgahgeeQuie",
                    ),
                ],
                expected_key: "from_env_pheethiewixohp9eecheeGhuayeeph4l",
                expected_secret: "from_env_ahb8eiwae0viey7gee4ieNgahgeeQuie",
            },
            Case {
                name: "Read from livekit keysecret",
                env: vec![("LIVEKIT_KEY_FILE", "./tests/keysecret.yaml")],
                expected_key: "keysecret_iethuB2LeLiNuishiaKeephei9jaatio",
                expected_secret: "keysecret_xefaingo4oos6ohla9phiMieBu3ohJi2",
            },
            Case {
                name: "Read from file",
                env: vec![
                    ("LIVEKIT_KEY_FROM_FILE", "./tests/key"),
                    ("LIVEKIT_SECRET_FROM_FILE", "./tests/secret"),
                ],
                expected_key: "from_file_oquusheiheiw4Iegah8te3Vienguus5a",
                expected_secret: "from_file_vohmahH3eeyieghohSh3kee8feuPhaim",
            },
            Case {
                name: "Read from file key only",
                env: vec![
                    ("LIVEKIT_KEY_FROM_FILE", "./tests/key"),
                    (
                        "LIVEKIT_SECRET",
                        "from_env_ahb8eiwae0viey7gee4ieNgahgeeQuie",
                    ),
                ],
                expected_key: "from_file_oquusheiheiw4Iegah8te3Vienguus5a",
                expected_secret: "from_env_ahb8eiwae0viey7gee4ieNgahgeeQuie",
            },
            Case {
                name: "Read from file secret only",
                env: vec![
                    ("LIVEKIT_SECRET_FROM_FILE", "./tests/secret"),
                    ("LIVEKIT_KEY", "from_env_qui8aiTopiekiechah9oocbeimeew2O"),
                ],
                expected_key: "from_env_qui8aiTopiekiechah9oocbeimeew2O",
                expected_secret: "from_file_vohmahH3eeyieghohSh3kee8feuPhaim",
            },
            Case {
                name: "Empty if secret no env",
                env: vec![],
                expected_key: "",
                expected_secret: "",
            },
        ];

        for tc in cases {
            let vars: Vec<(&str, Option<&str>)> =
                tc.env.iter().map(|(k, v)| (*k, Some(*v))).collect();
            temp_env::with_vars(vars, || {
                let (key, secret) = read_key_secret().expect("read_key_secret failed");
                assert_eq!(
                    (key.as_str(), secret.as_str()),
                    (tc.expected_key, tc.expected_secret),
                    "{}: expected key/secret to be {:?}/{:?} but got {:?}/{:?}",
                    tc.name,
                    tc.expected_key,
                    tc.expected_secret,
                    key,
                    secret,
                );
            });
        }
    }

    #[test]
    fn test_read_app_service_config() {
        struct Case {
            name: &'static str,
            registration_file_content: Option<&'static str>,
            extra_env: Vec<(&'static str, &'static str)>,
            expected: Option<AppServiceConfig>,
        }
        let cases = [
            Case {
                name: "Empty if no env",
                registration_file_content: None,
                extra_env: vec![],
                expected: Some(AppServiceConfig::default()),
            },
            Case {
                name: "Read from env",
                registration_file_content: None,
                extra_env: vec![
                    ("LIVEKIT_AS_TOKEN", "as_token_env_pheethiewixohp9eecheeGh"),
                    ("LIVEKIT_HS_TOKEN", "hs_token_env_ahb8eiwae0viey7gee4ieNg"),
                    ("LIVEKIT_HS_SERVER_NAME", "example.com")
                ],
                expected: Some(AppServiceConfig {
                    as_token: "as_token_env_pheethiewixohp9eecheeGh".into(),
                    hs_token: "hs_token_env_ahb8eiwae0viey7gee4ieNg".into(),
                    hs_server_name: "example.com".to_owned(),
                }),
            },
            Case {
                name: "Read from registration file",
                registration_file_content: Some(
                    "as_token: as_token_yaml_iethuB2LeLiNuishiaKe\nhs_token: hs_token_yaml_xefaingo4oos6ohla9ph\n",
                ),
                extra_env: vec![("LIVEKIT_HS_SERVER_NAME", "example.com")],
                expected: Some(AppServiceConfig {
                    as_token: "as_token_yaml_iethuB2LeLiNuishiaKe".into(),
                    hs_token: "hs_token_yaml_xefaingo4oos6ohla9ph".into(),
                    hs_server_name: "example.com".to_owned(),
                }),
            },
            Case {
                name: "Registration file takes precedence over env vars",
                registration_file_content: Some(
                    "as_token: as_token_yaml_iethuB2LeLiNuishiaKe\nhs_token: hs_token_yaml_xefaingo4oos6ohla9ph\n",
                ),
                extra_env: vec![
                    ("LIVEKIT_AS_TOKEN", "as_token_env_ignored"),
                    ("LIVEKIT_HS_TOKEN", "hs_token_env_ignored"),
                    ("LIVEKIT_HS_SERVER_NAME", "example.com"),
                ],
                expected: Some(AppServiceConfig {
                    as_token: "as_token_yaml_iethuB2LeLiNuishiaKe".into(),
                    hs_token: "hs_token_yaml_xefaingo4oos6ohla9ph".into(),
                    hs_server_name: "example.com".to_owned(),
                }),
            },
            Case {
                name: "Registration file tokens without hs_server_name is rejected",
                registration_file_content: Some(
                    "as_token: as_token_yaml_iethuB2LeLiNuishiaKe\nhs_token: hs_token_yaml_xefaingo4oos6ohla9ph\n",
                ),
                extra_env: vec![],
                expected: None,
            },
            Case {
                name: "Only LIVEKIT_AS_TOKEN set is rejected",
                registration_file_content: None,
                extra_env: vec![("LIVEKIT_AS_TOKEN", "as_token_env_pheethiewixohp9eecheeGh")],
                expected: None,
            },
            Case {
                name: "Only LIVEKIT_HS_TOKEN set is rejected",
                registration_file_content: None,
                extra_env: vec![("LIVEKIT_HS_TOKEN", "hs_token_env_ahb8eiwae0viey7gee4ieNg")],
                expected: None,
            },
            Case {
                name: "Only LIVEKIT_HS_SERVER_NAME set is rejected",
                registration_file_content: None,
                extra_env: vec![("LIVEKIT_HS_SERVER_NAME", "example.com")],
                expected: None,
            },
            Case {
                name: "AS/HS tokens without LIVEKIT_HS_SERVER_NAME is rejected",
                registration_file_content: None,
                extra_env: vec![
                    ("LIVEKIT_AS_TOKEN", "as_token_env_pheethiewixohp9eecheeGh"),
                    ("LIVEKIT_HS_TOKEN", "hs_token_env_ahb8eiwae0viey7gee4ieNg"),
                ],
                expected: None,
            },
            Case {
                name: "Registration file does not exist",
                registration_file_content: None,
                extra_env: vec![(
                    "LIVEKIT_AS_REGISTRATION_FILE",
                    "./tests/does_not_exist_as_app-service.yaml",
                )],
                expected: None,
            },
            Case {
                name: "Registration file invalid yaml",
                registration_file_content: Some("as_token: [unterminated\n"),
                extra_env: vec![],
                expected: None,
            },
            Case {
                name: "Registration file root not a mapping",
                registration_file_content: Some("- as_token\n- hs_token\n"),
                extra_env: vec![],
                expected: None,
            },
            Case {
                name: "Registration file missing as_token",
                registration_file_content: Some("hs_token: hs_token_value\n"),
                extra_env: vec![],
                expected: None,
            },
            Case {
                name: "Registration file missing hs_token",
                registration_file_content: Some("as_token: as_token_value\n"),
                extra_env: vec![],
                expected: None,
            },
            Case {
                name: "Registration file as_token wrong type",
                registration_file_content: Some(
                    "as_token: [1, 2, 3]\nhs_token: hs_token_value\n",
                ),
                extra_env: vec![],
                expected: None,
            },
            Case {
                name: "Registration file hs_token wrong type",
                registration_file_content: Some(
                    "as_token: as_token_value\nhs_token: [1, 2, 3]\n",
                ),
                extra_env: vec![],
                expected: None,
            },
        ];

        for (i, tc) in cases.iter().enumerate() {
            let temp_path = tc.registration_file_content.map(|content| {
                let path = std::env::temp_dir()
                    .join(format!("lk_jwt_service_test_as_registration_{i}.yaml"));
                std::fs::write(&path, content).expect("failed to write temp registration file");
                path
            });
            let temp_path_str = temp_path.as_ref().map(|p| p.to_str().unwrap().to_owned());

            let mut vars: Vec<(&str, Option<&str>)> =
                tc.extra_env.iter().map(|(k, v)| (*k, Some(*v))).collect();
            if let Some(path_str) = &temp_path_str {
                vars.push(("LIVEKIT_AS_REGISTRATION_FILE", Some(path_str.as_str())));
            }

            temp_env::with_vars(vars, || {
                let got = read_app_service_config();
                if let Some(expected) = &tc.expected {
                    assert_eq!(
                        got.as_ref(),
                        Ok(expected),
                        "{}: as_tokens mismatch",
                        tc.name
                    );
                } else {
                    assert!(got.is_err());
                }
            });

            if let Some(path) = temp_path {
                let _ = std::fs::remove_file(path);
            }
        }
    }

    #[test]
    fn test_read_cs_api_url_overrides() {
        struct Case {
            name: &'static str,
            env: &'static str,
            expected_map: Option<Vec<(&'static str, &'static str)>>,
            expected_err: bool,
        }
        let cases = [
            Case { name: "Empty", env: "", expected_map: Some(vec![]), expected_err: false },
            Case {
                name: "DNS name",
                env: "example.com=https://matrix-client.example.com",
                expected_map: Some(vec![("example.com", "https://matrix-client.example.com")]),
                expected_err: false,
            },
            Case {
                name: "DNS name with port",
                env: "example.com=https://matrix-client.example.com:1234",
                expected_map: Some(vec![(
                    "example.com",
                    "https://matrix-client.example.com:1234",
                )]),
                expected_err: false,
            },
            Case {
                name: "IPv4",
                env: "192.168.1.100=https://matrix-client.example.com",
                expected_map: Some(vec![("192.168.1.100", "https://matrix-client.example.com")]),
                expected_err: false,
            },
            Case {
                name: "IPv4 with port",
                env: "192.168.1.100:1234=https://matrix-client.example.com",
                expected_map: Some(vec![(
                    "192.168.1.100:1234",
                    "https://matrix-client.example.com",
                )]),
                expected_err: false,
            },
            Case {
                name: "IPv6",
                env: "[2001:0db8:85a3:0000:0000:8a2e:0370:7334]=https://matrix-client.example.com",
                expected_map: Some(vec![(
                    "[2001:0db8:85a3:0000:0000:8a2e:0370:7334]",
                    "https://matrix-client.example.com",
                )]),
                expected_err: false,
            },
            Case {
                name: "IPv6 with port",
                env: "[2001:0db8:85a3:0000:0000:8a2e:0370:7334]:1234=https://matrix-client.example.com",
                expected_map: Some(vec![(
                    "[2001:0db8:85a3:0000:0000:8a2e:0370:7334]:1234",
                    "https://matrix-client.example.com",
                )]),
                expected_err: false,
            },
            Case {
                name: "Invalid value at the start",
                env: "example.com",
                expected_map: None,
                expected_err: true,
            },
            Case {
                name: "Invalid value at the end",
                env: "example.com=https://matrix-client.example.com,example.com",
                expected_map: None,
                expected_err: true,
            },
            Case {
                name: "Empty key",
                env: "=https://matrix-client.example.com",
                expected_map: None,
                expected_err: true,
            },
            Case {
                name: "Empty value",
                env: "example.com=",
                expected_map: None,
                expected_err: true,
            },
            Case {
                name: "Two DNS names",
                env: "example.com=https://matrix-client.example.com,example.org=https://matrix-client.example.org",
                expected_map: Some(vec![
                    ("example.com", "https://matrix-client.example.com"),
                    ("example.org", "https://matrix-client.example.org"),
                ]),
                expected_err: false,
            },
            Case {
                name: "Two DNS names with whitespace",
                env: " example.com = https://matrix-client.example.com , example.org = https://matrix-client.example.org ",
                expected_map: Some(vec![
                    ("example.com", "https://matrix-client.example.com"),
                    ("example.org", "https://matrix-client.example.org"),
                ]),
                expected_err: false,
            },
        ];

        for tc in cases {
            let actual = read_cs_api_url_overrides(tc.env);
            match (&tc.expected_map, tc.expected_err, &actual) {
                (Some(expected), false, Ok(actual_map)) => {
                    let expected_map: HashMap<String, CsApiUrl> = expected
                        .iter()
                        .map(|(k, v)| ((*k).to_owned(), CsApiUrl((*v).to_owned())))
                        .collect();
                    assert_eq!(
                        &expected_map, actual_map,
                        "{}: expected map {:?}, got {:?}",
                        tc.name, expected_map, actual_map
                    );
                }
                (_, true, Err(_)) => {}
                _ => panic!(
                    "{}: expected map {:?}, err {:?}, got: {:?}",
                    tc.name, tc.expected_map, tc.expected_err, actual
                ),
            }
        }
    }

    #[test]
    fn test_parse_config() {
        struct Case {
            name: &'static str,
            env: Vec<(&'static str, &'static str)>,
            want_config: Option<Config>,
            want_err_msg: &'static str,
        }
        let cases = [
            Case {
                name: "Minimal valid config (explicit wildcard)",
                env: vec![
                    ("LIVEKIT_KEY", "test_key"),
                    ("LIVEKIT_SECRET", "test_secret"),
                    ("LIVEKIT_URL", "wss://test.livekit.cloud"),
                    ("LIVEKIT_FULL_ACCESS_HOMESERVERS", "*"),
                ],
                want_config: Some(Config {
                    key: "test_key".into(),
                    secret: "test_secret".into(),
                    lk_url: "wss://test.livekit.cloud".into(),
                    skip_verify_tls: false,
                    full_access_homeservers: vec!["*".into()],
                    lk_jwt_bind: ":8080".into(),
                    sanity_check_interval: Duration::ZERO,
                    cs_api_url_overrides: HashMap::new(),
                    redis_url: String::new(),
                    app_service_config: Default::default(),
                }),
                want_err_msg: "",
            },
            Case {
                name: "Full config with all options",
                env: vec![
                    ("LIVEKIT_KEY", "test_key"),
                    ("LIVEKIT_SECRET", "test_secret"),
                    ("LIVEKIT_URL", "wss://test.livekit.cloud"),
                    ("LIVEKIT_FULL_ACCESS_HOMESERVERS", "example.com, test.com"),
                    ("LIVEKIT_JWT_BIND", ":9090"),
                    ("LIVEKIT_INSECURE_SKIP_VERIFY_TLS", "YES_I_KNOW_WHAT_I_AM_DOING"),
                    ("LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS", "30"),
                    (
                        "LIVEKIT_CS_API_URL_OVERRIDES",
                        "matrix.com=https://matrix-client.matrix.com",
                    ),
                    ("LIVEKIT_REDIS_URL", "localhost:6379"),
                    ("LIVEKIT_AS_TOKEN", "as_token_env_pheethiewixohp9eecheeGh"),
                    ("LIVEKIT_HS_TOKEN", "hs_token_env_ahb8eiwae0viey7gee4ieNg"),
                    ("LIVEKIT_HS_SERVER_NAME", "example.com"),
                ],
                want_config: Some(Config {
                    key: "test_key".into(),
                    secret: "test_secret".into(),
                    lk_url: "wss://test.livekit.cloud".into(),
                    skip_verify_tls: true,
                    full_access_homeservers: vec!["example.com".into(), "test.com".into()],
                    lk_jwt_bind: ":9090".into(),
                    sanity_check_interval: Duration::from_secs(30),
                    cs_api_url_overrides: HashMap::from([(
                        "matrix.com".to_owned(),
                        CsApiUrl("https://matrix-client.matrix.com".into()),
                    )]),
                    redis_url: "localhost:6379".into(),
                    app_service_config: AppServiceConfig {
                        as_token: "as_token_env_pheethiewixohp9eecheeGh".to_owned(),
                        hs_token: "hs_token_env_ahb8eiwae0viey7gee4ieNg".to_owned(),
                        hs_server_name: "example.com".to_owned(),
                    },
                }),
                want_err_msg: "",
            },
            Case {
                name: "Legacy port configuration",
                env: vec![
                    ("LIVEKIT_KEY", "test_key"),
                    ("LIVEKIT_SECRET", "test_secret"),
                    ("LIVEKIT_URL", "wss://test.livekit.cloud"),
                    ("LIVEKIT_JWT_PORT", "9090"),
                    ("LIVEKIT_FULL_ACCESS_HOMESERVERS", "*"),
                ],
                want_config: Some(Config {
                    key: "test_key".into(),
                    secret: "test_secret".into(),
                    lk_url: "wss://test.livekit.cloud".into(),
                    skip_verify_tls: false,
                    full_access_homeservers: vec!["*".into()],
                    lk_jwt_bind: ":9090".into(),
                    sanity_check_interval: Duration::ZERO,
                    cs_api_url_overrides: HashMap::new(),
                    redis_url: String::new(),
                    app_service_config: Default::default(),
                }),
                want_err_msg: "",
            },
            Case {
                name: "Missing LIVEKIT_FULL_ACCESS_HOMESERVERS",
                env: vec![
                    ("LIVEKIT_KEY", "test_key"),
                    ("LIVEKIT_SECRET", "test_secret"),
                    ("LIVEKIT_URL", "wss://test.livekit.cloud"),
                ],
                want_config: None,
                want_err_msg: "LIVEKIT_FULL_ACCESS_HOMESERVERS environment variable must be set to the homeserver(s) you intend to serve — see README for guidance",
            },
            Case {
                name: "Missing required config",
                env: vec![("LIVEKIT_KEY", "test_key")],
                want_config: None,
                want_err_msg: "LIVEKIT_KEY* / LIVEKIT_SECRET* and LIVEKIT_URL must be set",
            },
            Case {
                name: "Conflicting bind configuration",
                env: vec![
                    ("LIVEKIT_KEY", "test_key"),
                    ("LIVEKIT_SECRET", "test_secret"),
                    ("LIVEKIT_URL", "wss://test.livekit.cloud"),
                    ("LIVEKIT_JWT_BIND", ":9090"),
                    ("LIVEKIT_JWT_PORT", "8080"),
                    ("LIVEKIT_FULL_ACCESS_HOMESERVERS", "*"),
                ],
                want_config: None,
                want_err_msg: "LIVEKIT_JWT_BIND and LIVEKIT_JWT_PORT must not be set together",
            },
            Case {
                name: "App service tokens without hs_server_name rejected",
                env: vec![
                    ("LIVEKIT_KEY", "test_key"),
                    ("LIVEKIT_SECRET", "test_secret"),
                    ("LIVEKIT_URL", "wss://test.livekit.cloud"),
                    ("LIVEKIT_FULL_ACCESS_HOMESERVERS", "*"),
                    ("LIVEKIT_AS_TOKEN", "as_token_env_pheethiewixohp9eecheeGh"),
                    ("LIVEKIT_HS_TOKEN", "hs_token_env_ahb8eiwae0viey7gee4ieNg"),
                ],
                want_config: None,
                want_err_msg: "The app service token(s) (LIVEKIT_AS_TOKEN/LIVEKIT_HS_TOKEN or \
                    LIVEKIT_AS_REGISTRATION_FILE) and LIVEKIT_HS_SERVER_NAME must either all be \
                    set or all be left empty",
            },
            Case {
                name: "Sanity check interval invalid",
                env: vec![
                    ("LIVEKIT_KEY", "test_key"),
                    ("LIVEKIT_SECRET", "test_secret"),
                    ("LIVEKIT_URL", "wss://test.livekit.cloud"),
                    ("LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS", "not-a-number"),
                    ("LIVEKIT_FULL_ACCESS_HOMESERVERS", "*"),
                ],
                want_config: None,
                want_err_msg: "LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS must be a non-negative integer, got \"not-a-number\"",
            },
            Case {
                name: "Sanity check interval zero disables",
                env: vec![
                    ("LIVEKIT_KEY", "test_key"),
                    ("LIVEKIT_SECRET", "test_secret"),
                    ("LIVEKIT_URL", "wss://test.livekit.cloud"),
                    ("LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS", "0"),
                    ("LIVEKIT_FULL_ACCESS_HOMESERVERS", "*"),
                ],
                want_config: Some(Config {
                    key: "test_key".into(),
                    secret: "test_secret".into(),
                    lk_url: "wss://test.livekit.cloud".into(),
                    skip_verify_tls: false,
                    full_access_homeservers: vec!["*".into()],
                    lk_jwt_bind: ":8080".into(),
                    sanity_check_interval: Duration::ZERO,
                    cs_api_url_overrides: HashMap::new(),
                    redis_url: String::new(),
                    app_service_config: Default::default(),
                }),
                want_err_msg: "",
            },
            Case {
                name: "Sanity check interval negative rejected",
                env: vec![
                    ("LIVEKIT_KEY", "test_key"),
                    ("LIVEKIT_SECRET", "test_secret"),
                    ("LIVEKIT_URL", "wss://test.livekit.cloud"),
                    ("LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS", "-1"),
                    ("LIVEKIT_FULL_ACCESS_HOMESERVERS", "*"),
                ],
                want_config: None,
                want_err_msg: "LIVEKIT_SANITY_CHECK_INTERVAL_SECONDS must be a non-negative integer, got \"-1\"",
            },
        ];

        for tc in cases {
            let vars: Vec<(&str, Option<&str>)> =
                tc.env.iter().map(|(k, v)| (*k, Some(*v))).collect();
            temp_env::with_vars(vars, || {
                let got = parse_config();

                if !tc.want_err_msg.is_empty() {
                    match got {
                        Ok(_) => panic!(
                            "{}: parse_config() ok, want err {:?}",
                            tc.name, tc.want_err_msg
                        ),
                        Err(e) => assert_eq!(
                            e, tc.want_err_msg,
                            "{}: parse_config() error mismatch",
                            tc.name
                        ),
                    }
                    return;
                }

                let got = got.unwrap_or_else(|e| {
                    panic!("{}: parse_config() unexpected error: {e}", tc.name)
                });
                // If any of the defaults change, the want_configs might need
                // updating.
                assert_eq!(Some(got), tc.want_config, "{}: config mismatch", tc.name);
            });
        }
    }

    #[test]
    fn test_bind_addresses() {
        assert_eq!(bind_addresses(":8080"), vec!["[::]:8080", "0.0.0.0:8080"]);
        assert_eq!(bind_addresses("127.0.0.1:9090"), vec!["127.0.0.1:9090"]);
        assert_eq!(bind_addresses("[::1]:9090"), vec!["[::1]:9090"]);
    }
}

// Copyright 2025 Element Creations Ltd.
// Copyright 2023 - 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//! Process entry point: sets up logging, parses the environment-driven
//! configuration, constructs the handler and serves it.

use std::sync::Arc;

use tracing::{info, warn};
use tracing_subscriber::EnvFilter;

use lk_jwt_service::config::{bind_addresses, parse_config};
use lk_jwt_service::handler::Handler;
use lk_jwt_service::helper::{LiveKitAuth, RealDeps};
use lk_jwt_service::store::{new_redis_store, Store};

#[tokio::main]
async fn main() {
    // Multiple rustls crypto backends are enabled in the dependency graph,
    // so the process-level provider must be picked explicitly.
    let _ = rustls::crypto::ring::default_provider().install_default();

    let log_level_string = std::env::var("LIVEKIT_LOG_LEVEL").unwrap_or_default();
    let (level, level_note) = match log_level_string.to_lowercase().as_str() {
        "debug" => ("debug", None),
        "info" => ("info", None),
        "warn" | "warning" => ("warn", None),
        "error" => ("error", None),
        "" => ("info", Some("log level defaulting to info")),
        _ => (
            "info",
            Some("Invalid log level in LIVEKIT_LOG_LEVEL, defaulting to info"),
        ),
    };
    // Library crates log at warn and above; LIVEKIT_LOG_LEVEL only controls
    // the service's own verbosity.
    tracing_subscriber::fmt()
        .with_env_filter(EnvFilter::new(format!("warn,lk_jwt_service={level}")))
        .with_writer(std::io::stderr)
        .with_ansi(std::io::IsTerminal::is_terminal(&std::io::stderr()))
        .init();
    if let Some(note) = level_note {
        if log_level_string.is_empty() {
            info!("{note}");
        } else {
            warn!(invalid_value = %log_level_string, "{note}");
        }
    }

    let config = match parse_config() {
        Ok(config) => config,
        Err(err) => {
            eprintln!("{err}");
            std::process::exit(1);
        }
    };

    let store: Option<Arc<dyn Store>> = if config.redis_url.is_empty() {
        warn!("LIVEKIT_REDIS_URL not set. Using in-memory store.");
        None
    } else {
        match new_redis_store(&config.redis_url).await {
            Ok(store) => Some(store),
            Err(err) => {
                eprintln!("Could not connect Redis store: {err}");
                std::process::exit(1);
            }
        }
    };

    let handler = Handler::new(
        LiveKitAuth {
            key: config.key.clone(),
            secret: config.secret.clone(),
            lk_url: config.lk_url.clone(),
        },
        config.skip_verify_tls,
        config.full_access_homeservers.clone(),
        config.sanity_check_interval,
        config.cs_api_url_overrides.clone(),
        store,
        Arc::new(RealDeps),
    );

    let sanity_check_interval_display = if config.sanity_check_interval.is_zero() {
        "disabled".to_owned()
    } else {
        format!("{:?}", config.sanity_check_interval)
    };
    info!(
        LIVEKIT_URL = %config.lk_url,
        LIVEKIT_JWT_BIND = %config.lk_jwt_bind,
        LIVEKIT_FULL_ACCESS_HOMESERVERS = ?config.full_access_homeservers,
        SkipVerifyTLS = config.skip_verify_tls,
        SanityCheckInterval = %sanity_check_interval_display,
        "Starting service"
    );

    let mut listener = None;
    for bind_addr in bind_addresses(&config.lk_jwt_bind) {
        match tokio::net::TcpListener::bind(&bind_addr).await {
            Ok(bound) => {
                listener = Some(bound);
                break;
            }
            Err(err) => warn!(bind_addr, err = %err, "Failed to bind"),
        }
    }
    let Some(listener) = listener else {
        eprintln!("Failed to bind {}", config.lk_jwt_bind);
        std::process::exit(1);
    };
    if let Err(err) = axum::serve(listener, handler.prepare_router()).await {
        eprintln!("{err}");
        std::process::exit(1);
    }
}

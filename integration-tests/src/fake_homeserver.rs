// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use std::collections::HashMap;
use std::net::TcpListener;
use std::sync::{Arc, Mutex};

use axum::Router;
use axum::extract::{Path, Query, State};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Json};
use axum::routing::{get, post};
use serde_json::json;

#[derive(Clone)]
pub struct FakeUser {
    pub user_id: String,
    pub openid_token: String,
}

#[derive(Clone, Debug)]
pub struct UserInfoRequest {
    pub access_token: String,
}

#[derive(Clone, Debug)]
pub struct DelayedEventRequest {
    pub delay_id: String,
    pub action: String,
}

#[derive(Default)]
struct HsState {
    /// The user IDs known to the homeserver, keyed by
    /// the associated OpenID token.
    tokens: HashMap<String, String>,

    /// The recorded /openid/userinfo requests.
    user_info_requests: Vec<UserInfoRequest>,
    
    /// The HTTP status to return on /delayed_events requests.
    /// None results in 200 OK.
    delayed_event_status: Option<u16>,
    
    /// The recorded /delayed_events requests.
    delayed_event_requests: Vec<DelayedEventRequest>,
}

pub struct FakeHomeserver {
    server_name: String,
    cs_api_url: String,
    state: Arc<Mutex<HsState>>,
}

impl FakeHomeserver {
    /// Start a new fake homeserver. The tasks live on the test's tokio runtime and die
    /// with it.
    pub async fn new() -> FakeHomeserver {
        let state = Arc::new(Mutex::new(HsState::default()));

        // Create a throwaway self-signed certificate.
        let cert = rcgen::generate_simple_self_signed(vec!["127.0.0.1".into()])
            .expect("failed to generate certificate");
        let tls_config = axum_server::tls_rustls::RustlsConfig::from_pem(
            cert.cert.pem().into_bytes(),
            cert.signing_key.serialize_pem().into_bytes(),
        )
        .await
        .expect("failed to build TLS config");

        // Register /openid/userinfo handler.
        let federation_listener =
            TcpListener::bind("127.0.0.1:0").expect("failed to bind federation listener");
        let server_name = federation_listener.local_addr().unwrap().to_string();
        let federation_app = Router::new()
            .route("/_matrix/federation/v1/openid/userinfo", get(handle_user_info))
            .with_state(Arc::clone(&state));
        tokio::spawn(
            axum_server::from_tcp_rustls(federation_listener, tls_config)
                .serve(federation_app.into_make_service()),
        );

        // Register /delayed_events handler.
        let cs_api_listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("failed to bind CS API listener");
        let cs_api_url = format!("http://{}", cs_api_listener.local_addr().unwrap());
        let cs_api_app = Router::new()
            .route("/_matrix/client/unstable/org.matrix.msc4140/delayed_events/{delay_id}/{action}", post(handle_delayed_event))
            .with_state(Arc::clone(&state));
        tokio::spawn(axum::serve(cs_api_listener, cs_api_app).into_future());

        FakeHomeserver {
            server_name,
            cs_api_url,
            state,
        }
    }

    pub fn server_name(&self) -> &str {
        &self.server_name
    }

    pub fn cs_api_url(&self) -> &str {
        &self.cs_api_url
    }

    /// A ready-made overrides map for routing this fake's server name to its
    /// client-server API listener.
    pub fn cs_api_url_override(&self) -> HashMap<String, String> {
        HashMap::from([(self.server_name.clone(), self.cs_api_url.clone())])
    }

    pub fn new_user(&self, localpart: &str) -> FakeUser {
        let user = FakeUser {
            user_id: format!("@{localpart}:{}", self.server_name),
            openid_token: format!("syt_{localpart}_integration"),
        };
        self.state
            .lock()
            .unwrap()
            .tokens
            .insert(user.openid_token.clone(), user.user_id.clone());
        user
    }

    /// The recorded /openid/userinfo requests.
    pub fn user_info_requests(&self) -> Vec<UserInfoRequest> {
        self.state.lock().unwrap().user_info_requests.clone()
    }

    /// Set the HTTP status to return on /delayed_events requests.
    /// None results in 200 OK.
    pub fn set_delayed_event_status(&self, status: u16) {
        self.state.lock().unwrap().delayed_event_status = Some(status);
    }

    /// The recorded /delayed_events requests.
    pub fn delayed_event_requests(&self) -> Vec<DelayedEventRequest> {
        self.state.lock().unwrap().delayed_event_requests.clone()
    }
}

/// Handler for /openid/userinfo requests.
async fn handle_user_info(
    State(state): State<Arc<Mutex<HsState>>>,
    Query(query): Query<HashMap<String, String>>,
) -> impl IntoResponse {
    // Extract the token from the request.
    let token = query.get("access_token").cloned().unwrap_or_default();

    // Record the request.
    let mut state = state.lock().unwrap();
    state.user_info_requests.push(UserInfoRequest {
        access_token: token.clone(),
    });

    // Check if the token is known and respond accordingly.
    match state.tokens.get(&token) {
        Some(user_id) => (StatusCode::OK, Json(json!({ "sub": user_id }))),
        None => (
            StatusCode::UNAUTHORIZED,
            Json(json!({
                "errcode": "M_UNKNOWN_TOKEN",
                "error": "Access token unknown or expired",
            })),
        ),
    }
}

/// Handler for /delayed_events requests.
async fn handle_delayed_event(
    State(state): State<Arc<Mutex<HsState>>>,
    Path((delay_id, action)): Path<(String, String)>,
) -> impl IntoResponse {
    let mut state = state.lock().unwrap();

    // Record the request.
    state
        .delayed_event_requests
        .push(DelayedEventRequest { delay_id, action });
    
    // Respond with the configured HTTP status.
    let status = state.delayed_event_status.unwrap_or(200);
    (
        StatusCode::from_u16(status).expect("invalid scripted status"),
        Json(json!({})),
    )
}

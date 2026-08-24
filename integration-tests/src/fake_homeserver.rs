// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use std::collections::{HashMap, HashSet};
use std::net::TcpListener;
use std::sync::{Arc, Mutex};

use axum::Router;
use axum::extract::{Path, Query, State};
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Json};
use axum::routing::{get, post};
use serde_json::json;

/// Which form of the MSC4502 `is_joined` endpoint (if any) the fake
/// homeserver advertises via `/versions`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Msc4502Support {
    Unstable,
    Stable,
    None,
}

#[derive(Clone, Debug)]
pub struct IsJoinedRequest {
    pub room_id: String,
    pub mxid: String,
    pub authorization: String,
}

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

    /// Which form of the is_joined endpoint /versions advertises.
    msc4502_support: Msc4502Support,

    /// (room_id, mxid) pairs considered NOT joined. Everything else is
    /// treated as joined.
    not_joined: HashSet<(String, String)>,

    /// The recorded /is_joined requests.
    is_joined_requests: Vec<IsJoinedRequest>,
}

impl Default for HsState {
    fn default() -> Self {
        HsState {
            tokens: HashMap::new(),
            user_info_requests: Vec::new(),
            delayed_event_status: None,
            delayed_event_requests: Vec::new(),
            msc4502_support: Msc4502Support::Unstable,
            not_joined: HashSet::new(),
            is_joined_requests: Vec::new(),
        }
    }
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
        federation_listener
            .set_nonblocking(true)
            .expect("failed to set federation listener non-blocking");
        let server_name = federation_listener.local_addr().unwrap().to_string();
        let federation_app = Router::new()
            .route(
                "/_matrix/federation/v1/openid/userinfo",
                get(handle_user_info),
            )
            .with_state(Arc::clone(&state));
        tokio::spawn(
            axum_server::from_tcp_rustls(federation_listener, tls_config)
                .expect("failed to build federation TLS server")
                .serve(federation_app.into_make_service()),
        );

        // Register /delayed_events handler.
        let cs_api_listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("failed to bind CS API listener");
        let cs_api_url = format!("http://{}", cs_api_listener.local_addr().unwrap());
        let cs_api_app = Router::new()
            .route(
                "/_matrix/client/unstable/org.matrix.msc4140/delayed_events/{delay_id}/{action}",
                post(handle_delayed_event),
            )
            .route("/_matrix/client/versions", get(handle_versions))
            .route(
                "/_matrix/client/unstable/io.element.msc4502/rooms/{room_id}/is_joined",
                get(handle_is_joined),
            )
            .route(
                "/_matrix/client/v3/rooms/{room_id}/is_joined",
                get(handle_is_joined),
            )
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

    /// Sets which form of the is_joined endpoint (MSC4502) /versions
    /// advertises. Defaults to `Unstable`.
    pub fn set_msc4502_support(&self, support: Msc4502Support) {
        self.state.lock().unwrap().msc4502_support = support;
    }

    /// Marks (room_id, mxid) as NOT a member of the room. Every other pair
    /// is treated as joined by default.
    pub fn set_not_joined(&self, room_id: &str, mxid: &str) {
        self.state
            .lock()
            .unwrap()
            .not_joined
            .insert((room_id.to_owned(), mxid.to_owned()));
    }

    /// The recorded /is_joined requests.
    pub fn is_joined_requests(&self) -> Vec<IsJoinedRequest> {
        self.state.lock().unwrap().is_joined_requests.clone()
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

/// Handler for /_matrix/client/versions requests.
async fn handle_versions(State(state): State<Arc<Mutex<HsState>>>) -> impl IntoResponse {
    let msc4502_support = state.lock().unwrap().msc4502_support;
    let unstable_features = match msc4502_support {
        Msc4502Support::Unstable => json!({ "io.element.msc4502": true }),
        Msc4502Support::Stable => json!({ "io.element.msc4502.stable": true }),
        Msc4502Support::None => json!({}),
    };
    Json(json!({
        "versions": ["v1.1"],
        "unstable_features": unstable_features,
    }))
}

/// Handler for the stable and unstable /is_joined requests (MSC4502).
async fn handle_is_joined(
    State(state): State<Arc<Mutex<HsState>>>,
    Path(room_id): Path<String>,
    Query(query): Query<HashMap<String, String>>,
    headers: HeaderMap,
) -> impl IntoResponse {
    let mxid = query.get("mxid").cloned().unwrap_or_default();
    let authorization = headers
        .get(axum::http::header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .unwrap_or_default()
        .to_owned();

    let mut state = state.lock().unwrap();
    state.is_joined_requests.push(IsJoinedRequest {
        room_id: room_id.clone(),
        mxid: mxid.clone(),
        authorization,
    });

    let joined = !state.not_joined.contains(&(room_id, mxid));
    Json(json!({ "joined": joined }))
}

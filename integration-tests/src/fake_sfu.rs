// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use std::sync::{Arc, Mutex};
use std::time::{SystemTime, UNIX_EPOCH};

use axum::Router;
use axum::body::Bytes;
use axum::extract::State;
use axum::http::{StatusCode, header};
use axum::response::{IntoResponse, Json};
use axum::routing::post;
use livekit_protocol as proto;
use prost::Message;
use serde_json::json;

#[derive(Clone, Debug)]
pub struct CreateRoomRequest {
    pub name: String,
    pub empty_timeout: u32,
    pub departure_timeout: u32,
    pub max_participants: u32,
}

#[derive(Default)]
struct SfuState {
    /// The recorded RoomService/CreateRoom requests.
    create_room_requests: Vec<CreateRoomRequest>,
}

pub struct FakeSfu {
    url: String,
    state: Arc<Mutex<SfuState>>,
}

impl FakeSfu {
    /// Start a new fake SFU serving the RoomService Twirp API (protobuf over HTTP).
    /// The task lives on the test's tokio runtime and dies with it.
    pub async fn new() -> FakeSfu {
        let state = Arc::new(Mutex::new(SfuState::default()));

        // Register the RoomService/CreateRoom handler.
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("failed to bind SFU listener");
        let url = format!("ws://{}", listener.local_addr().unwrap());
        let app = Router::new()
            .route(
                "/twirp/livekit.RoomService/CreateRoom",
                post(handle_create_room),
            )
            .with_state(Arc::clone(&state));
        tokio::spawn(axum::serve(listener, app).into_future());

        FakeSfu { url, state }
    }

    /// The WebSocket URL of the fake SFU.
    pub fn url(&self) -> &str {
        &self.url
    }

    /// The recorded RoomService/CreateRoom requests.
    pub fn create_room_requests(&self) -> Vec<CreateRoomRequest> {
        self.state.lock().unwrap().create_room_requests.clone()
    }
}

/// Handler for RoomService/CreateRoom requests.
async fn handle_create_room(
    State(state): State<Arc<Mutex<SfuState>>>,
    body: Bytes,
) -> impl IntoResponse {
    // Decode the protobuf request.
    let Ok(request) = proto::CreateRoomRequest::decode(body) else {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({ "code": "malformed", "msg": "invalid protobuf body" })),
        )
            .into_response();
    };

    // Record the request.
    state
        .lock()
        .unwrap()
        .create_room_requests
        .push(CreateRoomRequest {
            name: request.name.clone(),
            empty_timeout: request.empty_timeout,
            departure_timeout: request.departure_timeout,
            max_participants: request.max_participants,
        });

    // Respond with the created room.
    let room = proto::Room {
        sid: format!("RM_{}", request.name),
        name: request.name,
        creation_time: SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64,
        empty_timeout: request.empty_timeout,
        departure_timeout: request.departure_timeout,
        max_participants: request.max_participants,
        ..Default::default()
    };
    (
        [(header::CONTENT_TYPE, "application/protobuf")],
        room.encode_to_vec(),
    )
        .into_response()
}

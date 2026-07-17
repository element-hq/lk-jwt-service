// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use std::collections::HashSet;
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

#[derive(Clone, Debug)]
pub struct GetParticipantRequest {
    pub room: String,
    pub identity: String,
}

#[derive(Default)]
struct SfuState {
    /// The recorded RoomService/CreateRoom requests.
    create_room_requests: Vec<CreateRoomRequest>,
    /// The recorded RoomService/GetParticipant requests.
    get_participant_requests: Vec<GetParticipantRequest>,
    /// (room, identity) pairs currently considered present, per
    /// RoomService/GetParticipant.
    participants: HashSet<(String, String)>,
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

        // Register the RoomService/CreateRoom and GetParticipant handlers.
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("failed to bind SFU listener");
        let url = format!("ws://{}", listener.local_addr().unwrap());
        let app = Router::new()
            .route(
                "/twirp/livekit.RoomService/CreateRoom",
                post(handle_create_room),
            )
            .route(
                "/twirp/livekit.RoomService/GetParticipant",
                post(handle_get_participant),
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

    /// The recorded RoomService/GetParticipant requests.
    pub fn get_participant_requests(&self) -> Vec<GetParticipantRequest> {
        self.state.lock().unwrap().get_participant_requests.clone()
    }

    /// Mark (room, identity) as present, so GetParticipant succeeds for it.
    pub fn set_participant_present(&self, room: &str, identity: &str) {
        self.state
            .lock()
            .unwrap()
            .participants
            .insert((room.to_owned(), identity.to_owned()));
    }

    /// Mark (room, identity) as absent, so GetParticipant returns NotFound
    /// for it -- e.g. to simulate a missed disconnect webhook.
    pub fn set_participant_absent(&self, room: &str, identity: &str) {
        self.state
            .lock()
            .unwrap()
            .participants
            .remove(&(room.to_owned(), identity.to_owned()));
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

/// Handler for RoomService/GetParticipant requests.
async fn handle_get_participant(
    State(state): State<Arc<Mutex<SfuState>>>,
    body: Bytes,
) -> impl IntoResponse {
    let Ok(request) = proto::RoomParticipantIdentity::decode(body) else {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({ "code": "malformed", "msg": "invalid protobuf body" })),
        )
            .into_response();
    };

    let mut state = state.lock().unwrap();
    state.get_participant_requests.push(GetParticipantRequest {
        room: request.room.clone(),
        identity: request.identity.clone(),
    });

    if state
        .participants
        .contains(&(request.room.clone(), request.identity.clone()))
    {
        let info = proto::ParticipantInfo {
            identity: request.identity,
            ..Default::default()
        };
        (
            [(header::CONTENT_TYPE, "application/protobuf")],
            info.encode_to_vec(),
        )
            .into_response()
    } else {
        (
            StatusCode::NOT_FOUND,
            Json(json!({ "code": "not_found", "msg": "participant not found" })),
        )
            .into_response()
    }
}

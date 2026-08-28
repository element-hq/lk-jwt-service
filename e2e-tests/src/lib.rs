// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//! Support code for the end-to-end test suite: the addresses the Docker
//! Compose stack publishes to the host, and helpers for driving Synapse and
//! the LiveKit SFU through them.
//!
//! The stack itself is not started here. Every test shares one long-lived
//! stack, brought up and torn down by `cargo xtask e2e` — see
//! [`require_stack`] and the README.

use std::time::Duration;

/// This crate's package name, so `xtask` can name it on a `cargo test`
/// command line without hardcoding it a second time.
pub const PACKAGE_NAME: &str = env!("CARGO_PKG_NAME");

/// The environment variable `cargo xtask e2e` sets to tell the tests that
/// the stack is up and theirs to use. See [`require_stack`].
pub const STACK_RUNNING_ENV: &str = "LK_JWT_E2E_STACK_RUNNING";

/// The application service ID both service instances are registered under
/// (in docker/app-service-a.yaml and docker/app-service-b.yaml).
pub const APPSERVICE_ID: &str = "lk-jwt-service";

// ── Stack A ──────────────────────────────────────────────────────────────────

/// Stack A's Synapse client-server API, published to the host.
pub const SYNAPSE_A_CS_API_URL: &str = "http://127.0.0.1:18008";

/// The server name stack A's Synapse is configured with in
/// docker/homeserver-a.yaml.
pub const SYNAPSE_A_SERVER_NAME: &str = "synapse-a.e2e.test";

/// Stack A's service under test, published to the host.
pub const AUTH_SERVICE_A_URL: &str = "http://127.0.0.1:18080";

/// The LIVEKIT_A_URL stack A's jwt-service container is configured with. Only
/// resolvable from inside the Docker network. To actually reach the same
/// LiveKit instance from the host, use [`LIVEKIT_A_SFU_ADDR`] instead.
pub const LIVEKIT_A_URL: &str = "ws://livekit-a:7880";

/// The host-published address of the same LiveKit instance [`LIVEKIT_A_URL`]
/// points to from inside the Docker network.
pub const LIVEKIT_A_SFU_ADDR: &str = "127.0.0.1:17890";

// ── Stack B ──────────────────────────────────────────────────────────────────

/// Stack B's Synapse client-server API, published to the host.
pub const SYNAPSE_B_CS_API_URL: &str = "http://127.0.0.1:18009";

/// The server name stack B's Synapse is configured with in
/// docker/homeserver-b.yaml.
pub const SYNAPSE_B_SERVER_NAME: &str = "synapse-b.e2e.test";

/// Stack B's service under test, published to the host.
pub const AUTH_SERVICE_B_URL: &str = "http://127.0.0.1:18081";

/// The LIVEKIT_A_URL stack B's jwt-service container is configured with. Only
/// resolvable from inside the Docker network. To actually reach the same
/// LiveKit instance from the host, use [`LIVEKIT_B_SFU_ADDR`] instead.
pub const LIVEKIT_B_URL: &str = "ws://livekit-b:7880";

/// The host-published address of the same LiveKit instance [`LIVEKIT_B_URL`]
/// points to from inside the Docker network.
pub const LIVEKIT_B_SFU_ADDR: &str = "127.0.0.1:17891";

// ── The shared stack ─────────────────────────────────────────────────────────

/// Asserts that the Docker Compose stack is up, i.e. that the suite was
/// launched through `cargo xtask e2e`.
///
/// The stack is shared by every test and none of them starts it, so a test
/// run without it would otherwise fail with a wall of connection errors
/// rather than saying what's actually missing. Call this first in every
/// test.
pub fn require_stack() {
    assert!(
        std::env::var_os(STACK_RUNNING_ENV).is_some(),
        "the e2e stack isn't running: this suite doesn't start it itself. Run it as \
         `cargo xtask e2e` from the repository root, which brings the stack up once, \
         runs every test against it and tears it down again."
    );
}

// ── Matrix client helpers ────────────────────────────────────────────────────

/// A user provisioned on the Synapse instance.
pub struct MatrixUser {
    pub user_id: String,
    pub access_token: String,
}

/// Builds a Matrix ID localpart that's unique per call, so tests sharing
/// one long-lived homeserver never collide when registering users.
pub fn unique_localpart(prefix: &str) -> String {
    format!("{prefix}-{}", uuid::Uuid::new_v4())
}

/// Registers a new user against the homeserver behind `cs_api_url`.
///
/// `name` is only the readable half of the localpart: the homeserver
/// outlives every individual test, so [`unique_localpart`] makes the
/// registered localpart unique regardless of how often the same `name` is
/// used across the suite or across repeated runs.
pub async fn register_user(cs_api_url: &str, name: &str, password: &str) -> MatrixUser {
    let username = unique_localpart(name);
    let client = reqwest::Client::new();
    let register_url = format!("{cs_api_url}/_matrix/client/v3/register");

    // The first call carries no auth and is expected to be rejected with the
    // set of available UIA flows plus a session ID to complete one of them.
    let resp = client
        .post(&register_url)
        .json(&serde_json::json!({}))
        .send()
        .await
        .expect("registration (flows) request failed");
    assert_eq!(
        resp.status().as_u16(),
        401,
        "expected a UIA challenge from the flows-less registration request"
    );
    let flows: serde_json::Value = resp.json().await.expect("flows response was not JSON");
    let session = flows["session"]
        .as_str()
        .expect("flows response is missing `session`")
        .to_owned();

    // Complete the dummy stage.
    let resp = client
        .post(&register_url)
        .json(&serde_json::json!({
            "username": username,
            "password": password,
            "auth": {"type": "m.login.dummy", "session": session},
            "initial_device_display_name": "e2e-tests",
        }))
        .send()
        .await
        .expect("registration request failed");
    let status = resp.status();
    let body: serde_json::Value = resp
        .json()
        .await
        .expect("registration response was not JSON");
    assert!(status.is_success(), "registration failed: {status}: {body}");

    MatrixUser {
        user_id: body["user_id"]
            .as_str()
            .expect("registration response is missing `user_id`")
            .to_owned(),
        access_token: body["access_token"]
            .as_str()
            .expect("registration response is missing `access_token`")
            .to_owned(),
    }
}

/// Creates a room as the given user against the homeserver behind
/// `cs_api_url` and returns its room ID.
pub async fn create_and_join_room(cs_api_url: &str, user: &MatrixUser) -> String {
    let resp = reqwest::Client::new()
        .post(format!("{cs_api_url}/_matrix/client/v3/createRoom"))
        .bearer_auth(&user.access_token)
        .json(&serde_json::json!({"preset": "public_chat"}))
        .send()
        .await
        .expect("createRoom request failed");
    let status = resp.status();
    let body: serde_json::Value = resp.json().await.expect("createRoom response was not JSON");
    assert!(status.is_success(), "createRoom failed: {status}: {body}");
    body["room_id"]
        .as_str()
        .expect("createRoom response is missing `room_id`")
        .to_owned()
}

/// Joins an existing room as the given user against the homeserver behind
/// `cs_api_url`, resolving it via `via_server_name` — the server_name of a
/// homeserver already participating in the room.
pub async fn join_room_via(
    cs_api_url: &str,
    user: &MatrixUser,
    room_id: &str,
    via_server_name: &str,
) {
    let mut url = reqwest::Url::parse(cs_api_url).expect("invalid CS API URL");
    url.path_segments_mut()
        .expect("cs_api_url cannot be a base")
        .extend(["_matrix", "client", "v3", "join", room_id]);
    url.query_pairs_mut()
        .append_pair("server_name", via_server_name);

    let resp = reqwest::Client::new()
        .post(url)
        .bearer_auth(&user.access_token)
        .json(&serde_json::json!({}))
        .send()
        .await
        .expect("join request failed");
    let status = resp.status();
    let body: serde_json::Value = resp.json().await.expect("join response was not valid JSON");
    assert!(status.is_success(), "join failed: {status}: {body}");
}

// ── SFU verification ─────────────────────────────────────────────────────────

/// Connects to the LiveKit SFU at `sfu_addr`'s RTC signalling endpoint using
/// the given access token and confirms the SFU accepts it.
pub async fn verify_livekit_token_is_usable(sfu_addr: &str, access_token: &str) {
    LiveKitParticipant::connect(sfu_addr, access_token)
        .await
        .disconnect()
        .await;
}

/// A participant held open on the LiveKit SFU through its RTC signalling
/// endpoint.
///
/// Only the signalling connection is established — no media is negotiated —
/// which is enough for the SFU to consider the participant present.
///
/// The connection is kept alive by a background task that drains incoming
/// signalling messages and sends the protocol-level pings the SFU expects.
pub struct LiveKitParticipant {
    /// Signals the background task to leave the room. Taken by
    /// [`LiveKitParticipant::disconnect`].
    leave_tx: Option<tokio::sync::oneshot::Sender<()>>,
    task: tokio::task::JoinHandle<()>,
}

impl LiveKitParticipant {
    /// Connects to the SFU at `sfu_addr` with the given access token and
    /// returns once the SFU has admitted the participant into the room.
    pub async fn connect(sfu_addr: &str, access_token: &str) -> LiveKitParticipant {
        use futures_util::StreamExt;
        use tokio_tungstenite::tungstenite::Message;

        let url = format!(
            "ws://{sfu_addr}/rtc?access_token={access_token}&protocol=15&sdk=other&version=1.0.0&auto_subscribe=1"
        );
        let connect = tokio_tungstenite::connect_async(&url);
        let (mut socket, _) = tokio::time::timeout(Duration::from_secs(10), connect)
            .await
            .unwrap_or_else(|_| panic!("timed out connecting to the LiveKit SFU"))
            .unwrap_or_else(|e| panic!("failed to connect to the LiveKit SFU: {e}"));

        // Wait for the JoinResponse — the point at which the SFU has admitted
        // the participant. It also carries the ping interval to honour.
        let read_deadline = Duration::from_secs(10);
        let ping_interval = loop {
            let msg = tokio::time::timeout(read_deadline, socket.next())
                .await
                .unwrap_or_else(|_| panic!("timed out waiting for a JoinResponse from the SFU"))
                .unwrap_or_else(|| panic!("the SFU closed the connection before joining"))
                .unwrap_or_else(|e| panic!("error reading from the SFU: {e}"));

            let bytes = match msg {
                Message::Binary(bytes) => bytes,
                Message::Close(frame) => {
                    panic!("the SFU rejected the connection (likely an invalid token): {frame:?}")
                }
                _ => continue,
            };

            let response = <livekit_protocol::SignalResponse as prost::Message>::decode(&bytes[..])
                .unwrap_or_else(|e| panic!("failed to decode SignalResponse: {e}"));
            if let Some(livekit_protocol::signal_response::Message::Join(join)) = response.message {
                assert!(
                    join.room.is_some(),
                    "expected the JoinResponse to carry room info"
                );
                break Duration::from_secs(join.ping_interval.max(1) as u64);
            }
        };

        let (leave_tx, mut leave_rx) = tokio::sync::oneshot::channel();
        let task = tokio::spawn(async move {
            use futures_util::SinkExt;

            let mut ticker = tokio::time::interval(ping_interval);
            ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
            ticker.tick().await; // The first tick completes immediately.

            loop {
                tokio::select! {
                    _ = &mut leave_rx => {
                        // Leave explicitly rather than just dropping the
                        // socket: it makes the SFU report the departure as
                        // client-initiated, which is what a participant
                        // hanging up looks like.
                        let leave = livekit_protocol::SignalRequest {
                            message: Some(livekit_protocol::signal_request::Message::Leave(
                                livekit_protocol::LeaveRequest {
                                    can_reconnect: false,
                                    reason: livekit_protocol::DisconnectReason::ClientInitiated as i32,
                                    action: livekit_protocol::leave_request::Action::Disconnect as i32,
                                    regions: None,
                                },
                            )),
                        };
                        let _ = socket.send(Message::binary(prost::Message::encode_to_vec(&leave))).await;
                        let _ = socket.close(None).await;
                        return;
                    }
                    _ = ticker.tick() => {
                        let ping = livekit_protocol::SignalRequest {
                            message: Some(livekit_protocol::signal_request::Message::PingReq(
                                livekit_protocol::Ping { timestamp: 0, rtt: 0 },
                            )),
                        };
                        if socket.send(Message::binary(prost::Message::encode_to_vec(&ping))).await.is_err() {
                            return;
                        }
                    }
                    // Drain incoming messages so that the connection stays
                    // responsive (this is also what answers WebSocket pings).
                    msg = socket.next() => {
                        match msg {
                            None | Some(Err(_)) => return,
                            Some(Ok(_)) => {}
                        }
                    }
                }
            }
        });

        LiveKitParticipant {
            leave_tx: Some(leave_tx),
            task,
        }
    }

    /// Leaves the room and waits until the signalling connection is closed.
    pub async fn disconnect(mut self) {
        if let Some(tx) = self.leave_tx.take() {
            let _ = tx.send(());
        }
        let _ = tokio::time::timeout(Duration::from_secs(10), &mut self.task).await;
    }
}

impl Drop for LiveKitParticipant {
    fn drop(&mut self) {
        self.task.abort();
    }
}

// ── Helpers ────────────────────────────────────────────────────────

/// Requests a LiveKit access token for `member_id` / `device_id` in
/// `room_id` / `slot_id` through the homeserver's C-S API.
pub async fn get_livekit_token(
    cs_api_url: &str,
    user: &MatrixUser,
    livekit_url: &str,
    room_id: &str,
    slot_id: &str,
    member_id: &str,
    device_id: &str,
) -> String {
    let resp = reqwest::Client::new()
        .post(format!(
            "{cs_api_url}/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token"
        ))
        .bearer_auth(&user.access_token)
        .json(&serde_json::json!({
            "room_id": room_id,
            "slot_id": slot_id,
            "url": livekit_url,
            "member": {
                "id": member_id,
                "claimed_device_id": device_id,
            },
        }))
        .send()
        .await
        .expect("request to /rtc/livekit/get_token failed");
    let status = resp.status();
    let body: serde_json::Value = resp
        .json()
        .await
        .expect("get_token response was not valid JSON");
    assert!(
        status.is_success(),
        "expected a successful get_token response, got {status}: {body}"
    );
    body["jwt"]
        .as_str()
        .unwrap_or_else(|| panic!("get_token response is missing `jwt`: {body}"))
        .to_owned()
}

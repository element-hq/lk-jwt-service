// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//! Harness for the end-to-end test suite: brings up a real Synapse
//! homeserver and this service via Docker Compose, wired together as a
//! Matrix application service, shared by every test in the binary and torn
//! down once all of them have finished.

use std::path::PathBuf;
use std::process::{Command, Output};
use std::time::{Duration, Instant};

/// The service under test's published base URL.
pub const AUTH_SERVICE_URL: &str = "http://127.0.0.1:18080";

/// Synapse's client-server API, published to the host.
pub const SYNAPSE_CS_API_URL: &str = "http://127.0.0.1:18008";

/// The server name Synapse is configured with in docker/homeserver.yaml.
pub const SYNAPSE_SERVER_NAME: &str = "synapse.e2e.test";

/// The application service ID registered in docker/app-service.yaml.
pub const APPSERVICE_ID: &str = "lk-jwt-service";

/// The LIVEKIT_URL the jwt-service container is configured with. Only resolvable
/// from inside the Docker network. To actually reach the same LiveKit instance from the
/// host, use [`LIVEKIT_SFU_ADDR`] instead.
pub const LIVEKIT_URL: &str = "ws://livekit:7880";

/// The host-published address of the same LiveKit instance [`LIVEKIT_URL`]
/// points to from inside the Docker network.
pub const LIVEKIT_SFU_ADDR: &str = "127.0.0.1:17880";

/// The second service instance's published base URL
pub const AUTH_SERVICE2_URL: &str = "http://127.0.0.1:18081";

/// The second Synapse instance's client-server API, published to the host.
pub const SYNAPSE2_CS_API_URL: &str = "http://127.0.0.1:18009";

/// The server name the second Synapse instance is configured with in
/// docker/homeserver2.yaml.
pub const SYNAPSE2_SERVER_NAME: &str = "synapse2.e2e.test";

/// The LIVEKIT_URL the second jwt-service container is configured with. Only
/// resolvable from inside the Docker network. To actually reach the same LiveKit
/// instance from the host, use [`LIVEKIT2_SFU_ADDR`] instead.
pub const LIVEKIT2_URL: &str = "ws://livekit2:7880";

/// The host-published address of the same LiveKit instance [`LIVEKIT2_URL`]
/// points to from inside the Docker network.
pub const LIVEKIT2_SFU_ADDR: &str = "127.0.0.1:17881";

fn manifest_dir() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
}

/// The `docker compose` (or `docker-compose`) command prefix to use.
fn compose_base() -> &'static [&'static str] {
    use std::sync::OnceLock;
    static BASE: OnceLock<&'static [&'static str]> = OnceLock::new();
    BASE.get_or_init(|| {
        let has_plugin = Command::new("docker")
            .args(["compose", "version"])
            .output()
            .map(|o| o.status.success())
            .unwrap_or(false);
        if has_plugin {
            &["docker", "compose"]
        } else {
            &["docker-compose"]
        }
    })
}

/// Runs a `docker compose` command with the supplied arguments and returns its output.
fn compose(args: &[&str]) -> Output {
    let (cmd, base_args) = compose_base()
        .split_first()
        .expect("compose_base is non-empty");
    Command::new(cmd)
        .args(base_args)
        .args(["-f", "docker/docker-compose.yml"])
        .args(args)
        .current_dir(manifest_dir())
        .output()
        .expect("failed to run docker compose")
}

/// Tracks whether the e2e Docker Compose stack has been started, shared by
/// every test in the binary.
static STACK: tokio::sync::OnceCell<()> = tokio::sync::OnceCell::const_new();

/// Ensures the e2e Docker Compose stack (Synapse and the service under
/// test, registered as an application service) is up and both services
/// respond as healthy, starting it on the first call. Safe to call from
/// many concurrently-running tests: only the first caller actually starts
/// the stack, the rest just wait on that same result. Panics (dumping
/// container logs) if the stack doesn't become healthy in time.
///
/// The stack is torn down once, after every test in the binary has
/// finished — see [`teardown_stack`].
pub async fn ensure_stack() {
    STACK.get_or_init(start_stack).await;
}

async fn start_stack() {
    let up = compose(&["up", "-d", "--build"]);
    if !up.status.success() {
        panic!(
            "docker compose up failed:\nstdout: {}\nstderr: {}",
            String::from_utf8_lossy(&up.stdout),
            String::from_utf8_lossy(&up.stderr),
        );
    }

    wait_ready().await;
}

/// Waits for the stack's components to boot up and declare themselves as ready.
async fn wait_ready() {
    let client = reqwest::Client::builder()
        .timeout(Duration::from_secs(2))
        .build()
        .expect("failed to build reqwest client");
    let deadline = Instant::now() + Duration::from_secs(180);

    let checks = [
        ("synapse", format!("{SYNAPSE_CS_API_URL}/health")),
        ("jwt-service", format!("{AUTH_SERVICE_URL}/healthz")),
        ("livekit", format!("http://{LIVEKIT_SFU_ADDR}/")),
        ("synapse2", format!("{SYNAPSE2_CS_API_URL}/health")),
        ("jwt-service2", format!("{AUTH_SERVICE2_URL}/healthz")),
        ("livekit2", format!("http://{LIVEKIT2_SFU_ADDR}/")),
    ];
    for (name, url) in checks {
        loop {
            if let Ok(resp) = client.get(&url).send().await
                && resp.status().is_success()
            {
                break;
            }
            if Instant::now() > deadline {
                dump_logs();
                panic!("{name} did not become healthy in time (polled {url})");
            }
            tokio::time::sleep(Duration::from_millis(200)).await;
        }
    }
}

/// Prints the output of `docker compose logs`.
fn dump_logs() {
    let logs = compose(&["logs"]);
    eprintln!(
        "docker compose logs:\n{}\n{}",
        String::from_utf8_lossy(&logs.stdout),
        String::from_utf8_lossy(&logs.stderr),
    );
}

/// Tears the stack down once, after every test in the binary has finished.
///
/// Rust's built-in test harness ends by calling `std::process::exit`, which
/// skips normal `Drop`/thread-local destructors, so a plain `static`
/// guard's `Drop` wouldn't reliably fire here. `dtor` instead hooks the
/// OS-level process-exit path, which still runs in that case.
#[dtor::dtor(unsafe)]
fn teardown_stack() {
    if STACK.initialized() {
        let _ = compose(&["down", "-v"]); // Best-effort teardown.
    }
}

/// Builds a Matrix ID localpart that's unique per call, so tests sharing
/// one long-lived homeserver never collide when registering users.
pub fn unique_localpart(prefix: &str) -> String {
    format!("{prefix}-{}", uuid::Uuid::new_v4())
}

// ── Matrix client helpers ────────────────────────────────────────────────────

/// A user provisioned on the Synapse instance.
pub struct MatrixUser {
    pub user_id: String,
    pub access_token: String,
}

/// Registers a new user against the homeserver behind `cs_api_url`.
pub async fn register_user(cs_api_url: &str, username: &str, password: &str) -> MatrixUser {
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

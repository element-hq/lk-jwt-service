// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//! Harness for the appservice-ping end-to-end test: brings up a real
//! Synapse homeserver and this service via Docker Compose, wired together
//! as a Matrix application service, and tears them down afterward.

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

/// A running instance of the e2e Docker Compose stack.
///
/// This includes Synapse and the service under test, registered
/// as an application service. The stack is torn down on drop.
pub struct Stack;

impl Stack {
    /// Builds and starts the stack, waiting until both services respond as
    /// healthy. Panics (dumping container logs) if that doesn't happen in
    /// time.
    pub async fn start() -> Stack {
        let up = compose(&["up", "-d", "--build"]);
        if !up.status.success() {
            panic!(
                "docker compose up failed:\nstdout: {}\nstderr: {}",
                String::from_utf8_lossy(&up.stdout),
                String::from_utf8_lossy(&up.stderr),
            );
        }

        let stack = Stack;
        stack.wait_ready().await;
        stack
    }

    /// Waits for the stack's components to boot up and declare themselves as ready.
    async fn wait_ready(&self) {
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(2))
            .build()
            .expect("failed to build reqwest client");
        let deadline = Instant::now() + Duration::from_secs(180);

        let checks = [
            ("synapse", format!("{SYNAPSE_CS_API_URL}/health")),
            ("jwt-service", format!("{AUTH_SERVICE_URL}/healthz")),
            ("livekit", format!("http://{LIVEKIT_SFU_ADDR}/")),
        ];
        for (name, url) in checks {
            loop {
                if let Ok(resp) = client.get(&url).send().await
                    && resp.status().is_success()
                {
                    break;
                }
                if Instant::now() > deadline {
                    self.dump_logs();
                    panic!("{name} did not become healthy in time (polled {url})");
                }
                tokio::time::sleep(Duration::from_millis(200)).await;
            }
        }
    }

    /// Prints the output of `docker compose logs`.
    fn dump_logs(&self) {
        let logs = compose(&["logs"]);
        eprintln!(
            "docker compose logs:\n{}\n{}",
            String::from_utf8_lossy(&logs.stdout),
            String::from_utf8_lossy(&logs.stderr),
        );
    }
}

impl Drop for Stack {
    fn drop(&mut self) {
        let _ = compose(&["down", "-v"]); // Tear down the stack.
    }
}

// ── Matrix client helpers ────────────────────────────────────────────────────

/// A user provisioned on the Synapse instance.
pub struct MatrixUser {
    pub user_id: String,
    pub access_token: String,
}

/// Registers a new user against the Synapse instance.
pub async fn register_user(username: &str, password: &str) -> MatrixUser {
    let client = reqwest::Client::new();
    let register_url = format!("{SYNAPSE_CS_API_URL}/_matrix/client/v3/register");

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

/// Creates a room as the given user and returns its room ID.
pub async fn create_and_join_room(user: &MatrixUser) -> String {
    let resp = reqwest::Client::new()
        .post(format!("{SYNAPSE_CS_API_URL}/_matrix/client/v3/createRoom"))
        .bearer_auth(&user.access_token)
        .json(&serde_json::json!({}))
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

// ── SFU verification ─────────────────────────────────────────────────────────

/// Connects to the LiveKit SFU's RTC signalling endpoint using the
/// given access token and confirms the SFU accepts it.
pub async fn verify_livekit_token_is_usable(access_token: &str) {
    use futures_util::StreamExt;
    use tokio_tungstenite::tungstenite::Message;

    let url = format!(
        "ws://{LIVEKIT_SFU_ADDR}/rtc?access_token={access_token}&protocol=15&sdk=other&version=1.0.0&auto_subscribe=1"
    );
    let connect = tokio_tungstenite::connect_async(&url);
    let (mut socket, _) = tokio::time::timeout(Duration::from_secs(10), connect)
        .await
        .unwrap_or_else(|_| panic!("timed out connecting to the LiveKit SFU"))
        .unwrap_or_else(|e| panic!("failed to connect to the LiveKit SFU: {e}"));

    let read_deadline = Duration::from_secs(10);
    loop {
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
            let _ = socket.close(None).await;
            return;
        }
    }
}

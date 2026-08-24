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

/// The server name Synapse is configured with in docker/homeserver.yaml.
pub const SYNAPSE_SERVER_NAME: &str = "synapse.e2e.test";

/// The application service ID registered in docker/app-service.yaml.
pub const APPSERVICE_ID: &str = "lk-jwt-service";

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
    let (cmd, base_args) = compose_base().split_first().expect("compose_base is non-empty");
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
            ("synapse", "http://127.0.0.1:18008/health".to_owned()),
            ("jwt-service", format!("{AUTH_SERVICE_URL}/healthz")),
        ];
        for (name, url) in checks {
            loop {
                if let Ok(resp) = client.get(&url).send().await {
                    if resp.status().is_success() {
                        break;
                    }
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

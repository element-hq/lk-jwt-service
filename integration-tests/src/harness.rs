// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use std::collections::HashMap;
use std::io::Read;
use std::net::TcpListener;
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

/// The SFU API credentials the service under test is started with. Tests
/// use them to sign fake SFU webhooks.
pub const LIVEKIT_KEY: &str = "devkey";
pub const LIVEKIT_SECRET: &str = "devsecret";

static BINARY: OnceLock<Result<PathBuf, String>> = OnceLock::new();

/// Build the service from the repository root and return the path
/// to the resulting binary.
fn service_binary() -> PathBuf {
    let result = BINARY.get_or_init(|| {
        // Look up the repository root.
        let repo_root = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("..");

        // Build the service.
        let output = Command::new("cargo")
            .args(["build", "--bin", "lk-jwt-service"])
            .current_dir(&repo_root)
            .output()
            .map_err(|e| format!("failed to run cargo build: {e}"))?;
        if !output.status.success() {
            return Err(format!(
                "building service: {}",
                String::from_utf8_lossy(&output.stderr)
            ));
        }
        Ok(repo_root.join("target/debug/lk-jwt-service"))
    });
    match result {
        Ok(path) => path.clone(),
        Err(e) => panic!("failed to build service binary: {e}"),
    }
}

/// The shell environment that the service is started with.
#[derive(Default)]
pub struct ServiceConfig {
    /// Becomes LIVEKIT_FULL_ACCESS_HOMESERVERS.
    pub full_access_homeservers: Vec<String>,
    /// Becomes LIVEKIT_CS_API_URL_OVERRIDES.
    pub cs_api_url_overrides: HashMap<String, String>,
    /// Becomes LIVEKIT_URL. Defaults to a closed local port; tests that
    /// need a live SFU point this at a fake.
    pub livekit_url: Option<String>,
    /// Becomes LIVEKIT_REDIS_URL. None selects the in-memory store.
    pub redis_url: Option<String>,
    /// Appended last, wins over harness defaults.
    pub extra_env: HashMap<String, String>,
}

/// A running instance of the service under test. Killed on drop; logs are
/// dumped if the owning test is panicking.
pub struct Service {
    /// The http://host:port root of the service.
    pub base_url: String,

    /// The child process that lk-jwt-service runs as.
    child: Child,

    /// Logs collected from the child process.
    logs: Arc<Mutex<Vec<u8>>>,
}

impl Service {
    /// Build and start the service with the given configuration and wait
    /// until it responds on /healthz.
    pub async fn start(cfg: ServiceConfig) -> Service {
        // Prepare the shell environment.
        let port = free_port();
        let base_url = format!("http://127.0.0.1:{port}");

        let livekit_url = cfg.livekit_url.unwrap_or_else(|| {
            // Nothing listens here for now.
            "ws://127.0.0.1:9".to_owned()
        });

        let mut env: HashMap<String, String> = HashMap::from([
            ("LIVEKIT_URL".into(), livekit_url),
            ("LIVEKIT_KEY".into(), LIVEKIT_KEY.into()),
            ("LIVEKIT_SECRET".into(), LIVEKIT_SECRET.into()),
            ("LIVEKIT_JWT_BIND".into(), format!("127.0.0.1:{port}")),
            (
                "LIVEKIT_FULL_ACCESS_HOMESERVERS".into(),
                cfg.full_access_homeservers.join(","),
            ),
            (
                "LIVEKIT_INSECURE_SKIP_VERIFY_TLS".into(),
                "YES_I_KNOW_WHAT_I_AM_DOING".into(),
            ),
            ("LIVEKIT_LOG_LEVEL".into(), "debug".into()),
        ]);

        if !cfg.cs_api_url_overrides.is_empty() {
            let entries: Vec<String> = cfg
                .cs_api_url_overrides
                .iter()
                .map(|(server, url)| format!("{server}={url}"))
                .collect();
            env.insert("LIVEKIT_CS_API_URL_OVERRIDES".into(), entries.join(","));
        }

        if let Some(redis_url) = cfg.redis_url {
            env.insert("LIVEKIT_REDIS_URL".into(), redis_url);
        }

        env.extend(cfg.extra_env);

        // Spawn the child process.
        let binary = service_binary();
        let mut child = Command::new(binary)
            .envs(&env)
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .expect("failed to start service");

        // Set up log capturing.
        let logs = Arc::new(Mutex::new(Vec::new()));
        for pipe in [
            Box::new(child.stdout.take().unwrap()) as Box<dyn Read + Send>,
            Box::new(child.stderr.take().unwrap()),
        ] {
            let logs = Arc::clone(&logs);
            std::thread::spawn(move || {
                let mut pipe = pipe;
                let mut buf = [0u8; 4096];
                while let Ok(n) = pipe.read(&mut buf) {
                    if n == 0 {
                        break;
                    }
                    logs.lock().unwrap().extend_from_slice(&buf[..n]);
                }
            });
        }

        let mut svc = Service {
            base_url,
            child,
            logs,
        };

        // Wait for the service to confirm that it's healthy.
        svc.wait_healthy().await;

        svc
    }

    /// Poll /healthz until the service is healthy.
    async fn wait_healthy(&mut self) {
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(1))
            .build()
            .unwrap();
        let deadline = Instant::now() + Duration::from_secs(15);

        loop {
            // Call /healthz and return if it succeeds.
            if let Ok(resp) = client
                .get(format!("{}/healthz", self.base_url))
                .send()
                .await
                && resp.status() == reqwest::StatusCode::OK
            {
                return;
            }

            // Check if the service exited.
            if self
                .child
                .try_wait()
                .expect("failed to poll service")
                .is_some()
            {
                panic!(
                    "service exited before becoming healthy; logs:\n{}",
                    self.logs_string()
                );
            }

            // Check if the deadline expired.
            if Instant::now() > deadline {
                panic!(
                    "service did not become healthy in time; logs:\n{}",
                    self.logs_string()
                );
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
    }

    /// Convert the collected logs into a string.
    fn logs_string(&self) -> String {
        String::from_utf8_lossy(&self.logs.lock().unwrap()).into_owned()
    }
}

impl Drop for Service {
    fn drop(&mut self) {
        // Make sure to kill the child process once the service is dropped.
        let _ = self.child.kill();
        let _ = self.child.wait();
        if std::thread::panicking() {
            eprintln!("service logs:\n{}", self.logs_string());
        }
    }
}

/// Reserve an ephemeral TCP port on 127.0.0.1 and release it for the
/// service to bind. The tiny race between closing and rebinding is
/// acceptable in tests.
fn free_port() -> u16 {
    TcpListener::bind("127.0.0.1:0")
        .expect("failed to reserve port")
        .local_addr()
        .unwrap()
        .port()
}

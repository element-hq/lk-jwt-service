// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//! Workspace automation, invoked through the `cargo xtask` alias defined in
//! `.cargo/config.toml`.
//!
//! It exists for the end-to-end suite: bringing its Docker Compose stack up
//! costs minutes, so the tests don't do it themselves. This task can bring the
//! stack up once, run e2e tests against it and tear the stack down.

use std::io::IsTerminal;
use std::path::{Path, PathBuf};
use std::process::{Command, ExitCode, Stdio};
use std::sync::OnceLock;
use std::time::{Duration, Instant};

use lk_jwt_service_e2e_tests as e2e;

/// Prints a progress line, prefixed with the time elapsed since startup.
///
/// When images need to be rebuilt, the stack may take minutes to come up,
/// so the point of these is that someone reading along -- in a terminal or in
/// a CI log -- can always tell which step is running and how long it has been
/// at it.
macro_rules! log {
    ($($arg:tt)*) => {
        eprintln!("[xtask {}] {}", clock(), format_args!($($arg)*))
    };
}

const HELP: &str = "\
cargo xtask — workspace automation

USAGE:
    cargo xtask e2e [--keep] [-- <cargo test args>...]
            Bring the e2e Docker Compose stack up, run the whole e2e suite
            against it and tear it down again. Extra arguments after `--`
            are passed on to `cargo test`, e.g.

                cargo xtask e2e -- --locked
                cargo xtask e2e -- get_token_local_sfu_succeeds

            --keep  Leave the stack running afterwards, so the next run
                    doesn't have to boot it again. Tear it down with
                    `cargo xtask e2e-down`.

    cargo xtask e2e-up
            Bring the stack up and leave it running, without running tests.

    cargo xtask e2e-down
            Tear the stack down.
";

fn main() -> ExitCode {
    let started = *START.get_or_init(Instant::now);

    let mut args = std::env::args().skip(1);
    let Some(task) = args.next() else {
        eprint!("{HELP}");
        return ExitCode::FAILURE;
    };
    if matches!(task.as_str(), "help" | "-h" | "--help") {
        print!("{HELP}");
        return ExitCode::SUCCESS;
    }

    log!("running task `{task}`");
    let result = match task.as_str() {
        "e2e" => e2e_task(args),
        "e2e-up" => stack_up().inspect_err(|_| {
            dump_status();
            dump_logs();
        }),
        "e2e-down" => stack_down(),
        other => Err(format!("unknown task `{other}`\n\n{HELP}")),
    };

    let took = format_duration(started.elapsed());
    match result {
        Ok(()) => {
            log!("`{task}` succeeded, total time {took}");
            ExitCode::SUCCESS
        }
        Err(message) => {
            log!("`{task}` FAILED after {took}: {message}");
            ExitCode::FAILURE
        }
    }
}

/// Runs the end-to-end suite against a single, shared stack.
fn e2e_task(args: std::iter::Skip<std::env::Args>) -> Result<(), String> {
    // Everything up to `--` is ours, everything after it is `cargo test`'s.
    let mut keep = false;
    let mut args = args;
    for arg in args.by_ref() {
        match arg.as_str() {
            "--keep" => keep = true,
            "--" => break,
            other => return Err(format!("unknown option `{other}`\n\n{HELP}")),
        }
    }
    let cargo_test_args: Vec<String> = args.collect();

    // Note the lack of `?` on stack_up: a stack that came up but never went
    // healthy still has containers holding its ports, so it has to be torn
    // down like any other failure rather than left behind for the next run
    // to trip over.
    let outcome = stack_up().and_then(|()| run_e2e_tests(&cargo_test_args));
    if outcome.is_err() {
        // The stack is about to go away, so grab what its containers have to
        // say about the failure while they're still there.
        dump_status();
        dump_logs();
    }

    if keep {
        log!("leaving the stack running (--keep), tear it down with `cargo xtask e2e-down`");
    } else {
        stack_down()?;
    }

    outcome
}

/// Runs `cargo test` over the e2e suite, with the stack marked as running.
///
/// `--no-fail-fast` because the stack is the expensive part: once it's up,
/// running the remaining test binaries after a failure is nearly free, and
/// far more useful than a single failure per stack boot.
fn run_e2e_tests(extra_args: &[String]) -> Result<(), String> {
    let cargo = std::env::var("CARGO").unwrap_or_else(|_| "cargo".to_owned());
    let args: Vec<String> = ["test", "--package", e2e::PACKAGE_NAME, "--no-fail-fast"]
        .iter()
        .map(|a| (*a).to_owned())
        .chain(extra_args.iter().cloned())
        .collect();

    log!("running the e2e suite against the stack");
    log!("$ {cargo} {}", args.join(" "));
    let started = Instant::now();
    let status = Command::new(cargo)
        .args(&args)
        .env(e2e::STACK_RUNNING_ENV, "1")
        .current_dir(repo_root())
        .status()
        .map_err(|e| format!("failed to run cargo test: {e}"))?;
    let took = format_duration(started.elapsed());

    if status.success() {
        log!("the e2e suite passed in {took}");
        Ok(())
    } else {
        log!("the e2e suite failed in {took} ({status})");
        Err("the e2e suite failed".to_owned())
    }
}

// ── The Docker Compose stack ─────────────────────────────────────────────────

/// The Compose project the stack's containers, network and volumes belong to.
const PROJECT_NAME: &str = "lk-jwt-service-e2e";

/// How long the stack gets to boot before its components are declared dead.
const READY_TIMEOUT: Duration = Duration::from_secs(180);

/// How often to report a component that's still not answering.
const READY_REPORT_INTERVAL: Duration = Duration::from_secs(15);

/// Builds and starts the stack, returning once every component reports
/// healthy. Starting an already-running stack is a no-op beyond picking up
/// source changes, so this is safe to call repeatedly.
fn stack_up() -> Result<(), String> {
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .map_err(|e| format!("failed to build a tokio runtime: {e}"))?;
    let client = reqwest::Client::builder()
        .timeout(Duration::from_secs(2))
        .build()
        .map_err(|e| format!("failed to build a reqwest client: {e}"))?;

    log!("checking the host ports the stack publishes are free");
    runtime.block_on(check_ports_free(&client))?;

    log!("starting the stack (building images first, this takes a while)");
    let started = Instant::now();
    compose(&["up", "-d", "--build"])?;
    log!(
        "containers started in {}",
        format_duration(started.elapsed())
    );

    runtime.block_on(wait_ready(&client))
}

/// Tears the stack down, discarding its volumes.
fn stack_down() -> Result<(), String> {
    log!("tearing the stack down");
    let started = Instant::now();
    compose(&["down", "-v"])?;
    log!("stack torn down in {}", format_duration(started.elapsed()));
    Ok(())
}

/// One component of the stack, as seen from the host.
struct Component {
    name: &'static str,
    /// The `host:port` the container publishes.
    addr: String,
    /// The URL that answers once the component is ready.
    health_url: String,
}

/// The stack's components, in the order they're expected to come up.
///
/// Both the port preflight and the readiness polling read this, so the two
/// can't drift apart -- and neither can drift from the addresses the tests
/// themselves use, which is where the URLs come from.
fn components() -> Vec<Component> {
    let served = |name, base: &str, health_path: &str| Component {
        name,
        addr: base
            .trim_start_matches("http://")
            .trim_end_matches('/')
            .to_owned(),
        health_url: format!("{base}{health_path}"),
    };
    let sfu = |name, addr: &str| Component {
        name,
        addr: addr.to_owned(),
        health_url: format!("http://{addr}/"),
    };
    vec![
        served("synapse-a", e2e::SYNAPSE_A_CS_API_URL, "/health"),
        served("jwt-service-a", e2e::AUTH_SERVICE_A_URL, "/healthz"),
        sfu("livekit-a", e2e::LIVEKIT_A_SFU_ADDR),
        served("synapse-b", e2e::SYNAPSE_B_CS_API_URL, "/health"),
        served("jwt-service-b", e2e::AUTH_SERVICE_B_URL, "/healthz"),
        sfu("livekit-b", e2e::LIVEKIT_B_SFU_ADDR),
    ]
}

/// Fails when something on the host already holds a port the stack needs.
///
/// Compose only reports this once every image has been built and it gets
/// as far as creating the containers, which is minutes of work thrown away
/// -- and it reports it as a bare "port is already allocated" naming a
/// container rather than whatever is actually in the way. Checking first
/// costs milliseconds.
///
/// A port held by an already-running stack of our own is fine -- that's the
/// `--keep` workflow -- so a taken port only counts as a conflict when the
/// component that should be behind it doesn't answer as itself.
async fn check_ports_free(client: &reqwest::Client) -> Result<(), String> {
    let mut taken = Vec::new();
    for component in components() {
        let port = component
            .addr
            .rsplit(':')
            .next()
            .and_then(|p| p.parse::<u16>().ok())
            .ok_or_else(|| format!("no port in {}'s address {}", component.name, component.addr))?;

        // No SO_REUSEADDR, so this fails whenever anything holds the port,
        // whether it bound the wildcard address or just one interface.
        if std::net::TcpListener::bind(("0.0.0.0", port)).is_ok() {
            continue;
        }
        // Answering the health URL isn't enough on its own: a foreign
        // LiveKit on our port answers `GET /` exactly like ours would. It
        // has to be a container of this project too.
        if has_container(component.name) && is_healthy(client, &component.health_url).await {
            log!("  ✓ {} is already up on port {port}", component.name);
            continue;
        }
        taken.push(format!("{port} (wanted by {})", component.name));
    }

    if taken.is_empty() {
        Ok(())
    } else {
        Err(format!(
            "these host ports are in use by something that isn't this stack: {}. \
             Another Compose project, an SSH forward or a stray container is \
             holding them -- `lsof -nP -iTCP:<port> -sTCP:LISTEN` says who. Free \
             them and try again.",
            taken.join(", ")
        ))
    }
}

/// Whether this Compose project has a container for the given service.
fn has_container(service: &str) -> bool {
    compose_output(&["ps", "--quiet", service]).is_some_and(|out| !out.trim().is_empty())
}

/// Runs a `docker compose` command and captures its stdout, or returns
/// `None` if it couldn't be run or exited non-zero. For probing state,
/// where a failure just means "don't know" rather than something worth
/// reporting.
fn compose_output(args: &[&str]) -> Option<String> {
    let cli = compose_cli();
    let output = Command::new(&cli.program)
        .args(&cli.args)
        .args(args)
        .current_dir(docker_dir())
        .stdin(Stdio::null())
        .stderr(Stdio::null())
        .output()
        .ok()?;
    output
        .status
        .success()
        .then(|| String::from_utf8_lossy(&output.stdout).into_owned())
}

/// Whether the given health endpoint answers successfully right now.
async fn is_healthy(client: &reqwest::Client, url: &str) -> bool {
    matches!(client.get(url).send().await, Ok(r) if r.status().is_success())
}

/// Polls the endpoints the stack publishes to the host until every one of
/// them answers, reporting each component as it comes up and naming the ones
/// that are taking their time.
async fn wait_ready(client: &reqwest::Client) -> Result<(), String> {
    let checks = components();

    log!(
        "waiting for {} components to report healthy (giving them {})",
        checks.len(),
        format_duration(READY_TIMEOUT)
    );
    let waiting_since = Instant::now();

    for Component {
        name,
        health_url: url,
        ..
    } in &checks
    {
        let started = Instant::now();
        let mut last_report = Instant::now();
        loop {
            let reason = match client.get(url).send().await {
                Ok(response) if response.status().is_success() => break,
                Ok(response) => format!("HTTP {}", response.status()),
                Err(e) => root_cause(&e),
            };

            if started.elapsed() > READY_TIMEOUT {
                return Err(format!(
                    "{name} did not become healthy within {}: {reason} (polled {url})",
                    format_duration(READY_TIMEOUT)
                ));
            }
            if last_report.elapsed() >= READY_REPORT_INTERVAL {
                log!(
                    "  … still waiting for {name} after {}: {reason} ({url})",
                    format_duration(started.elapsed())
                );
                last_report = Instant::now();
            }
            tokio::time::sleep(Duration::from_millis(200)).await;
        }
        log!(
            "  ✓ {name} healthy after {} ({url})",
            format_duration(started.elapsed())
        );
    }

    log!(
        "the whole stack is healthy, {} after the containers started",
        format_duration(waiting_since.elapsed())
    );
    Ok(())
}

/// Prints the stack's container states.
fn dump_status() {
    log!("collecting container status");
    if let Err(e) = compose(&["ps", "--all"]) {
        log!("failed to collect container status: {e}");
    }
}

/// Prints the output of `docker compose logs`.
fn dump_logs() {
    log!("collecting container logs");
    if let Err(e) = compose(&["logs", "--timestamps"]) {
        log!("failed to collect container logs: {e}");
    }
}

/// Runs a `docker compose` command against the e2e stack, passing its
/// output straight through to this process' own stdout/stderr.
fn compose(args: &[&str]) -> Result<(), String> {
    let cli = compose_cli();
    let command_line = format!("{} {} {}", cli.program, cli.args.join(" "), args.join(" "));
    log!("$ {command_line}");

    let status = Command::new(&cli.program)
        .args(&cli.args)
        .args(args)
        .current_dir(docker_dir())
        .stdin(Stdio::null())
        .status()
        .map_err(|e| format!("failed to run {}: {e}", cli.program))?;
    if status.success() {
        Ok(())
    } else {
        Err(format!("`{command_line}` failed ({status})"))
    }
}

/// How to invoke Compose: the command itself plus the global flags that
/// every call to it shares.
struct ComposeCli {
    program: String,
    args: Vec<String>,
}

fn compose_cli() -> &'static ComposeCli {
    static CLI: OnceLock<ComposeCli> = OnceLock::new();
    CLI.get_or_init(|| {
        let has_plugin = Command::new("docker")
            .args(["compose", "version"])
            .output()
            .map(|o| o.status.success())
            .unwrap_or(false);

        let mut args: Vec<String> = Vec::new();
        let program = if has_plugin {
            args.push("compose".to_owned());
            // Without a TTY, Compose's default progress renderer collapses
            // a multi-minute image build into a handful of status lines.
            // Plain mode keeps every build step in the log instead, which
            // is the only view of the build a CI run gets. The flag is the
            // plugin's; standalone docker-compose has no equivalent.
            if !std::io::stderr().is_terminal() {
                args.extend(["--progress".to_owned(), "plain".to_owned()]);
            }
            "docker".to_owned()
        } else {
            "docker-compose".to_owned()
        };
        // An explicit project name: without one, Compose derives it from the
        // directory holding the file, which is just `docker` and thus prone
        // to collide with unrelated stacks on a developer's machine.
        args.extend(["--project-name".to_owned(), PROJECT_NAME.to_owned()]);
        args.extend(["--file".to_owned(), "docker-compose.yml".to_owned()]);

        ComposeCli { program, args }
    })
}

// ── Odds and ends ────────────────────────────────────────────────────────────

static START: OnceLock<Instant> = OnceLock::new();

/// The time elapsed since startup as `mm:ss`, for the log line prefix.
fn clock() -> String {
    let elapsed = START.get_or_init(Instant::now).elapsed().as_secs();
    format!("{:02}:{:02}", elapsed / 60, elapsed % 60)
}

/// Formats a duration compactly, e.g. `4.2s` or `3m12s`.
fn format_duration(duration: Duration) -> String {
    let seconds = duration.as_secs();
    if seconds >= 60 {
        format!("{}m{:02}s", seconds / 60, seconds % 60)
    } else {
        format!("{:.1}s", duration.as_secs_f64())
    }
}

/// The innermost cause of an error, which for the readiness polling is the
/// part worth printing -- "Connection refused (os error 61)" rather than
/// reqwest's full "error sending request for url ..." wrapping.
fn root_cause(error: &dyn std::error::Error) -> String {
    let mut cause = error;
    while let Some(source) = cause.source() {
        cause = source;
    }
    cause.to_string()
}

fn repo_root() -> &'static Path {
    // xtask lives directly beneath the workspace root.
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .expect("xtask's manifest directory has a parent")
}

fn docker_dir() -> PathBuf {
    repo_root().join("e2e-tests/docker")
}

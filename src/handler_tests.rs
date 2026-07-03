// Copyright 2025 Element Creations Ltd.
// Copyright 2023 - 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// handler_tests.rs: cross-cutting tests for the Handler infrastructure —
// healthcheck, is_full_access_user, get_join_token, the actor-loop
// lifecycle, sfu_event_from_webhook translation — plus the endpoint tests
// for /get_token, /sfu/get and /delegate_delayed_leave and the store
// recovery tests.

use std::sync::Mutex;

use axum::body::Body;
use base64::engine::general_purpose::STANDARD as BASE64_STANDARD;
use base64::Engine;
use futures::future::BoxFuture;
use sha2::Digest;
use tower::util::ServiceExt;

use super::*;
use crate::delayed_event_manager::DelayEventAction;
use crate::helper::{resolve_cs_api_url_via, ActionError, RoomServiceClient, UserInfo};
use crate::requests::MatrixRtcMemberType;
use crate::store::test_support::{new_in_memory_store, new_notifying_store, FailingStore};

// ── test deps ─────────────────────────────────────────────────────────────────

type ExchangeFn = Box<dyn Fn(&OpenIdTokenType, bool) -> Result<UserInfo, String> + Send + Sync>;
type ResolveFn = Box<dyn Fn(&str) -> Result<CsApiUrl, String> + Send + Sync>;
type CreateRoomMockFn =
    Box<dyn Fn(&LiveKitRoomAlias, &str, &LiveKitIdentity) -> Result<(), String> + Send + Sync>;
type ExistsFn = Box<
    dyn Fn(&LiveKitRoomAlias, &LiveKitIdentity) -> BoxFuture<'static, Result<bool, String>>
        + Send
        + Sync,
>;
type ExecFn =
    Box<dyn Fn(&CsApiUrl, &str, DelayEventAction) -> Result<u16, ActionError> + Send + Sync>;

/// A [`Deps`] implementation whose behaviours are swappable per test — the
/// equivalent of the Go tests' global function-variable swaps. Un-mocked
/// methods panic (they must never be reached in a test), except CS-API URL
/// resolution, which falls back to the real override/cache/discovery logic
/// like the un-swapped Go default.
#[derive(Default)]
struct HandlerTestDeps {
    exchange: Option<ExchangeFn>,
    resolve: Option<ResolveFn>,
    create_room: Option<CreateRoomMockFn>,
    exists: Option<ExistsFn>,
    exec: Option<ExecFn>,
}

#[async_trait::async_trait]
impl Deps for HandlerTestDeps {
    fn new_room_service_client(
        &self,
        _url: &str,
        _key: &str,
        _secret: &str,
    ) -> Arc<dyn RoomServiceClient> {
        panic!("new_room_service_client not mocked in HandlerTestDeps");
    }

    async fn discover_client_api(
        &self,
        _server_name: &str,
    ) -> Result<Option<crate::helper::ClientWellKnown>, String> {
        panic!("discover_client_api not mocked in HandlerTestDeps");
    }

    async fn resolve_cs_api_url(
        &self,
        server_name: &str,
        overrides: &HashMap<String, CsApiUrl>,
        cache: Option<&CsApiUrlCache>,
    ) -> Result<CsApiUrl, String> {
        match &self.resolve {
            Some(f) => f(server_name),
            None => resolve_cs_api_url_via(self, server_name, overrides, cache).await,
        }
    }

    async fn exchange_openid_userinfo(
        &self,
        token: &OpenIdTokenType,
        skip_verify_tls: bool,
    ) -> Result<UserInfo, String> {
        match &self.exchange {
            Some(f) => f(token, skip_verify_tls),
            None => panic!("exchange_openid_userinfo not mocked in HandlerTestDeps"),
        }
    }

    async fn create_livekit_room(
        &self,
        _livekit_auth: &LiveKitAuth,
        room: &LiveKitRoomAlias,
        matrix_user: &str,
        lk_identity: &LiveKitIdentity,
    ) -> Result<(), String> {
        match &self.create_room {
            Some(f) => f(room, matrix_user, lk_identity),
            None => panic!("create_livekit_room not mocked in HandlerTestDeps"),
        }
    }

    async fn participant_exists(
        &self,
        _lk_auth: &LiveKitAuth,
        room: &LiveKitRoomAlias,
        identity: &LiveKitIdentity,
    ) -> Result<bool, String> {
        match &self.exists {
            Some(f) => f(room, identity).await,
            None => panic!("participant_exists not mocked in HandlerTestDeps"),
        }
    }

    async fn execute_delayed_event_action(
        &self,
        cs_api_url: &CsApiUrl,
        delay_id: &str,
        action: DelayEventAction,
    ) -> Result<u16, ActionError> {
        match &self.exec {
            Some(f) => f(cs_api_url, delay_id, action),
            None => panic!("execute_delayed_event_action not mocked in HandlerTestDeps"),
        }
    }
}

/// A participant_exists mock that blocks until the surrounding job is
/// cancelled — the Go tests' `<-ctx.Done()` idiom.
fn exists_block_until_cancelled() -> Option<ExistsFn> {
    Some(Box::new(|_, _| Box::pin(std::future::pending())))
}

fn exchange_ok(sub: &str) -> Option<ExchangeFn> {
    let sub = sub.to_owned();
    Some(Box::new(move |_, _| Ok(UserInfo { sub: sub.clone() })))
}

fn exec_ok() -> Option<ExecFn> {
    Some(Box::new(|_, _, _| Ok(200)))
}

// ── construction helpers ──────────────────────────────────────────────────────

fn default_auth() -> LiveKitAuth {
    LiveKitAuth {
        key: "key".into(),
        secret: "secret".into(),
        lk_url: "ws://localhost:7880".into(),
    }
}

fn new_handler_with(
    deps: HandlerTestDeps,
    full_access: &[&str],
    store: Option<Arc<dyn Store>>,
) -> Arc<Handler> {
    Handler::new(
        default_auth(),
        false,
        full_access.iter().map(|s| s.to_string()).collect(),
        Duration::ZERO, // sanity check interval disabled
        HashMap::new(),
        store,
        Arc::new(deps),
    )
}

/// Creates a Handler configured for testing /get_token, with example.com as
/// the only full-access homeserver.
fn new_get_token_handler(deps: HandlerTestDeps) -> Arc<Handler> {
    Handler::new(
        LiveKitAuth {
            key: "key".into(),
            secret: "secret".into(),
            lk_url: "wss://lk.local".into(),
        },
        false,
        vec!["example.com".into()],
        Duration::ZERO,
        HashMap::new(),
        None,
        Arc::new(deps),
    )
}

/// Creates a Handler configured for testing /delegate_delayed_leave.
fn new_delegate_delayed_leave_handler(deps: HandlerTestDeps) -> Arc<Handler> {
    Handler::new(
        default_auth(),
        false,
        vec!["example.com".into()],
        Duration::ZERO,
        HashMap::from([(
            "example.com".to_owned(),
            CsApiUrl("https://matrix.example.com".into()),
        )]),
        None,
        Arc::new(deps),
    )
}

async fn send_request(handler: &Arc<Handler>, request: http::Request<Body>) -> Response {
    handler.prepare_router().oneshot(request).await.unwrap()
}

async fn body_bytes(resp: Response) -> Vec<u8> {
    axum::body::to_bytes(resp.into_body(), usize::MAX)
        .await
        .unwrap()
        .to_vec()
}

fn post_json(path: &str, body: String) -> http::Request<Body> {
    http::Request::builder()
        .method("POST")
        .uri(path)
        .body(Body::from(body))
        .unwrap()
}

fn parse_jwt_claims(token: &str, secret: &str) -> serde_json::Value {
    let validation = jsonwebtoken::Validation::new(jsonwebtoken::Algorithm::HS256);
    jsonwebtoken::decode::<serde_json::Value>(
        token,
        &jsonwebtoken::DecodingKey::from_secret(secret.as_bytes()),
        &validation,
    )
    .expect("failed to parse JWT")
    .claims
}

/// A fully populated, valid request. The default homeserver is "example.com"
/// (matches new_get_token_handler's full-access list).
fn valid_sfu_request() -> SfuRequest {
    SfuRequest {
        room_id: "!testRoom:example.com".into(),
        slot_id: "m.call#ROOM".into(),
        openid_token: OpenIdTokenType {
            access_token: "test-token".into(),
            matrix_server_name: "example.com".into(),
            ..Default::default()
        },
        member: MatrixRtcMemberType {
            id: "member-id".into(),
            claimed_user_id: "@user:example.com".into(),
            claimed_device_id: "device-id".into(),
        },
        ..Default::default()
    }
}

/// A valid request body with an optional mutation applied before marshalling.
fn marshal_sfu_request(mutate: impl FnOnce(&mut SfuRequest)) -> String {
    let mut req = valid_sfu_request();
    mutate(&mut req);
    serde_json::to_string(&req).expect("failed to marshal request")
}

fn valid_delegate_delayed_leave_request() -> DelegateDelayedLeaveRequest {
    DelegateDelayedLeaveRequest {
        room_id: "!testRoom:example.com".into(),
        slot_id: "m.call#ROOM".into(),
        openid_token: OpenIdTokenType {
            access_token: "test-token".into(),
            matrix_server_name: "example.com".into(),
            ..Default::default()
        },
        member: MatrixRtcMemberType {
            id: "member-id".into(),
            claimed_user_id: "@user:example.com".into(),
            claimed_device_id: "device-id".into(),
        },
        delay_id: "syd_delay123".into(),
        delay_timeout: 30000, // 30 s in ms
        delay_cs_api_url: "https://matrix.example.com".into(),
    }
}

fn marshal_delegate_delayed_leave_request(
    mutate: impl FnOnce(&mut DelegateDelayedLeaveRequest),
) -> String {
    let mut req = valid_delegate_delayed_leave_request();
    mutate(&mut req);
    serde_json::to_string(&req).expect("failed to marshal request")
}

async fn wait_recovery_done(handler: &Arc<Handler>) {
    let mut recovery = handler.recovery_done();
    tokio::time::timeout(Duration::from_secs(3), async {
        while !*recovery.borrow_and_update() {
            recovery.changed().await.expect("recovery watch closed");
        }
    })
    .await
    .expect("timed out waiting for handler to complete recovery");
}

// ── HTTP handler smoke tests ──────────────────────────────────────────────────

#[tokio::test]
async fn test_healthcheck() {
    let handler = new_handler_with(HandlerTestDeps::default(), &["*"], None);
    for method in ["GET", "HEAD"] {
        let req = http::Request::builder()
            .method(method)
            .uri("/healthz")
            .body(Body::empty())
            .unwrap();
        let resp = send_request(&handler, req).await;
        assert_eq!(resp.status(), StatusCode::OK, "{method}: wrong status code");
    }
    handler.close().await;
}

// ── Handler unit tests ────────────────────────────────────────────────────────

#[tokio::test]
async fn test_is_full_access_user() {
    let handler = Handler::new(
        LiveKitAuth {
            key: "testKey".into(),
            secret: "testSecret".into(),
            lk_url: "wss://lk.local:8080/foo".into(),
        },
        true,
        vec!["example.com".into(), "another.example.com".into()],
        Duration::ZERO,
        HashMap::new(),
        None,
        Arc::new(HandlerTestDeps::default()),
    );
    for (server, want) in [
        ("example.com", true),
        ("another.example.com", true),
        ("aanother.example.com", false),
        ("matrix.example.com", false),
    ] {
        assert_eq!(
            handler.is_full_access_user(server),
            want,
            "is_full_access_user({server:?})"
        );
    }
    handler.close().await;

    // With the wildcard as only entry, any server is a full-access user.
    let wildcard_handler = new_handler_with(HandlerTestDeps::default(), &["*"], None);
    assert!(
        wildcard_handler.is_full_access_user("other.com"),
        "expected wildcard to grant full access"
    );
    wildcard_handler.close().await;
}

#[tokio::test]
async fn test_get_join_token() {
    let token_string = get_join_token(
        "testKey",
        "testSecret",
        &LiveKitRoomAlias("testRoom".into()),
        &LiveKitIdentity("testIdentity@example.com".into()),
    )
    .expect("unexpected error");
    assert!(!token_string.is_empty(), "expected token to be non-empty");

    let claims = parse_jwt_claims(&token_string, "testSecret");
    assert_ne!(
        claims["video"]["roomCreate"],
        serde_json::Value::Bool(true),
        "roomCreate must be false"
    );
}

// ── Handler loop internals ────────────────────────────────────────────────────
// These tests exercise the loop directly via add_delayed_event_job and the
// sfu_event channel.

/// Verifies that close() terminates the loop cleanly.
#[tokio::test]
async fn test_handler_close() {
    let handler = new_handler_with(HandlerTestDeps::default(), &["*"], None);
    tokio::time::timeout(Duration::from_secs(3), handler.close())
        .await
        .expect("Handler close() timed out");
}

/// Exercises add_delayed_event_job through the full loop path.
#[tokio::test]
async fn test_handler_add_delayed_event_job() {
    let deps = HandlerTestDeps {
        exists: exists_block_until_cancelled(),
        ..Default::default()
    };
    let handler = new_handler_with(deps, &["example.com"], None);

    handler
        .add_delayed_event_job(DelayedEventJobParams {
            server_name: "example.com".into(),
            delay_id: "delay-id".into(),
            delay_timeout: Duration::from_secs(10),
            livekit_room: LiveKitRoomAlias("test-room".into()),
            livekit_identity: LiveKitIdentity("@user:example.com".into()),
        })
        .await
        .expect("add_delayed_event_job");
    // No panic or deadlock = success.
    handler.close().await;
}

/// Verifies that the loop cleans up a job after it signals done, exercising
/// the full Connected → Disconnected → cleanup path.
#[tokio::test]
async fn test_handler_loop_no_jobs_left() {
    let resolve_called = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let exec_called = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let resolve_called_clone = resolve_called.clone();
    let exec_called_clone = exec_called.clone();

    let deps = HandlerTestDeps {
        exists: exists_block_until_cancelled(),
        resolve: Some(Box::new(move |_| {
            resolve_called_clone.store(true, std::sync::atomic::Ordering::SeqCst);
            Ok(CsApiUrl("https://matrix-client.example.com".into()))
        })),
        exec: Some(Box::new(move |_, _, _| {
            exec_called_clone.store(true, std::sync::atomic::Ordering::SeqCst);
            Ok(200)
        })),
        ..Default::default()
    };
    let handler = new_handler_with(deps, &["example.com"], Some(new_in_memory_store()));

    let room = LiveKitRoomAlias("loop-test-room".into());
    let identity = LiveKitIdentity("@loopuser:example.com".into());

    handler
        .add_delayed_event_job(DelayedEventJobParams {
            server_name: "example.com".into(),
            delay_id: "loop-delay-id".into(),
            delay_timeout: Duration::from_secs(10),
            livekit_room: room.clone(),
            livekit_identity: identity.clone(),
        })
        .await
        .expect("add_delayed_event_job");

    tokio::time::sleep(Duration::from_millis(30)).await;
    handler
        .sfu_event_tx
        .send(SfuEventRequest {
            room_alias: room.clone(),
            msg: SfuMessage {
                signal: DelayedEventSignal::ParticipantConnected,
                livekit_identity: identity.clone(),
            },
        })
        .await
        .unwrap();
    tokio::time::sleep(Duration::from_millis(30)).await;
    handler
        .sfu_event_tx
        .send(SfuEventRequest {
            room_alias: room.clone(),
            msg: SfuMessage {
                signal: DelayedEventSignal::ParticipantDisconnectedIntentionally,
                livekit_identity: identity.clone(),
            },
        })
        .await
        .unwrap();
    tokio::time::sleep(Duration::from_millis(200)).await;
    assert!(
        resolve_called.load(std::sync::atomic::Ordering::SeqCst),
        "expected CS API resolution to be called"
    );
    assert!(
        exec_called.load(std::sync::atomic::Ordering::SeqCst),
        "expected delayed event action to be called"
    );
    // No assertion beyond no deadlock/panic — the loop cleans up internally.
    handler.close().await;
}

// ── close() timeout branch ────────────────────────────────────────────────────

/// Verifies that close() waits on the loop-done signal: with a loop that
/// never runs, close() blocks until the signal is flipped manually, then
/// returns promptly.
#[tokio::test]
async fn test_handler_close_timeout() {
    // Build a Handler whose loop is never started so loop_done never flips on
    // its own.
    let (handler, _rx) = Handler::new_without_loop(
        default_auth(),
        false,
        vec!["*".into()],
        Duration::ZERO,
        HashMap::new(),
        None,
        Arc::new(HandlerTestDeps::default()),
    );

    let closer = handler.clone();
    let done = tokio::spawn(async move { closer.close().await });

    // With a 10 s timeout inside close(), we can't wait that long in a test.
    // Instead we verify the wait path is exercised and then unblock it by
    // flipping loop_done ourselves after a short delay.
    tokio::time::sleep(Duration::from_millis(50)).await;
    assert!(
        !done.is_finished(),
        "close() returned before loop_done was set"
    );
    let _ = handler.loop_done_sender().send(true); // unblock close()
    tokio::time::timeout(Duration::from_secs(3), done)
        .await
        .expect("close() did not return after loop_done was set")
        .unwrap();
}

// ── add_delayed_event_job: post-shutdown branch ───────────────────────────────

/// Verifies that add_delayed_event_job returns immediately (via the
/// cancellation branch) when the handler has already been shut down.
#[tokio::test]
async fn test_handler_add_delayed_event_job_after_shutdown() {
    let handler = new_handler_with(
        HandlerTestDeps::default(),
        &["*"],
        Some(new_in_memory_store()),
    );
    handler.close().await; // shut down the loop so the handler is cancelled

    // Should return the shutdown error without blocking even though the loop
    // is gone.
    let add = handler.add_delayed_event_job(DelayedEventJobParams {
        server_name: "example.com".into(),
        delay_id: "id".into(),
        delay_timeout: Duration::from_secs(10),
        livekit_room: LiveKitRoomAlias("room".into()),
        livekit_identity: LiveKitIdentity("@user:example.com".into()),
    });
    match tokio::time::timeout(Duration::from_secs(3), add).await {
        Ok(Err(AddJobError::Shutdown)) => {}
        Ok(other) => panic!("expected shutdown error after shutdown, got {other:?}"),
        Err(_) => panic!("add_delayed_event_job blocked after shutdown"),
    }
}

// ── sfu_event_from_webhook (extracted routing logic) ──────────────────────────

/// Verifies that a participant_joined event produces a ParticipantConnected
/// SfuMessage.
#[test]
fn test_sfu_event_from_webhook_participant_joined() {
    let event = livekit_protocol::WebhookEvent {
        event: "participant_joined".into(),
        room: Some(livekit_protocol::Room {
            name: "test-room".into(),
            ..Default::default()
        }),
        participant: Some(livekit_protocol::ParticipantInfo {
            identity: "@alice:example.com".into(),
            ..Default::default()
        }),
        ..Default::default()
    };
    let (alias, msg) =
        sfu_event_from_webhook(&event).expect("expected Some for participant_joined");
    assert_eq!(
        alias,
        LiveKitRoomAlias("test-room".into()),
        "unexpected room alias"
    );
    assert_eq!(msg.signal, DelayedEventSignal::ParticipantConnected);
    assert_eq!(
        msg.livekit_identity,
        LiveKitIdentity("@alice:example.com".into())
    );
}

/// Verifies that a client-initiated disconnect produces
/// ParticipantDisconnectedIntentionally.
#[test]
fn test_sfu_event_from_webhook_participant_left_client_initiated() {
    for event_type in ["participant_left", "participant_connection_aborted"] {
        let event = livekit_protocol::WebhookEvent {
            event: event_type.into(),
            room: Some(livekit_protocol::Room {
                name: "test-room".into(),
                ..Default::default()
            }),
            participant: Some(livekit_protocol::ParticipantInfo {
                identity: "@bob:example.com".into(),
                disconnect_reason: livekit_protocol::DisconnectReason::ClientInitiated as i32,
                ..Default::default()
            }),
            ..Default::default()
        };
        let (alias, msg) = sfu_event_from_webhook(&event)
            .unwrap_or_else(|| panic!("expected Some for {event_type}"));
        assert_eq!(
            alias,
            LiveKitRoomAlias("test-room".into()),
            "unexpected room alias"
        );
        assert_eq!(
            msg.signal,
            DelayedEventSignal::ParticipantDisconnectedIntentionally,
            "{event_type}: expected ParticipantDisconnectedIntentionally"
        );
        assert_eq!(
            msg.livekit_identity,
            LiveKitIdentity("@bob:example.com".into())
        );
    }
}

/// Verifies that a non-client-initiated disconnect produces
/// ParticipantConnectionAborted.
#[test]
fn test_sfu_event_from_webhook_participant_left_non_client_reason() {
    let event = livekit_protocol::WebhookEvent {
        event: "participant_left".into(),
        room: Some(livekit_protocol::Room {
            name: "test-room".into(),
            ..Default::default()
        }),
        participant: Some(livekit_protocol::ParticipantInfo {
            identity: "@carol:example.com".into(),
            disconnect_reason: livekit_protocol::DisconnectReason::ServerShutdown as i32,
            ..Default::default()
        }),
        ..Default::default()
    };
    let (alias, msg) = sfu_event_from_webhook(&event).expect("expected Some for participant_left");
    assert_eq!(
        alias,
        LiveKitRoomAlias("test-room".into()),
        "unexpected room alias"
    );
    assert_eq!(msg.signal, DelayedEventSignal::ParticipantConnectionAborted);
    assert_eq!(
        msg.livekit_identity,
        LiveKitIdentity("@carol:example.com".into())
    );
}

/// Verifies that unknown event types return None and are not routed.
#[test]
fn test_sfu_event_from_webhook_unknown_event() {
    for event_type in ["room_started", "room_finished", "track_published", ""] {
        let event = livekit_protocol::WebhookEvent {
            event: event_type.into(),
            room: Some(livekit_protocol::Room {
                name: "room".into(),
                ..Default::default()
            }),
            participant: Some(livekit_protocol::ParticipantInfo {
                identity: "id".into(),
                ..Default::default()
            }),
            ..Default::default()
        };
        assert!(
            sfu_event_from_webhook(&event).is_none(),
            "expected None for event type {event_type:?}"
        );
    }
}

// ── handle_sfu_webhook (HTTP entry-point) ─────────────────────────────────────

/// Builds a POST /sfu_webhook request signed using the LiveKit webhook
/// protocol: JSON-encoded body, SHA-256 of the body embedded in an
/// AccessToken JWT in the Authorization header. Mirrors the URLNotifier's
/// signing logic so the webhook receiver accepts it.
fn signed_sfu_webhook_request(
    key: &str,
    secret: &str,
    event: &livekit_protocol::WebhookEvent,
) -> http::Request<Body> {
    let body = serde_json::to_string(event).expect("event serialization");
    let sum = sha2::Sha256::digest(body.as_bytes());
    let token = AccessToken::with_api_key(key, secret)
        .with_ttl(Duration::from_secs(5 * 60))
        .with_sha256(&BASE64_STANDARD.encode(sum))
        .to_jwt()
        .expect("to_jwt");
    http::Request::builder()
        .method("POST")
        .uri("/sfu_webhook")
        .header(header::AUTHORIZATION, token)
        .body(Body::from(body))
        .unwrap()
}

/// Builds a Handler with just enough wiring to serve /sfu_webhook directly —
/// the loop is NOT started so the test can observe the sfu_event channel
/// without competing with the actor.
fn new_sfu_webhook_test_handler(key: &str, secret: &str) -> (Arc<Handler>, LoopReceivers) {
    Handler::new_without_loop(
        LiveKitAuth {
            key: key.into(),
            secret: secret.into(),
            lk_url: String::new(),
        },
        false,
        vec![],
        Duration::ZERO,
        HashMap::new(),
        None,
        Arc::new(HandlerTestDeps::default()),
    )
}

/// Verifies that a properly signed participant_joined webhook is parsed and
/// routed to the sfu_event channel as a ParticipantConnected message.
#[tokio::test]
async fn test_handle_sfu_webhook_participant_joined() {
    const KEY: &str = "test-key";
    const SECRET: &str = "test-secret";
    let (handler, mut rx) = new_sfu_webhook_test_handler(KEY, SECRET);

    let event = livekit_protocol::WebhookEvent {
        event: "participant_joined".into(),
        room: Some(livekit_protocol::Room {
            name: "test-room".into(),
            ..Default::default()
        }),
        participant: Some(livekit_protocol::ParticipantInfo {
            identity: "@alice:example.com".into(),
            ..Default::default()
        }),
        ..Default::default()
    };
    let req = signed_sfu_webhook_request(KEY, SECRET, &event);
    let _ = send_request(&handler, req).await;

    match rx.sfu_event_rx.try_recv() {
        Ok(ev) => {
            assert_eq!(ev.room_alias, LiveKitRoomAlias("test-room".into()));
            assert_eq!(ev.msg.signal, DelayedEventSignal::ParticipantConnected);
            assert_eq!(
                ev.msg.livekit_identity,
                LiveKitIdentity("@alice:example.com".into())
            );
        }
        Err(_) => panic!("expected event on the sfu_event channel, got none"),
    }
}

/// Verifies that a request without a valid signature (e.g. missing
/// Authorization header) is silently dropped — the handler returns and no
/// event reaches the sfu_event channel.
#[tokio::test]
async fn test_handle_sfu_webhook_auth_failure() {
    let (handler, mut rx) = new_sfu_webhook_test_handler("test-key", "test-secret");

    // No Authorization header → webhook verification fails fast.
    let req = http::Request::builder()
        .method("POST")
        .uri("/sfu_webhook")
        .body(Body::from("{}"))
        .unwrap();
    let _ = send_request(&handler, req).await;

    if let Ok(ev) = rx.sfu_event_rx.try_recv() {
        panic!("unauthenticated request was routed: {ev:?}");
    }
}

/// Verifies that webhook event types which sfu_event_from_webhook does not
/// translate (e.g. room_started) are silently dropped before reaching the
/// sfu_event channel.
#[tokio::test]
async fn test_handle_sfu_webhook_unknown_event() {
    const KEY: &str = "test-key";
    const SECRET: &str = "test-secret";
    let (handler, mut rx) = new_sfu_webhook_test_handler(KEY, SECRET);

    let event = livekit_protocol::WebhookEvent {
        event: "room_started".into(),
        room: Some(livekit_protocol::Room {
            name: "test-room".into(),
            ..Default::default()
        }),
        ..Default::default()
    };
    let req = signed_sfu_webhook_request(KEY, SECRET, &event);
    let _ = send_request(&handler, req).await;

    if let Ok(ev) = rx.sfu_event_rx.try_recv() {
        panic!("non-routable event reached the sfu_event channel: {ev:?}");
    }
}

/// Covers the cancellation branch of the routing select: when the handler is
/// shut down and the sfu_event channel is full, the inbound webhook must not
/// block — the send is abandoned via cancellation and handle_sfu_webhook
/// returns promptly.
#[tokio::test]
async fn test_handle_sfu_webhook_shutdown_drop() {
    const KEY: &str = "test-key";
    const SECRET: &str = "test-secret";
    let (handler, _rx) = new_sfu_webhook_test_handler(KEY, SECRET);
    handler.cancel.cancel(); // cancelled immediately

    // Fill the sfu_event channel so the send arm of the select is never ready
    // — forcing the cancellation branch to win.
    while handler
        .sfu_event_tx
        .try_send(SfuEventRequest::default())
        .is_ok()
    {}

    let event = livekit_protocol::WebhookEvent {
        event: "participant_joined".into(),
        room: Some(livekit_protocol::Room {
            name: "shutdown-room".into(),
            ..Default::default()
        }),
        participant: Some(livekit_protocol::ParticipantInfo {
            identity: "@x".into(),
            ..Default::default()
        }),
        ..Default::default()
    };
    let req = signed_sfu_webhook_request(KEY, SECRET, &event);

    tokio::time::timeout(Duration::from_secs(3), send_request(&handler, req))
        .await
        .expect("handle_sfu_webhook blocked after handler shutdown");
}

// ── Handler loop job lifecycle ────────────────────────────────────────────────

/// Verifies that handler.close() waits for ALL participant-lookup tasks to
/// fully exit before returning. This prevents races on the mocked deps
/// between lookup tasks and test cleanup.
#[tokio::test]
async fn test_handler_loop_all_jobs_closed_on_shutdown() {
    let exited: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));

    /// Records the room when the pending lookup future is dropped — which
    /// happens exactly when the lookup task observes cancellation (the Go
    /// mock's `<-ctx.Done()` equivalent).
    struct RecordExit {
        room: String,
        exited: Arc<Mutex<Vec<String>>>,
    }
    impl Drop for RecordExit {
        fn drop(&mut self) {
            self.exited.lock().unwrap().push(self.room.clone());
        }
    }

    let exited_clone = exited.clone();
    let deps = HandlerTestDeps {
        exists: Some(Box::new(move |room, _| {
            let guard = RecordExit {
                room: room.0.clone(),
                exited: exited_clone.clone(),
            };
            Box::pin(async move {
                let _guard = guard;
                std::future::pending().await
            })
        })),
        ..Default::default()
    };
    let handler = new_handler_with(deps, &["example.com"], Some(new_in_memory_store()));

    // Start three jobs in three different rooms — each spawns a lookup task.
    let rooms = ["room-alpha", "room-beta", "room-gamma"];
    for room in rooms {
        handler
            .add_delayed_event_job(DelayedEventJobParams {
                server_name: "example.com".into(),
                delay_id: "delay-id".into(),
                delay_timeout: Duration::from_secs(10),
                livekit_room: LiveKitRoomAlias(room.into()),
                livekit_identity: LiveKitIdentity("@user:example.com".into()),
            })
            .await
            .unwrap_or_else(|e| panic!("add_delayed_event_job({room}): {e:?}"));
    }

    tokio::time::sleep(Duration::from_millis(50)).await;

    // close() must block until all three lookup tasks have exited.
    handler.close().await;

    let exited = exited.lock().unwrap();
    assert_eq!(
        exited.len(),
        rooms.len(),
        "expected {} lookup tasks to have exited, got {}: {exited:?}",
        rooms.len(),
        exited.len()
    );
}

/// Verifies the job-done path: after a job finishes and signals done, the job
/// is removed from the map AND its cleanup completes before handler.close()
/// returns.
#[tokio::test]
async fn test_handler_loop_done_ch_cleanup_before_handler_close() {
    let resolve_called = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let exec_called = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let resolve_called_clone = resolve_called.clone();
    let exec_called_clone = exec_called.clone();

    let deps = HandlerTestDeps {
        exists: exists_block_until_cancelled(),
        resolve: Some(Box::new(move |_| {
            resolve_called_clone.store(true, std::sync::atomic::Ordering::SeqCst);
            Ok(CsApiUrl("https://matrix-client.example.com".into()))
        })),
        exec: Some(Box::new(move |_, _, _| {
            exec_called_clone.store(true, std::sync::atomic::Ordering::SeqCst);
            Ok(200)
        })),
        ..Default::default()
    };
    let handler = new_handler_with(deps, &["example.com"], Some(new_in_memory_store()));

    let room = LiveKitRoomAlias("donech-shutdown-room".into());
    let identity = LiveKitIdentity("@user:example.com".into());

    handler
        .add_delayed_event_job(DelayedEventJobParams {
            server_name: "example.com".into(),
            delay_id: "delay-id".into(),
            delay_timeout: Duration::from_secs(10),
            livekit_room: room.clone(),
            livekit_identity: identity.clone(),
        })
        .await
        .expect("add_delayed_event_job");

    // Drive the job to Disconnected → ActionSend runs → done signal → job
    // removed.
    tokio::time::sleep(Duration::from_millis(30)).await;
    handler
        .sfu_event_tx
        .send(SfuEventRequest {
            room_alias: room.clone(),
            msg: SfuMessage {
                signal: DelayedEventSignal::ParticipantConnected,
                livekit_identity: identity.clone(),
            },
        })
        .await
        .unwrap();
    tokio::time::sleep(Duration::from_millis(30)).await;
    handler
        .sfu_event_tx
        .send(SfuEventRequest {
            room_alias: room.clone(),
            msg: SfuMessage {
                signal: DelayedEventSignal::ParticipantDisconnectedIntentionally,
                livekit_identity: identity.clone(),
            },
        })
        .await
        .unwrap();

    // Wait for the done signal to be processed (job removed from the map).
    tokio::time::sleep(Duration::from_millis(200)).await;

    // handler.close() must complete cleanly — the job's background tracking
    // ensures the cleanup spawned from done handling has also finished.
    tokio::time::timeout(Duration::from_secs(5), handler.close())
        .await
        .expect("handler.close() blocked after job done — background tasks not properly tracked");

    assert!(
        resolve_called.load(std::sync::atomic::Ordering::SeqCst),
        "expected CS API resolution to be called"
    );
    assert!(
        exec_called.load(std::sync::atomic::Ordering::SeqCst),
        "expected delayed event action to be called"
    );
}

/// Verifies that replacing a job for the same (room, identity) slot and then
/// calling handler.close() does not deadlock. A stale done signal from the
/// old job must be ignored (pointer equality check in the loop).
#[tokio::test]
async fn test_handler_loop_job_replacement_no_deadlock() {
    let deps = HandlerTestDeps {
        exists: exists_block_until_cancelled(),
        ..Default::default()
    };
    let handler = new_handler_with(deps, &["example.com"], Some(new_in_memory_store()));

    let room = LiveKitRoomAlias("replacement-room".into());
    let identity = LiveKitIdentity("@same-user:example.com".into());

    // Create first job.
    handler
        .add_delayed_event_job(DelayedEventJobParams {
            server_name: "example.com".into(),
            delay_id: "delay-1".into(),
            delay_timeout: Duration::from_secs(10),
            livekit_room: room.clone(),
            livekit_identity: identity.clone(),
        })
        .await
        .expect("add_delayed_event_job(delay-1)");
    tokio::time::sleep(Duration::from_millis(20)).await;

    // Replace with second job for the same identity — first job gets
    // JobReplaced.
    handler
        .add_delayed_event_job(DelayedEventJobParams {
            server_name: "example.com".into(),
            delay_id: "delay-2".into(),
            delay_timeout: Duration::from_secs(10),
            livekit_room: room.clone(),
            livekit_identity: identity.clone(),
        })
        .await
        .expect("add_delayed_event_job(delay-2)");
    tokio::time::sleep(Duration::from_millis(100)).await;

    tokio::time::timeout(Duration::from_secs(5), handler.close())
        .await
        .expect("handler.close() deadlocked after job replacement");
}

// ── store recovery (handler_restore_test.go) ──────────────────────────────────

#[tokio::test]
async fn test_handler_restore_resumes_saved_job() {
    // Set up the store with a single saved job.
    let room = LiveKitRoomAlias("test-room".into());
    let identity = LiveKitIdentity("@user:example.com".into());
    let key = JobKey {
        room: room.clone(),
        identity: identity.clone(),
    };
    let (store, _saved_rx, mut deleted_rx) = new_notifying_store();
    store
        .save_job(
            &key,
            &StoredJob {
                params: DelayedEventJobParams {
                    delay_id: "restore-delay-id".into(),
                    server_name: "example.com".into(),
                    delay_timeout: Duration::from_secs(30),
                    livekit_room: room.clone(),
                    livekit_identity: identity.clone(),
                },
                restarted_at: Utc::now(),
            },
        )
        .await
        .unwrap();

    // Block the restored job on phase one so that we can emit our own events
    // for the test. Mock all delayed event requests to succeed.
    let deps = HandlerTestDeps {
        exists: exists_block_until_cancelled(),
        exec: exec_ok(),
        resolve: Some(Box::new(|_| {
            Ok(CsApiUrl("https://matrix-client.example.com".into()))
        })),
        ..Default::default()
    };

    // Kick off the handler.
    let handler = new_handler_with(deps, &["example.com"], Some(store.clone()));

    // Wait for startup recovery to complete; our job should still be in the
    // store.
    wait_recovery_done(&handler).await;
    let jobs = store
        .all_jobs()
        .await
        .expect("could not load jobs from store");
    assert_eq!(jobs.len(), 1, "expected 1 job in the store");

    // Simulate a connect and disconnect cycle for the LiveKit identity.
    handler
        .sfu_event_tx
        .send(SfuEventRequest {
            room_alias: room.clone(),
            msg: SfuMessage {
                signal: DelayedEventSignal::ParticipantConnected,
                livekit_identity: identity.clone(),
            },
        })
        .await
        .unwrap();
    handler
        .sfu_event_tx
        .send(SfuEventRequest {
            room_alias: room.clone(),
            msg: SfuMessage {
                signal: DelayedEventSignal::ParticipantDisconnectedIntentionally,
                livekit_identity: identity.clone(),
            },
        })
        .await
        .unwrap();

    // Wait for the job to finish and be deleted from the store.
    match tokio::time::timeout(Duration::from_secs(3), deleted_rx.recv()).await {
        Ok(Some(actual_key)) => assert_eq!(actual_key, key, "unexpected delete key"),
        _ => panic!("timed out waiting for job to be deleted from store"),
    }
    handler.close().await;
}

#[tokio::test]
async fn test_handler_restore_purges_expired_jobs() {
    // Set up the store with an expired job.
    let room = LiveKitRoomAlias("test-room".into());
    let identity = LiveKitIdentity("@user:example.com".into());
    let key = JobKey {
        room: room.clone(),
        identity: identity.clone(),
    };
    let store = new_in_memory_store();
    store
        .save_job(
            &key,
            &StoredJob {
                params: DelayedEventJobParams {
                    delay_id: "expired-delay-id".into(),
                    server_name: "example.com".into(),
                    delay_timeout: Duration::from_secs(1),
                    livekit_room: room.clone(),
                    livekit_identity: identity.clone(),
                },
                restarted_at: Utc::now() - chrono::Duration::seconds(2),
            },
        )
        .await
        .unwrap();

    // The default (un-mocked) deps panic when participant_exists or
    // execute_delayed_event_action are called — verifying neither happens.
    let handler = new_handler_with(
        HandlerTestDeps::default(),
        &["example.com"],
        Some(store.clone()),
    );

    // Wait for startup recovery to complete; our job should have been deleted
    // from the store.
    wait_recovery_done(&handler).await;
    let jobs = store
        .all_jobs()
        .await
        .expect("could not load jobs from store");
    assert_eq!(jobs.len(), 0, "expected 0 jobs in the store");
    handler.close().await;
}

#[tokio::test]
async fn test_handler_restore_gracefully_ignores_store_errors() {
    // Block the job to be added on phase one so that it doesn't go off and do
    // anything for the sake of the test.
    let deps = HandlerTestDeps {
        exists: exists_block_until_cancelled(),
        ..Default::default()
    };

    // Kick off the handler with a store that always fails.
    let handler = new_handler_with(deps, &["example.com"], Some(Arc::new(FailingStore)));

    // The handler should still accept new jobs.
    handler
        .add_delayed_event_job(DelayedEventJobParams {
            server_name: "example.com".into(),
            delay_id: "new-delay-id".into(),
            delay_timeout: Duration::from_secs(10),
            livekit_room: LiveKitRoomAlias("error-recovery-room".into()),
            livekit_identity: LiveKitIdentity("@user:example.com:device:member".into()),
        })
        .await
        .expect("add_delayed_event_job failed");
    handler.close().await;
}

#[tokio::test]
async fn test_handler_restore_gracefully_ignores_missing_store() {
    // Block the job to be added on phase one so that it doesn't go off and do
    // anything for the sake of the test.
    let deps = HandlerTestDeps {
        exists: exists_block_until_cancelled(),
        ..Default::default()
    };

    // Kick off the handler without a store.
    let handler = new_handler_with(deps, &["example.com"], None);

    // The handler should still accept new jobs.
    handler
        .add_delayed_event_job(DelayedEventJobParams {
            server_name: "example.com".into(),
            delay_id: "new-delay-id".into(),
            delay_timeout: Duration::from_secs(10),
            livekit_room: LiveKitRoomAlias("error-recovery-room".into()),
            livekit_identity: LiveKitIdentity("@user:example.com:device:member".into()),
        })
        .await
        .expect("add_delayed_event_job failed");
    handler.close().await;
}

#[tokio::test]
async fn test_handler_restore_saves_new_jobs() {
    // Set up an empty store.
    let (store, mut saved_rx, mut deleted_rx) = new_notifying_store();

    // Block the job to be added on phase one so that we can emit our own
    // events for the test. Mock all delayed event requests to succeed.
    let deps = HandlerTestDeps {
        exists: exists_block_until_cancelled(),
        exec: exec_ok(),
        resolve: Some(Box::new(|_| {
            Ok(CsApiUrl("https://matrix-client.example.com".into()))
        })),
        ..Default::default()
    };

    // Kick off the handler.
    let handler = new_handler_with(deps, &["example.com"], Some(store.clone()));

    // Wait for startup recovery to complete; the store should be empty.
    wait_recovery_done(&handler).await;
    let jobs = store
        .all_jobs()
        .await
        .expect("could not load jobs from store");
    assert_eq!(jobs.len(), 0, "expected 0 jobs in the store");

    // Add a new job.
    let room = LiveKitRoomAlias("test-room".into());
    let identity = LiveKitIdentity("@user:example.com".into());
    let key = JobKey {
        room: room.clone(),
        identity: identity.clone(),
    };
    handler
        .add_delayed_event_job(DelayedEventJobParams {
            server_name: "example.com".into(),
            delay_id: "new-delay-id".into(),
            delay_timeout: Duration::from_secs(10),
            livekit_room: room.clone(),
            livekit_identity: identity.clone(),
        })
        .await
        .expect("add_delayed_event_job failed");

    // Wait for the job to be saved into the store.
    match tokio::time::timeout(Duration::from_secs(3), saved_rx.recv()).await {
        Ok(Some(actual_key)) => assert_eq!(actual_key, key, "unexpected save key"),
        _ => panic!("timed out waiting for job to be saved into the store"),
    }

    // Simulate a connect and disconnect cycle for the LiveKit identity.
    handler
        .sfu_event_tx
        .send(SfuEventRequest {
            room_alias: room.clone(),
            msg: SfuMessage {
                signal: DelayedEventSignal::ParticipantConnected,
                livekit_identity: identity.clone(),
            },
        })
        .await
        .unwrap();
    handler
        .sfu_event_tx
        .send(SfuEventRequest {
            room_alias: room.clone(),
            msg: SfuMessage {
                signal: DelayedEventSignal::ParticipantDisconnectedIntentionally,
                livekit_identity: identity.clone(),
            },
        })
        .await
        .unwrap();

    // Wait for the job to finish and be deleted from the store.
    match tokio::time::timeout(Duration::from_secs(3), deleted_rx.recv()).await {
        Ok(Some(actual_key)) => assert_eq!(actual_key, key, "unexpected delete key"),
        _ => panic!("timed out waiting for job to be deleted from store"),
    }
    handler.close().await;
}

// ── /get_token endpoint (get_token_test.go) ───────────────────────────────────

/// Verifies that non-POST/OPTIONS requests to /get_token return 405.
#[tokio::test]
async fn test_handle_get_token_method_not_allowed() {
    let handler = new_get_token_handler(HandlerTestDeps::default());
    for method in ["GET", "PUT", "DELETE", "PATCH"] {
        let req = http::Request::builder()
            .method(method)
            .uri("/get_token")
            .body(Body::empty())
            .unwrap();
        let resp = send_request(&handler, req).await;
        assert_eq!(
            resp.status(),
            StatusCode::METHOD_NOT_ALLOWED,
            "{method} /get_token: expected 405"
        );
    }
    handler.close().await;
}

/// Verifies that OPTIONS /get_token returns 200.
#[tokio::test]
async fn test_handle_get_token_options() {
    let handler = new_get_token_handler(HandlerTestDeps::default());
    let req = http::Request::builder()
        .method("OPTIONS")
        .uri("/get_token")
        .body(Body::empty())
        .unwrap();
    let resp = send_request(&handler, req).await;
    assert_eq!(resp.status(), StatusCode::OK, "expected 200 for OPTIONS");
    handler.close().await;
}

/// Verifies that malformed JSON to /get_token returns 400.
#[tokio::test]
async fn test_handle_get_token_invalid_json() {
    let handler = new_get_token_handler(HandlerTestDeps::default());
    let resp = send_request(&handler, post_json("/get_token", "{invalid json}".into())).await;
    assert_eq!(
        resp.status(),
        StatusCode::BAD_REQUEST,
        "expected 400 for invalid JSON"
    );
    handler.close().await;
}

/// Verifies the full-access happy-path POST /get_token: a valid request
/// produces a 200 SfuResponse, create_livekit_room is invoked, and the JWT's
/// sub/room claims match the MSC-4195 test vectors.
///
/// The OpenID exchange and LiveKit room creation are mocked here — the real
/// exchange path is exercised by test_exchange_openid_userinfo_success in
/// helper.rs.
#[tokio::test]
async fn test_handle_get_token_success() {
    const MATRIX_SERVER_NAME: &str = "example.com";
    const CLAIMED_USER_ID: &str = "@alice:example.com";

    let create_called = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let create_called_clone = create_called.clone();
    let deps = HandlerTestDeps {
        exchange: exchange_ok(CLAIMED_USER_ID),
        create_room: Some(Box::new(move |_, _, _| {
            create_called_clone.store(true, std::sync::atomic::Ordering::SeqCst);
            Ok(())
        })),
        ..Default::default()
    };
    let handler = new_get_token_handler(deps);

    // Inputs match the MSC-4195 test vectors exactly:
    // https://github.com/hughns/matrix-spec-proposals/blob/hughns/matrixrtc-livekit/proposals/4195-matrixrtc-livekit.md#test-vectors
    let body = marshal_sfu_request(|r| {
        r.room_id = "!roomid:example.com".into();
        r.slot_id = "slot1234".into();
        r.openid_token.matrix_server_name = MATRIX_SERVER_NAME.into();
        r.member.id = "memberABC".into();
        r.member.claimed_user_id = CLAIMED_USER_ID.into();
        r.member.claimed_device_id = "DEVICE123".into();
    });
    let resp = send_request(&handler, post_json("/get_token", body)).await;
    assert_eq!(resp.status(), StatusCode::OK, "status");
    assert!(
        create_called.load(std::sync::atomic::Ordering::SeqCst),
        "expected create_livekit_room to be called for full-access user"
    );

    let body = body_bytes(resp).await;
    let sfu_response: SfuResponse =
        serde_json::from_slice(&body).expect("failed to decode response body");
    assert_eq!(sfu_response.url, handler.livekit_auth.lk_url, "resp.url");
    assert!(!sfu_response.jwt.is_empty(), "expected JWT to be non-empty");

    let claims = parse_jwt_claims(&sfu_response.jwt, &handler.livekit_auth.secret);

    // MSC-4195 identity hash of (claimed_user_id, claimed_device_id,
    // member.id).
    let want_sub_raw = serde_json::to_vec(&[CLAIMED_USER_ID, "DEVICE123", "memberABC"]).unwrap();
    let want_sub_hash = sha2::Sha256::digest(&want_sub_raw);
    let want_sub = base64::engine::general_purpose::STANDARD_NO_PAD.encode(want_sub_hash);
    assert_eq!(claims["sub"], serde_json::Value::String(want_sub), "sub");
    // MSC-4195 room hash of ("!roomid:example.com", "slot1234").
    const WANT_ROOM: &str = "O8437W3+jmzMVjoIP3tNwbm+XxHQk2iKpOA7aqw3qSc";
    assert_eq!(
        claims["video"]["room"],
        serde_json::Value::String(WANT_ROOM.into()),
        "room"
    );
    handler.close().await;
}

/// Verifies that a mismatch between the OpenID-validated sub and the
/// claimed_user_id in the request body returns 401.
#[tokio::test]
async fn test_handle_get_token_unauthorized_user() {
    let deps = HandlerTestDeps {
        exchange: exchange_ok("@real:example.com"),
        ..Default::default()
    };
    let handler = new_get_token_handler(deps);
    let body = marshal_sfu_request(|r| {
        r.member.claimed_user_id = "@attacker:example.com".into(); // mismatch vs sub
    });
    let resp = send_request(&handler, post_json("/get_token", body)).await;
    assert_eq!(
        resp.status(),
        StatusCode::UNAUTHORIZED,
        "expected 401 for mismatched user"
    );
    handler.close().await;
}

/// Verifies that a request from a homeserver that is not on the full-access
/// list, with delayed-event delegation params, returns 400 / M_BAD_JSON.
/// Delegation is gated on full-access; restricted users may join existing
/// rooms but not delegate delayed-event handling.
#[tokio::test]
async fn test_handle_get_token_restricted_user() {
    let deps = HandlerTestDeps {
        exchange: exchange_ok("@user:restricted.com"),
        ..Default::default()
    };
    // new_get_token_handler configures full access only for example.com.
    let handler = new_get_token_handler(deps);
    let body = marshal_sfu_request(|r| {
        r.member.claimed_user_id = "@user:restricted.com".into();
        r.openid_token.matrix_server_name = "restricted.com".into(); // not in full-access list
                                                                     // Delegation params trigger the restricted-user reject path.
        r.delay_id = "delay-id".into();
        r.delay_timeout = 30000; // 30 s in ms
        r.delay_cs_api_url = "https://restricted.com".into();
    });
    let resp = send_request(&handler, post_json("/get_token", body)).await;
    assert_eq!(
        resp.status(),
        StatusCode::BAD_REQUEST,
        "expected 400 for restricted user requesting delegation"
    );
    handler.close().await;
}

/// Verifies that an OpenID lookup failure surfaces as 401 / M_UNAUTHORIZED —
/// the request couldn't be authorised.
#[tokio::test]
async fn test_handle_get_token_exchange_error() {
    let deps = HandlerTestDeps {
        exchange: Some(Box::new(|_, _| Err("M_UNAUTHORIZED: no".into()))),
        ..Default::default()
    };
    let handler = new_get_token_handler(deps);
    let resp = send_request(
        &handler,
        post_json("/get_token", marshal_sfu_request(|_| {})),
    )
    .await;
    assert_eq!(
        resp.status(),
        StatusCode::UNAUTHORIZED,
        "expected 401 when OpenID exchange fails"
    );
    handler.close().await;
}

#[tokio::test]
async fn test_handle_get_token_cs_api_url_resolution_error() {
    let deps = HandlerTestDeps {
        exchange: exchange_ok("@user:example.com"),
        resolve: Some(Box::new(|_| Err("M_NOT_FOUND: no".into()))),
        ..Default::default()
    };
    let handler = new_get_token_handler(deps);
    let body = marshal_sfu_request(|r| {
        r.delay_id = "did".into();
        r.delay_timeout = 1000;
    });
    let resp = send_request(&handler, post_json("/get_token", body)).await;
    assert_eq!(
        resp.status(),
        StatusCode::BAD_REQUEST,
        "expected 400 when CS API URL resolution fails"
    );
    handler.close().await;
}

#[tokio::test]
async fn test_process_sfu_request() {
    struct Case {
        name: &'static str,
        matrix_id: &'static str,
        claimed_matrix_id: &'static str,
        delay_id: &'static str,
        delay_timeout: i64,
        expect_join_token_error: bool,
        expect_exchange_error: bool,
        expect_resolution_error: bool,
        expect_create_room: bool,
        expect_error: bool,
    }
    let base = Case {
        name: "",
        matrix_id: "@user:example.com",
        claimed_matrix_id: "@user:example.com",
        delay_id: "",
        delay_timeout: 0,
        expect_join_token_error: false,
        expect_exchange_error: false,
        expect_resolution_error: false,
        expect_create_room: false,
        expect_error: false,
    };
    for tc in [
        Case {
            name: "Full access — all OK",
            expect_create_room: true,
            ..base
        },
        Case {
            name: "Restricted — all OK",
            matrix_id: "@user:other.com",
            claimed_matrix_id: "@user:other.com",
            ..base
        },
        Case {
            name: "Exchange fails",
            expect_exchange_error: true,
            expect_error: true,
            ..base
        },
        Case {
            name: "Token key empty",
            expect_join_token_error: true,
            expect_error: true,
            ..base
        },
        Case {
            name: "ClaimedUserID mismatch",
            claimed_matrix_id: "@user:faked.com",
            expect_error: true,
            ..base
        },
        Case {
            name: "Delegation — all OK",
            expect_create_room: true,
            delay_id: "did",
            delay_timeout: 1000,
            ..base
        },
        Case {
            name: "Delegation — resolution error",
            expect_create_room: false,
            delay_id: "did",
            delay_timeout: 1000,
            expect_resolution_error: true,
            expect_error: true,
            ..base
        },
    ] {
        let create_called = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let create_called_clone = create_called.clone();

        let fail_exchange = tc.expect_exchange_error;
        let exchange_matrix_id = tc.matrix_id.to_owned();
        let fail_resolution = tc.expect_resolution_error;

        let deps = HandlerTestDeps {
            create_room: Some(Box::new(move |room, _, _| {
                create_called_clone.store(true, std::sync::atomic::Ordering::SeqCst);
                assert!(!room.is_empty(), "expected non-empty room name");
                Ok(())
            })),
            exchange: Some(Box::new(move |_, _| {
                if fail_exchange {
                    Err("M_UNAUTHORIZED: unauthorised".into())
                } else {
                    Ok(UserInfo {
                        sub: exchange_matrix_id.clone(),
                    })
                }
            })),
            resolve: Some(Box::new(move |_| {
                if fail_resolution {
                    Err("M_NOT_FOUND: no".into())
                } else {
                    Ok(CsApiUrl("https://matrix.example.com".into()))
                }
            })),
            exists: exists_block_until_cancelled(),
            ..Default::default()
        };

        let api_key = if tc.expect_join_token_error {
            ""
        } else {
            "the_api_key"
        };
        let handler = Handler::new(
            LiveKitAuth {
                key: api_key.into(),
                secret: "secret".into(),
                lk_url: "wss://lk.local:8080/foo".into(),
            },
            false,
            vec!["example.com".into()],
            Duration::ZERO,
            HashMap::new(),
            None,
            Arc::new(deps),
        );
        let req = SfuRequest {
            room_id: "!room:example.com".into(),
            slot_id: "slot".into(),
            openid_token: OpenIdTokenType {
                access_token: "token".into(),
                matrix_server_name: tc.claimed_matrix_id.split(':').nth(1).unwrap().to_owned(),
                ..Default::default()
            },
            member: MatrixRtcMemberType {
                id: "device".into(),
                claimed_user_id: tc.claimed_matrix_id.into(),
                claimed_device_id: "dev".into(),
            },
            delay_id: tc.delay_id.into(),
            delay_timeout: tc.delay_timeout,
            ..Default::default()
        };
        let result = handler.process_sfu_request(&req).await;
        if tc.expect_error {
            assert!(result.is_err(), "{}: expected error but got Ok", tc.name);
        } else {
            assert!(result.is_ok(), "{}: unexpected error: {result:?}", tc.name);
        }
        assert_eq!(
            create_called.load(std::sync::atomic::Ordering::SeqCst),
            tc.expect_create_room,
            "{}: create_livekit_room called mismatch",
            tc.name
        );
        handler.close().await;
    }
}

// ── /sfu/get endpoint (sfu_get_test.go) ───────────────────────────────────────
// Deprecated: this endpoint is pre-Matrix-2.0. When /sfu/get is removed,
// delete these tests too.

/// Verifies that OPTIONS /sfu/get returns 200 with the expected CORS headers.
#[tokio::test]
async fn test_handle_sfu_get_options() {
    let handler = new_handler_with(HandlerTestDeps::default(), &["*"], None);
    let req = http::Request::builder()
        .method("OPTIONS")
        .uri("/sfu/get")
        .body(Body::empty())
        .unwrap();
    let resp = send_request(&handler, req).await;
    assert_eq!(
        resp.status(),
        StatusCode::OK,
        "wrong status code for OPTIONS"
    );
    assert_eq!(
        resp.headers()
            .get(header::ACCESS_CONTROL_ALLOW_ORIGIN)
            .and_then(|v| v.to_str().ok()),
        Some("*"),
        "wrong Access-Control-Allow-Origin"
    );
    assert_eq!(
        resp.headers()
            .get(header::ACCESS_CONTROL_ALLOW_METHODS)
            .and_then(|v| v.to_str().ok()),
        Some("POST"),
        "wrong Access-Control-Allow-Methods"
    );
    handler.close().await;
}

/// Verifies that POSTs missing required body fields return 400 / M_BAD_JSON
/// via LegacySfuRequest::validate.
#[tokio::test]
async fn test_handle_sfu_get_missing_params() {
    let handler = new_handler_with(HandlerTestDeps::default(), &["*"], None);
    for test_case in [serde_json::json!({}), serde_json::json!({"room": ""})] {
        let resp = send_request(&handler, post_json("/sfu/get", test_case.to_string())).await;
        assert_eq!(resp.status(), StatusCode::BAD_REQUEST, "wrong status code");
        let body = body_bytes(resp).await;
        let matrix_err: MatrixErrorBody =
            serde_json::from_slice(&body).expect("failed to decode response body");
        assert_eq!(matrix_err.errcode, "M_BAD_JSON", "unexpected error code");
    }
    handler.close().await;
}

/// Verifies the full-access happy-path POST /sfu/get: a valid request
/// produces a 200 SfuResponse, create_livekit_room is invoked, and the JWT's
/// sub/room claims encode the legacy (pre-Matrix-2.0) identity scheme
/// (`<sub>:<device>` for sub, hashed (room, "m.call#ROOM") for room).
#[tokio::test]
async fn test_handle_sfu_get_success() {
    const MATRIX_SERVER_NAME: &str = "example.com";
    const CLAIMED_USER_SUB: &str = "@user:example.com";
    const DEVICE_ID: &str = "testDevice";
    const MATRIX_ROOM: &str = "testRoom";

    let create_called = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let create_called_clone = create_called.clone();
    let deps = HandlerTestDeps {
        exchange: exchange_ok(CLAIMED_USER_SUB),
        create_room: Some(Box::new(move |_, _, _| {
            create_called_clone.store(true, std::sync::atomic::Ordering::SeqCst);
            Ok(())
        })),
        ..Default::default()
    };
    let handler = Handler::new(
        LiveKitAuth {
            key: "testKey".into(),
            secret: "testSecret".into(),
            lk_url: "wss://lk.local:8080/foo".into(),
        },
        false,
        vec![MATRIX_SERVER_NAME.into()],
        Duration::ZERO,
        HashMap::new(),
        Some(new_in_memory_store()),
        Arc::new(deps),
    );

    let body = serde_json::json!({
        "room": MATRIX_ROOM,
        "openid_token": {
            "access_token": "testAccessToken",
            "token_type": "testTokenType",
            "matrix_server_name": MATRIX_SERVER_NAME,
            "expires_in": 3600,
        },
        "device_id": DEVICE_ID,
    });
    let resp = send_request(&handler, post_json("/sfu/get", body.to_string())).await;
    assert_eq!(resp.status(), StatusCode::OK, "status");
    assert!(
        create_called.load(std::sync::atomic::Ordering::SeqCst),
        "expected create_livekit_room to be called for full-access user"
    );

    let body = body_bytes(resp).await;
    let sfu_response: SfuResponse =
        serde_json::from_slice(&body).expect("failed to decode response body");
    assert_eq!(sfu_response.url, "wss://lk.local:8080/foo", "resp.url");
    assert!(!sfu_response.jwt.is_empty(), "expected JWT to be non-empty");

    let claims = parse_jwt_claims(&sfu_response.jwt, &handler.livekit_auth.secret);
    let want_sub = format!("{CLAIMED_USER_SUB}:{DEVICE_ID}");
    assert_eq!(claims["sub"], serde_json::Value::String(want_sub), "sub");
    let want_room = livekit_room_alias_for(MATRIX_ROOM, "m.call#ROOM").0;
    assert_eq!(
        claims["video"]["room"],
        serde_json::Value::String(want_room),
        "room"
    );
    handler.close().await;
}

#[tokio::test]
async fn test_process_legacy_sfu_request() {
    struct Case {
        name: &'static str,
        matrix_id: &'static str,
        delay_id: &'static str,
        delay_timeout: i64,
        expect_join_token_error: bool,
        expect_exchange_error: bool,
        expect_resolution_error: bool,
        expect_create_room: bool,
        expect_error: bool,
    }
    let base = Case {
        name: "",
        matrix_id: "@user:example.com",
        delay_id: "",
        delay_timeout: 0,
        expect_join_token_error: false,
        expect_exchange_error: false,
        expect_resolution_error: false,
        expect_create_room: false,
        expect_error: false,
    };
    for tc in [
        Case {
            name: "Full access — all OK",
            expect_create_room: true,
            ..base
        },
        Case {
            name: "Restricted — all OK",
            matrix_id: "@user:other.com",
            ..base
        },
        Case {
            name: "Exchange fails",
            expect_exchange_error: true,
            expect_error: true,
            ..base
        },
        Case {
            name: "Token key empty",
            expect_join_token_error: true,
            expect_error: true,
            ..base
        },
        Case {
            name: "Delegation — all OK",
            expect_create_room: true,
            delay_id: "did",
            delay_timeout: 1000,
            ..base
        },
        Case {
            name: "Delegation — resolution error",
            expect_create_room: false,
            delay_id: "did",
            delay_timeout: 1000,
            expect_resolution_error: true,
            expect_error: true,
            ..base
        },
    ] {
        let create_called = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let create_called_clone = create_called.clone();

        let fail_exchange = tc.expect_exchange_error;
        let fail_resolution = tc.expect_resolution_error;

        let deps = HandlerTestDeps {
            create_room: Some(Box::new(move |room, _, _| {
                create_called_clone.store(true, std::sync::atomic::Ordering::SeqCst);
                assert!(!room.is_empty(), "expected non-empty room name");
                Ok(())
            })),
            exchange: Some(Box::new(move |_, _| {
                if fail_exchange {
                    Err("M_UNAUTHORIZED: unauthorised".into())
                } else {
                    Ok(UserInfo {
                        sub: "@mock:example.com".into(),
                    })
                }
            })),
            resolve: Some(Box::new(move |_| {
                if fail_resolution {
                    Err("M_NOT_FOUND: no".into())
                } else {
                    Ok(CsApiUrl("https://matrix.example.com".into()))
                }
            })),
            exists: exists_block_until_cancelled(),
            ..Default::default()
        };

        let api_key = if tc.expect_join_token_error {
            ""
        } else {
            "the_api_key"
        };
        let handler = Handler::new(
            LiveKitAuth {
                key: api_key.into(),
                secret: "secret".into(),
                lk_url: "wss://lk.local:8080/foo".into(),
            },
            false,
            vec!["example.com".into()],
            Duration::ZERO,
            HashMap::new(),
            Some(new_in_memory_store()),
            Arc::new(deps),
        );
        let req = LegacySfuRequest {
            room: "!room:example.com".into(),
            openid_token: OpenIdTokenType {
                access_token: "token".into(),
                matrix_server_name: tc.matrix_id.split(':').nth(1).unwrap().to_owned(),
                ..Default::default()
            },
            device_id: "dev".into(),
            delay_id: tc.delay_id.into(),
            delay_timeout: tc.delay_timeout,
            ..Default::default()
        };
        let result = handler.process_legacy_sfu_request(&req).await;
        if tc.expect_error {
            assert!(result.is_err(), "{}: expected error but got Ok", tc.name);
        } else {
            assert!(result.is_ok(), "{}: unexpected error: {result:?}", tc.name);
        }
        assert_eq!(
            create_called.load(std::sync::atomic::Ordering::SeqCst),
            tc.expect_create_room,
            "{}: create_livekit_room called mismatch",
            tc.name
        );
        handler.close().await;
    }
}

// ── /delegate_delayed_leave endpoint (delegate_delayed_leave_test.go) ─────────

#[tokio::test]
async fn test_handle_delegate_delayed_leave_options() {
    let handler = new_delegate_delayed_leave_handler(HandlerTestDeps::default());
    let req = http::Request::builder()
        .method("OPTIONS")
        .uri("/delegate_delayed_leave")
        .body(Body::empty())
        .unwrap();
    let resp = send_request(&handler, req).await;
    assert_eq!(resp.status(), StatusCode::OK, "expected 200 for OPTIONS");
    handler.close().await;
}

#[tokio::test]
async fn test_handle_delegate_delayed_leave_method_not_allowed() {
    let handler = new_delegate_delayed_leave_handler(HandlerTestDeps::default());
    for method in ["GET", "PUT", "DELETE"] {
        let req = http::Request::builder()
            .method(method)
            .uri("/delegate_delayed_leave")
            .body(Body::empty())
            .unwrap();
        let resp = send_request(&handler, req).await;
        assert_eq!(
            resp.status(),
            StatusCode::METHOD_NOT_ALLOWED,
            "{method}: expected 405"
        );
    }
    handler.close().await;
}

#[tokio::test]
async fn test_handle_delegate_delayed_leave_invalid_json() {
    let handler = new_delegate_delayed_leave_handler(HandlerTestDeps::default());
    let resp = send_request(
        &handler,
        post_json("/delegate_delayed_leave", "{bad json}".into()),
    )
    .await;
    assert_eq!(
        resp.status(),
        StatusCode::BAD_REQUEST,
        "expected 400 for invalid JSON"
    );
    handler.close().await;
}

#[tokio::test]
async fn test_handle_delegate_delayed_leave_missing_fields() {
    let handler = new_delegate_delayed_leave_handler(HandlerTestDeps::default());
    let resp = send_request(&handler, post_json("/delegate_delayed_leave", "{}".into())).await;
    assert_eq!(
        resp.status(),
        StatusCode::BAD_REQUEST,
        "expected 400 for missing fields"
    );
    handler.close().await;
}

#[tokio::test]
async fn test_handle_delegate_delayed_leave_unauthorized_user() {
    let deps = HandlerTestDeps {
        exchange: exchange_ok("@real:example.com"),
        ..Default::default()
    };
    let handler = new_delegate_delayed_leave_handler(deps);
    let body = marshal_delegate_delayed_leave_request(|r| {
        r.member.claimed_user_id = "@attacker:example.com".into(); // mismatch
    });
    let resp = send_request(&handler, post_json("/delegate_delayed_leave", body)).await;
    assert_eq!(
        resp.status(),
        StatusCode::UNAUTHORIZED,
        "expected 401 for mismatched user"
    );
    handler.close().await;
}

#[tokio::test]
async fn test_handle_delegate_delayed_leave_restricted_user() {
    let deps = HandlerTestDeps {
        exchange: exchange_ok("@user:restricted.com"),
        ..Default::default()
    };
    // Handler configured with only "example.com" as full-access.
    let handler = new_delegate_delayed_leave_handler(deps);
    let body = marshal_delegate_delayed_leave_request(|r| {
        r.member.claimed_user_id = "@user:restricted.com".into();
        r.openid_token.matrix_server_name = "restricted.com".into(); // not in full-access list
    });
    let resp = send_request(&handler, post_json("/delegate_delayed_leave", body)).await;
    assert_eq!(
        resp.status(),
        StatusCode::FORBIDDEN,
        "expected 403 for restricted user"
    );
    handler.close().await;
}

#[tokio::test]
async fn test_handle_delegate_delayed_leave_exchange_error() {
    let deps = HandlerTestDeps {
        exchange: Some(Box::new(|_, _| Err("M_UNAUTHORIZED: no".into()))),
        ..Default::default()
    };
    let handler = new_delegate_delayed_leave_handler(deps);
    let body = marshal_delegate_delayed_leave_request(|_| {});
    let resp = send_request(&handler, post_json("/delegate_delayed_leave", body)).await;
    assert_eq!(
        resp.status(),
        StatusCode::UNAUTHORIZED,
        "expected 401 when exchange fails"
    );
    handler.close().await;
}

#[tokio::test]
async fn test_handle_delegate_delayed_leave_cs_api_url_resolution_error() {
    let deps = HandlerTestDeps {
        exchange: exchange_ok("@user:example.com"),
        resolve: Some(Box::new(|_| Err("M_NOT_FOUND: no".into()))),
        ..Default::default()
    };
    let handler = new_delegate_delayed_leave_handler(deps);
    let body = marshal_delegate_delayed_leave_request(|_| {});
    let resp = send_request(&handler, post_json("/delegate_delayed_leave", body)).await;
    assert_eq!(
        resp.status(),
        StatusCode::BAD_REQUEST,
        "expected 400 when CS API URL resolution fails"
    );
    handler.close().await;
}

#[tokio::test]
async fn test_handle_delegate_delayed_leave_success() {
    let deps = HandlerTestDeps {
        exchange: exchange_ok("@user:example.com"),
        exists: exists_block_until_cancelled(),
        ..Default::default()
    };
    let handler = new_delegate_delayed_leave_handler(deps);
    let body = marshal_delegate_delayed_leave_request(|_| {});
    let resp = send_request(&handler, post_json("/delegate_delayed_leave", body)).await;

    assert_eq!(
        resp.status(),
        StatusCode::OK,
        "expected 200 for valid request"
    );
    // Response body should be empty (no JWT returned).
    let body = body_bytes(resp).await;
    assert_eq!(
        String::from_utf8_lossy(&body).trim(),
        "{}",
        "expected empty object in response body"
    );
    handler.close().await;
}

/// Verifies that the endpoint does NOT return a JWT — differentiating it from
/// /get_token.
#[tokio::test]
async fn test_handle_delegate_delayed_leave_no_jwt() {
    let deps = HandlerTestDeps {
        exchange: exchange_ok("@user:example.com"),
        exists: exists_block_until_cancelled(),
        ..Default::default()
    };
    let handler = new_delegate_delayed_leave_handler(deps);
    let body = marshal_delegate_delayed_leave_request(|_| {});
    let resp = send_request(&handler, post_json("/delegate_delayed_leave", body)).await;

    // 200 with an empty object body is the correct success response.
    assert_eq!(resp.status(), StatusCode::OK, "expected 200");
    let body = body_bytes(resp).await;
    if let Ok(resp_map) = serde_json::from_slice::<serde_json::Value>(&body) {
        assert!(
            resp_map.get("jwt").is_none(),
            "response must not contain a JWT"
        );
        assert!(
            resp_map.get("url").is_none(),
            "response must not contain a url"
        );
    }
    handler.close().await;
}

// ── process_delegate_delayed_leave unit tests ─────────────────────────────────

/// Verifies that a successful call to process_delegate_delayed_leave hands a
/// job over to the loop without creating a LiveKit room or token.
#[tokio::test]
async fn test_process_delegate_delayed_leave_creates_job() {
    let create_called = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let create_called_clone = create_called.clone();
    let deps = HandlerTestDeps {
        exchange: exchange_ok("@user:example.com"),
        exists: exists_block_until_cancelled(),
        create_room: Some(Box::new(move |_, _, _| {
            create_called_clone.store(true, std::sync::atomic::Ordering::SeqCst);
            Ok(())
        })),
        ..Default::default()
    };
    let handler = new_delegate_delayed_leave_handler(deps);
    let req = valid_delegate_delayed_leave_request();
    let resp = handler
        .process_delegate_delayed_leave(&req)
        .await
        .expect("unexpected error");
    let _ = resp; // a response was produced

    assert!(
        !create_called.load(std::sync::atomic::Ordering::SeqCst),
        "process_delegate_delayed_leave must NOT call create_livekit_room"
    );

    // Give the job a moment to be registered.
    tokio::time::sleep(Duration::from_millis(50)).await;
    handler.close().await;
}

/// Verifies that a job rejected by job creation (delay timeout <= 0,
/// bypassing request parsing) surfaces as 400 M_BAD_JSON carrying the
/// creation error.
#[tokio::test]
async fn test_process_delegate_delayed_leave_invalid_delay_timeout() {
    let deps = HandlerTestDeps {
        exchange: exchange_ok("@user:example.com"),
        ..Default::default()
    };
    let handler = new_delegate_delayed_leave_handler(deps);
    let mut req = valid_delegate_delayed_leave_request();
    req.delay_timeout = 0; // invalid — would be rejected by request parsing, too

    let err = handler
        .process_delegate_delayed_leave(&req)
        .await
        .expect_err("expected MatrixErrorResponse");
    assert_eq!(err.status, 400, "expected 400");
    assert_eq!(err.errcode, "M_BAD_JSON", "expected M_BAD_JSON");
    assert!(
        !err.err.is_empty(),
        "expected the job-creation error as message"
    );
    handler.close().await;
}

/// Verifies that a delegation request hitting an already-shut-down handler
/// surfaces as 503 M_UNKNOWN (not M_BAD_JSON — the request itself was fine).
#[tokio::test]
async fn test_process_delegate_delayed_leave_after_shutdown() {
    let deps = HandlerTestDeps {
        exchange: exchange_ok("@user:example.com"),
        ..Default::default()
    };
    let handler = new_delegate_delayed_leave_handler(deps);
    handler.close().await; // idempotent — closing again later is a no-op

    let req = valid_delegate_delayed_leave_request();
    let err = handler
        .process_delegate_delayed_leave(&req)
        .await
        .expect_err("expected MatrixErrorResponse");
    assert_eq!(err.status, 503, "expected 503");
    assert_eq!(err.errcode, "M_UNKNOWN", "expected M_UNKNOWN");
}

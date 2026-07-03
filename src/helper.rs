// Copyright 2025 Element Creations Ltd.
// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// helper.rs: cross-cutting helpers shared by request handlers and the
// delayed-event manager — ID generation, LiveKit SDK wrappers, Matrix
// CS-API calls. The `Deps` trait plays the role of the swappable
// function variables in the Go implementation: the real logic lives in
// the trait's default methods and tests override individual methods.

use std::collections::HashMap;
use std::sync::{Arc, RwLock};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use async_trait::async_trait;
use base64::engine::general_purpose::STANDARD_NO_PAD;
use base64::Engine;
use rand::RngCore;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use tracing::{debug, error, info, warn};

use crate::delayed_event_manager::{DelayEventAction, DELAYED_EVENTS_ENDPOINT};
use crate::requests::OpenIdTokenType;
use crate::retry::{Classify, ErrorClass};

/// The authentication bundle for talking to LiveKit.
#[derive(Debug, Clone, Default)]
pub struct LiveKitAuth {
    pub key: String,
    pub secret: String,
    pub lk_url: String,
}

// ── newtypes ─────────────────────────────────────────────────────────────────

macro_rules! string_newtype {
    ($name:ident) => {
        #[derive(
            Debug, Clone, Default, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize,
        )]
        #[serde(transparent)]
        pub struct $name(pub String);

        impl std::fmt::Display for $name {
            fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                self.0.fmt(f)
            }
        }

        impl From<&str> for $name {
            fn from(s: &str) -> Self {
                Self(s.to_owned())
            }
        }

        impl From<String> for $name {
            fn from(s: String) -> Self {
                Self(s)
            }
        }

        impl $name {
            pub fn as_str(&self) -> &str {
                &self.0
            }
            pub fn is_empty(&self) -> bool {
                self.0.is_empty()
            }
        }
    };
}

string_newtype!(LiveKitRoomAlias);
string_newtype!(LiveKitIdentity);
string_newtype!(CsApiUrl);
string_newtype!(UniqueId);

/// Generates a unique, chronologically sortable ID: an 8-byte big-endian
/// microsecond timestamp followed by 8 random bytes, Base32Hex-encoded
/// without padding. Base32Hex's alphabet is naturally ordered in ASCII
/// (0-9 then A-V), so string comparison matches chronological order.
pub fn new_unique_id() -> UniqueId {
    let mut b = [0u8; 16];

    let micros = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .expect("system time before Unix epoch")
        .as_micros() as u64;
    b[0..8].copy_from_slice(&micros.to_be_bytes());

    rand::thread_rng().fill_bytes(&mut b[8..16]);

    UniqueId(data_encoding::BASE32HEX_NOPAD.encode(&b))
}

// ── CS-API URL cache ─────────────────────────────────────────────────────────

struct CsApiUrlCacheEntry {
    url: CsApiUrl,
    expires_at: Instant,
}

/// A TTL cache for resolved Client-Server API URLs.
#[derive(Default)]
pub struct CsApiUrlCache {
    entries: RwLock<HashMap<String, CsApiUrlCacheEntry>>,
}

impl CsApiUrlCache {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn get(&self, server_name: &str) -> Option<CsApiUrl> {
        let entries = self.entries.read().unwrap();
        match entries.get(server_name) {
            // We don't evict the expired entry because it is extremely likely
            // that the caller will re-resolve the URL and update the cache.
            Some(entry) if Instant::now() <= entry.expires_at => Some(entry.url.clone()),
            _ => None,
        }
    }

    pub fn set(&self, server_name: &str, url: CsApiUrl, ttl: Duration) {
        self.entries.write().unwrap().insert(
            server_name.to_owned(),
            CsApiUrlCacheEntry {
                url,
                expires_at: Instant::now() + ttl,
            },
        );
    }
}

// ── hashing ──────────────────────────────────────────────────────────────────

/// Marshals a slice of strings to JSON, byte-compatible with Go's
/// json.Marshal for the inputs used here.
fn marshal_strings(ss: &[&str]) -> Vec<u8> {
    serde_json::to_vec(ss).expect("string slices always serialize")
}

fn sha256_unpadded_base64(data: &[u8]) -> String {
    let hash = Sha256::digest(data);
    STANDARD_NO_PAD.encode(hash)
}

/// Returns the deterministic LiveKit room alias for a given
/// (Matrix room ID, MatrixRTC slot) pair.
pub fn livekit_room_alias_for(matrix_room: &str, matrix_rtc_slot: &str) -> LiveKitRoomAlias {
    LiveKitRoomAlias(sha256_unpadded_base64(&marshal_strings(&[
        matrix_room,
        matrix_rtc_slot,
    ])))
}

/// Returns the deterministic LiveKit identity for a given
/// (Matrix user ID, device ID, MatrixRTC member ID) tuple.
pub fn livekit_identity_for(matrix_id: &str, device_id: &str, member_id: &str) -> LiveKitIdentity {
    LiveKitIdentity(sha256_unpadded_base64(&marshal_strings(&[
        matrix_id, device_id, member_id,
    ])))
}

// ── OpenID / well-known DTOs ─────────────────────────────────────────────────

/// The response of the Matrix federation OpenID userinfo endpoint.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct UserInfo {
    pub sub: String,
}

/// The relevant subset of a .well-known/matrix/client response.
#[derive(Debug, Clone, Default)]
pub struct ClientWellKnown {
    pub homeserver_base_url: String,
}

// ── LiveKit room service abstraction ─────────────────────────────────────────

/// Room creation parameters passed to [`RoomServiceClient::create_room`].
#[derive(Debug, Clone, Default)]
pub struct CreateRoomParams {
    pub name: String,
    pub empty_timeout: u32,
    pub departure_timeout: u32,
    pub max_participants: u32,
}

/// The subset of LiveKit room data this service inspects.
#[derive(Debug, Clone, Default)]
pub struct RoomInfo {
    pub sid: String,
    pub creation_time: i64,
}

#[derive(Debug, thiserror::Error)]
pub enum RoomServiceError {
    #[error("not found: {0}")]
    NotFound(String),
    #[error("{0}")]
    Other(String),
}

/// The interface for room operations (mirrors the Go RoomClient interface).
#[async_trait]
pub trait RoomServiceClient: Send + Sync {
    async fn create_room(&self, params: CreateRoomParams) -> Result<RoomInfo, RoomServiceError>;
    async fn get_participant(&self, room: &str, identity: &str) -> Result<(), RoomServiceError>;
}

/// Converts a LiveKit URL to its HTTP form for service API calls — the
/// equivalent of the Go SDK's ToHttpURL. ws:// becomes http://, wss://
/// becomes https://; anything else passes through unchanged.
pub(crate) fn to_http_url(url: &str) -> String {
    if let Some(rest) = url.strip_prefix("ws://") {
        format!("http://{rest}")
    } else if let Some(rest) = url.strip_prefix("wss://") {
        format!("https://{rest}")
    } else {
        url.to_owned()
    }
}

/// The real [`RoomServiceClient`].
///
/// TODO(upstream): livekit-api's `RoomClient` builds request URLs via
/// `url.set_path(...)`, which REPLACES the URL path and thereby strips any
/// reverse-proxy prefix encoded in LIVEKIT_URL (e.g. `wss://host/livekit/sfu`
/// → `/twirp/...` instead of `/livekit/sfu/twirp/...`). It also does not
/// convert ws/wss schemes for HTTP service calls. The Go SDK preserves the
/// prefix (sanitizeBaseURL + baseServicePath) and converts the scheme
/// (ToHttpURL). Until livekit-api joins paths instead of replacing them (or
/// exposes a prefix-aware constructor), the two RoomService Twirp calls are
/// issued directly here — the SDK is still used for access tokens (auth
/// headers, JWTs) and webhook verification.
struct LiveKitRoomServiceClient {
    /// The http(s) base URL, possibly carrying a path prefix.
    host: String,
    api_key: String,
    api_secret: String,
    client: reqwest::Client,
}

impl LiveKitRoomServiceClient {
    fn new(url: &str, api_key: &str, api_secret: &str) -> Self {
        Self {
            host: to_http_url(url),
            api_key: api_key.to_owned(),
            api_secret: api_secret.to_owned(),
            client: reqwest::Client::new(),
        }
    }

    /// Issues one Twirp RoomService call, preserving any path prefix in the
    /// host URL (unlike the SDK's TwirpClient — see the struct-level TODO).
    async fn twirp_call<Req: prost::Message, Resp: prost::Message + Default>(
        &self,
        method: &str,
        grants: livekit_api::access_token::VideoGrants,
        request: Req,
    ) -> Result<Resp, RoomServiceError> {
        let url = format!(
            "{}/twirp/livekit.RoomService/{method}",
            self.host.trim_end_matches('/')
        );
        let token =
            livekit_api::access_token::AccessToken::with_api_key(&self.api_key, &self.api_secret)
                .with_grants(grants)
                .to_jwt()
                .map_err(|e| RoomServiceError::Other(e.to_string()))?;

        let resp = self
            .client
            .post(&url)
            .header(http::header::AUTHORIZATION, format!("Bearer {token}"))
            .header(http::header::CONTENT_TYPE, "application/protobuf")
            .body(request.encode_to_vec())
            .send()
            .await
            .map_err(|e| RoomServiceError::Other(format!("failed to execute the request: {e}")))?;

        if resp.status() == http::StatusCode::OK {
            let bytes = resp
                .bytes()
                .await
                .map_err(|e| RoomServiceError::Other(e.to_string()))?;
            Resp::decode(bytes).map_err(|e| RoomServiceError::Other(format!("prost error: {e}")))
        } else {
            // Twirp errors carry a JSON body of the shape {"code": ..., "msg": ...}.
            #[derive(Deserialize)]
            struct TwirpErrorBody {
                #[serde(default)]
                code: String,
                #[serde(default)]
                msg: String,
            }
            let status = resp.status().as_u16();
            let body: TwirpErrorBody = resp.json().await.unwrap_or(TwirpErrorBody {
                code: String::new(),
                msg: String::new(),
            });
            let message = format!("twirp error (status {status}): {}: {}", body.code, body.msg);
            if body.code == "not_found" {
                Err(RoomServiceError::NotFound(message))
            } else {
                Err(RoomServiceError::Other(message))
            }
        }
    }
}

#[async_trait]
impl RoomServiceClient for LiveKitRoomServiceClient {
    async fn create_room(&self, params: CreateRoomParams) -> Result<RoomInfo, RoomServiceError> {
        let room: livekit_protocol::Room = self
            .twirp_call(
                "CreateRoom",
                livekit_api::access_token::VideoGrants {
                    room_create: true,
                    ..Default::default()
                },
                livekit_protocol::CreateRoomRequest {
                    name: params.name.clone(),
                    empty_timeout: params.empty_timeout,
                    departure_timeout: params.departure_timeout,
                    max_participants: params.max_participants,
                    ..Default::default()
                },
            )
            .await?;
        Ok(RoomInfo {
            sid: room.sid,
            creation_time: room.creation_time,
        })
    }

    async fn get_participant(&self, room: &str, identity: &str) -> Result<(), RoomServiceError> {
        let _: livekit_protocol::ParticipantInfo = self
            .twirp_call(
                "GetParticipant",
                livekit_api::access_token::VideoGrants {
                    room_admin: true,
                    room: room.to_owned(),
                    ..Default::default()
                },
                livekit_protocol::RoomParticipantIdentity {
                    room: room.to_owned(),
                    identity: identity.to_owned(),
                    ..Default::default()
                },
            )
            .await?;
        Ok(())
    }
}

// ── delayed-event action error taxonomy ──────────────────────────────────────

/// The error side of [`Deps::execute_delayed_event_action`]. Carries the HTTP
/// status code (0 for transport errors) so callers can log it, plus the retry
/// classification consumed by [`crate::retry::retry`].
#[derive(Debug, thiserror::Error)]
pub enum ActionError {
    /// 404 on ActionRestart: the delayed event no longer exists on the
    /// homeserver. Permanent — retrying is pointless.
    #[error("CS API: delayed event not found")]
    DelayedEventNotFound { status: u16 },
    /// 429 with a usable retry hint (Retry-After header or retry_after_ms).
    #[error("CS API rate limited (http status code {status}), retry after {retry_after:?}")]
    RetryAfter { status: u16, retry_after: Duration },
    /// Everything else — 5xx, hint-less 429, transport errors, and any
    /// unclassified status. Retried on the default backoff schedule.
    #[error("{msg}")]
    Transient { status: u16, msg: String },
}

impl ActionError {
    pub fn status(&self) -> u16 {
        match self {
            ActionError::DelayedEventNotFound { status } => *status,
            ActionError::RetryAfter { status, .. } => *status,
            ActionError::Transient { status, .. } => *status,
        }
    }

    pub fn is_delayed_event_not_found(&self) -> bool {
        matches!(self, ActionError::DelayedEventNotFound { .. })
    }
}

impl Classify for ActionError {
    fn classify(&self) -> ErrorClass {
        match self {
            ActionError::DelayedEventNotFound { .. } => ErrorClass::Permanent,
            ActionError::RetryAfter { retry_after, .. } => ErrorClass::RetryAfter(*retry_after),
            ActionError::Transient { .. } => ErrorClass::Transient,
        }
    }
}

/// The body Matrix homeservers attach to M_LIMIT_EXCEEDED responses.
#[derive(Debug, Deserialize)]
struct LimitExceededBody {
    #[serde(default)]
    #[allow(dead_code)]
    errcode: String,
    #[serde(default)]
    retry_after_ms: i64,
}

// ── Deps: the swappable dependency surface ───────────────────────────────────

/// The set of external interactions that tests replace. The real logic lives
/// in the default method implementations; [`RealDeps`] adds nothing. This is
/// the Rust equivalent of the Go implementation's `var X = func(...)`
/// swap-for-test pattern.
#[async_trait]
pub trait Deps: Send + Sync {
    /// Constructs a LiveKit room service client (mockable in tests).
    fn new_room_service_client(
        &self,
        url: &str,
        key: &str,
        secret: &str,
    ) -> Arc<dyn RoomServiceClient> {
        Arc::new(LiveKitRoomServiceClient::new(url, key, secret))
    }

    /// Resolves the Client-Server API base URL for a server name via
    /// .well-known/matrix/client discovery.
    async fn discover_client_api(
        &self,
        server_name: &str,
    ) -> Result<Option<ClientWellKnown>, String> {
        #[derive(Deserialize)]
        struct WellKnownHomeserver {
            #[serde(default)]
            base_url: String,
        }
        #[derive(Deserialize)]
        struct WellKnownClient {
            #[serde(rename = "m.homeserver")]
            homeserver: Option<WellKnownHomeserver>,
        }

        let url = format!("https://{server_name}/.well-known/matrix/client");
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(30))
            .build()
            .map_err(|e| e.to_string())?;
        let resp = client.get(&url).send().await.map_err(|e| e.to_string())?;
        if !resp.status().is_success() {
            return Err(format!(
                "failed to fetch {url}: http status code {}",
                resp.status().as_u16()
            ));
        }
        let parsed: WellKnownClient = resp.json().await.map_err(|e| e.to_string())?;
        Ok(Some(ClientWellKnown {
            homeserver_base_url: parsed.homeserver.map(|h| h.base_url).unwrap_or_default(),
        }))
    }

    /// Given a server name and a map of overrides, tries to resolve the URL of
    /// the Client-Server API. Overrides win over the cache, which wins over
    /// fresh .well-known resolution.
    async fn resolve_cs_api_url(
        &self,
        server_name: &str,
        overrides: &HashMap<String, CsApiUrl>,
        cache: Option<&CsApiUrlCache>,
    ) -> Result<CsApiUrl, String> {
        resolve_cs_api_url_via(self, server_name, overrides, cache).await
    }

    /// Validates an OpenID token against its homeserver's federation API and
    /// returns the user info (notably the `sub` — the Matrix user ID).
    async fn exchange_openid_userinfo(
        &self,
        token: &OpenIdTokenType,
        skip_verify_tls: bool,
    ) -> Result<UserInfo, String> {
        if token.access_token.is_empty() || token.matrix_server_name.is_empty() {
            return Err("missing parameters in openid token".into());
        }

        let base = resolve_federation_base_url(&token.matrix_server_name, skip_verify_tls).await;

        // Like Go's InsecureSkipVerify, skipping TLS verification covers both
        // the certificate chain and the hostname (reqwest's rustls backend
        // disables the whole verifier).
        let client = reqwest::Client::builder()
            .danger_accept_invalid_certs(skip_verify_tls)
            .timeout(Duration::from_secs(30))
            .build()
            .map_err(|e| e.to_string())?;

        let url = format!("{base}/_matrix/federation/v1/openid/userinfo");
        let resp = client
            .get(&url)
            .query(&[("access_token", token.access_token.as_str())])
            .send()
            .await
            .map_err(|e| {
                error!(err = %e, "OpenIDUserInfo: Failed to look up user info");
                format!("failed to look up user info: {e}")
            })?;
        if !resp.status().is_success() {
            let status = resp.status().as_u16();
            error!(status, "OpenIDUserInfo: Failed to look up user info");
            return Err(format!(
                "failed to look up user info: http status code {status}"
            ));
        }
        let info: UserInfo = resp
            .json()
            .await
            .map_err(|e| format!("failed to look up user info: {e}"))?;
        Ok(info)
    }

    /// Ensures a LiveKit room exists for the given alias, with the service's
    /// standard timeouts. Creating an already-existing room is not an error —
    /// LiveKit returns the existing room.
    async fn create_livekit_room(
        &self,
        livekit_auth: &LiveKitAuth,
        room: &LiveKitRoomAlias,
        matrix_user: &str,
        lk_identity: &LiveKitIdentity,
    ) -> Result<(), String> {
        let room_client = self.new_room_service_client(
            &livekit_auth.lk_url,
            &livekit_auth.key,
            &livekit_auth.secret,
        );
        let creation_start = unix_now();

        const EMPTY_TIMEOUT_SECS: u32 = 5 * 60; // 5 minutes: keep room open if no one joins
        const DEPARTURE_TIMEOUT_SECS: u32 = 20; // 20 seconds: keep room after everyone leaves

        let lk_room = room_client
            .create_room(CreateRoomParams {
                name: room.0.clone(),
                empty_timeout: EMPTY_TIMEOUT_SECS,
                departure_timeout: DEPARTURE_TIMEOUT_SECS,
                max_participants: 0, // 0 == no limitation
            })
            .await
            .map_err(|err| {
                error!(
                    room = %room, lk_id = %lk_identity, matrix_user, access = "full", err = %err,
                    "CreateLiveKitRoom: Error creating room"
                );
                format!("unable to create room {room}: {err}")
            })?;

        let is_new_room =
            lk_room.creation_time >= creation_start && lk_room.creation_time <= unix_now();
        info!(
            room_status = if is_new_room { "created" } else { "reused" },
            room = %room,
            room_sid = %lk_room.sid,
            lk_id = %lk_identity,
            matrix_user,
            access = "full",
            "CreateLiveKitRoom"
        );

        Ok(())
    }

    /// Reports whether the given identity is currently present in the given
    /// LiveKit room.
    ///
    ///   - `Ok(true)`   participant present.
    ///   - `Ok(false)`  participant confirmed absent (SFU returned NotFound).
    ///   - `Err(err)`   transport / auth / server error — presence unknown.
    async fn participant_exists(
        &self,
        lk_auth: &LiveKitAuth,
        room: &LiveKitRoomAlias,
        identity: &LiveKitIdentity,
    ) -> Result<bool, String> {
        let room_client =
            self.new_room_service_client(&lk_auth.lk_url, &lk_auth.key, &lk_auth.secret);
        match room_client.get_participant(&room.0, &identity.0).await {
            Ok(()) => Ok(true),
            Err(RoomServiceError::NotFound(_)) => Ok(false),
            Err(err) => Err(err.to_string()),
        }
    }

    /// POSTs the given action (restart or send) to the Matrix CS-API for
    /// `delay_id`.
    ///
    /// Return contract — by how [`crate::retry::retry`] treats the result:
    ///
    /// success — no retry; caller proceeds:
    ///   - 2xx                                → `Ok(status)`
    ///   - 404 on ActionSend                  → `Ok(404)` (MSC-4140 already-sent / cancelled)
    ///
    /// Permanent — no retry:
    ///   - 404 on ActionRestart               → [`ActionError::DelayedEventNotFound`]
    ///
    /// RetryAfter — retry after the server's hint:
    ///   - 429 + Retry-After or retry_after_ms → [`ActionError::RetryAfter`]
    ///
    /// transient — retry on default schedule until the elapsed budget runs out:
    ///   - 5xx                                → "temporarily unavailable"
    ///   - 429 with no usable retry hint      → "temporarily unavailable"
    ///   - Transport / URL error              → status field is 0
    ///   - Any other status (catchall)        → "CS API returned unexpected status: N"
    ///     (e.g. 408, 421, 423, 425 — genuinely retriable; also 1xx, 3xx)
    async fn execute_delayed_event_action(
        &self,
        cs_api_url: &CsApiUrl,
        delay_id: &str,
        action: DelayEventAction,
    ) -> Result<u16, ActionError> {
        // The URL is built by pushing path segments, which percent-escapes
        // delay_id — preventing path-traversal attacks since delay_id is
        // attacker-controlled. The action is a typed constant and safe.
        let mut endpoint =
            url::Url::parse(cs_api_url.as_str()).map_err(|e| ActionError::Transient {
                status: 0,
                msg: format!("execute_delayed_event_action: invalid URL: {e}"),
            })?;
        {
            let mut segments =
                endpoint
                    .path_segments_mut()
                    .map_err(|_| ActionError::Transient {
                        status: 0,
                        msg: "execute_delayed_event_action: invalid URL: cannot be a base".into(),
                    })?;
            segments.pop_if_empty();
            for segment in DELAYED_EVENTS_ENDPOINT.trim_matches('/').split('/') {
                segments.push(segment);
            }
            segments.push(delay_id);
            segments.push(action.as_str());
        }

        // The client is built once and reused — TLS configuration setup is
        // expensive enough to matter on the retry path.
        static CLIENT: std::sync::OnceLock<reqwest::Client> = std::sync::OnceLock::new();
        let client = CLIENT.get_or_init(|| {
            reqwest::Client::builder()
                .timeout(Duration::from_secs(5))
                .build()
                .expect("default reqwest client")
        });
        let resp = client
            .post(endpoint.clone())
            .header(http::header::CONTENT_TYPE, "application/json")
            .body("{}")
            .send()
            .await
            .map_err(|e| {
                debug!(url = %endpoint, err = %e, "execute_delayed_event_action");
                ActionError::Transient {
                    status: 0,
                    msg: e.to_string(),
                }
            })?;

        let status = resp.status().as_u16();
        debug!(url = %endpoint, status, "execute_delayed_event_action");

        match status {
            // Happy path: server accepted the action.
            200 | 204 => Ok(status),

            // MSC-4140: 404 on send means the event was already sent or
            // cancelled — treat as success.
            404 if action == DelayEventAction::Send => Ok(status),

            // 404 on restart: delayed event no longer present on the
            // homeserver. Permanent so the retry loop stops immediately.
            404 => Err(ActionError::DelayedEventNotFound { status }),

            // Any 5xx is transient (CS API restart, DB lock, load-balancer
            // hiccup, upstream timeout). Retry on the default schedule.
            500..=599 => Err(ActionError::Transient {
                status,
                msg: format!("CS API temporarily unavailable (http status code {status})"),
            }),

            429 => {
                // Prefer the standard HTTP Retry-After header (RFC 7231
                // §7.1.3) — works for all Matrix homeservers and for any
                // non-Matrix middlebox (CDN, proxy, gateway) sitting in front.
                let retry_after_header = resp
                    .headers()
                    .get(http::header::RETRY_AFTER)
                    .and_then(|v| v.to_str().ok())
                    .unwrap_or_default()
                    .to_owned();
                if let Ok(seconds) = retry_after_header.parse::<i64>() {
                    let seconds = seconds.max(0) as u64;
                    return Err(ActionError::RetryAfter {
                        status,
                        retry_after: Duration::from_secs(seconds),
                    });
                }
                if let Ok(t) = httpdate::parse_http_date(&retry_after_header) {
                    let d = t
                        .duration_since(SystemTime::now())
                        .unwrap_or(Duration::ZERO);
                    return Err(ActionError::RetryAfter {
                        status,
                        retry_after: Duration::from_secs(d.as_secs()),
                    });
                }
                // Matrix-spec fallback: M_LIMIT_EXCEEDED carries
                // retry_after_ms in the response body. Deprecated in spec
                // v1.10 in favour of Retry-After, but still emitted by
                // Synapse / Dendrite / Conduit for backwards compatibility —
                // and the only signal from older homeservers. Best-effort:
                // decode once; ignore failures.
                if let Ok(body) = resp.json::<LimitExceededBody>().await {
                    if body.retry_after_ms > 0 {
                        // Ceil ms → s (e.g. 500 ms → 1 s, 1500 ms → 2 s).
                        let seconds = (body.retry_after_ms as u64).div_ceil(1000);
                        return Err(ActionError::RetryAfter {
                            status,
                            retry_after: Duration::from_secs(seconds),
                        });
                    }
                }
                // Neither header nor body had a usable hint — let the retry
                // loop decide.
                Err(ActionError::Transient {
                    status,
                    msg: "CS API temporarily unavailable (http status code 429)".into(),
                })
            }

            // Anything not classified above is treated as transient — many
            // 4xx codes are genuinely retriable (408 Request Timeout,
            // 421 Misdirected, 423 Locked, 425 Too Early, …)
            _ => Err(ActionError::Transient {
                status,
                msg: format!("CS API returned unexpected status: {status}"),
            }),
        }
    }
}

/// The production [`Deps`] implementation — all behaviour comes from the
/// trait's default methods.
pub struct RealDeps;

impl Deps for RealDeps {}

/// The default resolution logic behind [`Deps::resolve_cs_api_url`]: overrides
/// win over the cache, which wins over fresh .well-known resolution (via
/// `deps.discover_client_api`). A free function so that test doubles that
/// override individual `Deps` methods can still fall back to it.
pub async fn resolve_cs_api_url_via<D: Deps + ?Sized>(
    deps: &D,
    server_name: &str,
    overrides: &HashMap<String, CsApiUrl>,
    cache: Option<&CsApiUrlCache>,
) -> Result<CsApiUrl, String> {
    // Prefer explicit overrides.
    if let Some(url) = overrides.get(server_name) {
        if !url.is_empty() {
            return Ok(url.clone());
        }
    }

    // Next, check the cache.
    if let Some(cache) = cache {
        if let Some(url) = cache.get(server_name) {
            return Ok(url);
        }
    }

    // Still nothing. Let's try .well-known resolution.
    let discovered = deps.discover_client_api(server_name).await;
    if let Ok(Some(well_known)) = &discovered {
        if !well_known.homeserver_base_url.is_empty() {
            if let Some(cache) = cache {
                // TODO: Read the TTL from cache-control headers and limit
                // them to a minimum of say 1 hour to prevent DDos-ing.
                cache.set(
                    server_name,
                    CsApiUrl(well_known.homeserver_base_url.clone()),
                    Duration::from_secs(4 * 60 * 60),
                );
            }
            return Ok(CsApiUrl(well_known.homeserver_base_url.clone()));
        }
    }

    // We're out of options.
    warn!(server_name, "Failed to resolve URL of Client-Server API");
    match discovered {
        Err(e) => Err(e),
        Ok(_) => Err(format!(
            "no .well-known/matrix/client record found for {server_name}"
        )),
    }
}

fn unix_now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// Resolves the federation base URL for a Matrix server name: an explicit
/// port is used verbatim; otherwise .well-known/matrix/server delegation is
/// attempted, falling back to the default federation port 8448.
async fn resolve_federation_base_url(server_name: &str, skip_verify_tls: bool) -> String {
    if has_explicit_port(server_name) {
        return format!("https://{server_name}");
    }

    #[derive(Deserialize)]
    struct WellKnownServer {
        #[serde(rename = "m.server", default)]
        server: String,
    }

    let client = reqwest::Client::builder()
        .danger_accept_invalid_certs(skip_verify_tls)
        .timeout(Duration::from_secs(10))
        .build();
    if let Ok(client) = client {
        let url = format!("https://{server_name}/.well-known/matrix/server");
        if let Ok(resp) = client.get(&url).send().await {
            if resp.status().is_success() {
                if let Ok(parsed) = resp.json::<WellKnownServer>().await {
                    if !parsed.server.is_empty() {
                        let delegated = if has_explicit_port(&parsed.server) {
                            parsed.server
                        } else {
                            format!("{}:8448", parsed.server)
                        };
                        return format!("https://{delegated}");
                    }
                }
            }
        }
    }

    format!("https://{server_name}:8448")
}

/// Reports whether a Matrix server name carries an explicit port
/// (host:port, or [ipv6]:port).
fn has_explicit_port(server_name: &str) -> bool {
    if let Some(rest) = server_name.strip_prefix('[') {
        // IPv6 literal: a port only exists after the closing bracket.
        return rest
            .rsplit_once(']')
            .is_some_and(|(_, after)| after.starts_with(':'));
    }
    server_name.contains(':')
}

// ─────────────────────────────────────────────────────────────────────────────

#[cfg(test)]
pub(crate) mod test_support {
    use std::net::SocketAddr;

    use axum::Router;
    use tokio::task::JoinHandle;

    /// Spawns an in-process HTTP server for the given router and returns its
    /// base URL. The server task is aborted when the guard drops.
    pub struct TestHttpServer {
        pub url: String,
        handle: JoinHandle<()>,
    }

    impl Drop for TestHttpServer {
        fn drop(&mut self) {
            self.handle.abort();
        }
    }

    pub async fn spawn_http_server(router: Router) -> TestHttpServer {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let handle = tokio::spawn(async move {
            axum::serve(listener, router).await.unwrap();
        });
        TestHttpServer {
            url: format!("http://{addr}"),
            handle,
        }
    }

    /// Spawns an in-process HTTPS server with a self-signed certificate and
    /// returns its address. The server task keeps running until the test's
    /// runtime shuts down.
    pub async fn spawn_https_server(router: Router) -> SocketAddr {
        // Multiple rustls crypto backends are enabled in the dependency
        // graph, so the process-level provider must be picked explicitly.
        let _ = rustls::crypto::ring::default_provider().install_default();
        let cert = rcgen::generate_simple_self_signed(vec!["localhost".into(), "127.0.0.1".into()])
            .unwrap();
        let config = axum_server::tls_rustls::RustlsConfig::from_der(
            vec![cert.cert.der().to_vec()],
            cert.key_pair.serialize_der(),
        )
        .await
        .unwrap();

        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum_server::from_tcp_rustls(listener, config)
                .serve(router.into_make_service())
                .await
                .unwrap();
        });
        addr
    }
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::{AtomicU32, Ordering};
    use std::sync::Mutex;

    use axum::extract::Request;
    use axum::routing::any;
    use axum::Router;

    use super::test_support::*;
    use super::*;

    // ── new_unique_id ─────────────────────────────────────────────────────────

    /// Verifies that each call to new_unique_id() returns a unique ID.
    #[test]
    fn test_new_unique_id_uniqueness() {
        const COUNT: usize = 1000;
        let mut ids = std::collections::HashSet::with_capacity(COUNT);
        for i in 0..COUNT {
            let id = new_unique_id();
            assert!(
                ids.insert(id.clone()),
                "duplicate ID generated: {id} (iteration {i})"
            );
        }
        assert_eq!(ids.len(), COUNT);
    }

    /// Verifies that subsequent IDs maintain chronological (lexicographic)
    /// order because the timestamp occupies the most significant bytes and
    /// Base32Hex preserves byte order.
    #[test]
    fn test_new_unique_id_chronological_order() {
        const COUNT: usize = 100;
        let mut ids = Vec::with_capacity(COUNT);
        for _ in 0..COUNT {
            ids.push(new_unique_id());
            std::thread::sleep(Duration::from_millis(1));
        }
        for i in 1..COUNT {
            assert!(
                ids[i - 1].0 < ids[i].0,
                "chronological order violated at index {i}: {} >= {}",
                ids[i - 1],
                ids[i]
            );
        }
    }

    /// Verifies that generated IDs have the correct length and only contain
    /// valid Base32Hex characters (0-9, A-V, no padding).
    #[test]
    fn test_new_unique_id_format() {
        let id = new_unique_id();
        // 16 bytes → Base32Hex without padding: ceil(16*8/5) = 26 characters.
        const EXPECTED_LEN: usize = 26;
        assert_eq!(
            id.0.len(),
            EXPECTED_LEN,
            "expected ID length {EXPECTED_LEN}, got: {id}"
        );
        const VALID_CHARS: &str = "0123456789ABCDEFGHIJKLMNOPQRSTUV";
        for ch in id.0.chars() {
            assert!(
                VALID_CHARS.contains(ch),
                "invalid character {ch:?} in ID {id}"
            );
        }
    }

    /// Verifies that generated IDs are never empty.
    #[test]
    fn test_new_unique_id_never_empty() {
        for _ in 0..100 {
            assert!(!new_unique_id().0.is_empty(), "generated empty UniqueId");
        }
    }

    /// Verifies that UniqueId round-trips through string conversion without
    /// loss.
    #[test]
    fn test_new_unique_id_string_conversion() {
        let id = new_unique_id();
        let id_again = UniqueId(id.0.clone());
        assert_eq!(id, id_again, "round-trip conversion failed");
    }

    // ── livekit_room_alias_for ────────────────────────────────────────────────

    /// Verifies against the test vector from the spec proposal to ensure
    /// compliance with the expected hashing and encoding scheme.
    /// https://github.com/hughns/matrix-spec-proposals/blob/hughns/matrixrtc-livekit/proposals/4195-matrixrtc-livekit.md#appendix-hash-derivation-test-vectors
    #[test]
    fn test_livekit_room_alias_for_test_vector() {
        let id = livekit_room_alias_for("!roomid:example.com", "slot1234");
        let want_id = "O8437W3+jmzMVjoIP3tNwbm+XxHQk2iKpOA7aqw3qSc";
        assert_eq!(id.0, want_id, "livekit_room_alias_for test vector mismatch");
    }

    /// Verifies that the same inputs always produce the same alias.
    #[test]
    fn test_livekit_room_alias_for_deterministic() {
        let a1 = livekit_room_alias_for("!room:example.com", "m.call#ROOM");
        let a2 = livekit_room_alias_for("!room:example.com", "m.call#ROOM");
        assert_eq!(a1, a2, "same inputs produced different aliases");
    }

    /// Verifies that different inputs produce different aliases.
    #[test]
    fn test_livekit_room_alias_for_sample_inputs_distinct() {
        let cases = [
            ("!room1:example.com", "m.call#ROOM"),
            ("!room2:example.com", "m.call#ROOM"),
            ("!room1:example.com", "m.call#OTHER"),
            ("", ""),
        ];
        let mut seen: HashMap<LiveKitRoomAlias, (&str, &str)> = HashMap::new();
        for c in cases {
            let alias = livekit_room_alias_for(c.0, c.1);
            if let Some(prev) = seen.get(&alias) {
                panic!("collision: {c:?} and {prev:?} produced the same alias {alias}");
            }
            seen.insert(alias, c);
        }
    }

    /// Verifies that the alias is a non-empty unpadded Base64 string (no
    /// trailing '=').
    #[test]
    fn test_livekit_room_alias_for_format() {
        let alias = livekit_room_alias_for("!room:example.com", "m.call#ROOM");
        assert!(!alias.0.is_empty(), "alias is empty");
        assert!(
            !alias.0.contains('='),
            "alias contains padding '=': {alias}"
        );
    }

    // ── livekit_identity_for ──────────────────────────────────────────────────

    /// Verifies against the test vector from the spec proposal to ensure
    /// compliance with the expected hashing and encoding scheme.
    /// https://github.com/hughns/matrix-spec-proposals/blob/hughns/matrixrtc-livekit/proposals/4195-matrixrtc-livekit.md#appendix-hash-derivation-test-vectors
    #[test]
    fn test_livekit_identity_for_test_vector() {
        let id = livekit_identity_for("@alice:example.com", "DEVICE123", "memberABC");
        let want_id = "J+T45tGruxc+HrUOqJJlyQSV33m728Cme4+vt8/SWrU";
        assert_eq!(id.0, want_id, "livekit_identity_for test vector mismatch");
    }

    /// Verifies that the same inputs always produce the same identity.
    #[test]
    fn test_livekit_identity_for_deterministic() {
        let id1 = livekit_identity_for("@user:example.com", "DEVICEID", "memberID");
        let id2 = livekit_identity_for("@user:example.com", "DEVICEID", "memberID");
        assert_eq!(id1, id2, "same inputs produced different identities");
    }

    /// Verifies that different inputs produce different identities.
    #[test]
    fn test_livekit_identity_for_sample_inputs_distinct() {
        let cases = [
            ("@alice:example.com", "DEV1", "mem1"),
            ("@bob:example.com", "DEV1", "mem1"),
            ("@alice:example.com", "DEV2", "mem1"),
            ("@alice:example.com", "DEV1", "mem2"),
        ];
        let mut seen: HashMap<LiveKitIdentity, (&str, &str, &str)> = HashMap::new();
        for c in cases {
            let id = livekit_identity_for(c.0, c.1, c.2);
            if let Some(prev) = seen.get(&id) {
                panic!("collision: {c:?} and {prev:?} produced the same identity {id}");
            }
            seen.insert(id, c);
        }
    }

    /// Verifies that the identity is a non-empty unpadded Base64 string.
    #[test]
    fn test_livekit_identity_for_format() {
        let id = livekit_identity_for("@user:example.com", "DEVICEID", "memberID");
        assert!(!id.0.is_empty(), "identity is empty");
        assert!(!id.0.contains('='), "identity contains padding '=': {id}");
    }

    // ── execute_delayed_event_action ──────────────────────────────────────────

    async fn exec(url: &str, delay_id: &str, action: DelayEventAction) -> Result<u16, ActionError> {
        RealDeps
            .execute_delayed_event_action(&CsApiUrl(url.to_owned()), delay_id, action)
            .await
    }

    /// Verifies that a 200 OK response returns the status code without error.
    #[tokio::test]
    async fn test_execute_delayed_event_action_success() {
        let router = Router::new().route(
            "/{*path}",
            any(|req: Request| async move {
                assert_eq!(req.method(), http::Method::POST, "expected POST");
                http::StatusCode::OK
            }),
        );
        let server = spawn_http_server(router).await;

        let status = exec(&server.url, "delay-id-1", DelayEventAction::Restart)
            .await
            .expect("unexpected error");
        assert_eq!(status, 200);
    }

    /// Verifies that the request URL is built correctly from base URL,
    /// delay ID, and action.
    #[tokio::test]
    async fn test_execute_delayed_event_action_url_construction() {
        let captured: Arc<Mutex<String>> = Arc::new(Mutex::new(String::new()));
        let captured_clone = captured.clone();
        let router = Router::new().route(
            "/{*path}",
            any(move |req: Request| {
                let captured = captured_clone.clone();
                async move {
                    *captured.lock().unwrap() = req.uri().path().to_owned();
                    http::StatusCode::OK
                }
            }),
        );
        let server = spawn_http_server(router).await;

        let _ = exec(&server.url, "myDelayID", DelayEventAction::Send).await;
        let expected = format!("{DELAYED_EVENTS_ENDPOINT}/myDelayID/send");
        assert_eq!(*captured.lock().unwrap(), expected);
    }

    /// Verifies that a delay_id containing path-traversal sequences or special
    /// characters is escaped and does not alter the request path beyond the
    /// intended segment.
    #[tokio::test]
    async fn test_execute_delayed_event_action_path_escaping() {
        let captured: Arc<Mutex<String>> = Arc::new(Mutex::new(String::new()));
        let captured_clone = captured.clone();
        let router = Router::new().route(
            "/{*path}",
            any(move |req: Request| {
                let captured = captured_clone.clone();
                async move {
                    *captured.lock().unwrap() = req.uri().path().to_owned();
                    http::StatusCode::OK
                }
            }),
        );
        let server = spawn_http_server(router).await;

        let malicious_id = "../../../admin";
        let _ = exec(&server.url, malicious_id, DelayEventAction::Send).await;

        // The path must not contain an unescaped ".." segment that would
        // traverse outside the expected endpoint prefix.
        let path = captured.lock().unwrap().clone();
        assert!(!path.is_empty(), "no request captured");
        for seg in path.split('/') {
            assert_ne!(
                seg, "..",
                "path contains unescaped traversal segment (full path: {path})"
            );
        }
    }

    /// Verifies that a 404 for ActionSend is treated as success (delayed event
    /// already sent/cancelled) and returns the status code without error.
    #[tokio::test]
    async fn test_execute_delayed_event_action_404_on_send() {
        let router = Router::new().route("/{*path}", any(|| async { http::StatusCode::NOT_FOUND }));
        let server = spawn_http_server(router).await;

        let status = exec(&server.url, "gone-id", DelayEventAction::Send)
            .await
            .expect("expected no error for ActionSend 404");
        assert_eq!(status, 404);
    }

    /// Verifies that a 404 for ActionRestart returns the DelayedEventNotFound
    /// error so callers can map it to the DelayedEventNotFound signal (vs. the
    /// generic transient/permanent failure path). This is the contract
    /// differentiator from 404-on-Send, which is treated as success per
    /// MSC-4140.
    #[tokio::test]
    async fn test_execute_delayed_event_action_404_on_restart() {
        let router = Router::new().route("/{*path}", any(|| async { http::StatusCode::NOT_FOUND }));
        let server = spawn_http_server(router).await;

        let err = exec(&server.url, "gone-id", DelayEventAction::Restart)
            .await
            .expect_err("expected DelayedEventNotFound for 404 on ActionRestart");
        assert!(
            err.is_delayed_event_not_found(),
            "expected DelayedEventNotFound, got {err:?}"
        );
        assert_eq!(err.status(), 404);
    }

    /// Verifies that a 429 response with a Retry-After header (both seconds
    /// and HTTP-date formats) is converted to a RetryAfter error with the
    /// correct duration.
    #[tokio::test]
    async fn test_execute_delayed_event_action_retry_after_formats() {
        let tests = [
            ("Retry-After as seconds", None, 30i64),
            (
                "Retry-After as HTTP date",
                Some(Duration::from_secs(10)),
                10i64,
            ),
        ];
        for (name, http_date_offset, expected_min_seconds) in tests {
            // Compute the header value just before issuing the request so the
            // date-based case is not skewed by earlier cases' runtime.
            let value = match http_date_offset {
                Some(offset) => httpdate::fmt_http_date(SystemTime::now() + offset),
                None => expected_min_seconds.to_string(),
            };
            let router = Router::new().route(
                "/{*path}",
                any(move || {
                    let value = value.clone();
                    async move {
                        (
                            http::StatusCode::TOO_MANY_REQUESTS,
                            [(http::header::RETRY_AFTER, value)],
                        )
                    }
                }),
            );
            let server = spawn_http_server(router).await;

            let err = exec(&server.url, "id", DelayEventAction::Send)
                .await
                .expect_err("expected error for 429");
            assert_eq!(err.status(), 429, "{name}: expected status 429");
            let ActionError::RetryAfter { retry_after, .. } = &err else {
                panic!("{name}: expected RetryAfter, got {err:?}");
            };
            // Allow ±1s tolerance for clock jitter between header generation
            // and assertion.
            let diff = expected_min_seconds - retry_after.as_secs() as i64;
            assert!(
                diff <= 1,
                "{name}: retry duration too short: expected ~{expected_min_seconds}s, got {retry_after:?}"
            );
        }
    }

    /// Verifies that a 429 without a usable Retry-After returns a plain
    /// transient error (so retry loops will retry with the default interval)
    /// and that the 429 status code is still surfaced to the caller for
    /// logging.
    #[tokio::test]
    async fn test_execute_delayed_event_action_429_without_retry_after() {
        let router = Router::new().route(
            "/{*path}",
            any(|| async { http::StatusCode::TOO_MANY_REQUESTS }),
        );
        let server = spawn_http_server(router).await;

        let err = exec(&server.url, "id", DelayEventAction::Send)
            .await
            .expect_err("expected transient error for 429 without Retry-After");
        assert!(
            !matches!(err, ActionError::RetryAfter { .. }),
            "did not expect RetryAfter, got {err:?}"
        );
        assert_eq!(err.status(), 429);
    }

    /// Verifies the Matrix-specific body fallback: when a 429 omits the
    /// Retry-After header (or its value is unparseable) but the response body
    /// carries the M_LIMIT_EXCEEDED `retry_after_ms` field, the helper honours
    /// it. Matrix spec v1.10 deprecated this in favour of the standard
    /// Retry-After header but Synapse/Dendrite/Conduit still emit it for
    /// backwards compatibility.
    #[tokio::test]
    async fn test_execute_delayed_event_action_429_matrix_retry_after_ms() {
        struct Case {
            name: &'static str,
            header: &'static str, // value of Retry-After (empty = unset)
            body: &'static str,   // raw response body
            want_retry_after: bool,
            want_min_seconds: u64,
        }
        for tc in [
            Case {
                name: "body retry_after_ms — 2000 ms → 2 s",
                header: "",
                body: r#"{"errcode":"M_LIMIT_EXCEEDED","retry_after_ms":2000}"#,
                want_retry_after: true,
                want_min_seconds: 2,
            },
            Case {
                name: "body retry_after_ms — sub-second 500 ms ceils to 1 s",
                header: "",
                body: r#"{"errcode":"M_LIMIT_EXCEEDED","retry_after_ms":500}"#,
                want_retry_after: true,
                want_min_seconds: 1,
            },
            Case {
                name: "header wins when both present",
                header: "10",
                body: r#"{"errcode":"M_LIMIT_EXCEEDED","retry_after_ms":30000}"#,
                want_retry_after: true,
                want_min_seconds: 10, // header value, not the body's 30
            },
            Case {
                name: "retry_after_ms = 0 → fall through to transient err",
                header: "",
                body: r#"{"errcode":"M_LIMIT_EXCEEDED","retry_after_ms":0}"#,
                want_retry_after: false,
                want_min_seconds: 0,
            },
            Case {
                name: "negative retry_after_ms → fall through to transient err",
                header: "",
                body: r#"{"errcode":"M_LIMIT_EXCEEDED","retry_after_ms":-100}"#,
                want_retry_after: false,
                want_min_seconds: 0,
            },
            Case {
                name: "invalid JSON body → fall through to transient err",
                header: "",
                body: "not json {",
                want_retry_after: false,
                want_min_seconds: 0,
            },
            Case {
                name: "body without retry_after_ms field → fall through",
                header: "",
                body: r#"{"errcode":"M_LIMIT_EXCEEDED","error":"slow down"}"#,
                want_retry_after: false,
                want_min_seconds: 0,
            },
        ] {
            let header = tc.header;
            let body = tc.body;
            let router = Router::new().route(
                "/{*path}",
                any(move || async move {
                    let mut resp =
                        http::Response::builder().status(http::StatusCode::TOO_MANY_REQUESTS);
                    if !header.is_empty() {
                        resp = resp.header(http::header::RETRY_AFTER, header);
                    }
                    resp.body(axum::body::Body::from(body)).unwrap()
                }),
            );
            let server = spawn_http_server(router).await;

            let err = exec(&server.url, "id", DelayEventAction::Send)
                .await
                .expect_err("expected error for 429");
            assert_eq!(err.status(), 429, "{}: expected status 429", tc.name);
            match (&err, tc.want_retry_after) {
                (ActionError::RetryAfter { retry_after, .. }, true) => {
                    assert!(
                        retry_after.as_secs() >= tc.want_min_seconds,
                        "{}: retry duration too short: got {retry_after:?}, want at least {}s",
                        tc.name,
                        tc.want_min_seconds
                    );
                }
                (ActionError::RetryAfter { .. }, false) => {
                    panic!(
                        "{}: RetryAfterError = true, want false (err: {err:?})",
                        tc.name
                    );
                }
                (_, true) => {
                    panic!(
                        "{}: RetryAfterError = false, want true (err: {err:?})",
                        tc.name
                    );
                }
                (_, false) => {}
            }
        }
    }

    /// Verifies that a 502 returns a transient error and surfaces the status
    /// code for logging.
    #[tokio::test]
    async fn test_execute_delayed_event_action_502_bad_gateway() {
        let router =
            Router::new().route("/{*path}", any(|| async { http::StatusCode::BAD_GATEWAY }));
        let server = spawn_http_server(router).await;

        let err = exec(&server.url, "id", DelayEventAction::Send)
            .await
            .expect_err("expected error for 502");
        assert_eq!(err.status(), 502);
    }

    /// Verifies that a 500 returns a transient error and surfaces the status
    /// code for logging.
    #[tokio::test]
    async fn test_execute_delayed_event_action_500_internal_server_error() {
        let router = Router::new().route(
            "/{*path}",
            any(|| async { http::StatusCode::INTERNAL_SERVER_ERROR }),
        );
        let server = spawn_http_server(router).await;

        let err = exec(&server.url, "id", DelayEventAction::Send)
            .await
            .expect_err("expected error for 500");
        assert_eq!(err.status(), 500);
    }

    /// Verifies that the generalized "any 5xx" branch returns a transient
    /// error for the common retryable codes — 500, 502, 503, 504 — plus a
    /// less common one (507) to lock the contract in place. None of these is
    /// a RetryAfter error; they all let the retry loop use its default
    /// schedule.
    #[tokio::test]
    async fn test_execute_delayed_event_action_5xx_are_all_transient() {
        for code in [500u16, 502, 503, 504, 507] {
            let status_code = http::StatusCode::from_u16(code).unwrap();
            let router = Router::new().route("/{*path}", any(move || async move { status_code }));
            let server = spawn_http_server(router).await;

            let err = exec(&server.url, "id", DelayEventAction::Send)
                .await
                .expect_err(&format!("expected transient error for {code}"));
            assert!(
                !matches!(err, ActionError::RetryAfter { .. }),
                "did not expect RetryAfter for {code}, got {err:?}"
            );
            assert_eq!(err.status(), code, "expected status {code}");
        }
    }

    /// Verifies the catch-all branch: any status code the classifier doesn't
    /// know (genuine transients like 408 / 421 / 423 / 425, oddball 4xx like
    /// 400 / 418, …) returns a non-Permanent error so retry loops keep trying
    /// until the elapsed budget expires. Locks in two things at once:
    ///
    ///   - the explicit-success contract (Ok means success — these don't
    ///     silently slip through);
    ///   - the "transient by default" policy (only DelayedEventNotFound is
    ///     permanent; everything else gets the benefit of retry budget).
    #[tokio::test]
    async fn test_execute_delayed_event_action_unknown_status_are_transient() {
        for code in [
            400u16, // permanent in spirit, but retry budget is cheap
            401, 403, 408, // actually transient
            421, // actually transient
            423, // actually transient
            425, // actually transient
            418,
        ] {
            let status_code = http::StatusCode::from_u16(code).unwrap();
            let router = Router::new().route("/{*path}", any(move || async move { status_code }));
            let server = spawn_http_server(router).await;

            let err = exec(&server.url, "id", DelayEventAction::Send)
                .await
                .expect_err(&format!("expected unexpected-status error for {code}"));
            assert_eq!(err.status(), code, "expected status {code}");
            // Not a RetryAfter (only 429 with parseable Retry-After is).
            assert!(
                !matches!(err, ActionError::RetryAfter { .. }),
                "did not expect RetryAfter for {code}, got {err:?}"
            );
            // Not Permanent — the retry loop should keep trying.
            assert_ne!(
                err.classify(),
                ErrorClass::Permanent,
                "did not expect a permanent error for {code} (let the loop retry), got {err:?}"
            );
        }
    }

    /// Verifies that a connection error is returned as an error.
    #[tokio::test]
    async fn test_execute_delayed_event_action_network_error() {
        // Use a closed server to provoke a connection error.
        let server = spawn_http_server(Router::new()).await;
        let url = server.url.clone();
        drop(server); // close immediately

        let result = exec(&url, "id", DelayEventAction::Send).await;
        assert!(result.is_err(), "expected a network error, got {result:?}");
    }

    /// Verifies that requests carry the correct Content-Type header.
    #[tokio::test]
    async fn test_execute_delayed_event_action_content_type() {
        let captured: Arc<Mutex<String>> = Arc::new(Mutex::new(String::new()));
        let captured_clone = captured.clone();
        let router = Router::new().route(
            "/{*path}",
            any(move |req: Request| {
                let captured = captured_clone.clone();
                async move {
                    *captured.lock().unwrap() = req
                        .headers()
                        .get(http::header::CONTENT_TYPE)
                        .and_then(|v| v.to_str().ok())
                        .unwrap_or_default()
                        .to_owned();
                    http::StatusCode::OK
                }
            }),
        );
        let server = spawn_http_server(router).await;

        let _ = exec(&server.url, "id", DelayEventAction::Restart).await;
        assert_eq!(*captured.lock().unwrap(), "application/json");
    }

    // ── resolve_cs_api_url ────────────────────────────────────────────────────

    /// A Deps implementation whose discover_client_api is backed by a
    /// swappable closure — the equivalent of Go's `discoverClientAPI = ...`.
    type DiscoverFn = Box<dyn Fn(&str) -> Result<Option<ClientWellKnown>, String> + Send + Sync>;

    struct DiscoverMockDeps {
        discover: DiscoverFn,
    }

    #[async_trait]
    impl Deps for DiscoverMockDeps {
        async fn discover_client_api(
            &self,
            server_name: &str,
        ) -> Result<Option<ClientWellKnown>, String> {
            (self.discover)(server_name)
        }
    }

    fn discover_must_not_be_called() -> DiscoverMockDeps {
        DiscoverMockDeps {
            discover: Box::new(|_| {
                panic!("discover_client_api should not be called");
            }),
        }
    }

    #[tokio::test]
    async fn test_resolve_cs_api_url_prefers_override_without_cache() {
        let deps = discover_must_not_be_called();
        let overrides = HashMap::from([(
            "example.com".to_owned(),
            CsApiUrl("https://matrix-client.example.com".into()),
        )]);

        let got = deps
            .resolve_cs_api_url("example.com", &overrides, None)
            .await
            .expect("unexpected error");
        assert_eq!(got.as_str(), "https://matrix-client.example.com");
    }

    #[tokio::test]
    async fn test_resolve_cs_api_url_prefers_override_despite_cache() {
        let deps = discover_must_not_be_called();
        let cache = CsApiUrlCache::new();
        cache.set(
            "example.com",
            CsApiUrl("https://client.example.com".into()),
            Duration::from_secs(4 * 60 * 60),
        );
        let overrides = HashMap::from([(
            "example.com".to_owned(),
            CsApiUrl("https://matrix-client.example.com".into()),
        )]);

        let got = deps
            .resolve_cs_api_url("example.com", &overrides, Some(&cache))
            .await
            .expect("unexpected error");
        assert_eq!(got.as_str(), "https://matrix-client.example.com");
    }

    #[tokio::test]
    async fn test_resolve_cs_api_url_falls_back_to_cache_without_overrides() {
        let deps = discover_must_not_be_called();
        let cache = CsApiUrlCache::new();
        cache.set(
            "example.com",
            CsApiUrl("https://matrix-client.example.com".into()),
            Duration::from_secs(4 * 60 * 60),
        );

        let got = deps
            .resolve_cs_api_url("example.com", &HashMap::new(), Some(&cache))
            .await
            .expect("unexpected error");
        assert_eq!(got.as_str(), "https://matrix-client.example.com");
    }

    #[tokio::test]
    async fn test_resolve_cs_api_url_resolves_from_well_known() {
        let deps = DiscoverMockDeps {
            discover: Box::new(|_| {
                Ok(Some(ClientWellKnown {
                    homeserver_base_url: "https://matrix-client.example.com".into(),
                }))
            }),
        };

        let got = deps
            .resolve_cs_api_url("example.com", &HashMap::new(), None)
            .await
            .expect("unexpected error");
        assert_eq!(got.as_str(), "https://matrix-client.example.com");
    }

    #[tokio::test]
    async fn test_resolve_cs_api_url_resolves_from_well_known_and_caches() {
        let deps = DiscoverMockDeps {
            discover: Box::new(|_| {
                Ok(Some(ClientWellKnown {
                    homeserver_base_url: "https://matrix-client.example.com".into(),
                }))
            }),
        };
        let cache = CsApiUrlCache::new();

        let got = deps
            .resolve_cs_api_url("example.com", &HashMap::new(), Some(&cache))
            .await
            .expect("unexpected error");
        assert_eq!(got.as_str(), "https://matrix-client.example.com");
        let cached = cache.get("example.com").expect("expected cache entry");
        assert_eq!(cached.as_str(), "https://matrix-client.example.com");
    }

    #[tokio::test]
    async fn test_resolve_cs_api_url_fails_when_well_known_yields_empty_base_url() {
        let deps = DiscoverMockDeps {
            discover: Box::new(|_| {
                Ok(Some(ClientWellKnown {
                    homeserver_base_url: String::new(),
                }))
            }),
        };

        let result = deps
            .resolve_cs_api_url("example.com", &HashMap::new(), None)
            .await;
        assert!(
            result.is_err(),
            "expected error when well-known BaseURL is empty"
        );
    }

    #[tokio::test]
    async fn test_resolve_cs_api_url_fails_when_well_known_yields_nil_response() {
        let deps = DiscoverMockDeps {
            discover: Box::new(|_| Ok(None)),
        };

        let result = deps
            .resolve_cs_api_url("example.com", &HashMap::new(), None)
            .await;
        assert!(
            result.is_err(),
            "expected error for nil well-known response"
        );
    }

    // ── exchange_openid_userinfo ──────────────────────────────────────────────

    /// Verifies the end-to-end OpenID userinfo lookup: exchange_openid_userinfo
    /// builds the correct federation request, parses the response body, and
    /// returns the sub. Endpoint happy-path tests mock this function to focus
    /// on endpoint behaviour — this is its dedicated home.
    #[tokio::test]
    async fn test_exchange_openid_userinfo_success() {
        const ACCESS_TOKEN: &str = "testAccessToken";

        // The server name (with port) is only known after binding, so the
        // handler reads it back from a shared cell.
        let server_name: Arc<Mutex<String>> = Arc::new(Mutex::new(String::new()));
        let server_name_clone = server_name.clone();
        let router = Router::new().route(
            "/{*path}",
            any(move |req: Request| {
                let server_name = server_name_clone.clone();
                async move {
                    assert_eq!(
                        req.uri().path(),
                        "/_matrix/federation/v1/openid/userinfo",
                        "unexpected path"
                    );
                    let query = req.uri().query().unwrap_or_default();
                    assert!(
                        query.contains(&format!("access_token={ACCESS_TOKEN}")),
                        "access_token missing from query: {query}"
                    );
                    let name = server_name.lock().unwrap().clone();
                    (
                        [(http::header::CONTENT_TYPE, "application/json")],
                        format!(r#"{{"sub": "@alice:{name}"}}"#),
                    )
                }
            }),
        );
        let addr = test_support::spawn_https_server(router).await;
        let matrix_server_name = format!("127.0.0.1:{}", addr.port());
        *server_name.lock().unwrap() = matrix_server_name.clone();

        let info = RealDeps
            .exchange_openid_userinfo(
                &OpenIdTokenType {
                    access_token: ACCESS_TOKEN.into(),
                    matrix_server_name: matrix_server_name.clone(),
                    ..Default::default()
                },
                true, // skip_verify_tls — required for the self-signed test server
            )
            .await
            .expect("unexpected error");
        assert_eq!(info.sub, format!("@alice:{matrix_server_name}"));
    }

    // ── create_livekit_room ───────────────────────────────────────────────────

    /// A Deps implementation returning a mock room service client — the
    /// equivalent of Go's `newRoomServiceClient = ...` swap.
    type CreateRoomFn =
        Box<dyn Fn(CreateRoomParams) -> Result<RoomInfo, RoomServiceError> + Send + Sync>;
    type GetParticipantFn = Box<dyn Fn(&str, &str) -> Result<(), RoomServiceError> + Send + Sync>;

    #[derive(Default)]
    struct MockRoomServiceClient {
        create_room_fn: Option<CreateRoomFn>,
        get_participant_fn: Option<GetParticipantFn>,
    }

    #[async_trait]
    impl RoomServiceClient for MockRoomServiceClient {
        async fn create_room(
            &self,
            params: CreateRoomParams,
        ) -> Result<RoomInfo, RoomServiceError> {
            match &self.create_room_fn {
                Some(f) => f(params),
                None => Ok(RoomInfo::default()),
            }
        }

        async fn get_participant(
            &self,
            room: &str,
            identity: &str,
        ) -> Result<(), RoomServiceError> {
            match &self.get_participant_fn {
                Some(f) => f(room, identity),
                None => Ok(()),
            }
        }
    }

    struct RoomClientMockDeps {
        client: Arc<MockRoomServiceClient>,
    }

    impl Deps for RoomClientMockDeps {
        fn new_room_service_client(
            &self,
            _url: &str,
            _key: &str,
            _secret: &str,
        ) -> Arc<dyn RoomServiceClient> {
            self.client.clone()
        }
    }

    /// Verifies that create_livekit_room successfully creates a new room and
    /// properly detects it as newly created.
    #[tokio::test]
    async fn test_create_livekit_room_success_creating_new_room() {
        let livekit_auth = LiveKitAuth {
            key: "test-key".into(),
            secret: "test-secret".into(),
            lk_url: "http://localhost:55002".into(),
        };
        let room_alias = LiveKitRoomAlias("test-room-alias".into());
        let identity = LiveKitIdentity("test-identity".into());
        let matrix_user = "@user:example.com";

        let creation_start = unix_now();
        let expected_room = room_alias.clone();
        let deps = RoomClientMockDeps {
            client: Arc::new(MockRoomServiceClient {
                create_room_fn: Some(Box::new(move |req| {
                    assert_eq!(req.name, expected_room.0, "expected room name to match");
                    assert_eq!(req.empty_timeout, 5 * 60, "expected empty timeout 300s");
                    assert_eq!(req.departure_timeout, 20, "expected departure timeout 20s");
                    assert_eq!(req.max_participants, 0, "expected max participants 0");
                    Ok(RoomInfo {
                        sid: "room-sid-123".into(),
                        // Room created after our start marker.
                        creation_time: creation_start + 1,
                    })
                })),
                get_participant_fn: None,
            }),
        };

        deps.create_livekit_room(&livekit_auth, &room_alias, matrix_user, &identity)
            .await
            .expect("expected no error");
    }

    /// Verifies that create_livekit_room succeeds when the room already exists
    /// (creation time is before our start marker).
    #[tokio::test]
    async fn test_create_livekit_room_success_using_existing_room() {
        let livekit_auth = LiveKitAuth {
            key: "test-key".into(),
            secret: "test-secret".into(),
            lk_url: "http://localhost:55002".into(),
        };

        let creation_start = unix_now();
        let deps = RoomClientMockDeps {
            client: Arc::new(MockRoomServiceClient {
                create_room_fn: Some(Box::new(move |_| {
                    Ok(RoomInfo {
                        sid: "room-sid-456".into(),
                        // Room created long ago.
                        creation_time: creation_start - 100,
                    })
                })),
                get_participant_fn: None,
            }),
        };

        deps.create_livekit_room(
            &livekit_auth,
            &LiveKitRoomAlias("existing-room".into()),
            "@user:example.com",
            &LiveKitIdentity("test-identity".into()),
        )
        .await
        .expect("expected no error");
    }

    /// Verifies that errors from the LiveKit SDK are properly wrapped and
    /// returned with the room alias in the message.
    #[tokio::test]
    async fn test_create_livekit_room_error_handling() {
        let livekit_auth = LiveKitAuth {
            key: "test-key".into(),
            secret: "test-secret".into(),
            lk_url: "http://localhost:55002".into(),
        };
        let room_alias = LiveKitRoomAlias("failed-room".into());

        let deps = RoomClientMockDeps {
            client: Arc::new(MockRoomServiceClient {
                create_room_fn: Some(Box::new(|_| {
                    Err(RoomServiceError::Other("SDK connection failed".into()))
                })),
                get_participant_fn: None,
            }),
        };

        let err = deps
            .create_livekit_room(
                &livekit_auth,
                &room_alias,
                "@user:example.com",
                &LiveKitIdentity("test-identity".into()),
            )
            .await
            .expect_err("expected error");
        assert!(
            err.contains(room_alias.as_str()),
            "error should mention room alias, got: {err}"
        );
    }

    /// Verifies that the room is created with correct timeout configuration.
    #[tokio::test]
    async fn test_create_livekit_room_room_configuration_parameters() {
        let livekit_auth = LiveKitAuth {
            key: "test-key".into(),
            secret: "test-secret".into(),
            lk_url: "http://localhost:55002".into(),
        };

        let captured: Arc<Mutex<Option<CreateRoomParams>>> = Arc::new(Mutex::new(None));
        let captured_clone = captured.clone();
        let deps = RoomClientMockDeps {
            client: Arc::new(MockRoomServiceClient {
                create_room_fn: Some(Box::new(move |req| {
                    *captured_clone.lock().unwrap() = Some(req);
                    Ok(RoomInfo {
                        sid: "test".into(),
                        creation_time: unix_now(),
                    })
                })),
                get_participant_fn: None,
            }),
        };

        let _ = deps
            .create_livekit_room(
                &livekit_auth,
                &LiveKitRoomAlias("room".into()),
                "@user:example.com",
                &LiveKitIdentity("id".into()),
            )
            .await;

        let captured = captured.lock().unwrap();
        let req = captured.as_ref().expect("create_room was not called");
        assert_eq!(req.empty_timeout, 5 * 60, "expected empty timeout 300s");
        assert_eq!(req.departure_timeout, 20, "expected departure timeout 20s");
        assert_eq!(
            req.max_participants, 0,
            "expected max participants 0 (unlimited)"
        );
    }

    // ── participant_exists via mocked room client ─────────────────────────────

    /// Verifies the tri-state contract of participant_exists: present, absent
    /// (NotFound) and transport error.
    #[tokio::test]
    async fn test_participant_exists_contract() {
        let auth = LiveKitAuth::default();
        let room = LiveKitRoomAlias("room".into());
        let identity = LiveKitIdentity("id".into());

        let calls = Arc::new(AtomicU32::new(0));
        let calls_clone = calls.clone();
        let deps = RoomClientMockDeps {
            client: Arc::new(MockRoomServiceClient {
                create_room_fn: None,
                get_participant_fn: Some(Box::new(move |_, _| {
                    match calls_clone.fetch_add(1, Ordering::SeqCst) {
                        0 => Ok(()),
                        1 => Err(RoomServiceError::NotFound("not found".into())),
                        _ => Err(RoomServiceError::Other("boom".into())),
                    }
                })),
            }),
        };

        assert!(deps
            .participant_exists(&auth, &room, &identity)
            .await
            .unwrap());
        assert!(!deps
            .participant_exists(&auth, &room, &identity)
            .await
            .unwrap());
        assert!(deps
            .participant_exists(&auth, &room, &identity)
            .await
            .is_err());
    }

    // ── real Twirp room-service client ────────────────────────────────────────
    // Regression tests for the divergences from livekit-api's RoomClient (see
    // the TODO(upstream) on LiveKitRoomServiceClient): a path prefix in
    // LIVEKIT_URL must be preserved and ws/wss schemes must be converted for
    // HTTP service calls, matching the Go SDK.

    /// Verifies the Go SDK ToHttpURL equivalence.
    #[test]
    fn test_to_http_url() {
        for (input, want) in [
            ("ws://host:7880", "http://host:7880"),
            ("wss://host/livekit/sfu", "https://host/livekit/sfu"),
            ("http://host", "http://host"),
            ("https://host/prefix", "https://host/prefix"),
        ] {
            assert_eq!(to_http_url(input), want);
        }
    }

    /// Captures one Twirp request (path + Authorization header) and returns a
    /// canned protobuf response.
    async fn spawn_twirp_server(
        response: Vec<u8>,
        status: http::StatusCode,
    ) -> (TestHttpServer, Arc<Mutex<(String, String)>>) {
        let captured: Arc<Mutex<(String, String)>> = Arc::new(Mutex::new(Default::default()));
        let captured_clone = captured.clone();
        let router = Router::new().route(
            "/{*path}",
            any(move |req: Request| {
                let captured = captured_clone.clone();
                let response = response.clone();
                async move {
                    let auth = req
                        .headers()
                        .get(http::header::AUTHORIZATION)
                        .and_then(|v| v.to_str().ok())
                        .unwrap_or_default()
                        .to_owned();
                    *captured.lock().unwrap() = (req.uri().path().to_owned(), auth);
                    http::Response::builder()
                        .status(status)
                        .header(http::header::CONTENT_TYPE, "application/protobuf")
                        .body(axum::body::Body::from(response))
                        .unwrap()
                }
            }),
        );
        let server = spawn_http_server(router).await;
        (server, captured)
    }

    /// Verifies that a reverse-proxy path prefix in LIVEKIT_URL is preserved
    /// by RoomService calls and that a ws:// URL is converted to http:// —
    /// livekit-api's own RoomClient strips the prefix (url.set_path) and does
    /// not convert the scheme; the Go SDK does both.
    #[tokio::test]
    async fn test_room_service_client_preserves_url_path_prefix() {
        use prost::Message;
        let room = livekit_protocol::Room {
            sid: "room-sid-1".into(),
            creation_time: 42,
            ..Default::default()
        };
        let (server, captured) =
            spawn_twirp_server(room.encode_to_vec(), http::StatusCode::OK).await;

        // ws scheme + path prefix, as configured for reverse-proxied setups.
        let lk_url = format!("ws{}/livekit/sfu", server.url.strip_prefix("http").unwrap());
        let client = RealDeps.new_room_service_client(&lk_url, "devkey", "secret");
        let info = client
            .create_room(CreateRoomParams {
                name: "room".into(),
                empty_timeout: 300,
                departure_timeout: 20,
                max_participants: 0,
            })
            .await
            .expect("create_room failed");
        assert_eq!(info.sid, "room-sid-1");
        assert_eq!(info.creation_time, 42);

        let (path, auth) = captured.lock().unwrap().clone();
        assert_eq!(
            path, "/livekit/sfu/twirp/livekit.RoomService/CreateRoom",
            "path prefix must be preserved"
        );
        assert!(
            auth.starts_with("Bearer "),
            "expected Bearer token, got {auth:?}"
        );
    }

    /// Verifies that GetParticipant reaches the prefixed endpoint and that a
    /// twirp not_found error surfaces as participant-absent (Ok(false)) via
    /// participant_exists.
    #[tokio::test]
    async fn test_room_service_client_get_participant_via_prefix() {
        use prost::Message;

        // Present: 200 with a ParticipantInfo body.
        let participant = livekit_protocol::ParticipantInfo::default();
        let (server, captured) =
            spawn_twirp_server(participant.encode_to_vec(), http::StatusCode::OK).await;
        let auth = LiveKitAuth {
            key: "devkey".into(),
            secret: "secret".into(),
            lk_url: format!("{}/livekit/sfu", server.url),
        };
        let exists = RealDeps
            .participant_exists(
                &auth,
                &LiveKitRoomAlias("r".into()),
                &LiveKitIdentity("i".into()),
            )
            .await
            .expect("participant_exists failed");
        assert!(exists, "expected participant present");
        assert_eq!(
            captured.lock().unwrap().0,
            "/livekit/sfu/twirp/livekit.RoomService/GetParticipant",
            "path prefix must be preserved"
        );

        // Absent: twirp not_found error maps to Ok(false).
        let (server, _captured) = spawn_twirp_server(
            br#"{"code":"not_found","msg":"participant does not exist"}"#.to_vec(),
            http::StatusCode::NOT_FOUND,
        )
        .await;
        let auth = LiveKitAuth {
            key: "devkey".into(),
            secret: "secret".into(),
            lk_url: format!("{}/livekit/sfu", server.url),
        };
        let exists = RealDeps
            .participant_exists(
                &auth,
                &LiveKitRoomAlias("r".into()),
                &LiveKitIdentity("i".into()),
            )
            .await
            .expect("participant_exists failed");
        assert!(!exists, "expected participant confirmed absent");

        // Server error: presence unknown, surfaced as Err.
        let (server, _captured) = spawn_twirp_server(
            br#"{"code":"internal","msg":"boom"}"#.to_vec(),
            http::StatusCode::INTERNAL_SERVER_ERROR,
        )
        .await;
        let auth = LiveKitAuth {
            key: "devkey".into(),
            secret: "secret".into(),
            lk_url: format!("{}/livekit/sfu", server.url),
        };
        let result = RealDeps
            .participant_exists(
                &auth,
                &LiveKitRoomAlias("r".into()),
                &LiveKitIdentity("i".into()),
            )
            .await;
        assert!(
            result.is_err(),
            "expected transport/server error, got {result:?}"
        );
    }
}

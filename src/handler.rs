// Copyright 2025 Element Creations Ltd.
// Copyright 2023 - 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//! The HTTP entry point of the service: request routing, JWT minting and
//! the actor loop that owns the delayed-event jobs.

use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use axum::extract::{Request, State};
use axum::response::{IntoResponse, Response};
use axum::routing::any;
use axum::{Json, Router};
use chrono::Utc;
use http::{header, HeaderValue, Method, StatusCode};
use livekit_api::access_token::{AccessToken, TokenVerifier, VideoGrants};
use livekit_api::webhooks::WebhookReceiver;
use tokio::sync::{mpsc, oneshot, watch};
use tokio_util::sync::CancellationToken;
use tracing::{debug, error, info, warn};

use crate::delayed_event_manager::{
    start_participant_lookup, DelayedEventJob, DelayedEventJobParams, DelayedEventSignal, JobKey,
    JobRestartedRequest, LookupCsApiUrlFn, SfuMessage,
};
use crate::helper::{
    livekit_identity_for, livekit_room_alias_for, CsApiUrl, CsApiUrlCache, Deps, LiveKitAuth,
    LiveKitIdentity, LiveKitRoomAlias, UniqueId,
};
use crate::requests::{
    DelegateDelayedLeaveRequest, DelegateDelayedLeaveResponse, LegacySfuRequest, MatrixErrorBody,
    MatrixErrorResponse, OpenIdTokenType, SfuRequest, SfuResponse,
};
use crate::store::{Store, StoredJob};

/// Mints the LiveKit join JWT for a (room, identity) pair.
pub fn get_join_token(
    api_key: &str,
    api_secret: &str,
    room: &LiveKitRoomAlias,
    identity: &LiveKitIdentity,
) -> Result<String, String> {
    let grants = VideoGrants {
        room_join: true,
        room_create: false,
        can_publish: true,
        can_subscribe: true,
        room: room.0.clone(),
        ..Default::default()
    };

    AccessToken::with_api_key(api_key, api_secret)
        .with_grants(grants)
        .with_identity(identity.as_str())
        .with_ttl(Duration::from_secs(60 * 60))
        .to_jwt()
        .map_err(|e| e.to_string())
}

// ── Handler ──────────────────────────────────────────────────────────────────

/// A request to add a delayed-event job.
pub(crate) struct AddJobRequest {
    params: DelayedEventJobParams,
    result: oneshot::Sender<Result<UniqueId, String>>,
}

/// An SFU webhook event on its way to the loop.
#[derive(Debug, Clone)]
pub(crate) struct SfuEventRequest {
    pub room_alias: LiveKitRoomAlias,
    pub msg: SfuMessage,
}

/// The receiving ends consumed by the handler loop.
pub(crate) struct LoopReceivers {
    add_job_rx: mpsc::Receiver<AddJobRequest>,
    pub(crate) sfu_event_rx: mpsc::Receiver<SfuEventRequest>,
    job_done_rx: mpsc::Receiver<Arc<DelayedEventJob>>,
    job_restarted_rx: mpsc::Receiver<JobRestartedRequest>,
}

/// A persistence operation queued for the store-writer task.
enum StoreOp {
    Save {
        key: JobKey,
        job: StoredJob,
        context: &'static str,
    },
    Delete {
        key: JobKey,
        context: &'static str,
    },
}

/// The error side of add_delayed_event_job.
#[derive(Debug)]
pub(crate) enum AddJobError {
    /// The handler has already shut down.
    Shutdown,
    /// Job creation failed (invalid parameters).
    Creation(String),
}

/// The top-level HTTP handler.
///
/// # Concurrency model
///
/// `run_loop` is the single task that owns the jobs map — no mutex needed.
/// Everything else communicates with the loop through channels:
///
///   - add_job: create a job (replacing any active job for the same key)
///   - sfu_event: route an SFU webhook event to the job for (room, identity)
///   - job_done: clean up a job that reached a terminal state
///   - job_restarted: update the stored job after a delayed-event restart
pub struct Handler {
    cancel: CancellationToken,
    pub(crate) livekit_auth: LiveKitAuth,
    full_access_homeservers: Vec<String>,
    skip_verify_tls: bool,
    /// The period between room-worker sanity checks. Zero disables the sanity
    /// check.
    sanity_check_interval: Duration,
    cs_api_url_overrides: Arc<HashMap<String, CsApiUrl>>,
    cs_api_url_cache: Arc<CsApiUrlCache>,
    store: Option<Arc<dyn Store>>,
    deps: Arc<dyn Deps>,
    /// Set to true when run_loop has exited.
    loop_done_tx: watch::Sender<bool>,
    /// Set to true once start-up recovery of previously stored jobs is
    /// complete.
    recovery_done_tx: watch::Sender<bool>,
    job_done_tx: mpsc::Sender<Arc<DelayedEventJob>>,
    job_restarted_tx: mpsc::Sender<JobRestartedRequest>,
    add_job_tx: mpsc::Sender<AddJobRequest>,
    pub(crate) sfu_event_tx: mpsc::Sender<SfuEventRequest>,
}

impl Handler {
    pub fn new(
        lk_auth: LiveKitAuth,
        skip_verify_tls: bool,
        full_access_homeservers: Vec<String>,
        sanity_check_interval: Duration,
        cs_api_url_overrides: HashMap<String, CsApiUrl>,
        store: Option<Arc<dyn Store>>,
        deps: Arc<dyn Deps>,
    ) -> Arc<Self> {
        let (handler, receivers) = Self::new_without_loop(
            lk_auth,
            skip_verify_tls,
            full_access_homeservers,
            sanity_check_interval,
            cs_api_url_overrides,
            store,
            deps,
        );
        let looped = handler.clone();
        tokio::spawn(async move { looped.run_loop(receivers).await });
        handler
    }

    /// Constructs a Handler without starting its actor loop, handing the
    /// loop's receiving ends to the caller.
    pub(crate) fn new_without_loop(
        lk_auth: LiveKitAuth,
        skip_verify_tls: bool,
        full_access_homeservers: Vec<String>,
        sanity_check_interval: Duration,
        cs_api_url_overrides: HashMap<String, CsApiUrl>,
        store: Option<Arc<dyn Store>>,
        deps: Arc<dyn Deps>,
    ) -> (Arc<Self>, LoopReceivers) {
        let (add_job_tx, add_job_rx) = mpsc::channel(1);
        let (sfu_event_tx, sfu_event_rx) = mpsc::channel(200);
        let (job_done_tx, job_done_rx) = mpsc::channel(10);
        let (job_restarted_tx, job_restarted_rx) = mpsc::channel(10);
        let (loop_done_tx, _) = watch::channel(false);
        let (recovery_done_tx, _) = watch::channel(false);

        let handler = Arc::new(Self {
            cancel: CancellationToken::new(),
            livekit_auth: lk_auth,
            skip_verify_tls,
            full_access_homeservers,
            sanity_check_interval,
            cs_api_url_overrides: Arc::new(cs_api_url_overrides),
            cs_api_url_cache: Arc::new(CsApiUrlCache::new()),
            store,
            deps,
            loop_done_tx,
            recovery_done_tx,
            job_done_tx,
            job_restarted_tx,
            add_job_tx,
            sfu_event_tx,
        });
        (
            handler,
            LoopReceivers {
                add_job_rx,
                sfu_event_rx,
                job_done_rx,
                job_restarted_rx,
            },
        )
    }

    /// A watch receiver that flips to true once start-up recovery completed.
    #[cfg(test)]
    pub(crate) fn recovery_done(&self) -> watch::Receiver<bool> {
        self.recovery_done_tx.subscribe()
    }

    #[cfg(test)]
    pub(crate) fn loop_done_sender(&self) -> &watch::Sender<bool> {
        &self.loop_done_tx
    }

    /// Builds a CS-API URL resolver backed by this handler's overrides and
    /// cache.
    fn make_lookup(&self) -> LookupCsApiUrlFn {
        let deps = self.deps.clone();
        let overrides = self.cs_api_url_overrides.clone();
        let cache = self.cs_api_url_cache.clone();
        Arc::new(move |server_name| {
            let deps = deps.clone();
            let overrides = overrides.clone();
            let cache = cache.clone();
            Box::pin(async move {
                deps.resolve_cs_api_url(&server_name, &overrides, Some(&cache))
                    .await
            })
        })
    }

    fn new_job(
        self: &Arc<Self>,
        params: DelayedEventJobParams,
    ) -> Result<Arc<DelayedEventJob>, String> {
        DelayedEventJob::new(
            &self.cancel,
            params,
            self.deps.clone(),
            self.make_lookup(),
            self.job_done_tx.clone(),
            self.job_restarted_tx.clone(),
        )
    }

    /// The actor task owning the jobs map. Runs until the handler is
    /// cancelled, then joins all job tasks and flips loop_done.
    ///
    /// Store writes are handed to a dedicated writer task so a slow store
    /// cannot stall event routing; the single writer preserves the order the
    /// loop decided on.
    async fn run_loop(self: Arc<Self>, mut rx: LoopReceivers) {
        let mut jobs: HashMap<JobKey, Arc<DelayedEventJob>> = HashMap::new();

        let (store_tx, store_writer) = match &self.store {
            Some(store) => {
                let store = store.clone();
                let (tx, mut op_rx) = mpsc::channel::<StoreOp>(256);
                let writer = tokio::spawn(async move {
                    while let Some(op) = op_rx.recv().await {
                        let (result, key, context) = match op {
                            StoreOp::Save { key, job, context } => {
                                (store.save_job(&key, &job).await, key, context)
                            }
                            StoreOp::Delete { key, context } => {
                                (store.delete_job(&key).await, key, context)
                            }
                        };
                        if let Err(err) = result {
                            error!(key = ?key, err = %err, "Handler: failed to {context}");
                        }
                    }
                });
                (Some(tx), Some(writer))
            }
            None => (None, None),
        };
        // Enqueues block only when the writer is 256 operations behind,
        // providing backpressure instead of unbounded queueing.
        let enqueue_store_op = |op: StoreOp| async {
            if let Some(tx) = &store_tx {
                if tx.send(op).await.is_err() {
                    error!("Handler: store writer is gone, dropping store operation");
                }
            }
        };

        // Load any existing jobs from the store.
        if let Some(store) = &self.store {
            match store.all_jobs().await {
                Err(err) => {
                    error!(err = %err, "Handler: failed to load stored jobs");
                }
                Ok(stored_jobs) => {
                    for stored_job in stored_jobs {
                        let key = JobKey {
                            room: stored_job.params.livekit_room.clone(),
                            identity: stored_job.params.livekit_identity.clone(),
                        };

                        // Check if the job's delay timeout has already
                        // exceeded. If so, delete it from the store.
                        let expires_at = stored_job.restarted_at
                            + chrono::Duration::from_std(stored_job.params.delay_timeout)
                                .unwrap_or_default();
                        if expires_at <= Utc::now() {
                            info!(lk_id = %key.identity, "Handler: skipping expired stored job");
                            if let Err(err) = store.delete_job(&key).await {
                                error!(key = ?key, err = %err,
                                    "Handler: failed to delete expired stored job");
                            }
                            continue;
                        }

                        // Create a new job. Job creation shouldn't emit
                        // temporary errors. So if we've failed here, just
                        // delete the job from the store.
                        let job = match self.new_job(stored_job.params.clone()) {
                            Ok(job) => job,
                            Err(err) => {
                                error!(room = %stored_job.params.livekit_room,
                                    lk_id = %stored_job.params.livekit_identity, err = %err,
                                    "Handler: failed to create delayed event job from stored job");
                                if let Err(err) = store.delete_job(&key).await {
                                    error!(key = ?key, err = %err,
                                        "Handler: failed to delete expired stored job");
                                }
                                continue;
                            }
                        };

                        // Store the job in the handler's local map and kick
                        // off its loop.
                        jobs.insert(key.clone(), job.clone());
                        start_participant_lookup(
                            &job,
                            self.deps.clone(),
                            self.livekit_auth.clone(),
                            self.sanity_check_interval,
                        );
                        job.spawn_loop();
                        debug!(room = %key.room, lk_id = %key.identity, job_id = %job.job_id,
                            "Handler: job created from stored job");
                    }
                }
            }
        }

        self.recovery_done_tx.send_replace(true);

        loop {
            tokio::select! {
                _ = self.cancel.cancelled() => {
                    debug!("Handler: loop exiting");
                    // Cancel all job contexts first — unblocks tasks waiting
                    // on the done channel so they take the cancellation path
                    // and exit promptly.
                    for job in jobs.values() {
                        job.stop();
                    }
                    // Drain any buffered terminal signals so no task stays
                    // blocked trying to send on the now-dead loop.
                    while rx.job_done_rx.try_recv().is_ok() {}
                    // Wait for all job loops to exit before returning.
                    for result in
                        futures::future::join_all(jobs.values().map(|job| job.close())).await
                    {
                        if let Err(err) = result {
                            warn!(err = %err, "Handler: job close failed during shutdown");
                        }
                    }
                    break;
                }

                // ── add job ──────────────────────────────────────────────────
                Some(req) = rx.add_job_rx.recv() => {
                    let key = JobKey {
                        room: req.params.livekit_room.clone(),
                        identity: req.params.livekit_identity.clone(),
                    };

                    // Create a new job.
                    let job = match self.new_job(req.params.clone()) {
                        Ok(job) => job,
                        Err(err) => {
                            error!(room = %req.params.livekit_room,
                                lk_id = %req.params.livekit_identity, err = %err,
                                "Handler: failed to create delayed event job");
                            let _ = req.result.send(Err(err));
                            continue;
                        }
                    };

                    if let Some(existing) = jobs.get(&key) {
                        info!(room = %key.room, lk_id = %key.identity,
                            old_job_id = %existing.job_id, new_job_id = %job.job_id,
                            "Handler: replacing existing job");
                        // Non-blocking send: if the channel is full the signal
                        // is simply dropped.
                        let _ = existing.event_tx.try_send(DelayedEventSignal::JobReplaced);
                        existing.stop();
                        // No synchronous close needed — the existing job's
                        // loop exits when its cancellation fires; the shutdown
                        // path handles the rest.
                    }

                    jobs.insert(key.clone(), job.clone());

                    // Save the job in the store. We assume the delayed event
                    // was restarted just before the request came in because
                    // that is our best guess.
                    if self.store.is_some() {
                        let stored_job =
                            StoredJob { params: req.params.clone(), restarted_at: Utc::now() };
                        enqueue_store_op(StoreOp::Save {
                            key: key.clone(),
                            job: stored_job,
                            context: "store job",
                        })
                        .await;
                    }

                    // Pull-based lookup, in addition to SFU webhooks. Phase 1
                    // confirms the initial connect (delegated leaves get no
                    // webhook for it); Phase 2, when enabled, periodically
                    // re-checks presence to catch missed disconnect webhooks.
                    start_participant_lookup(
                        &job,
                        self.deps.clone(),
                        self.livekit_auth.clone(),
                        self.sanity_check_interval,
                    );

                    job.spawn_loop();
                    debug!(room = %key.room, lk_id = %key.identity, job_id = %job.job_id,
                        "Handler: job created");
                    let _ = req.result.send(Ok(job.job_id.clone()));
                }

                // ── SFU webhook routing ──────────────────────────────────────
                Some(ev) = rx.sfu_event_rx.recv() => {
                    let key = JobKey {
                        room: ev.room_alias.clone(),
                        identity: ev.msg.livekit_identity.clone(),
                    };
                    if let Some(job) = jobs.get(&key) {
                        match ev.msg.signal {
                            DelayedEventSignal::ParticipantConnected
                            | DelayedEventSignal::ParticipantDisconnectedIntentionally
                            | DelayedEventSignal::ParticipantConnectionAborted
                            | DelayedEventSignal::SfuParticipantGone => {
                                tokio::select! {
                                    _ = job.cancel.cancelled() => {}
                                    _ = job.event_tx.send(ev.msg.signal) => {}
                                }
                            }
                            _ => {
                                warn!(signal = %ev.msg.signal, room = %ev.room_alias,
                                    lk_id = %ev.msg.livekit_identity,
                                    "Handler: unexpected SFU event type");
                            }
                        }
                    }
                }

                // ── job lifecycle ────────────────────────────────────────────
                Some(done_job) = rx.job_done_rx.recv() => {
                    let key = JobKey {
                        room: done_job.params.livekit_room.clone(),
                        identity: done_job.params.livekit_identity.clone(),
                    };
                    let is_current =
                        jobs.get(&key).is_some_and(|current| Arc::ptr_eq(current, &done_job));
                    if !is_current {
                        debug!(room = %key.room, lk_id = %key.identity, job_id = %done_job.job_id,
                            "Handler: ignoring stale job-done signal");
                        continue;
                    }
                    info!(room = %key.room, lk_id = %key.identity, job_id = %done_job.job_id,
                        "Handler: job done, cleaning up");
                    jobs.remove(&key);

                    if self.store.is_some() {
                        enqueue_store_op(StoreOp::Delete {
                            key: key.clone(),
                            context: "delete persisted job",
                        })
                        .await;
                    }

                    done_job.stop();
                    // No synchronous close needed — the job's loop exits on
                    // its own; the shutdown path handles cleanup.
                }

                Some(req) = rx.job_restarted_rx.recv() => {
                    let key = JobKey {
                        room: req.job.params.livekit_room.clone(),
                        identity: req.job.params.livekit_identity.clone(),
                    };
                    let is_current =
                        jobs.get(&key).is_some_and(|current| Arc::ptr_eq(current, &req.job));
                    if !is_current {
                        debug!(room = %key.room, lk_id = %key.identity, job_id = %req.job.job_id,
                            "Handler: ignoring stale job-restarted signal");
                        continue;
                    }

                    if self.store.is_some() {
                        let stored_job = StoredJob {
                            params: req.job.params.clone(),
                            restarted_at: req.restarted_at.into(),
                        };
                        enqueue_store_op(StoreOp::Save {
                            key: key.clone(),
                            job: stored_job,
                            context: "update stored job",
                        })
                        .await;
                    }
                }
            }
        }

        // Let the writer drain its queue before signalling completion.
        drop(store_tx);
        if let Some(writer) = store_writer {
            let _ = writer.await;
        }

        self.loop_done_tx.send_replace(true);
    }

    /// Shuts down the handler and waits for the loop to exit.
    pub async fn close(&self) {
        self.cancel.cancel();
        let mut done = self.loop_done_tx.subscribe();
        let wait = async {
            while !*done.borrow_and_update() {
                if done.changed().await.is_err() {
                    break;
                }
            }
        };
        if tokio::time::timeout(Duration::from_secs(10), wait)
            .await
            .is_err()
        {
            warn!("Handler: close() timed out");
        }
    }

    /// Adds a delayed-event job and waits for the outcome.
    pub(crate) async fn add_delayed_event_job(
        &self,
        params: DelayedEventJobParams,
    ) -> Result<(), AddJobError> {
        debug!(room = %params.livekit_room, lk_id = %params.livekit_identity,
            delay_id = %params.delay_id, "Handler: adding delayed event job");

        let (result_tx, result_rx) = oneshot::channel();
        let room = params.livekit_room.clone();
        let lk_id = params.livekit_identity.clone();
        tokio::select! {
            _ = self.cancel.cancelled() => {
                warn!(room = %room, "Handler: add_delayed_event_job called after shutdown");
                return Err(AddJobError::Shutdown);
            }
            sent = self.add_job_tx.send(AddJobRequest { params, result: result_tx }) => {
                if sent.is_err() {
                    return Err(AddJobError::Shutdown);
                }
            }
        }
        match result_rx.await {
            Ok(Ok(_job_id)) => Ok(()),
            Ok(Err(err)) => {
                error!(room = %room, lk_id = %lk_id, err = %err, "Handler: job handover failed");
                Err(AddJobError::Creation(err))
            }
            Err(_) => Err(AddJobError::Shutdown),
        }
    }

    pub(crate) fn is_full_access_user(&self, matrix_server_name: &str) -> bool {
        // Grant full access if wildcard '*' is present as the only entry
        if self.full_access_homeservers.len() == 1 && self.full_access_homeservers[0] == "*" {
            return true;
        }

        // Check if the matrix_server_name is in the list of full-access
        // homeservers
        self.full_access_homeservers
            .iter()
            .any(|s| s == matrix_server_name)
    }

    async fn verify_openid_token(
        &self,
        token: &OpenIdTokenType,
        claimed_user_id: &str,
    ) -> Result<String, MatrixErrorResponse> {
        let unauthorized = || MatrixErrorResponse {
            status: 401,
            errcode: "M_UNAUTHORIZED".into(),
            err: "The request could not be authorised.".into(),
        };

        let user_info = self
            .deps
            .exchange_openid_userinfo(token, self.skip_verify_tls)
            .await
            .map_err(|_| unauthorized())?;

        if !claimed_user_id.is_empty() && claimed_user_id != user_info.sub {
            warn!(claimed_user_id, matrix_id = %user_info.sub,
                "Handler: ClaimedUserID does not match token subject");
            return Err(unauthorized());
        }

        Ok(user_info.sub)
    }

    /// Deprecated: serves the pre-Matrix-2.0 /sfu/get endpoint. Remove once
    /// all in-the-wild clients have migrated to /get_token.
    pub(crate) async fn process_legacy_sfu_request(
        &self,
        req: &LegacySfuRequest,
    ) -> Result<SfuResponse, MatrixErrorResponse> {
        let matrix_id = self.verify_openid_token(&req.openid_token, "").await?;

        let is_full_access_user = self.is_full_access_user(&req.openid_token.matrix_server_name);
        let delayed_event_delegation_requested = !req.delay_id.is_empty();

        if delayed_event_delegation_requested && !is_full_access_user {
            return Err(MatrixErrorResponse {
                status: 400,
                errcode: "M_BAD_JSON".into(),
                err: "Delegation of delayed events is only supported for full access users".into(),
            });
        }

        debug!(matrix_id = %matrix_id,
            access = if is_full_access_user { "full" } else { "restricted" },
            "Handler: got Matrix user info");

        let lk_identity = LiveKitIdentity(format!("{matrix_id}:{}", req.device_id));
        let slot_id = "m.call#ROOM";
        let lk_room_alias = livekit_room_alias_for(&req.room, slot_id);

        let token = get_join_token(
            &self.livekit_auth.key,
            &self.livekit_auth.secret,
            &lk_room_alias,
            &lk_identity,
        )
        .map_err(|_| MatrixErrorResponse {
            status: 500,
            errcode: "M_UNKNOWN".into(),
            err: "Internal Server Error".into(),
        })?;

        if is_full_access_user {
            // If delegation is requested, verify that we can resolve the
            // Client-Server API and fail the request otherwise. We do this
            // before creating the LiveKit room so that we don't produce
            // lingering rooms in the error case.
            //
            // TODO: This code is currently duplicated across the token
            // endpoints and the delegation endpoint. This will be resolved
            // once the already deprecated delay parameters are removed from
            // the token endpoints.
            if delayed_event_delegation_requested
                && self
                    .deps
                    .resolve_cs_api_url(
                        &req.openid_token.matrix_server_name,
                        &self.cs_api_url_overrides,
                        Some(&self.cs_api_url_cache),
                    )
                    .await
                    .is_err()
            {
                return Err(MatrixErrorResponse {
                    status: 400,
                    errcode: "M_BAD_JSON".into(),
                    err: "Unable to resolve client-server API".into(),
                });
            }

            // Now create the LiveKit room.
            if self
                .deps
                .create_livekit_room(&self.livekit_auth, &lk_room_alias, &matrix_id, &lk_identity)
                .await
                .is_err()
            {
                return Err(MatrixErrorResponse {
                    status: 500,
                    errcode: "M_UNKNOWN".into(),
                    err: "Unable to create room on SFU".into(),
                });
            }

            if delayed_event_delegation_requested {
                info!(room = %lk_room_alias, lk_id = %lk_identity, delay_id = %req.delay_id,
                    matrix_server_name = %req.openid_token.matrix_server_name,
                    "Handler: scheduling delayed event job");
                self.add_delayed_event_job(DelayedEventJobParams {
                    server_name: req.openid_token.matrix_server_name.clone(),
                    delay_id: req.delay_id.clone(),
                    delay_timeout: Duration::from_millis(req.delay_timeout.max(0) as u64),
                    livekit_room: lk_room_alias.clone(),
                    livekit_identity: lk_identity.clone(),
                })
                .await
                .map_err(matrix_error_for_add_job)?;
            }
        }

        info!(matrix_id = %matrix_id, claimed_device_id = %req.device_id,
            access = if is_full_access_user { "full" } else { "restricted" },
            matrix_room = %req.room, lk_id = %lk_identity, room = %lk_room_alias,
            "Handler: generated Legacy SFU access token");

        Ok(SfuResponse {
            url: self.livekit_auth.lk_url.clone(),
            jwt: token,
        })
    }

    pub(crate) async fn process_sfu_request(
        &self,
        req: &SfuRequest,
    ) -> Result<SfuResponse, MatrixErrorResponse> {
        let matrix_id = self
            .verify_openid_token(&req.openid_token, &req.member.claimed_user_id)
            .await?;

        let is_full_access_user = self.is_full_access_user(&req.openid_token.matrix_server_name);
        let delayed_event_delegation_requested = !req.delay_id.is_empty();

        if delayed_event_delegation_requested && !is_full_access_user {
            return Err(MatrixErrorResponse {
                status: 400,
                errcode: "M_BAD_JSON".into(),
                err: "Delegation of delayed events is only supported for full access users".into(),
            });
        }

        debug!(matrix_id = %matrix_id,
            access = if is_full_access_user { "full" } else { "restricted" },
            "Handler: got Matrix user info");

        let lk_identity =
            livekit_identity_for(&matrix_id, &req.member.claimed_device_id, &req.member.id);
        let lk_room_alias = livekit_room_alias_for(&req.room_id, &req.slot_id);

        let token = get_join_token(
            &self.livekit_auth.key,
            &self.livekit_auth.secret,
            &lk_room_alias,
            &lk_identity,
        )
        .map_err(|err| {
            error!(matrix_id = %matrix_id, err = %err, "Handler: error getting LiveKit token");
            MatrixErrorResponse {
                status: 500,
                errcode: "M_UNKNOWN".into(),
                err: "Internal Server Error".into(),
            }
        })?;

        if is_full_access_user {
            // If delegation is requested, verify that we can resolve the
            // Client-Server API and fail the request otherwise. We do this
            // before creating the LiveKit room so that we don't produce
            // lingering rooms in the error case.
            //
            // TODO: This code is currently duplicated across the token
            // endpoints and the delegation endpoint. This will be resolved
            // once the already deprecated delay parameters are removed from
            // the token endpoints.
            if delayed_event_delegation_requested
                && self
                    .deps
                    .resolve_cs_api_url(
                        &req.openid_token.matrix_server_name,
                        &self.cs_api_url_overrides,
                        Some(&self.cs_api_url_cache),
                    )
                    .await
                    .is_err()
            {
                return Err(MatrixErrorResponse {
                    status: 400,
                    errcode: "M_BAD_JSON".into(),
                    err: "Unable to resolve client-server API".into(),
                });
            }

            // Now create the LiveKit room.
            if self
                .deps
                .create_livekit_room(&self.livekit_auth, &lk_room_alias, &matrix_id, &lk_identity)
                .await
                .is_err()
            {
                return Err(MatrixErrorResponse {
                    status: 500,
                    errcode: "M_UNKNOWN".into(),
                    err: "Unable to create room on SFU".into(),
                });
            }

            if delayed_event_delegation_requested {
                info!(room = %lk_room_alias, lk_id = %lk_identity, delay_id = %req.delay_id,
                    matrix_server_name = %req.openid_token.matrix_server_name,
                    "Handler: scheduling delayed event job");
                self.add_delayed_event_job(DelayedEventJobParams {
                    server_name: req.openid_token.matrix_server_name.clone(),
                    delay_id: req.delay_id.clone(),
                    delay_timeout: Duration::from_millis(req.delay_timeout.max(0) as u64),
                    livekit_room: lk_room_alias.clone(),
                    livekit_identity: lk_identity.clone(),
                })
                .await
                .map_err(matrix_error_for_add_job)?;
            }
        }

        info!(matrix_id = %matrix_id, claimed_device_id = %req.member.claimed_device_id,
            access = if is_full_access_user { "full" } else { "restricted" },
            matrix_room = %req.room_id, matrix_rtc_slot = %req.slot_id,
            lk_id = %lk_identity, room = %lk_room_alias,
            "Handler: generated SFU access token");

        Ok(SfuResponse {
            url: self.livekit_auth.lk_url.clone(),
            jwt: token,
        })
    }

    /// Handles /delegate_delayed_leave: schedules a delayed-leave job for a
    /// participant that is already connected to the SFU. Issues no JWT and
    /// creates no room; the delayed-event parameters are mandatory. Presence
    /// is confirmed by the job's participant lookup, since the connect
    /// webhook has already fired.
    pub(crate) async fn process_delegate_delayed_leave(
        &self,
        req: &DelegateDelayedLeaveRequest,
    ) -> Result<DelegateDelayedLeaveResponse, MatrixErrorResponse> {
        let matrix_id = self
            .verify_openid_token(&req.openid_token, &req.member.claimed_user_id)
            .await?;

        // Delayed event delegation is restricted to full-access homeservers.
        if !self.is_full_access_user(&req.openid_token.matrix_server_name) {
            return Err(MatrixErrorResponse {
                status: 403,
                errcode: "M_FORBIDDEN".into(),
                err: "Delegation of delayed events is only supported for full access users".into(),
            });
        }

        let lk_identity =
            livekit_identity_for(&matrix_id, &req.member.claimed_device_id, &req.member.id);
        let lk_room_alias = livekit_room_alias_for(&req.room_id, &req.slot_id);

        // Verify that the Client-Server API can be resolved and prime the
        // cache.
        if self
            .deps
            .resolve_cs_api_url(
                &req.openid_token.matrix_server_name,
                &self.cs_api_url_overrides,
                Some(&self.cs_api_url_cache),
            )
            .await
            .is_err()
        {
            return Err(MatrixErrorResponse {
                status: 400,
                errcode: "M_BAD_JSON".into(),
                err: "Unable to resolve client-server API".into(),
            });
        }

        info!(room = %lk_room_alias, lk_id = %lk_identity, delay_id = %req.delay_id,
            matrix_server_name = %req.openid_token.matrix_server_name,
            "Handler: scheduling delayed event job (delegate_delayed_leave)");

        self.add_delayed_event_job(DelayedEventJobParams {
            server_name: req.openid_token.matrix_server_name.clone(),
            delay_id: req.delay_id.clone(),
            delay_timeout: Duration::from_millis(req.delay_timeout.max(0) as u64),
            livekit_room: lk_room_alias.clone(),
            livekit_identity: lk_identity.clone(),
        })
        .await
        .map_err(matrix_error_for_add_job)?;

        Ok(DelegateDelayedLeaveResponse {})
    }

    pub fn prepare_router(self: &Arc<Self>) -> Router {
        Router::new()
            // Deprecated: pre-Matrix-2.0; remove once clients migrate to
            // /get_token.
            .route("/sfu/get", any(handle_legacy))
            .route("/get_token", any(handle_get_token))
            .route(
                "/delegate_delayed_leave",
                any(handle_delegate_delayed_leave),
            )
            .route("/sfu_webhook", any(handle_sfu_webhook))
            .route("/healthz", any(healthcheck))
            .with_state(self.clone())
    }
}

/// Maps an add_delayed_event_job failure to the client response:
///   - shutdown → 503 M_UNKNOWN,
///   - invalid job params → 400 M_BAD_JSON.
fn matrix_error_for_add_job(err: AddJobError) -> MatrixErrorResponse {
    match err {
        AddJobError::Shutdown => MatrixErrorResponse {
            status: 503,
            errcode: "M_UNKNOWN".into(),
            err: "Service is shutting down".into(),
        },
        AddJobError::Creation(err) => MatrixErrorResponse {
            status: 400,
            errcode: "M_BAD_JSON".into(),
            err,
        },
    }
}

// ── HTTP plumbing ────────────────────────────────────────────────────────────

/// Writes a Matrix-style error response.
fn matrix_error_response(status: u16, errcode: &str, err_msg: &str) -> Response {
    (
        StatusCode::from_u16(status).unwrap_or(StatusCode::INTERNAL_SERVER_ERROR),
        Json(MatrixErrorBody {
            errcode: errcode.into(),
            error: err_msg.into(),
        }),
    )
        .into_response()
}

fn matrix_error_into_response(err: &MatrixErrorResponse) -> Response {
    matrix_error_response(err.status, &err.errcode, &err.err)
}

/// Adds the standard CORS + JSON headers shared by the POST endpoints.
fn apply_cors_json(mut resp: Response) -> Response {
    let headers = resp.headers_mut();
    headers.insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("application/json"),
    );
    headers.insert(
        header::ACCESS_CONTROL_ALLOW_ORIGIN,
        HeaderValue::from_static("*"),
    );
    headers.insert(
        header::ACCESS_CONTROL_ALLOW_METHODS,
        HeaderValue::from_static("POST"),
    );
    headers.insert(
        header::ACCESS_CONTROL_ALLOW_HEADERS,
        HeaderValue::from_static(
            "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token",
        ),
    );
    resp
}

/// Reads and decodes a JSON request body.
async fn decode_json_body<T: serde::de::DeserializeOwned>(req: Request) -> Result<T, String> {
    let bytes = axum::body::to_bytes(req.into_body(), 1024 * 1024)
        .await
        .map_err(|e| e.to_string())?;
    serde_json::from_slice(&bytes).map_err(|e| e.to_string())
}

/// The CORS preflight / method-check shell shared by the POST endpoints.
enum PostGate {
    Handle(Request),
    Reply(Response),
}

fn gate_post(req: Request) -> PostGate {
    match *req.method() {
        Method::OPTIONS => PostGate::Reply(StatusCode::OK.into_response()),
        Method::POST => PostGate::Handle(req),
        _ => PostGate::Reply(StatusCode::METHOD_NOT_ALLOWED.into_response()),
    }
}

/// Deprecated: serves the pre-Matrix-2.0 /sfu/get endpoint. Remove once all
/// in-the-wild clients have migrated to /get_token.
async fn handle_legacy(State(handler): State<Arc<Handler>>, req: Request) -> Response {
    let req = match gate_post(req) {
        PostGate::Handle(req) => req,
        PostGate::Reply(resp) => return apply_cors_json(resp),
    };
    debug!("Handler (legacy): new request");

    let sfu_request: LegacySfuRequest = match decode_json_body(req).await {
        Ok(r) => r,
        Err(err) => {
            error!(err = %err, "Handler (legacy): error reading body");
            return apply_cors_json(matrix_error_response(
                400,
                "M_NOT_JSON",
                "Error reading request",
            ));
        }
    };

    if let Err(err) = sfu_request.validate() {
        return apply_cors_json(matrix_error_into_response(&err));
    }

    let resp = match handler.process_legacy_sfu_request(&sfu_request).await {
        Ok(sfu_response) => Json(sfu_response).into_response(),
        Err(err) => matrix_error_into_response(&err),
    };
    apply_cors_json(resp)
}

async fn handle_get_token(State(handler): State<Arc<Handler>>, req: Request) -> Response {
    let req = match gate_post(req) {
        PostGate::Handle(req) => req,
        PostGate::Reply(resp) => return apply_cors_json(resp),
    };
    debug!("Handler: new request");

    let sfu_request: SfuRequest = match decode_json_body(req).await {
        Ok(r) => r,
        Err(err) => {
            error!(err = %err, "Handler: error reading body");
            return apply_cors_json(matrix_error_response(
                400,
                "M_NOT_JSON",
                "Error reading request",
            ));
        }
    };

    if let Err(err) = sfu_request.validate() {
        return apply_cors_json(matrix_error_into_response(&err));
    }

    let resp = match handler.process_sfu_request(&sfu_request).await {
        Ok(sfu_response) => Json(sfu_response).into_response(),
        Err(err) => matrix_error_into_response(&err),
    };
    apply_cors_json(resp)
}

async fn handle_delegate_delayed_leave(
    State(handler): State<Arc<Handler>>,
    req: Request,
) -> Response {
    let req = match gate_post(req) {
        PostGate::Handle(req) => req,
        PostGate::Reply(resp) => return apply_cors_json(resp),
    };
    debug!("Handler: delegate_delayed_leave request");

    let request: DelegateDelayedLeaveRequest = match decode_json_body(req).await {
        Ok(r) => r,
        Err(err) => {
            error!(err = %err, "Handler: delegate_delayed_leave: error reading body");
            return apply_cors_json(matrix_error_response(
                400,
                "M_NOT_JSON",
                "Error reading request",
            ));
        }
    };

    if let Err(err) = request.validate() {
        return apply_cors_json(matrix_error_into_response(&err));
    }

    let resp = match handler.process_delegate_delayed_leave(&request).await {
        Ok(response) => Json(response).into_response(),
        Err(err) => matrix_error_into_response(&err),
    };
    apply_cors_json(resp)
}

async fn healthcheck(req: Request) -> Response {
    info!("Handler: health check");

    if req.method() == Method::GET || req.method() == Method::HEAD {
        StatusCode::OK.into_response()
    } else {
        StatusCode::METHOD_NOT_ALLOWED.into_response()
    }
}

/// Translates a validated LiveKit webhook event into a
/// (room alias, SfuMessage) pair. Returns None for event types that do not
/// require routing (e.g. room events, unknown types).
pub(crate) fn sfu_event_from_webhook(
    event: &livekit_protocol::WebhookEvent,
) -> Option<(LiveKitRoomAlias, SfuMessage)> {
    // https://docs.livekit.io/intro/basics/rooms-participants-tracks/webhooks-events/#webhook-events
    let room_alias = LiveKitRoomAlias(event.room.as_ref()?.name.clone());
    match event.event.as_str() {
        "participant_joined" => {
            let participant = event.participant.as_ref()?;
            debug!(lk_id = %participant.identity, room = %room_alias,
                "Handler: SFU participant joined");
            Some((
                room_alias,
                SfuMessage {
                    signal: DelayedEventSignal::ParticipantConnected,
                    livekit_identity: LiveKitIdentity(participant.identity.clone()),
                },
            ))
        }

        "participant_left" | "participant_connection_aborted" => {
            let participant = event.participant.as_ref()?;
            let signal = if participant.disconnect_reason()
                == livekit_protocol::DisconnectReason::ClientInitiated
            {
                DelayedEventSignal::ParticipantDisconnectedIntentionally
            } else {
                DelayedEventSignal::ParticipantConnectionAborted
            };
            debug!(lk_id = %participant.identity, room = %room_alias,
                disconnect_reason = ?participant.disconnect_reason(),
                "Handler: SFU participant left");
            Some((
                room_alias,
                SfuMessage {
                    signal,
                    livekit_identity: LiveKitIdentity(participant.identity.clone()),
                },
            ))
        }

        _ => None,
    }
}

async fn handle_sfu_webhook(State(handler): State<Arc<Handler>>, req: Request) -> Response {
    let auth_token = req
        .headers()
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .unwrap_or_default()
        .to_owned();
    let bytes = match axum::body::to_bytes(req.into_body(), 8 * 1024 * 1024).await {
        Ok(bytes) => bytes,
        Err(err) => {
            warn!(err = %err, "Handler: SFU webhook error");
            return StatusCode::OK.into_response();
        }
    };
    let body = String::from_utf8_lossy(&bytes);

    let receiver = WebhookReceiver::new(TokenVerifier::with_api_key(
        &handler.livekit_auth.key,
        &handler.livekit_auth.secret,
    ));
    let event = match receiver.receive(&body, &auth_token) {
        Ok(event) => event,
        Err(err) => {
            warn!(err = %err, "Handler: SFU webhook error");
            return StatusCode::OK.into_response();
        }
    };

    let Some((room_alias, msg)) = sfu_event_from_webhook(&event) else {
        return StatusCode::OK.into_response();
    };

    // Route via the loop so the map is accessed by a single task only.
    tokio::select! {
        _ = handler.cancel.cancelled() => {}
        _ = handler.sfu_event_tx.send(SfuEventRequest { room_alias, msg }) => {}
    }
    StatusCode::OK.into_response()
}

#[cfg(test)]
#[path = "handler_tests.rs"]
mod tests;

// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//! The job persistence layer.
//!
//! Jobs are serialized to JSON in the same shape as the original Go
//! implementation (`Params` + `RestartedAt`, with the delay timeout as
//! integer nanoseconds), so an existing Redis store keeps working across
//! the migration.

use std::sync::Arc;

use async_trait::async_trait;
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use tracing::{info, warn};

use crate::delayed_event_manager::{DelayedEventJobParams, JobKey};
use crate::helper::{LiveKitIdentity, LiveKitRoomAlias, ParticipantKey};

/// A stored job.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct StoredJob {
    #[serde(rename = "Params")]
    pub params: DelayedEventJobParams,
    #[serde(rename = "RestartedAt")]
    pub restarted_at: DateTime<Utc>,
}

/// An SFU participant the service issued a token for, together with the
/// Matrix room membership that entitles them to it. Persisted so that
/// membership enforcement picks up where it left off after a restart.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct StoredParticipant {
    /// The Matrix room the token was issued for.
    pub matrix_room_id: String,
    /// The Matrix user the token was issued to.
    pub matrix_user_id: String,
    /// The LiveKit room the token grants access to.
    pub livekit_room: LiveKitRoomAlias,
    /// The LiveKit identity the token was issued for.
    pub livekit_identity: LiveKitIdentity,
    /// When the token was issued.
    pub registered_at: DateTime<Utc>,
}

impl StoredParticipant {
    /// The key this participant is stored under.
    pub fn key(&self) -> ParticipantKey {
        ParticipantKey {
            room: self.livekit_room.clone(),
            identity: self.livekit_identity.clone(),
        }
    }
}

/// Interface for storage backends.
///
/// The caller is responsible for ensuring thread-safety when calling these
/// methods.
#[async_trait]
pub trait Store: Send + Sync {
    /// Adds a job to the store, overwriting any existing entry for the same
    /// key.
    async fn save_job(&self, key: &JobKey, job: &StoredJob) -> Result<(), String>;

    /// Removes the entry for the given key from the store. A missing entry is
    /// not an error.
    async fn delete_job(&self, key: &JobKey) -> Result<(), String>;

    /// Retrieves all jobs in the store.
    async fn all_jobs(&self) -> Result<Vec<StoredJob>, String>;

    /// Adds a participant to the store, overwriting any existing entry for
    /// the same key.
    async fn save_participant(
        &self,
        key: &ParticipantKey,
        participant: &StoredParticipant,
    ) -> Result<(), String>;

    /// Removes the participant stored under the given key. A missing entry
    /// is not an error.
    async fn delete_participant(&self, key: &ParticipantKey) -> Result<(), String>;

    /// Retrieves all participants in the store.
    async fn all_participants(&self) -> Result<Vec<StoredParticipant>, String>;
}

/// A store backend using an external Redis instance.
pub struct RedisStore {
    // ConnectionManager reconnects automatically after connection loss.
    conn: redis::aio::ConnectionManager,
}

pub(crate) const REDIS_JOBS_HASH_KEY: &str = "lk-jwt:jobs";
pub(crate) const REDIS_PARTICIPANTS_HASH_KEY: &str = "lk-jwt:participants";

pub async fn new_redis_store(redis_url: &str) -> Result<Arc<dyn Store>, String> {
    let client = redis::Client::open(redis_url)
        .map_err(|e| format!("store: invalid Redis URL {redis_url:?}: {e}"))?;
    let mut conn = client
        .get_connection_manager()
        .await
        .map_err(|e| format!("store: Redis connection failed: {e}"))?;
    redis::cmd("PING")
        .query_async::<()>(&mut conn)
        .await
        .map_err(|e| format!("store: Redis ping failed: {e}"))?;

    info!(addr = redis_url, "store: connected to Redis");
    Ok(Arc::new(RedisStore { conn }))
}

impl RedisStore {
    #[cfg(test)]
    pub(crate) fn with_connection(conn: redis::aio::ConnectionManager) -> Self {
        Self { conn }
    }

    fn key_to_string(key: &ParticipantKey) -> String {
        serde_json::to_string(&[key.room.as_str(), key.identity.as_str()])
            .expect("string slices always serialize")
    }

    /// Stores `value` under `key` in the given hash.
    async fn hset<T: Serialize>(
        &self,
        hash_key: &str,
        key: &ParticipantKey,
        value: &T,
        what: &str,
    ) -> Result<(), String> {
        let data = serde_json::to_string(value)
            .map_err(|e| format!("store: failed marshalling {what}: {e}"))?;
        let mut conn = self.conn.clone();
        redis::cmd("HSET")
            .arg(hash_key)
            .arg(Self::key_to_string(key))
            .arg(data)
            .query_async::<()>(&mut conn)
            .await
            .map_err(|e| e.to_string())
    }

    /// Removes `key` from the given hash.
    async fn hdel(&self, hash_key: &str, key: &ParticipantKey) -> Result<(), String> {
        let mut conn = self.conn.clone();
        redis::cmd("HDEL")
            .arg(hash_key)
            .arg(Self::key_to_string(key))
            .query_async::<()>(&mut conn)
            .await
            .map_err(|e| e.to_string())
    }

    /// Loads every parseable entry of the given hash, skipping (and logging)
    /// the rest.
    async fn hgetall<T: for<'de> Deserialize<'de>>(
        &self,
        hash_key: &str,
    ) -> Result<Vec<T>, String> {
        let mut conn = self.conn.clone();
        let fields: std::collections::HashMap<String, String> = redis::cmd("HGETALL")
            .arg(hash_key)
            .query_async(&mut conn)
            .await
            .map_err(|e| format!("store: failed getting all entries: {e}"))?;

        let mut entries = Vec::with_capacity(fields.len());
        for (identity, data) in fields {
            match serde_json::from_str::<T>(&data) {
                Ok(entry) => entries.push(entry),
                Err(err) => {
                    warn!(hash_key, identity, err = %err, "store: skipping unparseable entry");
                }
            }
        }
        Ok(entries)
    }
}

#[async_trait]
impl Store for RedisStore {
    async fn save_job(&self, key: &JobKey, job: &StoredJob) -> Result<(), String> {
        self.hset(REDIS_JOBS_HASH_KEY, key, job, "job").await
    }

    async fn delete_job(&self, key: &JobKey) -> Result<(), String> {
        self.hdel(REDIS_JOBS_HASH_KEY, key).await
    }

    async fn all_jobs(&self) -> Result<Vec<StoredJob>, String> {
        self.hgetall(REDIS_JOBS_HASH_KEY).await
    }

    async fn save_participant(
        &self,
        key: &ParticipantKey,
        participant: &StoredParticipant,
    ) -> Result<(), String> {
        self.hset(REDIS_PARTICIPANTS_HASH_KEY, key, participant, "participant")
            .await
    }

    async fn delete_participant(&self, key: &ParticipantKey) -> Result<(), String> {
        self.hdel(REDIS_PARTICIPANTS_HASH_KEY, key).await
    }

    async fn all_participants(&self) -> Result<Vec<StoredParticipant>, String> {
        self.hgetall(REDIS_PARTICIPANTS_HASH_KEY).await
    }
}

// ─────────────────────────────────────────────────────────────────────────────

#[cfg(test)]
pub(crate) mod test_support {
    use std::collections::HashMap;
    use std::sync::Mutex;

    use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
    use tokio::sync::mpsc;

    use super::*;

    // ── in-memory stores (used by handler tests) ──────────────────────────────

    /// An in-memory storage backend without persistence.
    pub(crate) struct InMemoryStore {
        jobs: Mutex<HashMap<JobKey, StoredJob>>,
        participants: Mutex<HashMap<ParticipantKey, StoredParticipant>>,
    }

    pub(crate) fn new_in_memory_store() -> Arc<dyn Store> {
        info!("store: created new in-memory store");
        Arc::new(InMemoryStore {
            jobs: Mutex::new(HashMap::new()),
            participants: Mutex::new(HashMap::new()),
        })
    }

    #[async_trait]
    impl Store for InMemoryStore {
        async fn save_job(&self, key: &JobKey, job: &StoredJob) -> Result<(), String> {
            self.jobs.lock().unwrap().insert(key.clone(), job.clone());
            Ok(())
        }

        async fn delete_job(&self, key: &JobKey) -> Result<(), String> {
            self.jobs.lock().unwrap().remove(key);
            Ok(())
        }

        async fn all_jobs(&self) -> Result<Vec<StoredJob>, String> {
            Ok(self.jobs.lock().unwrap().values().cloned().collect())
        }

        async fn save_participant(
            &self,
            key: &ParticipantKey,
            participant: &StoredParticipant,
        ) -> Result<(), String> {
            self.participants
                .lock()
                .unwrap()
                .insert(key.clone(), participant.clone());
            Ok(())
        }

        async fn delete_participant(&self, key: &ParticipantKey) -> Result<(), String> {
            self.participants.lock().unwrap().remove(key);
            Ok(())
        }

        async fn all_participants(&self) -> Result<Vec<StoredParticipant>, String> {
            Ok(self
                .participants
                .lock()
                .unwrap()
                .values()
                .cloned()
                .collect())
        }
    }

    /// An in-memory store that notifies about job save and deletion, and
    /// about participant save and deletion.
    pub(crate) struct NotifyingStore {
        inner: Arc<dyn Store>,
        saved_tx: mpsc::Sender<JobKey>,
        deleted_tx: mpsc::Sender<JobKey>,
        participant_saved_tx: mpsc::Sender<ParticipantKey>,
        participant_deleted_tx: mpsc::Sender<ParticipantKey>,
    }

    /// The receiving ends of a [`NotifyingStore`]'s participant notifications.
    pub(crate) struct ParticipantNotifications {
        pub saved_rx: mpsc::Receiver<ParticipantKey>,
        pub deleted_rx: mpsc::Receiver<ParticipantKey>,
    }

    pub(crate) fn new_notifying_store() -> (
        Arc<NotifyingStore>,
        mpsc::Receiver<JobKey>,
        mpsc::Receiver<JobKey>,
    ) {
        let (store, saved_rx, deleted_rx, _) = new_notifying_store_with_participants();
        (store, saved_rx, deleted_rx)
    }

    pub(crate) fn new_notifying_store_with_participants() -> (
        Arc<NotifyingStore>,
        mpsc::Receiver<JobKey>,
        mpsc::Receiver<JobKey>,
        ParticipantNotifications,
    ) {
        let (saved_tx, saved_rx) = mpsc::channel(10);
        let (deleted_tx, deleted_rx) = mpsc::channel(10);
        let (participant_saved_tx, participant_saved_rx) = mpsc::channel(10);
        let (participant_deleted_tx, participant_deleted_rx) = mpsc::channel(10);
        (
            Arc::new(NotifyingStore {
                inner: new_in_memory_store(),
                saved_tx,
                deleted_tx,
                participant_saved_tx,
                participant_deleted_tx,
            }),
            saved_rx,
            deleted_rx,
            ParticipantNotifications {
                saved_rx: participant_saved_rx,
                deleted_rx: participant_deleted_rx,
            },
        )
    }

    #[async_trait]
    impl Store for NotifyingStore {
        async fn save_job(&self, key: &JobKey, job: &StoredJob) -> Result<(), String> {
            self.inner.save_job(key, job).await?;
            let _ = self.saved_tx.send(key.clone()).await;
            Ok(())
        }

        async fn delete_job(&self, key: &JobKey) -> Result<(), String> {
            self.inner.delete_job(key).await?;
            let _ = self.deleted_tx.send(key.clone()).await;
            Ok(())
        }

        async fn all_jobs(&self) -> Result<Vec<StoredJob>, String> {
            self.inner.all_jobs().await
        }

        async fn save_participant(
            &self,
            key: &ParticipantKey,
            participant: &StoredParticipant,
        ) -> Result<(), String> {
            self.inner.save_participant(key, participant).await?;
            let _ = self.participant_saved_tx.send(key.clone()).await;
            Ok(())
        }

        async fn delete_participant(&self, key: &ParticipantKey) -> Result<(), String> {
            self.inner.delete_participant(key).await?;
            let _ = self.participant_deleted_tx.send(key.clone()).await;
            Ok(())
        }

        async fn all_participants(&self) -> Result<Vec<StoredParticipant>, String> {
            self.inner.all_participants().await
        }
    }

    /// A store whose job saves block until the gate receives permits.
    pub(crate) struct GatedStore {
        inner: Arc<dyn Store>,
        gate: Arc<tokio::sync::Semaphore>,
    }

    impl GatedStore {
        pub(crate) fn new(inner: Arc<dyn Store>, gate: Arc<tokio::sync::Semaphore>) -> Self {
            Self { inner, gate }
        }
    }

    #[async_trait]
    impl Store for GatedStore {
        async fn save_job(&self, key: &JobKey, job: &StoredJob) -> Result<(), String> {
            self.gate.acquire().await.expect("gate closed").forget();
            self.inner.save_job(key, job).await
        }

        async fn delete_job(&self, key: &JobKey) -> Result<(), String> {
            self.inner.delete_job(key).await
        }

        async fn all_jobs(&self) -> Result<Vec<StoredJob>, String> {
            self.inner.all_jobs().await
        }

        async fn save_participant(
            &self,
            key: &ParticipantKey,
            participant: &StoredParticipant,
        ) -> Result<(), String> {
            self.inner.save_participant(key, participant).await
        }

        async fn delete_participant(&self, key: &ParticipantKey) -> Result<(), String> {
            self.inner.delete_participant(key).await
        }

        async fn all_participants(&self) -> Result<Vec<StoredParticipant>, String> {
            self.inner.all_participants().await
        }
    }

    /// A store that fails on any operation.
    pub(crate) struct FailingStore;

    #[async_trait]
    impl Store for FailingStore {
        async fn save_job(&self, _key: &JobKey, _job: &StoredJob) -> Result<(), String> {
            Err("failed".into())
        }

        async fn delete_job(&self, _key: &JobKey) -> Result<(), String> {
            Err("failed".into())
        }

        async fn all_jobs(&self) -> Result<Vec<StoredJob>, String> {
            Err("failed".into())
        }

        async fn save_participant(
            &self,
            _key: &ParticipantKey,
            _participant: &StoredParticipant,
        ) -> Result<(), String> {
            Err("failed".into())
        }

        async fn delete_participant(&self, _key: &ParticipantKey) -> Result<(), String> {
            Err("failed".into())
        }

        async fn all_participants(&self) -> Result<Vec<StoredParticipant>, String> {
            Err("failed".into())
        }
    }

    // ── mini Redis server (miniredis replacement) ─────────────────────────────

    /// A minimal in-process Redis (RESP2) server supporting exactly the
    /// commands this service uses: PING, HSET, HDEL, HGETALL. Used to keep
    /// the Redis store tests hermetic.
    pub(crate) struct MiniRedis {
        pub addr: std::net::SocketAddr,
        handle: tokio::task::JoinHandle<()>,
    }

    impl Drop for MiniRedis {
        fn drop(&mut self) {
            self.handle.abort();
        }
    }

    pub(crate) async fn spawn_mini_redis() -> MiniRedis {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let state: Arc<Mutex<HashMap<String, HashMap<String, String>>>> =
            Arc::new(Mutex::new(HashMap::new()));

        let handle = tokio::spawn(async move {
            loop {
                let Ok((socket, _)) = listener.accept().await else {
                    return;
                };
                let state = state.clone();
                tokio::spawn(async move {
                    let _ = serve_connection(socket, state).await;
                });
            }
        });

        MiniRedis { addr, handle }
    }

    async fn read_command(
        reader: &mut BufReader<tokio::net::tcp::OwnedReadHalf>,
    ) -> std::io::Result<Option<Vec<String>>> {
        let mut line = String::new();
        if reader.read_line(&mut line).await? == 0 {
            return Ok(None);
        }
        let line = line.trim_end();
        let count: usize = line
            .strip_prefix('*')
            .and_then(|n| n.parse().ok())
            .ok_or_else(|| std::io::Error::other(format!("expected array, got {line:?}")))?;

        let mut parts = Vec::with_capacity(count);
        for _ in 0..count {
            let mut len_line = String::new();
            reader.read_line(&mut len_line).await?;
            let len: usize = len_line
                .trim_end()
                .strip_prefix('$')
                .and_then(|n| n.parse().ok())
                .ok_or_else(|| std::io::Error::other("expected bulk string"))?;
            let mut buf = vec![0u8; len + 2]; // data + CRLF
            reader.read_exact(&mut buf).await?;
            buf.truncate(len);
            parts.push(String::from_utf8_lossy(&buf).into_owned());
        }
        Ok(Some(parts))
    }

    fn bulk_string(s: &str) -> String {
        format!("${}\r\n{s}\r\n", s.len())
    }

    async fn serve_connection(
        socket: tokio::net::TcpStream,
        state: Arc<Mutex<HashMap<String, HashMap<String, String>>>>,
    ) -> std::io::Result<()> {
        let (read_half, mut write_half) = socket.into_split();
        let mut reader = BufReader::new(read_half);

        while let Some(parts) = read_command(&mut reader).await? {
            if parts.is_empty() {
                continue;
            }
            let command = parts[0].to_ascii_uppercase();
            let response = match command.as_str() {
                "PING" => "+PONG\r\n".to_owned(),
                "HSET" if parts.len() >= 4 && parts.len() % 2 == 0 => {
                    let mut state = state.lock().unwrap();
                    let hash = state.entry(parts[1].clone()).or_default();
                    let mut added = 0;
                    for pair in parts[2..].chunks(2) {
                        if hash.insert(pair[0].clone(), pair[1].clone()).is_none() {
                            added += 1;
                        }
                    }
                    format!(":{added}\r\n")
                }
                "HDEL" if parts.len() >= 3 => {
                    let mut state = state.lock().unwrap();
                    let mut removed = 0;
                    if let Some(hash) = state.get_mut(&parts[1]) {
                        for field in &parts[2..] {
                            if hash.remove(field).is_some() {
                                removed += 1;
                            }
                        }
                    }
                    format!(":{removed}\r\n")
                }
                "HGETALL" if parts.len() == 2 => {
                    let state = state.lock().unwrap();
                    let entries: Vec<(String, String)> = state
                        .get(&parts[1])
                        .map(|hash| hash.iter().map(|(k, v)| (k.clone(), v.clone())).collect())
                        .unwrap_or_default();
                    let mut out = format!("*{}\r\n", entries.len() * 2);
                    for (k, v) in entries {
                        out.push_str(&bulk_string(&k));
                        out.push_str(&bulk_string(&v));
                    }
                    out
                }
                _ => "+OK\r\n".to_owned(),
            };
            write_half.write_all(response.as_bytes()).await?;
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use chrono::SubsecRound;

    use super::test_support::*;
    use super::*;
    use crate::helper::{LiveKitIdentity, LiveKitRoomAlias};

    async fn run_store_tests<F, Fut>(new_store: F)
    where
        F: Fn() -> Fut,
        Fut: std::future::Future<Output = Arc<dyn Store>>,
    {
        let room = LiveKitRoomAlias("!room:example.com".into());
        let identity = LiveKitIdentity("@user:example.com".into());
        let key = JobKey {
            room: room.clone(),
            identity: identity.clone(),
        };
        let params = DelayedEventJobParams {
            delay_id: "id".into(),
            server_name: "example.com".into(),
            delay_timeout: Duration::from_secs(30),
            livekit_room: room.clone(),
            livekit_identity: identity.clone(),
            owner_user_id: String::new(),
        };
        let job = StoredJob {
            params: params.clone(),
            restarted_at: Utc::now().trunc_subsecs(3),
        };

        // TestLoadAllJobsOnEmptyStore
        {
            let store = new_store().await;

            // Load all jobs and check that the store is empty.
            let jobs = store.all_jobs().await.expect("getting all jobs failed");
            assert_eq!(jobs.len(), 0, "expected 0 jobs");
        }

        // TestSaveJobOnEmptyStore
        {
            let store = new_store().await;

            // Save a job.
            store.save_job(&key, &job).await.expect("saving job failed");

            // Load all jobs and check that we get the saved job.
            let jobs = store.all_jobs().await.expect("getting all jobs failed");
            assert_eq!(jobs.len(), 1, "expected 1 job");
            assert_eq!(jobs[0], job);
        }

        // TestSaveJobOverwrites
        {
            let store = new_store().await;

            // Save a job.
            store
                .save_job(&key, &job)
                .await
                .expect("saving first job failed");

            // Save an updated job for the same key.
            let mut updated = job.clone();
            updated.params.delay_id = "delay-updated".into();
            store
                .save_job(&key, &updated)
                .await
                .expect("saving second job failed");

            // Load all jobs and check that we get the updated job.
            let jobs = store.all_jobs().await.expect("getting all jobs failed");
            assert_eq!(jobs.len(), 1, "expected 1 job after overwrite");
            assert_eq!(jobs[0], updated);
        }

        // TestDeleteJob
        {
            let store = new_store().await;

            // Save a job.
            store
                .save_job(&key, &job)
                .await
                .expect("saving first job failed");

            // Save another job.
            let identity2 = LiveKitIdentity("@other:example.com:device-id:member-id".into());
            let key2 = JobKey {
                room: room.clone(),
                identity: identity2.clone(),
            };
            let mut job2 = job.clone();
            job2.params.livekit_identity = identity2;
            store
                .save_job(&key2, &job2)
                .await
                .expect("saving second job failed");

            // Delete the first job.
            store
                .delete_job(&key)
                .await
                .expect("deleting first job failed");

            // Load all jobs and check that we still get the second job.
            let jobs = store.all_jobs().await.expect("getting all jobs failed");
            assert_eq!(jobs.len(), 1, "expected 1 job after delete");
            assert_eq!(jobs[0], job2);
        }

        // TestDeleteMissingJob
        {
            let store = new_store().await;

            // Delete a job that doesn't exist in the store.
            let key = JobKey {
                room: LiveKitRoomAlias("nonexistent-room".into()),
                identity: LiveKitIdentity("@nonexistent:example.com".into()),
            };
            store
                .delete_job(&key)
                .await
                .expect("deleting missing job failed");
        }

        // ── participants ─────────────────────────────────────────────────────

        let participant = StoredParticipant {
            matrix_room_id: "!room:example.com".into(),
            matrix_user_id: "@user:example.com".into(),
            livekit_room: room.clone(),
            livekit_identity: identity.clone(),
            registered_at: Utc::now().trunc_subsecs(3),
        };
        let participant_key = participant.key();

        // Participants and jobs live side by side without interfering.
        {
            let store = new_store().await;
            store.save_job(&key, &job).await.expect("saving job failed");

            assert!(
                store
                    .all_participants()
                    .await
                    .expect("getting all participants failed")
                    .is_empty(),
                "expected no participants on a store holding only a job"
            );

            store
                .save_participant(&participant_key, &participant)
                .await
                .expect("saving participant failed");
            let participants = store
                .all_participants()
                .await
                .expect("getting all participants failed");
            assert_eq!(participants, vec![participant.clone()]);
            assert_eq!(
                store.all_jobs().await.expect("getting all jobs failed"),
                vec![job.clone()],
                "expected the job to be untouched by participant writes"
            );
        }

        // Saving a participant under an existing key overwrites it.
        {
            let store = new_store().await;
            store
                .save_participant(&participant_key, &participant)
                .await
                .expect("saving first participant failed");

            let mut updated = participant.clone();
            updated.matrix_room_id = "!other:example.com".into();
            store
                .save_participant(&participant_key, &updated)
                .await
                .expect("saving second participant failed");

            let participants = store
                .all_participants()
                .await
                .expect("getting all participants failed");
            assert_eq!(participants, vec![updated]);
        }

        // Deleting a participant leaves the others in place; deleting a
        // missing one is not an error.
        {
            let store = new_store().await;
            store
                .save_participant(&participant_key, &participant)
                .await
                .expect("saving first participant failed");

            let mut other = participant.clone();
            other.livekit_identity = LiveKitIdentity("@other:example.com".into());
            store
                .save_participant(&other.key(), &other)
                .await
                .expect("saving second participant failed");

            store
                .delete_participant(&participant_key)
                .await
                .expect("deleting first participant failed");
            store
                .delete_participant(&participant_key)
                .await
                .expect("deleting the participant again failed");

            let participants = store
                .all_participants()
                .await
                .expect("getting all participants failed");
            assert_eq!(participants, vec![other]);
            assert!(
                store
                    .all_jobs()
                    .await
                    .expect("getting all jobs failed")
                    .is_empty(),
                "expected participant writes not to create jobs"
            );
        }
    }

    // Redis store tests

    async fn new_mini_redis_store(
        mini: &MiniRedis,
    ) -> (Arc<dyn Store>, redis::aio::ConnectionManager) {
        let client = redis::Client::open(format!("redis://{}", mini.addr)).unwrap();
        let conn = client.get_connection_manager().await.unwrap();
        (Arc::new(RedisStore::with_connection(conn.clone())), conn)
    }

    #[tokio::test]
    async fn test_redis_store() {
        run_store_tests(|| async {
            // Each invocation gets a fresh mini-redis. The server task lives
            // until the runtime shuts down (the guard is leaked deliberately).
            let mini = spawn_mini_redis().await;
            let (store, _conn) = new_mini_redis_store(&mini).await;
            std::mem::forget(mini);
            store
        })
        .await;
    }

    #[tokio::test]
    async fn test_redis_store_skips_unparseable_entry() {
        let mini = spawn_mini_redis().await;
        let (store, mut conn) = new_mini_redis_store(&mini).await;

        // Save a corrupt JSON entry directly via the Redis client.
        redis::cmd("HSET")
            .arg(REDIS_JOBS_HASH_KEY)
            .arg("@bad:identity")
            .arg("not valid json")
            .query_async::<()>(&mut conn)
            .await
            .expect("writing into store failed");

        // Save a valid entry alongside it.
        let room = LiveKitRoomAlias("test-room".into());
        let identity = LiveKitIdentity("@good:example.com".into());
        let key = JobKey {
            room: room.clone(),
            identity: identity.clone(),
        };
        let job = StoredJob {
            params: DelayedEventJobParams {
                delay_id: "id".into(),
                delay_timeout: Duration::from_secs(1),
                server_name: String::new(),
                livekit_room: room,
                livekit_identity: identity,
                owner_user_id: String::new(),
            },
            restarted_at: Utc::now().trunc_subsecs(3),
        };
        store.save_job(&key, &job).await.expect("saving job failed");

        // Load all jobs and check that we only get the valid entry.
        let jobs = store.all_jobs().await.expect("getting all jobs failed");
        assert_eq!(jobs.len(), 1, "expected 1 valid job");
        assert_eq!(jobs[0], job);
    }
}

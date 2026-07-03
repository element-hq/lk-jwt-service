// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// retry.rs: exponential backoff with the same semantics as the
// cenkalti/backoff v5 library used by the Go implementation:
//
//   - permanent errors abort the retry loop immediately,
//   - retry-after errors override the next wait interval with a server hint,
//   - transient errors retry on the exponential schedule,
//   - the loop gives up once the next wait would exceed `max_elapsed`
//     (`max_elapsed == 0` means retry forever),
//   - cancellation aborts the loop between attempts.

use std::future::Future;
use std::time::Duration;

use rand::Rng;
use tokio::time::Instant;
use tokio_util::sync::CancellationToken;

/// How an error should be treated by [`retry`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ErrorClass {
    /// Do not retry; surface the error immediately.
    Permanent,
    /// Retry on the exponential schedule.
    Transient,
    /// Retry after the given server-provided delay.
    RetryAfter(Duration),
}

/// Implemented by error types fed through [`retry`].
pub trait Classify {
    fn classify(&self) -> ErrorClass;
}

/// The result of a failed [`retry`] call.
#[derive(Debug, thiserror::Error)]
pub enum RetryError<E> {
    /// The last error returned by the operation (permanent, or the retry
    /// budget was exhausted).
    #[error(transparent)]
    Error(E),
    /// The cancellation token fired while waiting between attempts.
    #[error("retry cancelled")]
    Cancelled,
}

/// Exponential backoff schedule matching cenkalti/backoff's
/// `NewExponentialBackOff` shape.
#[derive(Debug, Clone)]
pub struct ExponentialBackoff {
    pub initial_interval: Duration,
    pub multiplier: f64,
    pub randomization_factor: f64,
    pub max_interval: Duration,
}

impl ExponentialBackoff {
    /// The parameters used by every retry loop in this service.
    pub fn service_default() -> Self {
        Self {
            initial_interval: Duration::from_secs(1),
            multiplier: 1.5,
            randomization_factor: 0.5,
            max_interval: Duration::from_secs(60),
        }
    }
}

struct BackoffState {
    schedule: ExponentialBackoff,
    current: Duration,
}

impl BackoffState {
    fn new(schedule: ExponentialBackoff) -> Self {
        let current = schedule.initial_interval;
        Self { schedule, current }
    }

    /// Returns the next randomized wait and advances the schedule.
    fn next_interval(&mut self) -> Duration {
        let rf = self.schedule.randomization_factor;
        let base = self.current.as_secs_f64();
        let randomized = if rf > 0.0 {
            let delta = rf * base;
            rand::thread_rng().gen_range((base - delta)..=(base + delta))
        } else {
            base
        };
        let next = self.current.mul_f64(self.schedule.multiplier);
        self.current = next.min(self.schedule.max_interval);
        Duration::from_secs_f64(randomized.max(0.0))
    }
}

/// Retries `op` with exponential backoff until it succeeds, returns a
/// permanent error, the retry budget (`max_elapsed`) is exhausted, or
/// `cancel` fires. `max_elapsed == 0` retries forever (matching
/// backoff v5's `WithMaxElapsedTime(0)`).
pub async fn retry<T, E, F, Fut>(
    cancel: &CancellationToken,
    schedule: ExponentialBackoff,
    max_elapsed: Duration,
    mut op: F,
) -> Result<T, RetryError<E>>
where
    E: Classify,
    F: FnMut() -> Fut,
    Fut: Future<Output = Result<T, E>>,
{
    let start = Instant::now();
    let mut backoff = BackoffState::new(schedule);

    loop {
        if cancel.is_cancelled() {
            return Err(RetryError::Cancelled);
        }

        let err = match op().await {
            Ok(v) => return Ok(v),
            Err(e) => e,
        };

        let wait = match err.classify() {
            ErrorClass::Permanent => return Err(RetryError::Error(err)),
            ErrorClass::RetryAfter(d) => d,
            ErrorClass::Transient => backoff.next_interval(),
        };

        // Give up once the next wait would push us past the elapsed-time
        // budget — the same stop condition backoff v5 applies.
        if max_elapsed > Duration::ZERO && start.elapsed() + wait > max_elapsed {
            return Err(RetryError::Error(err));
        }

        tokio::select! {
            _ = cancel.cancelled() => return Err(RetryError::Cancelled),
            _ = tokio::time::sleep(wait) => {}
        }
    }
}

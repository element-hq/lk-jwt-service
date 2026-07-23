// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//! Retrying with exponential backoff:
//!
//!   - permanent errors abort the retry loop immediately,
//!   - retry-after errors override the next wait with a server hint,
//!   - transient errors retry on the exponential schedule,
//!   - the loop gives up once the next wait would exceed `max_elapsed`
//!     (`max_elapsed == 0` means retry forever),
//!   - cancellation aborts the loop between attempts.

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

/// Classifies an error for [`retry`].
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
    /// The cancellation token fired.
    #[error("retry cancelled")]
    Cancelled,
}

/// An exponential backoff schedule.
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
/// `cancel` fires. `max_elapsed == 0` retries forever.
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

        // Give up once the next wait would push past the elapsed-time budget.
        if max_elapsed > Duration::ZERO && start.elapsed() + wait > max_elapsed {
            return Err(RetryError::Error(err));
        }

        tokio::select! {
            _ = cancel.cancelled() => return Err(RetryError::Cancelled),
            _ = tokio::time::sleep(wait) => {}
        }
    }
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::{AtomicU32, Ordering};
    use std::sync::Arc;

    use super::*;

    #[derive(Debug)]
    struct TestError(ErrorClass);

    impl Classify for TestError {
        fn classify(&self) -> ErrorClass {
            self.0
        }
    }

    fn schedule() -> ExponentialBackoff {
        ExponentialBackoff {
            initial_interval: Duration::from_secs(1),
            multiplier: 2.0,
            randomization_factor: 0.0, // deterministic waits
            max_interval: Duration::from_secs(60),
        }
    }

    /// A success on the first attempt returns without waiting.
    #[tokio::test(start_paused = true)]
    async fn test_retry_immediate_success() {
        let cancel = CancellationToken::new();
        let result: Result<u32, RetryError<TestError>> =
            retry(&cancel, schedule(), Duration::from_secs(10), || async {
                Ok(7)
            })
            .await;
        assert_eq!(result.unwrap(), 7);
    }

    /// A permanent error stops the loop after a single attempt.
    #[tokio::test(start_paused = true)]
    async fn test_retry_permanent_stops_immediately() {
        let cancel = CancellationToken::new();
        let attempts = Arc::new(AtomicU32::new(0));
        let attempts_clone = attempts.clone();
        let result: Result<(), RetryError<TestError>> =
            retry(&cancel, schedule(), Duration::from_secs(100), || {
                let attempts = attempts_clone.clone();
                async move {
                    attempts.fetch_add(1, Ordering::SeqCst);
                    Err(TestError(ErrorClass::Permanent))
                }
            })
            .await;
        assert!(matches!(result, Err(RetryError::Error(_))));
        assert_eq!(attempts.load(Ordering::SeqCst), 1);
    }

    /// Transient errors are retried until the elapsed budget would be
    /// exceeded by the next wait. With deterministic waits of 1s, 2s, 4s,
    /// ... and a 4s budget, attempts run at t=0, t=1 and t=3.
    #[tokio::test(start_paused = true)]
    async fn test_retry_transient_respects_max_elapsed() {
        let cancel = CancellationToken::new();
        let attempts = Arc::new(AtomicU32::new(0));
        let attempts_clone = attempts.clone();
        let result: Result<(), RetryError<TestError>> =
            retry(&cancel, schedule(), Duration::from_secs(4), || {
                let attempts = attempts_clone.clone();
                async move {
                    attempts.fetch_add(1, Ordering::SeqCst);
                    Err(TestError(ErrorClass::Transient))
                }
            })
            .await;
        assert!(matches!(result, Err(RetryError::Error(_))));
        assert_eq!(attempts.load(Ordering::SeqCst), 3);
    }

    /// A retry-after error waits for the server hint instead of the
    /// exponential schedule.
    #[tokio::test(start_paused = true)]
    async fn test_retry_after_uses_server_hint() {
        let cancel = CancellationToken::new();
        let attempts = Arc::new(AtomicU32::new(0));
        let attempts_clone = attempts.clone();
        let start = Instant::now();
        let result: Result<u32, RetryError<TestError>> =
            retry(&cancel, schedule(), Duration::from_secs(100), || {
                let attempts = attempts_clone.clone();
                async move {
                    if attempts.fetch_add(1, Ordering::SeqCst) == 0 {
                        Err(TestError(ErrorClass::RetryAfter(Duration::from_secs(30))))
                    } else {
                        Ok(1)
                    }
                }
            })
            .await;
        assert_eq!(result.unwrap(), 1);
        assert_eq!(attempts.load(Ordering::SeqCst), 2);
        assert!(
            start.elapsed() >= Duration::from_secs(30),
            "expected the 30s hint to be waited"
        );
    }

    /// Cancellation during the wait aborts the loop.
    #[tokio::test(start_paused = true)]
    async fn test_retry_cancelled_during_wait() {
        let cancel = CancellationToken::new();
        let canceller = cancel.clone();
        tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(500)).await;
            canceller.cancel();
        });
        let result: Result<(), RetryError<TestError>> =
            retry(&cancel, schedule(), Duration::ZERO, || async {
                Err(TestError(ErrorClass::Transient))
            })
            .await;
        assert!(matches!(result, Err(RetryError::Cancelled)));
    }

    /// `max_elapsed == 0` never gives up on its own.
    #[tokio::test(start_paused = true)]
    async fn test_retry_zero_budget_retries_until_success() {
        let cancel = CancellationToken::new();
        let attempts = Arc::new(AtomicU32::new(0));
        let attempts_clone = attempts.clone();
        let result: Result<u32, RetryError<TestError>> =
            retry(&cancel, schedule(), Duration::ZERO, || {
                let attempts = attempts_clone.clone();
                async move {
                    if attempts.fetch_add(1, Ordering::SeqCst) < 20 {
                        Err(TestError(ErrorClass::Transient))
                    } else {
                        Ok(9)
                    }
                }
            })
            .await;
        assert_eq!(result.unwrap(), 9);
        assert_eq!(attempts.load(Ordering::SeqCst), 21);
    }
}

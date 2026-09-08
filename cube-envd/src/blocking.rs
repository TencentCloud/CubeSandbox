// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Single entry point for blocking (syscall-performing) work.
//!
//! Every filesystem RPC body, /metrics sampling and user-table read runs
//! here instead of directly on a tokio worker. Rust has no goroutine: the
//! Go baseline runs each handler as a blocking goroutine, and the closest
//! faithful analogue is one `spawn_blocking` crossing **per request**, never
//! one per syscall (`tokio::fs`'s mistake — each crossing costs ~29µs
//! measured, while the syscall itself is ~0.3µs; see
//! docs/cube-envd/fs-counterproposal-2026-09-08.md §1.4-1.6).
//!
//! Minimal version (plan §5): in-flight counting + peak tracking only. The
//! long-op semaphore is deliberately absent until queueing is actually
//! observed. `/metrics` is wire-compatible with the Go baseline, so the
//! counters are NOT exposed there — the unit tests and the worker-heartbeat
//! guard below assert on them directly.
//!
//! Removal clause (plan §5.3): if `blocking::run` ever has fewer than five
//! call sites, delete this module and go back to bare `spawn_blocking`.

use std::sync::atomic::{AtomicUsize, Ordering};

static IN_FLIGHT: AtomicUsize = AtomicUsize::new(0);
static PEAK: AtomicUsize = AtomicUsize::new(0);

/// Blocking tasks currently executing.
pub fn in_flight() -> usize {
    IN_FLIGHT.load(Ordering::Relaxed)
}

/// High-water mark of `in_flight` since process start.
pub fn peak() -> usize {
    PEAK.load(Ordering::Relaxed)
}

fn enter() {
    let n = IN_FLIGHT.fetch_add(1, Ordering::Relaxed) + 1;
    PEAK.fetch_max(n, Ordering::Relaxed);
    // The observability surface for "is the pool being touched" (plan §5):
    // a new peak is logged, but /metrics stays byte-identical with the Go
    // baseline, so nothing is added to the wire.
    if n > 1 && n == peak() {
        tracing::debug!(
            target: "blocking",
            peak = peak(),
            in_flight = in_flight(),
            "blocking concurrency reached a new peak"
        );
    }
}

/// Returns the previous in-flight count.
fn exit() -> usize {
    IN_FLIGHT.fetch_sub(1, Ordering::Relaxed) - 1
}

/// Guard so a panicking closure still decrements `in_flight`.
struct DecGuard;

impl Drop for DecGuard {
    fn drop(&mut self) {
        exit();
    }
}

/// Run `f` on tokio's blocking pool — exactly one crossing for the whole
/// request body. Concurrency comes from the pool (`max_blocking_threads`,
/// capped at 64 in main.rs).
///
/// The in-flight counter increments when the closure *starts executing*
/// (inside the closure), so a task cancelled while still queued — possible
/// only on runtime shutdown — never inflates or leaks the count, and
/// `in_flight()` means "currently executing", matching its doc. A closure
/// panic is re-thrown with its original payload via `resume_unwind`.
pub async fn run<F, R>(_label: &'static str, f: F) -> R
where
    F: FnOnce() -> R + Send + 'static,
    R: Send + 'static,
{
    tokio::task::spawn_blocking(move || {
        enter();
        let _guard = DecGuard;
        f()
    })
    .await
    .unwrap_or_else(|e| match e.try_into_panic() {
        Ok(payload) => std::panic::resume_unwind(payload),
        Err(cancelled) => {
            panic!("blocking task was cancelled before running (runtime shutdown): {cancelled}")
        }
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;
    use std::time::{Duration, Instant};

    /// IN_FLIGHT/PEAK are process-global and libtest runs #[test]s on
    /// parallel threads — both tests hold this lock so their counter
    /// assertions never observe each other's tasks.
    static TEST_SERIAL: Mutex<()> = Mutex::new(());

    /// The guardrail from plan §5: while blocking tasks occupy the pool,
    /// the async workers must keep ticking. A regression that runs blocking
    /// work on a worker (the pre-PR-A bug) makes this gap explode.
    /// CI-safe: the bound is an order of magnitude above the expected tick.
    #[test]
    fn workers_keep_ticking_during_blocking_calls() {
        let _serial = TEST_SERIAL.lock().unwrap_or_else(|p| p.into_inner());
        let rt = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .build()
            .unwrap();
        rt.block_on(async {
            // join_all polls every future concurrently — 8 tasks land on the
            // pool together (lazy futures would run one-by-one).
            let sleeps = (0..8).map(|_| {
                run("test.slow", || {
                    std::thread::sleep(Duration::from_millis(120))
                })
            });
            let mut max_gap_ms = 0u128;
            let deadline = Instant::now() + Duration::from_millis(100);
            let all = futures::future::join_all(sleeps);
            tokio::pin!(all);
            while Instant::now() < deadline {
                let step = Instant::now();
                tokio::time::sleep(Duration::from_millis(10)).await;
                max_gap_ms = max_gap_ms.max(step.elapsed().as_millis());
            }
            all.await;
            assert!(
                max_gap_ms < 100,
                "async tick gap was {max_gap_ms}ms — a worker is running blocking work"
            );
            // 8 concurrent blocking tasks must have been observed by the
            // counter (tokio's pool grows on demand; 8 << 64 cap).
            assert!(peak() >= 8, "peak = {}", peak());
            assert_eq!(in_flight(), 0, "all blocking tasks have completed");
        });
    }

    /// A panicking closure must not leak the in-flight count.
    #[test]
    fn panic_in_closure_still_decrements() {
        let _serial = TEST_SERIAL.lock().unwrap_or_else(|p| p.into_inner());
        let rt = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(1)
            .enable_all()
            .build()
            .unwrap();
        rt.block_on(async {
            let before = in_flight();
            // `run` re-throws the original payload via resume_unwind; hosting
            // the await in a nested task keeps the test alive to observe
            // that the guard still decremented.
            let jh = tokio::spawn(async {
                run("test.panic", || -> i32 { panic!("boom") }).await;
            });
            assert!(jh.await.is_err(), "the panic must propagate");
            assert_eq!(in_flight(), before);
        });
    }
}

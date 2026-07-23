// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Webhook event notification backend.
//!
//! Replaces the previous batch-log-forwarding stub.  Implements
//! the [`Logger`] trait to deliver sandbox lifecycle events
//! (`sandbox.created`, `sandbox.deleted`, `sandbox.paused`,
//! `sandbox.resumed`) to configured HTTP endpoints via
//! asynchronous POST requests with optional HMAC-SHA256 signing
//! and exponential-backoff retry.
//!
//! # Architecture
//!
//! ```text
//! log() → tx.send() → receiver loop → Semaphore → spawn → join_all → POST
//! ```
//!
//! - `log()` does only a non-blocking channel send (microsecond latency).
//! - A single background receiver task dispatches delivery to spawned tasks.
//! - `Arc<Semaphore>` bounds concurrent delivery tasks at 64 (consumer-side
//!   throttling — the HTTP handler is never backpressured).
//! - `Arc<AtomicUsize>` tracks inflight deliveries so `flush()` can wait for
//!   completion during graceful shutdown.
//!
//! # MVP scope — intentionally omitted
//!
//! 1. No jitter: single-instance has no thundering herd. For multi-instance,
//!    change `sleep(1 << attempt)` to sleep + jitter (one line).
//! 2. No outer delivery timeout: reqwest 30s per-request timeout + bounded
//!    retry (max 4 attempts) provides natural upper bound ~127s. If reqwest
//!    timeout fails, permit leaks → semaphore exhausts → channel grows.
//! 3. No PendingGuard RAII: task panic causes pending leak → flush timeout
//!    + warn. No data loss, only slower shutdown. Add back when code complex.
//! 4. No event name validation: invalid names never match → no output →
//!    user debugs naturally. "Empty=all" only triggers on explicit empty
//!    config, not from filtering invalid entries.

use std::collections::HashSet;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use tokio::sync::mpsc::{self, UnboundedSender};
use tokio::sync::{oneshot, Semaphore};

use super::{LogEvent, Logger};

// ─── Message enum ─────────────────────────────────────────────────────────

enum Msg {
    Event(LogEvent),
    Flush(oneshot::Sender<()>),
}

// ─── HttpLoggerConfig ─────────────────────────────────────────────────────

/// Configuration for `HttpLogger`.
pub struct HttpLoggerConfig {
    /// Webhook endpoint URLs.
    pub targets: Vec<String>,
    /// Event types to deliver. Empty = subscribe to all events.
    pub subscribed_events: HashSet<String>,
    /// Shared HMAC secret. Empty = no signing.
    pub secret: String,
    /// Maximum concurrent delivery tasks (Semaphore permits). Default: 64.
    pub max_concurrency: usize,
    /// HTTP client shared across all delivery tasks (clone is O(1)).
    pub http_client: reqwest::Client,
}

impl std::fmt::Debug for HttpLoggerConfig {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("HttpLoggerConfig")
            .field("targets", &self.targets)
            .field("subscribed_events", &self.subscribed_events)
            .field("secret", &"[REDACTED]")
            .field("max_concurrency", &self.max_concurrency)
            .finish()
    }
}

// ─── HttpLogger ───────────────────────────────────────────────────────────

/// Webhook event delivery backend. Clone is O(1) — only the channel sender
/// is cloned.
pub struct HttpLogger {
    tx: UnboundedSender<Msg>,
    subscribed_events: Arc<HashSet<String>>,
}

impl HttpLogger {
    /// Create an `HttpLogger` and spawn the background delivery loop.
    pub fn new(config: HttpLoggerConfig) -> Self {
        let HttpLoggerConfig {
            targets,
            subscribed_events,
            secret,
            max_concurrency,
            http_client,
        } = config;

        let targets = Arc::new(targets);
        let subscribed_events = Arc::new(subscribed_events);
        let secret: Arc<str> = Arc::from(secret.as_str());
        let pending = Arc::new(AtomicUsize::new(0));
        let delivery_semaphore = Arc::new(Semaphore::new(max_concurrency.max(1)));

        let (tx, mut rx) = mpsc::unbounded_channel::<Msg>();

        // ── Background receiver task ──────────────────────────────────────
        let targets_bg = targets.clone();
        let subscribed_events_bg = subscribed_events.clone();
        let secret_bg = secret.clone();
        let http_client_bg = http_client.clone();
        let pending_bg = pending.clone();
        let sem = delivery_semaphore.clone();

        tokio::spawn(async move {
            while let Some(msg) = rx.recv().await {
                match msg {
                    Msg::Event(event) => {
                        if !subscribed_events_bg.is_empty()
                            && !subscribed_events_bg.contains(&event.event)
                        {
                            continue;
                        }

                        let payload = build_payload(&event);
                        let body_bytes =
                            bytes::Bytes::from(serde_json::to_string(&payload).unwrap_or_default());
                        let event_name = event.event.clone();

                        pending_bg.fetch_add(1, Ordering::Relaxed);

                        let sem = sem.clone();
                        let targets = targets_bg.clone();
                        let secret = secret_bg.clone();
                        let client = http_client_bg.clone();
                        let pending = pending_bg.clone();

                        tokio::spawn(async move {
                            // Acquire inside spawn so the receiver loop
                            // is never blocked — Msg::Flush is always
                            // processed promptly.
                            let _permit = sem.acquire_owned().await;
                            let futs: Vec<_> = targets
                                .iter()
                                .map(|url| {
                                    deliver_with_retry(
                                        &client,
                                        url,
                                        body_bytes.clone(),
                                        &secret,
                                        &event_name,
                                    )
                                })
                                .collect();
                            futures::future::join_all(futs).await;
                            pending.fetch_sub(1, Ordering::Relaxed);
                        });
                    }

                    Msg::Flush(reply) => {
                        // Wait for all in-flight deliveries with a timeout.
                        let deadline = tokio::time::Instant::now() + Duration::from_secs(30);
                        loop {
                            let n = pending_bg.load(Ordering::Relaxed);
                            if n == 0 {
                                break;
                            }
                            if tokio::time::Instant::now() > deadline {
                                tracing::warn!(
                                    pending = n,
                                    "webhook: flush timed out, {} deliveries still in-flight",
                                    n
                                );
                                break;
                            }
                            tokio::time::sleep(Duration::from_millis(100)).await;
                        }
                        let _ = reply.send(());
                    }
                }
            }
        });

        Self {
            tx,
            subscribed_events: subscribed_events.clone(),
        }
    }
}

#[async_trait]
impl Logger for HttpLogger {
    /// Non-blocking enqueue. Filters subscribed events BEFORE `tx.send()`
    /// to avoid wasting channel capacity.
    ///
    /// `#[async_trait]` requires `async fn` — the function body contains
    /// no `.await` (same pattern as `FileLogger::log`).
    async fn log(&self, event: LogEvent) {
        // ── Pre-send filter ──────────────────────────────────────────────
        // Filter BEFORE enqueue — avoids wasting channel capacity.
        if !self.subscribed_events.is_empty() && !self.subscribed_events.contains(&event.event) {
            return;
        }

        if self.tx.send(Msg::Event(event)).is_err() {
            tracing::error!("webhook: background task is gone, dropping event");
        }
    }

    async fn flush(&self) {
        let (tx, rx) = oneshot::channel();
        if self.tx.send(Msg::Flush(tx)).is_ok() {
            let _ = rx.await;
        }
    }

    fn name(&self) -> &'static str {
        "webhook"
    }
}

// ─── Helpers ──────────────────────────────────────────────────────────────

/// Build the JSON payload for a webhook POST.
fn build_payload(event: &LogEvent) -> serde_json::Value {
    let mut payload = serde_json::json!({
        "event": event.event,
        "timestamp": event.timestamp.to_rfc3339(),
    });
    if let Some(obj) = payload.as_object_mut() {
        for (k, v) in &event.fields {
            obj.insert(k.clone(), v.clone());
        }
    }
    payload
}

/// HMAC-SHA256 sign the raw body bytes. Returns e.g. `"sha256=abcdef..."`.
fn sign_payload(secret: &str, body: &[u8]) -> String {
    use hmac::{Hmac, Mac};
    use sha2::Sha256;

    type HmacSha256 = Hmac<Sha256>;
    let mut mac =
        HmacSha256::new_from_slice(secret.as_bytes()).expect("HMAC can take key of any size");
    mac.update(body);
    let result = mac.finalize();
    format!("sha256={}", hex::encode(result.into_bytes()))
}

/// Extract host portion from a URL for safe logging.
fn redact_url(url: &str) -> &str {
    // Strip scheme
    let s = url
        .strip_prefix("https://")
        .or_else(|| url.strip_prefix("http://"))
        .unwrap_or(url);
    // Strip path / query / fragment
    s.split('/').next().unwrap_or(s)
}

/// Deliver a single event to a single webhook endpoint, with retry.
async fn deliver_with_retry(
    client: &reqwest::Client,
    url: &str,
    body: bytes::Bytes,
    secret: &str,
    event_name: &str,
) {
    let delivery_id = uuid::Uuid::new_v4().to_string();
    let host = redact_url(url);

    for attempt in 0..=3 {
        if attempt > 0 {
            let delay = Duration::from_secs(1 << (attempt - 1)); // 1s, 2s, 4s
            tokio::time::sleep(delay).await;
        }

        let mut req = client
            .post(url)
            .header("Content-Type", "application/json")
            .header("X-Cube-Event", event_name)
            .header("X-Cube-Delivery", delivery_id.as_str())
            .header("User-Agent", "CubeAPI-Webhook/1.0")
            .body(body.clone());

        if !secret.is_empty() {
            let sig = sign_payload(secret, &body);
            req = req.header("X-Cube-Signature", sig);
        }

        match req.send().await {
            Ok(resp) if resp.status().is_success() => {
                return;
            }
            Ok(resp) if resp.status().is_client_error() => {
                tracing::warn!(
                    url = %host,
                    delivery_id = %delivery_id,
                    status = %resp.status(),
                    event = %event_name,
                    "webhook delivery: client error, not retrying"
                );
                return;
            }
            Ok(resp) => {
                tracing::warn!(
                    url = %host,
                    delivery_id = %delivery_id,
                    status = %resp.status(),
                    event = %event_name,
                    attempt = attempt + 1,
                    "webhook delivery: server error, will retry"
                );
            }
            Err(e) => {
                tracing::warn!(
                    url = %host,
                    delivery_id = %delivery_id,
                    error = %e,
                    event = %event_name,
                    attempt = attempt + 1,
                    "webhook delivery: connection error, will retry"
                );
            }
        }
    }

    tracing::error!(
        url = %host,
        delivery_id = %delivery_id,
        event = %event_name,
        "webhook delivery failed after 4 attempts"
    );
}

// ─── Tests ───────────────────────────────────────────────────────────────


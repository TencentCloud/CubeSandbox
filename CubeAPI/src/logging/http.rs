// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! HTTP webhook log backend.
//!
//! Delivers sandbox lifecycle events (`sandbox.created`, `sandbox.deleted`,
//! `sandbox.paused`, `sandbox.resumed`, …) to user-configured HTTP endpoints
//! as JSON `POST` requests.
//!
//! # Design
//!
//! This backend implements [`Logger`] and is registered inside `MultiLogger`
//! alongside the file backend, so it consumes the **same** structured event
//! stream the rest of the application already emits — no changes to the
//! sandbox handlers are required.
//!
//! `log()` is fully non-blocking: it only forwards subscribed events into an
//! unbounded channel and returns immediately. A background dispatcher task
//! reads the channel and, for every endpoint subscribed to that event type,
//! **spawns an independent delivery task**. This isolation guarantees that a
//! slow or unreachable endpoint can never stall event emission on the sandbox
//! API request path, nor delay delivery to other endpoints.
//!
//! Each delivery:
//! - builds a JSON payload (`event`, `timestamp`, plus all event fields such
//!   as `sandbox_id` and `template_id`),
//! - tags the request with a unique `X-Cube-Delivery` id, stable across
//!   retries, so receivers can deduplicate,
//! - optionally signs the body with HMAC-SHA256 (`X-Cube-Signature` header),
//! - POSTs with a per-endpoint timeout,
//! - retries with exponential backoff on failure, logging the final error.

use super::{LogEvent, Logger};
use crate::config::WebhookConfig;
use async_trait::async_trait;
use hmac::{Hmac, Mac};
use sha2::Sha256;
use std::sync::{Arc, RwLock};
use std::time::Duration;
use tokio::sync::mpsc::{self, UnboundedSender};
use tracing::{debug, error, warn};

type HmacSha256 = Hmac<Sha256>;

/// Upper bound on the exponential backoff sleep between retries.
const MAX_BACKOFF_SECS: u64 = 30;

// ─── WebhookRegistry ─────────────────────────────────────────────────────────

/// One registered webhook endpoint: a stable id plus its configuration.
#[derive(Clone)]
pub struct WebhookEntry {
    pub id: String,
    pub config: WebhookConfig,
}

/// Thread-safe, runtime-mutable set of webhook endpoints, shared between the
/// delivery backend ([`WebhookLogger`]) and the management API handlers.
///
/// Backed by an `Arc<RwLock<..>>`: many readers (event delivery, listing) run
/// concurrently, while a writer (the management API) takes exclusive access
/// only for the brief moment it mutates the list. Cloning is O(1) — every
/// clone shares the same underlying lock.
#[derive(Clone, Default)]
pub struct WebhookRegistry {
    inner: Arc<RwLock<Vec<WebhookEntry>>>,
}

impl WebhookRegistry {
    /// Seed the registry from the startup config, assigning each endpoint an id.
    pub fn from_configs(configs: Vec<WebhookConfig>) -> Self {
        let entries = configs
            .into_iter()
            .map(|config| WebhookEntry {
                id: uuid::Uuid::new_v4().to_string(),
                config,
            })
            .collect();
        Self {
            inner: Arc::new(RwLock::new(entries)),
        }
    }

    /// Snapshot of all entries (used by the management API to list).
    pub fn list(&self) -> Vec<WebhookEntry> {
        self.read().clone()
    }

    /// Owned clones of every endpoint subscribed to `event`. The read lock is
    /// released as this returns, so callers never hold it across an `.await`.
    fn matching(&self, event: &str) -> Vec<WebhookConfig> {
        self.read()
            .iter()
            .filter(|e| e.config.events.iter().any(|ev| ev == event))
            .map(|e| e.config.clone())
            .collect()
    }

    /// Whether any endpoint currently subscribes to `event`.
    fn any_subscribed(&self, event: &str) -> bool {
        self.read()
            .iter()
            .any(|e| e.config.events.iter().any(|ev| ev == event))
    }

    /// Register a new endpoint, assigning it a fresh id. Takes the write lock.
    pub fn add(&self, config: WebhookConfig) -> WebhookEntry {
        let entry = WebhookEntry {
            id: uuid::Uuid::new_v4().to_string(),
            config,
        };
        self.write().push(entry.clone());
        entry
    }

    /// Remove the entry with `id`. Returns whether one was actually removed.
    pub fn remove(&self, id: &str) -> bool {
        let mut guard = self.write();
        let before = guard.len();
        guard.retain(|e| e.id != id);
        guard.len() != before
    }

    /// Acquire the read guard, recovering from a poisoned lock (a writer that
    /// panicked mid-mutation can only leave the `Vec` intact, so the data is
    /// still safe to read).
    fn read(&self) -> std::sync::RwLockReadGuard<'_, Vec<WebhookEntry>> {
        self.inner.read().unwrap_or_else(|e| e.into_inner())
    }

    /// Acquire the write guard, recovering from a poisoned lock.
    fn write(&self) -> std::sync::RwLockWriteGuard<'_, Vec<WebhookEntry>> {
        self.inner.write().unwrap_or_else(|e| e.into_inner())
    }
}

// ─── WebhookLogger ───────────────────────────────────────────────────────────

/// HTTP webhook log backend.
///
/// Clone is O(1) — only the channel sender and the registry handle are cloned.
#[derive(Clone)]
pub struct WebhookLogger {
    /// Events matching a subscription are forwarded here to the dispatcher.
    tx: UnboundedSender<LogEvent>,
    /// Shared, runtime-mutable endpoint list (also held by the management API).
    registry: WebhookRegistry,
}

impl WebhookLogger {
    /// Create a `WebhookLogger` backed by `registry` and spawn its dispatcher.
    ///
    /// The dispatcher reads the *current* registry on every event, so webhooks
    /// added or removed at runtime (via the management API) take effect
    /// immediately without a restart.
    pub fn new(registry: WebhookRegistry) -> Self {
        // A shared client gives us connection pooling; the request timeout is
        // applied per-request (each endpoint may configure its own).
        let client = reqwest::Client::builder()
            .build()
            .expect("failed to build webhook HTTP client");

        let (tx, rx) = mpsc::unbounded_channel::<LogEvent>();
        tokio::spawn(run_dispatcher(rx, client, registry.clone()));

        Self { tx, registry }
    }
}

// ─── Dispatcher ──────────────────────────────────────────────────────────────

/// Read events off the channel and fan each one out to its subscribed
/// endpoints, spawning an independent delivery task per (event, endpoint).
async fn run_dispatcher(
    mut rx: mpsc::UnboundedReceiver<LogEvent>,
    client: reqwest::Client,
    registry: WebhookRegistry,
) {
    while let Some(event) = rx.recv().await {
        // Build the JSON body once; it is identical for every endpoint.
        let body = match build_payload(&event) {
            Ok(b) => Arc::new(b),
            Err(e) => {
                error!(event = %event.event, "webhook: failed to serialise payload: {}", e);
                continue;
            }
        };

        // One stable id per *logical event*, shared by every endpoint and
        // reused across retries so receivers can deduplicate. It is assigned
        // here — the moment the event is accepted for delivery — rather than
        // inside the per-attempt retry loop, so any redelivery carries the
        // same id and identifies the *same event*, not just the same retry
        // sequence. (For at-least-once across a process restart the id must
        // travel with a persisted copy of the event; that is the durable-sink
        // follow-up. Assigning it at emission time is the prerequisite that
        // makes such a durable sink possible.)
        let delivery_id = Arc::new(uuid::Uuid::new_v4().to_string());

        // Snapshot the currently-subscribed endpoints (the read lock is
        // released before we start spawning delivery tasks).
        for endpoint in registry.matching(&event.event) {
            // Independent task per (event, endpoint) so a slow endpoint never
            // blocks the dispatcher or other endpoints.
            let client = client.clone();
            let body = body.clone();
            let event_name = event.event.clone();
            let delivery_id = delivery_id.clone();
            tokio::spawn(async move {
                deliver(&client, &endpoint, &event_name, &delivery_id, &body).await;
            });
        }
    }
    debug!("webhook dispatcher stopped (channel closed)");
}

// ─── Logger impl ─────────────────────────────────────────────────────────────

#[async_trait]
impl Logger for WebhookLogger {
    async fn log(&self, event: LogEvent) {
        // Cheap early drop: skip events no endpoint subscribes to. This keeps
        // high-frequency events (e.g. `api.request`) off the channel entirely.
        if !self.registry.any_subscribed(&event.event) {
            return;
        }
        // Non-blocking: hand off to the dispatcher and return immediately.
        if self.tx.send(event).is_err() {
            error!("webhook: dispatcher task is gone, dropping event");
        }
    }

    async fn flush(&self) {
        // Deliveries are best-effort, fire-and-forget background tasks; there
        // is no buffer to drain here. Durability is provided by the file
        // backend. In-flight deliveries continue until they finish or the
        // process exits.
    }

    fn name(&self) -> &'static str {
        "webhook"
    }
}

// ─── Payload, signing & delivery ─────────────────────────────────────────────

/// Build the webhook JSON body: `event`, `timestamp`, then every structured
/// field of the event (which already includes `sandbox_id` and, for
/// `sandbox.created`, `template_id`).
fn build_payload(event: &LogEvent) -> Result<Vec<u8>, serde_json::Error> {
    let mut map = serde_json::Map::new();
    map.insert(
        "event".to_string(),
        serde_json::Value::String(event.event.clone()),
    );
    map.insert(
        "timestamp".to_string(),
        serde_json::to_value(event.timestamp)?,
    );
    for (k, v) in &event.fields {
        map.insert(k.clone(), v.clone());
    }
    serde_json::to_vec(&serde_json::Value::Object(map))
}

/// Hex-encode bytes (lowercase). Avoids pulling in an extra dependency.
fn hex_encode(bytes: &[u8]) -> String {
    use std::fmt::Write;
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        let _ = write!(s, "{:02x}", b);
    }
    s
}

/// Compute the `sha256=<hex>` signature of `body` keyed by `secret`.
fn sign(secret: &str, body: &[u8]) -> String {
    let mut mac =
        HmacSha256::new_from_slice(secret.as_bytes()).expect("HMAC accepts keys of any length");
    mac.update(body);
    format!("sha256={}", hex_encode(&mac.finalize().into_bytes()))
}

/// Attempt delivery to a single endpoint with exponential-backoff retries.
///
/// `delivery_id` is the stable, event-level idempotency key assigned by the
/// dispatcher (see [`run_dispatcher`]). It is echoed in `X-Cube-Delivery` on
/// every attempt so a receiver that already processed an earlier attempt can
/// recognise the retry as a duplicate.
async fn deliver(
    client: &reqwest::Client,
    endpoint: &WebhookConfig,
    event_name: &str,
    delivery_id: &str,
    body: &[u8],
) {
    let signature = endpoint.secret.as_deref().map(|s| sign(s, body));
    let timeout = Duration::from_millis(endpoint.timeout_ms);

    // First attempt + `max_retries` retries.
    for attempt in 0..=endpoint.max_retries {
        let mut req = client
            .post(&endpoint.url)
            .timeout(timeout)
            .header(reqwest::header::CONTENT_TYPE, "application/json")
            .header("X-Cube-Event", event_name)
            .header("X-Cube-Delivery", delivery_id)
            .body(body.to_vec());
        if let Some(sig) = &signature {
            req = req.header("X-Cube-Signature", sig);
        }

        match req.send().await {
            Ok(resp) if resp.status().is_success() => {
                debug!(
                    url = %endpoint.url,
                    event = %event_name,
                    delivery = %delivery_id,
                    status = resp.status().as_u16(),
                    attempt,
                    "webhook delivered",
                );
                return;
            }
            Ok(resp) => {
                warn!(
                    url = %endpoint.url,
                    event = %event_name,
                    status = resp.status().as_u16(),
                    attempt,
                    "webhook delivery returned non-success status",
                );
            }
            Err(e) => {
                warn!(
                    url = %endpoint.url,
                    event = %event_name,
                    attempt,
                    "webhook delivery failed: {}",
                    e,
                );
            }
        }

        // Back off before the next retry (skip after the last attempt).
        if attempt < endpoint.max_retries {
            let secs = 2u64.saturating_pow(attempt).min(MAX_BACKOFF_SECS);
            tokio::time::sleep(Duration::from_secs(secs)).await;
        }
    }

    error!(
        url = %endpoint.url,
        event = %event_name,
        delivery = %delivery_id,
        retries = endpoint.max_retries,
        "webhook delivery giving up after exhausting retries",
    );
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::logging::{LogEvent, LogLevel};
    use axum::{extract::State, http::HeaderMap, routing::post, Router};
    use tokio::sync::mpsc::UnboundedReceiver;

    /// A request captured by the test receiver.
    #[derive(Debug)]
    struct Captured {
        signature: Option<String>,
        event_header: Option<String>,
        delivery_header: Option<String>,
        body: serde_json::Value,
    }

    /// Spin up a real HTTP receiver on an ephemeral port. Returns its base URL
    /// and a channel that yields each request it receives.
    async fn spawn_receiver() -> (String, UnboundedReceiver<Captured>) {
        let (tx, rx) = mpsc::unbounded_channel::<Captured>();
        let state = Arc::new(tx);

        async fn handle(
            State(tx): State<Arc<UnboundedSender<Captured>>>,
            headers: HeaderMap,
            body: axum::body::Bytes,
        ) -> &'static str {
            let signature = headers
                .get("X-Cube-Signature")
                .and_then(|v| v.to_str().ok())
                .map(String::from);
            let event_header = headers
                .get("X-Cube-Event")
                .and_then(|v| v.to_str().ok())
                .map(String::from);
            let delivery_header = headers
                .get("X-Cube-Delivery")
                .and_then(|v| v.to_str().ok())
                .map(String::from);
            let body: serde_json::Value = serde_json::from_slice(&body).unwrap();
            let _ = tx.send(Captured {
                signature,
                event_header,
                delivery_header,
                body,
            });
            "ok"
        }

        let app = Router::new()
            .route("/webhook", post(handle))
            .with_state(state);
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });
        (format!("http://{addr}/webhook"), rx)
    }

    fn created_event(sandbox_id: &str) -> LogEvent {
        LogEvent::new(LogLevel::Info, "sandbox.created")
            .field("sandbox_id", sandbox_id)
            .field("template_id", "tmpl-abc")
    }

    /// Build a logger whose registry is seeded with the given endpoints.
    fn logger_for(configs: Vec<WebhookConfig>) -> WebhookLogger {
        WebhookLogger::new(WebhookRegistry::from_configs(configs))
    }

    async fn recv_timeout(rx: &mut UnboundedReceiver<Captured>) -> Option<Captured> {
        tokio::time::timeout(Duration::from_secs(2), rx.recv())
            .await
            .ok()
            .flatten()
    }

    #[tokio::test]
    async fn delivers_subscribed_event_with_correct_payload() {
        let (url, mut rx) = spawn_receiver().await;
        let logger = logger_for(vec![WebhookConfig {
            url,
            events: vec!["sandbox.created".to_string()],
            secret: None,
            timeout_ms: 2000,
            max_retries: 0,
        }]);

        logger.log(created_event("sbx-1")).await;

        let got = recv_timeout(&mut rx).await.expect("should receive webhook");
        assert_eq!(got.event_header.as_deref(), Some("sandbox.created"));
        assert_eq!(got.body["event"], "sandbox.created");
        assert_eq!(got.body["sandbox_id"], "sbx-1");
        assert_eq!(got.body["template_id"], "tmpl-abc");
        assert!(got.body["timestamp"].is_string());
        assert!(got.signature.is_none());
        // Every event carries a stable id for receiver-side dedup.
        let delivery = got.delivery_header.expect("X-Cube-Delivery must be set");
        assert_eq!(
            delivery.len(),
            36,
            "delivery id should be a UUID string, got {delivery:?}"
        );
    }

    #[tokio::test]
    async fn one_event_shares_a_single_delivery_id_across_endpoints() {
        // Two distinct endpoints both subscribed to the same event.
        let (url_a, mut rx_a) = spawn_receiver().await;
        let (url_b, mut rx_b) = spawn_receiver().await;
        let logger = logger_for(vec![
            WebhookConfig {
                url: url_a,
                events: vec!["sandbox.created".to_string()],
                secret: None,
                timeout_ms: 2000,
                max_retries: 0,
            },
            WebhookConfig {
                url: url_b,
                events: vec!["sandbox.created".to_string()],
                secret: None,
                timeout_ms: 2000,
                max_retries: 0,
            },
        ]);

        logger.log(created_event("sbx-dedup")).await;

        let a = recv_timeout(&mut rx_a).await.expect("endpoint A receives");
        let b = recv_timeout(&mut rx_b).await.expect("endpoint B receives");
        let id_a = a.delivery_header.expect("A has X-Cube-Delivery");
        let id_b = b.delivery_header.expect("B has X-Cube-Delivery");
        // The id identifies the *logical event*, so both endpoints see the
        // same value — a receiver registered twice can dedup across them.
        assert_eq!(
            id_a, id_b,
            "the same event must carry one delivery id across all endpoints"
        );
    }

    #[tokio::test]
    async fn signs_payload_when_secret_is_set() {
        let (url, mut rx) = spawn_receiver().await;
        let secret = "top-secret";
        let logger = logger_for(vec![WebhookConfig {
            url,
            events: vec!["sandbox.created".to_string()],
            secret: Some(secret.to_string()),
            timeout_ms: 2000,
            max_retries: 0,
        }]);

        logger.log(created_event("sbx-2")).await;

        let got = recv_timeout(&mut rx).await.expect("should receive webhook");
        let sig = got.signature.expect("signature header must be present");
        // Recompute the signature over the exact received body and compare.
        let body_bytes = serde_json::to_vec(&got.body).unwrap();
        assert_eq!(sig, sign(secret, &body_bytes));
    }

    #[tokio::test]
    async fn does_not_deliver_unsubscribed_event() {
        let (url, mut rx) = spawn_receiver().await;
        let logger = logger_for(vec![WebhookConfig {
            url,
            events: vec!["sandbox.created".to_string()],
            secret: None,
            timeout_ms: 2000,
            max_retries: 0,
        }]);

        // Subscribed only to sandbox.created; deleted must not be delivered.
        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.deleted").field("sandbox_id", "sbx-3"))
            .await;

        assert!(
            recv_timeout(&mut rx).await.is_none(),
            "unsubscribed event should not be delivered",
        );
    }

    #[tokio::test]
    async fn log_is_non_blocking_when_endpoint_is_unreachable() {
        // Port 1 is not listening; delivery will fail and retry in the
        // background, but log() itself must return immediately.
        let logger = logger_for(vec![WebhookConfig {
            url: "http://127.0.0.1:1/webhook".to_string(),
            events: vec!["sandbox.created".to_string()],
            secret: None,
            timeout_ms: 200,
            max_retries: 5,
        }]);

        let start = tokio::time::Instant::now();
        logger.log(created_event("sbx-4")).await;
        assert!(
            start.elapsed() < Duration::from_millis(100),
            "log() must not block on delivery",
        );
    }
}

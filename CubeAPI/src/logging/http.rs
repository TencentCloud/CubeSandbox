// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
//! Webhook event delivery backend — sends log events as JSON POST requests
//! to configured HTTP endpoints.
//!
//! # Features
//!
//! - Multiple target URLs (comma-separated config), all receiving the same events
//! - Event type filtering (comma-separated list or `*` for all)
//! - HMAC-SHA256 payload signing via `X-Cube-Signature-256` header
//! - Exponential backoff retry (3 attempts: 200 ms → 500 ms → 1 s)
//! - Non-blocking delivery via `tokio::spawn` — never blocks the caller
//!
//! # Configuration
//!
//! See [`crate::config::ServerConfig`] fields:
//! - `webhook_urls` — target URLs, comma-separated
//! - `webhook_events` — event type filter, comma-separated (default: `*`)
//! - `webhook_secret` — optional HMAC key for payload signing

use async_trait::async_trait;
use chrono::{DateTime, Utc};
use hmac::{Hmac, Mac};
use serde::Serialize;
use sha2::Sha256;
use std::collections::HashMap;
use std::time::Duration;

use super::{LogEvent, Logger};

/// HMAC-SHA256 type alias.
type HmacSha256 = Hmac<Sha256>;

/// Retry delays (exponential backoff): 200 ms → 500 ms → 1 s.
const RETRY_DELAYS: [Duration; 3] = [
    Duration::from_millis(200),
    Duration::from_millis(500),
    Duration::from_secs(1),
];

/// Maximum retry attempts.
const MAX_RETRIES: usize = RETRY_DELAYS.len();

// ─── Payload ────────────────────────────────────────────────────────────

/// JSON payload sent to each webhook endpoint.
#[derive(Debug, Serialize)]
pub struct WebhookPayload {
    /// Event type, e.g. `"sandbox.created"`.
    pub event: String,
    /// ISO 8601 timestamp of when the event was generated.
    pub timestamp: DateTime<Utc>,
    /// Sandbox identifier, if applicable.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub sandbox_id: Option<String>,
    /// Template identifier, if applicable.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub template_id: Option<String>,
    /// Additional context fields flattened into the payload.
    #[serde(flatten)]
    pub fields: HashMap<String, serde_json::Value>,
}

// ─── Logger ─────────────────────────────────────────────────────────────

/// Webhook log backend — delivers events to HTTP endpoints asynchronously.
///
/// All configured URLs receive the same events, filtered by the event-type
/// list.  Creation is O(1) and the struct is cheap to clone.
#[derive(Clone)]
pub struct HttpLogger {
    client: reqwest::Client,
    urls: Vec<String>,
    /// Parsed event filter. Empty or contains `"*"` = match all.
    events: Vec<String>,
    /// Optional HMAC-SHA256 key (raw bytes).
    secret: Option<Vec<u8>>,
}

impl HttpLogger {
    /// Create a new `HttpLogger` from config fields.
    ///
    /// Returns `None` when no URLs are configured (no-op, nothing to deliver).
    pub fn new(urls: &str, events: &str, secret: Option<&str>) -> Option<Self> {
        let url_list: Vec<String> = urls
            .split(',')
            .map(str::trim)
            .filter(|s| !s.is_empty())
            .map(String::from)
            .collect();
        if url_list.is_empty() {
            return None;
        }

        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(10))
            .build()
            .expect("HttpLogger HTTP client should build");

        let event_list: Vec<String> = events
            .split(',')
            .map(str::trim)
            .filter(|s| !s.is_empty())
            .map(String::from)
            .collect();

        let key = secret.and_then(|s| {
            let t = s.trim();
            if t.is_empty() {
                None
            } else {
                Some(t.as_bytes().to_vec())
            }
        });

        Some(Self {
            client,
            urls: url_list,
            events: event_list,
            secret: key,
        })
    }

    /// Check whether an event name should trigger webhook delivery.
    fn matches(&self, event: &str) -> bool {
        self.events.is_empty()
            || self.events.iter().any(|e| e == "*" || e == event)
    }

    /// Build a `WebhookPayload` from a `LogEvent`.
    fn build_payload(&self, event: &LogEvent) -> WebhookPayload {
        let sandbox_id = event
            .fields
            .get("sandbox_id")
            .and_then(|v| v.as_str())
            .map(String::from);

        let template_id = event
            .fields
            .get("template_id")
            .and_then(|v| v.as_str())
            .map(String::from);

        WebhookPayload {
            event: event.event.clone(),
            timestamp: event.timestamp,
            sandbox_id,
            template_id,
            fields: event.fields.clone(),
        }
    }

    /// Compute HMAC-SHA256 signature of the raw JSON body.
    fn sign(&self, body: &[u8]) -> Option<String> {
        let key = self.secret.as_ref()?;
        let mut mac = HmacSha256::new_from_slice(key).ok()?;
        mac.update(body);
        let result = mac.finalize();
        let code = result.into_bytes();
        Some(hex::encode(code))
    }

    /// Deliver a payload to a single URL with exponential-backoff retry.
    async fn deliver(
        client: reqwest::Client,
        url: String,
        body: Vec<u8>,
        signature: Option<String>,
        event: String,
    ) {
        let mut last_error = String::new();

        for attempt in 0..MAX_RETRIES {
            let mut req = client
                .post(&url)
                .header("Content-Type", "application/json");

            if let Some(ref sig) = signature {
                req = req.header("X-Cube-Signature-256", format!("sha256={}", sig));
            }

            match req.body(body.clone()).send().await {
                Ok(resp) if resp.status().is_success() => {
                    tracing::debug!(
                        url = %url,
                        event = %event,
                        "webhook: delivered successfully"
                    );
                    return;
                }
                Ok(resp) => {
                    last_error = format!("HTTP {}", resp.status());
                    tracing::warn!(
                        url = %url,
                        event = %event,
                        attempt = attempt + 1,
                        status = %resp.status(),
                        "webhook: delivery failed, will retry"
                    );
                }
                Err(e) => {
                    last_error = e.to_string();
                    tracing::warn!(
                        url = %url,
                        event = %event,
                        attempt = attempt + 1,
                        error = %e,
                        "webhook: request failed, will retry"
                    );
                }
            }

            if attempt < MAX_RETRIES - 1 {
                tokio::time::sleep(RETRY_DELAYS[attempt]).await;
            }
        }

        tracing::error!(
            url = %url,
            event = %event,
            error = %last_error,
            "webhook: all retry attempts exhausted, giving up"
        );
    }
}

#[async_trait]
impl Logger for HttpLogger {
    async fn log(&self, event: LogEvent) {
        let event_name = event.event.clone();

        if !self.matches(&event_name) {
            return;
        }

        let payload = self.build_payload(&event);
        let body = match serde_json::to_vec(&payload) {
            Ok(b) => b,
            Err(e) => {
                tracing::error!(
                    error = %e,
                    event = %event_name,
                    "webhook: failed to serialize payload"
                );
                return;
            }
        };

        let signature = self.sign(&body);

        for url in &self.urls {
            let client = self.client.clone();
            let url = url.clone();
            let body = body.clone();
            let sig = signature.clone();
            let ev = event_name.clone();

            tokio::spawn(async move {
                Self::deliver(client, url, body, sig, ev).await;
            });
        }
    }

    async fn flush(&self) {
        // Fire-and-forget delivery; no buffering to flush.
    }

    fn name(&self) -> &'static str {
        "http"
    }
}

// ═══════════════════════════════════════════════════════════════════════════
// Tests
// ═══════════════════════════════════════════════════════════════════════════

#[cfg(test)]
mod tests {
    use super::*;
    use crate::logging::LogLevel;
    use axum::http::StatusCode;
    use axum::routing::post;
    use axum::Router;
    use chrono::TimeZone;
    use serde_json::json;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::{Arc, Mutex};

    // ── Helpers ──────────────────────────────────────────────────────────

    /// Build a minimal HttpLogger with a single target URL.
    fn logger_with(url: &str, events: Vec<&str>, secret: Option<&str>) -> HttpLogger {
        HttpLogger::new(url, &events.join(","), secret).unwrap()
    }

    /// Start a mock HTTP server that captures request info, then returns
    /// the given status code.  Returns (URL, request_counter, received_bodies).
    async fn mock_server(
        status: StatusCode,
    ) -> (String, Arc<AtomicUsize>, Arc<Mutex<Vec<(Vec<u8>, Vec<(String, String)>)>>>) {
        let counter = Arc::new(AtomicUsize::new(0));
        let received = Arc::new(Mutex::new(Vec::new()));
        let c = counter.clone();
        let r = received.clone();

        let app = Router::new().route(
            "/webhook",
            post(move |headers: axum::http::HeaderMap, body: axum::body::Bytes| async move {
                c.fetch_add(1, Ordering::SeqCst);
                let headers: Vec<(String, String)> = headers
                    .iter()
                    .map(|(k, v)| (k.to_string(), v.to_str().unwrap_or("").to_string()))
                    .collect();
                r.lock().unwrap().push((body.to_vec(), headers));
                StatusCode::OK
            }),
        );

        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let url = format!("http://{addr}/webhook");

        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });

        // Give the server a moment to start
        tokio::time::sleep(std::time::Duration::from_millis(50)).await;

        (url, counter, received)
    }

    // ── Payload serialization ────────────────────────────────────────────

    #[test]
    fn payload_serializes_basic_fields() {
        let ts = Utc.with_ymd_and_hms(2026, 7, 24, 12, 0, 0).unwrap();
        let payload = WebhookPayload {
            event: "sandbox.created".into(),
            timestamp: ts,
            sandbox_id: Some("sb-123".into()),
            template_id: Some("tpl-python".into()),
            fields: HashMap::new(),
        };

        let json = serde_json::to_value(&payload).unwrap();
        assert_eq!(json["event"], "sandbox.created");
        assert_eq!(json["sandbox_id"], "sb-123");
        assert_eq!(json["template_id"], "tpl-python");
        assert!(json.get("timestamp").is_some());
        // "fields" is flattened and should be absent when empty
        assert!(json.get("fields").is_none());
    }

    #[test]
    fn payload_omits_optional_fields_when_none() {
        let ts = Utc.with_ymd_and_hms(2026, 7, 24, 12, 0, 0).unwrap();
        let payload = WebhookPayload {
            event: "api.response".into(),
            timestamp: ts,
            sandbox_id: None,
            template_id: None,
            fields: HashMap::new(),
        };

        let json = serde_json::to_value(&payload).unwrap();
        assert_eq!(json["event"], "api.response");
        assert!(json.get("sandbox_id").is_none());
        assert!(json.get("template_id").is_none());
    }

    #[test]
    fn payload_fields_are_flattened() {
        let ts = Utc.with_ymd_and_hms(2026, 7, 24, 12, 0, 0).unwrap();
        let mut fields = HashMap::new();
        fields.insert("timeout".into(), json!(3600));
        let payload = WebhookPayload {
            event: "sandbox.timeout.updated".into(),
            timestamp: ts,
            sandbox_id: Some("sb-123".into()),
            template_id: None,
            fields,
        };

        let json = serde_json::to_value(&payload).unwrap();
        assert_eq!(json["timeout"], 3600);
    }

    // ── Signature computation ────────────────────────────────────────────

    #[test]
    fn sign_returns_none_without_secret() {
        let logger = HttpLogger::new("http://localhost:9999/webhook", "*", None).unwrap();
        assert!(logger.sign(b"hello").is_none());
    }

    #[test]
    fn sign_returns_expected_hex() {
        let logger =
            HttpLogger::new("http://localhost:9999/webhook", "*", Some("secret")).unwrap();
        let sig = logger.sign(b"hello").unwrap();
        // HMAC-SHA256("secret", "hello") hex
        let expected = {
            use hmac::Mac;
            let mut mac =
                HmacSha256::new_from_slice(b"secret").unwrap();
            mac.update(b"hello");
            hex::encode(mac.finalize().into_bytes())
        };
        assert_eq!(sig, expected);
        // Verify it's lowercase hex
        assert!(sig.chars().all(|c| c.is_ascii_hexdigit()));
        assert_eq!(sig, sig.to_lowercase());
    }

    #[test]
    fn sign_produces_different_output_for_different_body() {
        let logger =
            HttpLogger::new("http://localhost:9999/webhook", "*", Some("secret")).unwrap();
        let sig1 = logger.sign(b"body-a").unwrap();
        let sig2 = logger.sign(b"body-b").unwrap();
        assert_ne!(sig1, sig2);
    }

    #[test]
    fn sign_produces_different_output_for_different_secret() {
        let logger1 =
            HttpLogger::new("http://localhost:9999/webhook", "*", Some("key-a")).unwrap();
        let logger2 =
            HttpLogger::new("http://localhost:9999/webhook", "*", Some("key-b")).unwrap();
        let sig1 = logger1.sign(b"same-body").unwrap();
        let sig2 = logger2.sign(b"same-body").unwrap();
        assert_ne!(sig1, sig2);
    }

    // ── Event name matching ──────────────────────────────────────────────

    #[test]
    fn matches_accepts_all_with_wildcard() {
        let logger = logger_with("http://localhost:1/w", vec!["*"], None);
        assert!(logger.matches("sandbox.created"));
        assert!(logger.matches("sandbox.deleted"));
        assert!(logger.matches("api.response"));
        assert!(logger.matches("anything.else"));
    }

    #[test]
    fn matches_filters_by_exact_event_name() {
        let logger = logger_with("http://localhost:1/w", vec!["sandbox.created"], None);
        assert!(logger.matches("sandbox.created"));
        assert!(!logger.matches("sandbox.deleted"));
        assert!(!logger.matches("api.response"));
    }

    #[test]
    fn matches_supports_multiple_events() {
        let logger =
            logger_with("http://localhost:1/w", vec!["sandbox.created", "sandbox.deleted"], None);
        assert!(logger.matches("sandbox.created"));
        assert!(logger.matches("sandbox.deleted"));
        assert!(!logger.matches("sandbox.paused"));
    }

    #[test]
    fn matches_accepts_all_when_events_list_empty() {
        // When no events are configured, the Vec is empty and `matches` returns true
        // (empty means "no filter").
        let logger = HttpLogger {
            client: reqwest::Client::new(),
            urls: vec!["http://localhost:1/w".into()],
            events: vec![],
            secret: None,
        };
        assert!(logger.matches("sandbox.created"));
        assert!(logger.matches("anything"));
    }

    // ── Build payload from LogEvent ──────────────────────────────────────

    #[test]
    fn build_payload_maps_log_event_fields() {
        let logger = logger_with("http://localhost:1/w", vec!["*"], None);

        let mut fields = HashMap::new();
        fields.insert("sandbox_id".into(), json!("sb-456"));
        fields.insert("template_id".into(), json!("tpl-go"));
        fields.insert("extra".into(), json!(42));

        let event = LogEvent {
            timestamp: Utc.with_ymd_and_hms(2026, 7, 24, 12, 0, 0).unwrap(),
            level: LogLevel::Info,
            event: "sandbox.created".into(),
            fields,
        };

        let payload = logger.build_payload(&event);
        assert_eq!(payload.event, "sandbox.created");
        assert_eq!(payload.sandbox_id.as_deref(), Some("sb-456"));
        assert_eq!(payload.template_id.as_deref(), Some("tpl-go"));
        assert_eq!(payload.fields.get("extra").and_then(|v| v.as_i64()), Some(42));
    }

    // ── HTTP delivery (with mock server) ─────────────────────────────────

    #[tokio::test]
    async fn deliver_sends_event_to_mock_server() {
        let (url, _counter, received) = mock_server(StatusCode::OK).await;

        let logger = logger_with(&url, vec!["*"], None);
        let event = LogEvent::new(LogLevel::Info, "sandbox.created")
            .field("sandbox_id", "sb-777");

        logger.log(event).await;

        // Allow async delivery to complete
        tokio::time::sleep(std::time::Duration::from_millis(200)).await;

        let received = received.lock().unwrap();
        assert_eq!(received.len(), 1, "should receive exactly one request");

        let (body, headers) = &received[0];
        let payload: serde_json::Value = serde_json::from_slice(body).unwrap();
        assert_eq!(payload["event"], "sandbox.created");
        assert_eq!(payload["sandbox_id"], "sb-777");

        // Content-Type should be JSON
        let has_json_ct = headers.iter().any(|(k, v)| {
            k == "content-type" && v.starts_with("application/json")
        });
        assert!(has_json_ct, "should have application/json content-type");
    }

    #[tokio::test]
    async fn deliver_includes_signature_header_when_secret_is_set() {
        let (url, _counter, received) = mock_server(StatusCode::OK).await;

        let logger = logger_with(&url, vec!["*"], Some("shared-secret"));
        let event = LogEvent::new(LogLevel::Info, "sandbox.created")
            .field("sandbox_id", "sb-888");

        logger.log(event).await;
        tokio::time::sleep(std::time::Duration::from_millis(200)).await;

        let received = received.lock().unwrap();
        assert_eq!(received.len(), 1);

        let (_body, headers) = &received[0];
        let sig_header = headers.iter().find(|(k, _)| k == "x-cube-signature-256");
        assert!(sig_header.is_some(), "should include signature header");
        let (_, sig_value) = sig_header.unwrap();
        assert!(sig_value.starts_with("sha256="), "signature should start with sha256=");
        assert!(sig_value.len() > 50, "signature should be a hex hash");
    }

    #[tokio::test]
    async fn deliver_retries_on_http_500() {
        let counter = Arc::new(AtomicUsize::new(0));
        let c = counter.clone();

        let app = Router::new().route(
            "/webhook",
            post(move || async move {
                c.fetch_add(1, Ordering::SeqCst);
                StatusCode::INTERNAL_SERVER_ERROR
            }),
        );

        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let url = format!("http://{addr}/webhook");

        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });
        tokio::time::sleep(std::time::Duration::from_millis(50)).await;

        let logger = logger_with(&url, vec!["*"], None);
        let event = LogEvent::new(LogLevel::Info, "test.event");

        // Deliver synchronously via the internal method
        let body = serde_json::to_vec(&logger.build_payload(&event)).unwrap();
        let sig = logger.sign(&body);

        // Use deliver directly (it's pub-in-module, accessible from tests)
        HttpLogger::deliver(
            reqwest::Client::new(),
            url.clone(),
            body,
            sig,
            "test.event".into(),
        )
        .await;

        // Should have been called MAX_RETRIES times (3)
        assert_eq!(counter.load(Ordering::SeqCst), MAX_RETRIES);
    }

    #[tokio::test]
    async fn deliver_succeeds_on_first_attempt() {
        let counter = Arc::new(AtomicUsize::new(0));
        let c = counter.clone();

        let app = Router::new().route(
            "/webhook",
            post(move || async move {
                c.fetch_add(1, Ordering::SeqCst);
                StatusCode::OK
            }),
        );

        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let url = format!("http://{addr}/webhook");

        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });
        tokio::time::sleep(std::time::Duration::from_millis(50)).await;

        let logger = logger_with(&url, vec!["*"], None);
        let event = LogEvent::new(LogLevel::Info, "test.event");

        let body = serde_json::to_vec(&logger.build_payload(&event)).unwrap();
        let sig = logger.sign(&body);

        HttpLogger::deliver(
            reqwest::Client::new(),
            url.clone(),
            body,
            sig,
            "test.event".into(),
        )
        .await;

        // Should succeed on first try, no retries
        assert_eq!(counter.load(Ordering::SeqCst), 1);
    }

    // ── Logger trait integration ─────────────────────────────────────────

    #[tokio::test]
    async fn log_respects_event_filter() {
        let (url, counter, _received) = mock_server(StatusCode::OK).await;

        // Only accept sandbox.created
        let logger = logger_with(&url, vec!["sandbox.created"], None);

        // This event should be filtered out
        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.deleted"))
            .await;
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        assert_eq!(counter.load(Ordering::SeqCst), 0, "filtered event should not be delivered");

        // This event should go through
        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created"))
            .await;
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        assert_eq!(counter.load(Ordering::SeqCst), 1, "matching event should be delivered");
    }

    #[tokio::test]
    async fn log_sends_to_all_urls() {
        let (url1, _c1, r1) = mock_server(StatusCode::OK).await;
        let (url2, _c2, r2) = mock_server(StatusCode::OK).await;

        // Create a logger that targets both URLs
        let combined = format!("{url1},{url2}");
        let logger = HttpLogger::new(&combined, "*", None).unwrap();

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created"))
            .await;
        tokio::time::sleep(std::time::Duration::from_millis(300)).await;

        assert_eq!(r1.lock().unwrap().len(), 1, "first URL should receive the event");
        assert_eq!(r2.lock().unwrap().len(), 1, "second URL should receive the event");
    }

    // ── Constructor ──────────────────────────────────────────────────────

    #[test]
    fn new_returns_none_when_urls_empty() {
        assert!(HttpLogger::new("", "*", None).is_none());
        assert!(HttpLogger::new(",,,", "*", None).is_none());
        assert!(HttpLogger::new("  ", "*", None).is_none());
    }

    #[test]
    fn new_returns_some_when_urls_provided() {
        let logger = HttpLogger::new("http://localhost:9999/w", "*", None);
        assert!(logger.is_some());
        assert_eq!(logger.unwrap().urls.len(), 1);
    }

    #[test]
    fn new_parses_multiple_urls() {
        let logger =
            HttpLogger::new("http://a.com/w,http://b.com/w", "*", None).unwrap();
        assert_eq!(logger.urls.len(), 2);
        assert_eq!(logger.urls[0], "http://a.com/w");
        assert_eq!(logger.urls[1], "http://b.com/w");
    }

    #[test]
    fn new_trims_whitespace_around_urls() {
        let logger =
            HttpLogger::new("  http://a.com/w , http://b.com/w  ", "*", None).unwrap();
        assert_eq!(logger.urls.len(), 2);
        assert_eq!(logger.urls[0], "http://a.com/w");
    }
}
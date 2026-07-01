// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! HTTP webhook log backend.
//!
//! Sends structured log events as JSON POST requests to configurable webhook
//! endpoints. Delivery runs in a background Tokio task so `log()` only filters
//! and enqueues events; API handlers are not blocked by slow receivers.
//!
//! Events are buffered and POSTed when either:
//! - The buffer reaches `batch_size` (default 100), or
//! - A ticker fires every `flush_interval_secs` (default 5 s), or
//! - `flush()` is called.

use super::{LogEvent, Logger};
use async_trait::async_trait;
use hmac::{Hmac, Mac};
use serde::{Deserialize, Serialize};
use sha2::Sha256;
use std::{sync::Arc, time::Duration};
use tokio::{
    sync::{mpsc, oneshot},
    time::MissedTickBehavior,
};
use tracing::{error, warn};

type HmacSha256 = Hmac<Sha256>;

const DEFAULT_EVENT_PATTERN: &str = "sandbox.*";
const SIGNATURE_HEADER: &str = "X-Cube-Webhook-Signature";
const EVENT_HEADER: &str = "X-Cube-Webhook-Event";
const USER_AGENT: &str = "cube-api-webhook/0.1";

// ─── Configuration ──────────────────────────────────────────────────────────

/// One webhook endpoint and its subscription settings.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct WebhookEndpointConfig {
    /// Full URL to POST batches to, e.g. `"http://receiver.internal/webhook"`.
    pub url: String,
    /// Event patterns to subscribe to. Empty means `sandbox.*`.
    #[serde(default)]
    pub events: Vec<String>,
    /// Optional HMAC-SHA256 secret used to sign the raw request body.
    #[serde(default)]
    pub hmac_secret: Option<String>,
}

/// Configuration for the HTTP webhook backend.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct HttpLoggerConfig {
    /// Configured webhook endpoints.
    #[serde(default)]
    pub endpoints: Vec<WebhookEndpointConfig>,
    /// Max events per batch (default: 100).
    #[serde(default = "default_batch_size")]
    pub batch_size: usize,
    /// Flush interval in seconds even if batch is not full (default: 5).
    #[serde(default = "default_flush_interval_secs")]
    pub flush_interval_secs: u64,
    /// Number of retries after the initial delivery attempt (default: 3).
    #[serde(default = "default_max_retries")]
    pub max_retries: usize,
    /// Delay between retry attempts in milliseconds (default: 200).
    #[serde(default = "default_retry_backoff_millis")]
    pub retry_backoff_millis: u64,
    /// HTTP request timeout in seconds (default: 5).
    #[serde(default = "default_request_timeout_secs")]
    pub request_timeout_secs: u64,
}

fn default_batch_size() -> usize {
    100
}
fn default_flush_interval_secs() -> u64 {
    5
}
fn default_max_retries() -> usize {
    3
}
fn default_retry_backoff_millis() -> u64 {
    200
}
fn default_request_timeout_secs() -> u64 {
    5
}

impl Default for HttpLoggerConfig {
    fn default() -> Self {
        Self {
            endpoints: Vec::new(),
            batch_size: default_batch_size(),
            flush_interval_secs: default_flush_interval_secs(),
            max_retries: default_max_retries(),
            retry_backoff_millis: default_retry_backoff_millis(),
            request_timeout_secs: default_request_timeout_secs(),
        }
    }
}

// ─── Runtime messages ───────────────────────────────────────────────────────

enum Msg {
    Event(LogEvent),
    Flush(oneshot::Sender<()>),
}

/// HTTP webhook log backend.
///
/// Clone is O(1): only the channel sender and endpoint config handle are cloned.
#[derive(Clone)]
pub struct HttpLogger {
    tx: mpsc::UnboundedSender<Msg>,
    endpoints: Arc<Vec<WebhookEndpointConfig>>,
}

impl HttpLogger {
    /// Create an HTTP logger and spawn its background delivery task.
    pub fn new(config: HttpLoggerConfig) -> Self {
        let endpoints = Arc::new(config.endpoints.clone());
        let (tx, rx) = mpsc::unbounded_channel::<Msg>();

        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(config.request_timeout_secs.max(1)))
            .user_agent(USER_AGENT)
            .build()
            .unwrap_or_else(|e| {
                warn!(
                    "HttpLogger: failed to build reqwest client: {}; using default",
                    e
                );
                reqwest::Client::new()
            });

        tokio::spawn(run_delivery_loop(client, config, endpoints.clone(), rx));

        Self { tx, endpoints }
    }
}

#[async_trait]
impl Logger for HttpLogger {
    async fn log(&self, event: LogEvent) {
        if !self
            .endpoints
            .iter()
            .any(|endpoint| endpoint_matches(endpoint, &event.event))
        {
            return;
        }

        if self.tx.send(Msg::Event(event)).is_err() {
            error!("HttpLogger: delivery task is gone, dropping event");
        }
    }

    async fn flush(&self) {
        let (tx, rx) = oneshot::channel();
        if self.tx.send(Msg::Flush(tx)).is_ok() {
            let _ = tokio::time::timeout(Duration::from_secs(30), rx).await;
        }
    }

    fn name(&self) -> &'static str {
        "http"
    }
}

// ─── Delivery loop ──────────────────────────────────────────────────────────

async fn run_delivery_loop(
    client: reqwest::Client,
    config: HttpLoggerConfig,
    endpoints: Arc<Vec<WebhookEndpointConfig>>,
    mut rx: mpsc::UnboundedReceiver<Msg>,
) {
    let batch_size = config.batch_size.max(1);
    let mut buffer = Vec::with_capacity(batch_size);
    let mut ticker = tokio::time::interval(Duration::from_secs(config.flush_interval_secs.max(1)));
    ticker.set_missed_tick_behavior(MissedTickBehavior::Delay);

    loop {
        tokio::select! {
            Some(msg) = rx.recv() => {
                match msg {
                    Msg::Event(event) => {
                        buffer.push(event);
                        if buffer.len() >= batch_size {
                            flush_buffer(&client, &config, endpoints.as_ref(), &mut buffer).await;
                        }
                    }
                    Msg::Flush(reply) => {
                        flush_buffer(&client, &config, endpoints.as_ref(), &mut buffer).await;
                        let _ = reply.send(());
                    }
                }
            }
            _ = ticker.tick() => {
                flush_buffer(&client, &config, endpoints.as_ref(), &mut buffer).await;
            }
            else => break,
        }
    }

    flush_buffer(&client, &config, endpoints.as_ref(), &mut buffer).await;
}

async fn flush_buffer(
    client: &reqwest::Client,
    config: &HttpLoggerConfig,
    endpoints: &[WebhookEndpointConfig],
    buffer: &mut Vec<LogEvent>,
) {
    if buffer.is_empty() || endpoints.is_empty() {
        buffer.clear();
        return;
    }

    let events = std::mem::take(buffer);
    for endpoint in endpoints {
        let matching_events: Vec<LogEvent> = events
            .iter()
            .filter(|event| endpoint_matches(endpoint, &event.event))
            .cloned()
            .collect();

        if matching_events.is_empty() {
            continue;
        }

        match serde_json::to_vec(&WebhookPayload {
            events: matching_events,
        }) {
            Ok(body) => send_with_retries(client, config, endpoint, body).await,
            Err(e) => error!(
                url = %endpoint.url,
                "HttpLogger: failed to serialise webhook payload: {}",
                e
            ),
        }
    }
}

#[derive(Serialize)]
struct WebhookPayload {
    events: Vec<LogEvent>,
}

async fn send_with_retries(
    client: &reqwest::Client,
    config: &HttpLoggerConfig,
    endpoint: &WebhookEndpointConfig,
    body: Vec<u8>,
) {
    let max_attempts = config.max_retries.saturating_add(1);

    let signature = endpoint
        .hmac_secret
        .as_deref()
        .map(|secret| sign_body(secret, &body));

    for attempt in 0..max_attempts {
        let mut request = client
            .post(&endpoint.url)
            .header(reqwest::header::CONTENT_TYPE, "application/json")
            .header(EVENT_HEADER, "batch")
            .body(body.clone());

        if let Some(sig) = &signature {
            request = request.header(SIGNATURE_HEADER, sig.clone());
        }

        match request.send().await {
            Ok(response) if response.status().is_success() => return,
            Ok(response) => warn!(
                url = %endpoint.url,
                status = %response.status(),
                attempt = attempt + 1,
                max_attempts,
                "HttpLogger: webhook endpoint returned non-success status"
            ),
            Err(e) => warn!(
                url = %endpoint.url,
                attempt = attempt + 1,
                max_attempts,
                "HttpLogger: webhook delivery error: {}",
                e
            ),
        }

        if attempt + 1 < max_attempts {
            tokio::time::sleep(Duration::from_millis(retry_delay_millis(
                config.retry_backoff_millis,
                attempt,
            )))
            .await;
        }
    }

    error!(
        url = %endpoint.url,
        attempts = max_attempts,
        "HttpLogger: webhook delivery failed after all attempts"
    );
}

// ─── Matching and signing helpers ───────────────────────────────────────────

fn retry_delay_millis(base_millis: u64, retry_index: usize) -> u64 {
    let factor = 1u64
        .checked_shl(retry_index.min(63) as u32)
        .unwrap_or(u64::MAX);
    base_millis.saturating_mul(factor)
}

fn endpoint_matches(endpoint: &WebhookEndpointConfig, event: &str) -> bool {
    event_matches(&endpoint.events, event)
}

fn event_matches(patterns: &[String], event: &str) -> bool {
    if patterns.is_empty() {
        return event_matches_pattern(DEFAULT_EVENT_PATTERN, event);
    }

    patterns
        .iter()
        .any(|pattern| event_matches_pattern(pattern.as_str(), event))
}

fn event_matches_pattern(pattern: &str, event: &str) -> bool {
    if pattern == "*" {
        return true;
    }

    if let Some(prefix) = pattern.strip_suffix(".*") {
        return event.starts_with(prefix) && event.as_bytes().get(prefix.len()) == Some(&b'.');
    }

    pattern == event
}

fn sign_body(secret: &str, body: &[u8]) -> String {
    let mut mac = HmacSha256::new_from_slice(secret.as_bytes())
        .expect("HMAC accepts keys of any size for SHA-256");
    mac.update(body);
    format!("sha256={}", hex::encode(mac.finalize().into_bytes()))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::logging::LogLevel;
    use axum::{
        body::Bytes,
        extract::State,
        http::{HeaderMap, StatusCode},
        routing::post,
        Router,
    };
    use serde_json::Value;
    use std::sync::{
        atomic::{AtomicUsize, Ordering},
        Arc,
    };
    use tokio::sync::Mutex;

    #[test]
    fn event_patterns_match_expected_events() {
        assert!(event_matches(
            &["sandbox.created".into()],
            "sandbox.created"
        ));
        assert!(!event_matches(
            &["sandbox.created".into()],
            "sandbox.deleted"
        ));
        assert!(event_matches(&["sandbox.*".into()], "sandbox.paused"));
        assert!(!event_matches(&["sandbox.*".into()], "api.request"));
        assert!(event_matches(&["*".into()], "api.request"));
        assert!(event_matches(&[], "sandbox.created"));
        assert!(!event_matches(&[], "api.request"));
    }

    #[test]
    fn hmac_signature_is_stable_and_body_bound() {
        let body = br#"{"events":[]}"#;
        let signature = sign_body("secret", body);

        assert_eq!(signature, sign_body("secret", body));
        assert_ne!(signature, sign_body("secret", br#"{"events":[1]}"#));
        assert_ne!(signature, sign_body("other-secret", body));
        assert!(signature.starts_with("sha256="));
    }

    #[test]
    fn retry_delay_uses_exponential_backoff() {
        assert_eq!(retry_delay_millis(200, 0), 200);
        assert_eq!(retry_delay_millis(200, 1), 400);
        assert_eq!(retry_delay_millis(200, 2), 800);
        assert_eq!(retry_delay_millis(0, 3), 0);
        assert_eq!(retry_delay_millis(u64::MAX, 1), u64::MAX);
    }

    #[tokio::test]
    async fn flush_posts_matching_event() {
        let server = TestServer::start(0).await;
        let logger = HttpLogger::new(test_config(vec![endpoint(
            &server.url(),
            &["sandbox.created"],
        )]));

        logger.log(sandbox_event("sandbox.created")).await;
        logger.flush().await;

        let requests = server.requests().await;
        assert_eq!(requests.len(), 1);
        assert_eq!(requests[0].header(EVENT_HEADER), Some("batch"));
        let payload = requests[0].json();
        assert_eq!(payload["events"].as_array().unwrap().len(), 1);
        assert_eq!(payload["events"][0]["event"], "sandbox.created");
        assert_eq!(payload["events"][0]["sandbox_id"], "sbx-1");
    }

    #[tokio::test]
    async fn log_filters_unmatched_events_before_enqueueing() {
        let server = TestServer::start(0).await;
        let logger = HttpLogger::new(test_config(vec![endpoint(&server.url(), &["sandbox.*"])]));

        logger.log(api_request_event()).await;
        logger.flush().await;

        assert!(server.requests().await.is_empty());
    }

    #[tokio::test]
    async fn empty_endpoint_events_default_to_sandbox_events_only() {
        let server = TestServer::start(0).await;
        let logger = HttpLogger::new(test_config(vec![WebhookEndpointConfig {
            url: server.url(),
            events: Vec::new(),
            hmac_secret: None,
        }]));

        logger.log(api_request_event()).await;
        logger.log(sandbox_event("sandbox.deleted")).await;
        logger.flush().await;

        let requests = server.requests().await;
        assert_eq!(requests.len(), 1);
        let payload = requests[0].json();
        assert_eq!(payload["events"].as_array().unwrap().len(), 1);
        assert_eq!(payload["events"][0]["event"], "sandbox.deleted");
    }

    #[tokio::test]
    async fn batch_size_triggers_delivery() {
        let server = TestServer::start(0).await;
        let mut config = test_config(vec![endpoint(&server.url(), &["sandbox.*"])]);
        config.batch_size = 2;
        config.flush_interval_secs = 60;
        let logger = HttpLogger::new(config);

        logger.log(sandbox_event("sandbox.created")).await;
        assert!(server.requests().await.is_empty());

        logger.log(sandbox_event("sandbox.paused")).await;
        wait_for_requests(&server, 1).await;

        let requests = server.requests().await;
        assert_eq!(requests.len(), 1);
        let payload = requests[0].json();
        assert_eq!(payload["events"].as_array().unwrap().len(), 2);
    }

    #[tokio::test]
    async fn endpoints_receive_only_their_subscribed_events() {
        let created_server = TestServer::start(0).await;
        let deleted_server = TestServer::start(0).await;
        let wildcard_server = TestServer::start(0).await;
        let logger = HttpLogger::new(test_config(vec![
            endpoint(&created_server.url(), &["sandbox.created"]),
            endpoint(&deleted_server.url(), &["sandbox.deleted"]),
            endpoint(&wildcard_server.url(), &["sandbox.*"]),
        ]));

        logger.log(sandbox_event("sandbox.created")).await;
        logger.flush().await;

        assert_eq!(created_server.requests().await.len(), 1);
        assert!(deleted_server.requests().await.is_empty());
        assert_eq!(wildcard_server.requests().await.len(), 1);
    }

    #[tokio::test]
    async fn hmac_signature_header_matches_request_body() {
        let server = TestServer::start(0).await;
        let logger = HttpLogger::new(test_config(vec![WebhookEndpointConfig {
            url: server.url(),
            events: vec!["sandbox.created".into()],
            hmac_secret: Some("top-secret".into()),
        }]));

        logger.log(sandbox_event("sandbox.created")).await;
        logger.flush().await;

        let requests = server.requests().await;
        assert_eq!(requests.len(), 1);
        assert_eq!(
            requests[0].header(SIGNATURE_HEADER),
            Some(sign_body("top-secret", &requests[0].body).as_str())
        );
    }

    #[tokio::test]
    async fn retries_non_success_status_until_success() {
        let server = TestServer::start(1).await;
        let mut config = test_config(vec![endpoint(&server.url(), &["sandbox.created"])]);
        config.max_retries = 2;
        config.retry_backoff_millis = 1;
        let logger = HttpLogger::new(config);

        logger.log(sandbox_event("sandbox.created")).await;
        logger.flush().await;

        assert_eq!(server.requests().await.len(), 2);
    }

    #[tokio::test]
    async fn failed_endpoint_does_not_block_other_endpoints() {
        let failing_server = TestServer::start(10).await;
        let healthy_server = TestServer::start(0).await;
        let mut config = test_config(vec![
            endpoint(&failing_server.url(), &["sandbox.created"]),
            endpoint(&healthy_server.url(), &["sandbox.created"]),
        ]);
        config.max_retries = 1;
        config.retry_backoff_millis = 1;
        let logger = HttpLogger::new(config);

        logger.log(sandbox_event("sandbox.created")).await;
        logger.flush().await;

        assert_eq!(failing_server.requests().await.len(), 2);
        assert_eq!(healthy_server.requests().await.len(), 1);
    }

    fn test_config(endpoints: Vec<WebhookEndpointConfig>) -> HttpLoggerConfig {
        HttpLoggerConfig {
            endpoints,
            batch_size: 100,
            flush_interval_secs: 60,
            max_retries: 0,
            retry_backoff_millis: 1,
            request_timeout_secs: 1,
        }
    }

    fn endpoint(url: &str, events: &[&str]) -> WebhookEndpointConfig {
        WebhookEndpointConfig {
            url: url.to_string(),
            events: events.iter().map(|event| event.to_string()).collect(),
            hmac_secret: None,
        }
    }

    fn sandbox_event(event: &str) -> LogEvent {
        LogEvent::new(LogLevel::Info, event)
            .field("sandbox_id", "sbx-1")
            .field("template_id", "tmpl-1")
    }

    fn api_request_event() -> LogEvent {
        LogEvent::new(LogLevel::Debug, "api.request").field("handler", "create_sandbox")
    }

    async fn wait_for_requests(server: &TestServer, expected: usize) {
        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                if server.requests().await.len() >= expected {
                    break;
                }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .expect("timed out waiting for webhook requests");
    }

    #[derive(Clone, Default)]
    struct TestState {
        requests: Arc<Mutex<Vec<CapturedRequest>>>,
        failures_remaining: Arc<AtomicUsize>,
    }

    #[derive(Clone)]
    struct CapturedRequest {
        headers: HeaderMap,
        body: Vec<u8>,
    }

    impl CapturedRequest {
        fn header(&self, name: &str) -> Option<&str> {
            self.headers.get(name).and_then(|value| value.to_str().ok())
        }

        fn json(&self) -> Value {
            serde_json::from_slice(&self.body).expect("request body should be valid JSON")
        }
    }

    struct TestServer {
        addr: std::net::SocketAddr,
        state: TestState,
    }

    impl TestServer {
        async fn start(failures: usize) -> Self {
            let state = TestState {
                requests: Arc::new(Mutex::new(Vec::new())),
                failures_remaining: Arc::new(AtomicUsize::new(failures)),
            };
            let app = Router::new()
                .route("/", post(capture_request))
                .with_state(state.clone());
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
                .await
                .expect("bind test webhook server");
            let addr = listener.local_addr().expect("test server local addr");

            tokio::spawn(async move {
                axum::serve(listener, app)
                    .await
                    .expect("test webhook server failed");
            });

            Self { addr, state }
        }

        fn url(&self) -> String {
            format!("http://{}", self.addr)
        }

        async fn requests(&self) -> Vec<CapturedRequest> {
            self.state.requests.lock().await.clone()
        }
    }

    async fn capture_request(
        State(state): State<TestState>,
        headers: HeaderMap,
        body: Bytes,
    ) -> StatusCode {
        state.requests.lock().await.push(CapturedRequest {
            headers,
            body: body.to_vec(),
        });

        if state
            .failures_remaining
            .fetch_update(Ordering::SeqCst, Ordering::SeqCst, |remaining| {
                remaining.checked_sub(1)
            })
            .is_ok()
        {
            StatusCode::INTERNAL_SERVER_ERROR
        } else {
            StatusCode::OK
        }
    }
}

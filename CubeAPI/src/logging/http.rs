// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Asynchronous HTTP webhook backend for structured lifecycle events.
//!
//! `HttpLogger::log` only performs a bounded `try_send`, so a slow or
//! unreachable receiver never blocks the CubeAPI request path. A background
//! worker serialises each event once, sends it to every configured endpoint,
//! and retries transient failures with exponential backoff.

use super::{LogEvent, Logger};
use async_trait::async_trait;
use reqwest::{Client, StatusCode, Url};
use sha2::{Digest, Sha256};
use std::{sync::Arc, time::Duration};
use tokio::sync::{mpsc, oneshot};
use tracing::{error, info, warn};

const DEFAULT_EVENTS: [&str; 4] = [
    "sandbox.created",
    "sandbox.deleted",
    "sandbox.paused",
    "sandbox.resumed",
];
const MAX_RETRIES: u32 = 10;
const MAX_RETRY_DELAY: Duration = Duration::from_secs(60);
const MAX_RESPONSE_BODY_LOG_CHARS: usize = 256;

/// Configuration for asynchronous webhook delivery.
#[derive(Debug, Clone)]
pub struct HttpLoggerConfig {
    /// One or more HTTP(S) URLs. Invalid schemes and URLs are ignored.
    pub urls: Vec<String>,
    /// Event names accepted by the logger. `*` accepts every event.
    pub events: Vec<String>,
    /// Optional secret used for `X-Cube-Signature-256`.
    pub secret: Option<String>,
    /// Maximum number of events waiting for the background worker.
    pub queue_size: usize,
    /// Number of retries after the initial request.
    pub max_retries: u32,
    /// Initial exponential backoff delay in milliseconds.
    pub retry_base_delay_ms: u64,
    /// Timeout applied to each HTTP request.
    pub request_timeout_secs: u64,
}

impl Default for HttpLoggerConfig {
    fn default() -> Self {
        Self {
            urls: Vec::new(),
            events: DEFAULT_EVENTS
                .iter()
                .map(|event| (*event).to_string())
                .collect(),
            secret: None,
            queue_size: 1024,
            max_retries: 3,
            retry_base_delay_ms: 250,
            request_timeout_secs: 5,
        }
    }
}

impl HttpLoggerConfig {
    /// Build the runtime configuration from the comma-separated values held
    /// by [`crate::config::ServerConfig`].
    pub fn from_strings(
        urls: &str,
        events: &str,
        secret: Option<String>,
        queue_size: usize,
        max_retries: u32,
        retry_base_delay_ms: u64,
        request_timeout_secs: u64,
    ) -> Self {
        let default_events = DEFAULT_EVENTS.join(",");
        let event_source = if events.trim().is_empty() {
            default_events.as_str()
        } else {
            events
        };

        Self {
            urls: split_csv(urls),
            events: split_csv(event_source),
            secret: secret.filter(|value| !value.is_empty()),
            queue_size,
            max_retries,
            retry_base_delay_ms,
            request_timeout_secs,
        }
    }

    pub fn is_enabled(&self) -> bool {
        self.urls.iter().any(|url| !url.trim().is_empty())
    }
}

fn split_csv(value: &str) -> Vec<String> {
    value
        .split(',')
        .map(str::trim)
        .filter(|item| !item.is_empty())
        .map(ToOwned::to_owned)
        .collect()
}

#[derive(Debug)]
struct RuntimeConfig {
    endpoints: Vec<Url>,
    events: Vec<String>,
    secret: Option<String>,
    queue_size: usize,
    max_retries: u32,
    retry_base_delay: Duration,
    request_timeout: Duration,
}

impl RuntimeConfig {
    fn from_config(config: HttpLoggerConfig) -> Self {
        let endpoints = config
            .urls
            .into_iter()
            .filter_map(|raw_url| match Url::parse(&raw_url) {
                Ok(url) if matches!(url.scheme(), "http" | "https") => {
                    if url.scheme() == "http" && !is_loopback(&url) {
                        warn!(
                            webhook_url = %url,
                            "webhook URL is not HTTPS; use HTTPS outside local development"
                        );
                    }
                    Some(url)
                }
                Ok(url) => {
                    warn!(
                        webhook_url = %url,
                        "ignoring webhook URL with unsupported scheme"
                    );
                    None
                }
                Err(error) => {
                    warn!(webhook_url = %raw_url, %error, "ignoring invalid webhook URL");
                    None
                }
            })
            .collect();

        let max_retries = config.max_retries.min(MAX_RETRIES);
        if config.max_retries > MAX_RETRIES {
            warn!(
                configured = config.max_retries,
                capped = MAX_RETRIES,
                "webhook retry count is too large; capping it"
            );
        }

        let retry_base_delay_ms = config
            .retry_base_delay_ms
            .min(MAX_RETRY_DELAY.as_millis() as u64);
        let request_timeout_secs = config.request_timeout_secs.clamp(1, 300);

        Self {
            endpoints,
            events: config.events,
            secret: config.secret,
            queue_size: config.queue_size.max(1),
            max_retries,
            retry_base_delay: Duration::from_millis(retry_base_delay_ms),
            request_timeout: Duration::from_secs(request_timeout_secs),
        }
    }

    fn accepts(&self, event: &str) -> bool {
        self.events
            .iter()
            .any(|configured| configured == "*" || configured == event)
    }
}

fn is_loopback(url: &Url) -> bool {
    matches!(
        url.host_str(),
        Some("localhost") | Some("127.0.0.1") | Some("::1")
    )
}

enum Message {
    Event(LogEvent),
    Flush(oneshot::Sender<()>),
}

/// A non-blocking webhook logger backed by a bounded Tokio channel.
#[derive(Clone)]
pub struct HttpLogger {
    tx: mpsc::Sender<Message>,
    config: Arc<RuntimeConfig>,
}

impl HttpLogger {
    /// Create a webhook logger with a client that does not follow redirects.
    ///
    /// Redirects are disabled so a configured endpoint cannot silently move a
    /// signed payload or secret to an unexpected internal destination.
    pub fn new(config: HttpLoggerConfig) -> Self {
        let client = Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .build()
            .expect("failed to build webhook HTTP client");
        Self::with_client(client, config)
    }

    /// Construct a logger using a caller-provided client. This is useful for
    /// tests and for deployments that want to share a connection pool.
    pub fn with_client(client: Client, config: HttpLoggerConfig) -> Self {
        let config = Arc::new(RuntimeConfig::from_config(config));
        if config.endpoints.is_empty() {
            info!("webhook logger started without valid endpoints; delivery disabled");
        } else {
            info!(
                endpoint_count = config.endpoints.len(),
                queue_size = config.queue_size,
                max_retries = config.max_retries,
                "webhook logger started"
            );
        }

        let (tx, mut rx) = mpsc::channel(config.queue_size);
        let worker_config = Arc::clone(&config);
        tokio::spawn(async move {
            while let Some(message) = rx.recv().await {
                match message {
                    Message::Event(event) => {
                        if worker_config.accepts(&event.event) {
                            deliver_event(&client, &worker_config, &event).await;
                        }
                    }
                    Message::Flush(reply) => {
                        let _ = reply.send(());
                    }
                }
            }
            info!("webhook logger worker stopped");
        });

        Self { tx, config }
    }
}

#[async_trait]
impl Logger for HttpLogger {
    async fn log(&self, event: LogEvent) {
        if !self.config.accepts(&event.event) {
            return;
        }

        let event_name = event.event.clone();
        match self.tx.try_send(Message::Event(event)) {
            Ok(()) => {}
            Err(mpsc::error::TrySendError::Full(Message::Event(event))) => {
                warn!(
                    event = %event.event,
                    queue_size = self.config.queue_size,
                    "webhook queue is full; dropping event"
                );
            }
            Err(mpsc::error::TrySendError::Closed(Message::Event(_))) => {
                warn!(event = %event_name, "webhook worker is stopped; dropping event");
            }
            Err(_) => unreachable!("flush messages are never sent through log()"),
        }
    }

    async fn flush(&self) {
        let (reply, wait) = oneshot::channel();
        if self.tx.send(Message::Flush(reply)).await.is_ok() {
            let _ = wait.await;
        }
    }

    fn name(&self) -> &'static str {
        "http-webhook"
    }
}

async fn deliver_event(client: &Client, config: &RuntimeConfig, event: &LogEvent) {
    let body = match serde_json::to_vec(event) {
        Ok(body) => body,
        Err(error) => {
            error!(event = %event.event, %error, "failed to serialise webhook event");
            return;
        }
    };
    let signature = config
        .secret
        .as_deref()
        .map(|secret| format!("sha256={}", hmac_sha256_hex(secret.as_bytes(), &body)));

    let deliveries = config.endpoints.iter().map(|endpoint| {
        deliver_to_endpoint(
            client,
            config,
            endpoint,
            &body,
            signature.as_deref(),
            &event.event,
        )
    });
    futures::future::join_all(deliveries).await;
}

async fn deliver_to_endpoint(
    client: &Client,
    config: &RuntimeConfig,
    endpoint: &Url,
    body: &[u8],
    signature: Option<&str>,
    event_name: &str,
) {
    for retry_index in 0..=config.max_retries {
        let mut request = client
            .post(endpoint.clone())
            .timeout(config.request_timeout)
            .header(reqwest::header::CONTENT_TYPE, "application/json")
            .body(body.to_vec());
        if let Some(signature) = signature {
            request = request.header("X-Cube-Signature-256", signature);
        }

        let result = request.send().await;
        match result {
            Ok(response) if response.status().is_success() => return,
            Ok(response) => {
                let status = response.status();
                let response_body = response.text().await.unwrap_or_default();
                let body_snippet: String = response_body
                    .chars()
                    .take(MAX_RESPONSE_BODY_LOG_CHARS)
                    .collect();
                let retryable = is_retryable_status(status);

                warn!(
                    event = %event_name,
                    webhook_url = %endpoint,
                    status = status.as_u16(),
                    attempt = retry_index + 1,
                    retryable,
                    response = %body_snippet,
                    "webhook delivery failed"
                );
                if !retryable {
                    return;
                }
            }
            Err(error) => {
                warn!(
                    event = %event_name,
                    webhook_url = %endpoint,
                    attempt = retry_index + 1,
                    %error,
                    "webhook delivery request failed"
                );
            }
        }

        if retry_index == config.max_retries {
            error!(
                event = %event_name,
                webhook_url = %endpoint,
                attempts = retry_index + 1,
                "webhook delivery exhausted retries"
            );
            return;
        }

        let delay = retry_delay(config.retry_base_delay, retry_index);
        if !delay.is_zero() {
            tokio::time::sleep(delay).await;
        }
    }
}

fn is_retryable_status(status: StatusCode) -> bool {
    status == StatusCode::REQUEST_TIMEOUT
        || status == StatusCode::TOO_MANY_REQUESTS
        || status.is_server_error()
}

fn retry_delay(base: Duration, retry_index: u32) -> Duration {
    let multiplier = 1u128 << retry_index.min(16);
    let millis = base
        .as_millis()
        .saturating_mul(multiplier)
        .min(MAX_RETRY_DELAY.as_millis());
    Duration::from_millis(millis as u64)
}

fn hmac_sha256_hex(secret: &[u8], message: &[u8]) -> String {
    const BLOCK_SIZE: usize = 64;
    let mut key = [0u8; BLOCK_SIZE];
    if secret.len() > BLOCK_SIZE {
        let digest = Sha256::digest(secret);
        key[..digest.len()].copy_from_slice(&digest);
    } else {
        key[..secret.len()].copy_from_slice(secret);
    }

    let mut inner_pad = [0x36u8; BLOCK_SIZE];
    let mut outer_pad = [0x5cu8; BLOCK_SIZE];
    for index in 0..BLOCK_SIZE {
        inner_pad[index] ^= key[index];
        outer_pad[index] ^= key[index];
    }

    let mut inner = Sha256::new();
    inner.update(inner_pad);
    inner.update(message);
    let inner_digest = inner.finalize();

    let mut outer = Sha256::new();
    outer.update(outer_pad);
    outer.update(inner_digest);
    let digest = outer.finalize();

    digest.iter().map(|byte| format!("{byte:02x}")).collect()
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
    use std::sync::{
        atomic::{AtomicUsize, Ordering},
        Mutex,
    };

    type RecordedRequest = (String, Vec<u8>);

    #[derive(Clone)]
    struct ReceiverState {
        requests: Arc<Mutex<Vec<RecordedRequest>>>,
        attempts: Arc<AtomicUsize>,
        fail_first: bool,
        status: StatusCode,
    }

    impl Default for ReceiverState {
        fn default() -> Self {
            Self {
                requests: Arc::default(),
                attempts: Arc::default(),
                fail_first: false,
                status: StatusCode::NO_CONTENT,
            }
        }
    }

    async fn receiver(
        State(state): State<ReceiverState>,
        headers: HeaderMap,
        body: Bytes,
    ) -> StatusCode {
        let signature = headers
            .get("X-Cube-Signature-256")
            .and_then(|value| value.to_str().ok())
            .unwrap_or_default()
            .to_string();
        let attempt = state.attempts.fetch_add(1, Ordering::SeqCst);
        state
            .requests
            .lock()
            .unwrap()
            .push((signature, body.to_vec()));
        if state.fail_first && attempt == 0 {
            StatusCode::INTERNAL_SERVER_ERROR
        } else {
            state.status
        }
    }

    async fn start_receiver(state: ReceiverState) -> (String, tokio::task::JoinHandle<()>) {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let task = tokio::spawn(async move {
            axum::serve(
                listener,
                Router::new()
                    .route("/webhook", post(receiver))
                    .with_state(state),
            )
            .await
            .unwrap();
        });
        (format!("http://{address}/webhook"), task)
    }

    fn config(url: String) -> HttpLoggerConfig {
        HttpLoggerConfig {
            urls: vec![url],
            events: vec!["sandbox.created".to_string()],
            secret: Some("test-secret".to_string()),
            queue_size: 8,
            max_retries: 1,
            retry_base_delay_ms: 0,
            request_timeout_secs: 2,
        }
    }

    #[tokio::test]
    async fn delivers_json_with_hmac_and_retries_server_errors() {
        let state = ReceiverState {
            fail_first: true,
            status: StatusCode::NO_CONTENT,
            ..Default::default()
        };
        let requests = Arc::clone(&state.requests);
        let (url, server) = start_receiver(state).await;
        let logger = HttpLogger::new(config(url));

        logger
            .log(
                LogEvent::new(LogLevel::Info, "sandbox.created")
                    .field("sandbox_id", "sbx-1")
                    .field("template_id", "tpl-1"),
            )
            .await;
        logger.flush().await;

        let requests = requests.lock().unwrap();
        assert_eq!(requests.len(), 2);
        assert_eq!(requests[0].1, requests[1].1);
        let expected = format!("sha256={}", hmac_sha256_hex(b"test-secret", &requests[0].1));
        assert_eq!(requests[0].0, expected);
        let payload: serde_json::Value = serde_json::from_slice(&requests[0].1).unwrap();
        assert_eq!(payload["event"], "sandbox.created");
        assert_eq!(payload["sandbox_id"], "sbx-1");
        assert_eq!(payload["template_id"], "tpl-1");
        server.abort();
    }

    #[tokio::test]
    async fn filters_unsubscribed_events_before_enqueue() {
        let state = ReceiverState {
            status: StatusCode::NO_CONTENT,
            ..Default::default()
        };
        let attempts = Arc::clone(&state.attempts);
        let (url, server) = start_receiver(state).await;
        let logger = HttpLogger::new(config(url));

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.deleted"))
            .await;
        logger.flush().await;

        assert_eq!(attempts.load(Ordering::SeqCst), 0);
        server.abort();
    }

    #[tokio::test]
    async fn log_returns_without_waiting_for_receiver() {
        let logger = HttpLogger::new(HttpLoggerConfig {
            urls: vec!["http://127.0.0.1:9/webhook".to_string()],
            events: vec!["sandbox.created".to_string()],
            secret: None,
            queue_size: 1,
            max_retries: 0,
            retry_base_delay_ms: 0,
            request_timeout_secs: 2,
        });

        tokio::time::timeout(
            Duration::from_millis(100),
            logger.log(LogEvent::new(LogLevel::Info, "sandbox.created")),
        )
        .await
        .expect("enqueue must not wait for HTTP delivery");
    }

    #[test]
    fn hmac_matches_rfc_4231_case() {
        assert_eq!(
            hmac_sha256_hex(&[0x0b; 20], b"Hi There",),
            "b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7"
        );
    }

    #[test]
    fn retry_delay_is_bounded() {
        assert_eq!(
            retry_delay(Duration::from_millis(250), 0),
            Duration::from_millis(250)
        );
        assert_eq!(
            retry_delay(Duration::from_millis(250), 3),
            Duration::from_millis(2000)
        );
        assert_eq!(retry_delay(Duration::from_secs(60), 10), MAX_RETRY_DELAY);
    }
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! HTTP Webhook log backend.
//!
//! Each instance owns a bounded queue and a background delivery task. Calling
//! [`Logger::log`] only attempts to enqueue an event, so sandbox lifecycle
//! handlers never wait for an external HTTP endpoint.

use std::{collections::HashSet, net::IpAddr, sync::Arc, time::Duration};

use async_trait::async_trait;
use hmac::{Hmac, Mac};
use reqwest::StatusCode;
use sha2::Sha256;
use tokio::sync::{mpsc, oneshot};
use tracing::{error, warn};

use super::{LogEvent, Logger};

type HmacSha256 = Hmac<Sha256>;

const MAX_WEBHOOK_RETRIES: usize = 10;
const MAX_RETRY_DELAY_MS: u64 = 30_000;
const MAX_ERROR_BODY_BYTES: usize = 256;

/// Configuration for one HTTP Webhook endpoint.
#[derive(Debug, Clone)]
pub struct HttpLoggerConfig {
    /// Full URL that receives each matching event as a JSON POST.
    pub url: String,
    /// Event names the endpoint subscribes to.
    pub events: HashSet<String>,
    /// Optional secret used for the `X-Cube-Signature-256` header.
    pub secret: Option<String>,
    /// Maximum pending events before new events are dropped.
    pub queue_capacity: usize,
    /// Number of retries after the initial attempt.
    pub max_retries: usize,
    /// Initial retry backoff in milliseconds; later attempts double it.
    pub retry_base_ms: u64,
    /// HTTP client request timeout in seconds.
    pub request_timeout_secs: u64,
}

impl HttpLoggerConfig {
    pub fn new(url: impl Into<String>, events: impl IntoIterator<Item = String>) -> Self {
        Self {
            url: url.into(),
            events: events.into_iter().collect(),
            secret: None,
            queue_capacity: 1000,
            max_retries: 3,
            retry_base_ms: 200,
            request_timeout_secs: 10,
        }
    }
}

enum Message {
    Event(LogEvent),
    Flush(oneshot::Sender<()>),
}

/// Asynchronous HTTP Webhook logger.
#[derive(Clone)]
pub struct HttpLogger {
    tx: mpsc::Sender<Message>,
    events: Arc<HashSet<String>>,
}

/// Shared HTTP client configured specifically for Webhook delivery.
#[derive(Clone)]
pub(crate) struct WebhookClient(reqwest::Client);

impl WebhookClient {
    pub(crate) fn new(request_timeout_secs: u64) -> Result<Self, reqwest::Error> {
        reqwest::Client::builder()
            .timeout(Duration::from_secs(request_timeout_secs))
            .redirect(reqwest::redirect::Policy::none())
            .build()
            .map(Self)
    }
}

impl HttpLogger {
    /// Create a logger and start its background delivery task.
    #[cfg(test)]
    pub fn new(config: HttpLoggerConfig) -> Result<Self, reqwest::Error> {
        let client = WebhookClient::new(config.request_timeout_secs)?;
        Ok(Self::with_client(config, client))
    }

    /// Create a logger that shares a dedicated Webhook connection pool.
    pub(crate) fn with_client(mut config: HttpLoggerConfig, client: WebhookClient) -> Self {
        if config.max_retries > MAX_WEBHOOK_RETRIES {
            warn!(
                configured = config.max_retries,
                maximum = MAX_WEBHOOK_RETRIES,
                "Webhook retries capped"
            );
            config.max_retries = MAX_WEBHOOK_RETRIES;
        }
        if uses_plaintext_outside_loopback(&config.url) {
            warn!(
                url = %config.url,
                "Webhook URL uses plaintext HTTP outside loopback; use HTTPS to protect event data and credentials"
            );
        }

        let events = Arc::new(config.events.clone());
        let capacity = config.queue_capacity.max(1);
        let (tx, mut rx) = mpsc::channel(capacity);
        let client = client.0;

        tokio::spawn(async move {
            while let Some(message) = rx.recv().await {
                match message {
                    Message::Event(event) => {
                        deliver(&client, &config, event).await;
                    }
                    Message::Flush(reply) => {
                        let _ = reply.send(());
                    }
                }
            }
        });

        Self { tx, events }
    }
}

async fn deliver(client: &reqwest::Client, config: &HttpLoggerConfig, event: LogEvent) {
    let body = match serde_json::to_vec(&event).map(bytes::Bytes::from) {
        Ok(body) => body,
        Err(err) => {
            error!(error = %err, event = %event.event, "Webhook event serialization failed");
            return;
        }
    };

    let signature = match config.secret.as_deref() {
        Some(secret) => {
            let mut mac = match HmacSha256::new_from_slice(secret.as_bytes()) {
                Ok(mac) => mac,
                Err(err) => {
                    error!(error = %err, event = %event.event, "Webhook HMAC initialization failed");
                    return;
                }
            };
            mac.update(&body);
            Some(format!(
                "sha256={}",
                hex::encode(mac.finalize().into_bytes())
            ))
        }
        None => None,
    };

    for attempt in 0..=config.max_retries {
        let mut request = client
            .post(&config.url)
            .header(reqwest::header::CONTENT_TYPE, "application/json")
            .body(body.clone());

        if let Some(signature) = signature.as_deref() {
            request = request.header("X-Cube-Signature-256", signature);
        }

        match request.send().await {
            Ok(response) if response.status().is_success() => return,
            Ok(mut response) => {
                let status = response.status();
                let response_body = response_body_preview(&mut response).await;
                if !should_retry_status(status) {
                    warn!(
                        event = %event.event,
                        %status,
                        response_body = %response_body,
                        "Webhook endpoint rejected event without retry"
                    );
                    return;
                }
                warn!(
                    event = %event.event,
                    %status,
                    response_body = %response_body,
                    attempt,
                    "Webhook delivery failed"
                );
            }
            Err(err) => {
                warn!(event = %event.event, %err, attempt, "Webhook request failed");
            }
        }

        if attempt < config.max_retries {
            tokio::time::sleep(retry_delay(config.retry_base_ms, attempt)).await;
        }
    }

    error!(event = %event.event, url = %config.url, "Webhook delivery exhausted retries");
}

fn retry_delay(base_ms: u64, attempt: usize) -> Duration {
    let multiplier = 1u64.checked_shl(attempt as u32).unwrap_or(u64::MAX);
    Duration::from_millis(base_ms.saturating_mul(multiplier).min(MAX_RETRY_DELAY_MS))
}

async fn response_body_preview(response: &mut reqwest::Response) -> String {
    match response.chunk().await {
        Ok(Some(chunk)) => format_body_preview(&chunk),
        Ok(None) | Err(_) => String::new(),
    }
}

fn format_body_preview(body: &[u8]) -> String {
    let preview_len = body.len().min(MAX_ERROR_BODY_BYTES);
    let mut preview: String = String::from_utf8_lossy(&body[..preview_len])
        .chars()
        .map(|ch| if ch.is_control() { ' ' } else { ch })
        .collect();
    if body.len() > preview_len {
        preview.push_str("...");
    }
    preview.trim().to_owned()
}

fn uses_plaintext_outside_loopback(url: &str) -> bool {
    let Ok(url) = reqwest::Url::parse(url) else {
        return false;
    };
    if url.scheme() != "http" {
        return false;
    }
    let Some(host) = url.host_str() else {
        return false;
    };
    let host = host
        .strip_prefix('[')
        .and_then(|host| host.strip_suffix(']'))
        .unwrap_or(host);
    if host.eq_ignore_ascii_case("localhost")
        || host
            .to_ascii_lowercase()
            .strip_suffix(".localhost")
            .is_some()
    {
        return false;
    }
    host.parse::<IpAddr>()
        .map(|address| !address.is_loopback())
        .unwrap_or(true)
}

fn should_retry_status(status: StatusCode) -> bool {
    status == StatusCode::REQUEST_TIMEOUT
        || status == StatusCode::TOO_MANY_REQUESTS
        || status.is_server_error()
}

#[async_trait]
impl Logger for HttpLogger {
    async fn log(&self, event: LogEvent) {
        if !self.events.contains(&event.event) {
            return;
        }
        let event_name = event.event.clone();
        match self.tx.try_send(Message::Event(event)) {
            Ok(()) => {}
            Err(mpsc::error::TrySendError::Full(_)) => {
                warn!(event = %event_name, "Webhook queue is full; dropping event");
            }
            Err(mpsc::error::TrySendError::Closed(_)) => {
                error!(event = %event_name, "Webhook worker is unavailable; dropping event");
            }
        }
    }

    async fn flush(&self) {
        let (tx, rx) = oneshot::channel();
        if self.tx.send(Message::Flush(tx)).await.is_ok() {
            let _ = rx.await;
        }
    }

    fn name(&self) -> &'static str {
        "http"
    }
}

#[cfg(test)]
mod tests {
    use std::{
        collections::VecDeque,
        sync::{
            atomic::{AtomicUsize, Ordering},
            Arc,
        },
    };

    use axum::{
        body::Bytes,
        extract::State,
        http::{HeaderMap, StatusCode},
        response::Redirect,
        routing::post,
        Router,
    };
    use tokio::sync::Mutex;

    use super::*;
    use crate::logging::LogLevel;

    #[derive(Clone, Default)]
    struct ReceiverState {
        attempts: Arc<AtomicUsize>,
        bodies: Arc<Mutex<Vec<Vec<u8>>>>,
        signatures: Arc<Mutex<Vec<Option<String>>>>,
        statuses: Arc<Mutex<VecDeque<StatusCode>>>,
    }

    async fn receive(
        State(state): State<ReceiverState>,
        headers: HeaderMap,
        body: Bytes,
    ) -> StatusCode {
        state.attempts.fetch_add(1, Ordering::SeqCst);
        state.bodies.lock().await.push(body.to_vec());
        state.signatures.lock().await.push(
            headers
                .get("x-cube-signature-256")
                .and_then(|value| value.to_str().ok())
                .map(str::to_owned),
        );
        state
            .statuses
            .lock()
            .await
            .pop_front()
            .unwrap_or(StatusCode::OK)
    }

    async fn spawn_receiver(state: ReceiverState) -> String {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("listener should bind");
        let address = listener
            .local_addr()
            .expect("listener should expose address");
        tokio::spawn(async move {
            axum::serve(
                listener,
                Router::new()
                    .route("/webhook", post(receive))
                    .with_state(state),
            )
            .await
            .expect("receiver should run");
        });
        format!("http://{address}/webhook")
    }

    async fn redirect(State(target): State<String>) -> Redirect {
        Redirect::temporary(&target)
    }

    async fn spawn_redirect(target: String) -> String {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("listener should bind");
        let address = listener
            .local_addr()
            .expect("listener should expose address");
        tokio::spawn(async move {
            axum::serve(
                listener,
                Router::new()
                    .route("/webhook", post(redirect))
                    .with_state(target),
            )
            .await
            .expect("redirect server should run");
        });
        format!("http://{address}/webhook")
    }

    fn event(name: &str) -> LogEvent {
        LogEvent::new(LogLevel::Info, name).field("sandbox_id", "sb-test")
    }

    #[tokio::test]
    async fn delivers_matching_event_with_hmac_signature() {
        let state = ReceiverState::default();
        let url = spawn_receiver(state.clone()).await;
        let mut config = HttpLoggerConfig::new(url, ["sandbox.created".to_string()]);
        config.secret = Some("test-secret".to_string());
        let logger = HttpLogger::new(config).expect("logger should initialize");

        logger.log(event("sandbox.created")).await;
        logger.flush().await;

        assert_eq!(state.attempts.load(Ordering::SeqCst), 1);
        let body = state.bodies.lock().await[0].clone();
        let json: serde_json::Value = serde_json::from_slice(&body).expect("valid JSON body");
        assert_eq!(json["event"], "sandbox.created");
        assert_eq!(json["sandbox_id"], "sb-test");

        let mut mac = HmacSha256::new_from_slice(b"test-secret").expect("valid HMAC key");
        mac.update(&body);
        let expected = format!("sha256={}", hex::encode(mac.finalize().into_bytes()));
        assert_eq!(
            state.signatures.lock().await[0].as_deref(),
            Some(expected.as_str())
        );
    }

    #[tokio::test]
    async fn filters_events_outside_subscription() {
        let state = ReceiverState::default();
        let url = spawn_receiver(state.clone()).await;
        let logger = HttpLogger::new(HttpLoggerConfig::new(url, ["sandbox.deleted".to_string()]))
            .expect("logger should initialize");

        logger.log(event("sandbox.created")).await;
        logger.flush().await;

        assert_eq!(state.attempts.load(Ordering::SeqCst), 0);
    }

    #[tokio::test]
    async fn retries_transient_failures() {
        let state = ReceiverState::default();
        state
            .statuses
            .lock()
            .await
            .extend([StatusCode::INTERNAL_SERVER_ERROR, StatusCode::OK]);
        let url = spawn_receiver(state.clone()).await;
        let mut config = HttpLoggerConfig::new(url, ["sandbox.paused".to_string()]);
        config.max_retries = 1;
        config.retry_base_ms = 1;
        let logger = HttpLogger::new(config).expect("logger should initialize");

        logger.log(event("sandbox.paused")).await;
        logger.flush().await;

        assert_eq!(state.attempts.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn stops_after_retry_limit_is_exhausted() {
        let state = ReceiverState::default();
        state.statuses.lock().await.extend(
            std::iter::repeat(StatusCode::INTERNAL_SERVER_ERROR).take(MAX_WEBHOOK_RETRIES + 1),
        );
        let url = spawn_receiver(state.clone()).await;
        let mut config = HttpLoggerConfig::new(url, ["sandbox.paused".to_string()]);
        config.max_retries = usize::MAX;
        config.retry_base_ms = 0;
        let logger = HttpLogger::new(config).expect("logger should initialize");

        logger.log(event("sandbox.paused")).await;
        logger.flush().await;

        assert_eq!(
            state.attempts.load(Ordering::SeqCst),
            MAX_WEBHOOK_RETRIES + 1
        );
    }

    #[tokio::test]
    async fn does_not_retry_non_retryable_client_errors() {
        let state = ReceiverState::default();
        state
            .statuses
            .lock()
            .await
            .extend([StatusCode::BAD_REQUEST, StatusCode::OK]);
        let url = spawn_receiver(state.clone()).await;
        let mut config = HttpLoggerConfig::new(url, ["sandbox.paused".to_string()]);
        config.max_retries = 3;
        config.retry_base_ms = 0;
        let logger = HttpLogger::new(config).expect("logger should initialize");

        logger.log(event("sandbox.paused")).await;
        logger.flush().await;

        assert_eq!(state.attempts.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn retries_network_errors() {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("listener should bind");
        let address = listener
            .local_addr()
            .expect("listener should expose address");
        let attempts = Arc::new(AtomicUsize::new(0));
        let server_attempts = attempts.clone();
        tokio::spawn(async move {
            for _ in 0..2 {
                let (connection, _) = listener.accept().await.expect("connection should arrive");
                server_attempts.fetch_add(1, Ordering::SeqCst);
                drop(connection);
            }
        });

        let mut config = HttpLoggerConfig::new(
            format!("http://{address}/webhook"),
            ["sandbox.paused".to_string()],
        );
        config.max_retries = 1;
        config.retry_base_ms = 0;
        let logger = HttpLogger::new(config).expect("logger should initialize");

        logger.log(event("sandbox.paused")).await;
        logger.flush().await;

        assert_eq!(attempts.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn does_not_follow_redirects() {
        let target_state = ReceiverState::default();
        let target_url = spawn_receiver(target_state.clone()).await;
        let redirect_url = spawn_redirect(target_url).await;
        let mut config = HttpLoggerConfig::new(redirect_url, ["sandbox.created".to_string()]);
        config.max_retries = 0;
        let logger = HttpLogger::new(config).expect("logger should initialize");

        logger.log(event("sandbox.created")).await;
        logger.flush().await;

        assert_eq!(target_state.attempts.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn warns_only_for_plaintext_urls_outside_loopback() {
        assert!(!uses_plaintext_outside_loopback(
            "https://hooks.example.com/events"
        ));
        assert!(!uses_plaintext_outside_loopback(
            "http://127.0.0.1:8080/events"
        ));
        assert!(!uses_plaintext_outside_loopback("http://[::1]:8080/events"));
        assert!(!uses_plaintext_outside_loopback(
            "http://receiver.localhost:8080/events"
        ));
        assert!(uses_plaintext_outside_loopback(
            "http://hooks.example.com/events"
        ));
    }

    #[test]
    fn bounds_and_sanitizes_error_body_preview() {
        let mut body = vec![b'a'; MAX_ERROR_BODY_BYTES + 16];
        body[1] = b'\n';
        body[2] = b'\r';

        let preview = format_body_preview(&body);

        assert!(!preview.contains('\n'));
        assert!(!preview.contains('\r'));
        assert!(preview.ends_with("..."));
        assert_eq!(preview.len(), MAX_ERROR_BODY_BYTES + 3);
    }

    #[test]
    fn caps_retry_delay() {
        assert_eq!(
            retry_delay(u64::MAX, usize::MAX),
            Duration::from_millis(MAX_RETRY_DELAY_MS)
        );
    }
}

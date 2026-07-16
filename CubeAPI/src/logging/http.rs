// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! HTTP Webhook log backend.
//!
//! Each instance owns a bounded queue and a background delivery task. Calling
//! [`Logger::log`] only attempts to enqueue an event, so sandbox lifecycle
//! handlers never wait for an external HTTP endpoint.

use std::{collections::HashSet, time::Duration};

use async_trait::async_trait;
use hmac::{Hmac, Mac};
use reqwest::StatusCode;
use sha2::Sha256;
use tokio::sync::{mpsc, oneshot};
use tracing::{error, warn};

use super::{LogEvent, Logger};

type HmacSha256 = Hmac<Sha256>;

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

    fn accepts(&self, event: &LogEvent) -> bool {
        self.events.contains(&event.event)
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
}

impl HttpLogger {
    /// Create a logger and start its background delivery task.
    pub fn new(config: HttpLoggerConfig) -> Result<Self, reqwest::Error> {
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(config.request_timeout_secs))
            .redirect(reqwest::redirect::Policy::none())
            .build()?;
        let capacity = config.queue_capacity.max(1);
        let (tx, mut rx) = mpsc::channel(capacity);

        tokio::spawn(async move {
            while let Some(message) = rx.recv().await {
                match message {
                    Message::Event(event) => {
                        if config.accepts(&event) {
                            deliver(&client, &config, event).await;
                        }
                    }
                    Message::Flush(reply) => {
                        let _ = reply.send(());
                    }
                }
            }
        });

        Ok(Self { tx })
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
            Ok(response) if !should_retry_status(response.status()) => {
                warn!(
                    event = %event.event,
                    status = %response.status(),
                    "Webhook endpoint rejected event without retry"
                );
                return;
            }
            Ok(response) => {
                warn!(
                    event = %event.event,
                    status = %response.status(),
                    attempt,
                    "Webhook delivery failed"
                );
            }
            Err(err) => {
                warn!(event = %event.event, %err, attempt, "Webhook request failed");
            }
        }

        if attempt < config.max_retries {
            let multiplier = 1u64.checked_shl(attempt as u32).unwrap_or(u64::MAX);
            let delay_ms = config.retry_base_ms.saturating_mul(multiplier);
            tokio::time::sleep(Duration::from_millis(delay_ms)).await;
        }
    }

    error!(event = %event.event, url = %config.url, "Webhook delivery exhausted retries");
}

fn should_retry_status(status: StatusCode) -> bool {
    status == StatusCode::REQUEST_TIMEOUT
        || status == StatusCode::TOO_MANY_REQUESTS
        || status.is_server_error()
}

#[async_trait]
impl Logger for HttpLogger {
    async fn log(&self, event: LogEvent) {
        match self.tx.try_send(Message::Event(event)) {
            Ok(()) => {}
            Err(mpsc::error::TrySendError::Full(_)) => {
                warn!("Webhook queue is full; dropping event");
            }
            Err(mpsc::error::TrySendError::Closed(_)) => {
                error!("Webhook worker is unavailable; dropping event");
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
}

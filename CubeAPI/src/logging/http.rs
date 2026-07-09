// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! HTTP webhook log backend.
//!
//! The backend receives structured [`LogEvent`] values, filters them by endpoint
//! subscription, and delivers each matching event as an asynchronous JSON POST.
//! `log()` only sends into an internal channel, so a slow or unreachable
//! receiver does not block the API handler that emitted the event.

use std::{collections::HashSet, sync::Arc, time::Duration};

use async_trait::async_trait;
use hmac::{Hmac, Mac};
use reqwest::Client;
use sha2::Sha256;
use tokio::{
    sync::{
        mpsc::{self, UnboundedSender},
        oneshot,
    },
    task::JoinHandle,
};
use tracing::{error, warn};

use super::{LogEvent, Logger};
use crate::config::WebhookEndpointConfig;

type HmacSha256 = Hmac<Sha256>;

const SIGNATURE_HEADER: &str = "X-Cube-Signature-256";
const EVENT_HEADER: &str = "X-Cube-Event";
const TIMESTAMP_HEADER: &str = "X-Cube-Timestamp";

/// Configuration for the HTTP webhook backend.
#[derive(Debug, Clone, Default)]
pub struct HttpLoggerConfig {
    /// Endpoints that receive selected event types.
    pub endpoints: Vec<WebhookEndpointConfig>,
}

impl HttpLoggerConfig {
    pub fn new(endpoints: Vec<WebhookEndpointConfig>) -> Self {
        Self { endpoints }
    }
}

enum Msg {
    Event(LogEvent),
    Flush(oneshot::Sender<()>),
}

/// HTTP webhook log backend.
///
/// Clone is O(1): only the channel sender and endpoint metadata are cloned.
#[derive(Clone)]
pub struct HttpLogger {
    tx: UnboundedSender<Msg>,
    endpoints: Arc<Vec<WebhookEndpoint>>,
}

impl HttpLogger {
    /// Create a webhook logger and spawn its background dispatcher.
    pub fn new(config: HttpLoggerConfig) -> Self {
        let endpoints = Arc::new(
            config
                .endpoints
                .into_iter()
                .filter_map(WebhookEndpoint::from_config)
                .collect::<Vec<_>>(),
        );
        let (tx, rx) = mpsc::unbounded_channel::<Msg>();
        let client = Client::new();

        tokio::spawn(run_worker(rx, client, endpoints.clone()));

        Self { tx, endpoints }
    }

    pub fn is_enabled(&self) -> bool {
        !self.endpoints.is_empty()
    }
}

#[async_trait]
impl Logger for HttpLogger {
    async fn log(&self, event: LogEvent) {
        if !self
            .endpoints
            .iter()
            .any(|endpoint| endpoint.subscribes_to(&event.event))
        {
            return;
        }

        if self.tx.send(Msg::Event(event)).is_err() {
            error!("HttpLogger: dispatcher task is gone, dropping event");
        }
    }

    async fn flush(&self) {
        let (tx, rx) = oneshot::channel();
        if self.tx.send(Msg::Flush(tx)).is_ok() {
            let _ = rx.await;
        }
    }

    fn name(&self) -> &'static str {
        "http"
    }
}

#[derive(Debug, Clone)]
struct WebhookEndpoint {
    url: String,
    events: HashSet<String>,
    secret: Option<String>,
    max_retries: u32,
    timeout: Duration,
    retry_initial_delay: Duration,
}

impl WebhookEndpoint {
    fn from_config(config: WebhookEndpointConfig) -> Option<Self> {
        let url = config.url.trim().to_string();
        if url.is_empty() {
            return None;
        }

        let events = config
            .events
            .into_iter()
            .map(|event| event.trim().to_string())
            .filter(|event| !event.is_empty())
            .collect::<HashSet<_>>();
        if events.is_empty() {
            return None;
        }

        Some(Self {
            url,
            events,
            secret: config.secret.filter(|secret| !secret.trim().is_empty()),
            max_retries: config.max_retries,
            timeout: Duration::from_secs(config.timeout_secs.max(1)),
            retry_initial_delay: Duration::from_millis(config.retry_initial_delay_ms.max(1)),
        })
    }

    fn subscribes_to(&self, event: &str) -> bool {
        self.events.contains("*") || self.events.contains(event)
    }
}

async fn run_worker(
    mut rx: mpsc::UnboundedReceiver<Msg>,
    client: Client,
    endpoints: Arc<Vec<WebhookEndpoint>>,
) {
    let mut handles: Vec<JoinHandle<()>> = Vec::new();

    while let Some(msg) = rx.recv().await {
        handles.retain(|handle| !handle.is_finished());

        match msg {
            Msg::Event(event) => {
                for endpoint in endpoints
                    .iter()
                    .filter(|endpoint| endpoint.subscribes_to(&event.event))
                {
                    let client = client.clone();
                    let endpoint = endpoint.clone();
                    let event = event.clone();
                    handles.push(tokio::spawn(async move {
                        deliver_with_retries(client, endpoint, event).await;
                    }));
                }
            }
            Msg::Flush(reply) => {
                let pending = std::mem::take(&mut handles);
                for handle in pending {
                    let _ = handle.await;
                }
                let _ = reply.send(());
            }
        }
    }

    for handle in handles {
        let _ = handle.await;
    }
}

async fn deliver_with_retries(client: Client, endpoint: WebhookEndpoint, event: LogEvent) {
    let body = match serde_json::to_vec(&event) {
        Ok(body) => body,
        Err(err) => {
            error!(
                event = %event.event,
                error = %err,
                "HttpLogger: failed to serialize webhook payload"
            );
            return;
        }
    };

    let timestamp = event.timestamp.to_rfc3339();
    let signature = endpoint
        .secret
        .as_deref()
        .map(|secret| sign_payload(secret, &body));
    let attempts = endpoint.max_retries.saturating_add(1);

    for attempt in 1..=attempts {
        let mut request = client
            .post(&endpoint.url)
            .timeout(endpoint.timeout)
            .header(reqwest::header::CONTENT_TYPE, "application/json")
            .header(EVENT_HEADER, event.event.as_str())
            .header(TIMESTAMP_HEADER, timestamp.as_str())
            .body(body.clone());

        if let Some(signature) = signature.as_deref() {
            request = request.header(SIGNATURE_HEADER, signature);
        }

        match request.send().await {
            Ok(response) if response.status().is_success() => return,
            Ok(response) => {
                warn!(
                    url = %endpoint.url,
                    event = %event.event,
                    status = %response.status(),
                    attempt,
                    attempts,
                    "HttpLogger: webhook endpoint returned non-success status"
                );
            }
            Err(err) => {
                warn!(
                    url = %endpoint.url,
                    event = %event.event,
                    error = %err,
                    attempt,
                    attempts,
                    "HttpLogger: webhook delivery attempt failed"
                );
            }
        }

        if attempt < attempts {
            tokio::time::sleep(retry_delay(endpoint.retry_initial_delay, attempt)).await;
        }
    }

    error!(
        url = %endpoint.url,
        event = %event.event,
        attempts,
        "HttpLogger: webhook delivery exhausted retries"
    );
}

fn retry_delay(initial: Duration, completed_attempts: u32) -> Duration {
    let multiplier = 1u32.checked_shl(completed_attempts - 1).unwrap_or(u32::MAX);
    initial.saturating_mul(multiplier)
}

pub(crate) fn sign_payload(secret: &str, body: &[u8]) -> String {
    let mut mac =
        HmacSha256::new_from_slice(secret.as_bytes()).expect("HMAC accepts arbitrary key sizes");
    mac.update(body);
    format!("sha256={}", hex::encode(mac.finalize().into_bytes()))
}

#[cfg(test)]
mod tests {
    use std::{
        sync::{
            atomic::{AtomicUsize, Ordering},
            Arc, Mutex,
        },
        time::Duration,
    };

    use axum::{
        body::Bytes,
        extract::State,
        http::{HeaderMap, StatusCode},
        routing::post,
        Router,
    };
    use serde_json::Value;
    use tokio::task::JoinHandle;

    use super::{sign_payload, HttpLogger, HttpLoggerConfig, SIGNATURE_HEADER};
    use crate::{
        config::WebhookEndpointConfig,
        logging::{LogEvent, LogLevel, Logger},
    };

    #[derive(Debug, Clone)]
    struct CapturedRequest {
        headers: HeaderMap,
        body: Vec<u8>,
    }

    #[derive(Clone, Default)]
    struct ReceiverState {
        requests: Arc<Mutex<Vec<CapturedRequest>>>,
        attempts: Arc<AtomicUsize>,
        fail_first: bool,
    }

    async fn capture(
        State(state): State<ReceiverState>,
        headers: HeaderMap,
        body: Bytes,
    ) -> StatusCode {
        let attempt = state.attempts.fetch_add(1, Ordering::SeqCst);
        state.requests.lock().unwrap().push(CapturedRequest {
            headers,
            body: body.to_vec(),
        });

        if state.fail_first && attempt == 0 {
            StatusCode::INTERNAL_SERVER_ERROR
        } else {
            StatusCode::OK
        }
    }

    async fn start_receiver(state: ReceiverState) -> (String, JoinHandle<()>) {
        let app = Router::new()
            .route("/webhook", post(capture))
            .with_state(state);
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("bind test receiver");
        let addr = listener.local_addr().expect("receiver local addr");
        let handle = tokio::spawn(async move {
            axum::serve(listener, app)
                .await
                .expect("test receiver should serve");
        });

        (format!("http://{addr}/webhook"), handle)
    }

    fn endpoint(url: String, events: Vec<&str>, max_retries: u32) -> WebhookEndpointConfig {
        WebhookEndpointConfig {
            url,
            events: events.into_iter().map(str::to_string).collect(),
            secret: Some("top-secret".to_string()),
            max_retries,
            timeout_secs: 1,
            retry_initial_delay_ms: 1,
        }
    }

    #[tokio::test]
    async fn posts_matching_event_with_signature() {
        let state = ReceiverState::default();
        let requests = state.requests.clone();
        let (url, server) = start_receiver(state).await;

        let logger = HttpLogger::new(HttpLoggerConfig::new(vec![endpoint(
            url,
            vec!["sandbox.created"],
            0,
        )]));
        assert!(logger.is_enabled());

        logger
            .log(
                LogEvent::new(LogLevel::Info, "sandbox.created")
                    .field("sandbox_id", "sb-1")
                    .field("template_id", "tpl-1"),
            )
            .await;
        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.deleted").field("sandbox_id", "sb-2"))
            .await;
        logger.flush().await;

        let captured = requests.lock().unwrap();
        assert_eq!(captured.len(), 1);
        let request = &captured[0];
        let payload: Value = serde_json::from_slice(&request.body).expect("payload json");
        assert_eq!(payload["event"], "sandbox.created");
        assert_eq!(payload["sandbox_id"], "sb-1");
        assert_eq!(payload["template_id"], "tpl-1");
        assert_eq!(
            request
                .headers
                .get("X-Cube-Event")
                .expect("event header")
                .to_str()
                .unwrap(),
            "sandbox.created"
        );
        assert_eq!(
            request
                .headers
                .get(SIGNATURE_HEADER)
                .expect("signature header")
                .to_str()
                .unwrap(),
            sign_payload("top-secret", &request.body)
        );

        server.abort();
    }

    #[tokio::test]
    async fn retries_failed_delivery() {
        let state = ReceiverState {
            fail_first: true,
            ..ReceiverState::default()
        };
        let attempts = state.attempts.clone();
        let (url, server) = start_receiver(state).await;

        let logger = HttpLogger::new(HttpLoggerConfig::new(vec![endpoint(
            url,
            vec!["sandbox.deleted"],
            1,
        )]));

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.deleted").field("sandbox_id", "sb-1"))
            .await;
        logger.flush().await;

        assert_eq!(attempts.load(Ordering::SeqCst), 2);

        server.abort();
    }

    #[test]
    fn retry_delay_uses_exponential_backoff() {
        assert_eq!(
            super::retry_delay(Duration::from_millis(50), 1),
            Duration::from_millis(50)
        );
        assert_eq!(
            super::retry_delay(Duration::from_millis(50), 2),
            Duration::from_millis(100)
        );
        assert_eq!(
            super::retry_delay(Duration::from_millis(50), 3),
            Duration::from_millis(200)
        );
    }
}

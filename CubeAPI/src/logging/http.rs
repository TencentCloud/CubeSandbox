// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! HTTP webhook log backend.
//!
//! The backend fans selected structured events out to configured HTTP
//! endpoints. Each endpoint owns a bounded queue and a background worker, so
//! `Logger::log` never waits on external network I/O.

use super::{LogEvent, Logger};
use crate::config::WebhookEndpointConfig;
use async_trait::async_trait;
use hmac::{Hmac, Mac};
use reqwest::{redirect::Policy, Client, StatusCode};
use sha2::Sha256;
use std::{collections::HashSet, time::Duration};
use tokio::sync::{mpsc, oneshot};
use tracing::{error, info, warn};
use uuid::Uuid;

type HmacSha256 = Hmac<Sha256>;

const HEADER_EVENT: &str = "X-Cube-Event";
const HEADER_DELIVERY: &str = "X-Cube-Delivery";
const HEADER_TIMESTAMP: &str = "X-Cube-Timestamp";
const HEADER_NONCE: &str = "X-Cube-Nonce";
const HEADER_SIGNATURE: &str = "X-Cube-Signature-256";
const KNOWN_WEBHOOK_EVENTS: &[&str] = &[
    "sandbox.created",
    "sandbox.deleted",
    "sandbox.paused",
    "sandbox.resumed",
];

enum WorkerMsg {
    Event(LogEvent),
    Flush(oneshot::Sender<()>),
}

#[derive(Clone)]
struct EndpointWorker {
    url: String,
    events: HashSet<String>,
    tx: mpsc::Sender<WorkerMsg>,
}

/// HTTP webhook logger.
#[derive(Clone)]
pub struct HttpLogger {
    endpoints: Vec<EndpointWorker>,
}

impl HttpLogger {
    pub fn new(endpoints: Vec<WebhookEndpointConfig>) -> anyhow::Result<Self> {
        let mut workers = Vec::with_capacity(endpoints.len());

        for endpoint in endpoints {
            let url = endpoint.url.trim().to_string();
            let parsed_url = reqwest::Url::parse(&url)?;
            if !matches!(parsed_url.scheme(), "http" | "https") {
                anyhow::bail!("webhook endpoint URL must use http or https: {url}");
            }

            let client = Client::builder()
                .timeout(Duration::from_secs(endpoint.timeout_secs.max(1)))
                .redirect(Policy::none())
                .build()?;

            let events = endpoint
                .events
                .iter()
                .map(|event| event.trim().to_string())
                .filter(|event| !event.is_empty())
                .collect::<HashSet<_>>();
            for event in &events {
                if !KNOWN_WEBHOOK_EVENTS.contains(&event.as_str()) {
                    warn!(
                        url = %url,
                        event = %event,
                        "webhook endpoint subscribes to an unknown lifecycle event"
                    );
                }
            }
            let queue_capacity = endpoint.queue_capacity.max(1);
            let (tx, rx) = mpsc::channel(queue_capacity);

            let worker_endpoint = endpoint.clone();
            tokio::spawn(async move {
                run_endpoint_worker(client, worker_endpoint, rx).await;
            });

            workers.push(EndpointWorker { url, events, tx });
        }

        Ok(Self { endpoints: workers })
    }
}

#[async_trait]
impl Logger for HttpLogger {
    async fn log(&self, event: LogEvent) {
        for endpoint in &self.endpoints {
            if !endpoint.events.contains(&event.event) {
                continue;
            }

            match endpoint.tx.try_send(WorkerMsg::Event(event.clone())) {
                Ok(()) => {}
                Err(mpsc::error::TrySendError::Full(_)) => {
                    warn!(
                        url = %endpoint.url,
                        event = %event.event,
                        "webhook queue full, dropping event"
                    );
                }
                Err(mpsc::error::TrySendError::Closed(_)) => {
                    error!(
                        url = %endpoint.url,
                        event = %event.event,
                        "webhook worker stopped, dropping event"
                    );
                }
            }
        }
    }

    async fn flush(&self) {
        let flushes = self.endpoints.iter().map(|endpoint| async {
            let (tx, rx) = oneshot::channel();
            if endpoint.tx.send(WorkerMsg::Flush(tx)).await.is_ok() {
                let _ = rx.await;
            }
        });
        futures::future::join_all(flushes).await;
    }

    fn name(&self) -> &'static str {
        "http"
    }
}

async fn run_endpoint_worker(
    client: Client,
    endpoint: WebhookEndpointConfig,
    mut rx: mpsc::Receiver<WorkerMsg>,
) {
    while let Some(msg) = rx.recv().await {
        match msg {
            WorkerMsg::Event(event) => {
                deliver_event(&client, &endpoint, event).await;
            }
            WorkerMsg::Flush(reply) => {
                let _ = reply.send(());
            }
        }
    }
}

async fn deliver_event(client: &Client, endpoint: &WebhookEndpointConfig, event: LogEvent) {
    let body = match serde_json::to_vec(&event) {
        Ok(body) => body,
        Err(e) => {
            error!(
                url = %endpoint.url,
                event = %event.event,
                error = %e,
                "failed to serialize webhook payload"
            );
            return;
        }
    };

    let delivery_id = Uuid::new_v4().to_string();
    let max_retries = endpoint.max_retries;
    let attempts = max_retries.saturating_add(1);

    for attempt in 0..attempts {
        let request = build_request(client, endpoint, &event, &delivery_id, &body);

        match request.send().await {
            Ok(response) if response.status().is_success() => {
                info!(
                    url = %endpoint.url,
                    event = %event.event,
                    delivery_id = %delivery_id,
                    attempt = attempt + 1,
                    "webhook delivered"
                );
                return;
            }
            Ok(response) => {
                let status = response.status();
                let retryable = is_retryable_status(status);
                warn!(
                    url = %endpoint.url,
                    event = %event.event,
                    delivery_id = %delivery_id,
                    status = status.as_u16(),
                    attempt = attempt + 1,
                    retryable,
                    "webhook delivery returned non-success status"
                );
                if !retryable || attempt == max_retries {
                    return;
                }
            }
            Err(e) => {
                warn!(
                    url = %endpoint.url,
                    event = %event.event,
                    delivery_id = %delivery_id,
                    attempt = attempt + 1,
                    error = %e,
                    "webhook delivery request failed"
                );
                if attempt == max_retries {
                    return;
                }
            }
        }

        tokio::time::sleep(retry_delay(endpoint, attempt)).await;
    }
}

fn build_request(
    client: &Client,
    endpoint: &WebhookEndpointConfig,
    event: &LogEvent,
    delivery_id: &str,
    body: &[u8],
) -> reqwest::RequestBuilder {
    let mut request = client
        .post(&endpoint.url)
        .header(reqwest::header::CONTENT_TYPE, "application/json")
        .header(HEADER_EVENT, event.event.as_str())
        .header(HEADER_DELIVERY, delivery_id);

    if let Some(secret) = endpoint
        .secret
        .as_deref()
        .filter(|secret| !secret.is_empty())
    {
        let timestamp = chrono::Utc::now().timestamp_millis().to_string();
        let nonce = Uuid::new_v4().to_string();
        let signature = sign_body(secret, &timestamp, &nonce, &body);
        request = request
            .header(HEADER_TIMESTAMP, timestamp)
            .header(HEADER_NONCE, nonce)
            .header(HEADER_SIGNATURE, signature);
    }

    request.body(body.to_vec())
}

fn sign_body(secret: &str, timestamp: &str, nonce: &str, body: &[u8]) -> String {
    let mut mac =
        HmacSha256::new_from_slice(secret.as_bytes()).expect("HMAC accepts keys of any length");
    mac.update(timestamp.as_bytes());
    mac.update(b".");
    mac.update(nonce.as_bytes());
    mac.update(b".");
    mac.update(body);
    format!("sha256={}", to_hex(&mac.finalize().into_bytes()))
}

fn to_hex(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut out = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        out.push(HEX[(byte >> 4) as usize] as char);
        out.push(HEX[(byte & 0x0f) as usize] as char);
    }
    out
}

fn is_retryable_status(status: StatusCode) -> bool {
    status == StatusCode::REQUEST_TIMEOUT
        || status == StatusCode::TOO_MANY_REQUESTS
        || status.is_server_error()
}

fn retry_delay(endpoint: &WebhookEndpointConfig, attempt: usize) -> Duration {
    let base_ms = endpoint.retry_base_ms.max(1);
    let max_ms = endpoint.retry_max_ms.max(base_ms);
    let factor = 1u64.checked_shl(attempt.min(20) as u32).unwrap_or(u64::MAX);
    Duration::from_millis(base_ms.saturating_mul(factor).min(max_ms))
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::{
        body::Bytes,
        extract::State,
        http::{HeaderMap, StatusCode},
        routing::post,
        Router,
    };
    use serde_json::Value;
    use std::{collections::VecDeque, sync::Arc, time::Instant};
    use tokio::sync::Mutex;

    #[derive(Clone, Debug)]
    struct CapturedRequest {
        headers: HeaderMap,
        body: Vec<u8>,
    }

    #[derive(Clone)]
    struct ReceiverState {
        requests: Arc<Mutex<Vec<CapturedRequest>>>,
        statuses: Arc<Mutex<VecDeque<StatusCode>>>,
    }

    async fn capture(
        State(state): State<ReceiverState>,
        headers: HeaderMap,
        body: Bytes,
    ) -> StatusCode {
        state.requests.lock().await.push(CapturedRequest {
            headers,
            body: body.to_vec(),
        });
        state
            .statuses
            .lock()
            .await
            .pop_front()
            .unwrap_or(StatusCode::OK)
    }

    async fn spawn_receiver(
        statuses: Vec<StatusCode>,
    ) -> (String, Arc<Mutex<Vec<CapturedRequest>>>) {
        let requests = Arc::new(Mutex::new(Vec::new()));
        let state = ReceiverState {
            requests: requests.clone(),
            statuses: Arc::new(Mutex::new(statuses.into())),
        };
        let app = Router::new()
            .route("/webhook", post(capture))
            .with_state(state);
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("bind test receiver");
        let address = listener.local_addr().expect("receiver address");
        tokio::spawn(async move {
            axum::serve(listener, app)
                .await
                .expect("test receiver should run");
        });
        (format!("http://{address}/webhook"), requests)
    }

    fn endpoint(url: String) -> WebhookEndpointConfig {
        WebhookEndpointConfig {
            url,
            events: vec![
                "sandbox.created".to_string(),
                "sandbox.deleted".to_string(),
                "sandbox.paused".to_string(),
                "sandbox.resumed".to_string(),
            ],
            secret: None,
            queue_capacity: 16,
            max_retries: 0,
            retry_base_ms: 1,
            retry_max_ms: 10,
            timeout_secs: 1,
        }
    }

    #[tokio::test]
    async fn delivers_subscribed_event_payload() {
        let (url, requests) = spawn_receiver(vec![StatusCode::OK]).await;
        let logger = HttpLogger::new(vec![endpoint(url)]).expect("logger");
        logger
            .log(
                LogEvent::new(super::super::LogLevel::Info, "sandbox.created")
                    .field("sandbox_id", "sbx-1")
                    .field("template_id", "tpl-1"),
            )
            .await;
        logger.flush().await;

        let requests = requests.lock().await;
        assert_eq!(requests.len(), 1);
        let payload: Value =
            serde_json::from_slice(&requests[0].body).expect("payload should be json");
        assert_eq!(payload["event"], "sandbox.created");
        assert_eq!(payload["sandbox_id"], "sbx-1");
        assert_eq!(payload["template_id"], "tpl-1");
        assert!(payload.get("timestamp").is_some());
    }

    #[tokio::test]
    async fn filters_unsubscribed_events() {
        let (url, requests) = spawn_receiver(vec![StatusCode::OK]).await;
        let mut config = endpoint(url);
        config.events = vec!["sandbox.deleted".to_string()];
        let logger = HttpLogger::new(vec![config]).expect("logger");
        logger
            .log(LogEvent::new(
                super::super::LogLevel::Info,
                "sandbox.created",
            ))
            .await;
        logger.flush().await;

        assert!(requests.lock().await.is_empty());
    }

    #[tokio::test]
    async fn signs_requests_when_secret_is_configured() {
        let (url, requests) = spawn_receiver(vec![StatusCode::OK]).await;
        let mut config = endpoint(url);
        config.secret = Some("test-secret".to_string());
        let logger = HttpLogger::new(vec![config]).expect("logger");
        logger
            .log(LogEvent::new(
                super::super::LogLevel::Info,
                "sandbox.deleted",
            ))
            .await;
        logger.flush().await;

        let requests = requests.lock().await;
        let request = requests.first().expect("request");
        let timestamp = request
            .headers
            .get(HEADER_TIMESTAMP)
            .and_then(|value| value.to_str().ok())
            .expect("timestamp header");
        let nonce = request
            .headers
            .get(HEADER_NONCE)
            .and_then(|value| value.to_str().ok())
            .expect("nonce header");
        let signature = request
            .headers
            .get(HEADER_SIGNATURE)
            .and_then(|value| value.to_str().ok())
            .expect("signature header");
        assert_eq!(
            signature,
            sign_body("test-secret", timestamp, nonce, &request.body)
        );
    }

    #[tokio::test]
    async fn retries_retryable_failures() {
        let (url, requests) =
            spawn_receiver(vec![StatusCode::INTERNAL_SERVER_ERROR, StatusCode::OK]).await;
        let mut config = endpoint(url);
        config.max_retries = 2;
        let logger = HttpLogger::new(vec![config]).expect("logger");
        logger
            .log(LogEvent::new(
                super::super::LogLevel::Info,
                "sandbox.paused",
            ))
            .await;
        logger.flush().await;

        assert_eq!(requests.lock().await.len(), 2);
    }

    #[tokio::test]
    async fn does_not_retry_non_retryable_client_errors() {
        let (url, requests) = spawn_receiver(vec![StatusCode::BAD_REQUEST, StatusCode::OK]).await;
        let mut config = endpoint(url);
        config.max_retries = 2;
        let logger = HttpLogger::new(vec![config]).expect("logger");
        logger
            .log(LogEvent::new(
                super::super::LogLevel::Info,
                "sandbox.resumed",
            ))
            .await;
        logger.flush().await;

        assert_eq!(requests.lock().await.len(), 1);
    }

    #[tokio::test]
    async fn log_returns_without_waiting_for_delivery() {
        let mut config = endpoint("http://127.0.0.1:9/webhook".to_string());
        config.max_retries = 3;
        config.timeout_secs = 1;
        let logger = HttpLogger::new(vec![config]).expect("logger");
        let start = Instant::now();
        logger
            .log(LogEvent::new(
                super::super::LogLevel::Info,
                "sandbox.deleted",
            ))
            .await;

        assert!(start.elapsed() < Duration::from_millis(50));
    }

    #[tokio::test]
    async fn rejects_non_http_endpoint_urls() {
        let config = endpoint("file:///tmp/webhook".to_string());
        let err = match HttpLogger::new(vec![config]) {
            Ok(_) => panic!("invalid scheme should fail"),
            Err(err) => err,
        };

        assert!(err.to_string().contains("http or https"));
    }
}

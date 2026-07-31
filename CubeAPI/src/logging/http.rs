// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Asynchronous HTTP webhook backend for selected lifecycle events.

use super::{LogEvent, Logger};
use crate::config::WebhookEndpointConfig;
use async_trait::async_trait;
use chrono::Utc;
use hmac::{Hmac, Mac};
use reqwest::{StatusCode, Url};
use serde::Serialize;
use sha2::Sha256;
use std::{collections::HashSet, sync::Arc, time::Duration};
use tokio::sync::{mpsc, OwnedSemaphorePermit, Semaphore};
use uuid::Uuid;

const FLUSH_TIMEOUT: Duration = Duration::from_secs(10);
const SUPPORTED_EVENTS: [&str; 4] = [
    "sandbox.created",
    "sandbox.deleted",
    "sandbox.paused",
    "sandbox.resumed",
];

#[derive(Debug, Clone)]
struct Endpoint {
    config: WebhookEndpointConfig,
    subscriptions: HashSet<String>,
}

impl Endpoint {
    fn subscribes_to(&self, event: &str) -> bool {
        self.subscriptions.contains("*") || self.subscriptions.contains(event)
    }
}

#[derive(Debug, Clone, Serialize)]
struct WebhookPayload {
    id: Uuid,
    event: String,
    timestamp: chrono::DateTime<Utc>,
    #[serde(flatten)]
    fields: std::collections::HashMap<String, serde_json::Value>,
}

struct DeliveryJob {
    endpoint: Arc<Endpoint>,
    payload: WebhookPayload,
    body: Vec<u8>,
    _pending_permit: OwnedSemaphorePermit,
}

enum Msg {
    Delivery(DeliveryJob),
    Flush(tokio::sync::oneshot::Sender<()>),
}

/// Fans lifecycle events out to configured HTTP endpoints in a background task.
#[derive(Clone)]
pub struct HttpLogger {
    tx: mpsc::Sender<Msg>,
    endpoints: Arc<Vec<Arc<Endpoint>>>,
    pending: Arc<Semaphore>,
}

impl HttpLogger {
    pub fn new(
        client: reqwest::Client,
        configs: Vec<WebhookEndpointConfig>,
        queue_capacity: usize,
        max_concurrency: usize,
    ) -> anyhow::Result<Self> {
        validate_configs(&configs)?;
        anyhow::ensure!(
            queue_capacity > 0,
            "webhook queue capacity must be greater than zero"
        );
        anyhow::ensure!(
            max_concurrency > 0,
            "webhook max concurrency must be greater than zero"
        );

        let endpoints = Arc::new(
            configs
                .into_iter()
                .map(|config| {
                    Arc::new(Endpoint {
                        subscriptions: config.events.iter().cloned().collect(),
                        config,
                    })
                })
                .collect::<Vec<_>>(),
        );
        let (tx, mut rx) = mpsc::channel(queue_capacity);
        let semaphore = Arc::new(Semaphore::new(max_concurrency));
        let pending = Arc::new(Semaphore::new(queue_capacity));

        tokio::spawn(async move {
            let mut deliveries = tokio::task::JoinSet::new();
            while let Some(msg) = rx.recv().await {
                match msg {
                    Msg::Delivery(job) => {
                        let client = client.clone();
                        let semaphore = semaphore.clone();
                        deliveries.spawn(async move {
                            deliver_with_retry(client, semaphore, job).await;
                        });
                        while deliveries.try_join_next().is_some() {}
                    }
                    Msg::Flush(reply) => {
                        while deliveries.join_next().await.is_some() {}
                        let _ = reply.send(());
                    }
                }
            }
            while deliveries.join_next().await.is_some() {}
        });

        Ok(Self {
            tx,
            endpoints,
            pending,
        })
    }
}

#[async_trait]
impl Logger for HttpLogger {
    async fn log(&self, event: LogEvent) {
        if !SUPPORTED_EVENTS.contains(&event.event.as_str()) {
            return;
        }

        let payload = WebhookPayload {
            id: Uuid::new_v4(),
            event: event.event,
            timestamp: event.timestamp,
            fields: event.fields,
        };
        let body = match serde_json::to_vec(&payload) {
            Ok(body) => body,
            Err(error) => {
                tracing::error!(error = %error, "failed to serialize webhook event");
                return;
            }
        };

        for endpoint in self
            .endpoints
            .iter()
            .filter(|endpoint| endpoint.subscribes_to(&payload.event))
        {
            let permit = match self.pending.clone().try_acquire_owned() {
                Ok(permit) => permit,
                Err(error) => {
                    tracing::warn!(
                        endpoint = %endpoint.config.name,
                        event = %payload.event,
                        event_id = %payload.id,
                        error = %error,
                        "webhook pending limit reached; dropping delivery"
                    );
                    continue;
                }
            };
            let job = DeliveryJob {
                endpoint: endpoint.clone(),
                payload: payload.clone(),
                body: body.clone(),
                _pending_permit: permit,
            };
            if let Err(error) = self.tx.try_send(Msg::Delivery(job)) {
                tracing::warn!(
                    endpoint = %endpoint.config.name,
                    event = %payload.event,
                    event_id = %payload.id,
                    error = %error,
                    "webhook queue unavailable; dropping delivery"
                );
            }
        }
    }

    async fn flush(&self) {
        let (tx, rx) = tokio::sync::oneshot::channel();
        if self.tx.send(Msg::Flush(tx)).await.is_ok()
            && tokio::time::timeout(FLUSH_TIMEOUT, rx).await.is_err()
        {
            tracing::warn!("timed out waiting for pending webhook deliveries during shutdown");
        }
    }

    fn name(&self) -> &'static str {
        "http"
    }
}

fn validate_configs(configs: &[WebhookEndpointConfig]) -> anyhow::Result<()> {
    let mut names = HashSet::new();
    for config in configs {
        anyhow::ensure!(
            !config.name.trim().is_empty(),
            "webhook endpoint name cannot be empty"
        );
        anyhow::ensure!(
            names.insert(config.name.clone()),
            "duplicate webhook endpoint name: {}",
            config.name
        );
        let url = Url::parse(&config.url).map_err(|error| {
            anyhow::anyhow!("invalid webhook URL for {}: {}", config.name, error)
        })?;
        anyhow::ensure!(
            matches!(url.scheme(), "http" | "https"),
            "webhook endpoint {} must use HTTP or HTTPS",
            config.name
        );
        anyhow::ensure!(
            !config.events.is_empty(),
            "webhook endpoint {} must subscribe to at least one event",
            config.name
        );
        for event in &config.events {
            anyhow::ensure!(
                event == "*" || SUPPORTED_EVENTS.contains(&event.as_str()),
                "unsupported webhook event '{}' for endpoint {}",
                event,
                config.name
            );
        }
        anyhow::ensure!(
            config.timeout_secs > 0,
            "webhook timeout must be greater than zero for {}",
            config.name
        );
        anyhow::ensure!(
            config
                .secret
                .as_ref()
                .is_none_or(|secret| !secret.trim().is_empty()),
            "webhook signing secret cannot be empty for {}",
            config.name
        );
    }
    Ok(())
}

async fn deliver_with_retry(client: reqwest::Client, semaphore: Arc<Semaphore>, job: DeliveryJob) {
    let max_attempts = job.endpoint.config.max_retries.saturating_add(1);
    for attempt in 1..=max_attempts {
        let permit = match semaphore.acquire().await {
            Ok(permit) => permit,
            Err(_) => return,
        };
        let timestamp = Utc::now().timestamp();
        let mut request = client
            .post(&job.endpoint.config.url)
            .header("Content-Type", "application/json")
            .header("X-Cube-Webhook-Id", job.payload.id.to_string())
            .header("X-Cube-Webhook-Timestamp", timestamp.to_string())
            .timeout(Duration::from_secs(job.endpoint.config.timeout_secs))
            .body(job.body.clone());

        if let Some(secret) = job
            .endpoint
            .config
            .secret
            .as_deref()
            .filter(|s| !s.is_empty())
        {
            request = request.header(
                "X-Cube-Webhook-Signature",
                format!("sha256={}", sign(secret, timestamp, &job.body)),
            );
        }

        let response = request.send().await;
        drop(permit);
        match response {
            Ok(response) if response.status().is_success() => {
                tracing::info!(
                    endpoint = %job.endpoint.config.name,
                    event = %job.payload.event,
                    event_id = %job.payload.id,
                    attempt,
                    "webhook delivered"
                );
                return;
            }
            Ok(response) if !is_retryable_status(response.status()) => {
                tracing::error!(
                    endpoint = %job.endpoint.config.name,
                    event = %job.payload.event,
                    event_id = %job.payload.id,
                    status = %response.status(),
                    attempt,
                    "webhook delivery rejected without retry"
                );
                return;
            }
            Ok(response) => tracing::warn!(
                endpoint = %job.endpoint.config.name,
                event = %job.payload.event,
                event_id = %job.payload.id,
                status = %response.status(),
                attempt,
                "webhook delivery failed"
            ),
            Err(error) => tracing::warn!(
                endpoint = %job.endpoint.config.name,
                event = %job.payload.event,
                event_id = %job.payload.id,
                attempt,
                error = %error,
                "webhook delivery failed"
            ),
        }

        if attempt < max_attempts {
            tokio::time::sleep(retry_delay(attempt, job.payload.id)).await;
        }
    }

    tracing::error!(
        endpoint = %job.endpoint.config.name,
        event = %job.payload.event,
        event_id = %job.payload.id,
        attempts = max_attempts,
        "webhook delivery exhausted retries"
    );
}

fn is_retryable_status(status: StatusCode) -> bool {
    status == StatusCode::REQUEST_TIMEOUT
        || status == StatusCode::TOO_MANY_REQUESTS
        || status.is_server_error()
}

fn retry_delay(attempt: u32, event_id: Uuid) -> Duration {
    let exponential_ms = 1_000u64.saturating_mul(1u64 << attempt.saturating_sub(1).min(6));
    let jitter_ms = u64::from(event_id.as_bytes()[0]) * 2;
    Duration::from_millis((exponential_ms + jitter_ms).min(60_000))
}

fn sign(secret: &str, timestamp: i64, body: &[u8]) -> String {
    let mut mac =
        Hmac::<Sha256>::new_from_slice(secret.as_bytes()).expect("HMAC accepts keys of any size");
    mac.update(timestamp.to_string().as_bytes());
    mac.update(b".");
    mac.update(body);
    hex::encode(mac.finalize().into_bytes())
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::{
        body::{to_bytes, Body},
        extract::State,
        http::{Request, StatusCode},
        routing::post,
        Router,
    };
    use std::sync::{
        atomic::{AtomicUsize, Ordering},
        Arc,
    };
    use tokio::{net::TcpListener, sync::Mutex};

    #[derive(Clone, Debug)]
    struct ReceivedRequest {
        body: Vec<u8>,
        timestamp: String,
        signature: Option<String>,
    }

    #[derive(Clone)]
    struct ReceiverState {
        requests: Arc<Mutex<Vec<ReceivedRequest>>>,
        failures_remaining: Arc<AtomicUsize>,
        delay: Duration,
    }

    async fn receive(State(state): State<ReceiverState>, request: Request<Body>) -> StatusCode {
        if !state.delay.is_zero() {
            tokio::time::sleep(state.delay).await;
        }
        let timestamp = request
            .headers()
            .get("X-Cube-Webhook-Timestamp")
            .and_then(|value| value.to_str().ok())
            .unwrap_or_default()
            .to_string();
        let signature = request
            .headers()
            .get("X-Cube-Webhook-Signature")
            .and_then(|value| value.to_str().ok())
            .map(str::to_string);
        let body = to_bytes(request.into_body(), usize::MAX)
            .await
            .unwrap()
            .to_vec();
        state.requests.lock().await.push(ReceivedRequest {
            body,
            timestamp,
            signature,
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
            StatusCode::NO_CONTENT
        }
    }

    async fn spawn_receiver(failures: usize, delay: Duration) -> (String, ReceiverState) {
        let state = ReceiverState {
            requests: Arc::new(Mutex::new(Vec::new())),
            failures_remaining: Arc::new(AtomicUsize::new(failures)),
            delay,
        };
        let app = Router::new()
            .route("/webhook", post(receive))
            .with_state(state.clone());
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
        (format!("http://{address}/webhook"), state)
    }

    fn endpoint(url: String, events: &[&str]) -> WebhookEndpointConfig {
        WebhookEndpointConfig {
            name: "test".to_string(),
            url,
            events: events.iter().map(|event| event.to_string()).collect(),
            secret: None,
            timeout_secs: 2,
            max_retries: 0,
        }
    }

    #[tokio::test]
    async fn delivers_subscribed_event_with_payload_and_signature() {
        let (url, receiver) = spawn_receiver(0, Duration::ZERO).await;
        let mut config = endpoint(url, &["sandbox.created"]);
        config.secret = Some("test-secret".to_string());
        let logger = HttpLogger::new(reqwest::Client::new(), vec![config], 16, 2).unwrap();

        logger
            .log(
                LogEvent::new(super::super::LogLevel::Info, "sandbox.created")
                    .field("sandbox_id", "sandbox-1")
                    .field("template_id", "template-1"),
            )
            .await;
        logger.flush().await;

        let requests = receiver.requests.lock().await;
        assert_eq!(requests.len(), 1);
        let request = &requests[0];
        let payload: serde_json::Value = serde_json::from_slice(&request.body).unwrap();
        assert_eq!(payload["event"], "sandbox.created");
        assert_eq!(payload["sandbox_id"], "sandbox-1");
        assert_eq!(payload["template_id"], "template-1");
        assert!(payload["id"].as_str().is_some());

        let timestamp = request.timestamp.parse::<i64>().unwrap();
        assert_eq!(
            request.signature.as_deref(),
            Some(format!("sha256={}", sign("test-secret", timestamp, &request.body)).as_str())
        );
    }

    #[tokio::test]
    async fn ignores_unsubscribed_and_non_webhook_events() {
        let (url, receiver) = spawn_receiver(0, Duration::ZERO).await;
        let logger = HttpLogger::new(
            reqwest::Client::new(),
            vec![endpoint(url, &["sandbox.deleted"])],
            16,
            2,
        )
        .unwrap();

        logger
            .log(LogEvent::new(
                super::super::LogLevel::Info,
                "sandbox.created",
            ))
            .await;
        logger
            .log(LogEvent::new(super::super::LogLevel::Info, "api.request"))
            .await;
        logger.flush().await;

        assert!(receiver.requests.lock().await.is_empty());
    }

    #[tokio::test]
    async fn retries_server_errors_with_the_same_event() {
        let (url, receiver) = spawn_receiver(1, Duration::ZERO).await;
        let mut config = endpoint(url, &["sandbox.paused"]);
        config.max_retries = 1;
        let logger = HttpLogger::new(reqwest::Client::new(), vec![config], 16, 1).unwrap();

        logger
            .log(
                LogEvent::new(super::super::LogLevel::Info, "sandbox.paused")
                    .field("sandbox_id", "sandbox-1"),
            )
            .await;
        logger.flush().await;

        let requests = receiver.requests.lock().await;
        assert_eq!(requests.len(), 2);
        assert_eq!(requests[0].body, requests[1].body);
    }

    #[tokio::test]
    async fn log_does_not_wait_for_the_receiver() {
        let (url, _receiver) = spawn_receiver(0, Duration::from_millis(250)).await;
        let logger = HttpLogger::new(
            reqwest::Client::new(),
            vec![endpoint(url, &["sandbox.resumed"])],
            16,
            1,
        )
        .unwrap();

        tokio::time::timeout(
            Duration::from_millis(50),
            logger.log(LogEvent::new(
                super::super::LogLevel::Info,
                "sandbox.resumed",
            )),
        )
        .await
        .expect("log must only enqueue the webhook delivery");
        logger.flush().await;
    }

    #[tokio::test]
    async fn retry_backoff_does_not_block_a_healthy_endpoint() {
        let (failing_url, failing_receiver) = spawn_receiver(1, Duration::ZERO).await;
        let (healthy_url, healthy_receiver) = spawn_receiver(0, Duration::ZERO).await;
        let mut failing = endpoint(failing_url, &["sandbox.created"]);
        failing.name = "failing".to_string();
        failing.max_retries = 1;
        let mut healthy = endpoint(healthy_url, &["sandbox.created"]);
        healthy.name = "healthy".to_string();
        let logger =
            HttpLogger::new(reqwest::Client::new(), vec![failing, healthy], 16, 1).unwrap();

        logger
            .log(LogEvent::new(
                super::super::LogLevel::Info,
                "sandbox.created",
            ))
            .await;

        tokio::time::timeout(Duration::from_millis(500), async {
            loop {
                if !healthy_receiver.requests.lock().await.is_empty() {
                    break;
                }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .expect("healthy endpoint must not wait for another endpoint's retry backoff");
        logger.flush().await;

        assert_eq!(failing_receiver.requests.lock().await.len(), 2);
        assert_eq!(healthy_receiver.requests.lock().await.len(), 1);
    }

    #[test]
    fn validates_endpoint_configuration() {
        let mut config = endpoint("ftp://example.com/hook".to_string(), &["sandbox.created"]);
        assert!(validate_configs(&[config.clone()]).is_err());

        config.url = "https://example.com/hook".to_string();
        config.events = vec!["unknown.event".to_string()];
        assert!(validate_configs(&[config]).is_err());

        let mut config = endpoint("https://example.com/hook".to_string(), &["*"]);
        config.secret = Some("  ".to_string());
        assert!(validate_configs(&[config]).is_err());
    }

    #[test]
    fn rejects_zero_queue_or_concurrency_limits() {
        let config = endpoint("https://example.com/hook".to_string(), &["*"]);
        assert!(HttpLogger::new(reqwest::Client::new(), vec![config.clone()], 0, 1).is_err());
        assert!(HttpLogger::new(reqwest::Client::new(), vec![config], 1, 0).is_err());
    }

    #[test]
    fn classifies_retryable_statuses() {
        assert!(is_retryable_status(StatusCode::REQUEST_TIMEOUT));
        assert!(is_retryable_status(StatusCode::TOO_MANY_REQUESTS));
        assert!(is_retryable_status(StatusCode::BAD_GATEWAY));
        assert!(!is_retryable_status(StatusCode::BAD_REQUEST));
    }
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! HTTP webhook log backend.
//!
//! Sends selected structured log events as JSON POST requests to configured
//! endpoints. `log()` only performs event filtering plus a bounded channel
//! send; delivery, retry, timeout handling, and HMAC signing all happen in the
//! background worker.

use super::{LogEvent, Logger};
use async_trait::async_trait;
use hmac::{Hmac, Mac};
use reqwest::Client;
use sha2::Sha256;
use std::{sync::Arc, time::Duration};
use tokio::sync::{mpsc, oneshot};
use tracing::{error, warn};
use uuid::Uuid;

type HmacSha256 = Hmac<Sha256>;

/// Configuration for the HTTP webhook backend.
#[derive(Debug, Clone)]
#[allow(dead_code)]
pub struct HttpLoggerConfig {
    /// Full URL to POST batches to, e.g. `"http://log-ingest.internal/api/logs"`.
    pub url: String,
    /// Max events per batch (default: 100).
    pub batch_size: usize,
    /// Flush interval in seconds even if batch is not full (default: 5).
    pub flush_interval_secs: u64,
}

impl Default for HttpLoggerConfig {
    fn default() -> Self {
        Self {
            url: String::new(),
            batch_size: 100,
            flush_interval_secs: 5,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HttpWebhookEndpoint {
    pub url: String,
    pub events: Vec<String>,
    pub secret: Option<String>,
}

impl From<crate::config::WebhookEndpointConfig> for HttpWebhookEndpoint {
    fn from(value: crate::config::WebhookEndpointConfig) -> Self {
        Self {
            url: value.url,
            events: value.events,
            secret: value.secret,
        }
    }
}

#[derive(Debug, Clone)]
pub struct HttpWebhookOptions {
    pub queue_capacity: usize,
    pub request_timeout: Duration,
    pub max_attempts: usize,
    pub initial_backoff: Duration,
}

impl Default for HttpWebhookOptions {
    fn default() -> Self {
        Self {
            queue_capacity: 1024,
            request_timeout: Duration::from_secs(5),
            max_attempts: 3,
            initial_backoff: Duration::from_millis(200),
        }
    }
}

enum Msg {
    Event(LogEvent),
    Flush(oneshot::Sender<()>),
}

/// HTTP webhook log backend.
#[derive(Clone)]
pub struct HttpLogger {
    tx: mpsc::Sender<Msg>,
    endpoints: Arc<Vec<HttpWebhookEndpoint>>,
}

impl HttpLogger {
    /// Create an `HttpLogger` from the legacy single-URL config.
    #[allow(dead_code)]
    pub fn new(config: HttpLoggerConfig) -> Self {
        let endpoints = if config.url.trim().is_empty() {
            Vec::new()
        } else {
            vec![HttpWebhookEndpoint {
                url: config.url.trim().to_string(),
                events: Vec::new(),
                secret: None,
            }]
        };
        Self::from_endpoints(Client::new(), endpoints, HttpWebhookOptions::default())
    }

    pub fn from_endpoints(
        client: Client,
        endpoints: Vec<HttpWebhookEndpoint>,
        options: HttpWebhookOptions,
    ) -> Self {
        let endpoints = Arc::new(endpoints);
        let (tx, rx) = mpsc::channel(options.queue_capacity.max(1));
        spawn_worker(client, endpoints.clone(), options, rx);
        Self { tx, endpoints }
    }

    fn has_matching_endpoint(&self, event: &str) -> bool {
        self.endpoints
            .iter()
            .any(|endpoint| endpoint_subscribes(endpoint, event))
    }
}

#[async_trait]
impl Logger for HttpLogger {
    async fn log(&self, event: LogEvent) {
        if !self.has_matching_endpoint(&event.event) {
            return;
        }
        if let Err(err) = self.tx.try_send(Msg::Event(event)) {
            warn!(error = %err, "HttpLogger: webhook queue full or closed, dropping event");
        }
    }

    async fn flush(&self) {
        let (tx, rx) = oneshot::channel();
        if self.tx.send(Msg::Flush(tx)).await.is_ok() {
            let _ = rx.await;
        }
    }

    fn name(&self) -> &'static str {
        "http"
    }
}

fn spawn_worker(
    client: Client,
    endpoints: Arc<Vec<HttpWebhookEndpoint>>,
    options: HttpWebhookOptions,
    mut rx: mpsc::Receiver<Msg>,
) {
    tokio::spawn(async move {
        while let Some(msg) = rx.recv().await {
            match msg {
                Msg::Event(event) => {
                    let deliveries = endpoints
                        .iter()
                        .filter(|endpoint| endpoint_subscribes(endpoint, &event.event))
                        .cloned()
                        .map(|endpoint| {
                            deliver_with_retries(
                                client.clone(),
                                endpoint,
                                event.clone(),
                                options.clone(),
                            )
                        });
                    futures::future::join_all(deliveries).await;
                }
                Msg::Flush(reply) => {
                    let _ = reply.send(());
                }
            }
        }
    });
}

fn endpoint_subscribes(endpoint: &HttpWebhookEndpoint, event: &str) -> bool {
    endpoint.events.is_empty()
        || endpoint
            .events
            .iter()
            .any(|subscribed| subscribed == "*" || subscribed == event)
}

async fn deliver_with_retries(
    client: Client,
    endpoint: HttpWebhookEndpoint,
    event: LogEvent,
    options: HttpWebhookOptions,
) {
    let max_attempts = options.max_attempts.max(1);
    for attempt in 1..=max_attempts {
        match deliver_once(&client, &endpoint, &event, options.request_timeout).await {
            Ok(()) => return,
            Err(err) if attempt < max_attempts => {
                warn!(
                    error = %err,
                    endpoint = %endpoint.url,
                    event = %event.event,
                    attempt,
                    max_attempts,
                    "HttpLogger: webhook delivery failed, retrying"
                );
                tokio::time::sleep(backoff_delay(options.initial_backoff, attempt)).await;
            }
            Err(err) => {
                error!(
                    error = %err,
                    endpoint = %endpoint.url,
                    event = %event.event,
                    attempts = max_attempts,
                    "HttpLogger: webhook delivery failed permanently"
                );
            }
        }
    }
}

async fn deliver_once(
    client: &Client,
    endpoint: &HttpWebhookEndpoint,
    event: &LogEvent,
    timeout: Duration,
) -> anyhow::Result<()> {
    let body = serde_json::to_vec(event)?;
    let delivery_id = Uuid::new_v4().to_string();
    let mut request = client
        .post(&endpoint.url)
        .timeout(timeout)
        .header(reqwest::header::CONTENT_TYPE, "application/json")
        .header(reqwest::header::USER_AGENT, "CubeSandbox-CubeAPI-Webhook/1")
        .header("X-Cube-Event", &event.event)
        .header("X-Cube-Delivery", delivery_id)
        .body(body.clone());

    if let Some(secret) = endpoint.secret.as_deref().filter(|s| !s.is_empty()) {
        request = request.header("X-Cube-Signature", signature_header(secret, &body)?);
    }

    let response = request.send().await?;
    if response.status().is_success() {
        return Ok(());
    }

    let status = response.status();
    let text = response.text().await.unwrap_or_default();
    anyhow::bail!("webhook endpoint returned {status}: {text}");
}

fn signature_header(secret: &str, body: &[u8]) -> anyhow::Result<String> {
    let mut mac = HmacSha256::new_from_slice(secret.as_bytes())?;
    mac.update(body);
    Ok(format!(
        "sha256={}",
        hex::encode(mac.finalize().into_bytes())
    ))
}

fn backoff_delay(initial: Duration, completed_attempt: usize) -> Duration {
    let exponent = completed_attempt.saturating_sub(1).min(20) as u32;
    initial.saturating_mul(2_u32.saturating_pow(exponent))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::logging::{LogEvent, LogLevel, Logger};
    use axum::{body::Bytes, extract::State, http::HeaderMap, routing::post, Router};
    use std::sync::{
        atomic::{AtomicUsize, Ordering},
        Arc,
    };
    use tokio::sync::{mpsc, Mutex};

    #[tokio::test]
    async fn logger_posts_lifecycle_events_as_json() {
        let (tx, mut rx) = mpsc::channel::<Bytes>(1);
        let app = Router::new()
            .route(
                "/webhook",
                post(
                    |State(tx): State<Arc<Mutex<mpsc::Sender<Bytes>>>>, body: Bytes| async move {
                        let _ = tx.lock().await.send(body).await;
                        axum::http::StatusCode::NO_CONTENT
                    },
                ),
            )
            .with_state(Arc::new(Mutex::new(tx)));
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}/webhook", listener.local_addr().unwrap());
        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });

        let logger = HttpLogger::new(HttpLoggerConfig {
            url,
            batch_size: 1,
            flush_interval_secs: 1,
        });

        logger
            .log(
                LogEvent::new(LogLevel::Info, "sandbox.created")
                    .field("sandbox_id", "sb-test")
                    .field("template_id", "tpl-test"),
            )
            .await;
        logger.flush().await;

        let body = tokio::time::timeout(std::time::Duration::from_secs(1), rx.recv())
            .await
            .expect("webhook receiver should get a request")
            .expect("webhook body should be present");
        let payload: serde_json::Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(payload["event"], "sandbox.created");
        assert_eq!(payload["sandbox_id"], "sb-test");
        assert_eq!(payload["template_id"], "tpl-test");
        assert!(payload["timestamp"].as_str().is_some());
    }

    #[tokio::test]
    async fn logger_sends_hmac_signature_when_secret_is_configured() {
        let (tx, mut rx) = mpsc::channel::<(HeaderMap, Bytes)>(1);
        let app = Router::new()
            .route(
                "/webhook",
                post(
                    |State(tx): State<Arc<Mutex<mpsc::Sender<(HeaderMap, Bytes)>>>>,
                     headers: HeaderMap,
                     body: Bytes| async move {
                        let _ = tx.lock().await.send((headers, body)).await;
                        axum::http::StatusCode::NO_CONTENT
                    },
                ),
            )
            .with_state(Arc::new(Mutex::new(tx)));
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}/webhook", listener.local_addr().unwrap());
        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });

        let logger = HttpLogger::from_endpoints(
            Client::new(),
            vec![HttpWebhookEndpoint {
                url,
                events: vec!["sandbox.deleted".to_string()],
                secret: Some("secret".to_string()),
            }],
            HttpWebhookOptions {
                queue_capacity: 8,
                request_timeout: Duration::from_secs(1),
                max_attempts: 1,
                initial_backoff: Duration::from_millis(1),
            },
        );

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.deleted").field("sandbox_id", "sb-test"))
            .await;
        logger.flush().await;

        let (headers, body) = tokio::time::timeout(Duration::from_secs(1), rx.recv())
            .await
            .expect("webhook receiver should get a request")
            .expect("webhook request should be present");
        let expected = signature_header("secret", &body).unwrap();
        assert_eq!(
            headers.get("X-Cube-Signature").unwrap().to_str().unwrap(),
            expected
        );
        assert_eq!(
            headers.get("X-Cube-Event").unwrap().to_str().unwrap(),
            "sandbox.deleted"
        );
    }

    #[tokio::test]
    async fn logger_retries_failed_delivery() {
        let attempts = Arc::new(AtomicUsize::new(0));
        let app = Router::new()
            .route(
                "/webhook",
                post(|State(attempts): State<Arc<AtomicUsize>>| async move {
                    if attempts.fetch_add(1, Ordering::SeqCst) == 0 {
                        axum::http::StatusCode::INTERNAL_SERVER_ERROR
                    } else {
                        axum::http::StatusCode::NO_CONTENT
                    }
                }),
            )
            .with_state(attempts.clone());
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}/webhook", listener.local_addr().unwrap());
        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });

        let logger = HttpLogger::from_endpoints(
            Client::new(),
            vec![HttpWebhookEndpoint {
                url,
                events: vec!["sandbox.paused".to_string()],
                secret: None,
            }],
            HttpWebhookOptions {
                queue_capacity: 8,
                request_timeout: Duration::from_secs(1),
                max_attempts: 2,
                initial_backoff: Duration::from_millis(1),
            },
        );

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.paused").field("sandbox_id", "sb-test"))
            .await;
        logger.flush().await;

        assert_eq!(attempts.load(Ordering::SeqCst), 2);
    }

    #[test]
    fn endpoint_subscription_matches_exact_events_and_wildcards() {
        let endpoint = HttpWebhookEndpoint {
            url: "http://example.test".to_string(),
            events: vec!["sandbox.created".to_string()],
            secret: None,
        };
        assert!(endpoint_subscribes(&endpoint, "sandbox.created"));
        assert!(!endpoint_subscribes(&endpoint, "sandbox.deleted"));

        let wildcard = HttpWebhookEndpoint {
            events: vec!["*".to_string()],
            ..endpoint
        };
        assert!(endpoint_subscribes(&wildcard, "sandbox.deleted"));
    }
}

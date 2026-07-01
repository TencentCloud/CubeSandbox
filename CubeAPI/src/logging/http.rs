// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! HTTP webhook log backend.
//!
//! This backend forwards selected structured events as JSON POST requests.
//! `log()` only enqueues work; a background task handles HTTP delivery and
//! retries so sandbox lifecycle paths are not blocked by receiver latency.

use std::{
    collections::{HashMap, HashSet},
    time::Duration,
};

use async_trait::async_trait;
use hmac::{Hmac, Mac};
use reqwest::header::{HeaderMap, HeaderValue, CONTENT_TYPE};
use serde::Serialize;
use sha2::Sha256;
use tokio::sync::{mpsc, oneshot};
use tracing::{debug, error, warn};

use crate::config::WebhookEndpointConfig;

use super::{LogEvent, Logger};

type HmacSha256 = Hmac<Sha256>;

const DEFAULT_QUEUE_SIZE: usize = 1024;
const HEADER_EVENT: &str = "X-Cube-Event";
const HEADER_TIMESTAMP: &str = "X-Cube-Timestamp";
const HEADER_SIGNATURE: &str = "X-Cube-Signature";

enum Msg {
    Event(LogEvent),
    Flush(oneshot::Sender<()>),
}

#[derive(Debug, Clone)]
struct Endpoint {
    url: String,
    events: HashSet<String>,
    secret: Option<String>,
    timeout: Duration,
    max_retries: usize,
}

impl Endpoint {
    fn from_config(config: WebhookEndpointConfig) -> Option<Self> {
        let url = config.url.trim().to_string();
        if url.is_empty() {
            warn!("WebhookLogger: skipping endpoint with empty url");
            return None;
        }

        Some(Self {
            url,
            events: config
                .events
                .into_iter()
                .map(|event| event.trim().to_string())
                .filter(|event| !event.is_empty())
                .collect(),
            secret: config.secret.filter(|secret| !secret.is_empty()),
            timeout: Duration::from_secs(config.timeout_secs.max(1)),
            max_retries: config.max_retries,
        })
    }

    fn subscribes(&self, event: &LogEvent) -> bool {
        self.events.is_empty() || self.events.contains(&event.event)
    }
}

#[derive(Clone)]
pub struct WebhookLogger {
    tx: mpsc::Sender<Msg>,
}

impl WebhookLogger {
    pub fn new(configs: Vec<WebhookEndpointConfig>, client: reqwest::Client) -> Option<Self> {
        Self::with_queue_size(configs, client, DEFAULT_QUEUE_SIZE)
    }

    fn with_queue_size(
        configs: Vec<WebhookEndpointConfig>,
        client: reqwest::Client,
        queue_size: usize,
    ) -> Option<Self> {
        let endpoints: Vec<_> = configs
            .into_iter()
            .filter_map(Endpoint::from_config)
            .collect();
        if endpoints.is_empty() {
            return None;
        }

        let (tx, mut rx) = mpsc::channel::<Msg>(queue_size.max(1));
        tokio::spawn(async move {
            while let Some(msg) = rx.recv().await {
                match msg {
                    Msg::Event(event) => {
                        for endpoint in endpoints.iter().filter(|e| e.subscribes(&event)) {
                            let endpoint = endpoint.clone();
                            let client = client.clone();
                            let event = event.clone();
                            tokio::spawn(async move {
                                deliver_with_retries(client, endpoint, event).await;
                            });
                        }
                    }
                    Msg::Flush(reply) => {
                        let _ = reply.send(());
                    }
                }
            }
        });

        Some(Self { tx })
    }
}

#[async_trait]
impl Logger for WebhookLogger {
    async fn log(&self, event: LogEvent) {
        if let Err(err) = self.tx.try_send(Msg::Event(event)) {
            warn!(error = %err, "WebhookLogger: queue full or closed, dropping event");
        }
    }

    async fn flush(&self) {
        let (tx, rx) = oneshot::channel();
        if self.tx.send(Msg::Flush(tx)).await.is_ok() {
            let _ = rx.await;
        }
    }

    fn name(&self) -> &'static str {
        "webhook"
    }
}

#[derive(Serialize)]
struct WebhookPayload<'a> {
    event: &'a str,
    timestamp: String,
    #[serde(flatten)]
    fields: &'a HashMap<String, serde_json::Value>,
}

async fn deliver_with_retries(client: reqwest::Client, endpoint: Endpoint, event: LogEvent) {
    let mut delay = Duration::from_millis(200);
    for attempt in 0..=endpoint.max_retries {
        match deliver_once(&client, &endpoint, &event).await {
            Ok(()) => return,
            Err(err) if attempt < endpoint.max_retries => {
                warn!(
                    url = %endpoint.url,
                    event = %event.event,
                    attempt = attempt + 1,
                    error = %err,
                    "WebhookLogger: delivery failed, retrying"
                );
                tokio::time::sleep(delay).await;
                delay = delay.saturating_mul(2);
            }
            Err(err) => {
                error!(
                    url = %endpoint.url,
                    event = %event.event,
                    attempts = endpoint.max_retries + 1,
                    error = %err,
                    "WebhookLogger: delivery failed"
                );
            }
        }
    }
}

async fn deliver_once(
    client: &reqwest::Client,
    endpoint: &Endpoint,
    event: &LogEvent,
) -> anyhow::Result<()> {
    let payload = WebhookPayload {
        event: &event.event,
        timestamp: event.timestamp.to_rfc3339(),
        fields: &event.fields,
    };
    let body = serde_json::to_vec(&payload)?;
    let timestamp = event.timestamp.timestamp().to_string();
    let mut headers = HeaderMap::new();
    headers.insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
    headers.insert(HEADER_EVENT, HeaderValue::from_str(&event.event)?);
    headers.insert(HEADER_TIMESTAMP, HeaderValue::from_str(&timestamp)?);

    if let Some(secret) = endpoint.secret.as_deref() {
        headers.insert(
            HEADER_SIGNATURE,
            HeaderValue::from_str(&signature(secret, &timestamp, &body))?,
        );
    }

    let resp = client
        .post(&endpoint.url)
        .headers(headers)
        .timeout(endpoint.timeout)
        .body(body)
        .send()
        .await?;

    if !resp.status().is_success() {
        anyhow::bail!("receiver returned {}", resp.status());
    }

    debug!(url = %endpoint.url, event = %event.event, "WebhookLogger: delivered event");
    Ok(())
}

fn signature(secret: &str, timestamp: &str, body: &[u8]) -> String {
    let mut mac =
        HmacSha256::new_from_slice(secret.as_bytes()).expect("HMAC accepts keys of any length");
    mac.update(timestamp.as_bytes());
    mac.update(b".");
    mac.update(body);
    format!("sha256={}", hex::encode(mac.finalize().into_bytes()))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::logging::{LogLevel, Logger};
    use axum::{extract::State, http::HeaderMap, routing::post, Json, Router};
    use serde_json::Value;
    use std::{
        net::SocketAddr,
        sync::{
            atomic::{AtomicUsize, Ordering},
            Arc,
        },
    };
    use tokio::net::TcpListener;

    #[tokio::test]
    async fn sends_subscribed_event_with_signature() {
        let received = Arc::new(tokio::sync::Mutex::new(None));
        let received_state = received.clone();
        let addr = spawn_receiver(move |headers, body| {
            let received = received_state.clone();
            async move {
                *received.lock().await = Some((headers, body));
            }
        })
        .await;

        let logger = WebhookLogger::with_queue_size(
            vec![WebhookEndpointConfig {
                url: format!("http://{addr}/hook"),
                events: vec!["sandbox.created".to_string()],
                secret: Some("secret".to_string()),
                timeout_secs: 2,
                max_retries: 0,
            }],
            test_client(),
            4,
        )
        .expect("logger");

        logger
            .log(
                LogEvent::new(LogLevel::Info, "sandbox.created")
                    .field("sandbox_id", "sbx-1")
                    .field("template_id", "tpl-1"),
            )
            .await;

        wait_until(|| async { received.lock().await.is_some() }).await;
        let got = received.lock().await.clone().expect("webhook received");
        assert_eq!(
            got.0.get(HEADER_EVENT).and_then(|v| v.to_str().ok()),
            Some("sandbox.created")
        );
        let ts = got
            .0
            .get(HEADER_TIMESTAMP)
            .and_then(|v| v.to_str().ok())
            .expect("timestamp");
        assert_eq!(
            got.0.get(HEADER_SIGNATURE).and_then(|v| v.to_str().ok()),
            Some(signature("secret", ts, &got.1).as_str())
        );

        let body: Value = serde_json::from_slice(&got.1).expect("json");
        assert_eq!(body["event"], "sandbox.created");
        assert_eq!(body["sandbox_id"], "sbx-1");
        assert_eq!(body["template_id"], "tpl-1");
    }

    #[tokio::test]
    async fn ignores_unsubscribed_event() {
        let count = Arc::new(AtomicUsize::new(0));
        let count_state = count.clone();
        let addr = spawn_receiver(move |_headers, _body| {
            let count = count_state.clone();
            async move {
                count.fetch_add(1, Ordering::SeqCst);
            }
        })
        .await;

        let logger = WebhookLogger::with_queue_size(
            vec![WebhookEndpointConfig {
                url: format!("http://{addr}/hook"),
                events: vec!["sandbox.deleted".to_string()],
                secret: None,
                timeout_secs: 2,
                max_retries: 0,
            }],
            test_client(),
            4,
        )
        .expect("logger");

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sbx-1"))
            .await;

        tokio::time::sleep(Duration::from_millis(150)).await;
        assert_eq!(count.load(Ordering::SeqCst), 0);
    }

    #[tokio::test]
    async fn retries_failed_delivery() {
        let count = Arc::new(AtomicUsize::new(0));
        let count_state = count.clone();
        let addr = spawn_status_receiver(move || {
            if count_state.fetch_add(1, Ordering::SeqCst) == 0 {
                axum::http::StatusCode::INTERNAL_SERVER_ERROR
            } else {
                axum::http::StatusCode::NO_CONTENT
            }
        })
        .await;

        let logger = WebhookLogger::with_queue_size(
            vec![WebhookEndpointConfig {
                url: format!("http://{addr}/hook"),
                events: vec!["sandbox.created".to_string()],
                secret: None,
                timeout_secs: 2,
                max_retries: 1,
            }],
            test_client(),
            4,
        )
        .expect("logger");

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sbx-1"))
            .await;

        wait_until(|| async { count.load(Ordering::SeqCst) == 2 }).await;
        assert_eq!(count.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn unreachable_endpoint_does_not_block_log_call() {
        let unused_addr = {
            let listener = TcpListener::bind("127.0.0.1:0").await.expect("bind");
            listener.local_addr().expect("addr")
        };

        let logger = WebhookLogger::with_queue_size(
            vec![WebhookEndpointConfig {
                url: format!("http://{unused_addr}/hook"),
                events: vec!["sandbox.created".to_string()],
                secret: None,
                timeout_secs: 1,
                max_retries: 1,
            }],
            test_client(),
            4,
        )
        .expect("logger");

        tokio::time::timeout(
            Duration::from_millis(50),
            logger
                .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sbx-1")),
        )
        .await
        .expect("webhook logging must not wait for unreachable receivers");
    }

    fn test_client() -> reqwest::Client {
        reqwest::Client::builder()
            .no_proxy()
            .build()
            .expect("test client")
    }

    async fn wait_until<F, Fut>(mut predicate: F)
    where
        F: FnMut() -> Fut,
        Fut: std::future::Future<Output = bool>,
    {
        for _ in 0..50 {
            if predicate().await {
                return;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    }

    async fn spawn_receiver<F, Fut>(handler: F) -> SocketAddr
    where
        F: Fn(HeaderMap, Vec<u8>) -> Fut + Clone + Send + Sync + 'static,
        Fut: std::future::Future<Output = ()> + Send + 'static,
    {
        let app = Router::new()
            .route(
                "/hook",
                post(
                    |State(handler): State<F>, headers: HeaderMap, body: axum::body::Bytes| async move {
                        handler(headers, body.to_vec()).await;
                        axum::http::StatusCode::NO_CONTENT
                    },
                ),
            )
            .with_state(handler);
        let listener = TcpListener::bind("127.0.0.1:0").await.expect("bind");
        let addr = listener.local_addr().expect("addr");
        tokio::spawn(async move {
            axum::serve(listener, app).await.expect("server");
        });
        addr
    }

    async fn spawn_status_receiver<F>(status: F) -> SocketAddr
    where
        F: Fn() -> axum::http::StatusCode + Clone + Send + Sync + 'static,
    {
        let app = Router::new()
            .route(
                "/hook",
                post(|State(status): State<F>, Json(_body): Json<Value>| async move { status() }),
            )
            .with_state(status);
        let listener = TcpListener::bind("127.0.0.1:0").await.expect("bind");
        let addr = listener.local_addr().expect("addr");
        tokio::spawn(async move {
            axum::serve(listener, app).await.expect("server");
        });
        addr
    }
}

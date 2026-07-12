// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! HTTP webhook log backend.
//!
//! Sends selected structured log events as JSON POST requests to configured
//! webhook endpoints. Delivery is asynchronous: `log()` only attempts a
//! bounded-channel enqueue and never waits for receiver I/O.

use async_trait::async_trait;
use chrono::Utc;
use hmac::{Hmac, Mac};
use reqwest::{Client, StatusCode, Url};
use serde::Serialize;
use sha2::Sha256;
use std::{collections::HashSet, sync::Arc, time::Duration};
use tokio::{
    sync::{mpsc, oneshot, Semaphore},
    task::JoinSet,
};
use tracing::{debug, warn};

use super::{LogEvent, Logger};
use crate::config::{WebhookConfig, WebhookEndpointConfig};

type HmacSha256 = Hmac<Sha256>;

const SIGNATURE_HEADER: &str = "X-Cube-Webhook-Signature";
const TIMESTAMP_HEADER: &str = "X-Cube-Webhook-Timestamp";
const SUPPORTED_EVENTS: &[&str] = &[
    "sandbox.created",
    "sandbox.deleted",
    "sandbox.paused",
    "sandbox.resumed",
];
const FLUSH_TIMEOUT: Duration = Duration::from_secs(5);

#[derive(Debug, Clone)]
pub struct HttpLoggerConfig {
    pub enabled: bool,
    pub queue_size: usize,
    pub delivery_concurrency: usize,
    pub default_initial_backoff: Duration,
    pub default_max_backoff: Duration,
    pub endpoints: Vec<WebhookEndpoint>,
}

#[derive(Debug, Clone)]
pub struct WebhookEndpoint {
    pub name: String,
    pub url: Url,
    pub events: HashSet<String>,
    pub secret: Option<String>,
    pub timeout: Duration,
    pub max_retries: usize,
}

#[derive(Serialize)]
struct WebhookPayload {
    event_id: String,
    event: String,
    timestamp: chrono::DateTime<Utc>,
    sandbox_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    template_id: Option<String>,
    #[serde(flatten)]
    fields: serde_json::Map<String, serde_json::Value>,
}

enum Msg {
    Event(LogEvent),
    Flush(oneshot::Sender<()>),
}

enum EndpointMsg {
    Delivery(Vec<u8>),
    Flush(oneshot::Sender<()>),
}

struct DeliveryTarget {
    endpoint: Arc<WebhookEndpoint>,
    tx: mpsc::Sender<EndpointMsg>,
}

#[derive(Clone)]
pub struct HttpLogger {
    tx: mpsc::Sender<Msg>,
    subscribed_events: Arc<HashSet<String>>,
}

impl Default for HttpLoggerConfig {
    fn default() -> Self {
        Self {
            enabled: false,
            queue_size: 1024,
            delivery_concurrency: 64,
            default_initial_backoff: Duration::from_millis(200),
            default_max_backoff: Duration::from_millis(2000),
            endpoints: Vec::new(),
        }
    }
}

impl TryFrom<&WebhookConfig> for HttpLoggerConfig {
    type Error = anyhow::Error;

    fn try_from(config: &WebhookConfig) -> anyhow::Result<Self> {
        if !config.enabled {
            return Ok(Self::default());
        }
        anyhow::ensure!(config.queue_size > 0, "webhook.queue_size must be positive");
        anyhow::ensure!(
            config.delivery_concurrency > 0,
            "webhook.delivery_concurrency must be positive"
        );
        anyhow::ensure!(
            config.default_timeout_ms > 0,
            "webhook.default_timeout_ms must be positive"
        );
        anyhow::ensure!(
            config.default_initial_backoff_ms > 0,
            "webhook.default_initial_backoff_ms must be positive"
        );
        anyhow::ensure!(
            config.default_max_backoff_ms >= config.default_initial_backoff_ms,
            "webhook.default_max_backoff_ms must be >= default_initial_backoff_ms"
        );
        anyhow::ensure!(
            !config.endpoints.is_empty(),
            "webhook.endpoints must not be empty when webhook is enabled"
        );

        let default_timeout = Duration::from_millis(config.default_timeout_ms);
        let default_max_retries = config.default_max_retries;
        let mut endpoints = Vec::with_capacity(config.endpoints.len());
        for endpoint in &config.endpoints {
            endpoints.push(normalize_endpoint(
                endpoint,
                default_timeout,
                default_max_retries,
            )?);
        }

        Ok(Self {
            enabled: true,
            queue_size: config.queue_size,
            delivery_concurrency: config.delivery_concurrency,
            default_initial_backoff: Duration::from_millis(config.default_initial_backoff_ms),
            default_max_backoff: Duration::from_millis(config.default_max_backoff_ms),
            endpoints,
        })
    }
}

fn normalize_endpoint(
    endpoint: &WebhookEndpointConfig,
    default_timeout: Duration,
    default_max_retries: usize,
) -> anyhow::Result<WebhookEndpoint> {
    let raw_url = endpoint.url.trim();
    anyhow::ensure!(
        !raw_url.is_empty(),
        "webhook endpoint {} has an empty URL; expected http:// or https:// URL",
        endpoint.name
    );
    let url = Url::parse(raw_url).map_err(|err| {
        anyhow::anyhow!(
            "webhook endpoint {} has an invalid URL {:?}: {err}",
            endpoint.name,
            raw_url
        )
    })?;
    anyhow::ensure!(
        matches!(url.scheme(), "http" | "https") && url.host_str().is_some(),
        "webhook endpoint {} has invalid url",
        endpoint.name
    );
    anyhow::ensure!(
        !endpoint.events.is_empty(),
        "webhook endpoint {} events must not be empty",
        endpoint.name
    );

    let mut events = HashSet::with_capacity(endpoint.events.len());
    for event in &endpoint.events {
        let event = event.trim();
        anyhow::ensure!(
            SUPPORTED_EVENTS.contains(&event),
            "webhook endpoint {} has unsupported event {}",
            endpoint.name,
            event
        );
        events.insert(event.to_string());
    }

    Ok(WebhookEndpoint {
        name: endpoint.name.trim().to_string(),
        url,
        events,
        secret: endpoint.secret.as_ref().and_then(|s| {
            let trimmed = s.trim();
            (!trimmed.is_empty()).then(|| trimmed.to_string())
        }),
        timeout: endpoint
            .timeout_ms
            .filter(|v| *v > 0)
            .map(Duration::from_millis)
            .unwrap_or(default_timeout),
        max_retries: endpoint.max_retries.unwrap_or(default_max_retries),
    })
}

impl HttpLogger {
    pub fn new(config: HttpLoggerConfig) -> Self {
        let (tx, rx) = mpsc::channel(config.queue_size.max(1));
        let subscribed_events = Arc::new(
            config
                .endpoints
                .iter()
                .flat_map(|endpoint| endpoint.events.iter().cloned())
                .collect(),
        );
        let logger = Self {
            tx,
            subscribed_events,
        };
        tokio::spawn(run_worker(config, rx));
        logger
    }

    pub fn enabled(config: &HttpLoggerConfig) -> bool {
        config.enabled && !config.endpoints.is_empty()
    }

    fn subscribed(&self, event: &str) -> bool {
        self.subscribed_events.contains(event)
    }
}

#[async_trait]
impl Logger for HttpLogger {
    async fn log(&self, event: LogEvent) {
        if !self.subscribed(&event.event) {
            return;
        }
        if let Err(err) = self.tx.try_send(Msg::Event(event)) {
            warn!(error = %err, "HttpLogger: webhook queue unavailable, dropping event");
        }
    }

    async fn flush(&self) {
        let (tx, rx) = oneshot::channel();
        if self.tx.send(Msg::Flush(tx)).await.is_ok() {
            if tokio::time::timeout(FLUSH_TIMEOUT, rx).await.is_err() {
                warn!("HttpLogger: webhook flush timed out");
            }
        }
    }

    fn name(&self) -> &'static str {
        "http"
    }
}

async fn run_worker(config: HttpLoggerConfig, mut rx: mpsc::Receiver<Msg>) {
    if !HttpLogger::enabled(&config) {
        return;
    }

    let endpoint_count = config.endpoints.len();
    let endpoint_queue_size = (config.queue_size / endpoint_count).max(1);
    let endpoint_concurrency = config.delivery_concurrency.div_ceil(endpoint_count);
    let global_semaphore = Arc::new(Semaphore::new(config.delivery_concurrency));
    let client = webhook_client();
    let endpoints = config
        .endpoints
        .into_iter()
        .map(|endpoint| {
            let endpoint = Arc::new(endpoint);
            let (tx, endpoint_rx) = mpsc::channel(endpoint_queue_size);
            tokio::spawn(run_endpoint_worker(
                endpoint.clone(),
                client.clone(),
                global_semaphore.clone(),
                endpoint_concurrency,
                config.default_initial_backoff,
                config.default_max_backoff,
                endpoint_rx,
            ));
            DeliveryTarget { endpoint, tx }
        })
        .collect::<Vec<_>>();

    while let Some(msg) = rx.recv().await {
        match msg {
            Msg::Event(event) => {
                let Some((event_name, body)) = encode_event(event) else {
                    continue;
                };
                for target in &endpoints {
                    if !target.endpoint.events.contains(&event_name) {
                        continue;
                    }
                    if let Err(err) = target.tx.try_send(EndpointMsg::Delivery(body.clone())) {
                        warn!(
                            endpoint = %target.endpoint.name,
                            error = %err,
                            "HttpLogger: endpoint queue unavailable, dropping event"
                        );
                    }
                }
            }
            Msg::Flush(reply) => {
                flush_endpoints(&endpoints).await;
                let _ = reply.send(());
            }
        }
    }
}

async fn run_endpoint_worker(
    endpoint: Arc<WebhookEndpoint>,
    client: Client,
    global_semaphore: Arc<Semaphore>,
    concurrency: usize,
    initial_backoff: Duration,
    max_backoff: Duration,
    mut rx: mpsc::Receiver<EndpointMsg>,
) {
    let endpoint_semaphore = Arc::new(Semaphore::new(concurrency));
    let mut pending = JoinSet::new();
    while let Some(msg) = rx.recv().await {
        match msg {
            EndpointMsg::Delivery(body) => {
                // Each endpoint has a separate queue and permit pool, so a bad endpoint
                // cannot keep healthy endpoints from scheduling their own deliveries.
                let endpoint_permit = endpoint_semaphore
                    .clone()
                    .acquire_owned()
                    .await
                    .expect("webhook endpoint semaphore is never closed");
                let global_permit = global_semaphore
                    .clone()
                    .acquire_owned()
                    .await
                    .expect("webhook delivery semaphore is never closed");
                let endpoint = endpoint.clone();
                let client = client.clone();
                pending.spawn(async move {
                    let _endpoint_permit = endpoint_permit;
                    let _global_permit = global_permit;
                    deliver_with_retry(&client, &endpoint, body, initial_backoff, max_backoff)
                        .await;
                });
                drain_finished(&mut pending).await;
            }
            EndpointMsg::Flush(reply) => {
                flush_pending(&mut pending).await;
                let _ = reply.send(());
            }
        }
    }
}

async fn flush_endpoints(endpoints: &[DeliveryTarget]) {
    for target in endpoints {
        let (tx, rx) = oneshot::channel();
        match target.tx.try_send(EndpointMsg::Flush(tx)) {
            Ok(()) => {
                if tokio::time::timeout(FLUSH_TIMEOUT, rx).await.is_err() {
                    warn!(endpoint = %target.endpoint.name, "HttpLogger: endpoint flush timed out");
                }
            }
            Err(err) => warn!(
                endpoint = %target.endpoint.name,
                error = %err,
                "HttpLogger: endpoint flush unavailable"
            ),
        }
    }
}

async fn flush_pending(pending: &mut JoinSet<()>) {
    let completed = tokio::time::timeout(FLUSH_TIMEOUT, async {
        while let Some(result) = pending.join_next().await {
            if let Err(err) = result {
                warn!(error = %err, "HttpLogger: webhook delivery task failed");
            }
        }
    })
    .await;
    if completed.is_err() {
        warn!("HttpLogger: aborting webhook deliveries after flush timeout");
        pending.abort_all();
        drain_finished(pending).await;
    }
}

fn webhook_client() -> Client {
    Client::builder()
        // Webhook endpoints are operator-configured but must not turn a 3xx response
        // into a request to an arbitrary internal address.
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .expect("failed to build webhook HTTP client")
}

fn encode_event(event: LogEvent) -> Option<(String, Vec<u8>)> {
    let payload = build_payload(event)?;
    let event_name = payload.event.clone();
    let body = match serde_json::to_vec(&payload) {
        Ok(body) => body,
        Err(err) => {
            warn!(error = %err, "HttpLogger: failed to serialize webhook payload");
            return None;
        }
    };
    Some((event_name, body))
}

async fn drain_finished(pending: &mut JoinSet<()>) {
    while let Some(result) = pending.try_join_next() {
        if let Err(err) = result {
            warn!(error = %err, "HttpLogger: webhook delivery task failed");
        }
    }
}

fn build_payload(event: LogEvent) -> Option<WebhookPayload> {
    let mut fields = serde_json::Map::new();
    for (key, value) in event.fields {
        fields.insert(key, value);
    }

    let sandbox_id = take_string_field(&mut fields, "sandbox_id")?;
    let template_id = take_string_field(&mut fields, "template_id");
    let event_id = format!(
        "{}.{}.{}",
        event.event,
        sandbox_id,
        event.timestamp.timestamp_nanos_opt().unwrap_or_default()
    );

    Some(WebhookPayload {
        event_id,
        event: event.event,
        timestamp: event.timestamp,
        sandbox_id,
        template_id,
        fields,
    })
}

fn take_string_field(
    fields: &mut serde_json::Map<String, serde_json::Value>,
    key: &str,
) -> Option<String> {
    fields
        .remove(key)
        .and_then(|value| value.as_str().map(str::trim).map(str::to_string))
        .filter(|value| !value.is_empty())
}

async fn deliver_with_retry(
    client: &Client,
    endpoint: &WebhookEndpoint,
    body: Vec<u8>,
    initial_backoff: Duration,
    max_backoff: Duration,
) {
    let attempts = endpoint.max_retries + 1;
    for attempt in 1..=attempts {
        match deliver_once(client, endpoint, body.clone()).await {
            Ok(()) => return,
            Err(err) if attempt == attempts => {
                warn!(endpoint = %endpoint.name, attempt, error = %err, "HttpLogger: webhook delivery failed");
                return;
            }
            Err(err) => {
                warn!(endpoint = %endpoint.name, attempt, error = %err, "HttpLogger: webhook delivery retry");
            }
        }
        tokio::time::sleep(backoff(attempt, initial_backoff, max_backoff)).await;
    }
}

async fn deliver_once(
    client: &Client,
    endpoint: &WebhookEndpoint,
    body: Vec<u8>,
) -> anyhow::Result<()> {
    let mut request = client
        .post(endpoint.url.clone())
        .timeout(endpoint.timeout)
        .header(reqwest::header::CONTENT_TYPE, "application/json")
        .body(body.clone());

    if let Some(secret) = &endpoint.secret {
        let timestamp = Utc::now().timestamp().to_string();
        request = request
            .header(TIMESTAMP_HEADER, timestamp.clone())
            .header(SIGNATURE_HEADER, sign(secret, &timestamp, &body));
    }

    let response = request.send().await?;
    if !response.status().is_success() {
        return Err(anyhow::anyhow!(
            "unexpected status code {}",
            response.status()
        ));
    }
    debug!(endpoint = %endpoint.name, status = %StatusCode::OK, "HttpLogger: webhook delivered");
    Ok(())
}

fn sign(secret: &str, timestamp: &str, body: &[u8]) -> String {
    let mut mac = HmacSha256::new_from_slice(secret.as_bytes()).expect("HMAC accepts any key size");
    mac.update(timestamp.as_bytes());
    mac.update(b".");
    mac.update(body);
    format!("sha256={}", hex::encode(mac.finalize().into_bytes()))
}

fn backoff(attempt: usize, initial_backoff: Duration, max_backoff: Duration) -> Duration {
    let mut delay = initial_backoff;
    for _ in 1..attempt {
        delay = delay.saturating_mul(2);
        if delay >= max_backoff {
            return max_backoff;
        }
    }
    delay
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::logging::{LogEvent, LogLevel};
    use axum::{body::Bytes, http::HeaderMap, routing::post, Router};
    use std::sync::{
        atomic::{AtomicUsize, Ordering},
        Arc, Mutex,
    };

    #[test]
    fn payload_extracts_required_fields() {
        let event = LogEvent::new(LogLevel::Info, "sandbox.created")
            .field("sandbox_id", "sandbox-1")
            .field("template_id", "template-1")
            .field("host_ip", "10.0.0.1");

        let payload = build_payload(event).expect("payload");

        assert_eq!(payload.event, "sandbox.created");
        assert_eq!(payload.sandbox_id, "sandbox-1");
        assert_eq!(payload.template_id.as_deref(), Some("template-1"));
        assert!(payload.fields.contains_key("host_ip"));
    }

    #[test]
    fn payload_without_a_nonempty_sandbox_id_is_dropped() {
        assert!(build_payload(LogEvent::new(LogLevel::Info, "sandbox.created")).is_none());
        assert!(build_payload(
            LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "  ")
        )
        .is_none());
    }

    #[test]
    fn endpoint_validation_reports_operator_facing_errors() {
        let endpoint = |url: &str, events: Vec<&str>| WebhookEndpointConfig {
            name: "test-endpoint".to_string(),
            url: url.to_string(),
            events: events.into_iter().map(str::to_string).collect(),
            ..Default::default()
        };

        assert!(normalize_endpoint(
            &endpoint("", vec!["sandbox.created"]),
            Duration::from_secs(1),
            0
        )
        .unwrap_err()
        .to_string()
        .contains("empty URL"));
        assert!(normalize_endpoint(
            &endpoint("ftp://example.test/hook", vec!["sandbox.created"]),
            Duration::from_secs(1),
            0
        )
        .unwrap_err()
        .to_string()
        .contains("invalid url"));
        assert!(normalize_endpoint(
            &endpoint("http://", vec!["sandbox.created"]),
            Duration::from_secs(1),
            0
        )
        .unwrap_err()
        .to_string()
        .contains("invalid URL"));
        assert!(normalize_endpoint(
            &endpoint("https://example.test/hook", vec!["sandbox.unknown"]),
            Duration::from_secs(1),
            0
        )
        .unwrap_err()
        .to_string()
        .contains("unsupported event"));
    }

    #[test]
    fn webhook_config_validation_rejects_invalid_global_limits() {
        let valid_endpoint = WebhookEndpointConfig {
            name: "test-endpoint".to_string(),
            url: "https://example.test/hook".to_string(),
            events: vec!["sandbox.created".to_string()],
            ..Default::default()
        };
        let valid_config = || WebhookConfig {
            enabled: true,
            queue_size: 1,
            delivery_concurrency: 1,
            default_timeout_ms: 1,
            default_max_retries: 0,
            default_initial_backoff_ms: 1,
            default_max_backoff_ms: 1,
            endpoints: vec![valid_endpoint.clone()],
        };

        let mut config = valid_config();
        config.queue_size = 0;
        assert!(HttpLoggerConfig::try_from(&config)
            .unwrap_err()
            .to_string()
            .contains("queue_size"));
        let mut config = valid_config();
        config.delivery_concurrency = 0;
        assert!(HttpLoggerConfig::try_from(&config)
            .unwrap_err()
            .to_string()
            .contains("delivery_concurrency"));
        let mut config = valid_config();
        config.default_timeout_ms = 0;
        assert!(HttpLoggerConfig::try_from(&config)
            .unwrap_err()
            .to_string()
            .contains("default_timeout_ms"));
        let mut config = valid_config();
        config.default_initial_backoff_ms = 0;
        assert!(HttpLoggerConfig::try_from(&config)
            .unwrap_err()
            .to_string()
            .contains("default_initial_backoff_ms"));
        let mut config = valid_config();
        config.default_max_backoff_ms = 0;
        assert!(HttpLoggerConfig::try_from(&config)
            .unwrap_err()
            .to_string()
            .contains("default_max_backoff_ms"));
        let mut config = valid_config();
        config.endpoints.clear();
        assert!(HttpLoggerConfig::try_from(&config)
            .unwrap_err()
            .to_string()
            .contains("endpoints"));
    }

    #[test]
    fn sign_uses_timestamp_dot_body() {
        let body = br#"{"event":"sandbox.created"}"#;
        let got = sign("secret", "1782945600", body);
        assert!(got.starts_with("sha256="));
        assert_eq!(got.len(), "sha256=".len() + 64);
    }

    #[tokio::test]
    async fn deliver_once_posts_signed_request() {
        let (request_tx, request_rx) = tokio::sync::oneshot::channel();
        let request_tx = Arc::new(Mutex::new(Some(request_tx)));
        let app = Router::new().route(
            "/webhook",
            post({
                let request_tx = request_tx.clone();
                move |headers: HeaderMap, body: Bytes| {
                    let request_tx = request_tx.clone();
                    async move {
                        if let Some(tx) = request_tx.lock().unwrap().take() {
                            let _ = tx.send((headers, body));
                        }
                        "ok"
                    }
                }
            }),
        );
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });

        let endpoint = WebhookEndpoint {
            name: "test".to_string(),
            url: Url::parse(&format!("http://{addr}/webhook")).unwrap(),
            events: HashSet::from(["sandbox.created".to_string()]),
            secret: Some("secret".to_string()),
            timeout: Duration::from_secs(1),
            max_retries: 0,
        };
        let payload = build_payload(
            LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sandbox-1"),
        )
        .expect("payload");
        let body = serde_json::to_vec(&payload).unwrap();
        let client = Client::builder().no_proxy().build().unwrap();
        deliver_once(&client, &endpoint, body).await.unwrap();
        let (headers, body) = tokio::time::timeout(Duration::from_secs(5), request_rx)
            .await
            .expect("timed out waiting for request")
            .expect("request sender");
        assert!(headers.contains_key(SIGNATURE_HEADER));
        let body: serde_json::Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(body["event"], "sandbox.created");
        assert_eq!(body["sandbox_id"], "sandbox-1");
    }

    #[tokio::test]
    async fn delivery_does_not_follow_redirects() {
        let redirected_requests = Arc::new(AtomicUsize::new(0));
        let target = Router::new().route(
            "/target",
            post({
                let redirected_requests = redirected_requests.clone();
                move || {
                    let redirected_requests = redirected_requests.clone();
                    async move {
                        redirected_requests.fetch_add(1, Ordering::SeqCst);
                        axum::http::StatusCode::OK
                    }
                }
            }),
        );
        let target_listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let target_addr = target_listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(target_listener, target).await.unwrap();
        });

        let redirect = Router::new().route(
            "/webhook",
            post(move || async move {
                (
                    axum::http::StatusCode::TEMPORARY_REDIRECT,
                    [(
                        axum::http::header::LOCATION,
                        format!("http://{target_addr}/target"),
                    )],
                )
            }),
        );
        let redirect_listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let redirect_addr = redirect_listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(redirect_listener, redirect).await.unwrap();
        });

        let endpoint = WebhookEndpoint {
            name: "redirect".to_string(),
            url: Url::parse(&format!("http://{redirect_addr}/webhook")).unwrap(),
            events: HashSet::from(["sandbox.created".to_string()]),
            secret: None,
            timeout: Duration::from_secs(1),
            max_retries: 0,
        };
        assert!(deliver_once(&webhook_client(), &endpoint, b"{}".to_vec())
            .await
            .is_err());
        tokio::time::sleep(Duration::from_millis(20)).await;
        assert_eq!(redirected_requests.load(Ordering::SeqCst), 0);
    }

    #[tokio::test]
    async fn deliver_with_retry_retries_server_errors() {
        let attempts = Arc::new(AtomicUsize::new(0));
        let app = Router::new().route(
            "/webhook",
            post({
                let attempts = attempts.clone();
                move || {
                    let attempts = attempts.clone();
                    async move {
                        let attempt = attempts.fetch_add(1, Ordering::SeqCst) + 1;
                        if attempt < 3 {
                            axum::http::StatusCode::INTERNAL_SERVER_ERROR
                        } else {
                            axum::http::StatusCode::OK
                        }
                    }
                }
            }),
        );
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });

        let endpoint = WebhookEndpoint {
            name: "retry".to_string(),
            url: Url::parse(&format!("http://{addr}/webhook")).unwrap(),
            events: HashSet::from(["sandbox.created".to_string()]),
            secret: None,
            timeout: Duration::from_secs(1),
            max_retries: 2,
        };
        let client = Client::builder().no_proxy().build().unwrap();
        deliver_with_retry(
            &client,
            &endpoint,
            br#"{"event":"sandbox.created"}"#.to_vec(),
            Duration::from_millis(1),
            Duration::from_millis(1),
        )
        .await;

        assert_eq!(attempts.load(Ordering::SeqCst), 3);
    }

    #[tokio::test]
    async fn log_does_not_block_when_endpoint_is_unreachable() {
        let endpoint = WebhookEndpoint {
            name: "unreachable".to_string(),
            url: Url::parse("http://127.0.0.1:9/webhook").unwrap(),
            events: HashSet::from(["sandbox.created".to_string()]),
            secret: None,
            timeout: Duration::from_millis(10),
            max_retries: 0,
        };
        let logger = HttpLogger::new(HttpLoggerConfig {
            enabled: true,
            queue_size: 1,
            delivery_concurrency: 1,
            default_initial_backoff: Duration::from_millis(1),
            default_max_backoff: Duration::from_millis(1),
            endpoints: vec![endpoint],
        });

        tokio::time::timeout(
            Duration::from_millis(50),
            logger.log(
                LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sandbox-1"),
            ),
        )
        .await
        .expect("log should not block on webhook delivery");
    }

    #[tokio::test]
    async fn queues_delivery_while_concurrency_is_exhausted() {
        let first_started = Arc::new(tokio::sync::Notify::new());
        let release_first = Arc::new(tokio::sync::Notify::new());
        let requests = Arc::new(AtomicUsize::new(0));
        let app = Router::new().route(
            "/webhook",
            post({
                let first_started = first_started.clone();
                let release_first = release_first.clone();
                let requests = requests.clone();
                move || {
                    let first_started = first_started.clone();
                    let release_first = release_first.clone();
                    let requests = requests.clone();
                    async move {
                        let request_number = requests.fetch_add(1, Ordering::SeqCst);
                        if request_number == 0 {
                            first_started.notify_one();
                            release_first.notified().await;
                        }
                        axum::http::StatusCode::OK
                    }
                }
            }),
        );
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });

        let logger = HttpLogger::new(HttpLoggerConfig {
            enabled: true,
            queue_size: 2,
            delivery_concurrency: 1,
            default_initial_backoff: Duration::from_millis(1),
            default_max_backoff: Duration::from_millis(1),
            endpoints: vec![WebhookEndpoint {
                name: "blocked".to_string(),
                url: Url::parse(&format!("http://{addr}/webhook")).unwrap(),
                events: HashSet::from(["sandbox.created".to_string()]),
                secret: None,
                timeout: Duration::from_secs(5),
                max_retries: 0,
            }],
        });

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "one"))
            .await;
        tokio::time::timeout(Duration::from_secs(1), first_started.notified())
            .await
            .expect("first delivery should start");

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "two"))
            .await;
        tokio::time::sleep(Duration::from_millis(20)).await;
        assert_eq!(requests.load(Ordering::SeqCst), 1);

        release_first.notify_waiters();
        tokio::time::timeout(Duration::from_secs(1), async {
            while requests.load(Ordering::SeqCst) < 2 {
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .expect("queued delivery should run after the first request completes");
        logger.flush().await;
    }

    #[tokio::test]
    async fn blocked_endpoint_does_not_delay_healthy_endpoint() {
        let slow_started = Arc::new(tokio::sync::Notify::new());
        let release_slow = Arc::new(tokio::sync::Notify::new());
        let slow_requests = Arc::new(AtomicUsize::new(0));
        let slow = Router::new().route(
            "/webhook",
            post({
                let slow_started = slow_started.clone();
                let release_slow = release_slow.clone();
                let slow_requests = slow_requests.clone();
                move || {
                    let slow_started = slow_started.clone();
                    let release_slow = release_slow.clone();
                    let slow_requests = slow_requests.clone();
                    async move {
                        if slow_requests.fetch_add(1, Ordering::SeqCst) == 0 {
                            slow_started.notify_one();
                            release_slow.notified().await;
                        }
                        axum::http::StatusCode::OK
                    }
                }
            }),
        );
        let slow_listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let slow_addr = slow_listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(slow_listener, slow).await.unwrap();
        });

        let healthy_requests = Arc::new(AtomicUsize::new(0));
        let healthy = Router::new().route(
            "/webhook",
            post({
                let healthy_requests = healthy_requests.clone();
                move || {
                    let healthy_requests = healthy_requests.clone();
                    async move {
                        healthy_requests.fetch_add(1, Ordering::SeqCst);
                        axum::http::StatusCode::OK
                    }
                }
            }),
        );
        let healthy_listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let healthy_addr = healthy_listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(healthy_listener, healthy).await.unwrap();
        });

        let endpoint = |name: &str, addr| WebhookEndpoint {
            name: name.to_string(),
            url: Url::parse(&format!("http://{addr}/webhook")).unwrap(),
            events: HashSet::from(["sandbox.created".to_string()]),
            secret: None,
            timeout: Duration::from_secs(5),
            max_retries: 0,
        };
        let logger = HttpLogger::new(HttpLoggerConfig {
            enabled: true,
            queue_size: 4,
            delivery_concurrency: 2,
            default_initial_backoff: Duration::from_millis(1),
            default_max_backoff: Duration::from_millis(1),
            endpoints: vec![
                endpoint("slow", slow_addr),
                endpoint("healthy", healthy_addr),
            ],
        });

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "one"))
            .await;
        tokio::time::timeout(Duration::from_secs(1), slow_started.notified())
            .await
            .expect("slow endpoint should receive the first delivery");
        tokio::time::timeout(Duration::from_secs(1), async {
            while healthy_requests.load(Ordering::SeqCst) < 1 {
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .expect("healthy endpoint should receive the first delivery");

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "two"))
            .await;
        tokio::time::timeout(Duration::from_secs(1), async {
            while healthy_requests.load(Ordering::SeqCst) < 2 {
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .expect("healthy endpoint should not wait for the blocked endpoint");

        release_slow.notify_waiters();
        logger.flush().await;
    }

    #[test]
    fn backoff_is_capped() {
        assert_eq!(
            backoff(5, Duration::from_millis(200), Duration::from_millis(1000)),
            Duration::from_millis(1000)
        );
    }
}

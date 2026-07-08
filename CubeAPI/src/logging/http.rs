// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! HTTP webhook log backend.
//!
//! Sends individual [`LogEvent`] values as JSON POST requests to configured
//! endpoints. This backend is intentionally event-oriented rather than a
//! generic HTTP log batcher: each matching event becomes one webhook delivery.

use super::{LogEvent, Logger};
use anyhow::{anyhow, bail, Context};
use async_trait::async_trait;
use hmac::{Hmac, Mac};
use reqwest::{
    header::{CONTENT_TYPE, USER_AGENT},
    Client, StatusCode, Url,
};
use serde::Deserialize;
use sha2::Sha256;
use std::{
    collections::{HashMap, HashSet},
    fs,
    sync::Arc,
    time::Duration,
};
use tokio::{
    sync::{mpsc, oneshot, Semaphore},
    task::JoinSet,
};
use tracing::{error, warn};
use uuid::Uuid;

type HmacSha256 = Hmac<Sha256>;
type EndpointRoutes = HashMap<String, Vec<Arc<Endpoint>>>;

/// Configuration for the HTTP webhook backend.
#[derive(Debug, Clone, Deserialize)]
pub struct HttpLoggerConfig {
    #[serde(default)]
    pub delivery: DeliveryConfig,
    #[serde(default)]
    pub endpoints: Vec<WebhookEndpointConfig>,
}

impl HttpLoggerConfig {
    pub fn from_toml_str(raw: &str) -> anyhow::Result<Self> {
        toml::from_str(raw).context("parse webhook config TOML")
    }

    pub fn from_file(path: &str) -> anyhow::Result<Self> {
        let raw = fs::read_to_string(path)
            .with_context(|| format!("read webhook config file {}", path))?;
        Self::from_toml_str(&raw)
    }
}

#[derive(Debug, Clone, Deserialize)]
pub struct DeliveryConfig {
    #[serde(default = "default_queue_size")]
    pub queue_size: usize,
    #[serde(default = "default_request_timeout_secs")]
    pub request_timeout_secs: u64,
    #[serde(default = "default_max_attempts")]
    pub max_attempts: usize,
    #[serde(default = "default_initial_backoff_ms")]
    pub initial_backoff_ms: u64,
    #[serde(default = "default_max_backoff_secs")]
    pub max_backoff_secs: u64,
    #[serde(default = "default_max_in_flight")]
    pub max_in_flight: usize,
}

impl Default for DeliveryConfig {
    fn default() -> Self {
        Self {
            queue_size: default_queue_size(),
            request_timeout_secs: default_request_timeout_secs(),
            max_attempts: default_max_attempts(),
            initial_backoff_ms: default_initial_backoff_ms(),
            max_backoff_secs: default_max_backoff_secs(),
            max_in_flight: default_max_in_flight(),
        }
    }
}

#[derive(Debug, Clone, Deserialize)]
pub struct WebhookEndpointConfig {
    pub name: String,
    pub url: String,
    pub events: Vec<String>,
    #[serde(default)]
    pub secret_env: Option<String>,
}

struct Endpoint {
    name: String,
    url: Url,
    secret: Option<String>,
}

enum Msg {
    Event(LogEvent),
    Flush(oneshot::Sender<()>),
}

/// HTTP webhook log backend.
#[derive(Clone)]
pub struct HttpLogger {
    tx: mpsc::Sender<Msg>,
    routes: Arc<EndpointRoutes>,
}

impl HttpLogger {
    pub fn new(config: HttpLoggerConfig) -> anyhow::Result<Self> {
        validate_delivery(&config.delivery)?;
        let routes = Arc::new(resolve_endpoint_routes(config.endpoints)?);
        let client = Client::builder()
            .timeout(Duration::from_secs(config.delivery.request_timeout_secs))
            .build()
            .context("build webhook HTTP client")?;
        let (tx, rx) = mpsc::channel(config.delivery.queue_size);
        spawn_worker(rx, client, routes.clone(), config.delivery);
        Ok(Self { tx, routes })
    }
}

#[async_trait]
impl Logger for HttpLogger {
    async fn log(&self, event: LogEvent) {
        if !self.routes.contains_key(&event.event) {
            return;
        }

        match self.tx.try_send(Msg::Event(event)) {
            Ok(()) => {}
            Err(mpsc::error::TrySendError::Full(_)) => {
                error!("HttpLogger: webhook queue is full, dropping event");
            }
            Err(mpsc::error::TrySendError::Closed(_)) => {
                error!("HttpLogger: webhook worker is gone, dropping event");
            }
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

fn default_queue_size() -> usize {
    10_000
}
fn default_request_timeout_secs() -> u64 {
    5
}
fn default_max_attempts() -> usize {
    3
}
fn default_initial_backoff_ms() -> u64 {
    500
}
fn default_max_backoff_secs() -> u64 {
    10
}
fn default_max_in_flight() -> usize {
    100
}

fn validate_delivery(delivery: &DeliveryConfig) -> anyhow::Result<()> {
    if delivery.queue_size == 0 {
        bail!("webhook delivery queue_size must be greater than 0");
    }
    if delivery.request_timeout_secs == 0 {
        bail!("webhook delivery request_timeout_secs must be greater than 0");
    }
    if delivery.max_attempts == 0 {
        bail!("webhook delivery max_attempts must be greater than 0");
    }
    if delivery.max_in_flight == 0 {
        bail!("webhook delivery max_in_flight must be greater than 0");
    }
    Ok(())
}

fn resolve_endpoint_routes(configs: Vec<WebhookEndpointConfig>) -> anyhow::Result<EndpointRoutes> {
    if configs.is_empty() {
        bail!("webhook config must contain at least one endpoint");
    }

    let mut routes: EndpointRoutes = HashMap::new();

    for cfg in configs {
        let name = cfg.name.trim();
        if name.is_empty() {
            bail!("webhook endpoint name must not be empty");
        }

        let url =
            Url::parse(&cfg.url).with_context(|| format!("parse webhook endpoint {name} URL"))?;
        if url.scheme() != "http" && url.scheme() != "https" {
            bail!("webhook endpoint {name} URL must use http or https");
        }

        let events: HashSet<_> = cfg
            .events
            .into_iter()
            .map(|event| event.trim().to_string())
            .filter(|event| !event.is_empty())
            .collect();
        if events.is_empty() {
            bail!("webhook endpoint {name} must subscribe to at least one event");
        }

        let secret = match cfg.secret_env {
            Some(env_name) => {
                let env_name = env_name.trim().to_string();
                if env_name.is_empty() {
                    bail!("webhook endpoint {name} secret_env must not be empty");
                }
                let value = std::env::var(&env_name).with_context(|| {
                    format!("read webhook secret env {env_name} for endpoint {name}")
                })?;
                if value.is_empty() {
                    bail!("webhook secret env {env_name} for endpoint {name} must not be empty");
                }
                Some(value)
            }
            None => None,
        };

        let endpoint = Arc::new(Endpoint {
            name: name.to_string(),
            url,
            secret,
        });

        for event in events {
            routes.entry(event).or_default().push(endpoint.clone());
        }
    }

    Ok(routes)
}

fn spawn_worker(
    mut rx: mpsc::Receiver<Msg>,
    client: Client,
    routes: Arc<EndpointRoutes>,
    delivery: DeliveryConfig,
) {
    tokio::spawn(async move {
        let semaphore = Arc::new(Semaphore::new(delivery.max_in_flight));
        let mut tasks = JoinSet::new();

        loop {
            tokio::select! {
                msg = rx.recv() => {
                    match msg {
                        Some(Msg::Event(event)) => {
                            spawn_deliveries(
                                &mut tasks,
                                client.clone(),
                                routes.clone(),
                                semaphore.clone(),
                                delivery.clone(),
                                event,
                            );
                        }
                        Some(Msg::Flush(reply)) => {
                            while tasks.len() > 0 {
                                if let Some(result) = tasks.join_next().await {
                                    if let Err(err) = result {
                                        error!("HttpLogger: delivery task failed: {}", err);
                                    }
                                }
                            }
                            let _ = reply.send(());
                        }
                        None => break,
                    }
                }
                result = tasks.join_next(), if tasks.len() > 0 => {
                    if let Some(Err(err)) = result {
                        error!("HttpLogger: delivery task failed: {}", err);
                    }
                }
            }
        }

        while let Some(result) = tasks.join_next().await {
            if let Err(err) = result {
                error!("HttpLogger: delivery task failed: {}", err);
            }
        }
    });
}

fn spawn_deliveries(
    tasks: &mut JoinSet<()>,
    client: Client,
    routes: Arc<EndpointRoutes>,
    semaphore: Arc<Semaphore>,
    delivery: DeliveryConfig,
    event: LogEvent,
) {
    let body = match serde_json::to_vec(&event) {
        Ok(body) => body,
        Err(err) => {
            error!(
                event = %event.event,
                "HttpLogger: failed to serialise event for webhook: {}",
                err
            );
            return;
        }
    };

    let Some(endpoints) = routes.get(&event.event) else {
        return;
    };

    let event_id = Uuid::new_v4().to_string();

    for endpoint in endpoints.iter().cloned() {
        let client = client.clone();
        let semaphore = semaphore.clone();
        let delivery = delivery.clone();
        let event_name = event.event.clone();
        let body = body.clone();
        let event_id = event_id.clone();
        tasks.spawn(async move {
            deliver_with_retry(
                client, endpoint, semaphore, body, event_name, event_id, delivery,
            )
            .await;
        });
    }
}

async fn deliver_with_retry(
    client: Client,
    endpoint: Arc<Endpoint>,
    semaphore: Arc<Semaphore>,
    body: Vec<u8>,
    event_name: String,
    event_id: String,
    delivery: DeliveryConfig,
) {
    for attempt in 1..=delivery.max_attempts {
        let result = {
            // Limit only the HTTP attempt; retry backoff must not consume a permit.
            let permit = semaphore.clone().acquire_owned().await;
            let Ok(_permit_guard) = permit else {
                error!("HttpLogger: webhook semaphore closed, dropping delivery");
                return;
            };
            send_once(&client, &endpoint, &body, &event_name, &event_id).await
        };

        match result {
            Ok(status) if status.is_success() => return,
            Ok(status) => {
                if !is_retriable_status(status) || attempt == delivery.max_attempts {
                    error!(
                        endpoint = %endpoint.name,
                        event = %event_name,
                        event_id = %event_id,
                        attempts = attempt,
                        status = %status,
                        "HttpLogger: webhook delivery failed"
                    );
                    return;
                }
                warn!(
                    endpoint = %endpoint.name,
                    event = %event_name,
                    event_id = %event_id,
                    attempt,
                    status = %status,
                    "HttpLogger: webhook delivery retrying"
                );
            }
            Err(err) => {
                if attempt == delivery.max_attempts {
                    error!(
                        endpoint = %endpoint.name,
                        event = %event_name,
                        event_id = %event_id,
                        attempts = attempt,
                        error = %err,
                        "HttpLogger: webhook delivery failed"
                    );
                    return;
                }
                warn!(
                    endpoint = %endpoint.name,
                    event = %event_name,
                    event_id = %event_id,
                    attempt,
                    error = %err,
                    "HttpLogger: webhook delivery retrying"
                );
            }
        }

        tokio::time::sleep(backoff_duration(&delivery, attempt)).await;
    }
}

async fn send_once(
    client: &Client,
    endpoint: &Endpoint,
    body: &[u8],
    event_name: &str,
    event_id: &str,
) -> Result<StatusCode, reqwest::Error> {
    let mut request = client
        .post(endpoint.url.clone())
        .header(CONTENT_TYPE, "application/json")
        .header(USER_AGENT, "CubeSandbox-Webhook/1.0")
        .header("X-Cube-Event", event_name)
        .header("X-Cube-Event-Id", event_id)
        .body(body.to_vec());

    if let Some(secret) = endpoint.secret.as_ref() {
        request = request.header("X-Cube-Signature-256", signature_header(secret, body));
    }

    request.send().await.map(|resp| resp.status())
}

fn is_retriable_status(status: StatusCode) -> bool {
    status == StatusCode::REQUEST_TIMEOUT
        || status == StatusCode::TOO_MANY_REQUESTS
        || status.is_server_error()
}

fn backoff_duration(delivery: &DeliveryConfig, failed_attempt: usize) -> Duration {
    let exponent = failed_attempt.saturating_sub(1).min(16) as u32;
    let millis = delivery
        .initial_backoff_ms
        .saturating_mul(2_u64.saturating_pow(exponent));
    let max_millis = delivery.max_backoff_secs.saturating_mul(1_000);
    Duration::from_millis(millis.min(max_millis))
}

fn signature_header(secret: &str, body: &[u8]) -> String {
    let mut mac = HmacSha256::new_from_slice(secret.as_bytes())
        .map_err(|err| anyhow!(err))
        .expect("HMAC accepts any key length");
    mac.update(body);
    format!("sha256={}", hex::encode(mac.finalize().into_bytes()))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::logging::{LogEvent, LogLevel};
    use axum::{
        body::Bytes,
        extract::State,
        http::{HeaderMap, StatusCode},
        routing::post,
        Router,
    };
    use std::{
        sync::{
            atomic::{AtomicUsize, Ordering},
            Arc, Mutex,
        },
        time::{Duration, Instant},
    };
    use tokio::net::TcpListener;

    #[derive(Clone)]
    struct RecordedRequest {
        headers: HeaderMap,
        body: String,
    }

    struct Recorder {
        requests: Mutex<Vec<RecordedRequest>>,
        count: AtomicUsize,
        statuses: Vec<StatusCode>,
        delay: Option<Duration>,
    }

    async fn record_request(
        State(recorder): State<Arc<Recorder>>,
        headers: HeaderMap,
        body: Bytes,
    ) -> StatusCode {
        if let Some(delay) = recorder.delay {
            tokio::time::sleep(delay).await;
        }
        let idx = recorder.count.fetch_add(1, Ordering::SeqCst);
        recorder.requests.lock().unwrap().push(RecordedRequest {
            headers,
            body: String::from_utf8(body.to_vec()).unwrap(),
        });
        recorder
            .statuses
            .get(idx)
            .copied()
            .unwrap_or(StatusCode::NO_CONTENT)
    }

    async fn spawn_recorder(
        statuses: Vec<StatusCode>,
        delay: Option<Duration>,
    ) -> (String, Arc<Recorder>) {
        let recorder = Arc::new(Recorder {
            requests: Mutex::new(Vec::new()),
            count: AtomicUsize::new(0),
            statuses,
            delay,
        });
        let app = Router::new()
            .route("/webhook", post(record_request))
            .with_state(recorder.clone());
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });
        (format!("http://{addr}/webhook"), recorder)
    }

    async fn wait_for_count(recorder: &Arc<Recorder>, expected: usize, max_wait: Duration) -> bool {
        tokio::time::timeout(max_wait, async {
            loop {
                if recorder.count.load(Ordering::SeqCst) >= expected {
                    return;
                }
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .is_ok()
    }

    fn test_config(url: String, events: Vec<&str>) -> HttpLoggerConfig {
        HttpLoggerConfig {
            delivery: DeliveryConfig {
                queue_size: 16,
                request_timeout_secs: 1,
                max_attempts: 3,
                initial_backoff_ms: 1,
                max_backoff_secs: 1,
                max_in_flight: 4,
            },
            endpoints: vec![WebhookEndpointConfig {
                name: "test".to_string(),
                url,
                events: events.into_iter().map(str::to_string).collect(),
                secret_env: None,
            }],
        }
    }

    #[test]
    fn parses_toml_config() {
        let raw = r#"
            [delivery]
            queue_size = 99
            request_timeout_secs = 3
            max_attempts = 4
            initial_backoff_ms = 250
            max_backoff_secs = 8
            max_in_flight = 12

            [[endpoints]]
            name = "local"
            url = "http://127.0.0.1:8088/webhook"
            events = ["sandbox.created", "api.error"]
            secret_env = "CUBE_WEBHOOK_SECRET_0"
        "#;

        let cfg = HttpLoggerConfig::from_toml_str(raw).unwrap();

        assert_eq!(cfg.delivery.queue_size, 99);
        assert_eq!(cfg.delivery.max_attempts, 4);
        assert_eq!(cfg.endpoints.len(), 1);
        assert_eq!(cfg.endpoints[0].name, "local");
        assert_eq!(
            cfg.endpoints[0].events,
            vec!["sandbox.created", "api.error"]
        );
        assert_eq!(
            cfg.endpoints[0].secret_env.as_deref(),
            Some("CUBE_WEBHOOK_SECRET_0")
        );
    }

    #[test]
    fn builds_endpoint_routes_by_event() {
        let configs = vec![
            WebhookEndpointConfig {
                name: "audit".to_string(),
                url: "http://127.0.0.1:8080/audit".to_string(),
                events: vec![
                    "sandbox.created".to_string(),
                    "api.error".to_string(),
                    "sandbox.created".to_string(),
                ],
                secret_env: None,
            },
            WebhookEndpointConfig {
                name: "alerts".to_string(),
                url: "http://127.0.0.1:8080/alerts".to_string(),
                events: vec!["sandbox.created".to_string()],
                secret_env: None,
            },
        ];

        let routes = resolve_endpoint_routes(configs).unwrap();

        assert_eq!(routes["sandbox.created"].len(), 2);
        assert_eq!(routes["api.error"].len(), 1);
        assert_eq!(routes["api.error"][0].name, "audit");
    }

    #[test]
    fn missing_secret_env_fails_logger_construction() {
        std::env::remove_var("CUBE_WEBHOOK_SECRET_MISSING");
        let mut cfg = test_config(
            "http://127.0.0.1:8088/webhook".to_string(),
            vec!["api.error"],
        );
        cfg.endpoints[0].secret_env = Some("CUBE_WEBHOOK_SECRET_MISSING".to_string());

        let err = match HttpLogger::new(cfg) {
            Ok(_) => panic!("logger construction should fail when secret env is missing"),
            Err(err) => err,
        };

        assert!(err.to_string().contains("CUBE_WEBHOOK_SECRET_MISSING"));
    }

    #[test]
    fn hmac_signature_header_uses_sha256_hex() {
        let body = br#"{"event":"sandbox.created"}"#;

        let signature = signature_header("secret", body);

        assert_eq!(
            signature,
            "sha256=9fce8beb32bfed09995cada741f29b4b67882b1694b37dfa74e76cf82e29dca7"
        );
    }

    #[tokio::test]
    async fn filters_events_before_delivery() {
        let (url, recorder) = spawn_recorder(vec![], None).await;
        let logger = HttpLogger::new(test_config(url, vec!["sandbox.created"])).unwrap();

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.deleted").field("sandbox_id", "sbx-1"))
            .await;
        logger.flush().await;
        assert_eq!(recorder.count.load(Ordering::SeqCst), 0);

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sbx-1"))
            .await;
        logger.flush().await;

        let requests = recorder.requests.lock().unwrap();
        assert_eq!(requests.len(), 1);
        assert_eq!(
            requests[0].headers["x-cube-event"].to_str().unwrap(),
            "sandbox.created"
        );
        assert!(requests[0].body.contains(r#""event":"sandbox.created""#));
    }

    #[tokio::test]
    async fn signs_delivered_payload_when_secret_is_configured() {
        std::env::set_var("CUBE_WEBHOOK_SECRET_TEST", "secret");
        let (url, recorder) = spawn_recorder(vec![], None).await;
        let mut cfg = test_config(url, vec!["sandbox.created"]);
        cfg.endpoints[0].secret_env = Some("CUBE_WEBHOOK_SECRET_TEST".to_string());
        let logger = HttpLogger::new(cfg).unwrap();

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sbx-1"))
            .await;
        logger.flush().await;

        let requests = recorder.requests.lock().unwrap();
        assert_eq!(requests.len(), 1);
        let body = requests[0].body.as_bytes();
        assert_eq!(
            requests[0].headers["x-cube-signature-256"]
                .to_str()
                .unwrap(),
            signature_header("secret", body)
        );
    }

    #[tokio::test]
    async fn sends_one_event_id_header_across_fanout_and_rotates_between_events() {
        let (audit_url, audit_recorder) = spawn_recorder(vec![StatusCode::NO_CONTENT], None).await;
        let (ops_url, ops_recorder) = spawn_recorder(vec![StatusCode::NO_CONTENT], None).await;
        let logger = HttpLogger::new(HttpLoggerConfig {
            delivery: DeliveryConfig {
                queue_size: 16,
                request_timeout_secs: 1,
                max_attempts: 1,
                initial_backoff_ms: 1,
                max_backoff_secs: 1,
                max_in_flight: 4,
            },
            endpoints: vec![
                WebhookEndpointConfig {
                    name: "audit".to_string(),
                    url: audit_url,
                    events: vec!["sandbox.created".to_string()],
                    secret_env: None,
                },
                WebhookEndpointConfig {
                    name: "ops".to_string(),
                    url: ops_url,
                    events: vec!["sandbox.created".to_string()],
                    secret_env: None,
                },
            ],
        })
        .unwrap();

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sbx-1"))
            .await;
        logger.flush().await;

        let audit_requests = audit_recorder.requests.lock().unwrap();
        let ops_requests = ops_recorder.requests.lock().unwrap();
        assert_eq!(audit_requests.len(), 1);
        assert_eq!(ops_requests.len(), 1);
        let audit_event_id = audit_requests[0]
            .headers
            .get("x-cube-event-id")
            .unwrap()
            .to_str()
            .unwrap()
            .to_string();
        let ops_event_id = ops_requests[0]
            .headers
            .get("x-cube-event-id")
            .unwrap()
            .to_str()
            .unwrap()
            .to_string();
        assert!(!audit_event_id.is_empty());
        assert_eq!(audit_event_id, ops_event_id);
        assert!(!audit_requests[0].headers.contains_key("x-cube-delivery"));
        assert!(!ops_requests[0].headers.contains_key("x-cube-delivery"));
        drop(audit_requests);
        drop(ops_requests);

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sbx-2"))
            .await;
        logger.flush().await;

        let audit_requests = audit_recorder.requests.lock().unwrap();
        let ops_requests = ops_recorder.requests.lock().unwrap();
        assert_eq!(audit_requests.len(), 2);
        assert_eq!(ops_requests.len(), 2);
        let second_audit_event_id = audit_requests[1]
            .headers
            .get("x-cube-event-id")
            .unwrap()
            .to_str()
            .unwrap()
            .to_string();
        let second_ops_event_id = ops_requests[1]
            .headers
            .get("x-cube-event-id")
            .unwrap()
            .to_str()
            .unwrap()
            .to_string();
        assert_eq!(second_audit_event_id, second_ops_event_id);
        assert_ne!(audit_event_id, second_audit_event_id);
    }

    #[tokio::test]
    async fn retries_retriable_delivery_failures() {
        let (url, recorder) = spawn_recorder(
            vec![StatusCode::INTERNAL_SERVER_ERROR, StatusCode::NO_CONTENT],
            None,
        )
        .await;
        let logger = HttpLogger::new(test_config(url, vec!["api.error"])).unwrap();

        logger
            .log(LogEvent::new(LogLevel::Error, "api.error").field("handler", "create_sandbox"))
            .await;
        logger.flush().await;

        assert_eq!(recorder.count.load(Ordering::SeqCst), 2);
        let requests = recorder.requests.lock().unwrap();
        assert_eq!(requests.len(), 2);
        let first_event_id = requests[0]
            .headers
            .get("x-cube-event-id")
            .unwrap()
            .to_str()
            .unwrap();
        let second_event_id = requests[1]
            .headers
            .get("x-cube-event-id")
            .unwrap()
            .to_str()
            .unwrap();
        assert_eq!(first_event_id, second_event_id);
        assert!(!requests[0].headers.contains_key("x-cube-delivery"));
        assert!(!requests[1].headers.contains_key("x-cube-delivery"));
    }

    #[tokio::test]
    async fn retry_backoff_does_not_hold_global_permit() {
        let (failing_url, failing_recorder) = spawn_recorder(
            vec![StatusCode::INTERNAL_SERVER_ERROR, StatusCode::NO_CONTENT],
            None,
        )
        .await;
        let (healthy_url, healthy_recorder) =
            spawn_recorder(vec![StatusCode::NO_CONTENT], None).await;
        let logger = HttpLogger::new(HttpLoggerConfig {
            delivery: DeliveryConfig {
                queue_size: 16,
                request_timeout_secs: 1,
                max_attempts: 2,
                initial_backoff_ms: 500,
                max_backoff_secs: 1,
                max_in_flight: 1,
            },
            endpoints: vec![
                WebhookEndpointConfig {
                    name: "failing".to_string(),
                    url: failing_url,
                    events: vec!["api.error".to_string()],
                    secret_env: None,
                },
                WebhookEndpointConfig {
                    name: "healthy".to_string(),
                    url: healthy_url,
                    events: vec!["sandbox.created".to_string()],
                    secret_env: None,
                },
            ],
        })
        .unwrap();

        logger
            .log(LogEvent::new(LogLevel::Error, "api.error").field("handler", "create_sandbox"))
            .await;
        assert!(wait_for_count(&failing_recorder, 1, Duration::from_millis(100)).await);
        tokio::time::sleep(Duration::from_millis(10)).await;

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sbx-1"))
            .await;
        assert!(
            wait_for_count(&healthy_recorder, 1, Duration::from_millis(200)).await,
            "healthy webhook delivery should not wait for another endpoint's retry backoff"
        );

        logger.flush().await;
        assert_eq!(failing_recorder.count.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn log_does_not_wait_for_slow_receiver() {
        let (url, _recorder) = spawn_recorder(
            vec![StatusCode::NO_CONTENT],
            Some(Duration::from_millis(200)),
        )
        .await;
        let logger = HttpLogger::new(test_config(url, vec!["sandbox.created"])).unwrap();
        let start = Instant::now();

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sbx-1"))
            .await;

        assert!(start.elapsed() < Duration::from_millis(50));
        logger.flush().await;
    }
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! HTTP webhook log backend.
//!
//! Sends matching [`LogEvent`] values as JSON batch envelopes to configured
//! endpoints. The default `default_batch_size = 1` preserves event-trigger
//! behavior, while larger endpoint batches let operators use the same backend as
//! a lightweight log sink with bounded flush latency.

use super::{LogEvent, Logger};
use anyhow::{anyhow, bail, Context};
use async_trait::async_trait;
use hmac::{Hmac, Mac};
use reqwest::{
    header::{CONTENT_TYPE, USER_AGENT},
    Client, StatusCode, Url,
};
use serde::{Deserialize, Serialize};
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
    time::MissedTickBehavior,
};
use tracing::{error, warn};
use uuid::Uuid;

type HmacSha256 = Hmac<Sha256>;
type EndpointRoutes = HashMap<String, Vec<Arc<Endpoint>>>; // event -> endpoints

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
    #[serde(default = "default_event_queue_capacity")]
    pub event_queue_capacity: usize,
    #[serde(default = "default_batch_size")]
    pub default_batch_size: usize,
    #[serde(default = "default_flush_interval_secs")]
    pub flush_interval_secs: u64,
    #[serde(default = "default_request_timeout_secs")]
    pub request_timeout_secs: u64,
    #[serde(default = "default_max_attempts")]
    pub max_attempts: usize,
    #[serde(default = "default_initial_backoff_ms")]
    pub initial_backoff_ms: u64,
    #[serde(default = "default_max_backoff_secs")]
    pub max_backoff_secs: u64,
    #[serde(default = "default_max_outstanding_deliveries")]
    pub max_outstanding_deliveries: usize,
    #[serde(default = "default_max_concurrent_requests")]
    pub max_concurrent_requests: usize,
}

impl Default for DeliveryConfig {
    fn default() -> Self {
        Self {
            event_queue_capacity: default_event_queue_capacity(),
            default_batch_size: default_batch_size(),
            flush_interval_secs: default_flush_interval_secs(),
            request_timeout_secs: default_request_timeout_secs(),
            max_attempts: default_max_attempts(),
            initial_backoff_ms: default_initial_backoff_ms(),
            max_backoff_secs: default_max_backoff_secs(),
            max_outstanding_deliveries: default_max_outstanding_deliveries(),
            max_concurrent_requests: default_max_concurrent_requests(),
        }
    }
}

#[derive(Debug, Clone, Deserialize)]
pub struct WebhookEndpointConfig {
    pub name: String,
    pub url: String,
    pub events: Vec<String>,
    #[serde(default)]
    pub batch_size: Option<usize>,
    #[serde(default)]
    pub secret_env: Option<String>,
}

#[derive(Debug)]
struct Endpoint {
    id: usize,
    name: String,
    url: Url,
    batch_size: usize,
    secret: Option<String>,
}

#[derive(Debug)]
struct ResolvedEndpoints {
    routes: EndpointRoutes,        // event -> endpoints
    endpoints: Vec<Arc<Endpoint>>, // sorted by id
}

#[derive(Serialize)]
struct BatchPayload<'a> {
    batch_id: &'a str,
    events: &'a [LogEvent],
}

struct EndpointBatch {
    endpoint: Arc<Endpoint>,
    events: Vec<LogEvent>,
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

struct DroppedEventContext<'a> {
    event: &'a str,
    sandbox_id: Option<&'a str>,
}

fn dropped_event_context(event: &LogEvent) -> DroppedEventContext<'_> {
    DroppedEventContext {
        event: &event.event,
        sandbox_id: event
            .fields
            .get("sandbox_id")
            .and_then(serde_json::Value::as_str),
    }
}

impl HttpLogger {
    pub fn new(config: HttpLoggerConfig) -> anyhow::Result<Self> {
        validate_delivery(&config.delivery)?;
        let resolved =
            resolve_endpoint_routes(config.endpoints, config.delivery.default_batch_size)?;
        let routes = Arc::new(resolved.routes);
        let client = Client::builder()
            .timeout(Duration::from_secs(config.delivery.request_timeout_secs))
            .build()
            .context("build webhook HTTP client")?;
        let (tx, rx) = mpsc::channel(config.delivery.event_queue_capacity);
        spawn_worker(
            rx,
            client,
            routes.clone(),
            resolved.endpoints,
            config.delivery,
        );
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
            Err(mpsc::error::TrySendError::Full(Msg::Event(event))) => {
                let context = dropped_event_context(&event);
                error!(
                    event = %context.event,
                    sandbox_id = ?context.sandbox_id,
                    "HttpLogger: webhook queue is full, dropping event"
                );
            }
            Err(mpsc::error::TrySendError::Closed(Msg::Event(event))) => {
                let context = dropped_event_context(&event);
                error!(
                    event = %context.event,
                    sandbox_id = ?context.sandbox_id,
                    "HttpLogger: webhook worker is gone, dropping event"
                );
            }
            Err(mpsc::error::TrySendError::Full(Msg::Flush(_)))
            | Err(mpsc::error::TrySendError::Closed(Msg::Flush(_))) => {
                unreachable!("HttpLogger only sends Event messages with try_send");
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

fn default_event_queue_capacity() -> usize {
    10_000
}
fn default_batch_size() -> usize {
    1
}
fn default_flush_interval_secs() -> u64 {
    5
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
fn default_max_outstanding_deliveries() -> usize {
    1_000
}
fn default_max_concurrent_requests() -> usize {
    100
}

fn validate_delivery(delivery: &DeliveryConfig) -> anyhow::Result<()> {
    if delivery.event_queue_capacity == 0 {
        bail!("webhook delivery event_queue_capacity must be greater than 0");
    }
    if delivery.default_batch_size == 0 {
        bail!("webhook delivery default_batch_size must be greater than 0");
    }
    if delivery.flush_interval_secs == 0 {
        bail!("webhook delivery flush_interval_secs must be greater than 0");
    }
    if delivery.request_timeout_secs == 0 {
        bail!("webhook delivery request_timeout_secs must be greater than 0");
    }
    if delivery.max_attempts == 0 {
        bail!("webhook delivery max_attempts must be greater than 0");
    }
    if delivery.max_outstanding_deliveries == 0 {
        bail!("webhook delivery max_outstanding_deliveries must be greater than 0");
    }
    if delivery.max_concurrent_requests == 0 {
        bail!("webhook delivery max_concurrent_requests must be greater than 0");
    }
    Ok(())
}

fn resolve_endpoint_routes(
    configs: Vec<WebhookEndpointConfig>,
    default_batch_size: usize,
) -> anyhow::Result<ResolvedEndpoints> {
    if configs.is_empty() {
        bail!("webhook config must contain at least one endpoint");
    }

    let mut routes: EndpointRoutes = HashMap::new();
    let mut endpoints = Vec::with_capacity(configs.len());
    let mut seen_endpoint_events = HashSet::new();

    for (id, cfg) in configs.into_iter().enumerate() {
        let name = cfg.name.trim();
        if name.is_empty() {
            bail!("webhook endpoint name must not be empty");
        }

        let url =
            Url::parse(&cfg.url).with_context(|| format!("parse webhook endpoint {name} URL"))?;
        if url.scheme() != "http" && url.scheme() != "https" {
            bail!("webhook endpoint {name} URL must use http or https");
        }
        let url_key = url.as_str().to_string();

        let events: HashSet<_> = cfg
            .events
            .into_iter()
            .map(|event| event.trim().to_string())
            .filter(|event| !event.is_empty())
            .collect();
        if events.is_empty() {
            bail!("webhook endpoint {name} must subscribe to at least one event");
        }

        let batch_size = cfg.batch_size.unwrap_or(default_batch_size);
        if batch_size == 0 {
            bail!("webhook endpoint {name} batch_size must be greater than 0");
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
            id,
            name: name.to_string(),
            url,
            batch_size,
            secret,
        });

        for event in events {
            if !seen_endpoint_events.insert((url_key.clone(), event.clone())) {
                bail!(
                    "duplicate webhook endpoint subscription for url {} and event {}",
                    endpoint.url,
                    event
                );
            }
            routes.entry(event).or_default().push(endpoint.clone());
        }
        endpoints.push(endpoint);
    }

    Ok(ResolvedEndpoints { routes, endpoints })
}

fn spawn_worker(
    mut rx: mpsc::Receiver<Msg>,
    client: Client,
    routes: Arc<EndpointRoutes>,
    endpoints: Vec<Arc<Endpoint>>,
    delivery: DeliveryConfig,
) {
    tokio::spawn(async move {
        let request_semaphore = Arc::new(Semaphore::new(delivery.max_concurrent_requests));
        let mut tasks = JoinSet::new();
        let mut buffers: Vec<EndpointBatch> = endpoints
            .into_iter()
            .map(|endpoint| EndpointBatch {
                events: Vec::with_capacity(endpoint.batch_size),
                endpoint,
            })
            .collect();
        let mut flush_interval =
            tokio::time::interval(Duration::from_secs(delivery.flush_interval_secs));
        flush_interval.set_missed_tick_behavior(MissedTickBehavior::Delay);
        flush_interval.tick().await;

        loop {
            tokio::select! {
                msg = rx.recv() => {
                    match msg {
                        Some(Msg::Event(event)) => {
                            enqueue_event(
                                &mut buffers,
                                &mut tasks,
                                client.clone(),
                                routes.clone(),
                                request_semaphore.clone(),
                                delivery.clone(),
                                event,
                            ).await;
                        }
                        Some(Msg::Flush(reply)) => {
                            flush_all_buffers(
                                &mut buffers,
                                &mut tasks,
                                client.clone(),
                                request_semaphore.clone(),
                                delivery.clone(),
                            ).await;
                            while tasks.len() > 0 {
                                if let Some(result) = tasks.join_next().await {
                                    if let Err(err) = result {
                                        error!("HttpLogger: delivery task failed: {}", err);
                                    }
                                }
                            }
                            let _ = reply.send(());
                        }
                        None => {
                            flush_all_buffers(
                                &mut buffers,
                                &mut tasks,
                                client.clone(),
                                request_semaphore.clone(),
                                delivery.clone(),
                            ).await;
                            break;
                        }
                    }
                }
                _ = flush_interval.tick() => {
                    flush_all_buffers(
                        &mut buffers,
                        &mut tasks,
                        client.clone(),
                        request_semaphore.clone(),
                        delivery.clone(),
                    ).await;
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

async fn wait_for_delivery_task_capacity(tasks: &mut JoinSet<()>, capacity: usize) {
    while tasks.len() >= capacity {
        let Some(result) = tasks.join_next().await else {
            break;
        };
        if let Err(err) = result {
            error!("HttpLogger: delivery task failed: {}", err);
        }
    }
}

async fn enqueue_event(
    buffers: &mut [EndpointBatch],
    tasks: &mut JoinSet<()>,
    client: Client,
    routes: Arc<EndpointRoutes>,
    request_semaphore: Arc<Semaphore>,
    delivery: DeliveryConfig,
    event: LogEvent,
) {
    let Some(endpoints) = routes.get(&event.event) else {
        return;
    };

    for endpoint in endpoints.iter().cloned() {
        let endpoint_id = endpoint.id;
        let Some(batch) = buffers.get_mut(endpoint_id) else {
            error!(
                endpoint = %endpoint.name,
                endpoint_id,
                "HttpLogger: endpoint batch buffer missing, dropping event"
            );
            continue;
        };
        batch.events.push(event.clone());

        if batch.events.len() >= batch.endpoint.batch_size {
            let endpoint = batch.endpoint.clone();
            let events = std::mem::replace(
                &mut batch.events,
                Vec::with_capacity(batch.endpoint.batch_size),
            );
            wait_for_delivery_task_capacity(tasks, delivery.max_outstanding_deliveries).await;
            spawn_batch_delivery(
                tasks,
                client.clone(),
                request_semaphore.clone(),
                delivery.clone(),
                endpoint,
                events,
            );
        }
    }
}

async fn flush_all_buffers(
    buffers: &mut [EndpointBatch],
    tasks: &mut JoinSet<()>,
    client: Client,
    request_semaphore: Arc<Semaphore>,
    delivery: DeliveryConfig,
) {
    for batch in buffers.iter_mut() {
        if batch.events.is_empty() {
            continue;
        }
        let endpoint = batch.endpoint.clone();
        let events = std::mem::replace(
            &mut batch.events,
            Vec::with_capacity(batch.endpoint.batch_size),
        );
        wait_for_delivery_task_capacity(tasks, delivery.max_outstanding_deliveries).await;
        spawn_batch_delivery(
            tasks,
            client.clone(),
            request_semaphore.clone(),
            delivery.clone(),
            endpoint,
            events,
        );
    }
}

fn spawn_batch_delivery(
    tasks: &mut JoinSet<()>,
    client: Client,
    request_semaphore: Arc<Semaphore>,
    delivery: DeliveryConfig,
    endpoint: Arc<Endpoint>,
    events: Vec<LogEvent>,
) {
    if events.is_empty() {
        return;
    }

    let batch_id = Uuid::new_v4().to_string();
    let event_count = events.len();
    let body = match serde_json::to_vec(&BatchPayload {
        batch_id: &batch_id,
        events: &events,
    }) {
        Ok(body) => body,
        Err(err) => {
            error!(
                batch_id = %batch_id,
                event_count,
                "HttpLogger: failed to serialise webhook batch: {}",
                err
            );
            return;
        }
    };

    tasks.spawn(async move {
        deliver_with_retry(
            client,
            endpoint,
            request_semaphore,
            body,
            batch_id,
            event_count,
            delivery,
        )
        .await;
    });
}

async fn deliver_with_retry(
    client: Client,
    endpoint: Arc<Endpoint>,
    request_semaphore: Arc<Semaphore>,
    body: Vec<u8>,
    batch_id: String,
    event_count: usize,
    delivery: DeliveryConfig,
) {
    for attempt in 1..=delivery.max_attempts {
        let result = {
            // Limit only the HTTP attempt; retry backoff must not consume a permit.
            let permit = request_semaphore.clone().acquire_owned().await;
            let Ok(_permit_guard) = permit else {
                error!("HttpLogger: webhook request semaphore closed, dropping delivery");
                return;
            };
            send_once(&client, &endpoint, &body).await
        };

        match result {
            Ok(status) if status.is_success() => return,
            Ok(status) => {
                if !is_retriable_status(status) || attempt == delivery.max_attempts {
                    error!(
                        endpoint = %endpoint.name,
                        batch_id = %batch_id,
                        event_count,
                        attempts = attempt,
                        status = %status,
                        "HttpLogger: webhook delivery failed"
                    );
                    return;
                }
                warn!(
                    endpoint = %endpoint.name,
                    batch_id = %batch_id,
                    event_count,
                    attempt,
                    status = %status,
                    "HttpLogger: webhook delivery retrying"
                );
            }
            Err(err) => {
                if attempt == delivery.max_attempts {
                    error!(
                        endpoint = %endpoint.name,
                        batch_id = %batch_id,
                        event_count,
                        attempts = attempt,
                        error = %err,
                        "HttpLogger: webhook delivery failed"
                    );
                    return;
                }
                warn!(
                    endpoint = %endpoint.name,
                    batch_id = %batch_id,
                    event_count,
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
) -> Result<StatusCode, reqwest::Error> {
    let mut request = client
        .post(endpoint.url.clone())
        .header(CONTENT_TYPE, "application/json")
        .header(USER_AGENT, "CubeSandbox-Webhook/1.0")
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

    const EVENTUAL_DELIVERY_WAIT: Duration = Duration::from_secs(2);
    const ABSENT_DELIVERY_WAIT: Duration = Duration::from_millis(50);
    const RETRY_BACKOFF_GUARD_WAIT: Duration = Duration::from_secs(2);

    fn test_config(url: String, events: Vec<&str>) -> HttpLoggerConfig {
        HttpLoggerConfig {
            delivery: DeliveryConfig {
                event_queue_capacity: 16,
                default_batch_size: 1,
                flush_interval_secs: 5,
                request_timeout_secs: 1,
                max_attempts: 3,
                initial_backoff_ms: 1,
                max_backoff_secs: 1,
                max_outstanding_deliveries: 16,
                max_concurrent_requests: 4,
            },
            endpoints: vec![WebhookEndpointConfig {
                name: "test".to_string(),
                url,
                events: events.into_iter().map(str::to_string).collect(),
                batch_size: None,
                secret_env: None,
            }],
        }
    }

    #[test]
    fn parses_toml_config() {
        let raw = r#"
            [delivery]
            event_queue_capacity = 99
            max_outstanding_deliveries = 55
            max_concurrent_requests = 11
            default_batch_size = 2
            flush_interval_secs = 7
            request_timeout_secs = 3
            max_attempts = 4
            initial_backoff_ms = 250
            max_backoff_secs = 8

            [[endpoints]]
            name = "local"
            url = "http://127.0.0.1:8088/webhook"
            events = ["sandbox.created", "api.error"]
            batch_size = 3
            secret_env = "CUBE_WEBHOOK_SECRET_0"
        "#;

        let cfg = HttpLoggerConfig::from_toml_str(raw).unwrap();

        assert_eq!(cfg.delivery.event_queue_capacity, 99);
        assert_eq!(cfg.delivery.max_outstanding_deliveries, 55);
        assert_eq!(cfg.delivery.max_concurrent_requests, 11);
        assert_eq!(cfg.delivery.default_batch_size, 2);
        assert_eq!(cfg.delivery.flush_interval_secs, 7);
        assert_eq!(cfg.delivery.max_attempts, 4);
        assert_eq!(cfg.endpoints.len(), 1);
        assert_eq!(cfg.endpoints[0].name, "local");
        assert_eq!(cfg.endpoints[0].batch_size, Some(3));
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
    fn delivery_capacity_defaults_are_stable() {
        let delivery = DeliveryConfig::default();

        assert_eq!(delivery.event_queue_capacity, 10_000);
        assert_eq!(delivery.max_outstanding_deliveries, 1_000);
        assert_eq!(delivery.max_concurrent_requests, 100);
    }

    #[test]
    fn rejects_zero_delivery_capacity_values() {
        let mut delivery = DeliveryConfig::default();
        delivery.event_queue_capacity = 0;
        assert!(validate_delivery(&delivery).is_err());

        let mut delivery = DeliveryConfig::default();
        delivery.max_outstanding_deliveries = 0;
        assert!(validate_delivery(&delivery).is_err());

        let mut delivery = DeliveryConfig::default();
        delivery.max_concurrent_requests = 0;
        assert!(validate_delivery(&delivery).is_err());
    }

    #[tokio::test]
    async fn waits_for_outstanding_delivery_capacity() {
        let mut tasks = JoinSet::new();
        let (release_tx, release_rx) = oneshot::channel();
        tasks.spawn(async move {
            let _ = release_rx.await;
        });

        {
            let waiting = wait_for_delivery_task_capacity(&mut tasks, 1);
            tokio::pin!(waiting);
            assert!(
                tokio::time::timeout(Duration::from_millis(50), &mut waiting)
                    .await
                    .is_err()
            );

            release_tx.send(()).unwrap();
            tokio::time::timeout(Duration::from_secs(1), &mut waiting)
                .await
                .expect("capacity wait should finish after a delivery completes");
        }

        assert_eq!(tasks.len(), 0);
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
                batch_size: None,
                secret_env: None,
            },
            WebhookEndpointConfig {
                name: "alerts".to_string(),
                url: "http://127.0.0.1:8080/alerts".to_string(),
                events: vec!["sandbox.created".to_string()],
                batch_size: None,
                secret_env: None,
            },
        ];

        let resolved = resolve_endpoint_routes(configs, 1).unwrap();

        assert_eq!(resolved.routes["sandbox.created"].len(), 2);
        assert_eq!(resolved.routes["api.error"].len(), 1);
        assert_eq!(resolved.routes["api.error"][0].name, "audit");
        assert_eq!(resolved.endpoints.len(), 2);
        assert_eq!(resolved.endpoints[0].batch_size, 1);
    }

    #[test]
    fn rejects_duplicate_url_event_pairs_across_endpoints() {
        let configs = vec![
            WebhookEndpointConfig {
                name: "lifecycle".to_string(),
                url: "http://127.0.0.1:8080/webhook".to_string(),
                events: vec!["sandbox.created".to_string()],
                batch_size: None,
                secret_env: None,
            },
            WebhookEndpointConfig {
                name: "api".to_string(),
                url: "http://127.0.0.1:8080/webhook".to_string(),
                events: vec!["sandbox.created".to_string(), "api.request".to_string()],
                batch_size: Some(10),
                secret_env: None,
            },
        ];

        let err = resolve_endpoint_routes(configs, 1).unwrap_err();

        assert!(err
            .to_string()
            .contains("duplicate webhook endpoint subscription"));
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

    #[test]
    fn dropped_event_context_includes_event_and_sandbox_id() {
        let event = LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sbx-1");

        let context = dropped_event_context(&event);

        assert_eq!(context.event, "sandbox.created");
        assert_eq!(context.sandbox_id, Some("sbx-1"));
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
        assert!(!requests[0].headers.contains_key("x-cube-event"));
        assert!(!requests[0].headers.contains_key("x-cube-event-id"));
        let body: serde_json::Value = serde_json::from_str(&requests[0].body).unwrap();
        assert!(body["batch_id"].as_str().is_some());
        assert_eq!(body["events"].as_array().unwrap().len(), 1);
        assert_eq!(body["events"][0]["event"], "sandbox.created");
        assert_eq!(body["events"][0]["sandbox_id"], "sbx-1");
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
    async fn sends_batch_envelope_across_fanout_and_rotates_between_events() {
        let (audit_url, audit_recorder) = spawn_recorder(vec![StatusCode::NO_CONTENT], None).await;
        let (ops_url, ops_recorder) = spawn_recorder(vec![StatusCode::NO_CONTENT], None).await;
        let logger = HttpLogger::new(HttpLoggerConfig {
            delivery: DeliveryConfig {
                event_queue_capacity: 16,
                default_batch_size: 1,
                flush_interval_secs: 5,
                request_timeout_secs: 1,
                max_attempts: 1,
                initial_backoff_ms: 1,
                max_backoff_secs: 1,
                max_outstanding_deliveries: 16,
                max_concurrent_requests: 4,
            },
            endpoints: vec![
                WebhookEndpointConfig {
                    name: "audit".to_string(),
                    url: audit_url,
                    events: vec!["sandbox.created".to_string()],
                    batch_size: None,
                    secret_env: None,
                },
                WebhookEndpointConfig {
                    name: "ops".to_string(),
                    url: ops_url,
                    events: vec!["sandbox.created".to_string()],
                    batch_size: None,
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
        assert!(!audit_requests[0].headers.contains_key("x-cube-event"));
        assert!(!ops_requests[0].headers.contains_key("x-cube-event"));
        assert!(!audit_requests[0].headers.contains_key("x-cube-event-id"));
        assert!(!ops_requests[0].headers.contains_key("x-cube-event-id"));
        assert!(!audit_requests[0].headers.contains_key("x-cube-delivery"));
        assert!(!ops_requests[0].headers.contains_key("x-cube-delivery"));
        let audit_body: serde_json::Value = serde_json::from_str(&audit_requests[0].body).unwrap();
        let ops_body: serde_json::Value = serde_json::from_str(&ops_requests[0].body).unwrap();
        let audit_batch_id = audit_body["batch_id"].as_str().unwrap().to_string();
        let ops_batch_id = ops_body["batch_id"].as_str().unwrap().to_string();
        assert!(!audit_batch_id.is_empty());
        assert!(!ops_batch_id.is_empty());
        assert_eq!(audit_body["events"].as_array().unwrap().len(), 1);
        assert_eq!(ops_body["events"].as_array().unwrap().len(), 1);
        assert_eq!(audit_body["events"][0]["event"], "sandbox.created");
        assert_eq!(ops_body["events"][0]["event"], "sandbox.created");
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
        let second_audit_body: serde_json::Value =
            serde_json::from_str(&audit_requests[1].body).unwrap();
        let second_ops_body: serde_json::Value =
            serde_json::from_str(&ops_requests[1].body).unwrap();
        let second_audit_batch_id = second_audit_body["batch_id"].as_str().unwrap();
        let second_ops_batch_id = second_ops_body["batch_id"].as_str().unwrap();
        assert_ne!(audit_batch_id, second_audit_batch_id);
        assert_ne!(ops_batch_id, second_ops_batch_id);
        assert_eq!(second_audit_body["events"][0]["sandbox_id"], "sbx-2");
        assert_eq!(second_ops_body["events"][0]["sandbox_id"], "sbx-2");
    }

    #[tokio::test]
    async fn buffers_events_until_batch_size_is_reached() {
        let (url, recorder) = spawn_recorder(vec![StatusCode::NO_CONTENT], None).await;
        let logger = HttpLogger::new(HttpLoggerConfig {
            delivery: DeliveryConfig {
                event_queue_capacity: 16,
                default_batch_size: 2,
                flush_interval_secs: 60,
                request_timeout_secs: 1,
                max_attempts: 1,
                initial_backoff_ms: 1,
                max_backoff_secs: 1,
                max_outstanding_deliveries: 16,
                max_concurrent_requests: 4,
            },
            endpoints: vec![WebhookEndpointConfig {
                name: "test".to_string(),
                url,
                events: vec!["sandbox.created".to_string()],
                batch_size: None,
                secret_env: None,
            }],
        })
        .unwrap();

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sbx-1"))
            .await;
        assert!(!wait_for_count(&recorder, 1, ABSENT_DELIVERY_WAIT).await);

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sbx-2"))
            .await;
        assert!(wait_for_count(&recorder, 1, EVENTUAL_DELIVERY_WAIT).await);
        logger.flush().await;

        let requests = recorder.requests.lock().unwrap();
        assert_eq!(requests.len(), 1);
        let body: serde_json::Value = serde_json::from_str(&requests[0].body).unwrap();
        assert!(body["batch_id"].as_str().is_some());
        assert_eq!(body["events"].as_array().unwrap().len(), 2);
        assert_eq!(body["events"][0]["sandbox_id"], "sbx-1");
        assert_eq!(body["events"][1]["sandbox_id"], "sbx-2");
    }

    #[tokio::test]
    async fn endpoint_batch_size_overrides_delivery_default() {
        let (url, recorder) = spawn_recorder(vec![StatusCode::NO_CONTENT], None).await;
        let logger = HttpLogger::new(HttpLoggerConfig {
            delivery: DeliveryConfig {
                event_queue_capacity: 16,
                default_batch_size: 1,
                flush_interval_secs: 60,
                request_timeout_secs: 1,
                max_attempts: 1,
                initial_backoff_ms: 1,
                max_backoff_secs: 1,
                max_outstanding_deliveries: 16,
                max_concurrent_requests: 4,
            },
            endpoints: vec![
                WebhookEndpointConfig {
                    name: "lifecycle".to_string(),
                    url: url.clone(),
                    events: vec!["sandbox.created".to_string()],
                    batch_size: None,
                    secret_env: None,
                },
                WebhookEndpointConfig {
                    name: "api".to_string(),
                    url,
                    events: vec!["api.request".to_string()],
                    batch_size: Some(2),
                    secret_env: None,
                },
            ],
        })
        .unwrap();

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sbx-1"))
            .await;
        assert!(wait_for_count(&recorder, 1, EVENTUAL_DELIVERY_WAIT).await);

        logger
            .log(LogEvent::new(LogLevel::Debug, "api.request").field("handler", "health"))
            .await;
        assert!(!wait_for_count(&recorder, 2, ABSENT_DELIVERY_WAIT).await);

        logger
            .log(LogEvent::new(LogLevel::Debug, "api.request").field("handler", "list"))
            .await;
        assert!(wait_for_count(&recorder, 2, EVENTUAL_DELIVERY_WAIT).await);
        logger.flush().await;

        let requests = recorder.requests.lock().unwrap();
        assert_eq!(requests.len(), 2);
        let lifecycle_body: serde_json::Value = serde_json::from_str(&requests[0].body).unwrap();
        let api_body: serde_json::Value = serde_json::from_str(&requests[1].body).unwrap();
        assert_eq!(lifecycle_body["events"].as_array().unwrap().len(), 1);
        assert_eq!(lifecycle_body["events"][0]["event"], "sandbox.created");
        assert_eq!(api_body["events"].as_array().unwrap().len(), 2);
        assert_eq!(api_body["events"][0]["event"], "api.request");
        assert_eq!(api_body["events"][1]["handler"], "list");
    }

    #[tokio::test]
    async fn flush_sends_partial_batch() {
        let (url, recorder) = spawn_recorder(vec![StatusCode::NO_CONTENT], None).await;
        let logger = HttpLogger::new(HttpLoggerConfig {
            delivery: DeliveryConfig {
                event_queue_capacity: 16,
                default_batch_size: 2,
                flush_interval_secs: 60,
                request_timeout_secs: 1,
                max_attempts: 1,
                initial_backoff_ms: 1,
                max_backoff_secs: 1,
                max_outstanding_deliveries: 16,
                max_concurrent_requests: 4,
            },
            endpoints: vec![WebhookEndpointConfig {
                name: "test".to_string(),
                url,
                events: vec!["sandbox.created".to_string()],
                batch_size: None,
                secret_env: None,
            }],
        })
        .unwrap();

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sbx-1"))
            .await;
        logger.flush().await;

        let requests = recorder.requests.lock().unwrap();
        assert_eq!(requests.len(), 1);
        let body: serde_json::Value = serde_json::from_str(&requests[0].body).unwrap();
        assert_eq!(body["events"].as_array().unwrap().len(), 1);
        assert_eq!(body["events"][0]["sandbox_id"], "sbx-1");
    }

    #[tokio::test]
    async fn flush_interval_sends_partial_batch() {
        let (url, recorder) = spawn_recorder(vec![StatusCode::NO_CONTENT], None).await;
        let logger = HttpLogger::new(HttpLoggerConfig {
            delivery: DeliveryConfig {
                event_queue_capacity: 16,
                default_batch_size: 2,
                flush_interval_secs: 1,
                request_timeout_secs: 1,
                max_attempts: 1,
                initial_backoff_ms: 1,
                max_backoff_secs: 1,
                max_outstanding_deliveries: 16,
                max_concurrent_requests: 4,
            },
            endpoints: vec![WebhookEndpointConfig {
                name: "test".to_string(),
                url,
                events: vec!["sandbox.created".to_string()],
                batch_size: None,
                secret_env: None,
            }],
        })
        .unwrap();

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sbx-1"))
            .await;
        assert!(wait_for_count(&recorder, 1, Duration::from_millis(2500)).await);
        logger.flush().await;

        let requests = recorder.requests.lock().unwrap();
        assert_eq!(requests.len(), 1);
        let body: serde_json::Value = serde_json::from_str(&requests[0].body).unwrap();
        assert_eq!(body["events"].as_array().unwrap().len(), 1);
        assert_eq!(body["events"][0]["sandbox_id"], "sbx-1");
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
        let first_body: serde_json::Value = serde_json::from_str(&requests[0].body).unwrap();
        let second_body: serde_json::Value = serde_json::from_str(&requests[1].body).unwrap();
        assert_eq!(first_body["batch_id"], second_body["batch_id"]);
        assert_eq!(first_body["events"].as_array().unwrap().len(), 1);
        assert_eq!(second_body["events"].as_array().unwrap().len(), 1);
        assert!(!requests[0].headers.contains_key("x-cube-delivery"));
        assert!(!requests[1].headers.contains_key("x-cube-delivery"));
        assert!(!requests[0].headers.contains_key("x-cube-event"));
        assert!(!requests[1].headers.contains_key("x-cube-event"));
        assert!(!requests[0].headers.contains_key("x-cube-event-id"));
        assert!(!requests[1].headers.contains_key("x-cube-event-id"));
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
                event_queue_capacity: 16,
                default_batch_size: 1,
                flush_interval_secs: 5,
                request_timeout_secs: 1,
                max_attempts: 2,
                initial_backoff_ms: 3_000,
                max_backoff_secs: 3,
                max_outstanding_deliveries: 16,
                max_concurrent_requests: 1,
            },
            endpoints: vec![
                WebhookEndpointConfig {
                    name: "failing".to_string(),
                    url: failing_url,
                    events: vec!["api.error".to_string()],
                    batch_size: None,
                    secret_env: None,
                },
                WebhookEndpointConfig {
                    name: "healthy".to_string(),
                    url: healthy_url,
                    events: vec!["sandbox.created".to_string()],
                    batch_size: None,
                    secret_env: None,
                },
            ],
        })
        .unwrap();

        logger
            .log(LogEvent::new(LogLevel::Error, "api.error").field("handler", "create_sandbox"))
            .await;
        assert!(wait_for_count(&failing_recorder, 1, EVENTUAL_DELIVERY_WAIT).await);
        tokio::time::sleep(Duration::from_millis(10)).await;

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sbx-1"))
            .await;
        assert!(
            wait_for_count(&healthy_recorder, 1, RETRY_BACKOFF_GUARD_WAIT).await,
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

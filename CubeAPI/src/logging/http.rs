// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Best-effort HTTP webhook log backend.
//!
//! Events are accepted through a bounded channel and dispatched from a
//! background Tokio task. `Logger::log()` only attempts to enqueue an event;
//! it never waits for queue capacity or performs network I/O.

use std::{collections::HashMap, sync::Arc, time::Duration};

use anyhow::{anyhow, bail};
use async_trait::async_trait;
use chrono::{DateTime, Utc};
use hmac::{Hmac, Mac};
use rand::Rng;
use reqwest::{redirect::Policy, StatusCode, Url};
use serde::Serialize;
use serde_json::Value;
use sha2::Sha256;
use tokio::{
    runtime::Handle,
    sync::{mpsc, oneshot, Semaphore},
    task::{JoinError, JoinSet},
    time::{sleep, timeout},
};
use tracing::{error, warn};
use uuid::Uuid;

use crate::config::{WebhookConfig, WebhookEndpointConfig};

use super::{LogEvent, LogLevel, Logger};

const HEADER_EVENT: &str = "X-Cube-Webhook-Event";
const HEADER_DELIVERY: &str = "X-Cube-Webhook-Delivery";
const HEADER_TIMESTAMP: &str = "X-Cube-Webhook-Timestamp";
const HEADER_SIGNATURE: &str = "X-Cube-Webhook-Signature";

const DEFAULT_LIFECYCLE_EVENTS: [&str; 4] = [
    "sandbox.created",
    "sandbox.deleted",
    "sandbox.paused",
    "sandbox.resumed",
];

enum Msg {
    Event(LogEvent),
    Flush(oneshot::Sender<()>),
}

#[derive(Serialize)]
struct WebhookPayload {
    id: String,
    timestamp: DateTime<Utc>,
    level: LogLevel,
    event: String,
    #[serde(flatten)]
    fields: HashMap<String, Value>,
}

#[derive(Clone)]
struct Endpoint {
    index: usize,
    url: Url,
    events: Vec<String>,
    secret: Option<String>,
}

impl Endpoint {
    fn matches(&self, event: &str) -> bool {
        events_match(&self.events, event)
    }
}

struct Delivery {
    endpoint_index: usize,
    url: Url,
    event: String,
    id: String,
    timestamp: String,
    body: Vec<u8>,
    signature: Option<String>,
}

impl Delivery {
    fn new(
        event: &LogEvent,
        endpoint: &Endpoint,
        fields: &HashMap<String, Value>,
    ) -> anyhow::Result<Self> {
        let id = Uuid::new_v4().to_string();
        let timestamp = event.timestamp.timestamp().to_string();
        let payload = WebhookPayload {
            id: id.clone(),
            timestamp: event.timestamp,
            level: event.level,
            event: event.event.clone(),
            fields: fields.clone(),
        };
        let body = serde_json::to_vec(&payload)?;
        let signature = endpoint
            .secret
            .as_deref()
            .map(|secret| sign_payload(secret, &timestamp, &id, &body))
            .transpose()?;

        Ok(Self {
            endpoint_index: endpoint.index,
            url: endpoint.url.clone(),
            event: event.event.clone(),
            id,
            timestamp,
            body,
            signature,
        })
    }
}

#[derive(Clone, Copy)]
struct DeliveryOptions {
    max_retries: usize,
    initial_backoff_ms: u64,
    max_backoff_ms: u64,
}

/// Asynchronous best-effort HTTP webhook backend.
#[derive(Clone)]
pub struct HttpLogger {
    tx: mpsc::Sender<Msg>,
    flush_timeout: Duration,
}

impl HttpLogger {
    /// Validate the configuration, create the shared HTTP client, and start
    /// the background dispatcher.
    pub fn new(config: WebhookConfig) -> anyhow::Result<Self> {
        validate_config(&config)?;

        let endpoints = config
            .endpoints
            .iter()
            .enumerate()
            .filter(|(_, endpoint)| endpoint.enabled)
            .map(|(index, endpoint)| compile_endpoint(endpoint, index))
            .collect::<anyhow::Result<Vec<_>>>()?;

        let client = reqwest::Client::builder()
            .redirect(Policy::none())
            .timeout(Duration::from_secs(config.timeout_secs))
            .build()?;
        let handle = Handle::try_current()
            .map_err(|_| anyhow!("HttpLogger::new must be called from a Tokio runtime"))?;

        let queue_capacity = config.queue_capacity;
        let max_concurrency = config.max_concurrency;
        let flush_timeout = Duration::from_secs(config.flush_timeout_secs);
        let max_outstanding =
            max_outstanding_deliveries(queue_capacity, endpoints.len(), max_concurrency);
        let options = DeliveryOptions {
            max_retries: config.max_retries,
            initial_backoff_ms: config.initial_backoff_ms,
            max_backoff_ms: config.max_backoff_ms,
        };
        let (tx, rx) = mpsc::channel(queue_capacity);

        handle.spawn(run_dispatcher(
            rx,
            Arc::new(endpoints),
            client,
            Arc::new(Semaphore::new(max_concurrency)),
            options,
            flush_timeout,
            max_outstanding,
        ));

        Ok(Self { tx, flush_timeout })
    }
}

#[async_trait]
impl Logger for HttpLogger {
    async fn log(&self, event: LogEvent) {
        let event_name = event.event.clone();
        match self.tx.try_send(Msg::Event(event)) {
            Ok(()) => {}
            Err(mpsc::error::TrySendError::Full(_)) => {
                warn!(
                    event = %event_name,
                    "HttpLogger queue is full; dropping webhook event"
                );
            }
            Err(mpsc::error::TrySendError::Closed(_)) => {
                warn!(
                    event = %event_name,
                    "HttpLogger dispatcher is closed; dropping webhook event"
                );
            }
        }
    }

    async fn flush(&self) {
        let (reply_tx, reply_rx) = oneshot::channel();
        let completed = timeout(self.flush_timeout, async {
            if self.tx.send(Msg::Flush(reply_tx)).await.is_err() {
                return false;
            }
            reply_rx.await.is_ok()
        })
        .await;

        match completed {
            Ok(true) => {}
            Ok(false) => warn!("HttpLogger dispatcher closed before flush completed"),
            Err(_) => warn!(
                timeout_secs = self.flush_timeout.as_secs(),
                "HttpLogger flush timed out"
            ),
        }
    }

    fn name(&self) -> &'static str {
        "http"
    }
}

const MAX_OUTSTANDING_DELIVERY_TASKS: usize = 100_000;

fn max_outstanding_deliveries(
    queue_capacity: usize,
    endpoint_count: usize,
    max_concurrency: usize,
) -> usize {
    queue_capacity
        .saturating_mul(endpoint_count.max(1))
        .max(max_concurrency)
        .min(MAX_OUTSTANDING_DELIVERY_TASKS)
}

fn validate_config(config: &WebhookConfig) -> anyhow::Result<()> {
    if config.queue_capacity == 0 {
        bail!("webhook queue_capacity must be greater than zero");
    }
    if config.timeout_secs == 0 {
        bail!("webhook timeout_secs must be greater than zero");
    }
    if config.max_concurrency == 0 {
        bail!("webhook max_concurrency must be greater than zero");
    }
    if config.flush_timeout_secs == 0 {
        bail!("webhook flush_timeout_secs must be greater than zero");
    }
    if config.initial_backoff_ms > config.max_backoff_ms {
        bail!("webhook initial_backoff_ms must not exceed max_backoff_ms");
    }
    Ok(())
}

fn compile_endpoint(config: &WebhookEndpointConfig, index: usize) -> anyhow::Result<Endpoint> {
    if config.secret.as_deref() == Some("") {
        bail!("webhook endpoint {index} has an empty secret");
    }
    if config.events.iter().any(String::is_empty) {
        bail!("webhook endpoint {index} has an empty event name");
    }
    if config.events.iter().any(|event| event == "*") && config.events.len() != 1 {
        bail!("webhook endpoint {index} mixes '*' with explicit event names");
    }

    let url = Url::parse(&config.url)
        .map_err(|_| anyhow!("webhook endpoint {index} has an invalid URL"))?;
    if !matches!(url.scheme(), "http" | "https") || url.host_str().is_none() {
        bail!("webhook endpoint {index} must use an absolute HTTP(S) URL");
    }

    Ok(Endpoint {
        index,
        url,
        events: config.events.clone(),
        secret: config.secret.clone(),
    })
}

async fn run_dispatcher(
    mut rx: mpsc::Receiver<Msg>,
    endpoints: Arc<Vec<Endpoint>>,
    client: reqwest::Client,
    semaphore: Arc<Semaphore>,
    options: DeliveryOptions,
    flush_timeout: Duration,
    max_outstanding: usize,
) {
    let mut deliveries = JoinSet::new();

    loop {
        if deliveries.len() >= max_outstanding {
            if let Some(result) = deliveries.join_next().await {
                log_delivery_task_result(result);
            }
            continue;
        }

        tokio::select! {
            result = deliveries.join_next(), if !deliveries.is_empty() => {
                if let Some(result) = result {
                    log_delivery_task_result(result);
                }
            }
            message = rx.recv() => {
                match message {
                    Some(Msg::Event(event)) => {
                        spawn_deliveries(
                            &mut deliveries,
                            event,
                            &endpoints,
                            &client,
                            &semaphore,
                            options,
                        );
                    }
                    Some(Msg::Flush(reply)) => {
                        flush_deliveries(&mut deliveries, flush_timeout).await;
                        let _ = reply.send(());
                    }
                    None => {
                        flush_deliveries(&mut deliveries, flush_timeout).await;
                        break;
                    }
                }
            }
        }
    }
}

fn spawn_deliveries(
    deliveries: &mut JoinSet<()>,
    event: LogEvent,
    endpoints: &[Endpoint],
    client: &reqwest::Client,
    semaphore: &Arc<Semaphore>,
    options: DeliveryOptions,
) {
    let mut matching_endpoints = endpoints
        .iter()
        .filter(|endpoint| endpoint.matches(&event.event))
        .peekable();

    if matching_endpoints.peek().is_none() {
        return;
    }

    let fields = sanitized_fields(&event.fields);

    for endpoint in matching_endpoints {
        match Delivery::new(&event, endpoint, &fields) {
            Ok(delivery) => {
                deliveries.spawn(deliver_with_retry(
                    delivery,
                    client.clone(),
                    semaphore.clone(),
                    options,
                ));
            }
            Err(_) => error!(
                endpoint_index = endpoint.index,
                event = %event.event,
                "failed to build webhook delivery; dropping delivery"
            ),
        }
    }
}

async fn flush_deliveries(deliveries: &mut JoinSet<()>, flush_timeout: Duration) {
    let drain = async {
        while let Some(result) = deliveries.join_next().await {
            log_delivery_task_result(result);
        }
    };

    if timeout(flush_timeout, drain).await.is_err() {
        let pending = deliveries.len();
        warn!(
            pending,
            timeout_secs = flush_timeout.as_secs(),
            "HttpLogger delivery flush timed out; aborting pending deliveries"
        );
        deliveries.abort_all();
        while let Some(result) = deliveries.join_next().await {
            log_delivery_task_result(result);
        }
    }
}

fn log_delivery_task_result(result: Result<(), JoinError>) {
    if let Err(join_error) = result {
        error!(
            cancelled = join_error.is_cancelled(),
            panicked = join_error.is_panic(),
            "HttpLogger delivery task failed"
        );
    }
}

async fn deliver_with_retry(
    delivery: Delivery,
    client: reqwest::Client,
    semaphore: Arc<Semaphore>,
    options: DeliveryOptions,
) {
    for attempt in 0..=options.max_retries {
        let permit = match semaphore.acquire().await {
            Ok(permit) => permit,
            Err(_) => {
                error!(
                    endpoint_index = delivery.endpoint_index,
                    event = %delivery.event,
                    delivery_id = %delivery.id,
                    "HttpLogger concurrency limiter closed; dropping delivery"
                );
                return;
            }
        };
        let result = send_once(&client, &delivery).await;
        drop(permit);

        match result {
            Ok(status) if status.is_success() => return,
            Ok(status) if is_retryable_status(status) && attempt < options.max_retries => {
                warn!(
                    endpoint_index = delivery.endpoint_index,
                    event = %delivery.event,
                    delivery_id = %delivery.id,
                    status = status.as_u16(),
                    attempt = attempt + 1,
                    "webhook delivery failed; retrying"
                );
            }
            Ok(status) if is_retryable_status(status) => {
                error!(
                    endpoint_index = delivery.endpoint_index,
                    event = %delivery.event,
                    delivery_id = %delivery.id,
                    status = status.as_u16(),
                    attempts = attempt + 1,
                    "webhook delivery retries exhausted"
                );
                return;
            }
            Ok(status) => {
                warn!(
                    endpoint_index = delivery.endpoint_index,
                    event = %delivery.event,
                    delivery_id = %delivery.id,
                    status = status.as_u16(),
                    attempt = attempt + 1,
                    "webhook delivery rejected without retry"
                );
                return;
            }
            Err(request_error) if attempt < options.max_retries => {
                warn!(
                    endpoint_index = delivery.endpoint_index,
                    event = %delivery.event,
                    delivery_id = %delivery.id,
                    error_kind = request_error_kind(&request_error),
                    attempt = attempt + 1,
                    "webhook delivery failed; retrying"
                );
            }
            Err(request_error) => {
                error!(
                    endpoint_index = delivery.endpoint_index,
                    event = %delivery.event,
                    delivery_id = %delivery.id,
                    error_kind = request_error_kind(&request_error),
                    attempts = attempt + 1,
                    "webhook delivery retries exhausted"
                );
                return;
            }
        }

        sleep(backoff_delay(
            attempt,
            options.initial_backoff_ms,
            options.max_backoff_ms,
        ))
        .await;
    }
}

async fn send_once(
    client: &reqwest::Client,
    delivery: &Delivery,
) -> Result<StatusCode, reqwest::Error> {
    let mut request = client
        .post(delivery.url.clone())
        .header(reqwest::header::CONTENT_TYPE, "application/json")
        .header(HEADER_EVENT, &delivery.event)
        .header(HEADER_DELIVERY, &delivery.id)
        .header(HEADER_TIMESTAMP, &delivery.timestamp)
        .body(delivery.body.clone());

    if let Some(signature) = &delivery.signature {
        request = request.header(HEADER_SIGNATURE, signature);
    }

    request.send().await.map(|response| response.status())
}

#[cfg(test)]
fn endpoint_matches_event(endpoint: &WebhookEndpointConfig, event: &str) -> bool {
    endpoint.enabled && events_match(&endpoint.events, event)
}

fn events_match(events: &[String], event: &str) -> bool {
    if events.is_empty() {
        return is_default_lifecycle_event(event);
    }
    events
        .iter()
        .any(|configured| configured == "*" || configured == event)
}

fn is_default_lifecycle_event(event: &str) -> bool {
    DEFAULT_LIFECYCLE_EVENTS.contains(&event)
}

fn sign_payload(
    secret: &str,
    timestamp: &str,
    delivery_id: &str,
    body: &[u8],
) -> anyhow::Result<String> {
    let mut mac = Hmac::<Sha256>::new_from_slice(secret.as_bytes())
        .map_err(|_| anyhow!("failed to initialize HMAC-SHA256"))?;
    mac.update(timestamp.as_bytes());
    mac.update(b".");
    mac.update(delivery_id.as_bytes());
    mac.update(b".");
    mac.update(body);
    Ok(format!("v1={}", hex::encode(mac.finalize().into_bytes())))
}

fn is_retryable_status(status: StatusCode) -> bool {
    status == StatusCode::REQUEST_TIMEOUT
        || status.as_u16() == 425
        || status == StatusCode::TOO_MANY_REQUESTS
        || status.is_server_error()
}

fn backoff_delay(attempt: usize, initial_ms: u64, max_ms: u64) -> Duration {
    if initial_ms == 0 || max_ms == 0 {
        return Duration::from_millis(0);
    }

    let multiplier = 1_u64
        .checked_shl(attempt.min(63) as u32)
        .unwrap_or(u64::MAX);
    let base_ms = initial_ms.saturating_mul(multiplier).min(max_ms);

    if base_ms <= 1 {
        return Duration::from_millis(base_ms);
    }

    let jitter_ms = rand::thread_rng().gen_range(0..base_ms);
    let delay_ms = (base_ms / 2).saturating_add(jitter_ms).min(max_ms);

    Duration::from_millis(delay_ms)
}

fn request_error_kind(error: &reqwest::Error) -> &'static str {
    if error.is_timeout() {
        "timeout"
    } else if error.is_connect() {
        "connect"
    } else if error.is_request() {
        "request"
    } else if error.is_body() {
        "body"
    } else {
        "unknown"
    }
}

fn sanitized_fields(fields: &HashMap<String, Value>) -> HashMap<String, Value> {
    fields
        .iter()
        .filter(|(key, _)| !is_reserved_field(key) && !is_sensitive_field(key))
        .map(|(key, value)| (key.clone(), sanitize_value(value)))
        .collect()
}

fn sanitize_value(value: &Value) -> Value {
    match value {
        Value::Object(values) => Value::Object(
            values
                .iter()
                .filter(|(key, _)| !is_sensitive_field(key))
                .map(|(key, value)| (key.clone(), sanitize_value(value)))
                .collect(),
        ),
        Value::Array(values) => Value::Array(values.iter().map(sanitize_value).collect()),
        _ => value.clone(),
    }
}

fn is_reserved_field(key: &str) -> bool {
    matches!(key, "id" | "timestamp" | "level" | "event")
}

/// Returns true for field names that are likely to carry credentials or signing material.
///
/// The matcher intentionally avoids unrestricted substring checks. This keeps benign
/// metadata such as `token_bucket_config`, `password_reset_url`, or
/// `credentials_verification_status` from being stripped while still redacting common
/// credential field names and compound names such as `access_token`, `private_key`,
/// `db_password`, or `webhook_signature`.
fn is_sensitive_field(key: &str) -> bool {
    let normalized: String = key
        .chars()
        .map(|ch| match ch {
            '-' | '.' | ' ' => '_',
            _ => ch.to_ascii_lowercase(),
        })
        .collect();
    let normalized = normalized.trim_matches('_');

    if normalized.is_empty() {
        return false;
    }

    const EXACT_FIELDS: &[&str] = &[
        "api_key",
        "apikey",
        "api_token",
        "auth",
        "auth_header",
        "authorization",
        "authorization_header",
        "bearer",
        "bearer_token",
        "client_secret",
        "cookie",
        "credential",
        "credentials",
        "csrf",
        "csrf_token",
        "hashed_password",
        "id_token",
        "idtoken",
        "jwt",
        "jwt_token",
        "passphrase",
        "password",
        "password_digest",
        "password_hash",
        "passwd",
        "private_key",
        "private_key_pem",
        "privatekey",
        "refresh_token",
        "refreshtoken",
        "secret",
        "secret_key",
        "secret_value",
        "secretkey",
        "session_token",
        "set_cookie",
        "signature",
        "token",
        "token_value",
        "webhook_secret",
        "webhook_signature",
        "x_api_key",
        "xapikey",
    ];

    if EXACT_FIELDS.contains(&normalized) {
        return true;
    }

    const SENSITIVE_SUFFIXES: &[&str] = &[
        "_api_key",
        "_auth",
        "_authorization",
        "_bearer",
        "_cookie",
        "_credential",
        "_credentials",
        "_csrf",
        "_jwt",
        "_passphrase",
        "_password",
        "_passwd",
        "_private_key",
        "_secret",
        "_secret_key",
        "_signature",
        "_token",
    ];

    if SENSITIVE_SUFFIXES
        .iter()
        .any(|suffix| normalized.ends_with(suffix))
    {
        return true;
    }

    const SENSITIVE_PREFIXES: &[&str] = &[
        "access_token_",
        "api_key_",
        "authorization_",
        "bearer_",
        "client_secret_",
        "csrf_",
        "id_token_",
        "jwt_",
        "private_key_",
        "refresh_token_",
        "secret_key_",
        "set_cookie_",
        "x_api_key_",
    ];

    SENSITIVE_PREFIXES
        .iter()
        .any(|prefix| normalized.starts_with(prefix))
}

#[cfg(test)]
mod tests {
    use std::{
        collections::VecDeque,
        sync::{
            atomic::{AtomicUsize, Ordering},
            Arc, Mutex,
        },
    };

    use axum::{extract::State, routing::post, Router};

    use super::*;

    fn valid_webhook_config() -> WebhookConfig {
        WebhookConfig {
            endpoints: vec![endpoint(&["*"])],
            queue_capacity: 16,
            timeout_secs: 5,
            max_retries: 2,
            initial_backoff_ms: 100,
            max_backoff_ms: 1_000,
            max_concurrency: 4,
            flush_timeout_secs: 5,
        }
    }

    #[test]
    fn validate_config_accepts_valid_values() {
        let config = valid_webhook_config();
        assert!(validate_config(&config).is_ok());
    }

    #[test]
    fn validate_config_rejects_invalid_values() {
        let mut config = valid_webhook_config();
        config.queue_capacity = 0;
        assert!(validate_config(&config).is_err());

        let mut config = valid_webhook_config();
        config.timeout_secs = 0;
        assert!(validate_config(&config).is_err());

        let mut config = valid_webhook_config();
        config.max_concurrency = 0;
        assert!(validate_config(&config).is_err());

        let mut config = valid_webhook_config();
        config.flush_timeout_secs = 0;
        assert!(validate_config(&config).is_err());

        let mut config = valid_webhook_config();
        config.initial_backoff_ms = 2_000;
        config.max_backoff_ms = 1_000;
        assert!(validate_config(&config).is_err());
    }

    #[test]
    fn backoff_delay_handles_zero_and_single_millisecond_values() {
        assert_eq!(backoff_delay(0, 0, 1_000), Duration::from_millis(0));
        assert_eq!(backoff_delay(0, 100, 0), Duration::from_millis(0));
        assert_eq!(backoff_delay(0, 1, 1_000), Duration::from_millis(1));
    }

    #[test]
    fn backoff_delay_applies_exponential_base_with_jitter_bounds() {
        for attempt in 0..100 {
            let delay = backoff_delay(2, 100, 1_000);
            let millis = delay.as_millis() as u64;

            // attempt=2 gives base_ms=400. With jitter, the delay should be in
            // [base/2, base/2 + base), unless capped by max_ms.
            assert!(
                (200..600).contains(&millis),
                "attempt {attempt}: delay {millis}ms outside expected jitter range"
            );
        }
    }

    #[test]
    fn backoff_delay_caps_extreme_values() {
        for attempt in [10, 63, 64, usize::MAX] {
            let delay = backoff_delay(attempt, 1_000, 1_500);
            let millis = delay.as_millis() as u64;

            assert!(
                (750..=1_500).contains(&millis),
                "attempt {attempt}: capped delay {millis}ms outside expected range"
            );
        }
    }

    #[test]
    fn max_outstanding_preserves_default_scale() {
        assert_eq!(max_outstanding_deliveries(1024, 1, 32), 1024);
        assert_eq!(max_outstanding_deliveries(1, 1, 32), 32);
        assert_eq!(max_outstanding_deliveries(128, 3, 32), 384);
    }

    #[test]
    fn max_outstanding_caps_extreme_configurations() {
        assert_eq!(
            max_outstanding_deliveries(100_000, 5, 32),
            MAX_OUTSTANDING_DELIVERY_TASKS
        );
        assert_eq!(
            max_outstanding_deliveries(usize::MAX, usize::MAX, 32),
            MAX_OUTSTANDING_DELIVERY_TASKS
        );
    }

    #[test]
    fn sensitive_field_matching_redacts_common_credentials() {
        let sensitive_fields = [
            "password",
            "db_password",
            "passwd",
            "client_secret",
            "access_token",
            "refresh-token",
            "id.token",
            "private_key",
            "private_key_pem",
            "api_key",
            "x-api-key",
            "jwt",
            "bearer",
            "auth",
            "csrf",
            "authorization",
            "set-cookie",
            "webhook_signature",
        ];

        for field in sensitive_fields {
            assert!(is_sensitive_field(field), "{field} should be redacted");
        }
    }

    #[test]
    fn sensitive_field_matching_keeps_non_secret_metadata() {
        let safe_fields = [
            "token_bucket_config",
            "password_reset_url",
            "credentials_verification_status",
            "signature_algorithm",
            "secretary_name",
            "authored_by",
            "cookie_policy",
        ];

        for field in safe_fields {
            assert!(!is_sensitive_field(field), "{field} should not be redacted");
        }
    }

    fn endpoint(events: &[&str]) -> WebhookEndpointConfig {
        WebhookEndpointConfig {
            url: "http://127.0.0.1:1/webhook".to_string(),
            events: events.iter().map(|event| (*event).to_string()).collect(),
            secret: None,
            enabled: true,
        }
    }

    fn test_config(url: String, queue_capacity: usize, max_retries: usize) -> WebhookConfig {
        WebhookConfig {
            endpoints: vec![WebhookEndpointConfig {
                url,
                events: vec!["*".to_string()],
                secret: None,
                enabled: true,
            }],
            queue_capacity,
            timeout_secs: 1,
            max_retries,
            initial_backoff_ms: 1,
            max_backoff_ms: 2,
            max_concurrency: 4,
            flush_timeout_secs: 2,
        }
    }

    fn test_event() -> LogEvent {
        LogEvent::new(LogLevel::Info, "sandbox.created")
            .field("sandbox_id", "sandbox-123")
            .field("template_id", "template-456")
    }

    #[test]
    fn event_filtering_obeys_endpoint_configuration() {
        let mut disabled = endpoint(&["*"]);
        disabled.enabled = false;
        assert!(!endpoint_matches_event(&disabled, "sandbox.created"));

        let defaults = endpoint(&[]);
        for event in DEFAULT_LIFECYCLE_EVENTS {
            assert!(endpoint_matches_event(&defaults, event));
        }
        assert!(!endpoint_matches_event(&defaults, "api.request"));

        assert!(endpoint_matches_event(&endpoint(&["*"]), "api.request"));

        let created_only = endpoint(&["sandbox.created"]);
        assert!(endpoint_matches_event(&created_only, "sandbox.created"));
        assert!(!endpoint_matches_event(&created_only, "sandbox.deleted"));

        let mixed_wildcard = endpoint(&["*", "sandbox.created"]);
        assert!(compile_endpoint(&mixed_wildcard, 0).is_err());
    }

    #[test]
    fn hmac_signature_has_stable_v1_lowercase_hex_format() {
        let secret = "test-secret";
        let timestamp = "1710000000";
        let delivery_id = "550e8400-e29b-41d4-a716-446655440000";
        let body = br#"{"id":"delivery-1","event":"sandbox.created"}"#;

        let signature = sign_payload(secret, timestamp, delivery_id, body).unwrap();

        let mut expected_mac = Hmac::<Sha256>::new_from_slice(secret.as_bytes()).unwrap();
        expected_mac.update(timestamp.as_bytes());
        expected_mac.update(b".");
        expected_mac.update(delivery_id.as_bytes());
        expected_mac.update(b".");
        expected_mac.update(body);
        let expected = format!("v1={}", hex::encode(expected_mac.finalize().into_bytes()));

        assert_eq!(signature, expected);
        assert_eq!(signature.len(), 67);
        assert!(signature[3..]
            .chars()
            .all(|character| character.is_ascii_digit() || ('a'..='f').contains(&character)));

        let unsigned_endpoint = compile_endpoint(&endpoint(&["sandbox.created"]), 0).unwrap();
        let unsigned_event = test_event();
        let unsigned_fields = sanitized_fields(&unsigned_event.fields);
        let unsigned_delivery =
            Delivery::new(&unsigned_event, &unsigned_endpoint, &unsigned_fields).unwrap();
        assert!(unsigned_delivery.signature.is_none());
    }

    #[test]
    fn retryable_statuses_are_classified_correctly() {
        for status in [408, 425, 429, 500, 503] {
            assert!(is_retryable_status(StatusCode::from_u16(status).unwrap()));
        }
        for status in [200, 201, 301, 302, 400, 401, 403, 404] {
            assert!(!is_retryable_status(StatusCode::from_u16(status).unwrap()));
        }
    }

    #[test]
    fn payload_serialization_flattens_fields_and_includes_delivery_metadata() {
        let event = test_event();
        let compiled = compile_endpoint(&endpoint(&["*"]), 0).unwrap();
        let fields = sanitized_fields(&event.fields);
        let delivery = Delivery::new(&event, &compiled, &fields).unwrap();
        let payload: Value = serde_json::from_slice(&delivery.body).unwrap();

        assert_eq!(payload["id"], delivery.id);
        assert!(payload["timestamp"].is_string());
        assert_eq!(payload["level"], "info");
        assert_eq!(payload["event"], "sandbox.created");
        assert_eq!(payload["sandbox_id"], "sandbox-123");
        assert_eq!(payload["template_id"], "template-456");
        assert!(payload.get("fields").is_none());
    }

    #[test]
    fn payload_serialization_removes_sensitive_fields_recursively() {
        let event = test_event()
            .field("access_token", "top-level-token")
            .field_value(
                "metadata",
                serde_json::json!({
                    "safe": "visible",
                    "secret": "nested-secret",
                    "credentials": {"password": "nested-password"}
                }),
            );
        let compiled = compile_endpoint(&endpoint(&["*"]), 0).unwrap();
        let fields = sanitized_fields(&event.fields);
        let delivery = Delivery::new(&event, &compiled, &fields).unwrap();
        let payload: Value = serde_json::from_slice(&delivery.body).unwrap();

        assert!(payload.get("access_token").is_none());
        assert_eq!(payload["metadata"]["safe"], "visible");
        assert!(payload["metadata"].get("secret").is_none());
        assert!(payload["metadata"].get("credentials").is_none());
        assert!(!delivery
            .body
            .windows("nested-password".len())
            .any(|window| window == b"nested-password"));
    }

    #[derive(Clone)]
    struct MockState {
        statuses: Arc<Mutex<VecDeque<StatusCode>>>,
        calls: Arc<AtomicUsize>,
    }

    async fn mock_webhook(State(state): State<MockState>) -> StatusCode {
        state.calls.fetch_add(1, Ordering::SeqCst);
        state
            .statuses
            .lock()
            .unwrap()
            .pop_front()
            .unwrap_or(StatusCode::OK)
    }

    async fn spawn_mock_server(statuses: Vec<StatusCode>) -> (String, Arc<AtomicUsize>) {
        let state = MockState {
            statuses: Arc::new(Mutex::new(statuses.into())),
            calls: Arc::new(AtomicUsize::new(0)),
        };
        let calls = state.calls.clone();
        let app = Router::new()
            .route("/webhook", post(mock_webhook))
            .with_state(state);
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        tokio::spawn(async move {
            let _ = axum::serve(listener, app).await;
        });
        (format!("http://{address}/webhook"), calls)
    }

    async fn delivery_attempts(statuses: Vec<StatusCode>, max_retries: usize) -> usize {
        let (url, calls) = spawn_mock_server(statuses).await;
        let logger = HttpLogger::new(test_config(url, 8, max_retries)).unwrap();
        logger.log(test_event()).await;
        logger.flush().await;
        calls.load(Ordering::SeqCst)
    }

    #[tokio::test]
    async fn successful_2xx_response_is_not_retried() {
        assert_eq!(
            delivery_attempts(
                vec![StatusCode::CREATED, StatusCode::INTERNAL_SERVER_ERROR],
                3
            )
            .await,
            1
        );
    }

    #[tokio::test]
    async fn retryable_500_and_429_responses_are_retried() {
        assert_eq!(
            delivery_attempts(vec![StatusCode::INTERNAL_SERVER_ERROR, StatusCode::OK], 3).await,
            2
        );
        assert_eq!(
            delivery_attempts(vec![StatusCode::TOO_MANY_REQUESTS, StatusCode::OK], 3).await,
            2
        );
    }

    #[tokio::test]
    async fn ordinary_4xx_response_is_not_retried() {
        assert_eq!(
            delivery_attempts(vec![StatusCode::BAD_REQUEST, StatusCode::OK], 3).await,
            1
        );
    }

    #[tokio::test(flavor = "current_thread")]
    async fn full_queue_drops_events_without_blocking_log() {
        let (url, calls) = spawn_mock_server(vec![StatusCode::OK]).await;
        let logger = HttpLogger::new(test_config(url, 1, 0)).unwrap();

        timeout(Duration::from_millis(50), async {
            for _ in 0..20 {
                logger.log(test_event()).await;
            }
        })
        .await
        .expect("log() must not wait for queue capacity");

        logger.flush().await;
        assert_eq!(calls.load(Ordering::SeqCst), 1);
    }
}

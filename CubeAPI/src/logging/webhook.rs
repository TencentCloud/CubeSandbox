// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Asynchronous sandbox lifecycle webhook delivery.
//!
//! Handler calls only enqueue events with `try_send`. HTTP requests, retries,
//! and backoff run on bounded background tasks so a slow endpoint cannot block
//! sandbox lifecycle APIs.

use std::{
    collections::{HashMap, HashSet},
    sync::Arc,
    time::Duration,
};

use async_trait::async_trait;
use hmac::{Hmac, Mac};
use reqwest::{redirect::Policy, StatusCode, Url};
use sha2::Sha256;
use tokio::sync::{mpsc, oneshot, Semaphore};
use tracing::{debug, error, warn};
use uuid::Uuid;

use crate::config::{WebhookConfig, WebhookEndpointConfig};

use super::{LogEvent, Logger};

pub const SUPPORTED_EVENTS: [&str; 4] = [
    "sandbox.created",
    "sandbox.deleted",
    "sandbox.paused",
    "sandbox.resumed",
];

const SIGNATURE_HEADER: &str = "x-cubesandbox-signature";
const EVENT_HEADER: &str = "x-cubesandbox-event";
const DELIVERY_HEADER: &str = "x-cubesandbox-delivery";

type HmacSha256 = Hmac<Sha256>;

struct Endpoint {
    label: String,
    url: Url,
    secret: Option<Vec<u8>>,
}

impl std::fmt::Debug for Endpoint {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Endpoint")
            .field("label", &self.label)
            .field("url", &"<redacted>")
            .field("secret", &self.secret.as_ref().map(|_| "<redacted>"))
            .finish()
    }
}

#[derive(Debug, Clone, Copy)]
struct DeliveryPolicy {
    max_attempts: u32,
    retry_base_ms: u64,
    retry_max_ms: u64,
}

enum Message {
    Event(LogEvent),
    Flush(oneshot::Sender<()>),
}

#[derive(Clone)]
pub struct WebhookLogger {
    tx: mpsc::Sender<Message>,
    subscriptions: Arc<HashMap<String, Vec<Arc<Endpoint>>>>,
}

impl WebhookLogger {
    pub fn new(config: WebhookConfig) -> anyhow::Result<Self> {
        validate_delivery_config(&config)?;

        let subscriptions = Arc::new(build_subscriptions(&config.endpoints)?);
        let client = reqwest::Client::builder()
            .timeout(Duration::from_millis(config.timeout_ms))
            .redirect(Policy::none())
            .user_agent(concat!("CubeSandbox-Webhook/", env!("CARGO_PKG_VERSION")))
            .build()?;
        let policy = DeliveryPolicy {
            max_attempts: config.max_attempts,
            retry_base_ms: config.retry_base_ms,
            retry_max_ms: config.retry_max_ms,
        };
        let max_in_flight = config.max_in_flight as u32;
        let semaphore = Arc::new(Semaphore::new(config.max_in_flight));
        let (tx, rx) = mpsc::channel(config.queue_capacity);

        tokio::spawn(run_dispatcher(
            rx,
            subscriptions.clone(),
            client,
            policy,
            semaphore,
            max_in_flight,
        ));

        Ok(Self { tx, subscriptions })
    }
}

#[async_trait]
impl Logger for WebhookLogger {
    async fn log(&self, event: LogEvent) {
        if !self.subscriptions.contains_key(&event.event) {
            return;
        }

        match self.tx.try_send(Message::Event(event)) {
            Ok(()) => {}
            Err(mpsc::error::TrySendError::Full(Message::Event(event))) => {
                error!(
                    event = %event.event,
                    "webhook queue full; dropping lifecycle event"
                );
            }
            Err(mpsc::error::TrySendError::Closed(Message::Event(event))) => {
                error!(
                    event = %event.event,
                    "webhook dispatcher stopped; dropping lifecycle event"
                );
            }
            Err(_) => unreachable!("try_send returned a non-event message"),
        }
    }

    async fn flush(&self) {
        let (reply_tx, reply_rx) = oneshot::channel();
        if self.tx.send(Message::Flush(reply_tx)).await.is_ok() {
            let _ = reply_rx.await;
        }
    }

    fn name(&self) -> &'static str {
        "webhook"
    }
}

fn validate_delivery_config(config: &WebhookConfig) -> anyhow::Result<()> {
    anyhow::ensure!(
        config.queue_capacity > 0,
        "webhook queue capacity must be greater than zero"
    );
    anyhow::ensure!(
        config.max_in_flight > 0,
        "webhook max in-flight deliveries must be greater than zero"
    );
    anyhow::ensure!(
        config.max_in_flight <= u32::MAX as usize,
        "webhook max in-flight deliveries exceeds the supported limit"
    );
    anyhow::ensure!(
        config.timeout_ms > 0,
        "webhook timeout must be greater than zero"
    );
    anyhow::ensure!(
        config.max_attempts > 0,
        "webhook max attempts must be greater than zero"
    );
    anyhow::ensure!(
        config.retry_base_ms > 0,
        "webhook retry base delay must be greater than zero"
    );
    anyhow::ensure!(
        config.retry_max_ms > 0,
        "webhook retry max delay must be greater than zero"
    );
    Ok(())
}

fn build_subscriptions(
    configs: &[WebhookEndpointConfig],
) -> anyhow::Result<HashMap<String, Vec<Arc<Endpoint>>>> {
    let supported: HashSet<&str> = SUPPORTED_EVENTS.into_iter().collect();
    let mut subscriptions: HashMap<String, Vec<Arc<Endpoint>>> = HashMap::new();

    for (index, config) in configs.iter().enumerate() {
        let url = Url::parse(&config.url).map_err(|err| {
            anyhow::anyhow!("webhook endpoint {} has an invalid URL: {err}", index + 1)
        })?;
        anyhow::ensure!(
            matches!(url.scheme(), "http" | "https"),
            "webhook endpoint {} must use http or https",
            index + 1
        );
        anyhow::ensure!(
            url.host_str().is_some(),
            "webhook endpoint {} must include a host",
            index + 1
        );
        anyhow::ensure!(
            url.username().is_empty() && url.password().is_none(),
            "webhook endpoint {} must not contain URL userinfo",
            index + 1
        );

        let label = config
            .name
            .as_deref()
            .map(str::trim)
            .filter(|name| !name.is_empty())
            .map(str::to_owned)
            .unwrap_or_else(|| format!("webhook-{}", index + 1));
        let endpoint = Arc::new(Endpoint {
            label,
            url,
            secret: config
                .secret
                .as_deref()
                .filter(|secret| !secret.is_empty())
                .map(|secret| secret.as_bytes().to_vec()),
        });

        let events: Vec<&str> = if config.events.is_empty() {
            SUPPORTED_EVENTS.to_vec()
        } else {
            config.events.iter().map(String::as_str).collect()
        };
        let mut endpoint_events = HashSet::new();
        for event in events {
            anyhow::ensure!(
                supported.contains(event),
                "webhook endpoint {} subscribes to unsupported event {event:?}; supported events: {}",
                index + 1,
                SUPPORTED_EVENTS.join(", ")
            );
            if endpoint_events.insert(event) {
                subscriptions
                    .entry(event.to_string())
                    .or_default()
                    .push(endpoint.clone());
            }
        }
    }

    Ok(subscriptions)
}

async fn run_dispatcher(
    mut rx: mpsc::Receiver<Message>,
    subscriptions: Arc<HashMap<String, Vec<Arc<Endpoint>>>>,
    client: reqwest::Client,
    policy: DeliveryPolicy,
    semaphore: Arc<Semaphore>,
    max_in_flight: u32,
) {
    while let Some(message) = rx.recv().await {
        match message {
            Message::Event(event) => {
                let Some(endpoints) = subscriptions.get(&event.event) else {
                    continue;
                };
                let body = match serde_json::to_vec(&event) {
                    Ok(body) => Arc::new(body),
                    Err(err) => {
                        error!(event = %event.event, error = %err, "failed to serialize webhook event");
                        continue;
                    }
                };

                for endpoint in endpoints {
                    let permit = match semaphore.clone().acquire_owned().await {
                        Ok(permit) => permit,
                        Err(_) => return,
                    };
                    let endpoint = endpoint.clone();
                    let client = client.clone();
                    let event_name = event.event.clone();
                    let body = body.clone();
                    tokio::spawn(async move {
                        let _permit = permit;
                        deliver(&client, &endpoint, &event_name, body.as_slice(), policy).await;
                    });
                }
            }
            Message::Flush(reply) => {
                wait_for_deliveries(&semaphore, max_in_flight).await;
                let _ = reply.send(());
            }
        }
    }

    wait_for_deliveries(&semaphore, max_in_flight).await;
}

async fn wait_for_deliveries(semaphore: &Arc<Semaphore>, max_in_flight: u32) {
    if let Ok(permits) = semaphore.clone().acquire_many_owned(max_in_flight).await {
        drop(permits);
    }
}

async fn deliver(
    client: &reqwest::Client,
    endpoint: &Endpoint,
    event_name: &str,
    body: &[u8],
    policy: DeliveryPolicy,
) {
    let delivery_id = Uuid::new_v4().to_string();
    let signature = endpoint
        .secret
        .as_deref()
        .map(|secret| sign_body(secret, body));

    for attempt in 1..=policy.max_attempts {
        let mut request = client
            .post(endpoint.url.clone())
            .header(reqwest::header::CONTENT_TYPE, "application/json")
            .header(EVENT_HEADER, event_name)
            .header(DELIVERY_HEADER, &delivery_id)
            .body(body.to_vec());
        if let Some(signature) = signature.as_deref() {
            request = request.header(SIGNATURE_HEADER, signature);
        }

        let failure = match request.send().await {
            Ok(response) if response.status().is_success() => {
                debug!(
                    endpoint = %endpoint.label,
                    event = %event_name,
                    delivery_id = %delivery_id,
                    attempt,
                    "webhook delivered"
                );
                return;
            }
            Ok(response) => DeliveryFailure::Status(response.status()),
            Err(err) => DeliveryFailure::Transport(transport_error_kind(&err)),
        };

        let should_retry = attempt < policy.max_attempts && failure.is_retryable();
        if !should_retry {
            error!(
                endpoint = %endpoint.label,
                event = %event_name,
                delivery_id = %delivery_id,
                attempt,
                max_attempts = policy.max_attempts,
                reason = %failure,
                "webhook delivery failed"
            );
            return;
        }

        let delay_ms = retry_delay_ms(attempt, policy);
        warn!(
            endpoint = %endpoint.label,
            event = %event_name,
            delivery_id = %delivery_id,
            attempt,
            max_attempts = policy.max_attempts,
            reason = %failure,
            retry_in_ms = delay_ms,
            "webhook delivery failed; retrying"
        );
        tokio::time::sleep(Duration::from_millis(delay_ms)).await;
    }
}

#[derive(Debug)]
enum DeliveryFailure {
    Status(StatusCode),
    Transport(&'static str),
}

impl DeliveryFailure {
    fn is_retryable(&self) -> bool {
        match self {
            Self::Status(status) => {
                status.is_server_error()
                    || matches!(
                        *status,
                        StatusCode::REQUEST_TIMEOUT | StatusCode::TOO_MANY_REQUESTS
                    )
            }
            Self::Transport(_) => true,
        }
    }
}

impl std::fmt::Display for DeliveryFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Status(status) => write!(f, "HTTP {status}"),
            Self::Transport(kind) => f.write_str(kind),
        }
    }
}

fn transport_error_kind(err: &reqwest::Error) -> &'static str {
    if err.is_timeout() {
        "request timeout"
    } else if err.is_connect() {
        "connection failure"
    } else if err.is_request() {
        "request failure"
    } else if err.is_body() {
        "response body failure"
    } else {
        "transport failure"
    }
}

fn retry_delay_ms(attempt: u32, policy: DeliveryPolicy) -> u64 {
    let multiplier = 1_u64
        .checked_shl(attempt.saturating_sub(1))
        .unwrap_or(u64::MAX);
    policy
        .retry_base_ms
        .saturating_mul(multiplier)
        .min(policy.retry_max_ms)
}

fn sign_body(secret: &[u8], body: &[u8]) -> String {
    let mut mac = HmacSha256::new_from_slice(secret).expect("HMAC accepts keys of any size");
    mac.update(body);
    format!("sha256={}", hex::encode(mac.finalize().into_bytes()))
}

#[cfg(test)]
mod tests {
    use std::{
        collections::HashSet,
        sync::{
            atomic::{AtomicUsize, Ordering},
            Arc,
        },
        time::{Duration, Instant},
    };

    use axum::{
        body::Bytes,
        extract::State,
        http::{HeaderMap, StatusCode},
        routing::post,
        Router,
    };
    use tokio::sync::{mpsc, Mutex};

    use crate::{
        config::{WebhookConfig, WebhookEndpointConfig},
        logging::{LogEvent, LogLevel, Logger},
    };

    use super::{
        build_subscriptions, sign_body, Message, WebhookLogger, DELIVERY_HEADER, SIGNATURE_HEADER,
    };

    type CapturedRequest = (HeaderMap, Vec<u8>);

    #[derive(Clone)]
    struct MockState {
        calls: Arc<AtomicUsize>,
        requests: Arc<Mutex<Vec<CapturedRequest>>>,
        failures_before_success: usize,
        failure_status: StatusCode,
        delay: Duration,
    }

    async fn webhook_handler(
        State(state): State<MockState>,
        headers: HeaderMap,
        body: Bytes,
    ) -> StatusCode {
        let call = state.calls.fetch_add(1, Ordering::SeqCst) + 1;
        state.requests.lock().await.push((headers, body.to_vec()));
        if !state.delay.is_zero() {
            tokio::time::sleep(state.delay).await;
        }
        if call <= state.failures_before_success {
            state.failure_status
        } else {
            StatusCode::NO_CONTENT
        }
    }

    async fn mock_server(
        failures_before_success: usize,
        failure_status: StatusCode,
        delay: Duration,
    ) -> (String, MockState, tokio::task::JoinHandle<()>) {
        let state = MockState {
            calls: Arc::new(AtomicUsize::new(0)),
            requests: Arc::new(Mutex::new(Vec::new())),
            failures_before_success,
            failure_status,
            delay,
        };
        let app = Router::new()
            .route("/webhook", post(webhook_handler))
            .with_state(state.clone());
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let task = tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });
        (format!("http://{address}/webhook"), state, task)
    }

    fn config(url: String, events: Vec<&str>, secret: Option<&str>) -> WebhookConfig {
        WebhookConfig {
            endpoints: vec![WebhookEndpointConfig {
                name: Some("test-endpoint".to_string()),
                url,
                events: events.into_iter().map(str::to_string).collect(),
                secret: secret.map(str::to_string),
            }],
            queue_capacity: 8,
            max_in_flight: 2,
            timeout_ms: 100,
            max_attempts: 3,
            retry_base_ms: 1,
            retry_max_ms: 2,
        }
    }

    #[tokio::test]
    async fn filters_events_and_signs_the_exact_payload() {
        let (url, state, task) =
            mock_server(0, StatusCode::INTERNAL_SERVER_ERROR, Duration::ZERO).await;
        let logger =
            WebhookLogger::new(config(url, vec!["sandbox.created"], Some("test-secret"))).unwrap();

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.paused").field("sandbox_id", "sb-ignored"))
            .await;
        logger
            .log(
                LogEvent::new(LogLevel::Info, "sandbox.created")
                    .field("sandbox_id", "sb-1")
                    .field("template_id", "tpl-1"),
            )
            .await;
        logger.flush().await;

        assert_eq!(state.calls.load(Ordering::SeqCst), 1);
        let requests = state.requests.lock().await;
        let (headers, body) = &requests[0];
        let payload: serde_json::Value = serde_json::from_slice(body).unwrap();
        assert_eq!(payload["event"], "sandbox.created");
        assert_eq!(payload["sandbox_id"], "sb-1");
        assert_eq!(payload["template_id"], "tpl-1");
        assert!(payload["timestamp"].is_string());
        assert_eq!(
            headers.get(SIGNATURE_HEADER).unwrap().to_str().unwrap(),
            sign_body(b"test-secret", body)
        );
        assert!(!headers.get(DELIVERY_HEADER).unwrap().is_empty());
        task.abort();
    }

    #[tokio::test]
    async fn retries_retryable_failures_with_a_stable_delivery_id() {
        let (url, state, task) =
            mock_server(2, StatusCode::INTERNAL_SERVER_ERROR, Duration::ZERO).await;
        let logger = WebhookLogger::new(config(url, vec!["sandbox.deleted"], None)).unwrap();

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.deleted").field("sandbox_id", "sb-2"))
            .await;
        logger.flush().await;

        assert_eq!(state.calls.load(Ordering::SeqCst), 3);
        let requests = state.requests.lock().await;
        let delivery_ids: HashSet<_> = requests
            .iter()
            .map(|(headers, _)| headers.get(DELIVERY_HEADER).unwrap().to_str().unwrap())
            .collect();
        assert_eq!(delivery_ids.len(), 1);
        task.abort();
    }

    #[tokio::test]
    async fn fans_out_to_multiple_subscribed_endpoints() {
        let (first_url, first_state, first_task) =
            mock_server(0, StatusCode::INTERNAL_SERVER_ERROR, Duration::ZERO).await;
        let (second_url, second_state, second_task) =
            mock_server(0, StatusCode::INTERNAL_SERVER_ERROR, Duration::ZERO).await;
        let mut cfg = config(first_url, vec!["sandbox.created"], None);
        cfg.endpoints.push(WebhookEndpointConfig {
            name: Some("second".to_string()),
            url: second_url,
            events: vec!["sandbox.created".to_string()],
            secret: None,
        });
        let logger = WebhookLogger::new(cfg).unwrap();

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sb-fanout"))
            .await;
        logger.flush().await;

        assert_eq!(first_state.calls.load(Ordering::SeqCst), 1);
        assert_eq!(second_state.calls.load(Ordering::SeqCst), 1);
        first_task.abort();
        second_task.abort();
    }

    #[tokio::test]
    async fn does_not_retry_permanent_client_errors() {
        let (url, state, task) = mock_server(3, StatusCode::BAD_REQUEST, Duration::ZERO).await;
        let logger = WebhookLogger::new(config(url, vec!["sandbox.deleted"], None)).unwrap();

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.deleted").field("sandbox_id", "sb-4"))
            .await;
        logger.flush().await;

        assert_eq!(state.calls.load(Ordering::SeqCst), 1);
        task.abort();
    }

    #[tokio::test]
    async fn enqueue_does_not_wait_for_a_slow_endpoint() {
        let (url, state, task) = mock_server(
            0,
            StatusCode::INTERNAL_SERVER_ERROR,
            Duration::from_millis(250),
        )
        .await;
        let mut cfg = config(url, vec!["sandbox.paused"], None);
        cfg.timeout_ms = 50;
        cfg.max_attempts = 1;
        let logger = WebhookLogger::new(cfg).unwrap();

        let started = Instant::now();
        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.paused").field("sandbox_id", "sb-3"))
            .await;
        assert!(started.elapsed() < Duration::from_millis(100));
        tokio::time::timeout(Duration::from_secs(1), logger.flush())
            .await
            .expect("flush should be bounded by the request timeout");
        assert_eq!(state.calls.load(Ordering::SeqCst), 1);
        task.abort();
    }

    #[tokio::test]
    async fn drops_new_events_without_waiting_when_queue_is_full() {
        let cfg = config(
            "https://example.com/hook".to_string(),
            vec!["sandbox.created"],
            None,
        );
        let subscriptions = Arc::new(build_subscriptions(&cfg.endpoints).unwrap());
        let (tx, mut rx) = mpsc::channel(1);
        let logger = WebhookLogger { tx, subscriptions };

        logger
            .log(LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sb-first"))
            .await;
        tokio::time::timeout(
            Duration::from_millis(100),
            logger.log(
                LogEvent::new(LogLevel::Info, "sandbox.created").field("sandbox_id", "sb-dropped"),
            ),
        )
        .await
        .expect("a full queue must not block the caller");

        let Message::Event(event) = rx.try_recv().expect("first event should remain queued") else {
            panic!("queue should contain an event");
        };
        assert_eq!(
            event.fields.get("sandbox_id"),
            Some(&serde_json::Value::String("sb-first".to_string()))
        );
        assert!(matches!(
            rx.try_recv(),
            Err(mpsc::error::TryRecvError::Empty)
        ));
    }

    #[test]
    fn rejects_unsupported_events_and_unsafe_url_userinfo() {
        let unsupported = config(
            "https://example.com/hook".to_string(),
            vec!["api.request"],
            None,
        );
        assert!(WebhookLogger::new(unsupported)
            .err()
            .expect("unsupported event should fail")
            .to_string()
            .contains("unsupported event"));

        let with_userinfo = config(
            "https://user:password@example.com/hook".to_string(),
            vec!["sandbox.created"],
            None,
        );
        assert!(WebhookLogger::new(with_userinfo)
            .err()
            .expect("URL userinfo should fail")
            .to_string()
            .contains("must not contain URL userinfo"));
    }

    #[test]
    fn rejects_zero_retry_base_delay() {
        let mut cfg = config(
            "https://example.com/hook".to_string(),
            vec!["sandbox.created"],
            None,
        );
        cfg.retry_base_ms = 0;

        assert!(WebhookLogger::new(cfg)
            .err()
            .expect("zero retry delay should fail")
            .to_string()
            .contains("retry base delay must be greater than zero"));
    }
}

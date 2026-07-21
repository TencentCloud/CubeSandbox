// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Non-blocking HTTP Webhook backend for structured lifecycle events.

use std::{
    collections::HashSet,
    fmt,
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use async_trait::async_trait;
use bytes::Bytes;
use hmac::{Hmac, Mac};
use reqwest::{Client, StatusCode};
use sha2::Sha256;
use tokio::sync::{mpsc, oneshot};
use tracing::{error, warn};
use uuid::Uuid;

use super::{LogEvent, Logger};

type HmacSha256 = Hmac<Sha256>;

#[derive(Clone)]
pub struct HttpLoggerConfig {
    pub url: String,
    pub events: HashSet<String>,
    pub secret: Option<String>,
    pub queue_capacity: usize,
    pub max_retries: usize,
    pub retry_base_ms: u64,
    pub request_timeout_secs: u64,
}

impl fmt::Debug for HttpLoggerConfig {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("HttpLoggerConfig")
            .field("url", &self.url)
            .field("events", &self.events)
            .field("secret", &self.secret.as_ref().map(|_| "**REDACTED**"))
            .field("queue_capacity", &self.queue_capacity)
            .field("max_retries", &self.max_retries)
            .field("retry_base_ms", &self.retry_base_ms)
            .field("request_timeout_secs", &self.request_timeout_secs)
            .finish()
    }
}

enum Command {
    Deliver(LogEvent),
    Barrier(oneshot::Sender<()>),
}

#[derive(Clone)]
pub struct HttpLogger {
    tx: mpsc::Sender<Command>,
}

impl HttpLogger {
    pub fn new(config: HttpLoggerConfig) -> Result<Self, reqwest::Error> {
        let client = Client::builder()
            .timeout(Duration::from_secs(config.request_timeout_secs.max(1)))
            // Never forward webhook bodies or HMAC headers to a redirect target.
            .redirect(reqwest::redirect::Policy::none())
            .build()?;
        let signer = config
            .secret
            .as_deref()
            .filter(|secret| !secret.is_empty())
            .map(|secret| {
                HmacSha256::new_from_slice(secret.as_bytes())
                    .expect("HMAC-SHA256 accepts keys of any length")
            });
        let (tx, mut rx) = mpsc::channel(config.queue_capacity.max(1));
        tokio::spawn(async move {
            // Delivery is deliberately sequential for each endpoint so lifecycle
            // events cannot overtake one another. Different endpoint loggers have
            // independent workers, and the bounded queue isolates API requests.
            while let Some(command) = rx.recv().await {
                match command {
                    Command::Deliver(event) => {
                        deliver(&client, &config, signer.as_ref(), &event).await
                    }
                    Command::Barrier(done) => {
                        let _ = done.send(());
                    }
                }
            }
        });
        Ok(Self { tx })
    }
}

async fn deliver(
    client: &Client,
    config: &HttpLoggerConfig,
    signer: Option<&HmacSha256>,
    event: &LogEvent,
) {
    if !config.events.contains(&event.event) {
        return;
    }
    let body = match serde_json::to_vec(event) {
        Ok(body) => Bytes::from(body),
        Err(err) => {
            error!(event = %event.event, %err, "webhook serialization failed");
            return;
        }
    };
    for attempt in 0..=config.max_retries {
        // A fresh cryptographic nonce makes every retry independently verifiable
        // and lets receivers reject replayed requests within the timestamp window.
        let attempt_nonce = Uuid::new_v4();
        let signed_headers = signer.map(|signer| {
            let timestamp = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_secs()
                .to_string();
            let nonce = attempt_nonce.to_string();
            let mut mac = signer.clone();
            mac.update(timestamp.as_bytes());
            mac.update(b".");
            mac.update(nonce.as_bytes());
            mac.update(b".");
            mac.update(&body);
            let signature = format!("sha256={}", hex::encode(mac.finalize().into_bytes()));
            (timestamp, nonce, signature)
        });
        let mut request = client
            .post(&config.url)
            .header(reqwest::header::CONTENT_TYPE, "application/json")
            .header("X-Cube-Event", &event.event)
            .body(body.clone());
        if let Some((timestamp, nonce, signature)) = signed_headers.as_ref() {
            request = request
                .header("X-Cube-Timestamp", timestamp)
                .header("X-Cube-Nonce", nonce)
                .header("X-Cube-Signature-256", signature);
        }

        match request.send().await {
            Ok(response) if response.status().is_success() => return,
            Ok(response) if !retryable(response.status()) => {
                warn!(url = %config.url, event = %event.event, status = %response.status(),
                    "webhook rejected without retry");
                return;
            }
            Ok(response) => warn!(url = %config.url, event = %event.event,
                status = %response.status(), attempt, "webhook delivery failed"),
            Err(err) => warn!(url = %config.url, event = %event.event, %err, attempt,
                "webhook request failed"),
        }

        if attempt < config.max_retries {
            let factor = 1u64.checked_shl(attempt.min(31) as u32).unwrap_or(u64::MAX);
            let base = config.retry_base_ms.saturating_mul(factor);
            let quarter = base / 4;
            let jitter_span = quarter.saturating_mul(2).saturating_add(1);
            let entropy = u64::from_le_bytes(
                attempt_nonce.as_bytes()[..8]
                    .try_into()
                    .expect("UUID contains at least eight bytes"),
            );
            let delay = base
                .saturating_sub(quarter)
                .saturating_add(entropy % jitter_span);
            tokio::time::sleep(Duration::from_millis(delay)).await;
        }
    }
    error!(url = %config.url, event = %event.event, "webhook retries exhausted");
}

fn retryable(status: StatusCode) -> bool {
    status == StatusCode::REQUEST_TIMEOUT
        || status == StatusCode::TOO_MANY_REQUESTS
        || status.is_server_error()
}

#[async_trait]
impl Logger for HttpLogger {
    async fn log(&self, event: LogEvent) {
        match self.tx.try_send(Command::Deliver(event)) {
            Ok(()) => {}
            Err(mpsc::error::TrySendError::Full(_)) => warn!("webhook queue full; event dropped"),
            Err(mpsc::error::TrySendError::Closed(_)) => {
                error!("webhook worker closed; event dropped")
            }
        }
    }

    async fn flush(&self) {
        let (done, wait) = oneshot::channel();
        if self.tx.send(Command::Barrier(done)).await.is_ok() {
            if tokio::time::timeout(Duration::from_secs(30), wait)
                .await
                .is_err()
            {
                warn!("timed out waiting for webhook worker to flush");
            }
        }
    }

    fn name(&self) -> &'static str {
        "http"
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::logging::LogLevel;
    use axum::{
        body::Bytes,
        extract::State,
        http::{HeaderMap, StatusCode},
        routing::post,
        Router,
    };
    use std::{
        collections::VecDeque,
        sync::{
            atomic::{AtomicUsize, Ordering},
            Arc,
        },
    };
    use tokio::sync::Mutex;

    #[derive(Clone)]
    struct StateData {
        attempts: Arc<AtomicUsize>,
        bodies: Arc<Mutex<Vec<Vec<u8>>>>,
        headers: Arc<Mutex<Vec<HeaderMap>>>,
        statuses: Arc<Mutex<VecDeque<StatusCode>>>,
    }

    impl Default for StateData {
        fn default() -> Self {
            Self::with_statuses([StatusCode::NO_CONTENT])
        }
    }

    impl StateData {
        fn with_statuses(statuses: impl IntoIterator<Item = StatusCode>) -> Self {
            Self {
                attempts: Arc::new(AtomicUsize::new(0)),
                bodies: Arc::new(Mutex::new(Vec::new())),
                headers: Arc::new(Mutex::new(Vec::new())),
                statuses: Arc::new(Mutex::new(statuses.into_iter().collect())),
            }
        }
    }

    async fn receive(
        State(state): State<StateData>,
        headers: HeaderMap,
        body: Bytes,
    ) -> StatusCode {
        state.attempts.fetch_add(1, Ordering::SeqCst);
        state.bodies.lock().await.push(body.to_vec());
        state.headers.lock().await.push(headers);
        state
            .statuses
            .lock()
            .await
            .pop_front()
            .expect("test server ran out of status codes")
    }

    async fn receiver(state: StateData) -> String {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(
                listener,
                Router::new().route("/", post(receive)).with_state(state),
            )
            .await
            .unwrap();
        });
        format!("http://{address}/")
    }

    fn config(url: String, event: &str) -> HttpLoggerConfig {
        HttpLoggerConfig {
            url,
            events: [event.to_owned()].into(),
            secret: None,
            queue_capacity: 8,
            max_retries: 2,
            retry_base_ms: 1,
            request_timeout_secs: 2,
        }
    }

    fn event(name: &str) -> LogEvent {
        LogEvent::new(LogLevel::Info, name).field("sandbox_id", "sb-test")
    }

    #[tokio::test]
    async fn filters_and_delivers_without_blocking() {
        let state = StateData::default();
        let logger =
            HttpLogger::new(config(receiver(state.clone()).await, "sandbox.created")).unwrap();
        logger.log(event("sandbox.deleted")).await;
        logger.log(event("sandbox.created")).await;
        logger.flush().await;
        assert_eq!(state.attempts.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn signs_exact_body_and_sets_event_header() {
        let state = StateData::default();
        let mut cfg = config(receiver(state.clone()).await, "sandbox.paused");
        cfg.secret = Some("endpoint-secret".into());
        let logger = HttpLogger::new(cfg).unwrap();
        logger.log(event("sandbox.paused")).await;
        logger.flush().await;
        let body = state.bodies.lock().await[0].clone();
        let headers = state.headers.lock().await;
        let timestamp = headers[0]["x-cube-timestamp"].to_str().unwrap();
        let nonce = headers[0]["x-cube-nonce"].to_str().unwrap();
        let mut mac = HmacSha256::new_from_slice(b"endpoint-secret").unwrap();
        mac.update(timestamp.as_bytes());
        mac.update(b".");
        mac.update(nonce.as_bytes());
        mac.update(b".");
        mac.update(&body);
        let expected = format!("sha256={}", hex::encode(mac.finalize().into_bytes()));
        assert_eq!(
            headers[0]["x-cube-signature-256"].to_str().unwrap(),
            expected
        );
        assert_eq!(
            headers[0]["x-cube-event"].to_str().unwrap(),
            "sandbox.paused"
        );
    }

    #[test]
    fn debug_redacts_secret() {
        let mut cfg = config("http://localhost".into(), "sandbox.created");
        cfg.secret = Some("do-not-log-me".into());
        let debug = format!("{cfg:?}");
        assert!(debug.contains("**REDACTED**"));
        assert!(!debug.contains("do-not-log-me"));
    }

    #[tokio::test]
    async fn retries_500_but_not_400() {
        let transient =
            StateData::with_statuses([StatusCode::INTERNAL_SERVER_ERROR, StatusCode::NO_CONTENT]);
        let logger =
            HttpLogger::new(config(receiver(transient.clone()).await, "sandbox.resumed")).unwrap();
        logger.log(event("sandbox.resumed")).await;
        logger.flush().await;
        assert_eq!(transient.attempts.load(Ordering::SeqCst), 2);

        let permanent = StateData::with_statuses([StatusCode::BAD_REQUEST]);
        let logger =
            HttpLogger::new(config(receiver(permanent.clone()).await, "sandbox.resumed")).unwrap();
        logger.log(event("sandbox.resumed")).await;
        logger.flush().await;
        assert_eq!(permanent.attempts.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn signs_each_retry_with_a_fresh_nonce() {
        let state =
            StateData::with_statuses([StatusCode::INTERNAL_SERVER_ERROR, StatusCode::NO_CONTENT]);
        let mut cfg = config(receiver(state.clone()).await, "sandbox.resumed");
        cfg.secret = Some("endpoint-secret".into());
        let logger = HttpLogger::new(cfg).unwrap();
        logger.log(event("sandbox.resumed")).await;
        logger.flush().await;

        let headers = state.headers.lock().await;
        assert_ne!(headers[0]["x-cube-nonce"], headers[1]["x-cube-nonce"]);
        assert_ne!(
            headers[0]["x-cube-signature-256"],
            headers[1]["x-cube-signature-256"]
        );
    }

    #[tokio::test]
    async fn stops_after_retry_exhaustion() {
        let exhausted = StateData::with_statuses([
            StatusCode::INTERNAL_SERVER_ERROR,
            StatusCode::INTERNAL_SERVER_ERROR,
            StatusCode::INTERNAL_SERVER_ERROR,
        ]);
        let logger =
            HttpLogger::new(config(receiver(exhausted.clone()).await, "sandbox.created")).unwrap();
        logger.log(event("sandbox.created")).await;
        logger.flush().await;
        assert_eq!(exhausted.attempts.load(Ordering::SeqCst), 3);
    }

    #[tokio::test]
    async fn empty_secret_disables_signing() {
        let state = StateData::default();
        let mut cfg = config(receiver(state.clone()).await, "sandbox.created");
        cfg.secret = Some(String::new());
        let logger = HttpLogger::new(cfg).unwrap();
        logger.log(event("sandbox.created")).await;
        logger.flush().await;
        assert!(!state.headers.lock().await[0].contains_key("x-cube-signature-256"));
    }
}

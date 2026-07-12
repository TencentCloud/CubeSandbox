// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Non-blocking HTTP Webhook backend for structured lifecycle events.

use std::{collections::HashSet, time::Duration};

use async_trait::async_trait;
use hmac::{Hmac, Mac};
use reqwest::{Client, StatusCode};
use sha2::Sha256;
use tokio::sync::{mpsc, oneshot};
use tracing::{error, warn};

use super::{LogEvent, Logger};

type HmacSha256 = Hmac<Sha256>;

#[derive(Debug, Clone)]
pub struct HttpLoggerConfig {
    pub url: String,
    pub events: HashSet<String>,
    pub secret: Option<String>,
    pub queue_capacity: usize,
    pub max_retries: usize,
    pub retry_base_ms: u64,
    pub request_timeout_secs: u64,
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
            .build()?;
        let (tx, mut rx) = mpsc::channel(config.queue_capacity.max(1));
        tokio::spawn(async move {
            while let Some(command) = rx.recv().await {
                match command {
                    Command::Deliver(event) => deliver(&client, &config, &event).await,
                    Command::Barrier(done) => {
                        let _ = done.send(());
                    }
                }
            }
        });
        Ok(Self { tx })
    }
}

async fn deliver(client: &Client, config: &HttpLoggerConfig, event: &LogEvent) {
    if !config.events.contains(&event.event) {
        return;
    }
    let body = match serde_json::to_vec(event) {
        Ok(body) => body,
        Err(err) => {
            error!(event = %event.event, %err, "webhook serialization failed");
            return;
        }
    };

    for attempt in 0..=config.max_retries {
        let mut request = client
            .post(&config.url)
            .header(reqwest::header::CONTENT_TYPE, "application/json")
            .header("X-Cube-Event", &event.event)
            .body(body.clone());
        if let Some(secret) = config.secret.as_deref() {
            let mut mac = HmacSha256::new_from_slice(secret.as_bytes())
                .expect("HMAC accepts keys of any length");
            mac.update(&body);
            request = request.header(
                "X-Cube-Signature-256",
                format!("sha256={}", hex::encode(mac.finalize().into_bytes())),
            );
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
            tokio::time::sleep(Duration::from_millis(
                config.retry_base_ms.saturating_mul(factor),
            )).await;
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
            Err(mpsc::error::TrySendError::Closed(_)) => error!("webhook worker closed; event dropped"),
        }
    }

    async fn flush(&self) {
        let (done, wait) = oneshot::channel();
        if self.tx.send(Command::Barrier(done)).await.is_ok() {
            let _ = wait.await;
        }
    }

    fn name(&self) -> &'static str {
        "http"
    }
}

#[cfg(test)]
mod tests {
    use std::{
        collections::VecDeque,
        sync::{
            atomic::{AtomicUsize, Ordering},
            Arc,
        },
    };
    use axum::{
        body::Bytes,
        extract::State,
        http::{HeaderMap, StatusCode},
        routing::post,
        Router,
    };
    use tokio::sync::Mutex;
    use super::*;
    use crate::logging::LogLevel;

    #[derive(Clone, Default)]
    struct StateData {
        attempts: Arc<AtomicUsize>,
        bodies: Arc<Mutex<Vec<Vec<u8>>>>,
        headers: Arc<Mutex<Vec<HeaderMap>>>,
        statuses: Arc<Mutex<VecDeque<StatusCode>>>,
    }

    async fn receive(State(state): State<StateData>, headers: HeaderMap, body: Bytes) -> StatusCode {
        state.attempts.fetch_add(1, Ordering::SeqCst);
        state.bodies.lock().await.push(body.to_vec());
        state.headers.lock().await.push(headers);
        state.statuses.lock().await.pop_front().unwrap_or(StatusCode::NO_CONTENT)
    }

    async fn receiver(state: StateData) -> String {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(listener, Router::new().route("/", post(receive)).with_state(state)).await.unwrap();
        });
        format!("http://{address}/")
    }

    fn config(url: String, event: &str) -> HttpLoggerConfig {
        HttpLoggerConfig { url, events: [event.to_owned()].into(), secret: None,
            queue_capacity: 8, max_retries: 2, retry_base_ms: 1, request_timeout_secs: 2 }
    }

    fn event(name: &str) -> LogEvent {
        LogEvent::new(LogLevel::Info, name).field("sandbox_id", "sb-test")
    }

    #[tokio::test]
    async fn filters_and_delivers_without_blocking() {
        let state = StateData::default();
        let logger = HttpLogger::new(config(receiver(state.clone()).await, "sandbox.created")).unwrap();
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
        let mut mac = HmacSha256::new_from_slice(b"endpoint-secret").unwrap();
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

    #[tokio::test]
    async fn retries_500_but_not_400() {
        let transient = StateData::default();
        transient.statuses.lock().await.extend([StatusCode::INTERNAL_SERVER_ERROR, StatusCode::NO_CONTENT]);
        let logger = HttpLogger::new(config(receiver(transient.clone()).await, "sandbox.resumed")).unwrap();
        logger.log(event("sandbox.resumed")).await;
        logger.flush().await;
        assert_eq!(transient.attempts.load(Ordering::SeqCst), 2);

        let permanent = StateData::default();
        permanent.statuses.lock().await.push_back(StatusCode::BAD_REQUEST);
        let logger = HttpLogger::new(config(receiver(permanent.clone()).await, "sandbox.resumed")).unwrap();
        logger.log(event("sandbox.resumed")).await;
        logger.flush().await;
        assert_eq!(permanent.attempts.load(Ordering::SeqCst), 1);
    }
}

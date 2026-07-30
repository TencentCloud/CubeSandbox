// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Best-effort internal event forwarding from CubeAPI to CubeOps.

mod batcher;
mod client;
mod metrics;

use crate::logging::{LogEvent, Logger};
use anyhow::{bail, Context};
use async_trait::async_trait;
use std::{sync::Arc, time::Duration};
use tokio::sync::{mpsc, oneshot};
use tracing::error;

use client::OpsClient;
use metrics::{ForwarderMetrics};

const EVENT_QUEUE_CAPACITY: usize = 10_000;
const BATCH_SIZE: usize = 100;
const FLUSH_INTERVAL: Duration = Duration::from_millis(100);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(2);

pub struct ForwarderConfig {
    pub ops_url: String,
    pub event_queue_capacity: usize,
    pub batch_size: usize,
    pub flush_interval: Duration,
    pub request_timeout: Duration,
}

impl ForwarderConfig {
    pub fn for_ops_url(ops_url: &str) -> Self {
        Self {
            ops_url: ops_url.to_string(),
            event_queue_capacity: EVENT_QUEUE_CAPACITY,
            batch_size: BATCH_SIZE,
            flush_interval: FLUSH_INTERVAL,
            request_timeout: REQUEST_TIMEOUT,
        }
    }

    fn validate(&self) -> anyhow::Result<()> {
        if self.event_queue_capacity == 0 {
            bail!("CubeOps event queue capacity must be greater than zero");
        }
        if self.batch_size == 0 || self.batch_size > 100 {
            bail!("CubeOps event batch size must be between 1 and 100");
        }
        if self.flush_interval.is_zero() {
            bail!("CubeOps event flush interval must be greater than zero");
        }
        if self.request_timeout.is_zero() {
            bail!("CubeOps event request timeout must be greater than zero");
        }
        Ok(())
    }
}

pub(crate) enum EventMsg {
    Event(LogEvent),
    Flush(oneshot::Sender<()>),
}

#[derive(Clone)]
pub struct OpsEventForwarder {
    event_tx: mpsc::Sender<EventMsg>,
    metrics: Arc<ForwarderMetrics>,
}

impl OpsEventForwarder {
    pub fn new(config: ForwarderConfig) -> anyhow::Result<Self> {
        config.validate()?;
        let client = OpsClient::new(&config.ops_url, config.request_timeout)
            .context("build CubeOps event client")?;
        let (event_tx, event_rx) = mpsc::channel(config.event_queue_capacity);
        let metrics = Arc::new(ForwarderMetrics::default());

        batcher::spawn(
            event_rx,
            client,
            config.batch_size,
            config.flush_interval,
            metrics.clone(),
        );

        Ok(Self { event_tx, metrics })
    }
}

#[async_trait]
impl Logger for OpsEventForwarder {
    async fn log(&self, event: LogEvent) {
        match self.event_tx.try_send(EventMsg::Event(event)) {
            Ok(()) => {
                self.metrics
                    .events_enqueued
                    .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                tracing::debug!(
                    event_queue_depth = self.event_tx.max_capacity() - self.event_tx.capacity(),
                    "CubeOps event enqueued"
                );
            }
            Err(mpsc::error::TrySendError::Full(EventMsg::Event(event))) => {
                self.metrics
                    .events_dropped
                    .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                error!(event = %event.event, "CubeOps event queue is full; dropping event");
            }
            Err(mpsc::error::TrySendError::Closed(EventMsg::Event(event))) => {
                self.metrics
                    .events_dropped
                    .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                error!(event = %event.event, "CubeOps event batcher is unavailable; dropping event");
            }
            Err(mpsc::error::TrySendError::Full(EventMsg::Flush(_)))
            | Err(mpsc::error::TrySendError::Closed(EventMsg::Flush(_))) => unreachable!(),
        }
    }

    async fn flush(&self) {
        let (reply_tx, reply_rx) = oneshot::channel();
        if self.event_tx.send(EventMsg::Flush(reply_tx)).await.is_ok() {
            let _ = reply_rx.await;
        }
    }

    fn name(&self) -> &'static str {
        "cubeops-http"
    }
}

#[cfg(test)]
mod tests {
    use super::{ForwarderConfig, OpsEventForwarder};
    use crate::logging::{LogEvent, LogLevel, Logger};
    use axum::{body::Bytes, extract::State, http::StatusCode, routing::post, Router};
    use std::{
        sync::{
            atomic::{AtomicUsize, Ordering},
            Arc, Mutex,
        },
        time::{Duration, Instant},
    };

    #[derive(Default)]
    struct Recorder {
        bodies: Mutex<Vec<serde_json::Value>>,
        requests: AtomicUsize,
        active: AtomicUsize,
        max_active: AtomicUsize,
        status: Mutex<StatusCode>,
        delay: Mutex<Duration>,
    }

    impl Recorder {
        fn new(status: StatusCode, delay: Duration) -> Self {
            Self {
                status: Mutex::new(status),
                delay: Mutex::new(delay),
                ..Self::default()
            }
        }
    }

    async fn record(State(recorder): State<Arc<Recorder>>, body: Bytes) -> StatusCode {
        recorder.requests.fetch_add(1, Ordering::SeqCst);
        let active = recorder.active.fetch_add(1, Ordering::SeqCst) + 1;
        recorder.max_active.fetch_max(active, Ordering::SeqCst);
        recorder
            .bodies
            .lock()
            .unwrap()
            .push(serde_json::from_slice(&body).unwrap());
        let delay = *recorder.delay.lock().unwrap();
        if !delay.is_zero() {
            tokio::time::sleep(delay).await;
        }
        recorder.active.fetch_sub(1, Ordering::SeqCst);
        *recorder.status.lock().unwrap()
    }

    async fn spawn_recorder(status: StatusCode, delay: Duration) -> (String, Arc<Recorder>) {
        let recorder = Arc::new(Recorder::new(status, delay));
        let app = Router::new()
            .route("/internal/webhook/events/batch", post(record))
            .with_state(recorder.clone());
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
        (format!("http://{}", addr), recorder)
    }

    fn config(url: String) -> ForwarderConfig {
        ForwarderConfig {
            ops_url: url,
            event_queue_capacity: 32,
            batch_size: 2,
            flush_interval: Duration::from_millis(20),
            request_timeout: Duration::from_secs(1),
        }
    }

    async fn wait_for_requests(recorder: &Recorder, expected: usize) {
        tokio::time::timeout(Duration::from_secs(1), async {
            while recorder.requests.load(Ordering::SeqCst) < expected {
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .unwrap();
    }

    #[tokio::test]
    async fn internal_payload_has_schema_version_and_no_batch_id() {
        let (url, recorder) = spawn_recorder(StatusCode::ACCEPTED, Duration::ZERO).await;
        let logger = OpsEventForwarder::new(config(url)).unwrap();

        logger
            .log(LogEvent::new(LogLevel::Info, "future.unknown"))
            .await;
        logger.flush().await;

        let bodies = recorder.bodies.lock().unwrap();
        assert_eq!(bodies.len(), 1);
        assert_eq!(bodies[0]["schema_version"], 1);
        assert_eq!(bodies[0]["events"][0]["event"], "future.unknown");
        assert!(bodies[0].get("batch_id").is_none());
    }

    #[tokio::test]
    async fn flushes_when_batch_size_is_reached() {
        let (url, recorder) = spawn_recorder(StatusCode::ACCEPTED, Duration::ZERO).await;
        let logger = OpsEventForwarder::new(config(url)).unwrap();

        logger.log(LogEvent::new(LogLevel::Info, "one")).await;
        logger.log(LogEvent::new(LogLevel::Info, "two")).await;
        wait_for_requests(&recorder, 1).await;
        logger.flush().await;

        assert_eq!(
            recorder.bodies.lock().unwrap()[0]["events"]
                .as_array()
                .unwrap()
                .len(),
            2
        );
    }

    #[tokio::test]
    async fn flushes_partial_batch_on_interval() {
        let (url, recorder) = spawn_recorder(StatusCode::ACCEPTED, Duration::ZERO).await;
        let logger = OpsEventForwarder::new(config(url)).unwrap();

        logger.log(LogEvent::new(LogLevel::Info, "one")).await;
        wait_for_requests(&recorder, 1).await;
        logger.flush().await;

        assert_eq!(
            recorder.bodies.lock().unwrap()[0]["events"]
                .as_array()
                .unwrap()
                .len(),
            1
        );
    }

    #[tokio::test]
    async fn does_not_retry_non_202_response() {
        let (url, recorder) = spawn_recorder(StatusCode::SERVICE_UNAVAILABLE, Duration::ZERO).await;
        let logger = OpsEventForwarder::new(config(url)).unwrap();

        logger.log(LogEvent::new(LogLevel::Info, "one")).await;
        logger.flush().await;

        assert_eq!(recorder.requests.load(Ordering::SeqCst), 1);
        assert_eq!(logger.metrics.snapshot().status_failures.get(&503), Some(&1));
    }

    #[tokio::test]
    async fn sends_internal_batches_sequentially() {
        let (url, recorder) = spawn_recorder(StatusCode::ACCEPTED, Duration::from_millis(50)).await;
        let mut cfg = config(url);
        cfg.batch_size = 1;
        let logger = OpsEventForwarder::new(cfg).unwrap();

        for index in 0..6 {
            logger
                .log(LogEvent::new(LogLevel::Info, format!("event.{index}")))
                .await;
        }
        logger.flush().await;

        assert_eq!(recorder.requests.load(Ordering::SeqCst), 6);
        assert_eq!(recorder.max_active.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn log_does_not_wait_for_slow_cubeops() {
        let (url, _recorder) =
            spawn_recorder(StatusCode::ACCEPTED, Duration::from_millis(200)).await;
        let mut cfg = config(url);
        cfg.batch_size = 1;
        let logger = OpsEventForwarder::new(cfg).unwrap();
        let started = Instant::now();

        logger.log(LogEvent::new(LogLevel::Info, "one")).await;

        assert!(started.elapsed() < Duration::from_millis(50));
        logger.flush().await;
    }

    #[tokio::test]
    async fn records_internal_forwarding_outcomes() {
        let (url, _recorder) = spawn_recorder(StatusCode::ACCEPTED, Duration::ZERO).await;
        let logger = OpsEventForwarder::new(config(url)).unwrap();

        logger.log(LogEvent::new(LogLevel::Info, "one")).await;
        logger.flush().await;

        let stats = logger.metrics.snapshot();
        assert_eq!(stats.events_enqueued, 1);
        assert_eq!(stats.batches_formed, 1);
        assert_eq!(stats.batches_sent, 1);
        assert_eq!(stats.batches_failed, 0);
    }
}

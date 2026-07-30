// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use super::{
    client::{InternalBatch, OpsClient},
    metrics::ForwarderMetrics,
    EventMsg,
};
use crate::logging::LogEvent;
use reqwest::StatusCode;
use std::{
    sync::{atomic::Ordering, Arc},
    time::{Duration, Instant},
};
use tokio::{
    sync::mpsc,
    time::{self, MissedTickBehavior},
};
use tracing::{error, info};

pub(crate) fn spawn(
    mut event_rx: mpsc::Receiver<EventMsg>,
    client: OpsClient,
    batch_size: usize,
    flush_interval: Duration,
    metrics: Arc<ForwarderMetrics>,
) {
    tokio::spawn(async move {
        let mut events = Vec::with_capacity(batch_size);
        let mut flush_ticker = time::interval(flush_interval);
        flush_ticker.set_missed_tick_behavior(MissedTickBehavior::Delay);
        flush_ticker.tick().await;
        let mut stats_ticker = time::interval(Duration::from_secs(60));
        stats_ticker.tick().await;

        loop {
            tokio::select! {
                message = event_rx.recv() => match message {
                    Some(EventMsg::Event(event)) => {
                        events.push(event);
                        if events.len() >= batch_size {
                            send_batch(&client, take_batch(&mut events, batch_size), &metrics).await;
                        }
                    }
                    Some(EventMsg::Flush(reply)) => {
                        if !events.is_empty() {
                            send_batch(&client, take_batch(&mut events, batch_size), &metrics).await;
                        }
                        let _ = reply.send(());
                    }
                    None => {
                        if !events.is_empty() {
                            send_batch(&client, take_batch(&mut events, batch_size), &metrics).await;
                        }
                        break;
                    }
                },
                _ = flush_ticker.tick() => {
                    if !events.is_empty() {
                        send_batch(&client, take_batch(&mut events, batch_size), &metrics).await;
                    }
                }
                _ = stats_ticker.tick() => info!(stats = ?metrics.snapshot(), "CubeOps internal forwarding statistics"),
            }
        }

        info!(stats = ?metrics.snapshot(), "CubeOps internal forwarding statistics");
    });
}

fn take_batch(events: &mut Vec<LogEvent>, batch_size: usize) -> InternalBatch {
    InternalBatch::new(std::mem::replace(events, Vec::with_capacity(batch_size)))
}

async fn send_batch(client: &OpsClient, batch: InternalBatch, metrics: &ForwarderMetrics) {
    let event_count = batch.event_count();
    metrics.batches_formed.fetch_add(1, Ordering::Relaxed);
    tracing::debug!(event_count, "CubeOps event batch formed");

    let started = Instant::now();
    match client.send_once(&batch).await {
        Ok(StatusCode::ACCEPTED) => {
            metrics.batches_sent.fetch_add(1, Ordering::Relaxed);
            tracing::debug!(
                event_count,
                send_latency_ms = started.elapsed().as_millis(),
                "CubeOps event batch sent"
            );
        }
        Ok(status) => {
            metrics.batches_failed.fetch_add(1, Ordering::Relaxed);
            metrics.record_status_failure(status.as_u16());
            error!(%status, event_count, send_latency_ms = started.elapsed().as_millis(), "CubeOps rejected event batch; dropping batch");
        }
        Err(err) => {
            metrics.batches_failed.fetch_add(1, Ordering::Relaxed);
            if err.is_timeout() {
                metrics.timeout_failures.fetch_add(1, Ordering::Relaxed);
            } else if err.is_connect() {
                metrics.connection_failures.fetch_add(1, Ordering::Relaxed);
            } else {
                metrics.request_failures.fetch_add(1, Ordering::Relaxed);
            }
            error!(error = %err, event_count, send_latency_ms = started.elapsed().as_millis(), "CubeOps event request failed; dropping batch");
        }
    }
}

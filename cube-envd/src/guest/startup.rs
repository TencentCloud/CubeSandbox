// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use std::collections::VecDeque;
use std::sync::Arc;
use std::time::Duration;

use crate::init::metadata::{fetch_metadata, mmds_client, Metadata, MMDS_ADDRESS};

const MMDS_POLL_INTERVAL: Duration = Duration::from_millis(50);
const MMDS_REQUEST_TIMEOUT: Duration = Duration::from_secs(10);
const MMDS_REFRESH_TIMEOUT: Duration = Duration::from_secs(60);
const LOG_RETRY_INTERVAL: Duration = Duration::from_millis(100);
const LOG_SHUTDOWN_FLUSH: Duration = Duration::from_millis(500);

pub(crate) fn refresh_channel() -> (
    tokio::sync::mpsc::Sender<()>,
    tokio::sync::mpsc::Receiver<()>,
) {
    tokio::sync::mpsc::channel(1)
}

pub(crate) async fn metadata_service(
    runtime: Arc<crate::runtime::RuntimeStateStore>,
    metadata: tokio::sync::watch::Sender<Option<Arc<Metadata>>>,
    mut refresh: tokio::sync::mpsc::Receiver<()>,
    mut shutdown: tokio::sync::watch::Receiver<bool>,
) -> Result<(), ()> {
    let client = mmds_client().map_err(|_| ())?;
    if !poll_metadata(&client, &runtime, &metadata, &mut shutdown, None).await {
        return Ok(());
    }
    loop {
        tokio::select! {
            biased;
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow_and_update() {
                    return Ok(());
                }
            }
            next = refresh.recv() => {
                if next.is_none() {
                    return Ok(());
                }
                let deadline = tokio::time::Instant::now() + MMDS_REFRESH_TIMEOUT;
                if !poll_metadata(&client, &runtime, &metadata, &mut shutdown, Some(deadline)).await {
                    return Ok(());
                }
            }
        }
    }
}

async fn poll_metadata(
    client: &reqwest::Client,
    runtime: &crate::runtime::RuntimeStateStore,
    sender: &tokio::sync::watch::Sender<Option<Arc<Metadata>>>,
    shutdown: &mut tokio::sync::watch::Receiver<bool>,
    deadline: Option<tokio::time::Instant>,
) -> bool {
    loop {
        let fetched = tokio::select! {
            biased;
            _ = shutdown.wait_for(|stopping| *stopping) => return false,
            _ = async {
                match deadline {
                    Some(deadline) => tokio::time::sleep_until(deadline).await,
                    None => std::future::pending().await,
                }
            } => {
                tracing::warn!("MMDS metadata refresh timed out");
                return true;
            }
            result = fetch_metadata(client, MMDS_ADDRESS) => result,
        };
        if let Ok(metadata) = fetched {
            runtime.project_metadata(&metadata).await;
            let _ = sender.send(Some(Arc::new(metadata)));
            return true;
        }
        tokio::select! {
            biased;
            _ = shutdown.wait_for(|stopping| *stopping) => return false,
            _ = tokio::time::sleep(MMDS_POLL_INTERVAL) => {}
        }
    }
}

pub(crate) async fn log_exporter(
    mut logs: crate::telemetry::LogReceiver,
    mut metadata: tokio::sync::watch::Receiver<Option<Arc<Metadata>>>,
    mut shutdown: tokio::sync::watch::Receiver<bool>,
) -> Result<(), ()> {
    let client = reqwest::Client::builder()
        .timeout(MMDS_REQUEST_TIMEOUT)
        .build()
        .map_err(|_| ())?;
    let mut pending: VecDeque<Vec<u8>> = VecDeque::new();
    loop {
        if *shutdown.borrow() {
            break;
        }
        let current = metadata.borrow().clone();
        if let (Some(line), Some(current)) = (pending.front(), current) {
            if !current.collector_address.is_empty() {
                let sent = tokio::select! {
                    biased;
                    _ = shutdown.wait_for(|stopping| *stopping) => break,
                    sent = send_log(&client, line, &current) => sent,
                };
                if sent {
                    pending.pop_front();
                } else {
                    tokio::select! {
                        biased;
                        _ = shutdown.wait_for(|stopping| *stopping) => break,
                        _ = tokio::time::sleep(LOG_RETRY_INTERVAL) => {}
                    }
                }
                continue;
            }
        }
        tokio::select! {
            biased;
            _ = shutdown.wait_for(|stopping| *stopping) => break,
            line = logs.recv() => {
                match line {
                    Some(line) => pending.push_back(line),
                    None => return Ok(()),
                }
            }
            changed = metadata.changed() => {
                if changed.is_err() {
                    return Ok(());
                }
            }
        }
    }
    flush_logs(&client, &metadata, &mut logs, &mut pending).await;
    Ok(())
}

async fn send_log(client: &reqwest::Client, line: &[u8], metadata: &Metadata) -> bool {
    let Some(payload) = enrich_log(line, metadata) else {
        return true;
    };
    client
        .post(&metadata.collector_address)
        .header(http::header::CONTENT_TYPE, "application/json")
        .body(payload)
        .send()
        .await
        .is_ok()
}

async fn flush_logs(
    client: &reqwest::Client,
    metadata: &tokio::sync::watch::Receiver<Option<Arc<Metadata>>>,
    logs: &mut crate::telemetry::LogReceiver,
    pending: &mut VecDeque<Vec<u8>>,
) {
    let Some(current) = metadata.borrow().clone() else {
        return;
    };
    if current.collector_address.is_empty() {
        return;
    }
    let _ = tokio::time::timeout(LOG_SHUTDOWN_FLUSH, async {
        loop {
            let line = match pending.pop_front().or_else(|| logs.try_recv().ok()) {
                Some(line) => line,
                None => return,
            };
            while !send_log(client, &line, &current).await {
                tokio::time::sleep(LOG_RETRY_INTERVAL).await;
            }
        }
    })
    .await;
}

fn enrich_log(line: &[u8], metadata: &Metadata) -> Option<Vec<u8>> {
    let mut value: serde_json::Value = serde_json::from_slice(line).ok()?;
    let object = value.as_object_mut()?;
    object.insert(
        "instanceID".into(),
        serde_json::Value::String(metadata.sandbox_id.clone()),
    );
    object.insert(
        "envID".into(),
        serde_json::Value::String(metadata.template_id.clone()),
    );
    serde_json::to_vec(&value).ok()
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use std::io::Write;
use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::Arc;

use crash_recovery_worker::{app, Abort, Worker};
use tokio::net::TcpListener;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let address: SocketAddr = std::env::var("WORKER_ADDR")
        .unwrap_or_else(|_| "0.0.0.0:8080".to_owned())
        .parse()?;
    let audit = std::env::var_os("AUDIT_PATH")
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from("/workspace/crash-recovery/audit.jsonl"));

    if let Some(parent) = audit.parent() {
        std::fs::create_dir_all(parent)?;
    }

    let (worker, listener) = (Worker::open(audit)?, TcpListener::bind(address).await?);
    let address = listener.local_addr()?;

    println!("listening=http://{address}");
    std::io::stdout().flush()?;

    axum::serve(listener, app(worker, Arc::new(Abort)))
        .with_graceful_shutdown(shutdown())
        .await?;

    Ok(())
}

async fn shutdown() {
    let _ = tokio::signal::ctrl_c().await;
}

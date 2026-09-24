// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

//! Scheduling fixture for real VM capture of otherwise short Process windows.
//! This module exists only in the library test executable. The normal daemon
//! has no hook, configuration, extra API, or restore-time behavior.

use std::path::Path;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use crate::error::DomainError;
use tokio::time::Instant;

static SNAPSHOT_SERVER: AtomicBool = AtomicBool::new(false);
const DIRECTORY: &str = "/tmp/cube-envd-process-capture";

pub(super) async fn barrier(
    stage: &str,
    tag: Option<&str>,
    deadline: Option<Instant>,
) -> Result<(), DomainError> {
    if !SNAPSHOT_SERVER.load(Ordering::Relaxed) {
        return Ok(());
    }
    let Some(tag) = tag.filter(|tag| {
        !tag.is_empty() && tag.bytes().all(|c| c.is_ascii_alphanumeric() || c == b'-')
    }) else {
        return Ok(());
    };
    let path = |suffix| Path::new(DIRECTORY).join(format!("{tag}.{stage}.{suffix}"));
    match tokio::fs::remove_file(path("arm")).await {
        Ok(()) => {}
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(_) => return Err(DomainError::Internal),
    }
    tokio::fs::write(path("entered"), stage)
        .await
        .map_err(|_| DomainError::Internal)?;
    let release = path("release");
    let waiting = async {
        while !tokio::fs::try_exists(&release)
            .await
            .map_err(|_| DomainError::Internal)?
        {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        tokio::fs::remove_file(release)
            .await
            .map_err(|_| DomainError::Internal)
    };
    if let Some(deadline) = deadline {
        tokio::time::timeout_at(deadline, waiting)
            .await
            .map_err(|_| DomainError::DeadlineExceeded)?
    } else {
        waiting.await
    }
}

pub(super) fn signal_barrier(
    selector: &crate::proto::process::ProcessSelector,
) -> Result<(), DomainError> {
    if !SNAPSHOT_SERVER.load(Ordering::Relaxed) {
        return Ok(());
    }
    let Some(crate::proto::process::process_selector::Selector::Tag(tag)) = &selector.selector
    else {
        return Ok(());
    };
    // SendSignal is synchronous. Hold its caller before lookup while allowing
    // other runtime workers to serve the public barrier release request.
    tokio::task::block_in_place(|| {
        tokio::runtime::Handle::current().block_on(barrier("Signal", Some(tag), None))
    })
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "long-running HTTP daemon fixture, explicitly launched inside a snapshot test VM"]
async fn snapshot_server() {
    SNAPSHOT_SERVER.store(true, Ordering::Relaxed);
    eprintln!("snapshot scheduling fixture revision={}", crate::REVISION);
    crate::server::run_server(crate::server::ServerConfig {
        port: 49983,
        is_not_fc: true,
    })
    .await
    .unwrap();
}

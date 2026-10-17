// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

//! Bounded scheduling/observation for real VM tests, absent from normal builds.

use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use crate::error::DomainError;

static ENABLED: AtomicBool = AtomicBool::new(false);
const DIRECTORY: &str = "/tmp/cube-envd-files-init-capture";

pub(crate) struct Attempt(Option<PathBuf>);

impl Drop for Attempt {
    fn drop(&mut self) {
        if let Some(path) = &self.0 {
            // Marks either normal return or cancellation of the actual future.
            // Fixed-size test output; no task survives a dropped init request.
            let _ = std::fs::write(path, "done");
        }
    }
}

pub(crate) fn attempt_at(stage: &str, tag: Option<&str>) -> Attempt {
    Attempt(marker(stage, tag, "done"))
}

pub(crate) fn enable() {
    ENABLED.store(true, Ordering::Relaxed);
}

fn marker(stage: &str, tag: Option<&str>, suffix: &str) -> Option<PathBuf> {
    if !ENABLED.load(Ordering::Relaxed) {
        return None;
    }
    let tag = tag.filter(|tag| {
        tag.starts_with("capture-")
            && tag.len() <= 64
            && tag
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
    })?;
    Some(Path::new(DIRECTORY).join(format!("{tag}.{stage}.{suffix}")))
}

pub(crate) async fn barrier(stage: &str, tag: Option<&str>) -> Result<(), DomainError> {
    let Some(arm) = marker(stage, tag, "arm") else {
        return Ok(());
    };
    match tokio::fs::remove_file(arm).await {
        Ok(()) => {}
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(_) => return Err(DomainError::Internal),
    }
    tokio::fs::write(marker(stage, tag, "entered").unwrap(), stage)
        .await
        .map_err(|_| DomainError::Internal)?;
    let release = marker(stage, tag, "release").unwrap();
    tokio::time::timeout(Duration::from_secs(120), async {
        while !tokio::fs::try_exists(&release)
            .await
            .map_err(|_| DomainError::Internal)?
        {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        tokio::fs::remove_file(release)
            .await
            .map_err(|_| DomainError::Internal)
    })
    .await
    .map_err(|_| DomainError::DeadlineExceeded)?
}

pub(crate) fn file_barrier(
    stage: &str,
    path: &Path,
    cancel: &AtomicBool,
) -> Result<(), DomainError> {
    let tag = path.file_name().and_then(|name| name.to_str());
    let Some(arm) = marker(stage, tag, "arm") else {
        return Ok(());
    };
    match std::fs::remove_file(arm) {
        Ok(()) => {}
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(_) => return Err(DomainError::Internal),
    }
    let _finished = Attempt(marker(stage, tag, "done"));
    std::fs::write(marker(stage, tag, "entered").unwrap(), stage)
        .map_err(|_| DomainError::Internal)?;
    let release = marker(stage, tag, "release").unwrap();
    let deadline = std::time::Instant::now() + Duration::from_secs(120);
    loop {
        if cancel.load(Ordering::Acquire) {
            return Err(DomainError::Cancelled);
        }
        if release.try_exists().map_err(|_| DomainError::Internal)? {
            std::fs::remove_file(release).map_err(|_| DomainError::Internal)?;
            return Ok(());
        }
        if std::time::Instant::now() >= deadline {
            return Err(DomainError::DeadlineExceeded);
        }
        std::thread::sleep(Duration::from_millis(10));
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "long-running HTTP daemon fixture, explicitly launched inside a snapshot test VM"]
async fn snapshot_server() {
    ENABLED.store(true, Ordering::Relaxed);
    eprintln!(
        "init/filesystem scheduling fixture revision={}",
        crate::REVISION
    );
    crate::server::run_server(crate::server::ServerConfig {
        port: 49983,
        is_not_fc: true,
    })
    .await
    .unwrap();
}

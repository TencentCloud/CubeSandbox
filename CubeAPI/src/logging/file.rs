// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Async rolling-file log backend.
//!
//! Events are serialised as NDJSON (one JSON object per line) and written to
//! a file named `<dir>/<prefix>-YYYY-MM-DD.log`.  The file rotates at midnight
//! UTC.
//!
//! The writer runs in a dedicated background Tokio task and communicates via
//! an unbounded mpsc channel.  `log()` is therefore non-blocking from the
//! caller's perspective — it just sends into the channel.

use async_trait::async_trait;
use chrono::Utc;
use tokio::{
    fs::{self, OpenOptions},
    io::AsyncWriteExt,
    sync::mpsc::{self, UnboundedSender},
};
use tracing::error;

use super::{LogEvent, Logger};

// ─── Message enum ──────────────────────────────────────────────────────────

enum Msg {
    Event(LogEvent),
    Flush(tokio::sync::oneshot::Sender<()>),
}

// ─── FileLogger ────────────────────────────────────────────────────────────

/// Async rolling-file logger.
///
/// Clone is O(1) — only the channel sender is cloned.
#[derive(Clone)]
pub struct FileLogger {
    tx: UnboundedSender<Msg>,
}

impl FileLogger {
    /// Create a logger that writes to `<log_dir>/<prefix>-YYYY-MM-DD.log`.
    ///
    /// Spawns a background writer task immediately.
    pub async fn new(
        log_dir: impl Into<String>,
        prefix: impl Into<String>,
    ) -> anyhow::Result<Self> {
        let log_dir = log_dir.into();
        let prefix = prefix.into();

        // Ensure the directory exists.
        fs::create_dir_all(&log_dir).await?;

        let (tx, mut rx) = mpsc::unbounded_channel::<Msg>();

        tokio::spawn(async move {
            // Current open file state
            let mut current_date = String::new();
            let mut file: Option<tokio::fs::File> = None;

            while let Some(msg) = rx.recv().await {
                match msg {
                    Msg::Flush(reply) => {
                        // Flush the underlying OS buffers if a file is open.
                        if let Some(ref mut f) = file {
                            let _ = f.flush().await;
                        }
                        let _ = reply.send(());
                    }

                    Msg::Event(event) => {
                        // Check if we need to rotate (new calendar day).
                        let today = Utc::now().format("%Y-%m-%d").to_string();
                        if today != current_date || file.is_none() {
                            // Flush + close old file
                            if let Some(ref mut f) = file {
                                let _ = f.flush().await;
                            }
                            let path = format!("{}/{}-{}.log", log_dir, prefix, today);
                            match OpenOptions::new()
                                .create(true)
                                .append(true)
                                .open(&path)
                                .await
                            {
                                Ok(f) => {
                                    file = Some(f);
                                    current_date = today;
                                }
                                Err(e) => {
                                    error!("FileLogger: failed to open {}: {}", path, e);
                                    file = None;
                                }
                            }
                        }

                        // Serialise and write.
                        if let Some(ref mut f) = file {
                            match serde_json::to_string(&event) {
                                Ok(mut line) => {
                                    line.push('\n');
                                    if let Err(e) = f.write_all(line.as_bytes()).await {
                                        error!("FileLogger: write error: {}", e);
                                    }
                                }
                                Err(e) => {
                                    error!("FileLogger: serialise error: {}", e);
                                }
                            }
                        }
                    }
                }
            }

            // Channel closed — flush final buffer.
            if let Some(ref mut f) = file {
                let _ = f.flush().await;
            }
        });

        Ok(Self { tx })
    }
}

#[async_trait]
impl Logger for FileLogger {
    async fn log(&self, event: LogEvent) {
        // Non-blocking: if the channel is full (it's unbounded, so only OOM
        // would fail) just drop and log a tracing warning.
        if self.tx.send(Msg::Event(event)).is_err() {
            error!("FileLogger: writer task is gone, dropping event");
        }
    }

    async fn flush(&self) {
        let (tx, rx) = tokio::sync::oneshot::channel();
        if self.tx.send(Msg::Flush(tx)).is_ok() {
            let _ = rx.await;
        }
    }

    fn name(&self) -> &'static str {
        "file"
    }
}

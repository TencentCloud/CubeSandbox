// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! HTTP webhook log backend — stub.
//!
//! Sends log events as JSON POST requests to a configurable endpoint.
//! Useful for forwarding to log aggregators (Elasticsearch, Loki, custom
//! ingest APIs) that accept HTTP.
//!
//! # TODO (wire-up checklist)
//!
//! 1. Add `http_log_url: Option<String>` to `ServerConfig`.
//! 2. Construct `HttpLogger::new(client, url, batch_size, flush_interval)`.
//! 3. Register it in `AppState::new()` inside `MultiLogger`.
//!
//! # Batching strategy (suggested)
//!
//! Buffer events into a `Vec<LogEvent>` and POST when either:
//! - The buffer reaches `batch_size` (default 100), or
//! - A ticker fires every `flush_interval` (default 5 s).
//!
//! Use `tokio::select!` in the background task to wait on whichever fires
//! first.

use super::{LogEvent, Logger};
use async_trait::async_trait;

/// Configuration for the HTTP webhook backend.
#[derive(Debug, Clone)]
#[allow(dead_code)]
pub struct HttpLoggerConfig {
    /// Full URL to POST batches to, e.g. `"http://log-ingest.internal/api/logs"`.
    pub url: String,
    /// Max events per batch (default: 100).
    pub batch_size: usize,
    /// Flush interval in seconds even if batch is not full (default: 5).
    pub flush_interval_secs: u64,
}

impl Default for HttpLoggerConfig {
    fn default() -> Self {
        Self {
            url: String::new(),
            batch_size: 100,
            flush_interval_secs: 5,
        }
    }
}

/// HTTP webhook log backend (stub — not yet implemented).
#[derive(Clone)]
pub struct HttpLogger {
    #[allow(dead_code)]
    config: HttpLoggerConfig,
}

impl HttpLogger {
    /// Create an `HttpLogger`.  Currently a no-op; implement the background
    /// task described in the module doc when ready.
    #[allow(dead_code)]
    pub fn new(config: HttpLoggerConfig) -> Self {
        Self { config }
    }
}

#[async_trait]
impl Logger for HttpLogger {
    async fn log(&self, _event: LogEvent) {
        // TODO: send to internal channel; background task batches + POSTs.
        // Example batch payload:
        // POST config.url
        // Content-Type: application/json
        // Body: { "events": [ <LogEvent>, ... ] }
    }

    async fn flush(&self) {
        // TODO: drain the buffer and send the final batch.
    }

    fn name(&self) -> &'static str {
        "http"
    }
}

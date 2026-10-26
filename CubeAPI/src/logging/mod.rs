// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Structured event logging module.
//!
//! # Design
//!
//! All log backends implement the [`Logger`] async trait. The primary backend
//! is [`FileLogger`] which writes newline-delimited JSON to a rolling daily
//! file **asynchronously** via a background Tokio task (channel → writer loop).
//!
//! Additional backends can be plugged in by implementing [`Logger`] and
//! registering them in [`MultiLogger`].  Ready-made stubs are provided for:
//!
//! | Module         | Backend                        | Status   |
//! |----------------|-------------------------------|----------|
//! | `file`         | Async rolling file (NDJSON)   | ✅ Ready |
//! | `noop`         | Discard all events            | ✅ Ready |
//! | `multi`        | Fan-out to N backends         | ✅ Ready |
//! | `filtered`     | Min-level gate wrapper        | ✅ Ready |
//! | `otlp`         | OpenTelemetry OTLP exporter   | 🔲 Stub  |
//! | `http`         | Generic HTTP webhook          | 🔲 Stub  |

pub mod file;
pub mod filtered;
pub mod http;
pub mod multi;
pub mod noop;
pub mod otlp;

use async_trait::async_trait;
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::sync::Arc;

// ─── Log level ─────────────────────────────────────────────────────────────

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum LogLevel {
    Debug,
    Info,
    Warn,
    Error,
}

impl std::fmt::Display for LogLevel {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let s = match self {
            LogLevel::Debug => "debug",
            LogLevel::Info => "info",
            LogLevel::Warn => "warn",
            LogLevel::Error => "error",
        };
        f.write_str(s)
    }
}

// ─── Log event ─────────────────────────────────────────────────────────────

/// A single structured log event emitted by the application.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct LogEvent {
    pub timestamp: DateTime<Utc>,
    pub level: LogLevel,
    /// Short machine-readable event name, e.g. `"sandbox.created"`.
    pub event: String,
    /// Arbitrary structured fields.
    #[serde(flatten)]
    pub fields: HashMap<String, serde_json::Value>,
}

impl LogEvent {
    pub fn new(level: LogLevel, event: impl Into<String>) -> Self {
        Self {
            timestamp: Utc::now(),
            level,
            event: event.into(),
            fields: HashMap::new(),
        }
    }

    /// Attach a string field (builder-style).
    pub fn field(mut self, key: impl Into<String>, value: impl Into<String>) -> Self {
        self.fields
            .insert(key.into(), serde_json::Value::String(value.into()));
        self
    }

    /// Attach any serialisable value as a field (builder-style).
    pub fn field_value(mut self, key: impl Into<String>, value: impl Serialize) -> Self {
        if let Ok(v) = serde_json::to_value(value) {
            self.fields.insert(key.into(), v);
        }
        self
    }
}

// ─── Logger trait ──────────────────────────────────────────────────────────

/// Core abstraction for all log backends.
///
/// Implementations must be `Send + Sync + 'static` so they can live in `AppState`.
///
/// # Implementing a new backend
///
/// ```rust,no_run
/// use async_trait::async_trait;
///
/// struct MyBackend;
///
/// #[async_trait]
/// impl Logger for MyBackend {
///     async fn log(&self, event: LogEvent) { /* ... */ }
///     async fn flush(&self) {}
///     fn name(&self) -> &'static str { "my-backend" }
/// }
/// ```
#[async_trait]
pub trait Logger: Send + Sync + 'static {
    /// Emit a single log event.  Implementations must never block the caller.
    async fn log(&self, event: LogEvent);

    /// Flush buffered events.  Called during graceful shutdown.
    async fn flush(&self) {}

    /// Human-readable backend name for diagnostics.
    #[allow(dead_code)]
    fn name(&self) -> &'static str;
}

// ─── Arc-wrapped convenience ───────────────────────────────────────────────

/// A cheaply-cloneable handle to any `Logger`.
pub type ArcLogger = Arc<dyn Logger>;

/// Wrap any `Logger` in an `Arc`.
pub fn arc<L: Logger>(logger: L) -> ArcLogger {
    Arc::new(logger)
}

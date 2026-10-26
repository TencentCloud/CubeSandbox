// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Min-level filter wrapper.
//!
//! Wraps any `Logger` and drops events below the configured minimum level.
//! This is the primary mechanism for switching between info and debug mode:
//!
//! ```rust,no_run
//! use cube_api::logging::{arc, filtered::FilteredLogger, LogLevel, noop::NoopLogger};
//!
//! // In debug mode (--debug flag):
//! let logger = arc(FilteredLogger::new(arc(NoopLogger), LogLevel::Debug));
//!
//! // In normal mode (default):
//! let logger = arc(FilteredLogger::new(arc(NoopLogger), LogLevel::Info));
//! ```

use super::{ArcLogger, LogEvent, LogLevel, Logger};
use async_trait::async_trait;

#[derive(Clone)]
pub struct FilteredLogger {
    inner: ArcLogger,
    min_level: LogLevel,
}

impl FilteredLogger {
    /// Wrap `inner`, dropping any event with `level < min_level`.
    pub fn new(inner: ArcLogger, min_level: LogLevel) -> Self {
        Self { inner, min_level }
    }
}

#[async_trait]
impl Logger for FilteredLogger {
    async fn log(&self, event: LogEvent) {
        if event.level >= self.min_level {
            self.inner.log(event).await;
        }
    }

    async fn flush(&self) {
        self.inner.flush().await;
    }

    fn name(&self) -> &'static str {
        "filtered"
    }
}

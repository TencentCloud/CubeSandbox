// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Multi-backend fan-out logger.
//!
//! Dispatches every event to all registered backends concurrently.
//! Backends are stored as `ArcLogger` so they can be of different types.
//!
//! # Example
//!
//! ```rust,no_run
//! use cube_api::logging::{arc, multi::MultiLogger, noop::NoopLogger, file::FileLogger};
//!
//! let logger = MultiLogger::new()
//!     .add(arc(NoopLogger))
//!     .add(arc(FileLogger::new("/var/log/cube-api", "cube-api").await?));
//! ```

use async_trait::async_trait;
use futures::future::join_all;

use super::{ArcLogger, LogEvent, Logger};

#[derive(Clone, Default)]
pub struct MultiLogger {
    backends: Vec<ArcLogger>,
}

impl MultiLogger {
    pub fn new() -> Self {
        Self::default()
    }

    /// Register an additional backend. Returns `self` for chaining.
    pub fn add(mut self, backend: ArcLogger) -> Self {
        self.backends.push(backend);
        self
    }
}

#[async_trait]
impl Logger for MultiLogger {
    async fn log(&self, event: LogEvent) {
        let futs: Vec<_> = self
            .backends
            .iter()
            .map(|b| {
                let b = b.clone();
                let ev = event.clone();
                async move { b.log(ev).await }
            })
            .collect();
        join_all(futs).await;
    }

    async fn flush(&self) {
        let futs: Vec<_> = self
            .backends
            .iter()
            .map(|b| {
                let b = b.clone();
                async move { b.flush().await }
            })
            .collect();
        join_all(futs).await;
    }

    fn name(&self) -> &'static str {
        "multi"
    }
}

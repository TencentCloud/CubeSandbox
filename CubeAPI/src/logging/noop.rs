// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! No-op logger — silently discards all events.
//!
//! Useful in tests, benchmarks, or when logging is explicitly disabled.

use super::{LogEvent, Logger};
use async_trait::async_trait;

#[derive(Clone, Default)]
pub struct NoopLogger;

#[async_trait]
impl Logger for NoopLogger {
    async fn log(&self, _event: LogEvent) {}
    fn name(&self) -> &'static str {
        "noop"
    }
}

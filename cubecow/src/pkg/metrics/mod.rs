// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// In-memory metrics collector for runtime monitoring
//
// Collects key metrics (filesystem usage, volume count, snapshot count,
// etc.) and exposes them via the library API.

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;

use dashmap::DashMap;

// ---------------------------------------------------------------------------
// Well-known metric keys
// ---------------------------------------------------------------------------

/// Total number of active volumes
pub const METRIC_VOLUME_COUNT: &str = "volume_count";
/// Total number of active snapshots
pub const METRIC_SNAPSHOT_COUNT: &str = "snapshot_count";
/// Total bytes available on the underlying filesystem (`statvfs`)
pub const METRIC_TOTAL_BYTES: &str = "total_bytes";
/// Used bytes on the underlying filesystem (`statvfs`)
pub const METRIC_USED_BYTES: &str = "used_bytes";

// ---------------------------------------------------------------------------
// MetricsCollector
// ---------------------------------------------------------------------------

/// Thread-safe in-memory metrics collector.
///
/// Uses `DashMap<String, AtomicU64>` for lock-free concurrent access.
/// Each module reports metrics by calling `inc()`, `set()`, etc.
#[derive(Debug, Clone)]
pub struct MetricsCollector {
    counters: Arc<DashMap<String, AtomicU64>>,
}

impl Default for MetricsCollector {
    fn default() -> Self {
        Self::new()
    }
}

#[allow(dead_code)]
impl MetricsCollector {
    /// Create a new metrics collector with pre-defined metric keys.
    pub fn new() -> Self {
        let counters = Arc::new(DashMap::new());

        // Pre-register well-known metrics
        let keys = [
            METRIC_VOLUME_COUNT,
            METRIC_SNAPSHOT_COUNT,
            METRIC_TOTAL_BYTES,
            METRIC_USED_BYTES,
        ];
        for key in keys {
            counters.insert(key.to_string(), AtomicU64::new(0));
        }

        Self { counters }
    }

    /// Increment a counter by 1.
    pub fn inc(&self, key: &str) {
        self.counters
            .entry(key.to_string())
            .or_insert_with(|| AtomicU64::new(0))
            .fetch_add(1, Ordering::Relaxed);
    }

    /// Increment a counter by a given delta.
    pub fn inc_by(&self, key: &str, delta: u64) {
        self.counters
            .entry(key.to_string())
            .or_insert_with(|| AtomicU64::new(0))
            .fetch_add(delta, Ordering::Relaxed);
    }

    /// Decrement a counter by 1 (saturating).
    pub fn dec(&self, key: &str) {
        if let Some(entry) = self.counters.get(key) {
            // Saturating decrement to avoid underflow
            let _ = entry.fetch_update(Ordering::Relaxed, Ordering::Relaxed, |v| {
                Some(v.saturating_sub(1))
            });
        }
    }

    /// Set a gauge to an absolute value.
    pub fn set(&self, key: &str, value: u64) {
        self.counters
            .entry(key.to_string())
            .or_insert_with(|| AtomicU64::new(0))
            .store(value, Ordering::Relaxed);
    }

    /// Get the current value of a metric.
    pub fn get(&self, key: &str) -> u64 {
        self.counters
            .get(key)
            .map(|v| v.load(Ordering::Relaxed))
            .unwrap_or(0)
    }

    /// Take a snapshot of all current metric values.
    pub fn snapshot(&self) -> HashMap<String, u64> {
        let mut map = HashMap::with_capacity(self.counters.len());
        for entry in self.counters.iter() {
            map.insert(entry.key().clone(), entry.value().load(Ordering::Relaxed));
        }
        map
    }
}

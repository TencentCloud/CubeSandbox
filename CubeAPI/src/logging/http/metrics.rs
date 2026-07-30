// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use std::{
    collections::BTreeMap,
    sync::{
        atomic::{AtomicU64, Ordering},
        Mutex,
    },
};

#[derive(Default)]
pub(crate) struct ForwarderMetrics {
    pub(crate) events_enqueued: AtomicU64,
    pub(crate) events_dropped: AtomicU64,
    pub(crate) batches_formed: AtomicU64,
    pub(crate) batches_sent: AtomicU64,
    pub(crate) batches_failed: AtomicU64,
    pub(crate) connection_failures: AtomicU64,
    pub(crate) timeout_failures: AtomicU64,
    pub(crate) request_failures: AtomicU64,
    status_failures: Mutex<BTreeMap<u16, u64>>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ForwarderStats {
    pub events_enqueued: u64,
    pub events_dropped: u64,
    pub batches_formed: u64,
    pub batches_sent: u64,
    pub batches_failed: u64,
    pub connection_failures: u64,
    pub timeout_failures: u64,
    pub request_failures: u64,
    pub status_failures: BTreeMap<u16, u64>,
}

impl ForwarderMetrics {
    pub(crate) fn snapshot(&self) -> ForwarderStats {
        ForwarderStats {
            events_enqueued: self.events_enqueued.load(Ordering::Relaxed),
            events_dropped: self.events_dropped.load(Ordering::Relaxed),
            batches_formed: self.batches_formed.load(Ordering::Relaxed),
            batches_sent: self.batches_sent.load(Ordering::Relaxed),
            batches_failed: self.batches_failed.load(Ordering::Relaxed),
            connection_failures: self.connection_failures.load(Ordering::Relaxed),
            timeout_failures: self.timeout_failures.load(Ordering::Relaxed),
            request_failures: self.request_failures.load(Ordering::Relaxed),
            status_failures: self.status_failures.lock().unwrap().clone(),
        }
    }

    pub(crate) fn record_status_failure(&self, status: u16) {
        let mut failures = self.status_failures.lock().unwrap();
        *failures.entry(status).or_default() += 1;
    }
}

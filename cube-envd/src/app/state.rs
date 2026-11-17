// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Composition root: the two process-wide state holders the daemon shares —
//! `Config` (env, defaults, token, /init timestamp) and `ProcessTable` (live
//! processes and their cgroup leaves).
//!
//! Nothing else lives here. Handlers reach the pieces through axum's `FromRef`
//! (so a domain handler can ask for `State<Arc<Config>>` without the router
//! handing it the whole root), and `main.rs` swaps in the real cgroup manager
//! once at startup.

use std::sync::Arc;

use crate::platform::config::Config;
use crate::process::cgroup::Manager;
use crate::process::table::ProcessTable;

pub struct AppState {
    pub config: Arc<Config>,
    pub processes: Arc<ProcessTable>,
}

impl AppState {
    pub fn new() -> Self {
        Self {
            config: Arc::new(Config::new()),
            processes: Arc::new(ProcessTable::new(Arc::new(
                crate::process::cgroup::NoopManager,
            ))),
        }
    }

    /// Startup wiring (main.rs calls this once): swap in the real cgroup
    /// manager and keep it for the daemon lifetime. `new()` stays no-op so
    /// every unit test constructs the state without probing the host cgroup
    /// tree; the manager choice is then fixed by this single call.
    pub fn with_cgroup(mut self, cgroup: Arc<dyn Manager>) -> Self {
        self.processes = Arc::new(ProcessTable::new(cgroup));
        self
    }
}

impl Default for AppState {
    fn default() -> Self {
        Self::new()
    }
}

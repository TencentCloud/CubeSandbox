// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! `NoopManager` — startup failure fallback (upstream noop.go): no cgroup
//! enforcement, never blocks startup. Chosen once at startup when cgroup v2
//! is unavailable; runtime placement failures do **not** fall back (fail-fast
//! in exec.rs, plan §3.3).

use std::os::unix::io::RawFd;

use super::{Manager, ProcType};

pub struct NoopManager;

impl Manager for NoopManager {
    fn fd(&self, _t: ProcType) -> Option<RawFd> {
        None
    }
}

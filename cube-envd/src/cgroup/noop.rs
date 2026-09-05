// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! `NoopManager` — startup failure fallback (upstream noop.go): no cgroup
//! enforcement, never blocks startup. Chosen once at startup when cgroup v2
//! is unavailable; runtime failures of a real manager reject the command.

#[cfg(test)]
use std::os::unix::io::RawFd;

use super::Manager;
#[cfg(test)]
use super::ProcType;

pub struct NoopManager;

impl Manager for NoopManager {
    #[cfg(test)]
    fn fd(&self, _t: ProcType) -> Option<RawFd> {
        None
    }
}

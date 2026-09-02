// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! cgroup v2 manager (item 1.8). Mirrors upstream
//! `packages/envd/internal/services/cgroups` (iface.go / cgroup2.go /
//! noop.go) and the startup wiring of upstream `createCgroupManager`
//! (`cmd/envd/main.go:244-293`). Only the `ptys` and `user` subtrees are
//! created — `socat` is dropped because CubeSandbox has no port forwarding.
//!
//! Startup contract: `init()` builds the manager once. On any failure it logs
//! the reason and falls back to a `NoopManager` (never blocks startup); the
//! choice is fixed for the daemon lifetime. Runtime *placement* failures (in
//! exec.rs) are fail-fast and never fall back.

mod cgroup2;
mod noop;

use std::collections::HashMap;
use std::os::unix::io::RawFd;
use std::path::Path;
use std::sync::Arc;

pub use cgroup2::Cgroup2Manager;
pub use noop::NoopManager;

/// cgroup v2 filesystem magic number (`0x63677270`, ASCII "cgrp").
pub const CGROUP2_SUPER_MAGIC: u64 = 0x6367_7270;

/// Cap on the memory kept free: 128 MiB (upstream main.go:47,
/// `megabyte = 1024 * kilobyte` — binary MiB).
const MAX_RESERVED_BYTES: u64 = 128 * 1024 * 1024;

/// Subtree kind. `Socat` is intentionally absent (upstream ProcessTypeSocat).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum ProcType {
    Pty,
    User,
}

/// cgroup manager contract. Mirrors upstream `Manager` (iface.go); the
/// upstream `Close` is deliberately absent — `&self` close is not re-entrant
/// (a second call double-closes and the fd number may already be reused) and
/// `fd()` after close would hand out a dangling fd. Upstream's close-on-exit
/// semantics are taken over by `Cgroup2Manager`'s `Drop` (daemon shutdown
/// cleanup), so observable behaviour is unchanged. Startup failure fallback =
/// constructing a `NoopManager` instance (not an `Option`), matching upstream
/// `createCgroupManager`'s named-return + defer (plan §0.1).
pub trait Manager: Send + Sync {
    /// Returns the cgroup dir fd for `t`, or `None` when that subtree was not
    /// created (cgroup v2 unavailable → NoopManager). The fd is **borrowed**:
    /// the manager owns it for the daemon lifetime; the spawned child
    /// inherits it via fork and must never close it (plan §3.4).
    fn fd(&self, t: ProcType) -> Option<RawFd>;
}

/// One subtree's location (relative to the cgroup root) and kernel
/// attributes. Mirrors upstream `Cgroup2Config` + `WithCgroup2ProcessType`.
pub struct CgroupConfig {
    pub path: String,
    pub properties: HashMap<String, String>,
}

/// KiB→bytes of `/proc/meminfo` MemTotal minus `min(total/8, 128 MiB)`
/// (upstream main.go:259-262). `memory.high` equals the returned value
/// (main.go:262 `memoryHigh := memoryMax`), so one value covers both.
pub fn compute_limits(mem_total_kib: u64) -> u64 {
    let total_bytes = mem_total_kib * 1024;
    let reserved = (total_bytes / 8).min(MAX_RESERVED_BYTES);
    total_bytes - reserved
}

/// statfs `f_type` == cgroup v2 magic? (cgroup2.go:58-64 probe: on cgroup v1
/// `/sys/fs/cgroup` is a tmpfs where mkdir would "succeed" but hand out fds
/// the kernel rejects with EBADF on clone3(CLONE_INTO_CGROUP).)
pub fn check_magic(f_type: u64) -> bool {
    f_type == CGROUP2_SUPER_MAGIC
}

/// Parse `MemTotal` (KiB) out of a `/proc/meminfo`-shaped file. Split out so
/// the parser is testable against a temp file instead of the live system.
pub(crate) fn read_mem_total_kib(path: &Path) -> std::io::Result<u64> {
    let text = std::fs::read_to_string(path)?;
    for line in text.lines() {
        let mut words = line.split_whitespace();
        if words.next() == Some("MemTotal:") {
            if let Some(kib) = words.next().and_then(|v| v.parse::<u64>().ok()) {
                return Ok(kib);
            }
        }
    }
    Err(std::io::Error::new(
        std::io::ErrorKind::InvalidData,
        format!("no MemTotal line in {}", path.display()),
    ))
}

/// Startup wiring, 1:1 with upstream `createCgroupManager` (main.go:244-293):
/// read total memory → compute limits → try `Cgroup2Manager::new` → on any
/// failure log the reason and fall back to `NoopManager`. All-or-nothing:
/// a partially built subtree set is dropped inside `new` and never kept.
/// `main.rs` calls this exactly once; the choice is then fixed.
pub fn init() -> Arc<dyn Manager> {
    let mem_total_kib = match read_mem_total_kib(Path::new("/proc/meminfo")) {
        Ok(kib) => kib,
        Err(e) => {
            tracing::warn!("cgroup: failed to calculate host metrics: {e}");
            tracing::warn!("cgroup: falling back to no-op cgroup manager");
            return Arc::new(NoopManager);
        }
    };

    let memory_max = compute_limits(mem_total_kib);
    let types = [
        (ProcType::User, subtree("user", memory_max, "50")),
        // Real-time-ish processes: much preferred CPU, still memory-capped.
        (ProcType::Pty, subtree("ptys", memory_max, "200")),
    ];

    match Cgroup2Manager::new(Path::new("/sys/fs/cgroup"), &types) {
        Ok(mgr) => Arc::new(mgr),
        Err(e) => {
            tracing::warn!("cgroup: failed to create cgroup2 manager: {e}");
            tracing::warn!("cgroup: falling back to no-op cgroup manager");
            Arc::new(NoopManager)
        }
    }
}

/// Attributes of one subtree, mirroring upstream main.go:263-277. The memory
/// limit is applied twice (memory.high + memory.max, same value) exactly like
/// upstream — high is the throttle watermark, max the hard cap.
fn subtree(dir: &str, memory_max: u64, cpu_weight: &str) -> CgroupConfig {
    CgroupConfig {
        path: dir.to_string(),
        properties: HashMap::from([
            ("memory.high".to_string(), memory_max.to_string()),
            ("memory.max".to_string(), memory_max.to_string()),
            ("cpu.weight".to_string(), cpu_weight.to_string()),
        ]),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn compute_limits_keeps_1_8_or_128mib() {
        // 16 GiB: 1/8 (2 GiB) exceeds the 128 MiB cap → reserved is 128 MiB.
        let sixteen_gib_kib = 16 * 1024 * 1024;
        let sixteen_gib_bytes = sixteen_gib_kib * 1024;
        assert_eq!(
            compute_limits(sixteen_gib_kib),
            sixteen_gib_bytes - 128 * 1024 * 1024
        );
        // 512 MiB: 1/8 (64 MiB) is under the cap → reserved is 64 MiB.
        let five_twelve_mib_kib = 512 * 1024;
        let five_twelve_mib_bytes = five_twelve_mib_kib * 1024;
        assert_eq!(
            compute_limits(five_twelve_mib_kib),
            five_twelve_mib_bytes - five_twelve_mib_bytes / 8
        );
    }

    #[test]
    fn check_magic_distinguishes_cgroup2_from_tmpfs() {
        assert!(check_magic(CGROUP2_SUPER_MAGIC));
        // tmpfs magic (0x01021994) — what /sys/fs/cgroup shows under cgroup v1.
        assert!(!check_magic(0x0102_1994));
    }

    #[test]
    fn read_mem_total_parses_kib_and_ignores_trailing_unit() {
        let dir = tempfile::tempdir().unwrap();
        let meminfo = dir.path().join("meminfo");
        std::fs::write(
            &meminfo,
            "MemTotal:       16256848 kB\nMemFree:         1048576 kB\n",
        )
        .unwrap();
        assert_eq!(read_mem_total_kib(&meminfo).unwrap(), 16_256_848);
    }

    #[test]
    fn read_mem_total_errors_when_absent() {
        let dir = tempfile::tempdir().unwrap();
        let meminfo = dir.path().join("meminfo");
        std::fs::write(&meminfo, "MemFree: 1048576 kB\n").unwrap();
        assert!(read_mem_total_kib(&meminfo).is_err());
    }

    #[test]
    fn subtree_attributes_match_upstream_main() {
        // main.go:263-277 — user: memory.high/max + cpu.weight=50; pty keeps
        // the same memory cap with cpu.weight=200.
        let user = subtree("user", 1234, "50");
        assert_eq!(user.properties.get("memory.max").unwrap(), "1234");
        assert_eq!(user.properties.get("memory.high").unwrap(), "1234");
        assert_eq!(user.properties.get("cpu.weight").unwrap(), "50");
        let pty = subtree("ptys", 1234, "200");
        assert_eq!(pty.properties.get("cpu.weight").unwrap(), "200");
        assert_eq!(pty.properties.get("memory.max").unwrap(), "1234");
    }

    #[test]
    fn noop_manager_never_reports_a_fd() {
        assert_eq!(NoopManager.fd(ProcType::User), None);
        assert_eq!(NoopManager.fd(ProcType::Pty), None);
    }
}

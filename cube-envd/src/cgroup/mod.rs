// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! cgroup v2 manager (item 1.8). Mirrors upstream
//! `packages/envd/internal/services/cgroups` (iface.go / cgroup2.go /
//! noop.go) and the startup wiring of upstream `createCgroupManager`
//! (`cmd/envd/main.go:244-293`). Only the `ptys` and `user` subtrees are
//! created — `socat` is dropped because CubeSandbox has no port forwarding.
//!
//! Startup contract: `init()` builds the manager once. On any failure it logs
//! the reason and falls back to a `NoopManager` (never blocks startup). A
//! runtime leaf-allocation or placement failure rejects that command. Later
//! commands retry allocation; existing leaves remain supervised and cleaned up.

mod cgroup2;
mod noop;

use std::collections::HashMap;
use std::fs::File;
use std::io;
use std::os::fd::AsRawFd;
use std::os::unix::io::RawFd;
use std::path::{Path, PathBuf};
use std::sync::Arc;

pub use cgroup2::Cgroup2Manager;
pub use noop::NoopManager;

/// cgroup v2 filesystem magic number (`0x63677270`, ASCII "cgrp").
pub const CGROUP2_SUPER_MAGIC: u64 = 0x6367_7270;

/// Cap on the memory kept free: 128 MiB (upstream main.go:47,
/// `megabyte = 1024 * kilobyte` — binary MiB).
const MAX_RESERVED_BYTES: u64 = 128 * 1024 * 1024;
const MEMORY_MAX_ENV: &str = "CUBE_ENVD_CGROUP_MEMORY_MAX_BYTES";
const CGROUP_ROOT_ENV: &str = "CUBE_ENVD_CGROUP_ROOT";

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
    #[cfg(test)]
    fn fd(&self, t: ProcType) -> Option<RawFd>;

    /// Create one leaf cgroup for a command. Only the startup no-op manager
    /// returns `Ok(None)`; runtime errors must not bypass placement.
    fn create_process(&self, _t: ProcType) -> io::Result<Option<Arc<ProcessCgroup>>> {
        Ok(None)
    }
}

/// Per-command leaf beneath `user/` or `ptys/`. Keeping commands in distinct
/// leaves makes `cgroup.kill` escape-proof across `setsid()` and gives each
/// command an unambiguous cleanup boundary.
#[derive(Debug)]
pub struct ProcessCgroup {
    dir: PathBuf,
    fd: File,
    /// `memory.events` is cumulative for the cgroup hierarchy. Snapshot the
    /// value before the command is placed here so an earlier OOM in a reused
    /// parent cannot be attributed to this command.
    oom_kill_baseline: u64,
}

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct MemoryEvents {
    pub oom_kill: u64,
}

impl ProcessCgroup {
    pub(crate) fn new(dir: PathBuf, fd: File) -> Self {
        let oom_kill_baseline = read_memory_events(&dir)
            .map(|events| events.oom_kill)
            .unwrap_or(0);
        Self {
            dir,
            fd,
            oom_kill_baseline,
        }
    }

    pub fn fd(&self) -> RawFd {
        self.fd.as_raw_fd()
    }

    pub fn path(&self) -> &Path {
        &self.dir
    }

    /// Snapshot memory.events for this command. Missing/unsupported files are
    /// reported to the caller so an unavailable cgroup never masquerades as a
    /// confirmed non-OOM termination.
    pub fn memory_events(&self) -> io::Result<MemoryEvents> {
        read_memory_events(&self.dir)
    }

    /// Return whether this command's leaf observed a new OOM kill since it
    /// was allocated. The kernel counter is cumulative, so compare against the
    /// allocation-time baseline rather than treating any non-zero value as a
    /// kill of the current command.
    pub fn oom_killed(&self) -> io::Result<bool> {
        Ok(self.memory_events()?.oom_kill > self.oom_kill_baseline)
    }

    /// Atomically SIGKILL every task in the leaf, including descendants that
    /// escaped the original process group with setsid().
    pub fn kill_all(&self) -> io::Result<()> {
        // cgroup.kill is a write-only control file and reports success even
        // for an already-empty cgroup on some kernels. Check membership first
        // so SendSignal preserves the API's not_found result for a process
        // that has exited but whose table entry is waiting for final reap.
        let content = std::fs::read_to_string(self.dir.join("cgroup.procs"))?;
        let pids = parse_process_ids(&content)?;
        if pids.is_empty() {
            return Err(io::Error::from_raw_os_error(libc::ESRCH));
        }
        if let Some(pid) = pids.iter().find(|pid| **pid <= 1) {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("invalid process id {pid} in cgroup.procs"),
            ));
        }
        std::fs::write(self.dir.join("cgroup.kill"), "1")
    }

    /// Send a signal to every task currently listed in this leaf. cgroup v2
    /// only provides the atomic `cgroup.kill` operation for SIGKILL; for
    /// SIGTERM/SIGHUP/etc. the closest escape-resistant equivalent is to
    /// snapshot `cgroup.procs` and signal each member. The caller still uses
    /// the process-group fallback when this operation is unavailable.
    pub fn signal_all(&self, signo: i32) -> io::Result<()> {
        let content = std::fs::read_to_string(self.dir.join("cgroup.procs"))?;
        let pids = parse_process_ids(&content)?;
        if pids.is_empty() {
            return Err(io::Error::from_raw_os_error(libc::ESRCH));
        }

        let mut signaled = false;
        let mut first_error = None;
        for pid in pids {
            // A cgroup file must never contain pid 0/1 for a command leaf;
            // refusing them protects the daemon even if the hierarchy is
            // corrupted or a test fixture is malformed.
            if pid <= 1 {
                if first_error.is_none() {
                    first_error = Some(io::Error::new(
                        io::ErrorKind::InvalidData,
                        format!("invalid process id {pid} in cgroup.procs"),
                    ));
                }
                continue;
            }
            // SAFETY: pid is read from cgroupfs and validated above; libc::kill
            // does not retain any pointer or Rust-owned state.
            let rc = unsafe { libc::kill(pid as libc::pid_t, signo) };
            if rc == 0 {
                signaled = true;
            } else {
                let error = io::Error::last_os_error();
                // Processes can exit between the snapshot and kill. Ignore
                // that race when another member was successfully signalled;
                // preserve ESRCH when all members disappeared so callers keep
                // the existing not_found semantics.
                if error.raw_os_error() != Some(libc::ESRCH) && first_error.is_none() {
                    first_error = Some(error);
                }
            }
        }

        if let Some(error) = first_error {
            return Err(error);
        }
        if signaled {
            Ok(())
        } else {
            Err(io::Error::from_raw_os_error(libc::ESRCH))
        }
    }

    /// Remove an empty leaf. `WouldBlock` covers the cgroupfs EBUSY case so
    /// the async supervisor can retry while task migration/exit settles.
    pub fn remove_if_empty(&self) -> io::Result<()> {
        match std::fs::remove_dir(&self.dir) {
            Ok(()) => Ok(()),
            Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(()),
            Err(e) if matches!(e.raw_os_error(), Some(libc::EBUSY) | Some(libc::ENOTEMPTY)) => {
                Err(io::Error::new(io::ErrorKind::WouldBlock, e))
            }
            Err(e) => Err(e),
        }
    }
}

fn read_memory_events(dir: &Path) -> io::Result<MemoryEvents> {
    let text = std::fs::read_to_string(dir.join("memory.events"))?;
    let mut events = MemoryEvents::default();
    let mut saw_oom_kill = false;
    for line in text.lines() {
        let mut fields = line.split_whitespace();
        let Some(name) = fields.next() else { continue };
        let Some(value) = fields.next() else { continue };
        if name == "oom_kill" {
            saw_oom_kill = true;
            events.oom_kill = value.parse::<u64>().map_err(|e| {
                io::Error::new(
                    io::ErrorKind::InvalidData,
                    format!("invalid oom_kill count: {e}"),
                )
            })?;
        }
    }
    if !saw_oom_kill {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            format!(
                "{} has no oom_kill counter",
                dir.join("memory.events").display()
            ),
        ));
    }
    Ok(events)
}

pub(crate) fn parse_process_ids(content: &str) -> io::Result<Vec<u32>> {
    let mut pids = Vec::new();
    for token in content.split_whitespace() {
        let pid = token.parse::<u32>().map_err(|e| {
            io::Error::new(
                io::ErrorKind::InvalidData,
                format!("invalid process id {token:?} in cgroup.procs: {e}"),
            )
        })?;
        if !pids.contains(&pid) {
            pids.push(pid);
        }
    }
    Ok(pids)
}

impl Drop for ProcessCgroup {
    fn drop(&mut self) {
        // Supervisors normally perform this explicitly and retry EBUSY. The
        // drop fallback protects daemon shutdown and task cancellation from
        // leaving a live leaf behind; cgroup.kill is scoped to this unique
        // command directory, never the parent user/ptys subtree.
        let _ = self.kill_all();
        let _ = self.remove_if_empty();
    }
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
#[cfg(test)]
pub fn compute_limits(mem_total_kib: u64) -> u64 {
    compute_limit_bytes(mem_total_kib.saturating_mul(1024))
}

fn compute_limit_bytes(total_bytes: u64) -> u64 {
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
    let configured = match std::env::var(MEMORY_MAX_ENV) {
        Ok(raw) => match raw.parse::<u64>() {
            Ok(0) | Err(_) => {
                tracing::warn!(
                    "cgroup: ignoring invalid {MEMORY_MAX_ENV}={raw:?}; expected a positive byte count"
                );
                None
            }
            Ok(bytes) => Some(bytes),
        },
        Err(std::env::VarError::NotPresent) => None,
        Err(e) => {
            tracing::warn!("cgroup: cannot read {MEMORY_MAX_ENV}: {e}");
            None
        }
    };
    let root = std::env::var_os(CGROUP_ROOT_ENV)
        .filter(|value| !value.as_os_str().is_empty())
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from("/sys/fs/cgroup"));
    init_at(&root, Path::new("/proc/meminfo"), configured)
}

fn init_at(root: &Path, meminfo: &Path, configured: Option<u64>) -> Arc<dyn Manager> {
    let mem_total_kib = match read_mem_total_kib(meminfo) {
        Ok(kib) => kib,
        Err(e) => {
            tracing::warn!(
                "cgroup: mode=noop reason=failed to calculate guest memory metrics: {e}"
            );
            return Arc::new(NoopManager);
        }
    };

    let guest_bytes = mem_total_kib.saturating_mul(1024);
    // Never create `user`/`ptys` beside the daemon's actual cgroup when the
    // configured mount exposes a nested path. Doing so could place children
    // outside the limit that governs envd. The effective root is normally the
    // configured root in a private cgroup namespace (where `/proc/self/cgroup`
    // reports `/`), and becomes the nested directory in a shared hierarchy.
    let manager_root = resolve_cgroup_root(root);
    let parent_limit = match read_enclosing_memory_max(root) {
        Ok(limit) => limit,
        Err(e) => {
            tracing::warn!("cgroup: failed to read parent memory.max: {e}");
            None
        }
    };
    let memory_max = derive_memory_limit(guest_bytes, parent_limit, configured);
    let safe_default =
        compute_limit_bytes(parent_limit.map_or(guest_bytes, |limit| guest_bytes.min(limit)));
    if configured.is_some_and(|requested| requested > safe_default) {
        tracing::warn!(
            "cgroup: configured memory.max exceeds the safe guest/parent limit; clamping to {safe_default}"
        );
    }
    let types = [
        (ProcType::User, subtree("user", memory_max, "50")),
        // Real-time-ish processes: much preferred CPU, still memory-capped.
        (ProcType::Pty, subtree("ptys", memory_max, "200")),
    ];

    match Cgroup2Manager::new(&manager_root, &types) {
        Ok(mgr) => {
            tracing::info!(
                "cgroup: mode=enabled v2 user/ptys subtrees under {} (memory.high=max={memory_max}, guest_bytes={guest_bytes}, parent_limit={parent_limit:?})",
                manager_root.display()
            );
            Arc::new(mgr)
        }
        Err(e) => {
            tracing::warn!("cgroup: mode=noop reason=failed to create cgroup2 manager: {e}");
            Arc::new(NoopManager)
        }
    }
}

fn derive_memory_limit(
    guest_bytes: u64,
    parent_limit: Option<u64>,
    configured: Option<u64>,
) -> u64 {
    let effective_total = parent_limit.map_or(guest_bytes, |limit| guest_bytes.min(limit));
    let safe_default = compute_limit_bytes(effective_total);
    configured.map_or(safe_default, |requested| requested.min(safe_default))
}

/// Read this daemon's enclosing cgroup hard limit. The v2 root uses `max`
/// when unconstrained; a numeric value is an additional ceiling below guest
/// physical memory and must win when deriving child limits.
///
/// A daemon can be started below a nested cgroup (for example a systemd
/// scope). In that case `/sys/fs/cgroup/memory.max` describes the host/root
/// hierarchy, not the limit actually inherited by envd. Resolve the unified
/// path reported by `/proc/self/cgroup` relative to the configured mount and
/// prefer that file when it exists. If the mount is a private cgroup
/// namespace, the reported path is usually `/`, so this naturally falls back
/// to the configured root.
fn read_enclosing_memory_max(root: &Path) -> io::Result<Option<u64>> {
    let effective_root = resolve_cgroup_root(root);
    read_memory_max(&effective_root)
}

fn resolve_cgroup_root(root: &Path) -> PathBuf {
    resolve_cgroup_root_from(root, Path::new("/proc/self/cgroup"))
}

fn resolve_cgroup_root_from(root: &Path, proc_cgroup: &Path) -> PathBuf {
    let relative = match read_unified_cgroup_path(proc_cgroup) {
        Ok(relative) => relative,
        Err(error) => {
            tracing::warn!(
                "cgroup: cannot resolve envd's unified cgroup path; using configured root limit: {error}"
            );
            PathBuf::new()
        }
    };
    let candidate = root.join(&relative);
    if candidate != root && candidate.is_dir() {
        candidate
    } else {
        root.to_path_buf()
    }
}

/// Parse the unified (`0::/path`) entry from a `/proc/<pid>/cgroup` file.
/// The returned path is relative to the cgroup v2 mount root. Rejecting `.`
/// and `..` components prevents a malformed proc fixture from escaping the
/// configured mount when it is joined by `read_enclosing_memory_max`.
fn read_unified_cgroup_path(path: &Path) -> io::Result<PathBuf> {
    let content = std::fs::read_to_string(path)?;
    for line in content.lines() {
        let Some(raw) = line.strip_prefix("0::") else {
            continue;
        };
        let raw = raw.trim();
        let raw = raw.strip_prefix('/').unwrap_or(raw);
        let relative = Path::new(raw);
        if relative.components().any(|component| {
            matches!(
                component,
                std::path::Component::CurDir
                    | std::path::Component::ParentDir
                    | std::path::Component::RootDir
            )
        }) {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("invalid unified cgroup path {raw:?}"),
            ));
        }
        return Ok(relative.to_path_buf());
    }
    Err(io::Error::new(
        io::ErrorKind::InvalidData,
        format!("no unified cgroup entry in {}", path.display()),
    ))
}

/// Read a cgroup directory's hard limit. The v2 root uses `max`
/// when unconstrained; a numeric value is an additional ceiling below guest
/// physical memory and must win when deriving child limits.
fn read_memory_max(root: &Path) -> io::Result<Option<u64>> {
    let raw = match std::fs::read_to_string(root.join("memory.max")) {
        Ok(raw) => raw,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(e),
    };
    let value = raw.trim();
    if value == "max" {
        return Ok(None);
    }
    value.parse::<u64>().map(Some).map_err(|e| {
        io::Error::new(
            io::ErrorKind::InvalidData,
            format!(
                "invalid {} value {value:?}: {e}",
                root.join("memory.max").display()
            ),
        )
    })
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
    fn read_memory_max_handles_unlimited_numeric_and_invalid_values() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("memory.max");
        std::fs::write(&path, "max\n").unwrap();
        assert_eq!(read_memory_max(dir.path()).unwrap(), None);
        std::fs::write(&path, "1073741824\n").unwrap();
        assert_eq!(read_memory_max(dir.path()).unwrap(), Some(1 << 30));
        std::fs::write(&path, "bogus\n").unwrap();
        assert!(read_memory_max(dir.path()).is_err());
    }

    #[test]
    fn unified_cgroup_path_resolves_nested_relative_path() {
        let dir = tempfile::tempdir().unwrap();
        let proc_cgroup = dir.path().join("cgroup");
        std::fs::write(&proc_cgroup, "12:memory:/legacy\n0::/slice/session.scope\n").unwrap();
        assert_eq!(
            read_unified_cgroup_path(&proc_cgroup).unwrap(),
            PathBuf::from("slice/session.scope")
        );
    }

    #[test]
    fn unified_cgroup_path_rejects_traversal_and_missing_entry() {
        let dir = tempfile::tempdir().unwrap();
        let proc_cgroup = dir.path().join("cgroup");
        std::fs::write(&proc_cgroup, "0::/../outside\n").unwrap();
        assert!(read_unified_cgroup_path(&proc_cgroup).is_err());
        std::fs::write(&proc_cgroup, "1:name=systemd:/user.slice\n").unwrap();
        assert!(read_unified_cgroup_path(&proc_cgroup).is_err());
    }

    #[test]
    fn enclosing_memory_limit_prefers_the_daemons_nested_cgroup() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        std::fs::write(root.join("memory.max"), "1073741824\n").unwrap();
        let nested = root.join("slice/session.scope");
        std::fs::create_dir_all(&nested).unwrap();
        std::fs::write(nested.join("memory.max"), "536870912\n").unwrap();
        let proc_cgroup = root.join("proc.cgroup");
        std::fs::write(&proc_cgroup, "0::/slice/session.scope\n").unwrap();

        let effective = resolve_cgroup_root_from(root, &proc_cgroup);
        assert_eq!(effective, nested);
        assert_eq!(
            read_memory_max(&effective).unwrap(),
            Some(512 * 1024 * 1024)
        );
    }

    #[test]
    fn cgroup_root_falls_back_when_proc_path_is_not_visible_in_mount() {
        let dir = tempfile::tempdir().unwrap();
        let proc_cgroup = dir.path().join("proc.cgroup");
        std::fs::write(&proc_cgroup, "0::/not-mounted-here\n").unwrap();
        assert_eq!(
            resolve_cgroup_root_from(dir.path(), &proc_cgroup),
            dir.path().to_path_buf()
        );
    }

    #[test]
    fn kill_all_reports_empty_leaf_as_not_found() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("cgroup.procs"), "").unwrap();
        std::fs::write(dir.path().join("cgroup.kill"), "").unwrap();
        let leaf = ProcessCgroup::new(dir.path().to_path_buf(), File::open(dir.path()).unwrap());
        let error = leaf.kill_all().unwrap_err();
        assert_eq!(error.raw_os_error(), Some(libc::ESRCH));
    }

    #[test]
    fn compute_limits_saturates_extreme_meminfo() {
        assert_eq!(compute_limits(u64::MAX), u64::MAX - 128 * 1024 * 1024);
    }

    #[test]
    fn memory_limit_uses_parent_ceiling_and_conservative_override() {
        let guest = 8 * 1024 * 1024 * 1024u64;
        let parent = 2 * 1024 * 1024 * 1024u64;
        assert_eq!(
            derive_memory_limit(guest, Some(parent), None),
            compute_limit_bytes(parent)
        );
        assert_eq!(
            derive_memory_limit(guest, Some(parent), Some(256 * 1024 * 1024)),
            256 * 1024 * 1024
        );
        assert_eq!(
            derive_memory_limit(guest, Some(parent), Some(guest)),
            compute_limit_bytes(parent)
        );
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

    #[test]
    fn parse_process_ids_deduplicates_and_rejects_bad_entries() {
        assert_eq!(
            super::parse_process_ids("12\n12 34\n").unwrap(),
            vec![12, 34]
        );
        let err = super::parse_process_ids("12 nope\n").unwrap_err();
        assert_eq!(err.kind(), std::io::ErrorKind::InvalidData);
    }

    #[test]
    fn signal_all_handles_a_live_process_snapshot() {
        // SIGCONT is harmless for the current test process and exercises the
        // same libc path used for TERM/HUP without killing the test runner.
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(
            dir.path().join("cgroup.procs"),
            format!("{}\n{}\n", std::process::id(), std::process::id()),
        )
        .unwrap();
        let fd = std::fs::File::open(dir.path()).unwrap();
        let cgroup = ProcessCgroup::new(dir.path().to_path_buf(), fd);
        assert!(cgroup.signal_all(libc::SIGCONT).is_ok());
    }

    #[test]
    fn memory_events_oom_delta_is_scoped_to_leaf_baseline() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("memory.events"), "oom 0\noom_kill 4\n").unwrap();
        let fd = std::fs::File::open(dir.path()).unwrap();
        let cgroup = ProcessCgroup::new(dir.path().to_path_buf(), fd);
        assert!(!cgroup.oom_killed().unwrap());
        std::fs::write(dir.path().join("memory.events"), "oom 1\noom_kill 5\n").unwrap();
        assert!(cgroup.oom_killed().unwrap());
    }
}

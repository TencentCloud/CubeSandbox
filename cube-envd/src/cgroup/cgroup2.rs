// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! `Cgroup2Manager` — creates the per-type subtrees under the cgroup v2 root
//! and hands out their dir fds. Mirrors upstream `cgroup2.go`.

use std::collections::{HashMap, HashSet};
use std::ffi::CString;
use std::fs::File;
use std::io;
use std::os::unix::ffi::OsStrExt;
use std::os::unix::io::{IntoRawFd, RawFd};
use std::path::{Path, PathBuf};

use super::{check_magic, CgroupConfig, Manager, ProcType};

#[derive(Debug)]
pub struct Cgroup2Manager {
    fds: HashMap<ProcType, RawFd>,
}

impl Cgroup2Manager {
    /// Verify `root` is a cgroup v2 filesystem, enable the `memory`/`cpu`
    /// controllers on it, then create every subtree in `types`. All-or-
    /// nothing (upstream `createCgroups`, cgroup2.go): when one subtree fails
    /// the fds already built are closed by `Drop` and the whole construction
    /// returns Err — no partial subtrees survive. Directories are **not**
    /// removed on rollback (upstream behaviour).
    ///
    /// Order: statfs probe → subtree_control → mkdir → properties → open.
    pub fn new(root: &Path, types: &[(ProcType, CgroupConfig)]) -> io::Result<Self> {
        let f_type = statfs_type(root)?;
        if !check_magic(f_type) {
            return Err(io::Error::other(format!(
                "cgroup root {} is not a cgroup2 filesystem (type=0x{f_type:x})",
                root.display()
            )));
        }

        // Deviation from upstream (declared, plan §6): upstream never writes
        // cgroup.subtree_control, so in nested/guest deployments where the
        // parent group has controllers disabled the property files below are
        // ENOENT and everything falls back to Noop. Enabling them here is
        // strictly better — EBUSY/EPERM still falls back the same way.
        // The two outer wraps carry the operation and path upstream reports
        // (cgroup2.go:59-68) — this text is all a startup-failure warn has to
        // go on, so a bare errno is not enough to find the culprit path.
        build_subtrees(root, types)
            .map(|fds| Cgroup2Manager { fds })
            .map_err(|e| io::Error::new(e.kind(), format!("failed to create cgroups: {e}")))
    }
}

/// mkdir/property/open loop with the all-or-nothing rollback. Split from
/// `new` so the pure file logic is testable in a temp dir — the statfs probe
/// in `new` would (correctly) reject a non-cgroup2 temp dir first.
fn build_subtrees(
    root: &Path,
    types: &[(ProcType, CgroupConfig)],
) -> io::Result<HashMap<ProcType, RawFd>> {
    enable_subtree_control(root)?;

    let mut fds: HashMap<ProcType, RawFd> = HashMap::new();
    let mut errs: Vec<String> = Vec::new();
    for (t, cfg) in types {
        match create_one_cgroup(root, cfg) {
            Ok(fd) => {
                fds.insert(*t, fd);
            }
            Err(e) => {
                // Keep trying like upstream `createCgroups` (cgroup2.go:80-88):
                // every remaining subtree is still attempted, then the fds
                // built so far are closed and all errors are joined. Lower-case
                // type labels match upstream's string-typed ProcessType
                // (iface.go: pty/user).
                let name = match t {
                    ProcType::Pty => "pty",
                    ProcType::User => "user",
                };
                errs.push(format!("failed to create {name} cgroup: {e}"));
            }
        }
    }
    if !errs.is_empty() {
        for fd in fds.values() {
            // SAFETY: fds we opened ourselves; no other owner exists.
            unsafe {
                libc::close(*fd);
            }
        }
        return Err(io::Error::other(errs.join("; ")));
    }
    Ok(fds)
}

impl Manager for Cgroup2Manager {
    fn fd(&self, t: ProcType) -> Option<RawFd> {
        self.fds.get(&t).copied()
    }
}

/// Daemon-shutdown cleanup. Upstream closes explicitly (`Cgroup2Manager.
/// Close`); here the fd set is RAII — dropping the manager closes every
/// subtree fd exactly once.
impl Drop for Cgroup2Manager {
    fn drop(&mut self) {
        for fd in self.fds.values() {
            // SAFETY: every fd was produced by our own `open` and is not used
            // elsewhere — spawn only *borrows* it during pre_exec (fork
            // inherits, never closes).
            unsafe {
                libc::close(*fd);
            }
        }
    }
}

/// `statfs(root).f_type`, the cgroup v2 probe (upstream cgroup2.go:55-57).
fn statfs_type(root: &Path) -> io::Result<u64> {
    let c_root = c_path(root);
    // SAFETY: `c_root` is NUL-terminated and `st` is written by the kernel.
    let mut st: libc::statfs = unsafe { std::mem::zeroed() };
    let rc = unsafe { libc::statfs(c_root.as_ptr(), &mut st) };
    if rc != 0 {
        let e = io::Error::last_os_error();
        return Err(io::Error::new(
            e.kind(),
            format!("failed to statfs cgroup root {}: {e}", root.display()),
        ));
    }
    Ok(st.f_type as u64)
}

/// Enable `memory` and `cpu` on `root`'s `cgroup.subtree_control` (idempotent:
/// only controllers not already enabled are appended). When the file does not
/// exist — e.g. a temp dir in tests — there is nothing to enable and the step
/// is skipped. Any *other* failure (EBUSY because the group has live children
/// outside this manager, EPERM on an unprivileged nested root) is an error and
/// takes the whole construction down the Noop fallback, matching the upstream
/// "silently write nothing" outcome.
fn enable_subtree_control(root: &Path) -> io::Result<()> {
    let path = root.join("cgroup.subtree_control");
    let content = match std::fs::read_to_string(&path) {
        Ok(c) => c,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(e),
    };

    let enabled: HashSet<&str> = content.split_whitespace().collect();
    let mut missing: Vec<&str> = Vec::new();
    for controller in ["memory", "cpu"] {
        if !enabled.contains(controller) {
            missing.push(controller);
        }
    }
    if missing.is_empty() {
        return Ok(());
    }
    let value = missing
        .iter()
        .map(|c| format!("+{c}"))
        .collect::<Vec<_>>()
        .join(" ");
    std::fs::write(&path, value)
}

/// mkdir the subtree, write its properties, open it read-only. Mirrors
/// upstream `createCgroup` (cgroup2.go:105-121). Property writes keep going
/// after a failure so the error surface mirrors Go's collected errors as
/// closely as a single `io::Error` allows.
///
/// The returned fd has CLOEXEC set: `std::fs::File::open` always opens with
/// O_CLOEXEC on Linux, which upstream does not do (unix.Open O_RDONLY,
/// cgroup2.go:120) — this crate's child never leaks the dir fd past exec.
pub(crate) fn create_one_cgroup(root: &Path, cfg: &CgroupConfig) -> io::Result<RawFd> {
    let full: PathBuf = root.join(&cfg.path);
    std::fs::create_dir_all(&full).map_err(|e| {
        io::Error::new(
            e.kind(),
            format!("failed to create cgroup root {}: {e}", full.display()),
        )
    })?;

    let mut first_err: Option<io::Error> = None;
    for (name, value) in &cfg.properties {
        if let Err(e) = std::fs::write(full.join(name), value) {
            if first_err.is_none() {
                first_err = Some(io::Error::new(
                    e.kind(),
                    format!(
                        "failed to write cgroup property {name} in {}: {e}",
                        full.display()
                    ),
                ));
            }
        }
    }
    if let Some(e) = first_err {
        return Err(e);
    }

    let file = File::open(&full).map_err(|e| {
        io::Error::new(
            e.kind(),
            format!("failed to open cgroup dir {}: {e}", full.display()),
        )
    })?;
    Ok(file.into_raw_fd())
}

fn c_path(path: &Path) -> CString {
    CString::new(path.as_os_str().as_bytes()).expect("cgroup path contains no NUL")
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::cgroup::compute_limits;

    /// Count of this process's open fds whose target lives under `root`.
    /// Unlike a global fd count this is parallel-test safe: other tests' fds
    /// point at their own temp dirs, never at ours, so the rollback/drop
    /// leak assertions below cannot be disturbed by concurrent tests.
    fn open_fds_under(root: &Path) -> usize {
        std::fs::read_dir("/proc/self/fd")
            .unwrap()
            .filter_map(|e| e.ok().and_then(|e| std::fs::read_link(e.path()).ok()))
            .filter(|target| target.starts_with(root))
            .count()
    }

    fn props(items: &[(&str, &str)]) -> HashMap<String, String> {
        items
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect()
    }

    fn sample_types(root_mem_kib: u64) -> Vec<(ProcType, CgroupConfig)> {
        let max = compute_limits(root_mem_kib).to_string();
        vec![
            (
                ProcType::User,
                CgroupConfig {
                    path: "user".to_string(),
                    properties: props(&[
                        ("memory.high", &max),
                        ("memory.max", &max),
                        ("cpu.weight", "50"),
                    ]),
                },
            ),
            (
                ProcType::Pty,
                CgroupConfig {
                    path: "ptys".to_string(),
                    properties: props(&[
                        ("memory.high", &max),
                        ("memory.max", &max),
                        ("cpu.weight", "200"),
                    ]),
                },
            ),
        ]
    }

    /// Build a manager in a temp dir, skipping the statfs probe (a temp dir
    /// is not a cgroup2 filesystem — `new` is exercised separately below).
    fn manager_in(dir: &Path) -> Cgroup2Manager {
        let fds = build_subtrees(dir, &sample_types(16 * 1024 * 1024)).unwrap();
        Cgroup2Manager { fds }
    }

    #[test]
    fn new_creates_subtrees_with_matching_attributes() {
        let dir = tempfile::tempdir().unwrap();
        let max = compute_limits(16 * 1024 * 1024);
        let mgr = manager_in(dir.path());

        let user_fd = mgr.fd(ProcType::User).expect("user fd");
        let pty_fd = mgr.fd(ProcType::Pty).expect("pty fd");
        assert_ne!(user_fd, pty_fd);

        // Attributes landed, values mirror upstream main.go:263-277.
        let user_max = std::fs::read_to_string(dir.path().join("user/memory.max")).unwrap();
        assert_eq!(user_max.trim(), max.to_string());
        assert_eq!(
            std::fs::read_to_string(dir.path().join("user/cpu.weight"))
                .unwrap()
                .trim(),
            "50"
        );
        assert_eq!(
            std::fs::read_to_string(dir.path().join("ptys/cpu.weight"))
                .unwrap()
                .trim(),
            "200"
        );
        assert_eq!(mgr.fd(ProcType::Pty), Some(pty_fd));

        // Both subtree fds are owned by the manager while alive and closed
        // by Drop (RAII replaces upstream Close).
        assert_eq!(open_fds_under(dir.path()), 2);
        drop(mgr);
        assert_eq!(open_fds_under(dir.path()), 0);
    }

    #[test]
    fn fd_is_close_on_exec() {
        // CLOEXEC means the dir fd never leaks into user code past exec
        // (plan §6; upstream's O_RDONLY open without O_CLOEXEC leaks it).
        let dir = tempfile::tempdir().unwrap();
        let mgr = manager_in(dir.path());
        let fd = mgr.fd(ProcType::User).unwrap();
        let flags = unsafe { libc::fcntl(fd, libc::F_GETFD) };
        assert!(flags >= 0 && flags & libc::FD_CLOEXEC != 0);
    }

    #[test]
    fn rejects_non_cgroup2_root() {
        // tmpfs magic (what /sys/fs/cgroup shows under cgroup v1) must fail
        // the probe instead of "succeeding" with fds the kernel would reject
        // on clone3(CLONE_INTO_CGROUP).
        let dir = tempfile::tempdir().unwrap();
        let err = Cgroup2Manager::new(dir.path(), &sample_types(16 * 1024 * 1024)).unwrap_err();
        assert!(err.to_string().contains("not a cgroup2 filesystem"));
    }

    #[test]
    fn all_or_nothing_rollback_closes_built_fds() {
        let dir = tempfile::tempdir().unwrap();
        // Pre-create a *file* where the second subtree ("ptys") must be a
        // directory: create_dir_all fails → whole construction must fail and
        // the fd built for "user" must already be closed (no leak).
        std::fs::write(dir.path().join("ptys"), "in the way").unwrap();

        let before = open_fds_under(dir.path());
        let err = build_subtrees(dir.path(), &sample_types(16 * 1024 * 1024)).unwrap_err();
        assert!(err.to_string().contains("failed to create pty cgroup"));
        assert_eq!(
            open_fds_under(dir.path()),
            before,
            "partial fds must be closed on rollback"
        );

        // The successful subtree dir remains (upstream never deletes dirs on
        // rollback) but is not owned by any manager anymore.
        assert!(dir.path().join("user").is_dir());
    }

    #[test]
    fn enables_missing_controllers_idempotently() {
        let dir = tempfile::tempdir().unwrap();
        let ctl = dir.path().join("cgroup.subtree_control");
        std::fs::write(&ctl, "").unwrap();

        // First construction: both controllers missing → appended.
        let mgr = manager_in(dir.path());
        assert_eq!(
            std::fs::read_to_string(&ctl).unwrap().trim(),
            "+memory +cpu"
        );
        drop(mgr);

        // Second construction: already enabled → untouched, no duplicate.
        let mgr = manager_in(dir.path());
        assert_eq!(
            std::fs::read_to_string(&ctl).unwrap().trim(),
            "+memory +cpu"
        );
        drop(mgr);

        // A controller list missing only one is completed without duplicating
        // the other.
        std::fs::write(&ctl, "cpu").unwrap();
        let mgr = manager_in(dir.path());
        assert_eq!(std::fs::read_to_string(&ctl).unwrap().trim(), "+memory");
        drop(mgr);
    }

    #[test]
    fn missing_subtree_control_file_is_skipped() {
        // Temp dirs have no cgroup.subtree_control at all — the enable step
        // must be skipped, not fatal, so the pure file logic stays testable.
        let dir = tempfile::tempdir().unwrap();
        let mgr = manager_in(dir.path());
        assert!(mgr.fd(ProcType::User).is_some());
    }
}

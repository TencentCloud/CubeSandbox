// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Filesystem watch family — streaming `WatchDir` plus the pull-watcher trio
//! (CreateWatcher / GetWatcherEvents / RemoveWatcher).
//!
//! Ported from the envd 0.5.13 baseline. Every behavior below was verified
//! against the reference sources (paths cited inline); two of them are
//! non-obvious and easy to "fix" into a conformance failure:
//!
//! - **`IN_MOVED_TO` maps to Create, not Rename** (`fsnotify`
//!   `backend_inotify.go:568-596`): a rename inside the watched tree emits
//!   RENAME(old) followed by CREATE(new). Mapping both sides to Rename drifts.
//! - **"not a directory" carries the literal `%!w(<nil>)`**: upstream wraps a
//!   nil error into `%w` there (`watch.go:42-43`, `watch_sync.go:163-165`);
//!   go1.26 renders `fmt.Errorf("path %s not a directory: %w", p, nil)` as
//!   `path %s not a directory: %!w(<nil>)` (verified empirically).
//!
//! Deliberate deviations from upstream (documented; both are upstream
//! defects we choose not to reproduce):
//! - pull watchers cap their event buffer — upstream accumulates without
//!   bound (`watch_sync.go:107`), so a client that never polls grows the
//!   daemon's memory forever. Ours surfaces an error through
//!   GetWatcherEvents (the same channel upstream uses for watcher errors)
//!   instead of silently dropping or silently growing.
//! - the keepalive cadence defaults to the process-stream value (30s, see
//!   `connect::DEFAULT_KEEPALIVE_INTERVAL`) rather than the filesystem
//!   watch's 90s (`permissions/keepalive.go:10`) — same LB-idle-timeout
//!   rationale already recorded for the Start stream. The
//!   `Keepalive-Ping-Interval` header overrides it identically.
//!
//! Recursion is implemented by hand (inotify watches single directories):
//! the semantics are a faithful port of the fsnotify recursive backend
//! (`backend_inotify.go:230-263` for setup, `:495-563` for dynamic
//! directories), including the synthetic Create events for `mkdir -p`
//! subtrees and the cookie-paired path rewrite for directory renames.

use std::collections::HashMap;
use std::io::ErrorKind;
use std::os::unix::ffi::OsStringExt;
use std::os::unix::io::{AsRawFd, RawFd};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use tokio::io::unix::AsyncFd;
use tokio_stream::wrappers::ReceiverStream;

use crate::auth::{resolve_path, User};
use crate::connect;
use crate::error::{ConnectCode, ConnectError};
use crate::go_compat::errno::{errno_text, go_path_error};
use crate::msg::filesystem::{
    CreateWatcherRequest, CreateWatcherResponse, EventType, FilesystemEvent,
    GetWatcherEventsRequest, GetWatcherEventsResponse, RemoveWatcherRequest, RemoveWatcherResponse,
    StartEvent, WatchDirRequest, WatchDirResponse,
};

/// The exact mask fsnotify requests for its default op set
/// Create|Write|Remove|Rename|Chmod (`fsnotify.go:424-426` expanding through
/// `backend_inotify.go:195-223`): nothing more, nothing less — no IN_ONLYDIR,
/// no IN_MASK_ADD.
const WATCH_MASK: u32 = libc::IN_CREATE
    | libc::IN_MODIFY
    | libc::IN_DELETE
    | libc::IN_DELETE_SELF
    | libc::IN_MOVED_TO
    | libc::IN_MOVED_FROM
    | libc::IN_MOVE_SELF
    | libc::IN_ATTRIB;

/// fsnotify's default read buffer (`fsnotify.go:443-446`: 64K, "the highest
/// value that works on all filesystems").
const READ_BUF: usize = 64 * 1024;

/// Cap for a pull watcher's buffered events (see module docs: deliberate
/// deviation from upstream's unbounded accumulation).
const MAX_BUFFERED_EVENTS: usize = 10_000;

// ---------- network mount check (utils.go:19-38) ----------

/// Filesystem magic numbers from the kernel (include/uapi/linux/magic.h),
/// copied from upstream `utils.go:19-24`. Typed `u64` and compared through a
/// cast because `statfs.f_type` is `i64` on glibc but `u64` on musl.
const NFS_SUPER_MAGIC: u64 = 0x6969;
const CIFS_MAGIC: u64 = 0xFF534D42;
const SMB_MAGIC: u64 = 0x517B;
const SMB2_MAGIC: u64 = 0xFE534D42;
const FUSE_SUPER_MAGIC: u64 = 0x65735546;

/// Upstream `IsPathOnNetworkMount`: true on NFS/CIFS/SMB/SMB2/FUSE. The error
/// text double-wraps exactly like the Go chain (`utils.go:34` inside
/// `watch.go:49`), verified against go1.26.
fn is_network_mount(path: &Path) -> Result<bool, ConnectError> {
    let c = cstring(path)?;
    // SAFETY: `c` outlives the call; `st` is a valid, zeroed statfs.
    let mut st: libc::statfs = unsafe { std::mem::zeroed() };
    if unsafe { libc::statfs(c.as_ptr(), &mut st) } != 0 {
        let e = std::io::Error::last_os_error();
        return Err(ConnectError::new(
            ConnectCode::Internal,
            format!(
                "error checking mount status: failed to statfs {}: {}",
                path.display(),
                go_path_error("statfs", &path.to_string_lossy(), &e)
            ),
        ));
    }
    Ok(matches!(
        u64::from_ne_bytes(st.f_type.to_ne_bytes()),
        NFS_SUPER_MAGIC | CIFS_MAGIC | SMB_MAGIC | SMB2_MAGIC | FUSE_SUPER_MAGIC
    ))
}

fn cstring(path: &Path) -> Result<std::ffi::CString, ConnectError> {
    std::ffi::CString::new(path.as_os_str().as_encoded_bytes()).map_err(|_| {
        ConnectError::new(
            ConnectCode::Internal,
            format!("path contains NUL: {}", path.display()),
        )
    })
}

// ---------- inotify (raw libc — no inotify crate, see plan) ----------

struct Inotify {
    fd: RawFd,
}

impl Inotify {
    /// `inotify_init1(IN_NONBLOCK|IN_CLOEXEC)`: non-blocking so the pump can
    /// select between readability, keepalive ticks and client disconnect
    /// (upstream relies on `ctx.Done()` responsiveness, `watch.go:89-90`).
    fn new() -> Result<Self, ConnectError> {
        // SAFETY: plain syscall, no pointers.
        let fd = unsafe { libc::inotify_init1(libc::IN_NONBLOCK | libc::IN_CLOEXEC) };
        if fd < 0 {
            let e = std::io::Error::last_os_error();
            return Err(ConnectError::new(
                ConnectCode::Internal,
                format!("error creating watcher: inotify_init1: {}", errno_text(&e)),
            ));
        }
        Ok(Self { fd })
    }

    fn read(&self, buf: &mut [u8]) -> std::io::Result<usize> {
        // SAFETY: buf is valid for writes of buf.len().
        let n = unsafe { libc::read(self.fd, buf.as_mut_ptr().cast(), buf.len()) };
        if n < 0 {
            Err(std::io::Error::last_os_error())
        } else {
            Ok(n as usize)
        }
    }
}

impl AsRawFd for Inotify {
    fn as_raw_fd(&self) -> RawFd {
        self.fd
    }
}

impl Drop for Inotify {
    fn drop(&mut self) {
        // SAFETY: fd is owned.
        unsafe { libc::close(self.fd) };
    }
}

/// `inotify_add_watch` on a raw fd (free function so `WatchState` can add
/// watches while the `Inotify` itself lives inside an `AsyncFd`).
fn add_watch_raw(fd: RawFd, path: &Path) -> std::io::Result<i32> {
    let c = std::ffi::CString::new(path.as_os_str().as_encoded_bytes())?;
    // SAFETY: c outlives the call; fd is owned.
    let wd = unsafe { libc::inotify_add_watch(fd, c.as_ptr(), WATCH_MASK) };
    if wd < 0 {
        Err(std::io::Error::last_os_error())
    } else {
        Ok(wd)
    }
}

fn rm_watch_raw(fd: RawFd, wd: i32) {
    // SAFETY: fd is owned; wd was returned by add_watch on it.
    unsafe { libc::inotify_rm_watch(fd, wd) };
}

/// One parsed `inotify_event`. `name` is None for events on the watched
/// directory itself (kernel sends no name).
struct RawEvent {
    wd: i32,
    mask: u32,
    cookie: u32,
    name: Option<String>,
}

/// Parse a read() buffer into events. The kernel only ever returns whole
/// events per read, but headers are 4-byte aligned while names vary in
/// length, so every header read must be unaligned-safe.
fn parse_events(buf: &[u8]) -> Vec<RawEvent> {
    const HDR: usize = std::mem::size_of::<libc::inotify_event>();
    let mut out = Vec::new();
    let mut off = 0usize;
    while off + HDR <= buf.len() {
        // SAFETY: off+HDR <= buf.len(); unaligned by design (see above).
        let ev = unsafe {
            std::ptr::read_unaligned(buf.as_ptr().add(off) as *const libc::inotify_event)
        };
        let name_len = ev.len as usize;
        let name = if name_len > 0 && off + HDR + name_len <= buf.len() {
            let bytes = &buf[off + HDR..off + HDR + name_len];
            let end = bytes.iter().position(|&b| b == 0).unwrap_or(bytes.len());
            let s = String::from_utf8_lossy(&bytes[..end]).into_owned();
            (!s.is_empty()).then_some(s)
        } else {
            None
        };
        out.push(RawEvent {
            wd: ev.wd,
            mask: ev.mask,
            cookie: ev.cookie,
            name,
        });
        // Advance by header + name. A zero name_len is a NORMAL event (the
        // watched directory itself: IN_ATTRIB / IN_DELETE_SELF / ...) — the
        // only stop condition is a truncated tail, which the kernel never
        // produces for a single read (it returns whole events only).
        let total = HDR + name_len;
        if off + total > buf.len() {
            break;
        }
        off += total;
    }
    out
}

// ---------- op expansion (watch.go:105-123 + fsnotify newEvent) ----------

/// envd expands one kernel event into its ops in the fixed order
/// Create → Rename → Chmod → Write → Remove (`watch.go:105-123`); the
/// predicate for each op comes from fsnotify's `newEvent`
/// (`backend_inotify.go:568-596`). Note `IN_MOVED_TO` is a **Create**.
fn expand_ops(mask: u32) -> Vec<EventType> {
    let mut ops = Vec::with_capacity(2);
    if mask & (libc::IN_CREATE | libc::IN_MOVED_TO) != 0 {
        ops.push(EventType::Create);
    }
    if mask & (libc::IN_MOVED_FROM | libc::IN_MOVE_SELF) != 0 {
        ops.push(EventType::Rename);
    }
    if mask & libc::IN_ATTRIB != 0 {
        ops.push(EventType::Chmod);
    }
    if mask & libc::IN_MODIFY != 0 {
        ops.push(EventType::Write);
    }
    if mask & (libc::IN_DELETE | libc::IN_DELETE_SELF) != 0 {
        ops.push(EventType::Remove);
    }
    ops
}

/// Mirror of Go `filepath.Rel` for the paths envd feeds it (both absolute,
/// clean). Includes the exact error text, which envd surfaces verbatim via
/// `error getting relative path: %w` (`watch.go:128`). Go's Rel does NOT
/// require containment — it emits `..` segments — so this must too.
fn go_rel(targ: &Path, base: &Path) -> Result<String, String> {
    fn comps(p: &Path) -> Vec<String> {
        p.components()
            .filter_map(|c| match c {
                std::path::Component::Normal(s) => {
                    Some(String::from_utf8_lossy(s.as_encoded_bytes()).into_owned())
                }
                _ => None,
            })
            .collect()
    }
    if targ.is_absolute() != base.is_absolute() {
        return Err(format!(
            "Rel: can't make {} relative to {}",
            targ.display(),
            base.display()
        ));
    }
    let t = comps(targ);
    let b = comps(base);
    let mut i = 0;
    while i < t.len() && i < b.len() && t[i] == b[i] {
        i += 1;
    }
    let mut parts: Vec<String> = Vec::with_capacity(b.len() - i + t.len() - i);
    for _ in i..b.len() {
        parts.push("..".into());
    }
    for c in &t[i..] {
        parts.push(c.clone());
    }
    Ok(if parts.is_empty() {
        ".".to_string()
    } else {
        parts.join("/")
    })
}

fn rel_event(
    full: &Path,
    root: &Path,
    event_type: EventType,
) -> Result<FilesystemEvent, ConnectError> {
    let name = go_rel(full, root).map_err(|e| {
        // watch.go:126-129: the Rel failure kills the whole stream/watcher
        // (`error getting relative path: %w`).
        ConnectError::new(
            ConnectCode::Internal,
            format!("error getting relative path: {e}"),
        )
    })?;
    Ok(FilesystemEvent { name, event_type })
}

// ---------- watch state (dirs map, cookies, recursion) ----------

/// Per-watcher state: which directories are watched (wd → path) plus the
/// recursion bookkeeping. Owns no fd — the fd lives in the `AsyncFd` next to
/// it; `fd` here is a copy used for add/rm watch calls.
struct WatchState {
    fd: RawFd,
    root: PathBuf,
    root_wd: i32,
    recursive: bool,
    dirs: HashMap<i32, PathBuf>,
    /// fsnotify's rename-cookie ring: the last 10 MOVED_FROM cookies with
    /// their paths (`backend_inotify.go:598-606`). A matching MOVED_TO turns
    /// into `renamedFrom`, which drives the child-watch path rewrite.
    cookies: [(u32, PathBuf); 10],
    cookie_next: usize,
}

impl WatchState {
    fn new(root: PathBuf, recursive: bool) -> Result<(Inotify, Self), ConnectError> {
        let ino = Inotify::new()?;
        let fd = ino.as_raw_fd();
        let mut st = Self {
            fd,
            root_wd: -1,
            root: root.clone(),
            recursive,
            dirs: HashMap::new(),
            cookies: std::array::from_fn(|_| (0u32, PathBuf::new())),
            cookie_next: 0,
        };
        st.root_wd = st.register(&root)?;
        if recursive {
            // Initial recursive setup: walk and register every directory.
            // No synthetic events here (fsnotify only emits those when
            // `sendCreate` is requested, which envd's plain Add does not —
            // `backend_inotify.go:247`, `fsnotify.go:424-426`).
            st.walk_register(&root)?;
        }
        Ok((ino, st))
    }

    fn register(&mut self, dir: &Path) -> Result<i32, ConnectError> {
        let wd = add_watch_raw(self.fd, dir).map_err(|e| {
            // ENOSPC here is the inotify watch limit (max_user_watches /
            // max_user_instances): surfaced explicitly, never swallowed — a
            // silently lost watch would drop every later event. Text mirrors
            // envd watch.go:63 around fsnotify's os.NewSyscallError shape.
            ConnectError::new(
                ConnectCode::Internal,
                format!(
                    "error adding path {} to watcher: inotify_add_watch: {}",
                    dir.display(),
                    errno_text(&e)
                ),
            )
        })?;
        self.dirs.insert(wd, dir.to_path_buf());
        Ok(wd)
    }

    /// Initial recursive walk (fsnotify `AddWith` recurse path,
    /// `backend_inotify.go:228-263`): register every directory, lexical DFS,
    /// unreadable entries skipped silently, other errors fatal.
    fn walk_register(&mut self, dir: &Path) -> Result<(), ConnectError> {
        let entries = match std::fs::read_dir(dir) {
            Ok(rd) => rd.collect::<Result<Vec<_>, _>>().map_err(|e| {
                ConnectError::new(
                    ConnectCode::Internal,
                    format!("error adding path {} to watcher: {e}", dir.display()),
                )
            })?,
            Err(e) => {
                return Err(ConnectError::new(
                    ConnectCode::Internal,
                    format!("error adding path {} to watcher: {e}", dir.display()),
                ));
            }
        };
        let mut paths: Vec<PathBuf> = entries.iter().map(|e| e.path()).collect();
        paths.sort();
        for p in paths {
            match p.symlink_metadata() {
                // fsnotify skips unreadable entries silently; WalkDir does not
                // follow symlinks, so symlinked dirs are not registered.
                Err(e) if e.kind() == ErrorKind::PermissionDenied => continue,
                Err(e) => {
                    return Err(ConnectError::new(
                        ConnectCode::Internal,
                        format!("error adding path {} to watcher: {e}", p.display()),
                    ));
                }
                Ok(meta) => {
                    if meta.is_dir() {
                        self.register(&p)?;
                        self.walk_register(&p)?;
                    }
                }
            }
        }
        Ok(())
    }

    fn note_cookie(&mut self, cookie: u32, path: PathBuf) {
        self.cookies[self.cookie_next] = (cookie, path);
        self.cookie_next = (self.cookie_next + 1) % self.cookies.len();
    }

    fn take_cookie(&self, cookie: u32) -> Option<PathBuf> {
        self.cookies
            .iter()
            .find(|(c, _)| *c == cookie)
            .map(|(_, p)| p.clone())
    }

    /// A renamed directory invalidates every child watch's stored path;
    /// fsnotify rewrites them by prefix replacement
    /// (`backend_inotify.go:506-523`). The renamed dir's own entry matches
    /// the `from` prefix and is rewritten too; entries already at the new
    /// path are untouched.
    fn rename_children(&mut self, from: &Path, to: &Path) {
        let from_b = from.as_os_str().as_encoded_bytes();
        let to_b = to.as_os_str().as_encoded_bytes();
        for p in self.dirs.values_mut() {
            let pb = p.as_os_str().as_encoded_bytes();
            if pb.starts_with(from_b) {
                let mut new = to_b.to_vec();
                new.extend_from_slice(&pb[from_b.len()..]);
                *p = PathBuf::from(std::ffi::OsString::from_vec(new));
            }
        }
    }

    /// Dynamic handling of a directory creation inside a recursive watch
    /// (`backend_inotify.go:497-563`): register the new directory, then walk
    /// its tree — every entry except the walk root gets a **synthetic Create
    /// event** (files included), because the kernel only reported the top
    /// directory. This is the `mkdir -p one/two/three` case: without the
    /// synthetic events only "one" would ever be reported.
    fn walk_new_tree(
        &mut self,
        new_dir: &Path,
        out: &mut Vec<FilesystemEvent>,
    ) -> Result<(), ConnectError> {
        self.register(new_dir)?;
        let entries = match std::fs::read_dir(new_dir) {
            Ok(rd) => rd.collect::<Result<Vec<_>, _>>().map_err(|e| {
                ConnectError::new(ConnectCode::Internal, format!("watcher error: {e}"))
            })?,
            Err(e) => {
                return Err(ConnectError::new(
                    ConnectCode::Internal,
                    format!("watcher error: {e}"),
                ));
            }
        };
        let mut paths: Vec<PathBuf> = entries.iter().map(|e| e.path()).collect();
        paths.sort();
        for p in paths {
            // Emit the synthetic Create BEFORE the permission skip: upstream
            // sends the event first, then `Info()` may SkipDir the subtree
            // (backend_inotify.go:536-551).
            out.push(rel_event(&p, &self.root, EventType::Create)?);
            match p.symlink_metadata() {
                Err(e) if e.kind() == ErrorKind::PermissionDenied => continue,
                Err(e) => {
                    return Err(ConnectError::new(
                        ConnectCode::Internal,
                        format!("watcher error: {e}"),
                    ));
                }
                Ok(meta) => {
                    if meta.is_dir() {
                        self.register(&p)?;
                        self.walk_new_tree(&p, out)?;
                    }
                }
            }
        }
        Ok(())
    }

    /// Expand one raw kernel event into zero or more wire events. `Ok(vec![])`
    /// means "consumed, nothing to emit" (fsnotify drops several classes
    /// silently). `Err` kills the stream / poisons the pull watcher, matching
    /// upstream's watcher-error path (`watch.go:91-96`).
    fn handle_raw(&mut self, raw: &RawEvent) -> Result<Vec<FilesystemEvent>, ConnectError> {
        // Queue overflow reaches fsnotify as an error (ErrEventOverflow) and
        // envd turns any watcher error into a fatal one.
        if raw.mask & libc::IN_Q_OVERFLOW != 0 {
            return Err(ConnectError::new(
                ConnectCode::Internal,
                "watcher error: fsnotify queue overflow",
            ));
        }

        // Unknown wd: fsnotify skips silently (`backend_inotify.go:433-436`).
        let Some(dir) = self.dirs.get(&raw.wd).cloned() else {
            return Ok(Vec::new());
        };

        // IN_IGNORED / IN_UNMOUNT: the kernel auto-removed the watch; fsnotify
        // drops it and emits nothing (`backend_inotify.go:454-457`).
        if raw.mask & (libc::IN_IGNORED | libc::IN_UNMOUNT) != 0 {
            self.dirs.remove(&raw.wd);
            return Ok(Vec::new());
        }

        // IN_MOVE_SELF: the watched directory itself was renamed/moved.
        if raw.mask & libc::IN_MOVE_SELF != 0 {
            // A recursion-added child KEEPS its watch and stays silent: the
            // inode moved but an inotify watch follows the inode, so it is
            // still valid at the new location — the parent's MOVED_TO has
            // already (or will) rewrite the stored path via rename_children.
            // Removing it here would silently drop every later event inside
            // the moved directory. fsnotify returns early for recurse-added
            // children too (`backend_inotify.go:467-472`).
            if self.recursive && raw.wd != self.root_wd {
                return Ok(Vec::new());
            }
            // The user-added root (or a non-recursive watch): the move is
            // reported as Rename and the watch state is dropped — the parent
            // directory of `dir` is outside the tree, so nothing re-registers
            // it (`backend_inotify.go:474-484`).
            self.dirs.remove(&raw.wd);
            rm_watch_raw(self.fd, raw.wd);
            return Ok(vec![rel_event(&dir, &self.root, EventType::Rename)?]);
        }

        // IN_DELETE_SELF: clean state. The user-visible Remove is emitted by
        // the parent's IN_DELETE when the parent is watched too
        // (`backend_inotify.go:461-463, 486-493`).
        if raw.mask & libc::IN_DELETE_SELF != 0 {
            self.dirs.remove(&raw.wd);
            let parent_watched = dir
                .parent()
                .is_some_and(|p| self.dirs.values().any(|d| d == p));
            if parent_watched {
                return Ok(Vec::new());
            }
            return Ok(vec![rel_event(&dir, &self.root, EventType::Remove)?]);
        }

        // Full event path: watched dir + entry name (backend_inotify.go:439-448).
        let full = match raw.name.as_deref() {
            Some(n) if !n.is_empty() => dir.join(n),
            _ => dir.clone(),
        };

        // Cookie bookkeeping precedes the recursion handling in fsnotify
        // (`newEvent` runs before the recurse block).
        let renamed_from = if raw.mask & libc::IN_MOVED_FROM != 0 {
            self.note_cookie(raw.cookie, full.clone());
            None
        } else {
            self.take_cookie_for(raw.mask, raw.cookie)
        };

        let mut out: Vec<FilesystemEvent> = expand_ops(raw.mask)
            .into_iter()
            .map(|op| rel_event(&full, &self.root, op))
            .collect::<Result<_, _>>()?;

        // Recursive: a directory appeared (created or moved in) — register it
        // and synthesize Create events for its existing contents.
        if self.recursive
            && is_dir_mask(raw.mask)
            && raw.mask & (libc::IN_CREATE | libc::IN_MOVED_TO) != 0
        {
            match renamed_from {
                // Directory rename: rewrite child watch paths; the MOVED_FROM
                // side already emitted Rename(old) via the expansion above.
                Some(old) => self.rename_children(&old, &full),
                None => self.walk_new_tree(&full, &mut out)?,
            }
        }

        Ok(out)
    }

    fn take_cookie_for(&self, mask: u32, cookie: u32) -> Option<PathBuf> {
        if mask & libc::IN_MOVED_TO != 0 {
            self.take_cookie(cookie)
        } else {
            None
        }
    }
}

fn is_dir_mask(mask: u32) -> bool {
    mask & libc::IN_ISDIR != 0
}

// ---------- event drain (shared by streaming and pull) ----------

/// Read every currently-available event and expand it. Sync: called while the
/// `AsyncFd` guard marks the fd ready; ends on EAGAIN.
fn drain_events(ino: &Inotify, st: &mut WatchState) -> Result<Vec<FilesystemEvent>, ConnectError> {
    let mut buf = vec![0u8; READ_BUF];
    let mut out = Vec::new();
    loop {
        match ino.read(&mut buf) {
            Ok(0) => break,
            Ok(n) => {
                for raw in parse_events(&buf[..n]) {
                    out.extend(st.handle_raw(&raw)?);
                }
            }
            Err(e) if e.kind() == ErrorKind::WouldBlock => break,
            Err(e) => {
                return Err(ConnectError::new(
                    ConnectCode::Internal,
                    format!("watcher error: {}", errno_text(&e)),
                ));
            }
        }
    }
    Ok(out)
}

fn frame_of(resp: &WatchDirResponse) -> bytes::Bytes {
    match serde_json::to_value(resp) {
        Ok(v) => connect::message_frame(&v),
        Err(e) => connect::end_stream_error(&ConnectError::new(
            ConnectCode::Internal,
            format!("serialize response: {e}"),
        )),
    }
}

fn fail_stream(tx: &tokio::sync::mpsc::Sender<bytes::Bytes>, e: &ConnectError) {
    let _ = tx.try_send(connect::end_stream_error(e));
}

fn internal(msg: impl Into<String>) -> ConnectError {
    ConnectError::new(ConnectCode::Internal, msg)
}

// ---------- streaming WatchDir ----------

/// Prechecks + tree construction, shared by WatchDir and CreateWatcher
/// (`watch.go:28-53`, `watch_sync.go:149-174` — identical chains).
fn prepare_watch_target(path: &str, user: &User) -> Result<PathBuf, ConnectError> {
    let p = resolve_path(path, user);
    let meta = std::fs::metadata(&p).map_err(|e| {
        if e.kind() == ErrorKind::NotFound {
            ConnectError::new(
                ConnectCode::NotFound,
                format!("path {p} not found: {}", go_path_error("stat", &p, &e)),
            )
        } else {
            ConnectError::new(
                ConnectCode::Internal,
                format!("error statting path {p}: {}", go_path_error("stat", &p, &e)),
            )
        }
    })?;
    if !meta.is_dir() {
        // upstream wraps a NIL error in %w here; the wire text keeps the
        // literal `%!w(<nil>)` (verified against go1.26 — see module docs).
        return Err(ConnectError::new(
            ConnectCode::InvalidArgument,
            format!("path {p} not a directory: %!w(<nil>)"),
        ));
    }
    if is_network_mount(Path::new(&p))? {
        return Err(ConnectError::new(
            ConnectCode::InvalidArgument,
            format!("cannot watch path on network filesystem: {p}"),
        ));
    }
    Ok(PathBuf::from(p))
}

/// Build (prechecks + inotify + initial walk). Sync — runs inside one
/// blocking-pool crossing; the async pump takes over afterwards.
fn build_stream(req: &WatchDirRequest, user: &User) -> Result<(Inotify, WatchState), ConnectError> {
    let root = prepare_watch_target(&req.path, user)?;
    WatchState::new(root, req.recursive)
}

async fn run_stream(
    ino: Inotify,
    mut st: WatchState,
    keepalive: Duration,
    deadline: Option<Duration>,
    tx: tokio::sync::mpsc::Sender<bytes::Bytes>,
) {
    // Start frame first (watch.go:66-73). A failed send means the client is
    // already gone.
    if tx
        .send(frame_of(&WatchDirResponse::Start(StartEvent {})))
        .await
        .is_err()
    {
        return;
    }
    let afd = match AsyncFd::new(ino) {
        Ok(a) => a,
        Err(e) => {
            fail_stream(&tx, &internal(format!("watcher error: {e}")));
            return;
        }
    };
    // First tick after the interval, like time.NewTicker (not immediately).
    let mut interval = tokio::time::interval_at(tokio::time::Instant::now() + keepalive, keepalive);
    // Connect-Timeout-Ms bounds the whole stream: on expiry upstream returns
    // ctx.Err() (`watch.go:89-90`), which connect renders as
    // deadline_exceeded "context deadline exceeded" — an EndStream error
    // frame that also releases the inotify fd.
    let has_deadline = deadline.is_some();
    let inner: futures::future::OptionFuture<tokio::time::Sleep> =
        deadline.map(tokio::time::sleep).into();
    let mut deadline_fut = Box::pin(inner);
    loop {
        tokio::select! {
            // Client disconnect → immediate teardown; dropping `afd` closes
            // the inotify fd (the `defer w.Close()` + ctx.Done() pair).
            _ = tx.closed() => break,
            _ = deadline_fut.as_mut(), if has_deadline => {
                fail_stream(
                    &tx,
                    &ConnectError::new(
                        ConnectCode::DeadlineExceeded,
                        "context deadline exceeded",
                    ),
                );
                break;
            }
            _ = interval.tick() => {
                if tx
                    .send(frame_of(&WatchDirResponse::KeepAlive(Default::default())))
                    .await
                    .is_err()
                {
                    break;
                }
            }
            ready = afd.readable() => {
                let mut guard = match ready {
                    Ok(g) => g,
                    Err(e) => {
                        fail_stream(&tx, &internal(format!("watcher error: {e}")));
                        break;
                    }
                };
                guard.clear_ready();
                match drain_events(afd.get_ref(), &mut st) {
                    Ok(events) => {
                        for ev in events {
                            if tx
                                .send(frame_of(&WatchDirResponse::Filesystem(ev)))
                                .await
                                .is_err()
                            {
                                return;
                            }
                            // upstream resets the keepalive after every op
                            // (watch.go:155)
                            interval.reset();
                        }
                    }
                    Err(e) => {
                        fail_stream(&tx, &e);
                        break;
                    }
                }
            }
        }
    }
}

// ---------- pull watchers (watch_sync.go) ----------

#[derive(Default)]
struct Shared {
    events: Vec<FilesystemEvent>,
    error: Option<ConnectError>,
}

/// A pull watcher: accumulates events until polled. The pump task keeps
/// running (upstream: `context.WithoutCancel`, watch_sync.go:36) regardless
/// of the creating request; it stops when `close()` is called, when the
/// registry entry is dropped, or on a watcher error.
struct FileWatcher {
    shared: Arc<Mutex<Shared>>,
    stop_tx: tokio::sync::watch::Sender<bool>,
}

impl FileWatcher {
    fn spawn(root: PathBuf, recursive: bool) -> Result<Arc<Self>, ConnectError> {
        let (ino, state) = WatchState::new(root, recursive)?;
        let (stop_tx, stop_rx) = tokio::sync::watch::channel(false);
        let shared = Arc::new(Mutex::new(Shared::default()));
        let shared_task = Arc::clone(&shared);
        tokio::spawn(async move {
            let shared_err = Arc::clone(&shared_task);
            let res = match AsyncFd::new(ino) {
                Ok(afd) => run_pull(afd, state, stop_rx, shared_task).await,
                Err(e) => Err(internal(format!("watcher error: {e}"))),
            };
            if let Err(e) = res {
                let mut s = shared_err.lock().unwrap_or_else(|p| p.into_inner());
                if s.error.is_none() {
                    s.error = Some(e);
                }
            }
        });
        Ok(Arc::new(Self { shared, stop_tx }))
    }

    fn take_events(&self) -> Vec<FilesystemEvent> {
        let mut s = self.shared.lock().unwrap_or_else(|p| p.into_inner());
        std::mem::take(&mut s.events)
    }

    fn error(&self) -> Option<ConnectError> {
        self.shared
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .error
            .clone()
    }

    fn close(&self) {
        let _ = self.stop_tx.send(true);
    }
}

/// Buffered-event cap (see module docs: deliberate deviation from upstream's
/// unbounded accumulation). Returns Err when the cap is reached — the caller
/// turns that into the watcher error, which GetWatcherEvents surfaces.
fn buffer_events(shared: &Mutex<Shared>, events: Vec<FilesystemEvent>) -> Result<(), ConnectError> {
    let mut s = shared.lock().unwrap_or_else(|p| p.into_inner());
    for ev in events {
        if s.events.len() >= MAX_BUFFERED_EVENTS {
            return Err(internal(format!(
                "watcher event buffer overflow (>{MAX_BUFFERED_EVENTS} buffered)"
            )));
        }
        s.events.push(ev);
    }
    Ok(())
}

async fn run_pull(
    afd: AsyncFd<Inotify>,
    mut st: WatchState,
    mut stop: tokio::sync::watch::Receiver<bool>,
    shared: Arc<Mutex<Shared>>,
) -> Result<(), ConnectError> {
    loop {
        tokio::select! {
            // A dropped stop sender (registry entry removed without close)
            // also ends the pump: `changed()` errors on a closed channel.
            _ = stop.changed() => break,
            ready = afd.readable() => {
                let mut guard = match ready {
                    Ok(g) => g,
                    Err(e) => return Err(internal(format!("watcher error: {e}"))),
                };
                guard.clear_ready();
                let events = drain_events(afd.get_ref(), &mut st)?;
                buffer_events(&shared, events)?;
            }
        }
    }
    Ok(())
}

/// id → watcher. Held by the router (server.rs constructs the Arc),
/// mirroring upstream's `Service.watchers` (`service.go:15-19`): the
/// registry and its only users live together in this module, so the shared
/// state layer stays free of watch-specific entries.
pub struct WatchRegistry {
    watchers: Mutex<HashMap<String, Arc<FileWatcher>>>,
}

impl WatchRegistry {
    pub fn new() -> Self {
        Self {
            watchers: Mutex::new(HashMap::new()),
        }
    }

    fn insert(&self, id: String, w: Arc<FileWatcher>) {
        self.watchers
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .insert(id, w);
    }

    fn get(&self, id: &str) -> Option<Arc<FileWatcher>> {
        self.watchers
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .get(id)
            .cloned()
    }

    fn remove(&self, id: &str) -> Option<Arc<FileWatcher>> {
        self.watchers
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .remove(id)
    }
}

/// Upstream: `"w" + id.Generate()` (watch_sync.go:176). The id is opaque to
/// clients; any unique generator is wire-compatible as long as the
/// conformance harness normalizes watcher ids before comparing (it does).
fn generate_id() -> String {
    use std::io::Read;
    let mut buf = [0u8; 16];
    let ok = std::fs::File::open("/dev/urandom")
        .and_then(|mut f| f.read_exact(&mut buf))
        .is_ok();
    if !ok {
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0);
        buf[..16].copy_from_slice(&nanos.to_le_bytes());
    }
    let mut hex = String::with_capacity(32);
    for b in buf {
        hex.push_str(&format!("{b:02x}"));
    }
    hex
}

// ---------- handlers ----------

pub fn watch_dir(
    req: &WatchDirRequest,
    user: &User,
    headers: &axum::http::HeaderMap,
) -> axum::response::Response {
    let (ino, state) = match build_stream(req, user) {
        Ok(v) => v,
        Err(e) => return crate::services::process::stream_error_response(e),
    };
    let (tx, rx) = tokio::sync::mpsc::channel::<bytes::Bytes>(64);
    let keepalive = connect::keepalive_interval_from_headers(headers);
    let deadline = connect::timeout_from_headers(headers);
    tokio::spawn(run_stream(ino, state, keepalive, deadline, tx));
    frame_stream_response(ReceiverStream::new(rx))
}

pub fn create_watcher(
    req: &CreateWatcherRequest,
    user: &User,
    registry: &WatchRegistry,
) -> Result<serde_json::Value, ConnectError> {
    let root = prepare_watch_target(&req.path, user)?;
    let id = format!("w{}", generate_id());
    let fw = FileWatcher::spawn(root, req.recursive)?;
    registry.insert(id.clone(), fw);
    to_json(CreateWatcherResponse { watcher_id: id })
}

pub fn get_watcher_events(
    req: &GetWatcherEventsRequest,
    _user: &User,
    registry: &WatchRegistry,
) -> Result<serde_json::Value, ConnectError> {
    let Some(w) = registry.get(&req.watcher_id) else {
        return Err(ConnectError::new(
            ConnectCode::NotFound,
            format!("watcher with id {} not found", req.watcher_id),
        ));
    };
    // Upstream checks the watcher error BEFORE draining events and returns it
    // without clearing (watch_sync.go:198-200).
    if let Some(e) = w.error() {
        return Err(e);
    }
    let events = w.take_events();
    to_json(GetWatcherEventsResponse { events })
}

pub fn remove_watcher(
    req: &RemoveWatcherRequest,
    _user: &User,
    registry: &WatchRegistry,
) -> Result<serde_json::Value, ConnectError> {
    let Some(w) = registry.remove(&req.watcher_id) else {
        return Err(ConnectError::new(
            ConnectCode::NotFound,
            format!("watcher with id {} not found", req.watcher_id),
        ));
    };
    w.close();
    to_json(RemoveWatcherResponse {})
}

fn to_json<T: serde::Serialize>(value: T) -> Result<serde_json::Value, ConnectError> {
    serde_json::to_value(value)
        .map_err(|e| ConnectError::new(ConnectCode::Internal, format!("serialize response: {e}")))
}

// `frame_stream_response` lives in process.rs (pub) and is reused here
// verbatim; if the planned process.rs refactor moves it, this import is the
// only line to touch.
use crate::services::process::frame_stream_response;

#[cfg(test)]
mod tests {
    use super::*;
    use crate::auth::User;

    fn user() -> User {
        User {
            name: "test".into(),
            uid: unsafe { libc::getuid() },
            gid: unsafe { libc::getgid() },
            home: "/home/test".into(),
            groups: vec![],
        }
    }

    fn raw(wd: i32, mask: u32, name: &str) -> RawEvent {
        RawEvent {
            wd,
            mask,
            cookie: 0,
            name: (!name.is_empty()).then(|| name.to_string()),
        }
    }

    // ---- parse_events ----

    #[test]
    fn parse_roundtrips_named_and_anonymous_events() {
        let mut buf: Vec<u8> = Vec::new();
        let push = |buf: &mut Vec<u8>, wd: i32, mask: u32, cookie: u32, name: &str| {
            buf.extend_from_slice(&wd.to_ne_bytes());
            buf.extend_from_slice(&mask.to_ne_bytes());
            buf.extend_from_slice(&cookie.to_ne_bytes());
            let name_bytes = name.as_bytes();
            let len = name_bytes.len() + 1; // NUL-terminated, kernel-padded
            buf.extend_from_slice(&(len as u32).to_ne_bytes());
            buf.extend_from_slice(name_bytes);
            buf.push(0);
        };
        push(&mut buf, 3, libc::IN_CREATE, 0, "hello.txt");
        push(&mut buf, 5, libc::IN_DELETE_SELF, 0, "");

        let evs = parse_events(&buf);
        assert_eq!(evs.len(), 2);
        assert_eq!(evs[0].wd, 3);
        assert_eq!(evs[0].mask, libc::IN_CREATE);
        assert_eq!(evs[0].name.as_deref(), Some("hello.txt"));
        assert_eq!(evs[1].wd, 5);
        assert_eq!(evs[1].name, None);
    }

    /// Regression: an anonymous event (name_len == 0 — the watched directory
    /// itself) must NOT truncate the rest of the batch. A read commonly
    /// carries [ATTRIB-on-dir, CREATE-on-entry, …].
    #[test]
    fn anonymous_event_does_not_truncate_batch() {
        let mut buf: Vec<u8> = Vec::new();
        let push = |buf: &mut Vec<u8>, wd: i32, mask: u32, name: &str| {
            buf.extend_from_slice(&wd.to_ne_bytes());
            buf.extend_from_slice(&mask.to_ne_bytes());
            buf.extend_from_slice(&0u32.to_ne_bytes());
            let name_bytes = name.as_bytes();
            buf.extend_from_slice(&((name_bytes.len() + 1) as u32).to_ne_bytes());
            buf.extend_from_slice(name_bytes);
            buf.push(0);
        };
        push(&mut buf, 3, libc::IN_ATTRIB, ""); // anonymous: dir itself
        push(&mut buf, 3, libc::IN_CREATE, "after.txt");
        let evs = parse_events(&buf);
        assert_eq!(evs.len(), 2, "anonymous event must not stop parsing");
        assert_eq!(evs[0].name, None);
        assert_eq!(evs[1].name.as_deref(), Some("after.txt"));
    }

    // ---- op expansion: the fsnotify mapping is the contract ----

    #[test]
    fn moved_to_is_create_and_moved_from_is_rename() {
        assert_eq!(expand_ops(libc::IN_MOVED_TO), vec![EventType::Create]);
        assert_eq!(expand_ops(libc::IN_MOVED_FROM), vec![EventType::Rename]);
    }

    #[test]
    fn multi_op_mask_expands_in_fixed_order() {
        // A single kernel event carrying several ops must expand in the
        // watch.go:105-123 order Create→Rename→Chmod→Write→Remove — NOT in
        // enum numbering order (CREATE,WRITE,REMOVE,RENAME,CHMOD).
        let mask = libc::IN_ATTRIB | libc::IN_MODIFY | libc::IN_MOVED_FROM;
        assert_eq!(
            expand_ops(mask),
            vec![EventType::Rename, EventType::Chmod, EventType::Write]
        );
    }

    // ---- go_rel: Go filepath.Rel semantics + exact error text ----

    #[test]
    fn rel_matches_go_filepath_rel() {
        let r = Path::new("/watch");
        assert_eq!(go_rel(r, r).unwrap(), ".");
        assert_eq!(go_rel(Path::new("/watch/a/b.txt"), r).unwrap(), "a/b.txt");
        // Rel does NOT require containment — it emits ".." segments.
        assert_eq!(
            go_rel(Path::new("/elsewhere/x"), r).unwrap(),
            "../elsewhere/x"
        );
    }

    #[test]
    fn rel_error_carries_go_text() {
        let err = go_rel(Path::new("relative"), Path::new("/abs")).unwrap_err();
        assert_eq!(err, "Rel: can't make relative relative to /abs");
    }

    // ---- WatchState: registration bookkeeping ----

    #[test]
    fn recursive_setup_registers_subdirs() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("a/b")).unwrap();
        let (_ino, st) = WatchState::new(dir.path().to_path_buf(), true).unwrap();
        assert_eq!(st.dirs.len(), 3); // root, a, a/b
        assert!(st.dirs.values().any(|p| p.ends_with("a/b")));
    }

    #[test]
    fn non_recursive_setup_registers_root_only() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir(dir.path().join("a")).unwrap();
        let (_ino, st) = WatchState::new(dir.path().to_path_buf(), false).unwrap();
        assert_eq!(st.dirs.len(), 1);
    }

    // ---- handle_raw: the upstream event semantics ----

    #[test]
    fn file_create_expands_to_relative_create() {
        let dir = tempfile::tempdir().unwrap();
        let (_ino, mut st) = WatchState::new(dir.path().to_path_buf(), false).unwrap();
        let evs = st
            .handle_raw(&raw(st.root_wd, libc::IN_CREATE, "f.txt"))
            .unwrap();
        assert_eq!(evs.len(), 1);
        assert_eq!(evs[0].name, "f.txt");
        assert_eq!(evs[0].event_type, EventType::Create);
    }

    #[test]
    fn rename_pair_yields_rename_then_create() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("old"), b"").unwrap();
        let (_ino, mut st) = WatchState::new(dir.path().to_path_buf(), false).unwrap();
        let mut moved = raw(st.root_wd, libc::IN_MOVED_FROM, "old");
        moved.cookie = 7;
        assert_eq!(
            st.handle_raw(&moved).unwrap()[0].event_type,
            EventType::Rename
        );
        let mut moved_to = raw(st.root_wd, libc::IN_MOVED_TO, "new");
        moved_to.cookie = 7;
        let evs = st.handle_raw(&moved_to).unwrap();
        assert_eq!(evs.len(), 1);
        assert_eq!(evs[0].name, "new");
        assert_eq!(evs[0].event_type, EventType::Create); // MOVED_TO is Create!
    }

    #[test]
    fn new_dir_synthesizes_creates_for_existing_tree() {
        let dir = tempfile::tempdir().unwrap();
        let (_ino, mut st) = WatchState::new(dir.path().to_path_buf(), true).unwrap();
        // mkdir -p one/two happens before the kernel event for "one" is read:
        // the walk must synthesize Create(one/two) and register it.
        std::fs::create_dir_all(dir.path().join("one/two")).unwrap();
        let evs = st
            .handle_raw(&raw(st.root_wd, libc::IN_CREATE | libc::IN_ISDIR, "one"))
            .unwrap();
        let names: Vec<_> = evs.iter().map(|e| e.name.as_str()).collect();
        assert_eq!(names, vec!["one", "one/two"]);
        assert!(evs.iter().all(|e| e.event_type == EventType::Create));
        assert!(st.dirs.values().any(|p| p.ends_with("one/two")));

        // A file written into the new subtree reports its relative path.
        std::fs::write(dir.path().join("one/two/f"), b"").unwrap();
        let two_wd = *st
            .dirs
            .iter()
            .find(|(_, p)| p.ends_with("one/two"))
            .unwrap()
            .0;
        let evs = st.handle_raw(&raw(two_wd, libc::IN_CREATE, "f")).unwrap();
        assert_eq!(evs[0].name, "one/two/f");
    }

    #[test]
    fn dir_rename_rewrites_child_watch_paths() {
        let dir = tempfile::tempdir().unwrap();
        let (_ino, mut st) = WatchState::new(dir.path().to_path_buf(), true).unwrap();
        std::fs::create_dir(dir.path().join("sub")).unwrap();
        st.handle_raw(&raw(st.root_wd, libc::IN_CREATE | libc::IN_ISDIR, "sub"))
            .unwrap();
        let sub_wd = *st.dirs.iter().find(|(_, p)| p.ends_with("sub")).unwrap().0;

        let mut from = raw(st.root_wd, libc::IN_MOVED_FROM | libc::IN_ISDIR, "sub");
        from.cookie = 9;
        assert_eq!(
            st.handle_raw(&from).unwrap()[0].event_type,
            EventType::Rename
        );
        let mut to = raw(st.root_wd, libc::IN_MOVED_TO | libc::IN_ISDIR, "renamed");
        to.cookie = 9;
        st.handle_raw(&to).unwrap();

        // The child watch now points at the renamed path.
        assert!(st.dirs.get(&sub_wd).unwrap().ends_with("renamed"));
        assert!(!st
            .dirs
            .values()
            .any(|p| p.ends_with("/sub") || *p == dir.path().join("sub")));
    }

    #[test]
    fn delete_self_is_silent_when_parent_watched_loud_when_root() {
        let dir = tempfile::tempdir().unwrap();
        let (_ino, mut st) = WatchState::new(dir.path().to_path_buf(), true).unwrap();
        std::fs::create_dir(dir.path().join("sub")).unwrap();
        st.handle_raw(&raw(st.root_wd, libc::IN_CREATE | libc::IN_ISDIR, "sub"))
            .unwrap();
        let sub_wd = *st.dirs.iter().find(|(_, p)| p.ends_with("sub")).unwrap().0;

        // Parent (root) is watched → the parent's IN_DELETE reports it.
        assert!(st
            .handle_raw(&raw(sub_wd, libc::IN_DELETE_SELF, ""))
            .unwrap()
            .is_empty());
        assert!(!st.dirs.contains_key(&sub_wd));

        // Root's own deletion: parent (/) is not watched → Remove(".").
        let evs = st
            .handle_raw(&raw(st.root_wd, libc::IN_DELETE_SELF, ""))
            .unwrap();
        assert_eq!(evs.len(), 1);
        assert_eq!(evs[0].name, ".");
        assert_eq!(evs[0].event_type, EventType::Remove);
    }

    #[test]
    fn move_self_is_silent_for_children_and_rename_for_root() {
        let dir = tempfile::tempdir().unwrap();
        let (_ino, mut st) = WatchState::new(dir.path().to_path_buf(), true).unwrap();
        std::fs::create_dir(dir.path().join("sub")).unwrap();
        st.handle_raw(&raw(st.root_wd, libc::IN_CREATE | libc::IN_ISDIR, "sub"))
            .unwrap();
        let sub_wd = *st.dirs.iter().find(|(_, p)| p.ends_with("sub")).unwrap().0;

        assert!(st
            .handle_raw(&raw(sub_wd, libc::IN_MOVE_SELF, ""))
            .unwrap()
            .is_empty());
        let evs = st
            .handle_raw(&raw(st.root_wd, libc::IN_MOVE_SELF, ""))
            .unwrap();
        assert_eq!(evs.len(), 1);
        assert_eq!(evs[0].name, ".");
        assert_eq!(evs[0].event_type, EventType::Rename);
    }

    #[test]
    fn ignored_is_dropped_silently() {
        let dir = tempfile::tempdir().unwrap();
        let (_ino, mut st) = WatchState::new(dir.path().to_path_buf(), false).unwrap();
        assert!(st
            .handle_raw(&raw(st.root_wd, libc::IN_IGNORED, ""))
            .unwrap()
            .is_empty());
        assert!(st.dirs.is_empty());
    }

    // ---- prepare_watch_target error shapes (baseline vocabulary) ----

    #[test]
    fn not_a_directory_keeps_the_upstream_nil_wrap_literal() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("f"), b"").unwrap();
        let err =
            prepare_watch_target(&dir.path().join("f").to_string_lossy(), &user()).unwrap_err();
        assert_eq!(err.code, ConnectCode::InvalidArgument);
        assert!(
            err.message.contains("not a directory: %!w(<nil>)"),
            "{}",
            err.message
        );
    }

    #[test]
    fn missing_path_is_not_found_with_stat_op() {
        let err = prepare_watch_target("/nope/watch-me", &user()).unwrap_err();
        assert_eq!(err.code, ConnectCode::NotFound);
        assert_eq!(
            err.message,
            "path /nope/watch-me not found: stat /nope/watch-me: no such file or directory"
        );
    }

    #[test]
    fn network_mount_check_runs_on_real_fs() {
        let dir = tempfile::tempdir().unwrap();
        // tmpfs/ext4/overlayfs/9p — none of the five network magics.
        assert!(!is_network_mount(dir.path()).unwrap());
    }

    // ---- pull watcher lifecycle ----

    #[tokio::test]
    async fn pull_watcher_accumulates_drains_and_stops() {
        let dir = tempfile::tempdir().unwrap();
        let fw = FileWatcher::spawn(dir.path().to_path_buf(), false).unwrap();
        std::fs::write(dir.path().join("f.txt"), b"x").unwrap();
        let deadline = std::time::Instant::now() + Duration::from_secs(3);
        let events = loop {
            let ev = fw.take_events();
            if !ev.is_empty() {
                break ev;
            }
            assert!(std::time::Instant::now() < deadline, "no event within 3s");
            tokio::time::sleep(Duration::from_millis(20)).await;
        };
        assert!(events
            .iter()
            .any(|e| e.name == "f.txt" && e.event_type == EventType::Create));
        // Drain clears the buffer.
        assert!(fw.take_events().is_empty());
        fw.close();
    }

    #[tokio::test]
    async fn watcher_registry_lifecycle_matches_upstream_codes() {
        let registry = WatchRegistry::new();
        // Unknown id on both RPCs → NotFound with the upstream text.
        for req_err in [
            get_watcher_events(
                &GetWatcherEventsRequest {
                    watcher_id: "wX".into(),
                },
                &user(),
                &registry,
            )
            .unwrap_err(),
            remove_watcher(
                &RemoveWatcherRequest {
                    watcher_id: "wX".into(),
                },
                &user(),
                &registry,
            )
            .unwrap_err(),
        ] {
            assert_eq!(req_err.code, ConnectCode::NotFound);
            assert_eq!(req_err.message, "watcher with id wX not found");
        }

        let dir = tempfile::tempdir().unwrap();
        let created = create_watcher(
            &CreateWatcherRequest {
                path: dir.path().to_string_lossy().into_owned(),
                recursive: false,
            },
            &user(),
            &registry,
        )
        .unwrap();
        let id = created["watcherId"].as_str().unwrap().to_string();
        assert!(id.starts_with('w'));

        // Remove twice: second must be NotFound (upstream deletes on remove).
        remove_watcher(
            &RemoveWatcherRequest {
                watcher_id: id.clone(),
            },
            &user(),
            &registry,
        )
        .unwrap();
        assert_eq!(
            remove_watcher(&RemoveWatcherRequest { watcher_id: id }, &user(), &registry)
                .unwrap_err()
                .code,
            ConnectCode::NotFound
        );
    }

    #[test]
    fn pull_buffer_cap_surfaces_error_instead_of_growing() {
        let shared = Arc::new(Mutex::new(Shared::default()));
        let events: Vec<FilesystemEvent> = (0..=MAX_BUFFERED_EVENTS)
            .map(|i| FilesystemEvent {
                name: i.to_string(),
                event_type: EventType::Create,
            })
            .collect();
        let res = buffer_events(&shared, events);
        assert!(res.is_err());
        // The cap holds: exactly MAX events retained, error recorded by the
        // pump wrapper (run_pull's `?`).
        assert_eq!(shared.lock().unwrap().events.len(), MAX_BUFFERED_EVENTS);
    }

    // ---- streaming teardown ----

    #[tokio::test]
    async fn stream_ends_when_client_disconnects() {
        let dir = tempfile::tempdir().unwrap();
        let (ino, st) = WatchState::new(dir.path().to_path_buf(), false).unwrap();
        let (tx, mut rx) = tokio::sync::mpsc::channel::<bytes::Bytes>(8);
        let jh = tokio::spawn(run_stream(
            ino,
            st,
            Duration::from_millis(50),
            None,
            tx.clone(),
        ));
        // First frame must be the Start event.
        let first = rx.recv().await.unwrap();
        let text = std::str::from_utf8(&first[5..]).unwrap(); // skip 1+4 header
        assert!(text.contains("\"start\""), "{text}");
        // Client hangs up → the pump must exit promptly (fd closed by Drop).
        drop(rx);
        let _ = tx.closed().await;
        tokio::time::timeout(Duration::from_secs(2), jh)
            .await
            .expect("pump did not exit after disconnect")
            .unwrap();
    }
    // ---- queue overflow → fatal (watch.go:91-96 watcher-error path) ----

    #[test]
    fn queue_overflow_kills_the_watcher_with_internal() {
        let dir = tempfile::tempdir().unwrap();
        let (_ino, mut st) = WatchState::new(dir.path().to_path_buf(), false).unwrap();
        let err = st
            .handle_raw(&raw(st.root_wd, libc::IN_Q_OVERFLOW, ""))
            .unwrap_err();
        assert_eq!(err.code, ConnectCode::Internal);
        assert!(
            err.message.contains("fsnotify queue overflow"),
            "{}",
            err.message
        );
    }

    // ---- recursion must not follow symlinked directories ----

    #[test]
    fn symlinked_directories_are_not_recursively_registered() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir(dir.path().join("sub")).unwrap();
        // A link to a watched directory and one pointing outside the tree:
        // neither may be registered (fsnotify's WalkDir never follows links),
        // otherwise a rename/move of the target would dangle the watch.
        std::os::unix::fs::symlink(dir.path().join("sub"), dir.path().join("link")).unwrap();
        std::os::unix::fs::symlink("/etc", dir.path().join("outside")).unwrap();
        let (_ino, st) = WatchState::new(dir.path().to_path_buf(), true).unwrap();
        assert_eq!(st.dirs.len(), 2, "root + sub only: {:?}", st.dirs);
        assert!(st.dirs.values().any(|p| p.ends_with("sub")));
        assert!(!st
            .dirs
            .values()
            .any(|p| p.ends_with("link") || p.ends_with("outside")));
    }

    // ---- keepalive reset: each event pushes the next ping a full period ----

    #[tokio::test]
    async fn keepalive_resets_after_every_event() {
        use std::time::Instant;
        let dir = tempfile::tempdir().unwrap();
        // Pre-create the file so the single in-sandbox write yields exactly
        // one IN_MODIFY (a fresh create would emit CREATE + MODIFY).
        std::fs::write(dir.path().join("seed"), b"").unwrap();
        let (ino, st) = WatchState::new(dir.path().to_path_buf(), false).unwrap();
        let (tx, mut rx) = tokio::sync::mpsc::channel::<bytes::Bytes>(64);
        let period = Duration::from_millis(400);
        let jh = tokio::spawn(run_stream(ino, st, period, None, tx));

        // Start frame; record the mutation moment.
        let _start = rx.recv().await.unwrap();
        tokio::time::sleep(Duration::from_millis(200)).await;
        let t_event = Instant::now();
        std::fs::write(dir.path().join("seed"), b"x").unwrap();

        // Collect every frame for 900ms after the event, recording keepalive
        // arrival offsets. The event resets the ticker, so the next ping must
        // land a full period (400ms) after the event — not at the stale
        // schedule (200ms after).
        let mut ka_offsets_ms: Vec<u128> = Vec::new();
        let mut got_event = false;
        loop {
            let remaining = Duration::from_millis(900).saturating_sub(t_event.elapsed());
            if remaining.is_zero() {
                break;
            }
            match tokio::time::timeout(remaining, rx.recv()).await {
                Ok(Some(frame)) => {
                    let payload = std::str::from_utf8(&frame[5..]).unwrap_or("");
                    let elapsed = t_event.elapsed().as_millis();
                    if payload.contains("\"filesystem\"") {
                        got_event = true;
                    } else if payload.contains("\"keepalive\"") {
                        ka_offsets_ms.push(elapsed);
                    }
                }
                _ => break,
            }
        }
        assert!(got_event, "filesystem event frame never arrived");
        jh.abort();

        // Without a reset the ping would fire 200ms after the event (the
        // original schedule); with it, a full period later (400ms). The gap
        // between the two hypotheses is 200ms — wide enough for CI.
        assert!(
            ka_offsets_ms.first().is_none_or(|ms| *ms >= 300),
            "keepalive arrived {}ms after the event — reset missing",
            ka_offsets_ms.first().unwrap_or(&0)
        );
        assert!(
            ka_offsets_ms.first().is_some_and(|ms| *ms <= 700),
            "no keepalive within 700ms after the event — ticker died: {ka_offsets_ms:?}"
        );
    }

    // ---- concurrent lifecycle: get/remove racing a create must never
    // panic, deadlock, or resurrect a removed watcher ----

    #[tokio::test]
    async fn concurrent_lifecycle_races_stay_consistent() {
        let registry = Arc::new(WatchRegistry::new());
        let dir = tempfile::tempdir().unwrap();
        let mut handles = Vec::new();
        for _ in 0..8 {
            let reg = Arc::clone(&registry);
            let root = dir.path().to_string_lossy().into_owned();
            handles.push(tokio::spawn(async move {
                let created = create_watcher(
                    &CreateWatcherRequest {
                        path: root,
                        recursive: false,
                    },
                    &user(),
                    &reg,
                )
                .unwrap();
                let id = created["watcherId"].as_str().unwrap().to_string();
                // Racing gets: either the watcher is still there (Ok) or
                // already removed (NotFound) — both are valid outcomes.
                let _ = get_watcher_events(
                    &GetWatcherEventsRequest {
                        watcher_id: id.clone(),
                    },
                    &user(),
                    &reg,
                );
                remove_watcher(
                    &RemoveWatcherRequest {
                        watcher_id: id.clone(),
                    },
                    &user(),
                    &reg,
                )
                .unwrap();
                // Double remove: NotFound, never a panic or silent Ok.
                assert_eq!(
                    remove_watcher(
                        &RemoveWatcherRequest {
                            watcher_id: id.clone()
                        },
                        &user(),
                        &reg,
                    )
                    .unwrap_err()
                    .code,
                    ConnectCode::NotFound
                );
                id
            }));
        }
        // The timeout catches a registry deadlock; unwraps catch panics.
        let mut ids = Vec::new();
        for h in handles {
            ids.push(
                tokio::time::timeout(Duration::from_secs(5), h)
                    .await
                    .expect("lifecycle task deadlocked")
                    .unwrap(),
            );
        }
        // Every watcher is gone from the registry.
        for id in ids {
            assert_eq!(
                get_watcher_events(
                    &GetWatcherEventsRequest { watcher_id: id },
                    &user(),
                    &registry,
                )
                .unwrap_err()
                .code,
                ConnectCode::NotFound
            );
        }
    }
    // ---- PR #16 review P1: MOVE_SELF on a recursion-added child must keep
    // the watch (the parent's MOVED_TO rewrites its stored path), otherwise
    // events inside the moved directory silently stop ----

    #[test]
    fn in_tree_dir_rename_keeps_child_watch_reporting() {
        let dir = tempfile::tempdir().unwrap();
        let (_ino, mut st) = WatchState::new(dir.path().to_path_buf(), true).unwrap();
        std::fs::create_dir(dir.path().join("a")).unwrap();
        st.handle_raw(&raw(st.root_wd, libc::IN_CREATE | libc::IN_ISDIR, "a"))
            .unwrap();
        let a_wd = *st
            .dirs
            .iter()
            .find(|(_, p)| p.as_path() == dir.path().join("a"))
            .unwrap()
            .0;

        // rename a → b: parent MOVED_FROM/MOVED_TO pair, then the child's
        // own MOVE_SELF.
        let mut from = raw(st.root_wd, libc::IN_MOVED_FROM | libc::IN_ISDIR, "a");
        from.cookie = 5;
        st.handle_raw(&from).unwrap();
        let mut to = raw(st.root_wd, libc::IN_MOVED_TO | libc::IN_ISDIR, "b");
        to.cookie = 5;
        st.handle_raw(&to).unwrap();
        // The events above mirror a real rename — perform it on disk too, so
        // later writes land inside the moved directory.
        std::fs::rename(dir.path().join("a"), dir.path().join("b")).unwrap();
        assert!(st
            .handle_raw(&raw(a_wd, libc::IN_MOVE_SELF, ""))
            .unwrap()
            .is_empty());

        // The child watch SURVIVES and points at the new path…
        assert!(st.dirs.contains_key(&a_wd));
        assert!(st.dirs[&a_wd].ends_with("b"));
        // …and events inside the moved directory keep flowing, named
        // relative to the watch root.
        std::fs::write(dir.path().join("b/f"), b"").unwrap();
        let evs = st.handle_raw(&raw(a_wd, libc::IN_CREATE, "f")).unwrap();
        assert_eq!(evs.len(), 1);
        assert_eq!(evs[0].name, "b/f");
    }

    // ---- PR #16 review P2: Connect-Timeout-Ms bounds the stream ----

    #[tokio::test]
    async fn connect_timeout_ends_the_watch_stream() {
        let dir = tempfile::tempdir().unwrap();
        let (ino, st) = WatchState::new(dir.path().to_path_buf(), false).unwrap();
        let (tx, mut rx) = tokio::sync::mpsc::channel::<bytes::Bytes>(8);
        let jh = tokio::spawn(run_stream(
            ino,
            st,
            Duration::from_secs(30),
            Some(Duration::from_millis(150)),
            tx,
        ));
        let _start = rx.recv().await.unwrap();
        // On expiry upstream returns ctx.Err(): deadline_exceeded
        // "context deadline exceeded" as an EndStream error frame.
        let frame = tokio::time::timeout(Duration::from_secs(1), rx.recv())
            .await
            .expect("no deadline frame within 1s")
            .unwrap();
        let payload = std::str::from_utf8(&frame[5..]).unwrap_or("");
        assert!(payload.contains("deadline_exceeded"), "{payload}");
        assert!(payload.contains("context deadline exceeded"), "{payload}");
        assert_ne!(frame[0] & 0x02, 0, "EndStream flag must be set");
        // The stream is over.
        assert!(rx.recv().await.is_none());
        jh.await.unwrap();
    }
}

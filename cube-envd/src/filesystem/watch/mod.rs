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
//!   `protocol::keepalive::DEFAULT_KEEPALIVE_INTERVAL`) rather than the filesystem
//!   watch's 90s (`permissions/keepalive.go:10`) — same LB-idle-timeout
//!   rationale already recorded for the Start stream. The
//!   `Keepalive-Ping-Interval` header overrides it identically.
//!
//! Recursion is implemented by hand (inotify watches single directories):
//! the semantics are a faithful port of the fsnotify recursive backend
//! (`backend_inotify.go:230-263` for setup, `:495-563` for dynamic
//! directories), including the synthetic Create events for `mkdir -p`
//! subtrees and the cookie-paired path rewrite for directory renames.
//!
//! Module layout — one file, one question:
//! - `inotify` — the kernel boundary: raw `libc::inotify_*` calls and the
//!   `read()` buffer parser; depends on nothing above it.
//! - `tree`    — the semantic machine: expansion order, recursion, the cookie
//!   ring, directory mapping, and the MOVE_SELF / DELETE_SELF branches.
//! - `pump`    — the streaming pump: the deadline / keepalive / disconnect
//!   four-way select.
//! - `mod`     — the facade: the four RPC handlers, the watch-target
//!   prechecks, and the pull-watcher buffer + registry.

use std::collections::HashMap;
use std::io::ErrorKind;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use tokio::io::unix::AsyncFd;
use tokio_stream::wrappers::ReceiverStream;

use crate::compat::vocab::go_path_error;
use crate::filesystem::wire::{
    CreateWatcherRequest, CreateWatcherResponse, FilesystemEvent, GetWatcherEventsRequest,
    GetWatcherEventsResponse, RemoveWatcherRequest, RemoveWatcherResponse, WatchDirRequest,
};
use crate::platform::identity::{resolve_path, User};
use crate::protocol;
use crate::protocol::stream::frame_stream_response;
use crate::protocol::{ConnectCode, ConnectError};

mod inotify;
mod pump;
mod tree;

use inotify::Inotify;
use pump::{internal, run_stream};
use tree::{drain_events, WatchState};

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

/// id → watcher. Held by the router (`app/routes.rs` constructs the Arc),
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
        Err(e) => return crate::protocol::stream::stream_error_response(e),
    };
    let (tx, rx) = tokio::sync::mpsc::channel::<bytes::Bytes>(64);
    let keepalive = protocol::keepalive_interval_from_headers(headers);
    let deadline = protocol::timeout_from_headers(headers);
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    use crate::filesystem::wire::EventType;

    fn user() -> User {
        User {
            name: "test".into(),
            uid: unsafe { libc::getuid() },
            gid: unsafe { libc::getgid() },
            home: "/home/test".into(),
            groups: vec![],
        }
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
}

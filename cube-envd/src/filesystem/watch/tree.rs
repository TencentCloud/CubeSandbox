// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! How events become semantics: expansion order / recursion / cookie pairing /
//! directory mapping / MOVE_SELF.
//!
//! Carries over the semantic machine of `services/watch.rs`. Every contract
//! is checked line by line against fsnotify `backend_inotify.go:568-596` and
//! friends (see the per-item comments below).

use std::collections::HashMap;
use std::io::ErrorKind;
use std::os::unix::ffi::OsStringExt;
use std::os::unix::io::{AsRawFd, RawFd};
use std::path::{Path, PathBuf};

use crate::compat::vocab::errno_text;
use crate::filesystem::wire::{EventType, FilesystemEvent};
use crate::protocol::{ConnectCode, ConnectError};

use super::inotify::{add_watch_raw, parse_events, rm_watch_raw, Inotify, RawEvent};

/// fsnotify's default read buffer (`fsnotify.go:443-446`: 64K, "the highest
/// value that works on all filesystems").
const READ_BUF: usize = 64 * 1024;

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
pub(super) struct WatchState {
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
    pub(super) fn new(root: PathBuf, recursive: bool) -> Result<(Inotify, Self), ConnectError> {
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
pub(super) fn drain_events(
    ino: &Inotify,
    st: &mut WatchState,
) -> Result<Vec<FilesystemEvent>, ConnectError> {
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

#[cfg(test)]
mod tests {
    use super::*;

    fn raw(wd: i32, mask: u32, name: &str) -> RawEvent {
        RawEvent {
            wd,
            mask,
            cookie: 0,
            name: (!name.is_empty()).then(|| name.to_string()),
        }
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
    // ---- MOVE_SELF on a recursion-added child must keep the watch (the
    // parent's MOVED_TO rewrites its stored path), otherwise events inside
    // the moved directory silently stop ----

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
}

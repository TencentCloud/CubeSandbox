// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! `filesystem.Filesystem` unary RPC implementations.
//!
//! Error vocabulary is aligned with the Go envd 0.5.13 baseline, including
//! the Go-syscall-flavored messages SDK users may match on. Every (context,
//! op) pair below is derived from the upstream call site listed next to it;
//! errno texts come from `compat::vocab` (go1.26 table, lowercase).
//! Verified shapes:
//! - Stat missing:    404 not_found  "file not found: lstat <p>: no such file or directory"
//! - Stat other:      500 internal   "error getting file info: lstat <p>: <errno>"   (utils.go:49)
//! - ListDir missing: 404 not_found  "path not found: lstat <p>: no such file or directory"
//! - ListDir ELOOP:   400 failed_precondition "cyclic symlink or chain >255 links at \"<p>\"" (dir.go:109-111)
//! - ListDir walk:    500 internal   "error reading directory <d>: <inner>"          (dir.go:180)
//! - MakeDir exists:  409 already_exists "directory already exists: <p>"            (dir.go:74)
//! - MakeDir levels:  500 internal   "failed to create directory: mkdir <p>: <errno>" / "path is a file: <p>" (path.go:77-94)
//! - Move missing:    404 not_found  "source file not found: rename <s> <d>: no such file or directory"
//! - Move other:      500 internal   "error renaming: rename <s> <d>: <errno>"       (move.go:47)
//! - Watch family:    implemented in `filesystem/watch/` (streaming + pull watchers)

use crate::compat::vocab::{go_link_error, go_path_error};
use crate::filesystem::entry::entry_info;
use crate::filesystem::wire::{
    EntryInfo, EntryResponse, ListDirRequest, ListDirResponse, MoveRequest, PathRequest,
};
use crate::platform::identity::User;
use crate::protocol::{ConnectCode, ConnectError};
use std::os::unix::fs::DirBuilderExt;

#[cfg(test)]
mod data_plane_tests;
pub mod download;
pub mod entry;
pub mod errors;
pub mod http;
pub mod upload;
pub mod watch;
pub mod wire;

pub use download::download;
pub use upload::upload;

#[cfg(test)]
pub(crate) use download::modtime_of;
#[cfg(test)]
pub(crate) use upload::{entry_for, parse_boundary, spawn_upload_writer};

pub fn stat(req: &PathRequest, user: &User) -> Result<serde_json::Value, ConnectError> {
    let path = crate::platform::identity::resolve_path(&req.path, user);
    let meta = std::fs::symlink_metadata(&path).map_err(|e| entry_error(&path, &e))?;
    to_json(EntryResponse {
        entry: entry_info(&path, &meta),
    })
}

pub fn make_dir(req: &PathRequest, user: &User) -> Result<serde_json::Value, ConnectError> {
    let path = crate::platform::identity::resolve_path(&req.path, user);
    // Follow like upstream's os.Stat (dir.go MakeDir :69-85): an existing
    // path is AlreadyExists only when it IS a directory; an existing file
    // (or a link to one) is a caller bug -> InvalidArgument.
    match std::fs::metadata(&path) {
        Ok(meta) if meta.is_dir() => {
            return Err(ConnectError::new(
                ConnectCode::AlreadyExists,
                format!("directory already exists: {path}"),
            ));
        }
        Ok(_) => {
            return Err(ConnectError::new(
                ConnectCode::InvalidArgument,
                format!("path already exists but it is not a directory: {path}"),
            ));
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Err(e) => {
            return Err(ConnectError::from_io(
                "error getting file info",
                "stat",
                &path,
                &e,
            ));
        }
    }
    // Upstream EnsureDirs (dir.go:85 → path.go:68-98): create every missing
    // component root→leaf and chown each to the requesting user — so
    // `MakeDir a/b` hands ownership of both `a` and `a/b`.
    ensure_dirs(&path, user)?;
    let meta = std::fs::symlink_metadata(&path).map_err(|e| entry_error(&path, &e))?;
    to_json(EntryResponse {
        entry: entry_info(&path, &meta),
    })
}

pub fn move_entry(req: &MoveRequest, user: &User) -> Result<serde_json::Value, ConnectError> {
    let source = crate::platform::identity::resolve_path(&req.source, user);
    let destination = crate::platform::identity::resolve_path(&req.destination, user);
    // move.go:36 — the destination's parent chain is created (and chowned)
    // before the rename, so moving into a not-yet-existing directory
    // succeeds upstream. Skipping this made us fail with `internal` where
    // the baseline returns 200.
    let parent = std::path::Path::new(&destination)
        .parent()
        .map(|p| p.to_string_lossy().into_owned())
        .unwrap_or_else(|| "/".to_string());
    ensure_dirs(&parent, user)?;
    std::fs::rename(&source, &destination).map_err(|e| {
        if e.kind() == std::io::ErrorKind::NotFound {
            // move.go:43-45. No `exists()` pre-check: rename(2) does not
            // follow symlinks, so — like upstream — a dangling source
            // symlink is renamed successfully rather than reported missing.
            ConnectError::new(
                ConnectCode::NotFound,
                format!(
                    "source file not found: {}",
                    go_link_error("rename", &source, &destination, &e)
                ),
            )
        } else {
            // move.go:47 — `*os.LinkError` carries both paths.
            ConnectError::from_io_link("error renaming", &source, &destination, &e)
        }
    })?;
    let meta =
        std::fs::symlink_metadata(&destination).map_err(|e| entry_error(&destination, &e))?;
    to_json(EntryResponse {
        entry: entry_info(&destination, &meta),
    })
}

/// Baseline: removing a missing path succeeds (200 {}).
pub fn remove(req: &PathRequest, user: &User) -> Result<serde_json::Value, ConnectError> {
    let path = crate::platform::identity::resolve_path(&req.path, user);
    match std::fs::symlink_metadata(&path) {
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            return Ok(serde_json::json!({}));
        }
        Err(e) => {
            // Upstream has no pre-check (remove.go:25 goes straight to
            // RemoveAll), so this branch has no exact baseline shape; as root
            // it is unreachable (CAP_DAC_OVERRIDE). Rendered in the baseline
            // vocabulary, best-effort.
            return Err(ConnectError::from_io(
                "error removing file or directory",
                "lstat",
                &path,
                &e,
            ));
        }
        Ok(meta) => {
            let res = if meta.is_dir() {
                std::fs::remove_dir_all(&path)
            } else {
                std::fs::remove_file(&path)
            };
            // remove.go:27 wraps RemoveAll failures as
            // "error removing file or directory: %w". The inner PathError op
            // varies inside Go's RemoveAll (`open` for parent traversal,
            // `unlinkat` for the actual unlink); `unlinkat` is the common
            // terminal case.
            res.map_err(|e| {
                ConnectError::from_io("error removing file or directory", "unlinkat", &path, &e)
            })?;
        }
    }
    Ok(serde_json::json!({}))
}

/// Directory walk with the upstream `filepath.WalkDir` semantics (dir.go
/// walkDir :120-175): lexical order (ReadDirNames sorts), depth-first — a
/// subdirectory's entries follow it immediately — and symlinked directories
/// are listed but never entered (WalkDir does not follow links).
///
/// Quirk faithfully inherited from upstream's combination of WalkDir (lstat)
/// and GetEntryInfo (follows): a symlink-to-directory entry lists as
/// `type: FILE_TYPE_DIRECTORY` yet is never descended into. That is correct
/// behavior here; do not "fix" it by entering the link.
/// `cur` is the depth of the entries produced by this call: 1 = the root's
/// children.
fn walk_dir(
    dir: &str,
    cur: u32,
    max: u32,
    entries: &mut Vec<EntryInfo>,
) -> Result<(), ConnectError> {
    if cur > max {
        return Ok(());
    }
    let read = std::fs::read_dir(dir).map_err(|e| {
        // dir.go:180 wraps every WalkDir failure as
        // "error reading directory %s: %w"; the inner PathError for reading
        // a directory is `readdirent {dir}: …` (go1.26 os/dir_unix.go:89).
        ConnectError::from_io("error reading directory", "readdirent", dir, &e)
    })?;
    let mut children: Vec<_> = read.filter_map(|e| e.ok()).collect();
    children.sort_by_key(|e| e.file_name());
    for child in children {
        let child_path = child.path().to_string_lossy().into_owned();
        // dir.go:163-169: entries that vanish between readdir and lstat are
        // skipped (NotFound), but any other lstat failure aborts the whole
        // walk — the entryInfo error surfaces nested inside dir.go:180's
        // "error reading directory" wrapper.
        match std::fs::symlink_metadata(&child_path) {
            Ok(meta) => {
                entries.push(entry_info(&child_path, &meta));
                if meta.is_dir() && cur < max {
                    walk_dir(&child_path, cur + 1, max, entries)?;
                }
            }
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
            Err(e) => {
                return Err(ConnectError::from_io(
                    &format!("error reading directory {dir}: error getting file info"),
                    "lstat",
                    &child_path,
                    &e,
                ));
            }
        }
    }
    Ok(())
}

/// DFS listing with the proto `depth` semantics (0/absent behaves as 1).
pub fn list_dir(req: &ListDirRequest, user: &User) -> Result<serde_json::Value, ConnectError> {
    let root = crate::platform::identity::resolve_path(&req.path, user);
    // Upstream ListDir resolves the root through EvalSymlinks (dir.go:36),
    // stats the resolved path (checkIfDirectory, dir.go:41-56) and walks it
    // (dir.go:46) while naming entries after the *requested* path
    // (dir.go:172). `canonicalize` is the Rust equivalent of EvalSymlinks
    // and is what makes the ELOOP branch reachable.
    let canonical = match std::fs::canonicalize(&root) {
        Ok(p) => p,
        Err(e) => return Err(listdir_root_error(&root, &e)),
    };
    let root_meta = std::fs::metadata(&canonical).map_err(|e| {
        // checkIfDirectory (dir.go:120-135) on the resolved path.
        let canonical_str = canonical.to_string_lossy();
        if e.kind() == std::io::ErrorKind::NotFound {
            ConnectError::new(
                ConnectCode::NotFound,
                format!(
                    "directory not found: {}",
                    go_path_error("stat", &canonical_str, &e)
                ),
            )
        } else {
            ConnectError::from_io("error getting file info", "stat", &canonical_str, &e)
        }
    })?;
    if !root_meta.is_dir() {
        // dir.go:131 — the message carries the *resolved* path (upstream
        // passes resolvedPath into checkIfDirectory).
        return Err(ConnectError::new(
            ConnectCode::InvalidArgument,
            format!("path is not a directory: {}", canonical.display()),
        ));
    }
    let max_depth = if req.depth == 0 { 1 } else { req.depth };

    let mut entries = Vec::new();
    walk_dir(&root, 1, max_depth, &mut entries)?;
    to_json(ListDirResponse { entries })
}

/// Baseline entryInfo error vocabulary (upstream `utils.go:42-50`):
/// - ENOENT        -> NotFound  "file not found: lstat {p}: …"
/// - anything else -> Internal  "error getting file info: lstat {p}: …"
fn entry_error(path: &str, err: &std::io::Error) -> ConnectError {
    if err.kind() == std::io::ErrorKind::NotFound {
        ConnectError::new(
            ConnectCode::NotFound,
            format!("file not found: {}", go_path_error("lstat", path, err)),
        )
    } else {
        ConnectError::from_io("error getting file info", "lstat", path, err)
    }
}

/// Baseline ListDir root error vocabulary (upstream `followSymlink`,
/// `dir.go:36-113`):
/// - ENOENT -> NotFound           "path not found: lstat {p}: …"
/// - ELOOP  -> FailedPrecondition "cyclic symlink or chain >255 links at \"{p}\"" —
///   dir.go:109-111; the EvalSymlinks error itself never appears (go1.26
///   renders it as `EvalSymlinks: too many links`, no path)
/// - else   -> Internal           "error resolving symlink: lstat {p}: …"
fn listdir_root_error(path: &str, err: &std::io::Error) -> ConnectError {
    if err.raw_os_error() == Some(libc::ELOOP) {
        ConnectError::new(
            ConnectCode::FailedPrecondition,
            format!("cyclic symlink or chain >255 links at \"{path}\""),
        )
    } else if err.kind() == std::io::ErrorKind::NotFound {
        ConnectError::new(
            ConnectCode::NotFound,
            format!("path not found: {}", go_path_error("lstat", path, err)),
        )
    } else {
        ConnectError::from_io("error resolving symlink", "lstat", path, err)
    }
}

/// Upstream `permissions.EnsureDirs` (dir.go:85 → path.go:68-98): walk the
/// components root→leaf, stat each (following links), create missing ones
/// with mode 0o755 and chown each to the requesting user. Failure shapes,
/// all `internal` (dir.go:87):
/// - `failed to stat directory: stat {p}: …`      (path.go:72-74)
/// - `failed to create directory: mkdir {p}: …`   (path.go:77-81)
/// - `failed to chown directory: chown {p}: …`    (path.go:82-87, hard error)
/// - `path is a file: {p}`                        (path.go:92-94)
fn ensure_dirs(path: &str, user: &User) -> Result<(), ConnectError> {
    let mut subpaths: Vec<String> = Vec::new();
    let mut cur = path.to_string();
    loop {
        subpaths.push(cur.clone());
        // getSubpaths (path.go:53-66) stops before "/" — it is never created.
        match std::path::Path::new(&cur).parent() {
            Some(p) if !p.as_os_str().is_empty() && p != std::path::Path::new("/") => {
                cur = p.to_string_lossy().into_owned();
            }
            _ => break,
        }
    }
    subpaths.reverse();
    for sp in subpaths {
        match std::fs::metadata(&sp) {
            Ok(info) => {
                if !info.is_dir() {
                    return Err(ConnectError::new(
                        ConnectCode::Internal,
                        format!("path is a file: {sp}"),
                    ));
                }
            }
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                // Upstream os.Mkdir mode 0o755 (path.go:77).
                std::fs::DirBuilder::new()
                    .mode(0o755)
                    .create(&sp)
                    .map_err(|e| {
                        ConnectError::from_io("failed to create directory", "mkdir", &sp, &e)
                    })?;
                // lchown rather than upstream's os.Chown: the component was
                // just created by us, so the two agree on the happy path, and
                // lchown cannot be redirected by a swapped-in symlink.
                if let Ok(c_path) = std::ffi::CString::new(sp.as_bytes()) {
                    unsafe {
                        if libc::lchown(c_path.as_ptr(), user.uid, user.gid) != 0 {
                            let e = std::io::Error::last_os_error();
                            return Err(ConnectError::from_io(
                                "failed to chown directory",
                                "chown",
                                &sp,
                                &e,
                            ));
                        }
                    }
                }
            }
            Err(e) => {
                return Err(ConnectError::from_io(
                    "failed to stat directory",
                    "stat",
                    &sp,
                    &e,
                ));
            }
        }
    }
    Ok(())
}

fn to_json<T: serde::Serialize>(value: T) -> Result<serde_json::Value, ConnectError> {
    serde_json::to_value(value)
        .map_err(|e| ConnectError::new(ConnectCode::Internal, format!("serialize response: {e}")))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn test_user(home: &str) -> User {
        User {
            name: "test".into(),
            uid: unsafe { libc::getuid() },
            gid: unsafe { libc::getgid() },
            home: home.into(),
            groups: vec![],
        }
    }

    #[test]
    fn stat_missing_matches_baseline_message() {
        let dir = tempfile::tempdir().unwrap();
        let user = test_user(dir.path().to_str().unwrap());
        let err = stat(
            &PathRequest {
                path: "nope".into(),
            },
            &user,
        )
        .unwrap_err();
        assert_eq!(err.code, ConnectCode::NotFound);
        assert_eq!(
            err.message,
            format!(
                "file not found: lstat {}/nope: no such file or directory",
                dir.path().display()
            )
        );
    }

    #[test]
    fn makedir_move_remove_roundtrip() {
        let dir = tempfile::tempdir().unwrap();
        let user = test_user(dir.path().to_str().unwrap());

        let v = make_dir(&PathRequest { path: "a/b".into() }, &user).unwrap();
        assert_eq!(v["entry"]["type"], "FILE_TYPE_DIRECTORY");
        assert_eq!(v["entry"]["name"], "b");

        let err = make_dir(&PathRequest { path: "a/b".into() }, &user).unwrap_err();
        assert_eq!(err.code, ConnectCode::AlreadyExists);
        assert!(err.message.starts_with("directory already exists: "));

        std::fs::write(dir.path().join("f.txt"), b"data").unwrap();
        // An existing FILE at the target is a caller bug, not an
        // AlreadyExists: upstream splits on os.Stat isDir (dir.go:73-83).
        let ferr = make_dir(
            &PathRequest {
                path: "f.txt".into(),
            },
            &user,
        )
        .unwrap_err();
        assert_eq!(ferr.code, ConnectCode::InvalidArgument);
        assert!(ferr
            .message
            .starts_with("path already exists but it is not a directory: "));
        let v = move_entry(
            &MoveRequest {
                source: "f.txt".into(),
                destination: "g.txt".into(),
            },
            &user,
        )
        .unwrap();
        assert_eq!(v["entry"]["name"], "g.txt");
        assert_eq!(v["entry"]["size"], "4");

        let err = move_entry(
            &MoveRequest {
                source: "f.txt".into(),
                destination: "h.txt".into(),
            },
            &user,
        )
        .unwrap_err();
        assert_eq!(err.code, ConnectCode::NotFound);
        assert!(err.message.starts_with("source file not found: rename "));

        // Remove is idempotent per baseline.
        assert_eq!(
            remove(&PathRequest { path: "a".into() }, &user).unwrap(),
            serde_json::json!({})
        );
        assert_eq!(
            remove(&PathRequest { path: "a".into() }, &user).unwrap(),
            serde_json::json!({})
        );
    }

    #[test]
    fn list_dir_depth() {
        let dir = tempfile::tempdir().unwrap();
        let user = test_user(dir.path().to_str().unwrap());
        std::fs::create_dir_all(dir.path().join("d1/d2")).unwrap();
        std::fs::write(dir.path().join("d1/f1"), b"x").unwrap();
        std::fs::write(dir.path().join("d1/d2/d3.txt"), b"z").unwrap();
        std::fs::write(dir.path().join("top"), b"y").unwrap();

        let v = list_dir(
            &ListDirRequest {
                path: ".".into(),
                depth: 1,
            },
            &user,
        )
        .unwrap();
        let names: Vec<&str> = v["entries"]
            .as_array()
            .unwrap()
            .iter()
            .map(|e| e["name"].as_str().unwrap())
            .collect();
        assert_eq!(names, vec!["d1", "top"]);

        let v = list_dir(
            &ListDirRequest {
                path: ".".into(),
                depth: 2,
            },
            &user,
        )
        .unwrap();
        let names: Vec<&str> = v["entries"]
            .as_array()
            .unwrap()
            .iter()
            .map(|e| e["name"].as_str().unwrap())
            .collect();
        // DFS order (filepath.WalkDir semantics): d1's children follow d1
        // immediately, before the next root entry. This is the ordering the
        // BFS->DFS change exists to match — pin the full sequence, not
        // membership.
        assert_eq!(names, vec!["d1", "d2", "f1", "top"]);

        let v = list_dir(
            &ListDirRequest {
                path: ".".into(),
                depth: 3,
            },
            &user,
        )
        .unwrap();
        let names: Vec<&str> = v["entries"]
            .as_array()
            .unwrap()
            .iter()
            .map(|e| e["name"].as_str().unwrap())
            .collect();
        // ... and a third level really is a third level: d3.txt appears only
        // at depth >= 3, and it follows d2 immediately (DFS), not after the
        // rest of d1's children.
        assert_eq!(names, vec!["d1", "d2", "d3.txt", "f1", "top"]);

        let err = list_dir(
            &ListDirRequest {
                path: "missing".into(),
                depth: 1,
            },
            &user,
        )
        .unwrap_err();
        assert!(err.message.starts_with("path not found: lstat "));
    }

    #[test]
    fn list_dir_dangling_root_is_not_found() {
        // The root is resolved with a following stat, so a dangling link is
        // NotFound — NOT InvalidArgument ("path is not a directory"), which
        // is what the previous non-following lstat produced. The `lstat`
        // wording is upstream's (EvalSymlinks fails with an lstat error);
        // see the comment on `list_dir` before "correcting" it.
        use std::os::unix::fs::symlink;
        let dir = tempfile::tempdir().unwrap();
        let user = test_user(dir.path().to_str().unwrap());
        symlink(dir.path().join("gone"), dir.path().join("dangling")).unwrap();

        let err = list_dir(
            &ListDirRequest {
                path: "dangling".into(),
                depth: 1,
            },
            &user,
        )
        .unwrap_err();
        assert_eq!(err.code, ConnectCode::NotFound);
        assert_eq!(
            err.message,
            format!(
                "path not found: lstat {}/dangling: no such file or directory",
                dir.path().display()
            )
        );
    }

    #[test]
    fn list_dir_root_follows_symlink_and_children_do_not() {
        // Upstream ListDir resolves the ROOT with followSymlink + os.Stat
        // (dir.go:44-56), but walks children with lstat semantics — a
        // symlink-to-directory root lists its contents, while a symlinked
        // child is listed (as the target's type, via entry_info) but never
        // descended into.
        use std::os::unix::fs::symlink;
        let dir = tempfile::tempdir().unwrap();
        let user = test_user(dir.path().to_str().unwrap());
        std::fs::create_dir_all(dir.path().join("real/sub")).unwrap();
        std::fs::write(dir.path().join("real/sub/deep.txt"), b"x").unwrap();
        std::fs::write(dir.path().join("real/top.txt"), b"x").unwrap();
        symlink(dir.path().join("real"), dir.path().join("lroot")).unwrap();
        symlink(dir.path().join("real/sub"), dir.path().join("real/lsub")).unwrap();

        // A symlink-to-dir as the ROOT lists fine (followed).
        let v = list_dir(
            &ListDirRequest {
                path: "lroot".into(),
                depth: 1,
            },
            &user,
        )
        .unwrap();
        let names: Vec<&str> = v["entries"]
            .as_array()
            .unwrap()
            .iter()
            .map(|e| e["name"].as_str().unwrap())
            .collect();
        assert_eq!(names, vec!["lsub", "sub", "top.txt"]);
        // The symlinked child lists as DIRECTORY (followed type) ...
        let types: Vec<&str> = v["entries"]
            .as_array()
            .unwrap()
            .iter()
            .map(|e| e["type"].as_str().unwrap())
            .collect();
        assert_eq!(
            types,
            vec![
                "FILE_TYPE_DIRECTORY",
                "FILE_TYPE_DIRECTORY",
                "FILE_TYPE_FILE"
            ]
        );

        // ... yet depth 2 must NOT enter the SYMLINKED child (deep.txt under
        // real/sub comes only via the real `sub` directory): WalkDir does not
        // follow links.
        let v = list_dir(
            &ListDirRequest {
                path: "lroot".into(),
                depth: 2,
            },
            &user,
        )
        .unwrap();
        let names: Vec<&str> = v["entries"]
            .as_array()
            .unwrap()
            .iter()
            .map(|e| e["name"].as_str().unwrap())
            .collect();
        assert_eq!(
            names,
            vec!["lsub", "sub", "deep.txt", "top.txt"],
            "real sub is entered, symlinked lsub is not"
        );
    }

    /// dir.go:109-111 — a cyclic root is FailedPrecondition with the %q
    /// message; the underlying EvalSymlinks error never appears.
    #[test]
    fn listdir_eloop_is_failed_precondition() {
        let dir = tempfile::tempdir().unwrap();
        let user = test_user(dir.path().to_str().unwrap());
        std::os::unix::fs::symlink("loop", dir.path().join("loop")).unwrap();

        let err = list_dir(
            &ListDirRequest {
                path: "loop".into(),
                depth: 1,
            },
            &user,
        )
        .unwrap_err();
        assert_eq!(err.code, ConnectCode::FailedPrecondition);
        assert_eq!(
            err.message,
            format!(
                "cyclic symlink or chain >255 links at \"{}\"",
                dir.path().join("loop").display()
            )
        );
    }

    /// Non-ENOENT errno texts come from the compat::vocab table (go1.26), not
    /// from strerror — this is the exact divergence the conformance harness
    /// never covered.
    #[test]
    fn stat_enotdir_renders_go_table_text() {
        let dir = tempfile::tempdir().unwrap();
        let user = test_user(dir.path().to_str().unwrap());
        std::fs::write(dir.path().join("f.txt"), b"x").unwrap();

        let err = stat(
            &PathRequest {
                path: "f.txt/sub".into(),
            },
            &user,
        )
        .unwrap_err();
        assert_eq!(err.code, ConnectCode::Internal);
        assert_eq!(
            err.message,
            format!(
                "error getting file info: lstat {}/f.txt/sub: not a directory",
                dir.path().display()
            )
        );
    }

    /// MakeDir whose parent chain crosses a file: the precheck os.Stat
    /// returns ENOTDIR (not ENOENT), so upstream fails in dir.go:69 before
    /// EnsureDirs is ever reached.
    #[test]
    fn makedir_through_file_uses_precheck_vocabulary() {
        let dir = tempfile::tempdir().unwrap();
        let user = test_user(dir.path().to_str().unwrap());
        std::fs::write(dir.path().join("blocker"), b"x").unwrap();

        let err = make_dir(
            &PathRequest {
                path: "blocker/sub".into(),
            },
            &user,
        )
        .unwrap_err();
        assert_eq!(err.code, ConnectCode::Internal);
        assert_eq!(
            err.message,
            format!(
                "error getting file info: stat {}/blocker/sub: not a directory",
                dir.path().display()
            )
        );
    }

    /// move.go:36 — EnsureDirs on the destination's parent runs before the
    /// rename, so moving into a missing directory succeeds (and chowns it).
    #[test]
    fn move_into_missing_parent_succeeds_like_baseline() {
        let dir = tempfile::tempdir().unwrap();
        let user = test_user(dir.path().to_str().unwrap());
        std::fs::write(dir.path().join("f.txt"), b"x").unwrap();

        let v = move_entry(
            &MoveRequest {
                source: "f.txt".into(),
                destination: "newdir/sub/x.txt".into(),
            },
            &user,
        )
        .unwrap();
        assert_eq!(v["entry"]["name"], "x.txt");
        assert!(dir.path().join("newdir/sub/x.txt").exists());
    }

    /// rename(2) does not follow symlinks: a dangling source symlink is
    /// renamed successfully upstream (no exists() pre-check exists there).
    #[test]
    fn move_dangling_symlink_source_succeeds_like_baseline() {
        let dir = tempfile::tempdir().unwrap();
        let user = test_user(dir.path().to_str().unwrap());
        std::os::unix::fs::symlink(dir.path().join("nope"), dir.path().join("dangling")).unwrap();

        let v = move_entry(
            &MoveRequest {
                source: "dangling".into(),
                destination: "renamed".into(),
            },
            &user,
        )
        .unwrap();
        assert_eq!(v["entry"]["name"], "renamed");
        // Dangling links type as UNSPECIFIED, which proto3 JSON omits
        // (baseline-verified: default enum values are not serialized).
        assert!(v["entry"].get("type").is_none());
        // "renamed" is itself a dangling symlink — exists() (following)
        // would be false; lstat sees the link.
        assert!(dir.path().join("renamed").symlink_metadata().is_ok());
    }
}

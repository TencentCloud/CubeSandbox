// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! `filesystem.Filesystem` unary RPC implementations.
//!
//! Error vocabulary is aligned with the Go envd 0.5.13 baseline, including
//! the Go-syscall-flavored messages SDK users may match on:
//! - Stat missing:    404 not_found  "file not found: lstat <p>: no such file or directory"
//! - ListDir missing: 404 not_found  "path not found: lstat <p>: no such file or directory"
//! - MakeDir exists:  409 already_exists "directory already exists: <p>"
//! - Move missing:    404 not_found  "source file not found: rename <s> <d>: no such file or directory"
//! - Remove missing:  200 {} (idempotent — baseline-verified)
//! - Watch family:    not implemented by cube-envd (MVP scope, issue #1227)

use std::collections::VecDeque;

use crate::auth::User;
use crate::error::{ConnectCode, ConnectError};
use crate::msg::filesystem::{
    entry_info, EntryResponse, ListDirRequest, ListDirResponse, MoveRequest, PathRequest,
};

pub fn stat(req: &PathRequest, user: &User) -> Result<serde_json::Value, ConnectError> {
    let path = crate::auth::resolve_path(&req.path, user);
    let meta = std::fs::symlink_metadata(&path).map_err(|e| stat_error("file", &path, &e))?;
    to_json(EntryResponse {
        entry: entry_info(&path, &meta),
    })
}

pub fn make_dir(req: &PathRequest, user: &User) -> Result<serde_json::Value, ConnectError> {
    let path = crate::auth::resolve_path(&req.path, user);
    if std::path::Path::new(&path).exists() {
        return Err(ConnectError::new(
            ConnectCode::AlreadyExists,
            format!("directory already exists: {path}"),
        ));
    }
    // Track every component about to be created so ownership can be handed
    // to the requesting user on all of them (baseline: `MakeDir a/b` chowns
    // both `a` and `a/b`).
    let mut missing = Vec::new();
    let mut cursor = std::path::PathBuf::from(&path);
    while !cursor.exists() {
        missing.push(cursor.clone());
        match cursor.parent() {
            Some(p) => cursor = p.to_path_buf(),
            None => break,
        }
    }
    std::fs::create_dir_all(&path)
        .map_err(|e| ConnectError::from_io("error creating directory", &path, &e))?;
    for created in missing.iter().rev() {
        chown_path(created, user);
    }
    let meta = std::fs::symlink_metadata(&path)
        .map_err(|e| ConnectError::from_io("error reading created directory", &path, &e))?;
    to_json(EntryResponse {
        entry: entry_info(&path, &meta),
    })
}

pub fn move_entry(req: &MoveRequest, user: &User) -> Result<serde_json::Value, ConnectError> {
    let source = crate::auth::resolve_path(&req.source, user);
    let destination = crate::auth::resolve_path(&req.destination, user);
    if !std::path::Path::new(&source).exists() {
        return Err(ConnectError::new(
            ConnectCode::NotFound,
            format!(
                "source file not found: rename {source} {destination}: no such file or directory"
            ),
        ));
    }
    std::fs::rename(&source, &destination)
        .map_err(|e| ConnectError::from_io("error moving file", &source, &e))?;
    let meta = std::fs::symlink_metadata(&destination)
        .map_err(|e| ConnectError::from_io("error reading moved file", &destination, &e))?;
    to_json(EntryResponse {
        entry: entry_info(&destination, &meta),
    })
}

/// Baseline: removing a missing path succeeds (200 {}).
pub fn remove(req: &PathRequest, user: &User) -> Result<serde_json::Value, ConnectError> {
    let path = crate::auth::resolve_path(&req.path, user);
    match std::fs::symlink_metadata(&path) {
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            return Ok(serde_json::json!({}));
        }
        Err(e) => return Err(ConnectError::from_io("error removing path", &path, &e)),
        Ok(meta) => {
            let res = if meta.is_dir() {
                std::fs::remove_dir_all(&path)
            } else {
                std::fs::remove_file(&path)
            };
            res.map_err(|e| ConnectError::from_io("error removing path", &path, &e))?;
        }
    }
    Ok(serde_json::json!({}))
}

/// BFS listing with the proto `depth` semantics (0/absent behaves as 1).
pub fn list_dir(req: &ListDirRequest, user: &User) -> Result<serde_json::Value, ConnectError> {
    let root = crate::auth::resolve_path(&req.path, user);
    let root_meta = std::fs::symlink_metadata(&root).map_err(|e| stat_error("path", &root, &e))?;
    if !root_meta.is_dir() {
        return Err(ConnectError::new(
            ConnectCode::InvalidArgument,
            format!("path is not a directory: {root}"),
        ));
    }
    let max_depth = if req.depth == 0 { 1 } else { req.depth };

    let mut entries = Vec::new();
    let mut queue: VecDeque<(String, u32)> = VecDeque::from([(root, 1u32)]);
    while let Some((dir, depth)) = queue.pop_front() {
        let read = std::fs::read_dir(&dir)
            .map_err(|e| ConnectError::from_io("error listing directory", &dir, &e))?;
        let mut children: Vec<_> = read.filter_map(|e| e.ok()).collect();
        children.sort_by_key(|e| e.file_name());
        for child in children {
            let child_path = child.path().to_string_lossy().into_owned();
            if let Ok(meta) = std::fs::symlink_metadata(&child_path) {
                let is_dir = meta.is_dir();
                entries.push(entry_info(&child_path, &meta));
                if is_dir && depth < max_depth {
                    queue.push_back((child_path, depth + 1));
                }
            }
        }
    }
    to_json(ListDirResponse { entries })
}

fn stat_error(noun: &str, path: &str, err: &std::io::Error) -> ConnectError {
    if err.kind() == std::io::ErrorKind::NotFound {
        ConnectError::new(
            ConnectCode::NotFound,
            format!("{noun} not found: lstat {path}: no such file or directory"),
        )
    } else {
        ConnectError::from_io(&format!("error reading {noun}"), path, err)
    }
}

/// Give a newly-created path to the requesting user, best-effort.
fn chown_path(path: &std::path::Path, user: &User) {
    if let Ok(c_path) = std::ffi::CString::new(path.as_os_str().as_encoded_bytes()) {
        unsafe {
            // lchown so a symlink swapped in at `path` cannot redirect the
            // ownership change onto an unrelated target.
            libc::lchown(c_path.as_ptr(), user.uid, user.gid);
        }
    }
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
        assert!(names.contains(&"d2") && names.contains(&"f1"));

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
}

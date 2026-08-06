// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! GET/POST /files — download and upload.
//!
//! Baseline contract:
//! - user resolution: `username` query > Basic auth > root;
//! - relative paths anchor at the user's home;
//! - download errors: 400 (directory / missing path param), 401 (bad user),
//!   404 (missing file), body `{"code":<int>,"message":"..."}`;
//! - upload: multipart (part filename = target path) or raw octet-stream
//!   (`path` query required); parents are created; the file is chowned to
//!   the requesting user; response is `[{"name","path","type":"file"}]`;
//! - gzip response encoding is NOT implemented (declared difference —
//!   identity responses are valid HTTP for any Accept-Encoding).

use std::collections::HashMap;
use std::io::Write;
use std::os::unix::fs::PermissionsExt;
use std::sync::Arc;

use axum::extract::{Query, State};
use axum::http::{HeaderMap, StatusCode};
use axum::response::IntoResponse;

use crate::auth::{self, User};
use crate::error::RestError;
use crate::state::AppState;

fn resolve_request_user(
    params: &HashMap<String, String>,
    headers: &HeaderMap,
) -> Result<User, RestError> {
    let name = params
        .get("username")
        .cloned()
        .or_else(|| {
            auth::user_from_basic_auth(
                headers
                    .get(axum::http::header::AUTHORIZATION)
                    .and_then(|v| v.to_str().ok()),
            )
        })
        .unwrap_or_else(|| auth::DEFAULT_USER.to_string());
    auth::lookup_user(&name).map_err(|msg| RestError::new(StatusCode::UNAUTHORIZED, msg))
}

fn check_token_rest(state: &AppState, headers: &HeaderMap) -> Result<(), RestError> {
    super::check_token(state, headers)
        .map_err(|_| RestError::new(StatusCode::UNAUTHORIZED, "invalid access token".to_string()))
}

/// GET /files — stream a file back as application/octet-stream.
pub async fn download(
    State(state): State<Arc<AppState>>,
    Query(params): Query<HashMap<String, String>>,
    headers: HeaderMap,
) -> axum::response::Response {
    if let Err(e) = check_token_rest(&state, &headers) {
        return e.into_response();
    }
    let user = match resolve_request_user(&params, &headers) {
        Ok(u) => u,
        Err(e) => return e.into_response(),
    };
    // Baseline: a missing `path` parameter falls back to the user's home
    // directory (which then fails with the "is a directory" error).
    let raw_path = params
        .get("path")
        .cloned()
        .unwrap_or_else(|| user.home.clone());
    let path = auth::resolve_path(&raw_path, &user);

    let meta = match tokio::fs::metadata(&path).await {
        Ok(m) => m,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            return RestError::new(
                StatusCode::NOT_FOUND,
                format!("path '{path}' does not exist"),
            )
            .into_response();
        }
        Err(e) if e.kind() == std::io::ErrorKind::PermissionDenied => {
            return RestError::new(
                StatusCode::INTERNAL_SERVER_ERROR,
                format!("error opening file '{path}': permission denied"),
            )
            .into_response();
        }
        Err(e) => {
            return RestError::new(
                StatusCode::INTERNAL_SERVER_ERROR,
                format!("error opening file '{path}': {e}"),
            )
            .into_response();
        }
    };
    if meta.is_dir() {
        return RestError::new(
            StatusCode::BAD_REQUEST,
            format!("path '{path}' is a directory"),
        )
        .into_response();
    }

    let mut file = match tokio::fs::File::open(&path).await {
        Ok(f) => f,
        Err(e) => {
            return RestError::new(
                StatusCode::INTERNAL_SERVER_ERROR,
                format!("error opening file '{path}': {e}"),
            )
            .into_response();
        }
    };
    // Baseline serves downloads through Go's content sniffer; approximate it
    // with a text/binary split, which covers what SDK users observe. The
    // full DetectContentType table is a declared non-goal.
    let content_type = match sniff_content_type(&mut file).await {
        Ok(ct) => ct,
        Err(_) => {
            // Rewind failed (non-seekable special file): reopen so the body
            // still starts at byte 0 instead of after the sniffed prefix.
            match tokio::fs::File::open(&path).await {
                Ok(f) => {
                    file = f;
                    "application/octet-stream"
                }
                Err(e) => {
                    return RestError::new(
                        StatusCode::INTERNAL_SERVER_ERROR,
                        format!("error opening file '{path}': {e}"),
                    )
                    .into_response();
                }
            }
        }
    };
    let stream = tokio_util_reader_stream(file);
    let mut builder = axum::response::Response::builder()
        .status(StatusCode::OK)
        .header(axum::http::header::CONTENT_TYPE, content_type);
    // stat-size-0 but readable files (/proc/*, some sysfs) would get a
    // Content-Length: 0 header that truncates the real body — stream those
    // chunked instead. Regular files keep the explicit length (baseline
    // sends one).
    if meta.len() > 0 {
        builder = builder.header(axum::http::header::CONTENT_LENGTH, meta.len());
    }
    builder
        .body(axum::body::Body::from_stream(stream))
        .expect("build download response")
}

/// text/plain for valid-UTF-8, NUL-free content (first 512 bytes), else
/// application/octet-stream. Reads from the already-open handle and rewinds,
/// so the file is opened exactly once per download; a failed rewind is
/// returned as Err so the caller can reopen rather than stream from a
/// mid-file offset.
async fn sniff_content_type(f: &mut tokio::fs::File) -> std::io::Result<&'static str> {
    use tokio::io::{AsyncReadExt, AsyncSeekExt};
    let mut buf = [0u8; 512];
    // A failed read must fall back to the generic binary type, not be
    // treated as "0 bytes of text".
    let n = match f.read(&mut buf).await {
        Ok(n) => n,
        Err(_) => {
            f.rewind().await?;
            return Ok("application/octet-stream");
        }
    };
    f.rewind().await?;
    let head = &buf[..n];
    let looks_text = !head.contains(&0)
        && match std::str::from_utf8(head) {
            Ok(_) => true,
            // A multi-byte char may be cut at the 512-byte boundary.
            Err(e) => e.valid_up_to() + 4 > head.len(),
        };
    if looks_text {
        Ok("text/plain; charset=utf-8")
    } else {
        Ok("application/octet-stream")
    }
}

/// 64 KiB chunked reader stream without pulling in tokio-util.
fn tokio_util_reader_stream(
    file: tokio::fs::File,
) -> impl futures::Stream<Item = Result<bytes::Bytes, std::io::Error>> + Send {
    use tokio::io::AsyncReadExt;
    futures::stream::unfold(file, |mut file| async move {
        let mut buf = vec![0u8; 64 * 1024];
        match file.read(&mut buf).await {
            Ok(0) => None,
            Ok(n) => {
                buf.truncate(n);
                Some((Ok(bytes::Bytes::from(buf)), file))
            }
            Err(e) => Some((Err(e), file)),
        }
    })
}

/// POST /files — multipart or raw octet-stream upload.
pub async fn upload(
    State(state): State<Arc<AppState>>,
    Query(params): Query<HashMap<String, String>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    if let Err(e) = check_token_rest(&state, &headers) {
        return e.into_response();
    }
    let user = match resolve_request_user(&params, &headers) {
        Ok(u) => u,
        Err(e) => return e.into_response(),
    };

    let content_type = headers
        .get(axum::http::header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_string();

    let result = if content_type.starts_with("multipart/form-data") {
        upload_multipart(&content_type, body, &user).await
    } else {
        upload_raw(body, &params, &user).await
    };

    match result {
        // Baseline quirk: upstream writes the JSON array without an explicit
        // content type, so Go's sniffer labels it text/plain.
        Ok(entries) => (
            StatusCode::OK,
            [(
                axum::http::header::CONTENT_TYPE,
                "text/plain; charset=utf-8",
            )],
            serde_json::to_string(&entries).unwrap_or_else(|_| "[]".to_string()),
        )
            .into_response(),
        Err(e) => e.into_response(),
    }
}

#[derive(serde::Serialize)]
struct UploadEntry {
    name: String,
    path: String,
    #[serde(rename = "type")]
    entry_type: &'static str,
}

async fn upload_raw(
    body: axum::body::Body,
    params: &HashMap<String, String>,
    user: &User,
) -> Result<Vec<UploadEntry>, RestError> {
    let Some(raw_path) = params.get("path") else {
        return Err(RestError::new(
            StatusCode::BAD_REQUEST,
            "the 'path' query parameter is required for application/octet-stream uploads",
        ));
    };
    let path = auth::resolve_path(raw_path, user);
    let data = axum::body::to_bytes(body, crate::connect::MAX_ENVELOPE_SIZE)
        .await
        .map_err(|e| {
            // A body over the cap must report 413 like the multipart path
            // (and the documented contract), not a generic 400.
            let msg = e.to_string();
            if msg.contains("length limit") {
                RestError::new(
                    StatusCode::PAYLOAD_TOO_LARGE,
                    format!(
                        "the upload exceeds the {}-byte limit",
                        crate::connect::MAX_ENVELOPE_SIZE
                    ),
                )
            } else {
                RestError::new(
                    StatusCode::BAD_REQUEST,
                    format!("error reading body: {msg}"),
                )
            }
        })?;
    let user_owned = user.clone();
    let path_owned = path.clone();
    // Disk writes (up to 64 MiB + fsync) must not block the small tokio
    // worker pool — a stalled worker would also stall /health.
    tokio::task::spawn_blocking(move || write_file(&path_owned, &data, &user_owned))
        .await
        .map_err(|e| {
            RestError::new(
                StatusCode::INTERNAL_SERVER_ERROR,
                format!("write task: {e}"),
            )
        })??;
    Ok(vec![entry_for(&path)])
}

/// RFC 2045-style parameter extraction for the multipart boundary: handles
/// a quoted boundary (semicolons allowed inside the quotes) and trailing
/// parameters after it (`; charset=...`), not just `boundary=` at the end.
fn parse_boundary(content_type: &str) -> Option<String> {
    let after = content_type.split("boundary=").nth(1)?;
    if let Some(rest) = after.strip_prefix('"') {
        rest.split('"').next().map(|s| s.to_string())
    } else {
        Some(after.split(';').next().unwrap_or(after).trim().to_string())
    }
    .filter(|b| !b.is_empty())
}

async fn upload_multipart(
    content_type: &str,
    body: axum::body::Body,
    user: &User,
) -> Result<Vec<UploadEntry>, RestError> {
    let boundary = parse_boundary(content_type)
        .ok_or_else(|| RestError::new(StatusCode::BAD_REQUEST, "missing multipart boundary"))?;

    let stream = body.into_data_stream();
    // Bound the whole multipart payload so an unbounded upload cannot OOM the
    // daemon. Matches the cap the raw octet-stream path already enforces
    // (upload_raw uses MAX_ENVELOPE_SIZE); without this, multer defaults to
    // unlimited and bypasses axum's DefaultBodyLimit on a streamed body.
    let constraints = multer::Constraints::new().size_limit(
        multer::SizeLimit::new()
            .whole_stream(crate::connect::MAX_ENVELOPE_SIZE as u64)
            .per_field(crate::connect::MAX_ENVELOPE_SIZE as u64),
    );
    let mut multipart = multer::Multipart::with_constraints(stream, boundary, constraints);

    let mut entries = Vec::new();
    while let Some(field) = multipart.next_field().await.map_err(map_multipart_error)? {
        // Only parts carrying a filename are file uploads (part filename =
        // target path, matching upstream). A plain form field must not fall
        // back to the `?path` query target — that would let a stray text
        // field overwrite the real file's bytes. The `?path` fallback
        // belongs to the raw octet-stream path only.
        let Some(target) = field.file_name().map(|s| s.to_string()) else {
            continue;
        };
        let path = auth::resolve_path(&target, user);
        let data = field.bytes().await.map_err(map_multipart_error)?;
        let user_owned = user.clone();
        let path_owned = path.clone();
        // Same reasoning as upload_raw: keep big writes off the async workers.
        tokio::task::spawn_blocking(move || write_file(&path_owned, &data, &user_owned))
            .await
            .map_err(|e| {
                RestError::new(
                    StatusCode::INTERNAL_SERVER_ERROR,
                    format!("write task: {e}"),
                )
            })??;
        entries.push(entry_for(&path));
    }
    if entries.is_empty() {
        return Err(RestError::new(
            StatusCode::BAD_REQUEST,
            "multipart upload contained no file",
        ));
    }
    Ok(entries)
}

/// Map a multer error, reporting a size-limit breach as 413 (the upload was
/// refused for being too large) and any other parse failure as 400.
fn map_multipart_error(e: multer::Error) -> RestError {
    if matches!(
        e,
        multer::Error::StreamSizeExceeded { .. } | multer::Error::FieldSizeExceeded { .. }
    ) {
        RestError::new(
            StatusCode::PAYLOAD_TOO_LARGE,
            format!(
                "multipart upload exceeds the {}-byte limit",
                crate::connect::MAX_ENVELOPE_SIZE
            ),
        )
    } else {
        RestError::new(
            StatusCode::BAD_REQUEST,
            format!("error reading multipart: {e}"),
        )
    }
}

fn entry_for(path: &str) -> UploadEntry {
    let name = std::path::Path::new(path)
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_else(|| path.to_string());
    UploadEntry {
        name,
        path: path.to_string(),
        entry_type: "file",
    }
}

/// Create parents (owned by the user), write via temp file + rename, chown.
/// Overwriting an existing file preserves its mode bits (upstream envd writes
/// in place with O_TRUNC, which never touches the mode; the temp-file path
/// must copy it explicitly or an executable script would lose its x bits).
fn write_file(path: &str, data: &[u8], user: &User) -> Result<(), RestError> {
    let target = std::path::Path::new(path);
    if let Some(parent) = target.parent() {
        if !parent.exists() {
            create_dirs_owned(parent, user)?;
        }
    }
    // lstat, not stat: if the target is a symlink we replace the link itself,
    // so the mode to preserve is the link entry's, never the followed target's.
    let existing_mode = std::fs::symlink_metadata(target)
        .ok()
        .filter(|m| m.is_file())
        .map(|m| m.permissions().mode());

    let dir = target.parent().unwrap_or(std::path::Path::new("/"));
    let tmp_path = dir.join(format!(
        ".cube-envd-upload-{}-{}",
        std::process::id(),
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0)
    ));

    let write_result = (|| -> std::io::Result<()> {
        // create_new (O_EXCL): never follow or reuse a pre-planted path at
        // the (predictable) temp name.
        let mut f = std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&tmp_path)?;
        f.write_all(data)?;
        if let Some(mode) = existing_mode {
            f.set_permissions(std::fs::Permissions::from_mode(mode))?;
        }
        f.sync_all()?;
        Ok(())
    })();
    if let Err(e) = write_result {
        let _ = std::fs::remove_file(&tmp_path);
        return Err(map_write_error(path, &e));
    }
    chown(&tmp_path, user);
    if let Err(e) = std::fs::rename(&tmp_path, target) {
        let _ = std::fs::remove_file(&tmp_path);
        return Err(map_write_error(path, &e));
    }
    Ok(())
}

fn create_dirs_owned(dir: &std::path::Path, user: &User) -> Result<(), RestError> {
    // Find the deepest existing ancestor, then create and chown below it.
    let mut missing = Vec::new();
    let mut cursor = dir.to_path_buf();
    while !cursor.exists() {
        missing.push(cursor.clone());
        match cursor.parent() {
            Some(p) => cursor = p.to_path_buf(),
            None => break,
        }
    }
    std::fs::create_dir_all(dir).map_err(|e| map_write_error(&dir.to_string_lossy(), &e))?;
    for created in missing.iter().rev() {
        chown(created, user);
    }
    Ok(())
}

fn chown(path: &std::path::Path, user: &User) {
    if let Ok(c_path) = std::ffi::CString::new(path.as_os_str().as_encoded_bytes()) {
        unsafe {
            // lchown, not chown: never follow a symlink when setting ownership,
            // so a planted symlink at `path` cannot redirect the chown onto an
            // arbitrary target the caller shouldn't be able to take over.
            libc::lchown(c_path.as_ptr(), user.uid, user.gid);
        }
    }
}

fn map_write_error(path: &str, e: &std::io::Error) -> RestError {
    if e.raw_os_error() == Some(libc::ENOSPC) {
        RestError::new(
            StatusCode::INSUFFICIENT_STORAGE,
            "not enough disk space available",
        )
    } else if e.kind() == std::io::ErrorKind::PermissionDenied {
        RestError::new(
            StatusCode::INTERNAL_SERVER_ERROR,
            format!("error writing file '{path}': permission denied"),
        )
    } else {
        RestError::new(
            StatusCode::INTERNAL_SERVER_ERROR,
            format!("error writing file '{path}': {e}"),
        )
    }
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
    fn write_file_creates_parents_and_is_atomic() {
        let dir = tempfile::tempdir().unwrap();
        let user = test_user(dir.path().to_str().unwrap());
        let target = dir.path().join("a/b/c.txt");
        write_file(target.to_str().unwrap(), b"hello", &user).unwrap();
        assert_eq!(std::fs::read(&target).unwrap(), b"hello");
        // Overwrite works and leaves no temp files behind.
        write_file(target.to_str().unwrap(), b"world", &user).unwrap();
        assert_eq!(std::fs::read(&target).unwrap(), b"world");
        let leftovers: Vec<_> = std::fs::read_dir(target.parent().unwrap())
            .unwrap()
            .filter_map(|e| e.ok())
            .filter(|e| {
                e.file_name()
                    .to_string_lossy()
                    .starts_with(".cube-envd-upload")
            })
            .collect();
        assert!(leftovers.is_empty());
    }

    #[test]
    fn overwrite_preserves_mode_bits() {
        let dir = tempfile::tempdir().unwrap();
        let user = test_user(dir.path().to_str().unwrap());
        let target = dir.path().join("script.sh");
        write_file(target.to_str().unwrap(), b"#!/bin/sh\n", &user).unwrap();
        std::fs::set_permissions(&target, std::fs::Permissions::from_mode(0o755)).unwrap();
        // Overwriting through the temp-file+rename path must keep 0755.
        write_file(target.to_str().unwrap(), b"#!/bin/sh\necho v2\n", &user).unwrap();
        let mode = std::fs::metadata(&target).unwrap().permissions().mode() & 0o7777;
        assert_eq!(mode, 0o755);
        // A fresh file still gets default create permissions.
        let fresh = dir.path().join("plain.txt");
        write_file(fresh.to_str().unwrap(), b"x", &user).unwrap();
        assert!(std::fs::metadata(&fresh).unwrap().permissions().mode() & 0o111 == 0);
    }

    #[test]
    fn boundary_parsing_variants() {
        assert_eq!(
            parse_boundary("multipart/form-data; boundary=abc123").as_deref(),
            Some("abc123")
        );
        assert_eq!(
            parse_boundary("multipart/form-data; boundary=abc; charset=utf-8").as_deref(),
            Some("abc")
        );
        assert_eq!(
            parse_boundary(r#"multipart/form-data; boundary="quo;ted"; charset=utf-8"#).as_deref(),
            Some("quo;ted")
        );
        assert_eq!(parse_boundary("multipart/form-data"), None);
        assert_eq!(parse_boundary("multipart/form-data; boundary="), None);
    }

    #[test]
    fn upload_entry_shape() {
        let e = entry_for("/home/user/hello.txt");
        let v = serde_json::to_value(&e).unwrap();
        assert_eq!(
            v,
            serde_json::json!({"name":"hello.txt","path":"/home/user/hello.txt","type":"file"})
        );
    }
}

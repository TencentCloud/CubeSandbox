// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! POST `/files`: raw and multipart upload handling.

use std::collections::HashMap;
use std::io::Write;
use std::sync::Arc;

use axum::extract::{Query, State};
use axum::http::{HeaderMap, StatusCode};
use axum::response::IntoResponse;

use super::errors::{check_token_rest, resolve_request_user, MAX_UPLOAD_SIZE};
use crate::auth::{self, User};
use crate::error::RestError;
use crate::state::AppState;

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
    let user = match resolve_request_user(&state, &params, &headers) {
        Ok(u) => u,
        Err(e) => return e.into_response(),
    };

    // Byte-oriented like Go's Header.Get: an obs-text byte in the boundary
    // must not misroute a multipart request into the raw path.
    let content_type = headers
        .get(axum::http::header::CONTENT_TYPE)
        .map(|v| String::from_utf8_lossy(v.as_bytes()).into_owned())
        .unwrap_or_default();

    let result = if content_type.starts_with("multipart/form-data") {
        upload_multipart(&content_type, body, &user).await
    } else {
        let content_length = headers
            .get(axum::http::header::CONTENT_LENGTH)
            .and_then(|v| v.to_str().ok())
            .and_then(|v| v.parse::<u64>().ok());
        upload_raw(body, &params, &user, content_length).await
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
pub(crate) struct UploadEntry {
    name: String,
    path: String,
    #[serde(rename = "type")]
    entry_type: &'static str,
}

async fn upload_raw(
    body: axum::body::Body,
    params: &HashMap<String, String>,
    user: &User,
    content_length: Option<u64>,
) -> Result<Vec<UploadEntry>, RestError> {
    let Some(raw_path) = params.get("path") else {
        return Err(RestError::new(
            StatusCode::BAD_REQUEST,
            "the 'path' query parameter is required for application/octet-stream uploads",
        ));
    };
    let path = auth::resolve_path(raw_path, user);
    // Typed fast path for the common case: a declared Content-Length over
    // the cap is rejected without reading the body. (axum 0.7's to_bytes
    // error has no public typed accessor, so chunked bodies still rely on
    // the string match below.)
    if let Some(len) = content_length {
        if len > MAX_UPLOAD_SIZE as u64 {
            return Err(RestError::new(
                StatusCode::PAYLOAD_TOO_LARGE,
                format!("the upload exceeds the {MAX_UPLOAD_SIZE}-byte limit"),
            ));
        }
    }
    let (tx, rx) = tokio::sync::mpsc::channel::<Result<bytes::Bytes, RestError>>(16);
    let writer = spawn_upload_writer(path.clone(), user.clone(), rx);
    let mut stream = std::pin::pin!(body.into_data_stream());
    {
        use futures::StreamExt as _;
        while let Some(chunk) = stream.next().await {
            match chunk {
                Ok(chunk) => {
                    if tx.send(Ok(chunk)).await.is_err() {
                        break;
                    }
                }
                Err(e) => {
                    let _ = tx
                        .send(Err(RestError::new(
                            StatusCode::BAD_REQUEST,
                            format!("error reading body: {e}"),
                        )))
                        .await;
                    break;
                }
            }
        }
    }
    drop(tx);
    writer.await.map_err(|e| {
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
pub(crate) fn parse_boundary(content_type: &str) -> Option<String> {
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
    // (upload_raw uses MAX_UPLOAD_SIZE); without this, multer defaults to
    // unlimited and bypasses axum's DefaultBodyLimit on a streamed body.
    let constraints = multer::Constraints::new().size_limit(
        multer::SizeLimit::new()
            .whole_stream(MAX_UPLOAD_SIZE as u64)
            .per_field(MAX_UPLOAD_SIZE as u64),
    );
    let mut multipart = multer::Multipart::with_constraints(stream, boundary, constraints);

    let mut entries = Vec::new();
    while let Some(mut field) = multipart.next_field().await.map_err(map_multipart_error)? {
        // Only parts carrying a filename are file uploads (part filename =
        // target path, matching upstream). A plain form field must not fall
        // back to the `?path` query target — that would let a stray text
        // field overwrite the real file's bytes. The `?path` fallback
        // belongs to the raw octet-stream path only.
        let Some(target) = field.file_name().map(|s| s.to_string()) else {
            continue;
        };
        let path = auth::resolve_path(&target, user);
        let (tx, rx) = tokio::sync::mpsc::channel::<Result<bytes::Bytes, RestError>>(16);
        let writer = spawn_upload_writer(path.clone(), user.clone(), rx);
        loop {
            match field.chunk().await {
                Ok(Some(chunk)) => {
                    if tx.send(Ok(chunk)).await.is_err() {
                        break;
                    }
                }
                Ok(None) => break,
                Err(e) => {
                    let _ = tx.send(Err(map_multipart_error(e))).await;
                    break;
                }
            }
        }
        drop(tx);
        writer.await.map_err(|e| {
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
            format!("multipart upload exceeds the {MAX_UPLOAD_SIZE}-byte limit",),
        )
    } else {
        RestError::new(
            StatusCode::BAD_REQUEST,
            format!("error reading multipart: {e}"),
        )
    }
}

pub(crate) fn entry_for(path: &str) -> UploadEntry {
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

/// Stream an upload into the target using the upstream in-place `O_TRUNC`
/// contract. The bounded channel keeps network reads backpressured against
/// disk writes without buffering the complete payload.
pub(crate) fn spawn_upload_writer(
    path: String,
    user: User,
    mut chunks: tokio::sync::mpsc::Receiver<Result<bytes::Bytes, RestError>>,
) -> tokio::task::JoinHandle<Result<(), RestError>> {
    tokio::task::spawn_blocking(move || {
        let target = std::path::Path::new(&path);
        if let Some(parent) = target.parent() {
            if !parent.exists() {
                create_dirs_owned(parent, &user)?;
            }
        }
        use std::os::unix::fs::OpenOptionsExt;
        let mut file = std::fs::OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o666)
            .open(target)
            .map_err(|e| map_write_error(&path, &e))?;
        let mut written = 0u64;
        while let Some(item) = chunks.blocking_recv() {
            let chunk = item?;
            written += chunk.len() as u64;
            if written > MAX_UPLOAD_SIZE as u64 {
                return Err(RestError::new(
                    StatusCode::PAYLOAD_TOO_LARGE,
                    format!("the upload exceeds the {MAX_UPLOAD_SIZE}-byte limit"),
                ));
            }
            file.write_all(&chunk)
                .map_err(|e| map_write_error(&path, &e))?;
        }
        chown(target, &user);
        Ok(())
    })
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
            let rc = libc::lchown(c_path.as_ptr(), user.uid, user.gid);
            if rc != 0 {
                // Silent failure would break the ownership contract: the
                // upload "succeeds" while the file stays daemon-owned.
                tracing::warn!(
                    "upload: lchown({}) to uid={} gid={} failed: {}",
                    path.display(),
                    user.uid,
                    user.gid,
                    std::io::Error::last_os_error()
                );
            }
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

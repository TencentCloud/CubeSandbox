// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! POST `/files`: raw and multipart upload handling.

use std::collections::HashMap;
use std::io::Write;

use axum::http::{HeaderMap, StatusCode};
use axum::response::IntoResponse;

use super::errors::{check_token_rest, resolve_request_user, MAX_UPLOAD_SIZE};
use crate::platform::config::Config;
use crate::platform::identity::{self, User};
use crate::protocol::RestError;

/// POST /files — multipart or raw octet-stream upload.
pub async fn upload(
    config: &Config,
    params: HashMap<String, String>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    if let Err(e) = check_token_rest(config, &headers) {
        return e.into_response();
    }
    let user = match resolve_request_user(config, &params, &headers) {
        Ok(u) => u,
        Err(e) => return e.into_response(),
    };

    // Byte-oriented like Go's Header.Get: an obs-text byte in the boundary
    // must not misroute a multipart request into the raw path.
    let content_type = headers
        .get(axum::http::header::CONTENT_TYPE)
        .map(|v| String::from_utf8_lossy(v.as_bytes()).into_owned())
        .unwrap_or_default();

    // Upstream dispatches on the parsed media type — lowercased, parameters
    // stripped: `application/octet-stream` is the raw path, any `multipart/*`
    // subtype is the multipart path, and everything else (a missing header and
    // `text/plain` included) is rejected *before* the body is read
    // (`upload.go` PostFiles switch). Matching only `multipart/form-data` and
    // treating every other type as raw bodies let a mistyped Content-Type
    // write the request body into the `?path` target.
    let result = match media_type(&content_type).as_deref() {
        Some("application/octet-stream") => {
            let content_length = headers
                .get(axum::http::header::CONTENT_LENGTH)
                .and_then(|v| v.to_str().ok())
                .and_then(|v| v.parse::<u64>().ok());
            upload_raw(body, &params, &user, content_length).await
        }
        Some(media) if media.starts_with("multipart/") => {
            upload_multipart(
                &content_type,
                body,
                &user,
                params.get("path").map(String::as_str),
            )
            .await
        }
        _ => Err(RestError::new(
            StatusCode::BAD_REQUEST,
            format!(
                "unsupported content type: {content_type}, expected multipart/form-data or application/octet-stream"
            ),
        )),
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
            // Upstream's wording for this shape (`upload.go` handleRawUpload).
            "path query parameter is required for raw body upload",
        ));
    };
    let path = identity::resolve_path(raw_path, user);
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

/// The part's `Content-Disposition`, parsed the way upstream reads it.
///
/// Go's `mime.ParseMediaType` lowercases the disposition type and every
/// *parameter name* while leaving values alone, and `Part.FormName()` returns
/// the empty string unless the type is exactly `form-data`. `multer`'s own
/// `name()`/`file_name()` compare parameter names case-sensitively, so a valid
/// `NAME="file"` part was skipped and the upload was silently dropped; parse the
/// header here instead.
fn part_disposition(field: &multer::Field<'_>) -> Option<mime::Mime> {
    let raw = field
        .headers()
        .get(axum::http::header::CONTENT_DISPOSITION)?;
    // A Content-Disposition value is a *bare* disposition (`form-data; …`), which
    // `mime` refuses to parse because it wants `type/subtype`. Prefix a throwaway
    // type so the real parser handles quoting, escapes and case-insensitive
    // parameter names, then compare the lowercased essence the way Go's
    // ParseMediaType + FormName() do.
    let parsed: mime::Mime = format!("text/{}", raw.to_str().ok()?).parse().ok()?;
    (parsed.essence_str() == "text/form-data").then_some(parsed)
}

/// A `Content-Disposition` parameter by (case-insensitive) name, accepting the
/// RFC 2231 `key*=charset'lang'value` form the way Go's parser does for the
/// usual `filename*=utf-8''name` shape.
fn disposition_param(parsed: &mime::Mime, key: &str) -> Option<String> {
    if let Some(value) = parsed.get_param(key) {
        return Some(value.as_str().to_string());
    }
    for (name, value) in parsed.params() {
        let name = name.as_str();
        if name.len() != key.len() + 1 || !name.ends_with('*') {
            continue;
        }
        if !name[..key.len()].eq_ignore_ascii_case(key) {
            continue;
        }
        let value = value.as_str();
        let encoded = match (value.find('\''), value.rfind('\'')) {
            (Some(first), Some(second)) if first < second => &value[second + 1..],
            _ => value,
        };
        return Some(percent_decode(encoded));
    }
    None
}

/// RFC 2231 values are percent-encoded; `%` not followed by two hex digits is
/// left as-is, like Go's `unescape` fallback.
fn percent_decode(value: &str) -> String {
    let bytes = value.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%' && i + 2 < bytes.len() {
            let hex = std::str::from_utf8(&bytes[i + 1..i + 3]).ok();
            if let Some(byte) = hex.and_then(|h| u8::from_str_radix(h, 16).ok()) {
                out.push(byte);
                i += 3;
                continue;
            }
        }
        out.push(bytes[i]);
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

/// The lowercased `type/subtype` of a Content-Type value, parameters stripped —
/// the shape Go's `mime.ParseMediaType` yields for upstream's dispatch switch.
/// `None` when the value carries no media type at all, which upstream then
/// rejects like any other unsupported type.
fn media_type(content_type: &str) -> Option<String> {
    let media = content_type.split(';').next().unwrap_or("").trim();
    (!media.is_empty()).then(|| media.to_ascii_lowercase())
}

async fn upload_multipart(
    content_type: &str,
    body: axum::body::Body,
    user: &User,
    query_path: Option<&str>,
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

    let mut entries: Vec<UploadEntry> = Vec::new();
    while let Some(mut field) = multipart.next_field().await.map_err(map_multipart_error)? {
        // Upstream treats a part as a file only when its *field name* is
        // exactly `file`: Go's `FormName()` returns the empty string unless the
        // disposition type is `form-data`, and returns the `name` parameter with
        // no default, so `handlePart` skips a part with no name, with a
        // different name, or with a non-`form-data` disposition — even when it
        // carries a filename. (`multer`'s accessors compare the parameter *name*
        // case-sensitively, so `NAME="file"` used to be dropped silently.)
        let Some(disposition) = part_disposition(&field) else {
            continue;
        };
        match disposition_param(&disposition, "name") {
            Some(name) if name == "file" => {}
            _ => continue,
        }
        // The `?path` query wins when it is present; the part's filename is
        // only the fallback, and it is used verbatim — path separators and
        // all, not `filepath.Base` (`upload.go` resolvePath).
        let target = match query_path {
            Some(path) => path.to_string(),
            None => match disposition_param(&disposition, "filename") {
                Some(name) => name,
                None => {
                    return Err(RestError::new(
                        StatusCode::BAD_REQUEST,
                        "error getting multipart custom part file name: filename not found in Content-Disposition header",
                    ))
                }
            },
        };
        let path = identity::resolve_path(&target, user);
        if entries.iter().any(|entry| entry.path == path) {
            // Upstream refuses a second write to the same path in one request
            // and keeps whatever the first part wrote.
            let others: Vec<&str> = entries
                .iter()
                .map(|entry| entry.path.as_str())
                .filter(|other| *other != path)
                .collect();
            let mut message = format!(
                "you cannot upload multiple files to the same path '{path}' in one upload request, only the first specified file was uploaded"
            );
            if others.len() > 1 {
                // `%v` of `strings.Join(alreadyUploaded, ", ")` — a bare,
                // comma-space separated list, no brackets.
                message.push_str(&format!(
                    ", also the following files were uploaded: {}",
                    others.join(", ")
                ));
            }
            return Err(RestError::new(StatusCode::BAD_REQUEST, message));
        }
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
    // Upstream answers 200 with an empty array when no part was a file part;
    // it does not treat that as a bad request.
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
        // Upstream stats the target before writing and rejects a directory as a
        // *caller* error (`processFile`: `path is a directory: <path>`, 400).
        // Without the pre-check `open` fails with EISDIR and map_write_error
        // turns a client mistake into a 500.
        if target.is_dir() {
            return Err(RestError::new(
                StatusCode::BAD_REQUEST,
                format!("path is a directory: {path}"),
            ));
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
            // chown (FOLLOWS symlinks), not lchown — matching upstream's
            // `os.Chown(path, uid, gid)` (upload.go:56/:84): an upload through
            // a symlink writes the link's destination, and ownership lands on
            // that same destination. lchown would leave a daemon-owned target
            // behind while chowning the link itself (caught by the
            // symlink probe). Following adds no takeover risk beyond the
            // write, which already followed the same link.
            let rc = libc::chown(c_path.as_ptr(), user.uid, user.gid);
            if rc != 0 {
                // Silent failure would break the ownership contract: the
                // upload "succeeds" while the file stays daemon-owned.
                tracing::warn!(
                    "upload: chown({}) to uid={} gid={} failed: {}",
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

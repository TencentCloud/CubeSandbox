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
//! - gzip response encoding is not implemented: responses are always
//!   identity (valid HTTP for any Accept-Encoding).

use std::collections::HashMap;
use std::io::Write;
use std::sync::Arc;

use axum::extract::{Query, State};
use axum::http::{HeaderMap, StatusCode};
use axum::response::IntoResponse;

use crate::auth::{self, User};
use crate::error::RestError;
use crate::rest::{content_disposition, encoding, httpdate, preconditions, ranges};
use crate::state::AppState;

/// Upload size cap for both upload paths (raw octet-stream and multipart).
///
/// Upstream envd's upload handler is unbounded (it streams to disk); in
/// production the cap is enforced by the proxy layer, which rejects bodies
/// over 256 MiB. Mirroring that external cap keeps the 64 MiB - 256 MiB
/// range functional (a proxy-passed 100 MiB upload must succeed here too).
/// Both upload paths stream straight to disk, so the cap is enforced by
/// counting bytes mid-stream — crossing it stops the write with 413 and the
/// partial content stays in the target, like an interrupted upstream upload;
/// the payload never sits in memory. Deliberately NOT
/// `connect::MAX_ENVELOPE_SIZE` (64 MiB): that constant bounds Connect
/// envelopes, not file uploads.
const MAX_UPLOAD_SIZE: usize = 256 * 1024 * 1024;

fn resolve_request_user(
    state: &AppState,
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
        // Upstream falls back to `defaults.User` (root until /init overrides it).
        .unwrap_or_else(|| state.default_user());
    auth::lookup_user(&name).map_err(|msg| RestError::new(StatusCode::UNAUTHORIZED, msg))
}

fn check_token_rest(state: &AppState, headers: &HeaderMap) -> Result<(), RestError> {
    super::check_token(state, headers)
        .map_err(|_| RestError::new(StatusCode::UNAUTHORIZED, "invalid access token".to_string()))
}

/// GET /files — stream a file back with upstream `http.ServeContent`
/// semantics: Last-Modified, conditional requests (If-Match / If-Unmodified-
/// Since / If-None-Match / If-Modified-Since / If-Range → 304/412), Range
/// (single range → 206, unsatisfiable → 416), Accept-Ranges, and the two 406
/// Accept-Encoding exits — in upstream download.go's exact order. The stages
/// below are extracted as helpers so each mirrors one upstream step; the
/// order of the calls *is* the pipeline order.
pub async fn download(
    State(state): State<Arc<AppState>>,
    Query(params): Query<HashMap<String, String>>,
    headers: HeaderMap,
) -> axum::response::Response {
    // Stage 1: token → user → path → stat/isdir (error order unchanged).
    let ResolvedFile { path, meta } = match resolve_download(&state, &params, &headers).await {
        Ok(r) => r,
        Err(resp) => return resp,
    };
    // Stage 2: Accept-Encoding gates (parse 406 before Vary is set, identity
    // gate after it — upstream's order).
    if let Err(resp) = gate_accept_encoding(&headers) {
        return resp;
    }
    // Stage 3: open once; the handle is reused for sniffing, seek and stream.
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
    // Stage 4: preset headers (Vary / Content-Disposition / Last-Modified)
    // and the modtime the conditional steps key off.
    let preset = Preset::new(&path, &meta);
    // Stage 5: conditional requests → 304/412. Runs before the Content-Type
    // probe so those answers carry no Content-Type, exactly like upstream.
    if let Some(resp) = preset.condition_response(&headers) {
        return resp;
    }
    // Stage 6: Content-Type sniff (approximates Go's DetectContentType); a
    // failed rewind reopens the file so the body still starts at byte 0.
    // Known difference, left deliberately: fs.go's sniff path 500s on a
    // rewind failure, we reopen and continue — unreachable for practical
    // files, and a genuinely non-seekable handle is caught by the Seek(End)
    // in stage 6b below anyway. Do not "fix" this toward Go.
    let content_type = match sniff_content_type_or_reopen(&mut file, &path).await {
        Ok(ct) => ct,
        Err(resp) => return resp,
    };
    // Stage 6b: response size from the open handle — Go's sizeFunc is
    // Seek(End) + Seek(Start), NOT the earlier path stat (fs.go serveContent
    // via ServeContent's sizeFunc). This makes /dev/zero-style devices
    // (SeekEnd = 0) answer Content-Length: 0 with an empty body instead of
    // streaming forever, and a non-seekable file (/proc/* reports EINVAL on
    // SeekEnd) fail exactly like Go's errSeeker path. It also closes the
    // stat-vs-handle size race: only the modtime still comes from the path
    // stat, like download.go.
    use tokio::io::AsyncSeekExt;
    // Either seek failing is Go's errSeeker → serveError(500, "seeker can't
    // seek") — including the (unreachable on Linux) case of Seek(End)
    // succeeding but Seek(Start) failing. Never panic here: this is the
    // request path's only non-builder expect, and the panic handler answers
    // a JSON shape, not Go's text/plain serveError.
    let seek_end = file.seek(std::io::SeekFrom::End(0)).await;
    let size = match seek_end {
        Ok(s) => s,
        Err(_) => {
            // fs.go: serveError(w, "seeker can't seek", 500) — plain_error
            // is that exact shape (Vary/Disposition kept, Last-Modified
            // stripped, text/plain + nosniff, "text\n").
            return preset.plain_error(
                StatusCode::INTERNAL_SERVER_ERROR,
                None,
                "seeker can't seek\n".to_string(),
            );
        }
    };
    if file.seek(std::io::SeekFrom::Start(0)).await.is_err() {
        return preset.plain_error(
            StatusCode::INTERNAL_SERVER_ERROR,
            None,
            "seeker can't seek\n".to_string(),
        );
    }
    // Stage 7: Range dispatch — If-Range gate, parse, single-range seek.
    let single = match plan_range(&mut file, &headers, &preset, size).await {
        Ok(s) => s,
        Err(resp) => return resp,
    };
    // Stage 8: assemble the success response (200 full / 206 range).
    // Both arms stream exactly their send size: Go io.CopyN(sendSize) reads
    // nothing for size 0 (/dev/zero → CL: 0, empty body) and never overruns
    // the Seek(End) size.
    let (code, stream_limit) = match &single {
        Some(r) => (StatusCode::PARTIAL_CONTENT, Some(r.length as u64)),
        None => (StatusCode::OK, Some(size)),
    };

    // Common success headers: Content-Type always; Content-Range only on
    // 206; Accept-Ranges always; Content-Length always on 200 — Go's
    // serveContent sets it unconditionally for identity responses, so a
    // size-0 answer (empty file, /dev/zero) carries an explicit CL: 0.
    let mut b = axum::response::Response::builder().status(code);
    b = preset.apply(b);
    b = b.header(axum::http::header::CONTENT_TYPE, content_type);
    if let Some(r) = single {
        b = b.header(
            axum::http::header::CONTENT_RANGE,
            r.content_range(size as i64),
        );
    }
    b = b.header(axum::http::header::ACCEPT_RANGES, "bytes");
    let body_len = match single {
        Some(r) => Some(r.length as u64),
        None => Some(size),
    };
    if let Some(len) = body_len {
        b = b.header(axum::http::header::CONTENT_LENGTH, len);
    }
    let stream = reader_stream(file, stream_limit);
    b.body(axum::body::Body::from_stream(stream))
        .expect("build download response")
}

/// Stage 1 result: the request resolved to a path whose stat succeeded and
/// which is not a directory.
struct ResolvedFile {
    path: String,
    meta: std::fs::Metadata,
}

/// Stage 1 — token → user → path → stat. Mirrors download.go's order; every
/// error exit keeps its baseline status and message.
#[allow(clippy::result_large_err)] // axum helpers propagate prebuilt responses, not an error type
async fn resolve_download(
    state: &Arc<AppState>,
    params: &HashMap<String, String>,
    headers: &HeaderMap,
) -> Result<ResolvedFile, axum::response::Response> {
    if let Err(e) = check_token_rest(state, headers) {
        return Err(e.into_response());
    }
    let user = match resolve_request_user(state, params, headers) {
        Ok(u) => u,
        Err(e) => return Err(e.into_response()),
    };
    // Baseline: a missing `path` parameter falls back to the user's home
    // directory (which then fails with the "is a directory" error). A
    // `/init`-configured defaultWorkdir takes that slot instead — upstream
    // resolves an empty path through ResolveDefaultWorkdir first
    // (`execcontext/context.go:15`) and only then anchors it at the home dir.
    let raw_path = params
        .get("path")
        .filter(|p| !p.is_empty())
        .cloned()
        .or_else(|| state.default_workdir())
        .unwrap_or_else(|| user.home.clone());
    let path = auth::resolve_path(&raw_path, &user);

    let meta = match tokio::fs::metadata(&path).await {
        Ok(m) => m,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            return Err(RestError::new(
                StatusCode::NOT_FOUND,
                format!("path '{path}' does not exist"),
            )
            .into_response());
        }
        Err(e) if e.kind() == std::io::ErrorKind::PermissionDenied => {
            return Err(RestError::new(
                StatusCode::INTERNAL_SERVER_ERROR,
                format!("error opening file '{path}': permission denied"),
            )
            .into_response());
        }
        Err(e) => {
            return Err(RestError::new(
                StatusCode::INTERNAL_SERVER_ERROR,
                format!("error opening file '{path}': {e}"),
            )
            .into_response());
        }
    };
    if meta.is_dir() {
        return Err(RestError::new(
            StatusCode::BAD_REQUEST,
            format!("path '{path}' is a directory"),
        )
        .into_response());
    }
    Ok(ResolvedFile { path, meta })
}

/// Stage 2 — the two Accept-Encoding 406 exits, in upstream order: the parse
/// failure answers before `Vary` is set; the Range/conditional identity gate
/// answers after it and therefore carries `Vary` itself (nginx's `add_header
/// Vary` has no `always`, so error responses must carry it when upstream
/// does).
#[allow(clippy::result_large_err)] // axum helpers propagate prebuilt responses, not an error type
fn gate_accept_encoding(headers: &HeaderMap) -> Result<(), axum::response::Response> {
    let ae_value = header_str(headers, axum::http::header::ACCEPT_ENCODING);
    let ae = ae_value.as_deref().unwrap_or("");
    if encoding::parse_accept_encoding(ae).is_err() {
        return Err(RestError::new(
            StatusCode::NOT_ACCEPTABLE,
            "error parsing Accept-Encoding: no acceptable encoding found, supported: [gzip]",
        )
        .into_response());
    }
    // cube-envd serves identity only: the parsed best encoding is
    // deliberately unused, but the rejection above must stay.
    let has_range_or_conditional = [
        axum::http::header::RANGE,
        axum::http::header::IF_MODIFIED_SINCE,
        axum::http::header::IF_NONE_MATCH,
        axum::http::header::IF_RANGE,
    ]
    .iter()
    .any(|h| {
        header_str(headers, h.clone())
            .map(|v| !v.is_empty())
            .unwrap_or(false)
    });
    if has_range_or_conditional && !encoding::is_identity_acceptable(ae) {
        // This 406 answers AFTER Vary was set, so it must carry Vary itself
        // (see fn doc). Body shape matches RestError / upstream jsonError.
        let body = serde_json::json!({
            "code": 406,
            "message": "identity encoding not acceptable for Range or conditional request",
        })
        .to_string();
        return Err(axum::response::Response::builder()
            .status(StatusCode::NOT_ACCEPTABLE)
            .header(axum::http::header::VARY, "Accept-Encoding")
            .header(
                axum::http::header::CONTENT_TYPE,
                "application/json; charset=utf-8",
            )
            .body(axum::body::Body::from(body))
            .expect("build 406 response"));
    }
    Ok(())
}

/// The header set shared by every download answer: Vary + Content-Disposition
/// (+ Last-Modified when the mtime is meaningful), plus the variant that
/// drops Last-Modified for 416 (fs.go serveError).
struct Preset {
    /// fs.go `checkPreconditions`/`setLastModified` view of the mtime:
    /// `None` = `isZeroTime` (exactly the epoch, or unavailable) — no
    /// Last-Modified and the date-based conditions do not apply.
    modtime: Option<i64>,
    /// fs.go `checkIfRange` view: no zero-time gate there, only
    /// `t.Unix() == modtime.Unix()` — so this is the floor seconds whenever
    /// the mtime is known at all (epoch included).
    if_range_secs: Option<i64>,
    headers: Vec<(axum::http::HeaderName, String)>,
    no_last_modified: Vec<(axum::http::HeaderName, String)>,
}

/// fs.go's mtime semantics (net/http): the raw mtime is compared against the
/// exact epoch (`isZeroTime`) *before* any truncation, so a sub-second mtime
/// near 1970 is not "no time" — Last-Modified prints its floor second and
/// the date conditions compare the floor (`Truncate(time.Second)`, which
/// floors). A pre-epoch mtime is kept too: Go `Truncate` floors on the
/// absolute timeline, so -1.5s lands on -2s. Only an exactly-epoch (or
/// unreportable) mtime means "no time".
fn modtime_of(res: std::io::Result<std::time::SystemTime>) -> (Option<i64>, Option<i64>) {
    let Ok(t) = res else {
        // Go: a failed ModTime() is the zero Time → isZeroTime.
        return (None, None);
    };
    let (secs, is_exact_epoch) = match t.duration_since(std::time::UNIX_EPOCH) {
        Ok(d) => (d.as_secs() as i64, d.is_zero()),
        Err(e) => {
            // Pre-epoch: duration_since yields the distance back to the
            // epoch; floor on the epoch axis (Go Truncate floors).
            let d = e.duration();
            let mut s = -(d.as_secs() as i64);
            if d.subsec_nanos() > 0 {
                s -= 1;
            }
            (s, false)
        }
    };
    let precondition_secs = if is_exact_epoch { None } else { Some(secs) };
    (precondition_secs, Some(secs))
}

impl Preset {
    /// Upstream sets Content-Disposition before http.ServeContent, which then
    /// adds Last-Modified from the stat mtime (unless isZeroTime).
    fn new(path: &str, meta: &std::fs::Metadata) -> Preset {
        let disposition = content_disposition::format_content_disposition(
            std::path::Path::new(path)
                .file_name()
                .and_then(|n| n.to_str())
                .unwrap_or("download"),
        );
        let (modtime, if_range_secs) = modtime_of(meta.modified());
        let mut headers = Vec::with_capacity(3);
        headers.push((axum::http::header::VARY, "Accept-Encoding".to_string()));
        headers.push((axum::http::header::CONTENT_DISPOSITION, disposition));
        if let Some(mt) = modtime {
            headers.push((
                axum::http::header::LAST_MODIFIED,
                httpdate::format_http_date(mt),
            ));
        }
        let no_last_modified = headers
            .iter()
            .filter(|(k, _)| *k != axum::http::header::LAST_MODIFIED)
            .cloned()
            .collect();
        Preset {
            modtime,
            if_range_secs,
            headers,
            no_last_modified,
        }
    }

    fn apply(&self, mut b: axum::http::response::Builder) -> axum::http::response::Builder {
        for (k, v) in &self.headers {
            b = b.header(k.clone(), v.clone());
        }
        b
    }

    fn apply_without_last_modified(
        &self,
        mut b: axum::http::response::Builder,
    ) -> axum::http::response::Builder {
        for (k, v) in &self.no_last_modified {
            b = b.header(k.clone(), v.clone());
        }
        b
    }

    /// Stage 5 — fs.go checkPreconditions: 304 (writeNotModified header set)
    /// or 412 (bare) when a condition fires, else the download continues.
    fn condition_response(&self, headers: &HeaderMap) -> Option<axum::response::Response> {
        use preconditions::CondOutcome;
        match preconditions::check_preconditions(
            header_str(headers, axum::http::header::IF_MATCH).as_deref(),
            header_str(headers, axum::http::header::IF_UNMODIFIED_SINCE).as_deref(),
            header_str(headers, axum::http::header::IF_NONE_MATCH).as_deref(),
            header_str(headers, axum::http::header::IF_MODIFIED_SINCE).as_deref(),
            self.modtime,
        ) {
            CondOutcome::NotModified => {
                // fs.go writeNotModified: strips Content-Type/Content-Length/
                // Content-Encoding (none set yet here) and keeps Last-Modified
                // (no ETag).
                Some(self.empty(StatusCode::NOT_MODIFIED))
            }
            CondOutcome::PreconditionFailed => {
                // fs.go: bare 412, no body, no Content-Type.
                Some(self.empty(StatusCode::PRECONDITION_FAILED))
            }
            CondOutcome::Serve => None,
        }
    }

    /// Empty-bodied 304/412 answer carrying the preset headers.
    fn empty(&self, code: StatusCode) -> axum::response::Response {
        self.apply(axum::response::Response::builder().status(code))
            .body(axum::body::Body::empty())
            .expect("build error response")
    }

    /// fs.go serveError shape: text/plain + nosniff, optional Content-Range,
    /// explicit Content-Length, Last-Modified stripped (416 and the 206 seek
    /// failure both answer this way).
    fn plain_error(
        &self,
        code: StatusCode,
        content_range: Option<String>,
        body: String,
    ) -> axum::response::Response {
        let mut b =
            self.apply_without_last_modified(axum::response::Response::builder().status(code));
        if let Some(cr) = content_range {
            b = b.header(axum::http::header::CONTENT_RANGE, cr);
        }
        b.header(
            axum::http::header::CONTENT_TYPE,
            "text/plain; charset=utf-8",
        )
        .header(axum::http::header::X_CONTENT_TYPE_OPTIONS, "nosniff")
        .header(axum::http::header::CONTENT_LENGTH, body.len())
        .body(axum::body::Body::from(body))
        .expect("build plain error response")
    }
}

/// Stage 6 — approximate Go's DetectContentType with a text/binary split
/// (the full table is a non-goal). A failed rewind means a non-seekable
/// special file: reopen so the stream still starts at byte 0.
#[allow(clippy::result_large_err)] // axum helpers propagate prebuilt responses, not an error type
async fn sniff_content_type_or_reopen(
    file: &mut tokio::fs::File,
    path: &str,
) -> Result<&'static str, axum::response::Response> {
    match sniff_content_type(file).await {
        Ok(ct) => Ok(ct),
        Err(_) => match tokio::fs::File::open(path).await {
            Ok(f) => {
                *file = f;
                Ok("application/octet-stream")
            }
            Err(e) => Err(RestError::new(
                StatusCode::INTERNAL_SERVER_ERROR,
                format!("error opening file '{path}': {e}"),
            )
            .into_response()),
        },
    }
}

/// Stage 7 — fs.go serveContent Range dispatch: an If-Range that fails drops
/// the Range header (full 200); a single range is seeked for the 206; any
/// multi-range request is answered with the full file; the 416 shapes and the
/// seek-error 416 are emitted here.
#[allow(clippy::result_large_err)] // axum helpers propagate prebuilt responses, not an error type
async fn plan_range(
    file: &mut tokio::fs::File,
    headers: &HeaderMap,
    preset: &Preset,
    size: u64,
) -> Result<Option<ranges::ByteRange>, axum::response::Response> {
    let range_value = header_str(headers, axum::http::header::RANGE);
    let range_hdr = range_value.as_deref();
    let range_kept = range_hdr.is_some_and(|_| {
        preconditions::if_range_keeps_range(
            header_str(headers, axum::http::header::IF_RANGE).as_deref(),
            preset.if_range_secs,
        )
    });
    let ranges = if range_kept {
        ranges::parse_range(range_hdr, size as i64)
    } else {
        Ok(None)
    };
    let single = match ranges {
        Ok(ranges) => {
            // A single-range request is served as 206; any multi-range
            // request is answered with the full file instead.
            if ranges.as_ref().is_some_and(|r| r.len() == 1) {
                ranges.unwrap().pop()
            } else {
                None
            }
        }
        // errNoOverlap with an empty file is ignored (fs.go: some clients
        // always send Range; answer 200 rather than 416).
        Err(ranges::RangeError::NoOverlap) if size == 0 => None,
        Err(e) => {
            let cr = match e {
                ranges::RangeError::NoOverlap => Some(format!("bytes */{size}")),
                ranges::RangeError::Invalid => None,
            };
            let body = format!("{}\n", e.message());
            return Err(preset.plain_error(StatusCode::RANGE_NOT_SATISFIABLE, cr, body));
        }
    };
    if let Some(r) = single {
        use tokio::io::AsyncSeekExt;
        if let Err(e) = file.seek(std::io::SeekFrom::Start(r.start as u64)).await {
            let body = format!("{e}\n");
            return Err(preset.plain_error(StatusCode::RANGE_NOT_SATISFIABLE, None, body));
        }
    }
    Ok(single)
}

/// Header value as text. Go's `Header.Get` is byte-oriented and feeds raw
/// bytes to the parsers, where non-ASCII is just garbage — and garbage (a
/// present value) deliberately has different outcomes from absent here
/// (scan break vs. condNone, 416 vs. 200, 406 vs. pass). hyper accepts
/// obs-text (0x80-0xFF) in header values, so `to_str()`-based extraction
/// would silently reclassify garbage as "absent"; lossy UTF-8 keeps the
/// value present and maps every invalid byte to U+FFFD, which every parser
/// below treats exactly like Go treats the raw byte (an ETag tag-char that
/// never terminates, a name/literal/digit mismatch in dates and ranges, a
/// plain token in Accept-Encoding).
fn header_str(
    headers: &HeaderMap,
    name: axum::http::HeaderName,
) -> Option<std::borrow::Cow<'_, str>> {
    headers
        .get(name)
        .map(|v| String::from_utf8_lossy(v.as_bytes()))
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

/// Chunked reader stream (64 KiB) without pulling in tokio-util. `limit`
/// bounds the total bytes produced (single-range 206 bodies); `None` streams
/// to EOF.
///
/// Deliberately NOT a blocking-task pump: a measured alternative (channel +
/// dedicated reader task, 256 KiB chunks) peaked at ~570-670 MiB/s on
/// loopback while this unfold version sustains ~870-900 MiB/s — the
/// per-read spawn_blocking dispatch overlaps with IO instead of serializing
/// behind a cross-task handoff. Measured, not assumed (PR-C, 2026-09-09).
fn reader_stream(
    file: tokio::fs::File,
    limit: Option<u64>,
) -> impl futures::Stream<Item = Result<bytes::Bytes, std::io::Error>> + Send {
    use tokio::io::AsyncReadExt;
    futures::stream::unfold((file, limit), |(mut file, mut remaining)| async move {
        let want = match remaining {
            Some(0) => return None,
            Some(r) => r.min(64 * 1024) as usize,
            None => 64 * 1024,
        };
        let mut buf = vec![0u8; want];
        match file.read(&mut buf).await {
            Ok(0) => None,
            Ok(n) => {
                buf.truncate(n);
                if let Some(r) = remaining.as_mut() {
                    *r -= n as u64;
                }
                Some((Ok(bytes::Bytes::from(buf)), (file, remaining)))
            }
            Err(e) => Some((Err(e), (file, remaining))),
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
    // the cap is rejected without reading the body.
    if let Some(len) = content_length {
        if len > MAX_UPLOAD_SIZE as u64 {
            return Err(RestError::new(
                StatusCode::PAYLOAD_TOO_LARGE,
                format!("the upload exceeds the {MAX_UPLOAD_SIZE}-byte limit"),
            ));
        }
    }
    // Stream the body into the in-place writer through a bounded channel:
    // network-paced reads backpressure against disk-paced writes, and the
    // payload never sits in memory (one blocking crossing per upload).
    let (tx, rx) = tokio::sync::mpsc::channel::<Result<bytes::Bytes, RestError>>(16);
    let writer = spawn_upload_writer(path.clone(), user.clone(), rx);
    let mut stream = std::pin::pin!(body.into_data_stream());
    {
        use futures::StreamExt as _;
        while let Some(chunk) = stream.next().await {
            match chunk {
                Ok(c) => {
                    if tx.send(Ok(c)).await.is_err() {
                        // The writer finished early (413/write error) — its
                        // result is the authoritative outcome.
                        break;
                    }
                }
                Err(e) => {
                    // Forward as the writer's terminal error; the partial
                    // content stays in the target, like upstream's io.Copy
                    // failure mid-stream.
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
        // Stream the part chunk-by-chunk into the in-place writer; multer's
        // whole-stream/per-field limits keep counting as data flows
        // (StreamSizeExceeded still maps to 413). Fields are handled
        // serially — each writer finishes before the next field starts — to
        // preserve the entries order and error propagation, like upstream's
        // part loop.
        let (tx, rx) = tokio::sync::mpsc::channel::<Result<bytes::Bytes, RestError>>(16);
        let writer = spawn_upload_writer(path.clone(), user.clone(), rx);
        // Stream the part chunk-by-chunk into the in-place writer; multer's
        // whole-stream/per-field limits keep counting as data flows
        // (StreamSizeExceeded still maps to 413). Fields are handled
        // serially — each writer finishes before the next field starts — to
        // preserve the entries order and error propagation, like upstream's
        // part loop.
        //
        // A part read error is forwarded as the writer's terminal error
        // (like the raw path) instead of returning early: the writer is
        // ALWAYS awaited below, so its result — a mid-stream write error,
        // ENOSPC, or this parser error, whichever lands first — is never
        // discarded and the file is finalized before the response goes out.
        loop {
            match field.chunk().await {
                Ok(Some(chunk)) => {
                    if tx.send(Ok(chunk)).await.is_err() {
                        break; // writer finished early (413/write error) — its result wins
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

/// Streaming upload sink: owns the target file and pulls chunks through a
/// bounded channel — network-paced reads backpressure against disk-paced
/// writes, one blocking crossing per upload. The write is **in-place
/// `O_TRUNC` at the target path**, matching upstream `upload.go:68`
/// (`os.OpenFile(path, O_WRONLY|O_CREATE|O_TRUNC, 0o666)`): no temp file, no
/// rename, partial content visible on interruption (upstream contract),
/// symlinks followed (upstream contract), and an existing file's mode
/// untouched by `O_TRUNC`.
///
/// A chunk arriving after the size cap is exceeded stops the write and
/// returns 413 — the partial content stays in the target, like an in-place
/// upload that dies mid-stream. A body-read failure (sent as the `Err` arm)
/// behaves the same way, like upstream's io.Copy failing mid-copy.
fn spawn_upload_writer(
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
        // mode 0o666 with umask applied, exactly like Go's OpenFile — a
        // fresh file gets default create permissions, an existing file's
        // mode is untouched by O_TRUNC.
        use std::os::unix::fs::OpenOptionsExt;
        let mut f = std::fs::OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o666)
            .open(target)
            .map_err(|e| map_write_error(&path, &e))?;
        let mut written: u64 = 0;
        while let Some(item) = chunks.blocking_recv() {
            let chunk = item?;
            written += chunk.len() as u64;
            if written > MAX_UPLOAD_SIZE as u64 {
                return Err(RestError::new(
                    StatusCode::PAYLOAD_TOO_LARGE,
                    format!("the upload exceeds the {MAX_UPLOAD_SIZE}-byte limit"),
                ));
            }
            f.write_all(&chunk)
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
            // behind while chowning the link itself (caught by the PR-C
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;

    fn test_user(home: &str) -> User {
        User {
            name: "test".into(),
            uid: unsafe { libc::getuid() },
            gid: unsafe { libc::getgid() },
            home: home.into(),
            groups: vec![],
        }
    }

    #[tokio::test]
    async fn upload_writer_creates_parents_and_writes_all_chunks() {
        let dir = tempfile::tempdir().unwrap();
        let user = test_user(dir.path().to_str().unwrap());
        let target = dir.path().join("a/b/c.txt");
        let (tx, rx) = tokio::sync::mpsc::channel(4);
        let jh = spawn_upload_writer(target.to_str().unwrap().to_string(), user.clone(), rx);
        // Chunk boundaries must not affect the content.
        for part in [&b"hel"[..], &b"lo"[..]] {
            tx.send(Ok(bytes::Bytes::copy_from_slice(part)))
                .await
                .unwrap();
        }
        drop(tx);
        jh.await.unwrap().unwrap();
        assert_eq!(std::fs::read(&target).unwrap(), b"hello");
        // In-place write: no `.cube-envd-upload` temp files ever exist.
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

    #[tokio::test]
    async fn overwrite_keeps_mode_bits_automatically() {
        let dir = tempfile::tempdir().unwrap();
        let user = test_user(dir.path().to_str().unwrap());
        let target = dir.path().join("script.sh");
        // O_TRUNC never touches the mode of an existing file, so an
        // executable script keeps its x bits across an overwrite — the
        // upstream behavior the old mode-copy logic worked around.
        std::fs::write(&target, b"#!/bin/sh\n").unwrap();
        std::fs::set_permissions(&target, std::fs::Permissions::from_mode(0o755)).unwrap();
        let (tx, rx) = tokio::sync::mpsc::channel(4);
        let jh = spawn_upload_writer(target.to_str().unwrap().to_string(), user.clone(), rx);
        tx.send(Ok(bytes::Bytes::from_static(b"#!/bin/sh\necho v2\n")))
            .await
            .unwrap();
        drop(tx);
        jh.await.unwrap().unwrap();
        let mode = std::fs::metadata(&target).unwrap().permissions().mode() & 0o7777;
        assert_eq!(mode, 0o755);
        // A fresh file gets 0o666 & ~umask — no x bits.
        let fresh = dir.path().join("plain.txt");
        let (tx, rx) = tokio::sync::mpsc::channel(4);
        let jh = spawn_upload_writer(fresh.to_str().unwrap().to_string(), user.clone(), rx);
        tx.send(Ok(bytes::Bytes::from_static(b"x"))).await.unwrap();
        drop(tx);
        jh.await.unwrap().unwrap();
        assert!(std::fs::metadata(&fresh).unwrap().permissions().mode() & 0o111 == 0);
    }

    #[tokio::test]
    async fn cap_exceeded_mid_stream_reports_413_and_keeps_partial_content() {
        let dir = tempfile::tempdir().unwrap();
        let user = test_user(dir.path().to_str().unwrap());
        let target = dir.path().join("cap.bin");
        // Two halves: the second chunk crosses the line and stops the write
        // before any of it lands — interrupted-write semantics (upstream has
        // no cap of its own; this is our documented 413 behavior).
        let half = MAX_UPLOAD_SIZE / 2 + 1;
        let (tx, rx) = tokio::sync::mpsc::channel(4);
        let jh = spawn_upload_writer(target.to_str().unwrap().to_string(), user.clone(), rx);
        tx.send(Ok(bytes::Bytes::from(vec![b'a'; half])))
            .await
            .unwrap();
        tx.send(Ok(bytes::Bytes::from(vec![b'b'; half])))
            .await
            .unwrap();
        drop(tx);
        let err = jh.await.unwrap().unwrap_err();
        assert_eq!(err.status, StatusCode::PAYLOAD_TOO_LARGE);
        // The partial content (everything before the cap) stays in the
        // target — in-place, like an interrupted upstream upload.
        let content = std::fs::read(&target).unwrap();
        assert_eq!(content.len(), half);
        assert!(content.iter().all(|&b| b == b'a'));
    }

    #[tokio::test]
    async fn multipart_read_error_awaits_writer_and_propagates() {
        // PR-C review, High: a body read failure while a field's writer is
        // ACTIVE must be forwarded as that writer's terminal error — the
        // writer is awaited before the handler returns, so its result is
        // never discarded and the partial content is finalized before the
        // error response goes out.
        //
        // The stream paces its items with sleeps: a ready in-memory stream
        // would be drained eagerly by multer's next_field (the error would
        // surface before any writer exists), while the sleeps make each
        // stage surface separately — head → next_field returns the field →
        // data chunk (writer writes) → read error (forwarded to the writer).
        let dir = tempfile::tempdir().unwrap();
        let target = dir.path().join("part.bin");
        let head = format!(
            "--X\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{}\"\r\n\
             Content-Type: application/octet-stream\r\n\r\n",
            target.to_str().unwrap()
        );
        let stream = futures::stream::unfold((0u8, Some(head)), |(step, head)| async move {
            tokio::time::sleep(std::time::Duration::from_millis(120)).await;
            match step {
                0 => Some((
                    Ok::<bytes::Bytes, std::io::Error>(bytes::Bytes::from(head.unwrap())),
                    (1u8, None),
                )),
                1 => Some((Ok(bytes::Bytes::from_static(b"more")), (2u8, None))),
                _ => Some((Err(std::io::Error::other("body died")), (3u8, None))),
            }
        });
        let mut headers = HeaderMap::new();
        headers.insert(
            axum::http::header::CONTENT_TYPE,
            "multipart/form-data; boundary=X".parse().unwrap(),
        );
        let resp = upload(
            State(std::sync::Arc::new(AppState::new())),
            Query(HashMap::new()),
            headers,
            axum::body::Body::from_stream(stream),
        )
        .await;
        let (parts, body) = resp.into_parts();
        let db = axum::body::to_bytes(body, 1 << 20).await.unwrap();
        assert!(String::from_utf8_lossy(&db).contains("error reading multipart"));
        assert_eq!(parts.status, StatusCode::BAD_REQUEST);
        // The partial content written before the failure is finalized — the
        // writer completed before the error response went out.
        assert_eq!(std::fs::read(&target).unwrap(), b"more");
    }

    #[tokio::test]
    async fn upload_through_symlink_writes_target_and_leaves_link() {
        // In-place write follows a symlink (like upstream os.OpenFile), and
        // chown follows too (like upstream os.Chown): content lands on the
        // link's destination and the link itself survives. The ownership
        // differential (daemon-owned target without the fix) needs a
        // root-owned pre-existing target to observe — covered by the
        // container probe recorded in RESULTS.md.
        let dir = tempfile::tempdir().unwrap();
        let user = test_user(dir.path().to_str().unwrap());
        let real = dir.path().join("real.bin");
        std::fs::write(&real, b"old").unwrap();
        std::os::unix::fs::symlink(&real, dir.path().join("lnk.bin")).unwrap();
        let (tx, rx) = tokio::sync::mpsc::channel(4);
        let jh = spawn_upload_writer(
            dir.path().join("lnk.bin").to_str().unwrap().to_string(),
            user.clone(),
            rx,
        );
        tx.send(Ok(bytes::Bytes::from_static(b"new")))
            .await
            .unwrap();
        drop(tx);
        jh.await.unwrap().unwrap();
        assert_eq!(std::fs::read(&real).unwrap(), b"new");
        let link_meta = std::fs::symlink_metadata(dir.path().join("lnk.bin")).unwrap();
        assert!(link_meta.file_type().is_symlink());
    }

    #[tokio::test]
    async fn body_read_error_keeps_partial_content_and_propagates() {
        let dir = tempfile::tempdir().unwrap();
        let user = test_user(dir.path().to_str().unwrap());
        let target = dir.path().join("err.bin");
        let (tx, rx) = tokio::sync::mpsc::channel(4);
        let jh = spawn_upload_writer(target.to_str().unwrap().to_string(), user.clone(), rx);
        tx.send(Ok(bytes::Bytes::from_static(b"partial")))
            .await
            .unwrap();
        // A body-read failure arrives as the terminal Err arm: the writer
        // stops, leaves the partial content (upstream io.Copy semantics),
        // and the handler surfaces the error.
        tx.send(Err(RestError::new(
            StatusCode::BAD_REQUEST,
            "error reading body: boom",
        )))
        .await
        .unwrap();
        drop(tx);
        let err = jh.await.unwrap().unwrap_err();
        assert_eq!(err.status, StatusCode::BAD_REQUEST);
        assert_eq!(std::fs::read(&target).unwrap(), b"partial");
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

#[cfg(test)]
mod download_tests {
    use super::*;
    use axum::body::to_bytes;
    use axum::extract::Query;
    use axum::http::header;
    use std::collections::HashMap;

    /// Drive the real GET /files handler against a tempdir file. The default
    /// user is root (present in /etc/passwd everywhere this suite runs); an
    /// absolute `path` query bypasses home anchoring. AppState has no access
    /// token set, so the token gate passes.
    async fn get(path: &str, extra: &[(&str, &str)]) -> axum::response::Response {
        let state = State(std::sync::Arc::new(AppState::new()));
        let mut params = HashMap::new();
        params.insert("path".to_string(), path.to_string());
        let mut headers = HeaderMap::new();
        for (k, v) in extra {
            headers.insert(k.parse::<header::HeaderName>().unwrap(), v.parse().unwrap());
        }
        download(state, Query(params), headers).await
    }

    async fn body(
        resp: axum::response::Response,
    ) -> (axum::http::StatusCode, axum::http::HeaderMap, Vec<u8>) {
        let (parts, b) = resp.into_parts();
        let bytes = to_bytes(b, 1 << 20).await.unwrap();
        (parts.status, parts.headers, bytes.to_vec())
    }

    /// 406 responses use the REST error shape (JSON code+message, like
    /// upstream jsonError) — the human message rides inside `message`.
    fn json_message(body: &[u8]) -> String {
        let v: serde_json::Value = serde_json::from_slice(body).unwrap();
        v["message"].as_str().unwrap().to_string()
    }

    fn tmp_file(name: &str, content: &[u8]) -> (tempfile::TempDir, String) {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join(name);
        std::fs::write(&p, content).unwrap();
        (dir, p.to_str().unwrap().to_string())
    }

    #[tokio::test]
    async fn plain_get_has_negotiation_headers() {
        let (_d, p) = tmp_file("base_a.txt", b"hello-octet\n");
        let resp = get(&p, &[]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 200);
        assert_eq!(&b[..], b"hello-octet\n");
        assert_eq!(h[header::VARY], "Accept-Encoding");
        assert_eq!(h[header::ACCEPT_RANGES], "bytes");
        assert_eq!(h[header::CONTENT_TYPE], "text/plain; charset=utf-8");
        assert_eq!(h[header::CONTENT_LENGTH], "12");
        assert!(h.contains_key(header::LAST_MODIFIED)); // mtime is present
        assert_eq!(
            h[header::CONTENT_DISPOSITION],
            "inline; filename=base_a.txt"
        );
    }

    #[tokio::test]
    async fn range_returns_206_slice() {
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        let resp = get(&p, &[("Range", "bytes=2-5")]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 206);
        assert_eq!(&b[..], b"2345");
        assert_eq!(h[header::CONTENT_RANGE], "bytes 2-5/10");
        assert_eq!(h[header::CONTENT_LENGTH], "4");
        assert_eq!(h[header::ACCEPT_RANGES], "bytes");
    }

    #[tokio::test]
    async fn range_open_ended_and_suffix() {
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        let resp = get(&p, &[("Range", "bytes=7-")]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 206);
        assert_eq!(&b[..], b"789");
        assert_eq!(h[header::CONTENT_RANGE], "bytes 7-9/10");

        let resp = get(&p, &[("Range", "bytes=-3")]).await;
        let (_s, h, b) = body(resp).await;
        assert_eq!(&b[..], b"789");
        assert_eq!(h[header::CONTENT_RANGE], "bytes 7-9/10");
    }

    #[tokio::test]
    async fn if_modified_since_roundtrip_304() {
        let (_d, p) = tmp_file("c.txt", b"cache-me");
        let resp = get(&p, &[]).await;
        let (_s, h, _b) = body(resp).await;
        let lm = h[header::LAST_MODIFIED].to_str().unwrap().to_string();

        // Same Last-Modified → not modified.
        let resp = get(&p, &[("If-Modified-Since", &lm)]).await;
        let (status, h304, b) = body(resp).await;
        assert_eq!(status, 304);
        assert!(b.is_empty());
        assert_eq!(h304[header::LAST_MODIFIED], lm); // kept on 304
        assert_eq!(h304[header::VARY], "Accept-Encoding"); // preset survives
        assert!(!h304.contains_key(header::CONTENT_TYPE)); // none on 304
        assert!(!h304.contains_key(header::CONTENT_LENGTH));

        // Stale IMS (before the file's mtime) → full 200.
        let resp = get(
            &p,
            &[("If-Modified-Since", "Thu, 01 Jan 1970 00:00:00 GMT")],
        )
        .await;
        let (status, _h, b) = body(resp).await;
        assert_eq!(status, 200);
        assert_eq!(&b[..], b"cache-me");
    }

    #[tokio::test]
    async fn if_none_match_concrete_etag_serves_full_200() {
        // No ETag on the representation: a concrete If-None-Match never
        // matches → 200 full body, and If-Modified-Since is skipped (the
        // client's "changed" signal wins) — fs.go condTrue semantics.
        let (_d, p) = tmp_file("c.txt", b"etag-test");
        let resp = get(
            &p,
            &[
                ("If-None-Match", "\"deadbeef\""),
                ("If-Modified-Since", "Thu, 01 Jan 2099 00:00:00 GMT"),
            ],
        )
        .await;
        let (status, _h, b) = body(resp).await;
        assert_eq!(status, 200);
        assert_eq!(&b[..], b"etag-test");
    }

    #[tokio::test]
    async fn if_none_match_star_is_304() {
        let (_d, p) = tmp_file("c.txt", b"etag-test");
        let resp = get(&p, &[("If-None-Match", "*")]).await;
        let (status, _h, b) = body(resp).await;
        assert_eq!(status, 304);
        assert!(b.is_empty());
    }

    #[tokio::test]
    async fn empty_conditional_headers_are_absent_like_go() {
        // Go reads conditional headers with Header.Get, so a present-but-
        // empty header is the same as a missing one. Empty If-Match must not
        // 412 (a concrete etag would); empty If-None-Match must not
        // short-circuit If-Modified-Since.
        let (_d, p) = tmp_file("e.txt", b"0123456789");
        let (status, _h, _b) = body(get(&p, &[("If-Match", "")]).await).await;
        assert_eq!(status, 200);
        let lm = body(get(&p, &[]).await).await;
        let last_modified = lm.1[header::LAST_MODIFIED].to_str().unwrap().to_string();
        let resp = get(
            &p,
            &[("If-None-Match", ""), ("If-Modified-Since", &last_modified)],
        )
        .await;
        let (status, _h, b) = body(resp).await;
        assert_eq!(status, 304);
        assert!(b.is_empty());
    }

    #[tokio::test]
    async fn range_no_overlap_416_shape() {
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        let resp = get(&p, &[("Range", "bytes=100-")]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 416);
        assert_eq!(&b[..], b"invalid range: failed to overlap\n");
        assert_eq!(h[header::CONTENT_RANGE], "bytes */10");
        assert_eq!(h[header::CONTENT_TYPE], "text/plain; charset=utf-8");
        assert_eq!(h[header::X_CONTENT_TYPE_OPTIONS], "nosniff");
        // fs.go serveError strips Last-Modified from the 416 header set.
        assert!(!h.contains_key(header::LAST_MODIFIED));
        // Vary + Content-Disposition survive.
        assert_eq!(h[header::VARY], "Accept-Encoding");
        assert!(h.contains_key(header::CONTENT_DISPOSITION));
    }

    #[tokio::test]
    async fn range_bad_syntax_416_without_content_range() {
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        let resp = get(&p, &[("Range", "bytes=abc")]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 416);
        assert_eq!(&b[..], b"invalid range\n");
        assert!(!h.contains_key(header::CONTENT_RANGE));
    }

    #[tokio::test]
    async fn range_on_empty_file_ignored_200() {
        let (_d, p) = tmp_file("empty.bin", b"");
        // errNoOverlap + size == 0 → ignore Range, answer 200 (fs.go).
        let resp = get(&p, &[("Range", "bytes=0-")]).await;
        let (status, _h, b) = body(resp).await;
        assert_eq!(status, 200);
        assert!(b.is_empty());
    }

    /// Round-2 review: a true empty file must send `Content-Length: 0`
    /// exactly like Go's Seek(End)-sized ServeContent — not chunked (the
    /// conformance normalizer hides framing, so this was invisible there).
    #[tokio::test]
    async fn empty_file_sends_content_length_zero() {
        let (_d, p) = tmp_file("empty.bin", b"");
        let resp = get(&p, &[]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 200);
        assert!(b.is_empty());
        assert_eq!(h[header::CONTENT_LENGTH], "0");
    }

    /// The other half of the Seek(End) model: /proc files seek fine at the
    /// start but report EINVAL on Seek(End) — Go's sizeFunc fails and
    /// ServeContent answers 500 "seeker can't seek" (fs.go errSeeker path).
    #[tokio::test]
    async fn proc_pseudo_file_answers_500_seeker_cant_seek() {
        // The 500 relies on the kernel answering EINVAL to SEEK_END on
        // procfs. Probe the actual seek behavior rather than trusting the
        // file type: if a runtime ever allowed it, both sides would answer
        // CL: 0 and this test must not assert cube-envd's mapping.
        match tokio::fs::File::open("/proc/self/status").await {
            Ok(mut f) => {
                use tokio::io::AsyncSeekExt;
                if f.seek(std::io::SeekFrom::End(0)).await.is_ok() {
                    return; // kernel allows SEEK_END here: skip
                }
            }
            Err(_) => return, // no procfs: nothing to assert
        }
        let resp = get("/proc/self/status", &[]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 500);
        assert_eq!(&b[..], b"seeker can't seek\n");
        // fs.go serveError shape: text/plain + nosniff, Last-Modified
        // stripped, Vary/Content-Disposition kept.
        assert_eq!(h[header::CONTENT_TYPE], "text/plain; charset=utf-8");
        assert!(h.contains_key(header::X_CONTENT_TYPE_OPTIONS));
        assert!(!h.contains_key(header::LAST_MODIFIED));
        assert_eq!(h[header::VARY], "Accept-Encoding");
        assert!(h.contains_key(header::CONTENT_DISPOSITION));
    }

    /// Round-5: an obs-text byte after a fractional second used to panic the
    /// date parser (byte index inside U+FFFD) and answer 500 instead of
    /// treating the unparseable date as "condition does not apply".
    #[tokio::test]
    async fn obs_text_after_fractional_second_does_not_500() {
        let (_d, p) = tmp_file("c.bin", b"cache-me");
        let mut h = HeaderMap::new();
        h.insert(
            header::IF_MODIFIED_SINCE,
            header::HeaderValue::from_bytes(b"Sun, 06 Sep 2026 07:00:00.5\xFF1 GMT").unwrap(),
        );
        let (status, _h, b) = body(get_headers(&p, h).await).await;
        assert_eq!(status, 200);
        assert_eq!(&b[..], b"cache-me");
    }

    /// Round-5: an asctime If-Modified-Since with a fractional second — the
    /// digit run must not swallow the year (Go answers 304).
    #[tokio::test]
    async fn asctime_fractional_second_ims_304s() {
        let t = std::time::UNIX_EPOCH
            .checked_add(std::time::Duration::from_secs(1_788_678_000))
            .unwrap();
        let (_d, p, kept) = tmp_file_with_mtime("f.txt", b"cache-me", t);
        if !kept {
            return; // filesystem clamped the stamp: nothing to assert
        }
        let resp = get(&p, &[("If-Modified-Since", "Sun Sep  6 07:00:00.5 2026")]).await;
        let (status, _h, _b) = body(resp).await;
        assert_eq!(status, 304);
    }

    /// Char devices with stat size 0 (Go Seek(End) = 0): the answer is an
    /// explicit CL: 0 with an empty body — never an unbounded chunked
    /// stream. (Round-4 hardening; verified against ServeContent directly.)
    #[tokio::test]
    async fn char_device_answers_empty_cl0_like_go() {
        if tokio::fs::metadata("/dev/zero")
            .await
            .map(|m| m.is_file())
            .unwrap_or(true)
        {
            return; // no /dev/zero here: nothing to assert
        }
        let resp = get("/dev/zero", &[]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 200);
        assert!(b.is_empty());
        assert_eq!(h[header::CONTENT_LENGTH], "0");
    }

    #[tokio::test]
    async fn if_match_concrete_fails_412() {
        let (_d, p) = tmp_file("c.txt", b"412-body");
        let resp = get(&p, &[("If-Match", "\"etag\"")]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 412);
        assert!(b.is_empty());
        assert!(!h.contains_key(header::CONTENT_TYPE));
        assert_eq!(h[header::VARY], "Accept-Encoding");
        // 412 keeps Last-Modified (bare WriteHeader, no serveError).
        assert!(h.contains_key(header::LAST_MODIFIED));
    }

    #[tokio::test]
    async fn range_with_identity_rejected_is_406_with_vary() {
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        // `identity;q=0, gzip` parses fine (gzip acceptable) but rejects
        // identity → the Range/conditional gate's 406b, which carries Vary
        // (it answers after Vary was set).
        let resp = get(
            &p,
            &[
                ("Range", "bytes=0-1"),
                ("Accept-Encoding", "identity;q=0, gzip"),
            ],
        )
        .await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 406);
        assert_eq!(
            json_message(&b),
            "identity encoding not acceptable for Range or conditional request"
        );
        assert_eq!(h[header::VARY], "Accept-Encoding");
    }

    #[tokio::test]
    async fn multi_range_served_as_full_200() {
        // No multipart writer: a multi-range request gets the whole file.
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        let resp = get(&p, &[("Range", "bytes=0-1, 5-6")]).await;
        let (status, _h, b) = body(resp).await;
        assert_eq!(status, 200);
        assert_eq!(&b[..], b"0123456789");
    }

    #[tokio::test]
    async fn if_range_mismatched_etag_drops_range() {
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        // If-Range with an etag never matches (no current etag) → Range is
        // dropped and the whole file is served.
        let resp = get(&p, &[("Range", "bytes=0-1"), ("If-Range", "\"old-etag\"")]).await;
        let (status, _h, b) = body(resp).await;
        assert_eq!(status, 200);
        assert_eq!(&b[..], b"0123456789");
    }

    #[tokio::test]
    async fn gzip_rejected_406_before_vary() {
        let (_d, p) = tmp_file("g.txt", b"x");
        // *;q=0 without an identity entry rejects everything → 406 answers
        // BEFORE Vary is set (upstream order).
        let resp = get(&p, &[("Accept-Encoding", "*;q=0")]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 406);
        assert_eq!(
            json_message(&b),
            "error parsing Accept-Encoding: no acceptable encoding found, supported: [gzip]"
        );
        assert!(!h.contains_key(header::VARY));
    }

    // ---- mtime edge cases (review findings #2/#3, PR #13) ------------------

    use std::time::{Duration, SystemTime, UNIX_EPOCH};

    /// Create a file and stamp it with an exact mtime (std File::set_modified
    /// → futimens, nanosecond precision on Linux). Prefer /dev/shm: some
    /// filesystems (e.g. ext4) silently clamp far-future stamps. Returns
    /// whether the filesystem actually kept the stamp.
    fn tmp_file_with_mtime(
        name: &str,
        content: &[u8],
        mtime: SystemTime,
    ) -> (tempfile::TempDir, String, bool) {
        let dir = match tempfile::tempdir_in("/dev/shm") {
            Ok(d) => d,
            Err(_) => tempfile::tempdir().unwrap(),
        };
        let p = dir.path().join(name);
        std::fs::write(&p, content).unwrap();
        std::fs::File::open(&p)
            .unwrap()
            .set_modified(mtime)
            .unwrap();
        let got = dir
            .path()
            .join(name)
            .metadata()
            .unwrap()
            .modified()
            .unwrap();
        let kept = got == mtime;
        (dir, p.to_str().unwrap().to_string(), kept)
    }

    #[test]
    fn modtime_of_matches_go_is_zero_time_and_floor() {
        // Exactly the epoch → isZeroTime: no date conditions, but If-Range
        // still knows the floor second.
        assert_eq!(modtime_of(Ok(UNIX_EPOCH)), (None, Some(0)));
        // Sub-second after the epoch: NOT zero — Last-Modified prints the
        // floor (epoch date) and the date conditions compare the floor.
        let t = UNIX_EPOCH.checked_add(Duration::from_millis(500)).unwrap();
        assert_eq!(modtime_of(Ok(t)), (Some(0), Some(0)));
        // Pre-epoch mtimes are kept (the old duration_since().ok() dropped
        // them); Go Truncate floors: -1.5s → -2s.
        let t = UNIX_EPOCH.checked_sub(Duration::from_secs(1)).unwrap();
        assert_eq!(modtime_of(Ok(t)), (Some(-1), Some(-1)));
        let t = UNIX_EPOCH.checked_sub(Duration::from_millis(500)).unwrap();
        assert_eq!(modtime_of(Ok(t)), (Some(-1), Some(-1)));
        let t = UNIX_EPOCH.checked_sub(Duration::from_millis(1500)).unwrap();
        assert_eq!(modtime_of(Ok(t)), (Some(-2), Some(-2)));
    }

    /// `touch -d '@253402300800'` (year 10000, beyond the time crate's
    /// range): Go answers 200 with the file; the old `expect()` panicked →
    /// 500.
    #[tokio::test]
    async fn out_of_range_mtime_downloads_without_panic() {
        let t = UNIX_EPOCH
            .checked_add(Duration::from_secs(253_402_300_800))
            .unwrap();
        let (_d, p, kept) = tmp_file_with_mtime("far.txt", b"0123456789abcdefghij", t);
        let resp = get(&p, &[]).await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 200);
        assert_eq!(&b[..], b"0123456789abcdefghij");
        if kept {
            assert_eq!(h[header::LAST_MODIFIED], "Sat, 01 Jan 10000 00:00:00 GMT");
        }
        // (A filesystem that clamps the stamp keeps the test to the
        // no-panic/200 contract; tmpfs keeps it verbatim.)
    }

    /// mtime = epoch + 0.5s: isZeroTime does NOT fire (the raw time is not
    /// the epoch), so Last-Modified is present and a matching If-Modified-
    /// Since (the epoch date, which is what the header truncates to) 304s.
    #[tokio::test]
    async fn subsecond_epoch_mtime_keeps_last_modified_and_304s() {
        let t = UNIX_EPOCH.checked_add(Duration::from_millis(500)).unwrap();
        let (_d, p, _kept) = tmp_file_with_mtime("half.txt", b"cache-me", t);
        let resp = get(&p, &[]).await;
        let (_s, h, _b) = body(resp).await;
        assert_eq!(h[header::LAST_MODIFIED], "Thu, 01 Jan 1970 00:00:00 GMT");

        let resp = get(
            &p,
            &[("If-Modified-Since", "Thu, 01 Jan 1970 00:00:00 GMT")],
        )
        .await;
        let (status, _h, _b) = body(resp).await;
        assert_eq!(status, 304);
    }

    /// Pre-epoch mtime: Last-Modified is kept (floored) and round-trips.
    #[tokio::test]
    async fn pre_epoch_mtime_kept_and_304s() {
        let t = UNIX_EPOCH.checked_sub(Duration::from_millis(1500)).unwrap();
        let (_d, p, _kept) = tmp_file_with_mtime("old.txt", b"cache-me", t);
        let resp = get(&p, &[]).await;
        let (_s, h, _b) = body(resp).await;
        // -1.5s floors to -2s (Go Truncate).
        assert_eq!(h[header::LAST_MODIFIED], "Wed, 31 Dec 1969 23:59:58 GMT");

        let resp = get(
            &p,
            &[("If-Modified-Since", "Wed, 31 Dec 1969 23:59:58 GMT")],
        )
        .await;
        let (status, _h, _b) = body(resp).await;
        assert_eq!(status, 304);

        // An earlier IMS (before the mtime) → full 200.
        let resp = get(
            &p,
            &[("If-Modified-Since", "Wed, 31 Dec 1969 23:59:57 GMT")],
        )
        .await;
        let (status, _h, _b) = body(resp).await;
        assert_eq!(status, 200);
    }

    /// Exactly-epoch mtime: no Last-Modified, date conditions condNone — but
    /// If-Range has no zero-time gate in fs.go, so a matching epoch date
    /// keeps the Range → 206.
    #[tokio::test]
    async fn exact_epoch_mtime_hides_last_modified_but_keeps_if_range() {
        let (_d, p, _kept) = tmp_file_with_mtime("epoch.bin", b"0123456789", UNIX_EPOCH);
        let resp = get(&p, &[]).await;
        let (_s, h, _b) = body(resp).await;
        assert!(!h.contains_key(header::LAST_MODIFIED));

        // IMS on the epoch date must NOT 304 (condNone).
        let resp = get(
            &p,
            &[("If-Modified-Since", "Thu, 01 Jan 1970 00:00:00 GMT")],
        )
        .await;
        let (status, _h, _b) = body(resp).await;
        assert_eq!(status, 200);

        // If-Range date compare: t.Unix() == modtime.Unix() → Range kept.
        let resp = get(
            &p,
            &[
                ("Range", "bytes=0-1"),
                ("If-Range", "Thu, 01 Jan 1970 00:00:00 GMT"),
            ],
        )
        .await;
        let (status, h, b) = body(resp).await;
        assert_eq!(status, 206);
        assert_eq!(&b[..], b"01");
        assert_eq!(h[header::CONTENT_RANGE], "bytes 0-1/10");
    }

    /// Drive GET /files with a pre-built header map (obs-text values need
    /// HeaderValue::from_bytes; `from_str` rejects non-ASCII).
    async fn get_headers(path: &str, headers: HeaderMap) -> axum::response::Response {
        let state = State(std::sync::Arc::new(AppState::new()));
        let mut params = HashMap::new();
        params.insert("path".to_string(), path.to_string());
        download(state, Query(params), headers).await
    }

    // ---- obs-text header bytes: garbage, not absent (review finding #1) ----

    #[tokio::test]
    async fn obs_text_range_is_garbage_416_not_absent_200() {
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        let mut h = HeaderMap::new();
        h.insert(
            header::RANGE,
            header::HeaderValue::from_bytes(b"bytes=5-6\xFF").unwrap(),
        );
        let (status, _h, _b) = body(get_headers(&p, h).await).await;
        assert_eq!(status, 416);
    }

    #[tokio::test]
    async fn obs_text_inm_is_garbage_serve_not_ims_304() {
        let (_d, p) = tmp_file("c.txt", b"cache-me");
        let mut h = HeaderMap::new();
        h.insert(
            header::IF_NONE_MATCH,
            header::HeaderValue::from_bytes(b"\xFF").unwrap(),
        );
        h.insert(
            header::IF_MODIFIED_SINCE,
            "Sun, 06 Sep 2099 07:00:00 GMT".parse().unwrap(),
        );
        // Go: INM garbage → scan break → condTrue → serve; IMS is skipped,
        // so the fresh IMS date must NOT produce 304.
        let (status, _h, _b) = body(get_headers(&p, h).await).await;
        assert_eq!(status, 200);
    }

    #[tokio::test]
    async fn obs_text_if_range_drops_range() {
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        let mut h = HeaderMap::new();
        h.insert(header::RANGE, "bytes=0-1".parse().unwrap());
        h.insert(
            header::IF_RANGE,
            header::HeaderValue::from_bytes(b"\xFF").unwrap(),
        );
        // Go: neither a valid etag nor a parseable date → condFalse → Range
        // dropped → full 200, never 206.
        let (status, _h, _b) = body(get_headers(&p, h).await).await;
        assert_eq!(status, 200);
    }

    #[tokio::test]
    async fn obs_text_accept_encoding_is_garbage_406() {
        let (_d, p) = tmp_file("r.bin", b"0123456789");
        let mut h = HeaderMap::new();
        h.insert(header::RANGE, "bytes=0-1".parse().unwrap());
        h.insert(
            header::ACCEPT_ENCODING,
            header::HeaderValue::from_bytes(b"identity;q=0,\xFF").unwrap(),
        );
        // Go encoding.go: the garbage token is neither identity nor a
        // supported encoding, identity is q=0-rejected → parse error
        // ("no acceptable encoding found") → 406 before Vary is set.
        let resp = get_headers(&p, h).await;
        let (status, hvary, b) = body(resp).await;
        assert_eq!(status, 406);
        assert_eq!(
            json_message(&b),
            "error parsing Accept-Encoding: no acceptable encoding found, supported: [gzip]"
        );
        assert!(!hvary.contains_key(header::VARY));
    }
}

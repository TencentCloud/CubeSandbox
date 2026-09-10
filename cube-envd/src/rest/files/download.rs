// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! GET `/files`: download, Range and conditional request handling.

use std::collections::HashMap;
use std::sync::Arc;

use axum::extract::{Query, State};
use axum::http::{HeaderMap, StatusCode};
use axum::response::IntoResponse;

use super::errors::{check_token_rest, resolve_request_user};
use crate::auth;
use crate::error::RestError;
use crate::rest::{content_disposition, encoding, httpdate, preconditions, ranges};
use crate::state::AppState;

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
pub(crate) struct ResolvedFile {
    pub(crate) path: String,
    pub(crate) meta: std::fs::Metadata,
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
pub(crate) fn modtime_of(
    res: std::io::Result<std::time::SystemTime>,
) -> (Option<i64>, Option<i64>) {
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

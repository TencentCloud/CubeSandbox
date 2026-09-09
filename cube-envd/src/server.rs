// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Single-port router: REST endpoints plus the two Connect services.
//!
//! RPC-surface user errors use the baseline vocabulary
//! `invalid username: '<name>'` (the REST /files surface uses the longer
//! `error looking up user ...` message — both are baseline-verified).

use std::sync::Arc;

use axum::extract::State;
use axum::http::header::{HeaderMap, HeaderValue};
use axum::response::IntoResponse;
use axum::routing::{get, post};
use axum::{Extension, Router};
use futures::StreamExt;

use crate::auth::{self, User};
use crate::connect;
use crate::cors;
use crate::error::{ConnectCode, ConnectError};
use crate::legacy;
use crate::rest;
use crate::services::watch as watch_svc;
use crate::services::{filesystem as fs_svc, process as proc_svc};
use crate::state::AppState;

pub(crate) const MAX_UNARY_BODY: usize = 4 * 1024 * 1024;

pub fn router(state: Arc<AppState>) -> Router {
    // Pull-watcher registry: constructed here and shared via Extension,
    // mirroring upstream's `Service.watchers` (`service.go:15-19`). It lives
    // with its only users in `services/watch.rs` instead of `AppState`, so
    // the shared state layer carries no watch-specific entries.
    let watchers = Arc::new(watch_svc::WatchRegistry::new());
    Router::new()
        // REST
        .route("/health", get(rest::health))
        .route("/init", post(rest::init))
        .route("/envs", get(rest::envs))
        .route("/metrics", get(rest::metrics::metrics))
        .route(
            "/files",
            get(rest::files::download).post(rest::files::upload),
        )
        .route("/files/compose", post(compose_unimplemented))
        // process.Process
        .route("/process.Process/Start", post(process_start))
        .route("/process.Process/List", post(process_list))
        .route("/process.Process/SendSignal", post(process_send_signal))
        .route("/process.Process/Connect", post(process_connect))
        .route("/process.Process/StreamInput", post(process_stream_input))
        .route("/process.Process/SendInput", post(process_send_input))
        .route("/process.Process/CloseStdin", post(process_close_stdin))
        .route("/process.Process/Update", post(process_update))
        // filesystem.Filesystem
        .route("/filesystem.Filesystem/Stat", post(fs_stat))
        .route("/filesystem.Filesystem/ListDir", post(fs_list_dir))
        .route("/filesystem.Filesystem/MakeDir", post(fs_make_dir))
        .route("/filesystem.Filesystem/Move", post(fs_move))
        .route("/filesystem.Filesystem/Remove", post(fs_remove))
        .route("/filesystem.Filesystem/WatchDir", post(fs_watch_dir))
        .route(
            "/filesystem.Filesystem/CreateWatcher",
            post(fs_create_watcher),
        )
        .route(
            "/filesystem.Filesystem/GetWatcherEvents",
            post(fs_get_watcher_events),
        )
        .route(
            "/filesystem.Filesystem/RemoveWatcher",
            post(fs_remove_watcher),
        )
        // Pull-watcher registry (see above). Applied to every route; only
        // the watch unaries actually extract it.
        .layer(Extension(watchers))
        .layer(tower_http::catch_panic::CatchPanicLayer::custom(
            panic_handler,
        ))
        // CORS sits outside the panic layer: a preflight is answered here and
        // never reaches the router, like upstream's withCORS wrapping the
        // whole server (main.go:194).
        .layer(axum::middleware::from_fn(cors::middleware))
        .with_state(state)
}

/// A panicking handler must never take the daemon down or hang the client;
/// answer with a plain 500 (issue #1227: no panic, no silent success).
fn panic_handler(
    err: Box<dyn std::any::Any + Send + 'static>,
) -> axum::http::Response<axum::body::Body> {
    let detail = err
        .downcast_ref::<&str>()
        .map(|s| s.to_string())
        .or_else(|| err.downcast_ref::<String>().cloned())
        .unwrap_or_else(|| "unknown panic".to_string());
    tracing::error!("handler panic: {detail}");
    axum::http::Response::builder()
        .status(axum::http::StatusCode::INTERNAL_SERVER_ERROR)
        .header(axum::http::header::CONTENT_TYPE, "application/json")
        .body(axum::body::Body::from(
            ConnectError::new(ConnectCode::Internal, "internal error").body_json(),
        ))
        .expect("build panic response")
}

// ---------- shared helpers ----------

/// RPC-surface user resolution: Basic auth, falling back to the default user
/// configured through `/init` (`root` until then, like upstream's
/// `defaults.User`).
fn rpc_user(state: &AppState, headers: &HeaderMap) -> Result<User, ConnectError> {
    let name = auth::user_from_basic_auth(
        headers
            .get(axum::http::header::AUTHORIZATION)
            .and_then(|v| v.to_str().ok()),
    )
    .unwrap_or_else(|| state.default_user());
    auth::lookup_user(&name).map_err(|_| {
        ConnectError::new(
            ConnectCode::Unauthenticated,
            format!("invalid username: '{name}'"),
        )
    })
}

fn rpc_token_check(state: &AppState, headers: &HeaderMap) -> Result<(), ConnectError> {
    rest::check_token(state, headers)
        .map_err(|_| ConnectError::new(ConnectCode::Unauthenticated, "invalid access token"))
}

fn unary_json(value: serde_json::Value) -> axum::response::Response {
    (
        axum::http::StatusCode::OK,
        [(axum::http::header::CONTENT_TYPE, "application/json")],
        value.to_string(),
    )
        .into_response()
}

async fn read_unary_request<T: serde::de::DeserializeOwned>(
    headers: &HeaderMap,
    body: axum::body::Body,
) -> Result<T, ConnectError> {
    connect::check_json_codec(headers)?;
    let bytes = axum::body::to_bytes(body, MAX_UNARY_BODY)
        .await
        .map_err(|e| ConnectError::new(ConnectCode::InvalidArgument, format!("read body: {e}")))?;
    // Unary requests normally arrive as bare JSON; a few hand-rolled clients
    // send an enveloped frame even for unary calls — accept both.
    let payload: &[u8] = if bytes.first() == Some(&0) && bytes.len() >= 5 {
        &bytes[5..]
    } else {
        &bytes
    };
    let payload = if payload.is_empty() { b"{}" } else { payload };
    serde_json::from_slice(payload).map_err(|e| {
        ConnectError::new(
            ConnectCode::InvalidArgument,
            format!("unmarshal message: {e}"),
        )
    })
}

fn unary_result(result: Result<serde_json::Value, ConnectError>) -> axum::response::Response {
    match result {
        Ok(v) => unary_json(v),
        Err(e) => e.into_response(),
    }
}

// ---------- process handlers ----------

async fn process_start(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    // Streaming surface: every failure is an EndStream error frame on 200.
    if let Err(e) = connect::check_json_codec(&headers) {
        return proc_svc::stream_error_response(e);
    }
    if let Err(e) = rpc_token_check(&state, &headers) {
        return proc_svc::stream_error_response(e);
    }
    let bytes = match axum::body::to_bytes(body, connect::MAX_ENVELOPE_SIZE + 5).await {
        Ok(b) => b,
        Err(e) => {
            return proc_svc::stream_error_response(ConnectError::new(
                ConnectCode::InvalidArgument,
                format!("read request: {e}"),
            ))
        }
    };
    let payload = match connect::decode_single_envelope(&bytes) {
        Ok(p) => p,
        Err(e) => return proc_svc::stream_error_response(e),
    };
    let req: crate::msg::process::StartRequest = match serde_json::from_slice(&payload) {
        Ok(r) => r,
        Err(e) => {
            return proc_svc::stream_error_response(ConnectError::new(
                ConnectCode::InvalidArgument,
                format!("unmarshal message: {e}"),
            ))
        }
    };
    let user = match rpc_user(&state, &headers) {
        Ok(u) => u,
        Err(e) => return proc_svc::stream_error_response(e),
    };
    let deadline = connect::timeout_from_headers(&headers);
    let keepalive = connect::keepalive_interval_from_headers(&headers);
    tracing::info!(
        "Start: cmd={:?} args={:?} user={} tag={:?} timeout={:?} keepalive={:?}",
        req.process.cmd,
        req.process.args,
        user.name,
        req.tag,
        deadline,
        keepalive
    );
    proc_svc::start(state, req, user, deadline, keepalive)
}

async fn process_connect(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    // Streaming surface: every failure is an EndStream error frame on 200.
    if let Err(e) = connect::check_json_codec(&headers) {
        return proc_svc::stream_error_response(e);
    }
    if let Err(e) = rpc_token_check(&state, &headers) {
        return proc_svc::stream_error_response(e);
    }
    let bytes = match axum::body::to_bytes(body, connect::MAX_ENVELOPE_SIZE + 5).await {
        Ok(b) => b,
        Err(e) => {
            return proc_svc::stream_error_response(ConnectError::new(
                ConnectCode::InvalidArgument,
                format!("read request: {e}"),
            ))
        }
    };
    let payload = match connect::decode_single_envelope(&bytes) {
        Ok(p) => p,
        Err(e) => return proc_svc::stream_error_response(e),
    };
    let req: crate::msg::process::ConnectRequest = match serde_json::from_slice(&payload) {
        Ok(r) => r,
        Err(e) => {
            return proc_svc::stream_error_response(ConnectError::new(
                ConnectCode::InvalidArgument,
                format!("unmarshal message: {e}"),
            ))
        }
    };
    let keepalive = connect::keepalive_interval_from_headers(&headers);
    // Connect is an attachment, not a command owner. The Start request owns
    // the process deadline; applying Connect-Timeout-Ms here would terminate
    // long-lived PTY attachments after an absolute wall-clock interval.
    proc_svc::connect(state, req, keepalive, None)
}

async fn process_list(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    // Token gate first, like every other handler: an unauthenticated caller
    // learns nothing about the body parser.
    if let Err(e) = rpc_token_check(&state, &headers) {
        return e.into_response();
    }
    let parsed: Result<serde_json::Value, _> = read_unary_request(&headers, body).await;
    if let Err(e) = parsed {
        return e.into_response();
    }
    unary_json(proc_svc::list(&state))
}

async fn process_send_signal(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    if let Err(e) = rpc_token_check(&state, &headers) {
        return e.into_response();
    }
    let req: crate::msg::process::SendSignalRequest = match read_unary_request(&headers, body).await
    {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    unary_result(proc_svc::send_signal(&state, &req))
}

async fn process_send_input(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    if let Err(e) = rpc_token_check(&state, &headers) {
        return e.into_response();
    }
    let req: crate::msg::process::SendInputRequest = match read_unary_request(&headers, body).await
    {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    unary_result(proc_svc::send_input(&state, &req).await)
}

async fn process_close_stdin(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    if let Err(e) = rpc_token_check(&state, &headers) {
        return e.into_response();
    }
    let req: crate::msg::process::CloseStdinRequest = match read_unary_request(&headers, body).await
    {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    unary_result(proc_svc::close_stdin(&state, &req).await)
}

async fn process_stream_input(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    // StreamInput is a streaming surface in both directions: request parsing
    // and service failures are returned as an EndStream error on HTTP 200.
    if let Err(e) = connect::check_json_codec(&headers) {
        return proc_svc::stream_error_response(e);
    }
    if let Err(e) = rpc_token_check(&state, &headers) {
        return proc_svc::stream_error_response(e);
    }

    let mut chunks = body.into_data_stream();
    let mut decoder = connect::EnvelopeDecoder::default();
    let mut selected = None;
    while let Some(chunk) = chunks.next().await {
        let chunk = match chunk {
            Ok(chunk) => chunk,
            Err(e) => {
                return proc_svc::stream_error_response(ConnectError::new(
                    ConnectCode::InvalidArgument,
                    format!("read request: {e}"),
                ))
            }
        };
        decoder.push(&chunk);
        loop {
            let payload = match decoder.next_message() {
                Ok(Some(payload)) => payload,
                Ok(None) => break,
                Err(e) => return proc_svc::stream_error_response(e),
            };
            let req: crate::msg::process::StreamInputRequest =
                match serde_json::from_slice(&payload) {
                    Ok(req) => req,
                    Err(e) => {
                        return proc_svc::stream_error_response(ConnectError::new(
                            ConnectCode::InvalidArgument,
                            format!("unmarshal message: {e}"),
                        ))
                    }
                };
            if let Err(e) = proc_svc::stream_input_event(&state, &mut selected, req).await {
                return proc_svc::stream_error_response(e);
            }
        }
    }
    if let Err(e) = decoder.finish() {
        return proc_svc::stream_error_response(e);
    }
    proc_svc::empty_stream_response()
}

async fn process_update(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    if let Err(e) = rpc_token_check(&state, &headers) {
        return e.into_response();
    }
    let req: crate::msg::process::UpdateRequest = match read_unary_request(&headers, body).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    unary_result(proc_svc::update(&state, &req))
}

// ---------- filesystem handlers ----------

macro_rules! fs_unary {
    ($name:ident, $req:ty, $svc:path) => {
        async fn $name(
            State(state): State<Arc<AppState>>,
            headers: HeaderMap,
            body: axum::body::Body,
        ) -> axum::response::Response {
            if let Err(e) = rpc_token_check(&state, &headers) {
                return e.into_response();
            }
            let req: $req = match read_unary_request(&headers, body).await {
                Ok(r) => r,
                Err(e) => return e.into_response(),
            };
            let user = match rpc_user(&state, &headers) {
                Ok(u) => u,
                Err(e) => return e.into_response(),
            };
            // Legacy downgrade applies ONLY to success (200) filesystem responses,
            // mirroring upstream WrapUnary's early err-return (interceptor.go:33-36):
            // errors never reach shouldHideChanges, so no header and no narrowing.
            let legacy = legacy::is_legacy(&headers);
            // One blocking-pool crossing per request (blocking.rs): the whole
            // service body — stat/mkdir/rename/… — runs sequentially on a
            // pool thread, mirroring the baseline's blocking goroutine
            // per handler. Never cross per syscall (~29µs each).
            let fut = crate::blocking::run(stringify!($name), move || $svc(&req, &user));
            match fut.await {
                Ok(mut v) => {
                    if legacy {
                        legacy::narrow(&mut v);
                    }
                    let mut resp = unary_json(v);
                    if legacy {
                        resp.headers_mut()
                            .insert(legacy::LEGACY_HEADER, HeaderValue::from_static("true"));
                    }
                    resp
                }
                Err(e) => e.into_response(),
            }
        }
    };
}

fs_unary!(fs_stat, crate::msg::filesystem::PathRequest, fs_svc::stat);
fs_unary!(
    fs_list_dir,
    crate::msg::filesystem::ListDirRequest,
    fs_svc::list_dir
);
fs_unary!(
    fs_make_dir,
    crate::msg::filesystem::PathRequest,
    fs_svc::make_dir
);
fs_unary!(
    fs_move,
    crate::msg::filesystem::MoveRequest,
    fs_svc::move_entry
);
fs_unary!(
    fs_remove,
    crate::msg::filesystem::PathRequest,
    fs_svc::remove
);

// ---------- filesystem watch handlers ----------

// The upstream legacy filesystem surface has no watch family, so none of
// these get the legacy downgrade/narrowing the fs_unary! unaries apply.

/// Streaming WatchDir: prechecks + tree build cross the blocking pool once
/// (a deep recursive initial walk is syscall-heavy), then the async pump owns
/// the inotify fd until the client disconnects or a watcher error kills the
/// stream. All failures — precheck or mid-stream — surface as EndStream error
/// frames on HTTP 200, like every upstream streaming error.
async fn fs_watch_dir(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    if let Err(e) = rpc_token_check(&state, &headers) {
        return e.into_response();
    }
    let req: crate::msg::filesystem::WatchDirRequest =
        match read_unary_request(&headers, body).await {
            Ok(r) => r,
            Err(e) => return e.into_response(),
        };
    let user = match rpc_user(&state, &headers) {
        Ok(u) => u,
        Err(e) => return e.into_response(),
    };
    crate::blocking::run("fs_watch_dir", move || {
        watch_svc::watch_dir(&req, &user, &headers)
    })
    .await
}

macro_rules! watch_unary {
    ($name:ident, $req:ty, $svc:path) => {
        async fn $name(
            State(state): State<Arc<AppState>>,
            Extension(watchers): Extension<Arc<watch_svc::WatchRegistry>>,
            headers: HeaderMap,
            body: axum::body::Body,
        ) -> axum::response::Response {
            if let Err(e) = rpc_token_check(&state, &headers) {
                return e.into_response();
            }
            let req: $req = match read_unary_request(&headers, body).await {
                Ok(r) => r,
                Err(e) => return e.into_response(),
            };
            let user = match rpc_user(&state, &headers) {
                Ok(u) => u,
                Err(e) => return e.into_response(),
            };
            let fut = crate::blocking::run(stringify!($name), move || $svc(&req, &user, &watchers));
            match fut.await {
                Ok(v) => unary_json(v),
                Err(e) => e.into_response(),
            }
        }
    };
}

watch_unary!(
    fs_create_watcher,
    crate::msg::filesystem::CreateWatcherRequest,
    watch_svc::create_watcher
);
watch_unary!(
    fs_get_watcher_events,
    crate::msg::filesystem::GetWatcherEventsRequest,
    watch_svc::get_watcher_events
);
watch_unary!(
    fs_remove_watcher,
    crate::msg::filesystem::RemoveWatcherRequest,
    watch_svc::remove_watcher
);

// ---------- unimplemented surfaces ----------

async fn compose_unimplemented() -> axum::response::Response {
    crate::error::RestError::new(
        axum::http::StatusCode::NOT_IMPLEMENTED,
        "/files/compose is not implemented by cube-envd (see CubeSandbox issue #1227 for the MVP scope)",
    )
    .into_response()
}

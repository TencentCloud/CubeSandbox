// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Transport pipeline for the Connect endpoints: token check, request decode,
//! user resolution, the blocking-pool crossing and response assembly, plus one
//! adapter per endpoint. Domain logic lives in `filesystem/` and `process/`.

use std::sync::Arc;

use axum::extract::{Query, State};
use axum::http::header::{HeaderMap, HeaderValue};
use axum::response::IntoResponse;
use axum::Extension;
use futures::StreamExt;

use crate::app::state::AppState;
use crate::filesystem as fs_svc;
use crate::filesystem::watch as watch_svc;
use crate::platform::config::Config;
use crate::platform::identity::{self, User};
use crate::process as proc_svc;
use crate::protocol;
use crate::protocol::{ConnectCode, ConnectError};

// ---------- shared helpers ----------

/// RPC-surface user resolution: Basic auth, falling back to the default user
/// configured through `/init` (`root` until then, like upstream's
/// `defaults.User`).
pub(crate) fn rpc_user(config: &Config, headers: &HeaderMap) -> Result<User, ConnectError> {
    let name = identity::user_from_basic_auth(
        headers
            .get(axum::http::header::AUTHORIZATION)
            .and_then(|v| v.to_str().ok()),
    )
    .unwrap_or_else(|| config.default_user());
    identity::lookup_user(&name).map_err(|_| {
        ConnectError::new(
            ConnectCode::Unauthenticated,
            format!("invalid username: '{name}'"),
        )
    })
}

pub(crate) fn rpc_token_check(config: &Config, headers: &HeaderMap) -> Result<(), ConnectError> {
    crate::app::lifecycle::check_token(config, headers)
        .map_err(|_| ConnectError::new(ConnectCode::Unauthenticated, "invalid access token"))
}

pub(crate) fn unary_json(value: serde_json::Value) -> axum::response::Response {
    (
        axum::http::StatusCode::OK,
        [(axum::http::header::CONTENT_TYPE, "application/json")],
        value.to_string(),
    )
        .into_response()
}

pub(crate) async fn read_unary_request<T: serde::de::DeserializeOwned>(
    headers: &HeaderMap,
    body: axum::body::Body,
) -> Result<T, ConnectError> {
    protocol::check_json_codec(headers)?;
    let bytes = axum::body::to_bytes(body, protocol::MAX_UNARY_BODY)
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

pub(crate) fn unary_result(
    result: Result<serde_json::Value, ConnectError>,
) -> axum::response::Response {
    match result {
        Ok(v) => unary_json(v),
        Err(e) => e.into_response(),
    }
}

// ---------- process handlers ----------

pub(crate) async fn process_start(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    // Streaming surface: every failure is an EndStream error frame on 200.
    if let Err(e) = protocol::check_json_codec(&headers) {
        return proc_svc::stream_error_response(e);
    }
    if let Err(e) = rpc_token_check(&state.config, &headers) {
        return proc_svc::stream_error_response(e);
    }
    let bytes = match axum::body::to_bytes(body, protocol::MAX_ENVELOPE_SIZE + 5).await {
        Ok(b) => b,
        Err(e) => {
            return proc_svc::stream_error_response(ConnectError::new(
                ConnectCode::InvalidArgument,
                format!("read request: {e}"),
            ))
        }
    };
    let payload = match protocol::decode_single_envelope(&bytes) {
        Ok(p) => p,
        Err(e) => return proc_svc::stream_error_response(e),
    };
    let req: crate::process::wire::StartRequest = match serde_json::from_slice(&payload) {
        Ok(r) => r,
        Err(e) => {
            return proc_svc::stream_error_response(ConnectError::new(
                ConnectCode::InvalidArgument,
                format!("unmarshal message: {e}"),
            ))
        }
    };
    let user = match rpc_user(&state.config, &headers) {
        Ok(u) => u,
        Err(e) => return proc_svc::stream_error_response(e),
    };
    let deadline = protocol::timeout_from_headers(&headers);
    let keepalive = protocol::keepalive_interval_from_headers(&headers);
    tracing::info!(
        "Start: cmd={:?} args={:?} user={} tag={:?} timeout={:?} keepalive={:?}",
        req.process.cmd,
        req.process.args,
        user.name,
        req.tag,
        deadline,
        keepalive
    );
    proc_svc::start(
        state.config.clone(),
        state.processes.clone(),
        req,
        user,
        deadline,
        keepalive,
    )
}

pub(crate) async fn process_connect(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    // Streaming surface: every failure is an EndStream error frame on 200.
    if let Err(e) = protocol::check_json_codec(&headers) {
        return proc_svc::stream_error_response(e);
    }
    if let Err(e) = rpc_token_check(&state.config, &headers) {
        return proc_svc::stream_error_response(e);
    }
    let bytes = match axum::body::to_bytes(body, protocol::MAX_ENVELOPE_SIZE + 5).await {
        Ok(b) => b,
        Err(e) => {
            return proc_svc::stream_error_response(ConnectError::new(
                ConnectCode::InvalidArgument,
                format!("read request: {e}"),
            ))
        }
    };
    let payload = match protocol::decode_single_envelope(&bytes) {
        Ok(p) => p,
        Err(e) => return proc_svc::stream_error_response(e),
    };
    let req: crate::process::wire::ConnectRequest = match serde_json::from_slice(&payload) {
        Ok(r) => r,
        Err(e) => {
            return proc_svc::stream_error_response(ConnectError::new(
                ConnectCode::InvalidArgument,
                format!("unmarshal message: {e}"),
            ))
        }
    };
    let keepalive = protocol::keepalive_interval_from_headers(&headers);
    // Connect is an attachment, not a command owner. The Start request owns
    // the process deadline; applying Connect-Timeout-Ms here would terminate
    // long-lived PTY attachments after an absolute wall-clock interval.
    proc_svc::connect(state.processes.clone(), req, keepalive, None)
}

pub(crate) async fn process_list(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    // Token gate first, like every other handler: an unauthenticated caller
    // learns nothing about the body parser.
    if let Err(e) = rpc_token_check(&state.config, &headers) {
        return e.into_response();
    }
    let parsed: Result<serde_json::Value, _> = read_unary_request(&headers, body).await;
    if let Err(e) = parsed {
        return e.into_response();
    }
    unary_json(proc_svc::list(&state.processes))
}

pub(crate) async fn process_send_signal(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    if let Err(e) = rpc_token_check(&state.config, &headers) {
        return e.into_response();
    }
    let req: crate::process::wire::SendSignalRequest =
        match read_unary_request(&headers, body).await {
            Ok(r) => r,
            Err(e) => return e.into_response(),
        };
    unary_result(proc_svc::send_signal(&state.processes, &req))
}

pub(crate) async fn process_send_input(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    if let Err(e) = rpc_token_check(&state.config, &headers) {
        return e.into_response();
    }
    let req: crate::process::wire::SendInputRequest = match read_unary_request(&headers, body).await
    {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    unary_result(proc_svc::send_input(&state.processes, &req).await)
}

pub(crate) async fn process_close_stdin(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    if let Err(e) = rpc_token_check(&state.config, &headers) {
        return e.into_response();
    }
    let req: crate::process::wire::CloseStdinRequest =
        match read_unary_request(&headers, body).await {
            Ok(r) => r,
            Err(e) => return e.into_response(),
        };
    unary_result(proc_svc::close_stdin(&state.processes, &req).await)
}

pub(crate) async fn process_stream_input(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    // StreamInput is a streaming surface in both directions: request parsing
    // and service failures are returned as an EndStream error on HTTP 200.
    if let Err(e) = protocol::check_json_codec(&headers) {
        return proc_svc::stream_error_response(e);
    }
    if let Err(e) = rpc_token_check(&state.config, &headers) {
        return proc_svc::stream_error_response(e);
    }

    let mut chunks = body.into_data_stream();
    let mut decoder = protocol::EnvelopeDecoder::default();
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
            let req: crate::process::wire::StreamInputRequest =
                match serde_json::from_slice(&payload) {
                    Ok(req) => req,
                    Err(e) => {
                        return proc_svc::stream_error_response(ConnectError::new(
                            ConnectCode::InvalidArgument,
                            format!("unmarshal message: {e}"),
                        ))
                    }
                };
            if let Err(e) = proc_svc::stream_input_event(&state.processes, &mut selected, req).await
            {
                return proc_svc::stream_error_response(e);
            }
        }
    }
    if let Err(e) = decoder.finish() {
        return proc_svc::stream_error_response(e);
    }
    proc_svc::empty_stream_response()
}

pub(crate) async fn process_update(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    if let Err(e) = rpc_token_check(&state.config, &headers) {
        return e.into_response();
    }
    let req: crate::process::wire::UpdateRequest = match read_unary_request(&headers, body).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    unary_result(proc_svc::update(&state.processes, &req))
}

// ---------- filesystem handlers ----------

/// The shared unary pipeline for the filesystem RPC handlers. A plain generic
/// function (not a macro) keeps the whole pipeline readable and
/// GitHub-diff-visible, per the monorepo convention of zero handler macros in
/// CubeAPI.
pub(crate) async fn fs_unary_endpoint<T, F>(
    state: State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
    label: &'static str,
    svc: F,
) -> axum::response::Response
where
    T: serde::de::DeserializeOwned + Send + 'static,
    F: FnOnce(&T, &User) -> Result<serde_json::Value, ConnectError> + Send + 'static,
{
    if let Err(e) = rpc_token_check(&state.config, &headers) {
        return e.into_response();
    }
    let req: T = match read_unary_request(&headers, body).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    let user = match rpc_user(&state.config, &headers) {
        Ok(u) => u,
        Err(e) => return e.into_response(),
    };
    // Legacy downgrade applies ONLY to success (200) filesystem responses,
    // mirroring upstream WrapUnary's early err-return (interceptor.go:33-36):
    // errors never reach shouldHideChanges, so no header and no narrowing.
    let legacy = crate::app::middleware::legacy::is_legacy(&headers);
    // One blocking-pool crossing per request (app/pool.rs): the whole
    // service body — stat/mkdir/rename/… — runs sequentially on a
    // pool thread, mirroring the baseline's blocking goroutine
    // per handler. Never cross per syscall (~29µs each).
    let fut = crate::app::pool::run(label, move || svc(&req, &user));
    match fut.await {
        Ok(mut v) => {
            if legacy {
                crate::app::middleware::legacy::narrow(&mut v);
            }
            let mut resp = unary_json(v);
            if legacy {
                resp.headers_mut().insert(
                    crate::app::middleware::legacy::LEGACY_HEADER,
                    HeaderValue::from_static("true"),
                );
            }
            resp
        }
        Err(e) => e.into_response(),
    }
}

pub(crate) async fn fs_stat(
    state: State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    fs_unary_endpoint(state, headers, body, "fs_stat", fs_svc::stat).await
}

pub(crate) async fn fs_list_dir(
    state: State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    fs_unary_endpoint(state, headers, body, "fs_list_dir", fs_svc::list_dir).await
}

pub(crate) async fn fs_make_dir(
    state: State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    fs_unary_endpoint(state, headers, body, "fs_make_dir", fs_svc::make_dir).await
}

pub(crate) async fn fs_move(
    state: State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    fs_unary_endpoint(state, headers, body, "fs_move", fs_svc::move_entry).await
}

pub(crate) async fn fs_remove(
    state: State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    fs_unary_endpoint(state, headers, body, "fs_remove", fs_svc::remove).await
}

// ---------- filesystem watch handlers ----------

// The upstream legacy filesystem surface has no watch family, so none of
// these get the legacy downgrade/narrowing the fs unaries apply.

/// Streaming WatchDir: prechecks + tree build cross the blocking pool once
/// (a deep recursive initial walk is syscall-heavy), then the async pump owns
/// the inotify fd until the client disconnects or a watcher error kills the
/// stream. All failures — precheck or mid-stream — surface as EndStream error
/// frames on HTTP 200, like every upstream streaming error.
pub(crate) async fn fs_watch_dir(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    if let Err(e) = rpc_token_check(&state.config, &headers) {
        return e.into_response();
    }
    let req: crate::filesystem::wire::WatchDirRequest =
        match read_unary_request(&headers, body).await {
            Ok(r) => r,
            Err(e) => return e.into_response(),
        };
    let user = match rpc_user(&state.config, &headers) {
        Ok(u) => u,
        Err(e) => return e.into_response(),
    };
    crate::app::pool::run("fs_watch_dir", move || {
        watch_svc::watch_dir(&req, &user, &headers)
    })
    .await
}

macro_rules! watch_unary {
    ($name:ident, $req:ty, $svc:path) => {
        pub(crate) async fn $name(
            State(state): State<Arc<AppState>>,
            Extension(watchers): Extension<Arc<watch_svc::WatchRegistry>>,
            headers: HeaderMap,
            body: axum::body::Body,
        ) -> axum::response::Response {
            if let Err(e) = rpc_token_check(&state.config, &headers) {
                return e.into_response();
            }
            let req: $req = match read_unary_request(&headers, body).await {
                Ok(r) => r,
                Err(e) => return e.into_response(),
            };
            let user = match rpc_user(&state.config, &headers) {
                Ok(u) => u,
                Err(e) => return e.into_response(),
            };
            let fut =
                crate::app::pool::run(stringify!($name), move || $svc(&req, &user, &watchers));
            match fut.await {
                Ok(v) => unary_json(v),
                Err(e) => e.into_response(),
            }
        }
    };
}

watch_unary!(
    fs_create_watcher,
    crate::filesystem::wire::CreateWatcherRequest,
    watch_svc::create_watcher
);
watch_unary!(
    fs_get_watcher_events,
    crate::filesystem::wire::GetWatcherEventsRequest,
    watch_svc::get_watcher_events
);
watch_unary!(
    fs_remove_watcher,
    crate::filesystem::wire::RemoveWatcherRequest,
    watch_svc::remove_watcher
);

/// Adapter for `GET /files`: the domain handler takes the config it needs, so
/// the data plane never sees the composition root.
pub(crate) async fn files_download(
    State(state): State<Arc<AppState>>,
    Query(params): Query<std::collections::HashMap<String, String>>,
    headers: HeaderMap,
) -> axum::response::Response {
    fs_svc::download(&state.config, params, headers).await
}

/// Adapter for `POST /files`.
pub(crate) async fn files_upload(
    State(state): State<Arc<AppState>>,
    Query(params): Query<std::collections::HashMap<String, String>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    fs_svc::upload(&state.config, params, headers, body).await
}

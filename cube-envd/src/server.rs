// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Single-port router: REST endpoints plus the two Connect services.
//!
//! RPC-surface user errors use the baseline vocabulary
//! `invalid username: '<name>'` (the REST /files surface uses the longer
//! `error looking up user ...` message — both are baseline-verified).

use std::sync::Arc;

use axum::extract::State;
use axum::http::HeaderMap;
use axum::response::IntoResponse;
use axum::routing::{get, post};
use axum::Router;

use crate::auth::{self, User};
use crate::connect;
use crate::error::{ConnectCode, ConnectError};
use crate::rest;
use crate::services::{filesystem as fs_svc, process as proc_svc};
use crate::state::AppState;

pub(crate) const MAX_UNARY_BODY: usize = 4 * 1024 * 1024;

pub fn router(state: Arc<AppState>) -> Router {
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
        .route(
            "/process.Process/Connect",
            post(stream_unimplemented("Process/Connect")),
        )
        .route(
            "/process.Process/StreamInput",
            post(stream_unimplemented("Process/StreamInput")),
        )
        .route(
            "/process.Process/SendInput",
            post(unary_unimplemented("Process/SendInput")),
        )
        .route(
            "/process.Process/CloseStdin",
            post(unary_unimplemented("Process/CloseStdin")),
        )
        .route(
            "/process.Process/Update",
            post(unary_unimplemented("Process/Update")),
        )
        // filesystem.Filesystem
        .route("/filesystem.Filesystem/Stat", post(fs_stat))
        .route("/filesystem.Filesystem/ListDir", post(fs_list_dir))
        .route("/filesystem.Filesystem/MakeDir", post(fs_make_dir))
        .route("/filesystem.Filesystem/Move", post(fs_move))
        .route("/filesystem.Filesystem/Remove", post(fs_remove))
        .route(
            "/filesystem.Filesystem/WatchDir",
            post(stream_unimplemented("Filesystem/WatchDir")),
        )
        .route(
            "/filesystem.Filesystem/CreateWatcher",
            post(unary_unimplemented("Filesystem/CreateWatcher")),
        )
        .route(
            "/filesystem.Filesystem/GetWatcherEvents",
            post(unary_unimplemented("Filesystem/GetWatcherEvents")),
        )
        .route(
            "/filesystem.Filesystem/RemoveWatcher",
            post(unary_unimplemented("Filesystem/RemoveWatcher")),
        )
        .layer(tower_http::catch_panic::CatchPanicLayer::custom(
            panic_handler,
        ))
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

/// RPC-surface user resolution (Basic auth, default root).
fn rpc_user(headers: &HeaderMap) -> Result<User, ConnectError> {
    let name = auth::user_from_basic_auth(
        headers
            .get(axum::http::header::AUTHORIZATION)
            .and_then(|v| v.to_str().ok()),
    )
    .unwrap_or_else(|| auth::DEFAULT_USER.to_string());
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
    let user = match rpc_user(&headers) {
        Ok(u) => u,
        Err(e) => return proc_svc::stream_error_response(e),
    };
    let deadline = connect::timeout_from_headers(&headers);
    tracing::info!(
        "Start: cmd={:?} args={:?} user={} tag={:?} timeout={:?}",
        req.process.cmd,
        req.process.args,
        user.name,
        req.tag,
        deadline
    );
    proc_svc::start(state, req, user, deadline)
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
            let user = match rpc_user(&headers) {
                Ok(u) => u,
                Err(e) => return e.into_response(),
            };
            unary_result($svc(&req, &user))
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

// ---------- unimplemented surfaces ----------

/// Unary RPCs outside the MVP: HTTP 501 + `{"code":"unimplemented",...}`.
fn unary_unimplemented(
    what: &'static str,
) -> impl Fn(
    HeaderMap,
) -> std::pin::Pin<Box<dyn std::future::Future<Output = axum::response::Response> + Send>>
       + Clone {
    move |_headers: HeaderMap| {
        Box::pin(async move { ConnectError::unimplemented(what).into_response() })
    }
}

/// Streaming RPCs outside the MVP: HTTP 200 + EndStream error frame,
/// matching how upstream reports errors on streaming surfaces.
fn stream_unimplemented(
    what: &'static str,
) -> impl Fn(
    HeaderMap,
) -> std::pin::Pin<Box<dyn std::future::Future<Output = axum::response::Response> + Send>>
       + Clone {
    move |_headers: HeaderMap| {
        Box::pin(async move { proc_svc::stream_error_response(ConnectError::unimplemented(what)) })
    }
}

async fn compose_unimplemented() -> axum::response::Response {
    crate::error::RestError::new(
        axum::http::StatusCode::NOT_IMPLEMENTED,
        "/files/compose is not implemented by cube-envd (see CubeSandbox issue #1227 for the MVP scope)",
    )
    .into_response()
}

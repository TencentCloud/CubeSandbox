// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! The single route table: every endpoint the daemon exposes, one line each,
//! plus the panic and CORS layers. Wiring only — the adapters live in
//! `handlers.rs`, domain logic in `filesystem/` and `process/`.

use std::sync::Arc;

use axum::response::IntoResponse;
use axum::routing::{get, post};
use axum::{Extension, Router};

use crate::app::handlers::{
    files_download, files_upload, fs_create_watcher, fs_get_watcher_events, fs_list_dir,
    fs_make_dir, fs_move, fs_remove, fs_remove_watcher, fs_stat, fs_watch_dir, process_close_stdin,
    process_connect, process_list, process_send_input, process_send_signal, process_start,
    process_stream_input, process_update,
};
use crate::app::middleware::cors;
use crate::app::state::AppState;
use crate::app::{lifecycle, metrics};
use crate::filesystem::watch as watch_svc;
use crate::protocol::{ConnectCode, ConnectError};

pub fn router(state: Arc<AppState>) -> Router {
    // Pull-watcher registry: constructed here and shared via Extension,
    // mirroring upstream's `Service.watchers` (`service.go:15-19`). It lives
    // with its only users in `filesystem/watch/mod.rs` instead of `AppState`, so
    // the shared state layer carries no watch-specific entries.
    let watchers = Arc::new(watch_svc::WatchRegistry::new());
    Router::new()
        // REST
        .route("/health", get(lifecycle::health))
        .route("/init", post(lifecycle::init))
        .route("/envs", get(lifecycle::envs))
        .route("/metrics", get(metrics::metrics))
        .route("/files", get(files_download).post(files_upload))
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

// ---------- unimplemented surfaces ----------

async fn compose_unimplemented() -> axum::response::Response {
    crate::protocol::RestError::new(
        axum::http::StatusCode::NOT_IMPLEMENTED,
        "/files/compose is not implemented by cube-envd (see CubeSandbox issue #1227 for the MVP scope)",
    )
    .into_response()
}

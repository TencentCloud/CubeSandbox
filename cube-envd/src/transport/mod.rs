// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

mod auth;
pub mod connect;
mod cors;
pub(crate) mod encoding;
mod end_message;
mod framing;
mod grpc;
pub(crate) mod idle;
mod json;
mod json_error;
pub mod limits;
pub mod rest;
pub mod timeout;

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

use axum::body::Body;
use axum::extract::State;
use axum::http::Request;
use axum::middleware::{self, Next};
use axum::response::{IntoResponse, Response};
use axum::Router;
use connectrpc::Router as ConnectRouter;
use futures::FutureExt;
use tokio::sync::Notify;

use self::rest::AppState;

#[derive(Default)]
pub(crate) struct RequestShutdown {
    forced: AtomicBool,
    notify: Notify,
}

impl RequestShutdown {
    pub(crate) fn force(&self) {
        self.forced.store(true, Ordering::Release);
        self.notify.notify_waiters();
    }

    async fn forced(&self) {
        loop {
            let notified = self.notify.notified();
            if self.forced.load(Ordering::Acquire) {
                return;
            }
            notified.await;
        }
    }
}

pub fn build_router(state: Arc<AppState>) -> Router {
    let connect = ConnectRouter::new()
        .add_service(Arc::new(connect::ProcessConnectService(state.clone())))
        .add_service(Arc::new(connect::FilesystemConnectService(state.clone())));
    build_router_with_connect(state, connect)
}

/// Compose registered services through the same transport and authentication boundary.
pub fn build_router_with_connect(state: Arc<AppState>, connect: ConnectRouter) -> Router {
    let mut router = Router::new()
        .route(
            "/health",
            axum::routing::get(rest::health_handler)
                .head(health_method_not_allowed)
                .fallback(health_method_not_allowed),
        )
        .route(
            "/init",
            axum::routing::post(rest::init_handler).fallback(rest::method_not_allowed),
        )
        .route(
            "/files",
            axum::routing::get(rest::file_get_handler)
                .post(rest::file_post_handler)
                .head(rest::file_method_not_allowed)
                .fallback(rest::file_method_not_allowed),
        )
        .route(
            "/files/compose",
            axum::routing::post(rest::compose_handler).fallback(rest::method_not_allowed),
        )
        .route(
            "/envs",
            axum::routing::get(rest::envs_handler)
                .head(health_method_not_allowed)
                .fallback(health_method_not_allowed),
        )
        .fallback(http_not_found)
        .with_state(state.clone());
    router = connect::register(router, connect);
    router
        .layer(middleware::from_fn_with_state(
            state.clone(),
            auth::authorize,
        ))
        .layer(middleware::from_fn_with_state(state, supervise_request))
        .layer(middleware::from_fn(cors::cors))
}

async fn health_method_not_allowed() -> Response {
    (http::StatusCode::METHOD_NOT_ALLOWED, [("allow", "GET")]).into_response()
}

async fn http_not_found() -> Response {
    (
        axum::http::StatusCode::NOT_FOUND,
        [("x-content-type-options", "nosniff")],
        "404 page not found\n",
    )
        .into_response()
}

pub async fn supervise_request(
    State(state): State<Arc<AppState>>,
    mut request: Request<Body>,
    next: Next,
) -> Response {
    if request.uri().path().starts_with("/process.Process/")
        || request.uri().path().starts_with("/filesystem.Filesystem/")
    {
        if let Some(value) = request
            .headers()
            .get("content-type")
            .and_then(|value| value.to_str().ok())
        {
            if let Ok(normalized) = http::HeaderValue::from_str(&value.to_ascii_lowercase()) {
                request.headers_mut().insert("content-type", normalized);
            }
        }
    }
    timeout::process_timeout(&mut request);
    let lifecycle = state.lifecycle.clone();
    let request_shutdown = state.request_shutdown.clone();
    let response = std::panic::AssertUnwindSafe(async move {
        if request.uri().path() != "/health" && !state.readiness.is_ready().await {
            return rest::unavailable();
        }
        next.run(request).await
    })
    .catch_unwind();
    tokio::pin!(response);
    let response = tokio::select! {
        biased;
        _ = request_shutdown.forced() => return rest::unavailable(),
        response = &mut response => response,
    };
    match response {
        Ok(response) => response,
        Err(_) => {
            lifecycle.fail_with(crate::server::EnvdFailure::request_panic());
            rest::unavailable()
        }
    }
}

pub fn new_app_state(lifecycle: Arc<crate::server::LifecycleState>) -> Arc<AppState> {
    new_app_state_with_checks(lifecycle, Vec::new())
}

pub fn new_app_state_with_checks(
    lifecycle: Arc<crate::server::LifecycleState>,
    checks: Vec<Arc<dyn crate::server::ReadinessCheck>>,
) -> Arc<AppState> {
    new_server_state(lifecycle, checks, false)
}

pub(crate) fn new_server_state(
    lifecycle: Arc<crate::server::LifecycleState>,
    checks: Vec<Arc<dyn crate::server::ReadinessCheck>>,
    is_sandbox: bool,
) -> Arc<AppState> {
    new_server_state_with_processes(
        lifecycle,
        checks,
        is_sandbox,
        Arc::new(crate::process::ProcessManager::default()),
    )
}

pub(crate) fn new_server_state_with_processes(
    lifecycle: Arc<crate::server::LifecycleState>,
    checks: Vec<Arc<dyn crate::server::ReadinessCheck>>,
    is_sandbox: bool,
    processes: Arc<crate::process::ProcessManager>,
) -> Arc<AppState> {
    let runtime = Arc::new(crate::runtime::RuntimeStateStore::with_sandbox_mode(
        is_sandbox,
    ));
    let filesystem = Arc::new(crate::filesystem::FilesystemService::default());
    let request_shutdown = Arc::new(RequestShutdown::default());
    let mut checks = checks;
    checks.insert(0, runtime.clone());
    checks.push(processes.clone());
    checks.push(filesystem.clone());
    Arc::new(AppState {
        readiness: Arc::new(crate::server::ReadinessManager::new(
            lifecycle.clone(),
            checks,
        )),
        lifecycle,
        request_shutdown,
        runtime,
        processes,
        filesystem,
        users: crate::runtime::UserDatabase::system(),
    })
}

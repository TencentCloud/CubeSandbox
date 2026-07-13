// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use axum::{
    middleware,
    routing::{delete, get, patch, post},
    Router,
};
use std::time::Duration;
use tower::ServiceBuilder;
use tower_http::{
    compression::CompressionLayer,
    cors::CorsLayer,
    request_id::{MakeRequestUuid, SetRequestIdLayer},
    timeout::TimeoutLayer,
    trace::TraceLayer,
};

use crate::{
    handlers::{health, sandboxes, snapshots, templates, volumes},
    middleware::{auth::unified_auth, rate_limit::rate_limit},
    state::AppState,
};

const DEFAULT_ROUTE_TIMEOUT: Duration = Duration::from_secs(30);

/// Timeout budget for routes that front a *synchronous* CubeMaster operation
/// which can legitimately take well beyond the default 30 s — currently
/// snapshot create (`POST /sandboxes/:id/snapshots`) and snapshot/template
/// delete (`DELETE /templates/:id`).
const SNAPSHOT_LONG_ROUTE_TIMEOUT: Duration = Duration::from_secs(240);

pub fn build_router(state: AppState) -> Router {
    let auth_configured = state
        .config
        .auth_callback_url
        .as_deref()
        .is_some_and(|u| !u.is_empty())
        || state
            .config
            .cube_api_key
            .as_deref()
            .is_some_and(|k| !k.is_empty());

    let standard_router = apply_http_layers(
        Router::new().merge(build_e2b_router(&state, auth_configured)),
        DEFAULT_ROUTE_TIMEOUT,
    );
    let snapshot_long_router = apply_http_layers(
        Router::new().merge(build_e2b_snapshot_long_router(&state, auth_configured)),
        SNAPSHOT_LONG_ROUTE_TIMEOUT,
    );

    Router::new()
        .merge(standard_router)
        .merge(snapshot_long_router)
        .with_state(state)
}

fn build_e2b_router(state: &AppState, auth_configured: bool) -> Router<AppState> {
    Router::new()
        .route("/health", get(health::health))
        .merge(build_sandbox_routes(state, auth_configured))
        .merge(build_template_routes(state, auth_configured))
        .merge(build_volume_routes(state, auth_configured))
}

/// Routes that need the longer 240 s timeout when surfaced under the e2b
/// (root) prefix.  Currently snapshot create + template/snapshot delete.
fn build_e2b_snapshot_long_router(state: &AppState, auth_configured: bool) -> Router<AppState> {
    Router::new()
        .merge(build_long_sandbox_routes(state, auth_configured))
        .merge(build_long_template_routes(state, auth_configured))
}

fn build_sandbox_routes(state: &AppState, auth_configured: bool) -> Router<AppState> {
    let routes = Router::new()
        .route("/sandboxes", get(sandboxes::list_sandboxes))
        .route("/sandboxes", post(sandboxes::create_sandbox))
        .route("/v2/sandboxes", get(sandboxes::list_sandboxes_v2))
        .route("/sandboxes/:sandboxID", get(sandboxes::get_sandbox))
        .route("/sandboxes/:sandboxID", delete(sandboxes::kill_sandbox))
        .route(
            "/sandboxes/:sandboxID/logs",
            get(sandboxes::get_sandbox_logs),
        )
        .route(
            "/v2/sandboxes/:sandboxID/logs",
            get(sandboxes::get_sandbox_logs_v2),
        )
        .route(
            "/sandboxes/:sandboxID/timeout",
            post(sandboxes::set_sandbox_timeout),
        )
        .route(
            "/sandboxes/:sandboxID/refreshes",
            post(sandboxes::refresh_sandbox),
        )
        .route(
            "/sandboxes/:sandboxID/pause",
            post(sandboxes::pause_sandbox),
        )
        .route(
            "/sandboxes/:sandboxID/resume",
            post(sandboxes::resume_sandbox),
        )
        .route(
            "/sandboxes/:sandboxID/connect",
            post(sandboxes::connect_sandbox),
        )
        .route("/snapshots", get(snapshots::list_snapshots));

    with_auth_and_rate_limit(routes, state, auth_configured)
}

/// Sandbox-rooted routes that must run on the long (240 s) budget.
fn build_long_sandbox_routes(state: &AppState, auth_configured: bool) -> Router<AppState> {
    let routes = Router::new()
        .route(
            "/sandboxes/:sandboxID/snapshots",
            post(snapshots::create_snapshot),
        )
        .route(
            "/sandboxes/:sandboxID/rollback",
            post(snapshots::rollback_sandbox),
        );

    with_auth_and_rate_limit(routes, state, auth_configured)
}

fn build_template_routes(state: &AppState, auth_configured: bool) -> Router<AppState> {
    let routes = Router::new()
        .route("/templates", get(templates::list_templates))
        .route("/templates", post(templates::create_template))
        .route("/templates/compat", get(templates::template_compat))
        .route(
            "/templates/compat/:templateID/adopt-baseline",
            post(templates::adopt_template_compat_baseline),
        )
        .route("/templates/:templateID", get(templates::get_template))
        .route("/templates/:templateID", post(templates::rebuild_template))
        .route("/templates/:templateID", patch(templates::update_template))
        .route(
            "/templates/:templateID/builds/:buildID",
            post(templates::start_template_build),
        )
        .route(
            "/templates/:templateID/builds/:buildID/status",
            get(templates::get_template_build_status),
        )
        .route(
            "/templates/:templateID/builds/:buildID/logs",
            get(templates::get_template_build_logs),
        );

    with_auth(routes, state, auth_configured)
}

/// Template/snapshot deletion lives on the long (240 s) router.
fn build_long_template_routes(state: &AppState, auth_configured: bool) -> Router<AppState> {
    let routes = Router::new().route("/templates/:templateID", delete(templates::delete_template));

    with_auth(routes, state, auth_configured)
}

fn build_volume_routes(state: &AppState, auth_configured: bool) -> Router<AppState> {
    let routes = Router::new()
        .route(
            "/volumes",
            get(volumes::list_volumes).post(volumes::create_volume),
        )
        .route(
            "/volumes/:volumeID",
            get(volumes::get_volume).delete(volumes::delete_volume),
        );

    with_auth(routes, state, auth_configured)
}

fn with_auth(
    routes: Router<AppState>,
    state: &AppState,
    auth_configured: bool,
) -> Router<AppState> {
    if auth_configured {
        routes.layer(middleware::from_fn_with_state(state.clone(), unified_auth))
    } else {
        routes
    }
}

fn with_auth_and_rate_limit(
    routes: Router<AppState>,
    state: &AppState,
    auth_configured: bool,
) -> Router<AppState> {
    if auth_configured {
        routes
            .layer(middleware::from_fn_with_state(state.clone(), rate_limit))
            .layer(middleware::from_fn_with_state(state.clone(), unified_auth))
    } else {
        routes
    }
}

fn apply_http_layers(router: Router<AppState>, timeout: Duration) -> Router<AppState> {
    router.layer(
        ServiceBuilder::new()
            .layer(SetRequestIdLayer::x_request_id(MakeRequestUuid))
            .layer(TraceLayer::new_for_http())
            .layer(TimeoutLayer::new(timeout))
            .layer(CompressionLayer::new())
            .layer(CorsLayer::permissive()),
    )
}

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
    handlers::{cluster, health, sandboxes, templates},
    middleware::{auth::unified_auth, rate_limit::rate_limit},
    state::AppState,
};

pub fn build_router(state: AppState) -> Router {
    let auth_configured = state
        .config
        .auth_callback_url
        .as_deref()
        .is_some_and(|u| !u.is_empty());
    let e2b_router = build_e2b_router(&state, auth_configured);
    let cubeapi_router = build_cubeapi_router(&state, auth_configured);

    // ── Full router with global middleware ────────────────────────────────
    Router::new()
        .merge(e2b_router)
        .nest("/cubeapi/v1", cubeapi_router)
        .with_state(state)
        .layer(
            ServiceBuilder::new()
                .layer(SetRequestIdLayer::x_request_id(MakeRequestUuid))
                .layer(TraceLayer::new_for_http())
                .layer(TimeoutLayer::new(Duration::from_secs(30)))
                .layer(CompressionLayer::new())
                .layer(CorsLayer::permissive()),
        )
}

fn build_e2b_router(state: &AppState, auth_configured: bool) -> Router<AppState> {
    Router::new()
        .route("/health", get(health::health))
        .merge(build_sandbox_routes(state, auth_configured))
        .merge(build_template_routes(state, auth_configured))
}

fn build_cubeapi_router(state: &AppState, auth_configured: bool) -> Router<AppState> {
    Router::new()
        .route("/health", get(health::health))
        .merge(build_sandbox_routes(state, auth_configured))
        .merge(build_template_routes(state, auth_configured))
        .merge(build_cluster_routes(state, auth_configured))
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
        .route(
            "/sandboxes/:sandboxID/snapshots",
            post(sandboxes::create_snapshot),
        );

    with_auth_and_rate_limit(routes, state, auth_configured)
}

fn build_template_routes(state: &AppState, auth_configured: bool) -> Router<AppState> {
    let routes = Router::new()
        .route("/templates", get(templates::list_templates))
        .route("/templates", post(templates::create_template))
        .route("/templates/:templateID", get(templates::get_template))
        .route("/templates/:templateID", post(templates::rebuild_template))
        .route("/templates/:templateID", delete(templates::delete_template))
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

fn build_cluster_routes(state: &AppState, auth_configured: bool) -> Router<AppState> {
    let routes = Router::new()
        .route("/cluster/overview", get(cluster::cluster_overview))
        .route("/nodes", get(cluster::list_nodes))
        .route("/nodes/:nodeID", get(cluster::get_node));

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

#[cfg(test)]
mod tests {
    use super::build_router;
    use crate::{
        config::ServerConfig,
        logging::{arc, noop::NoopLogger},
        state::AppState,
    };
    use axum::http::StatusCode;
    use axum_test::TestServer;

    fn test_server() -> TestServer {
        let mut config = ServerConfig::default();
        config.cubemaster_url = "http://127.0.0.1:9".to_string();

        let state = AppState::new(config, arc(NoopLogger));
        TestServer::new(build_router(state)).expect("router should build")
    }

    #[tokio::test]
    async fn preserves_root_e2b_routes() {
        let server = test_server();

        server.get("/health").await.assert_status_ok();
        assert_ne!(
            server.get("/v2/sandboxes").await.status_code(),
            StatusCode::NOT_FOUND
        );
        assert_ne!(
            server.get("/templates").await.status_code(),
            StatusCode::NOT_FOUND
        );
    }

    #[tokio::test]
    async fn serves_web_routes_under_cubeapi_prefix() {
        let server = test_server();

        server.get("/cubeapi/v1/health").await.assert_status_ok();
        assert_ne!(
            server.get("/cubeapi/v1/v2/sandboxes").await.status_code(),
            StatusCode::NOT_FOUND
        );
        assert_ne!(
            server.get("/cubeapi/v1/templates").await.status_code(),
            StatusCode::NOT_FOUND
        );
        assert_ne!(
            server
                .get("/cubeapi/v1/cluster/overview")
                .await
                .status_code(),
            StatusCode::NOT_FOUND
        );
    }

    #[tokio::test]
    async fn removes_cluster_routes_from_root_surface() {
        let server = test_server();
        server
            .get("/cluster/overview")
            .await
            .assert_status(StatusCode::NOT_FOUND);
        server
            .get("/nodes")
            .await
            .assert_status(StatusCode::NOT_FOUND);
    }
}

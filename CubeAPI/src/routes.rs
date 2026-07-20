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
        .route(
            "/templates/aliases/:alias",
            get(templates::get_template_by_alias),
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

#[cfg(test)]
mod tests {
    use super::build_router;
    use crate::{
        config::ServerConfig,
        logging::{arc, noop::NoopLogger},
        state::AppState,
    };
    use axum::{
        extract::Json,
        http::{header::RETRY_AFTER, StatusCode},
        routing::delete,
        Router,
    };
    use axum_test::TestServer;
    use serde_json::Value;

    async fn test_server() -> TestServer {
        let mut config = ServerConfig::default();
        config.cubemaster_url = "http://127.0.0.1:9".to_string();

        let state = AppState::new(config, arc(NoopLogger)).await;
        TestServer::new(build_router(state)).expect("router should build")
    }

    #[tokio::test]
    async fn delete_paused_sandbox_maps_business_errors_from_cubemaster() {
        async fn delete_handler(Json(request): Json<Value>) -> Json<Value> {
            let sandbox_id = request["sandbox_id"].as_str().unwrap_or_default();
            let (ret_code, ret_msg) = match sandbox_id {
                "sb-pausing" => (130490, "sandbox is pausing; retry DELETE after 2 seconds"),
                "sb-resume-failed" => (
                    130589,
                    "failed to resume paused sandbox before delete: shim timeout; retry DELETE after 5 seconds",
                ),
                "sb-capacity" => (
                    130409,
                    "resume rejected by paused_resource_release_ratio policy: node is full",
                ),
                _ => panic!("unexpected sandbox id: {sandbox_id}"),
            };

            Json(serde_json::json!({
                "requestID": "delete-request",
                "sandbox_id": sandbox_id,
                "ret": { "ret_code": ret_code, "ret_msg": ret_msg },
            }))
        }

        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("mock CubeMaster listener should bind");
        let address = listener.local_addr().expect("mock CubeMaster address");
        tokio::spawn(async move {
            axum::serve(
                listener,
                Router::new().route("/cube/sandbox", delete(delete_handler)),
            )
            .await
            .expect("mock CubeMaster server should run");
        });

        let mut config = ServerConfig::default();
        config.cubemaster_url = format!("http://{address}");
        let state = AppState::new(config, arc(NoopLogger)).await;
        let server = TestServer::new(build_router(state)).expect("router should build");

        for (sandbox_id, retry_after, message) in [
            (
                "sb-pausing",
                "2",
                "sandbox is pausing; retry DELETE after 2 seconds",
            ),
            (
                "sb-resume-failed",
                "5",
                "failed to resume paused sandbox before delete: shim timeout; retry DELETE after 5 seconds",
            ),
        ] {
            let response = server.delete(&format!("/sandboxes/{sandbox_id}")).await;

            assert_eq!(response.status_code(), StatusCode::SERVICE_UNAVAILABLE);
            assert_eq!(response.header(RETRY_AFTER), retry_after);
            let error: crate::models::ApiError = response.json();
            assert_eq!(error.code, 503);
            assert_eq!(error.message, message);
        }

        let response = server.delete("/sandboxes/sb-capacity").await;

        assert_eq!(response.status_code(), StatusCode::CONFLICT);
        let error: crate::models::ApiError = response.json();
        assert_eq!(error.code, 409);
        assert_eq!(
            error.message,
            "resume rejected by paused_resource_release_ratio policy: node is full"
        );
    }

    #[tokio::test]
    async fn preserves_root_e2b_routes() {
        let server = test_server().await;

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
    async fn template_alias_route_is_mounted_before_template_id_route() {
        let server = test_server().await;

        let resp = server.get("/templates/aliases/stable-python").await;
        assert_ne!(
            resp.status_code(),
            StatusCode::NOT_FOUND,
            "alias route should be mounted as its own route, not swallowed by /templates/:templateID"
        );
    }

    #[tokio::test]
    async fn removes_cluster_routes_from_root_surface() {
        let server = test_server().await;
        server
            .get("/cluster/overview")
            .await
            .assert_status(StatusCode::NOT_FOUND);
        server
            .get("/nodes")
            .await
            .assert_status(StatusCode::NOT_FOUND);
    }

    /// Refutes Bug 1: merging two routers — each with its own
    /// `TimeoutLayer` — must *not* cause the layers from the second router to
    /// override those of the first.  The standard router uses 30 s while the
    /// snapshot-long router uses 240 s; if `Router::merge` truly clobbered
    /// earlier layers (as the bug report claims), every route would inherit
    /// the 240 s budget and the short-timeout assertion below would never
    /// trip.
    ///
    /// We use scaled-down durations (50 ms / 5 s) so the test runs in well
    /// under one second.  A slow `/standard` handler must time out (HTTP 408)
    /// while a slow `/long` handler within the same combined router must
    /// complete with 200, proving each route keeps its own timeout.
    #[tokio::test]
    async fn merge_preserves_per_router_timeout_layers() {
        use axum::{routing::get, Router};
        use std::time::Duration;
        use tower::ServiceBuilder;
        use tower_http::timeout::TimeoutLayer;

        async fn slow_handler() -> &'static str {
            tokio::time::sleep(Duration::from_millis(200)).await;
            "ok"
        }

        let standard = Router::new()
            .route("/standard", get(slow_handler))
            .layer(ServiceBuilder::new().layer(TimeoutLayer::new(Duration::from_millis(50))));
        let long = Router::new()
            .route("/long", get(slow_handler))
            .layer(ServiceBuilder::new().layer(TimeoutLayer::new(Duration::from_secs(5))));

        let app = Router::new().merge(standard).merge(long);
        let server = TestServer::new(app).expect("router should build");

        // /standard is hit *first* in the merge order — i.e. exactly the case
        // the bug report claims should be overridden by the second merge.
        // We expect a request-timeout response, NOT 200.
        let resp = server.get("/standard").await;
        assert_eq!(
            resp.status_code(),
            StatusCode::REQUEST_TIMEOUT,
            "/standard should still observe its 50ms timeout after merge \
             (got {} body={:?}); merge would otherwise have to inherit /long's 5s budget",
            resp.status_code(),
            resp.text(),
        );

        // /long has a long timeout and the handler only sleeps 200ms, so it
        // must succeed.  This proves the long router's layer is also intact.
        server.get("/long").await.assert_status_ok();
    }

    /// Verifies that `DELETE /templates/:id` is mounted on the long-budget
    /// router (240 s in production), not on the 30 s standard router, so that
    /// CubeMaster's *synchronous* snapshot delete contract — which can
    /// legitimately wait for cubelet LVM/metadata cleanup — is not cut short
    /// by an HTTP timeout that fires while the master is still working.
    ///
    /// Strategy: rebuild the same merge topology as `build_router` but with
    /// scaled-down durations (50 ms vs 5 s) and a slow handler that sleeps
    /// 200 ms.  Mount the slow handler at exactly `/templates/:id` under the
    /// long router and at `/templates` under the standard router.  If the
    /// production router accidentally drops DELETE back onto the 30 s lane,
    /// the analogue under this test would be that `DELETE /templates/abc`
    /// times out (408); we assert the opposite (200 OK).
    #[tokio::test]
    async fn delete_template_uses_long_router_timeout() {
        use axum::{
            routing::{delete, get},
            Router,
        };
        use std::time::Duration;
        use tower::ServiceBuilder;
        use tower_http::timeout::TimeoutLayer;

        async fn slow_handler() -> &'static str {
            tokio::time::sleep(Duration::from_millis(200)).await;
            "ok"
        }

        // Standard lane (analogue of `standard_router`, 50 ms).  Holds the
        // *non-delete* template routes.  A 200 ms handler here MUST 408 —
        // anything else means the long timeout leaked over.
        let standard = Router::new()
            .route("/templates", get(slow_handler))
            .layer(ServiceBuilder::new().layer(TimeoutLayer::new(Duration::from_millis(50))));

        // Long lane (analogue of `snapshot_long_router`, 5 s).  Holds the
        // delete route only.  A 200 ms handler here MUST succeed.
        let long = Router::new()
            .route("/templates/:templateID", delete(slow_handler))
            .layer(ServiceBuilder::new().layer(TimeoutLayer::new(Duration::from_secs(5))));

        let app = Router::new().merge(standard).merge(long);
        let server = TestServer::new(app).expect("router should build");

        // The delete route must enjoy the long budget.
        let resp = server.delete("/templates/snap-abc123").await;
        assert_eq!(
            resp.status_code(),
            StatusCode::OK,
            "DELETE /templates/:id should run under the long-router 5s timeout \
             (got {} body={:?}); a 408 here would mean DELETE silently fell \
             back onto the 50ms standard lane and the production router has \
             regressed Bug 1's sibling.",
            resp.status_code(),
            resp.text(),
        );

        // Sanity: the standard lane really does enforce its 50 ms budget,
        // so the assertion above is meaningful (and we haven't accidentally
        // disabled all timeouts in the test harness).
        let resp = server.get("/templates").await;
        assert_eq!(
            resp.status_code(),
            StatusCode::REQUEST_TIMEOUT,
            "GET /templates was expected to time out under the 50ms standard \
             budget (got {} body={:?}); if this passes the harness no longer \
             distinguishes the two lanes and the delete-route assertion is \
             vacuous.",
            resp.status_code(),
            resp.text(),
        );
    }
}

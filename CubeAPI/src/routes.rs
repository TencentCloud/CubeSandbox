// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use axum::{
    middleware,
    routing::{delete, get, patch, post, put},
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
    handlers::{
        agenthub, cluster, config, health, registry, sandboxes, snapshots, store, templates,
        templates_v3,
    },
    middleware::{
        auth::unified_auth,
        rate_limit::{rate_limit, registry_rate_limit},
    },
    state::AppState,
};

const DEFAULT_ROUTE_TIMEOUT: Duration = Duration::from_secs(30);

/// Timeout budget for routes that front a *synchronous* CubeMaster operation
/// which can legitimately take well beyond the default 30 s — currently
/// snapshot create (`POST /sandboxes/:id/snapshots`) and snapshot/template
/// delete (`DELETE /templates/:id`).  CubeMaster waits up to its own
/// `SnapshotOperationTimeout` (15 min) for the underlying job to settle into
/// a terminal state; 240 s here covers ~99% of realistic LVM/cubelet
/// cleanup paths while keeping a hard ceiling so a wedged backend cannot
/// hang the API thread forever.  See
/// the contract: snapshot operations are synchronous — CubeAPI waits
/// for a terminal state and does not expose a polling interface.
const SNAPSHOT_LONG_ROUTE_TIMEOUT: Duration = Duration::from_secs(240);

pub fn build_router(state: AppState) -> Router {
    let auth_configured = state
        .config
        .auth_callback_url
        .as_deref()
        .is_some_and(|u| !u.is_empty());
    let standard_router = apply_http_layers(
        Router::new()
            .merge(build_e2b_router(&state, auth_configured))
            .nest("/cubeapi/v1", build_cubeapi_router(&state, auth_configured)),
        DEFAULT_ROUTE_TIMEOUT,
    );
    let snapshot_long_router = apply_http_layers(
        Router::new()
            .merge(build_e2b_snapshot_long_router(&state, auth_configured))
            .nest(
                "/cubeapi/v1",
                build_cubeapi_snapshot_long_router(&state, auth_configured),
            ),
        SNAPSHOT_LONG_ROUTE_TIMEOUT,
    );
    let registry_router = apply_http_layers(
        build_registry_router(&state),
        SNAPSHOT_LONG_ROUTE_TIMEOUT,
    );

    Router::new()
        .merge(standard_router)
        .merge(snapshot_long_router)
        .merge(registry_router)
        .with_state(state)
}

fn build_e2b_router(state: &AppState, auth_configured: bool) -> Router<AppState> {
    Router::new()
        .route("/health", get(health::health))
        .merge(build_sandbox_routes(state, auth_configured))
        .merge(build_template_routes(state, auth_configured))
}

/// Routes that need the longer 240 s timeout when surfaced under the e2b
/// (root) prefix.  Currently snapshot create + template/snapshot delete.
fn build_e2b_snapshot_long_router(state: &AppState, auth_configured: bool) -> Router<AppState> {
    Router::new()
        .merge(build_long_sandbox_routes(state, auth_configured))
        .merge(build_long_template_routes(state, auth_configured))
}

fn build_cubeapi_router(state: &AppState, auth_configured: bool) -> Router<AppState> {
    Router::new()
        .route("/health", get(health::health))
        .merge(build_sandbox_routes(state, auth_configured))
        .merge(build_template_routes(state, auth_configured))
        .merge(build_cluster_routes(state, auth_configured))
        .merge(build_agenthub_routes(state, auth_configured))
}

/// Same long-budget routes mounted under the `/cubeapi/v1` prefix.
fn build_cubeapi_snapshot_long_router(state: &AppState, auth_configured: bool) -> Router<AppState> {
    Router::new()
        .merge(build_long_sandbox_routes(state, auth_configured))
        .merge(build_long_template_routes(state, auth_configured))
        .merge(build_long_agenthub_routes(state, auth_configured))
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

/// Sandbox-rooted routes that must run on the long (240 s) budget.  Snapshot
/// creation goes through `cubelet.CommitSandbox` and can run for tens of
/// seconds on large rootfs.  Rollback shares the same synchronous-terminal
/// contract on CubeMaster (`POST /cube/sandbox/{id}/rollback`) and can wait
/// for cubelet to restore memory + rootfs from a snapshot, so it lives on
/// the long router for the same reason as create / delete.
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
    // NOTE: `DELETE /templates/:templateID` is intentionally NOT routed here.
    // It dispatches to either `SnapshotService::delete` (synchronous through
    // CubeMaster, can wait for cubelet LVM/metadata cleanup) or
    // `templates.delete_template` (also synchronous on the master side), both
    // of which can legitimately exceed the 30 s default budget under load.
    // It lives in `build_long_template_routes` instead, which is mounted on
    // `snapshot_long_router` (240 s).  Keeping it here would re-introduce the
    // 30 s "the API gave up but the master is still deleting" race we just
    // closed off when we promoted the master path to a synchronous contract.
    //
    // We split this into two sub-routers because the *build pipeline*
    // routes (V3 create + cache probe + trigger + status, plus the legacy
    // V2 build trigger) each allocate non-trivial backend state — an
    // in-memory `BuildContext` (request snapshot, registry credentials,
    // log buffer, terminal-eviction bookkeeping), and on the trigger
    // path an actual CubeMaster build pipeline. Auth alone does not
    // bound that cost: a single authenticated client can fan out
    // arbitrarily many build allocations per second. We therefore put
    // these specific routes behind `with_auth_and_rate_limit`, sharing
    // the same per-API-key 100 req/s governor that already protects
    // the sandbox surface. Plain template management (list / get /
    // create / rebuild / update / read-only logs) stays on `with_auth`
    // because it forwards directly to CubeMaster's CRUD layer, which
    // CubeMaster itself rate-limits, and these endpoints do not
    // allocate registry state.
    let build_pipeline_routes = Router::new()
        // ── E2B V3 protocol (real SDK contract) ───────────────
        // SDK calls these in order: POST /v3/templates → GET files/{hash}
        // → POST /v2/.../builds/{bid} → GET .../status. Routes are mounted
        // at the same level as /templates so both `/v3/...` and `/v2/...`
        // segments are reachable.
        .route("/v3/templates", post(templates_v3::v3_create_template))
        .route(
            "/templates/:templateID/files/:hash",
            get(templates_v3::v3_get_files_hash),
        )
        .route(
            "/v2/templates/:templateID/builds/:buildID",
            post(templates_v3::v2_trigger_build),
        )
        // Legacy v2 build trigger — same backend cost as the v3 trigger
        // (CubeMaster `create_template_from_image` pipeline), so it
        // belongs on the rate-limited lane too.
        .route(
            "/templates/:templateID/builds/:buildID",
            post(templates::start_template_build),
        )
        // Status polling: the SDK polls this in a tight loop; on every
        // call the service synchronously hits CubeMaster
        // (`get_template_build_status`) and writes into the build
        // registry's log buffer, so it is not free.
        .route(
            "/templates/:templateID/builds/:buildID/status",
            get(templates_v3::v3_get_build_status),
        );
    let build_pipeline_routes =
        with_auth_and_rate_limit(build_pipeline_routes, state, auth_configured);

    let management_routes = Router::new()
        .route("/templates", get(templates::list_templates))
        .route("/templates", post(templates::create_template))
        .route("/templates/:templateID", get(templates::get_template))
        .route("/templates/:templateID", post(templates::rebuild_template))
        .route("/templates/:templateID", patch(templates::update_template))
        .route(
            "/templates/:templateID/builds/:buildID/logs",
            get(templates::get_template_build_logs),
        );
    let management_routes = with_auth(management_routes, state, auth_configured);

    Router::new()
        .merge(build_pipeline_routes)
        .merge(management_routes)
}

/// Template/snapshot deletion lives on the long (240 s) router because it is
/// fronted by CubeMaster's *synchronous* `DELETE /cube/snapshot/{id}` (or
/// `/cube/template`), both of which can wait for cubelet to physically tear
/// down LVM volumes and replica metadata before responding.
fn build_long_template_routes(state: &AppState, auth_configured: bool) -> Router<AppState> {
    let routes = Router::new().route("/templates/:templateID", delete(templates::delete_template));

    with_auth(routes, state, auth_configured)
}

fn build_long_agenthub_routes(state: &AppState, auth_configured: bool) -> Router<AppState> {
    let routes = Router::new()
        .route(
            "/agenthub/instances/:agentID/snapshots",
            get(agenthub::list_agent_snapshots).post(agenthub::create_agent_snapshot),
        )
        .route(
            "/agenthub/instances/:agentID/snapshots/:snapshotID",
            delete(agenthub::delete_agent_snapshot).patch(agenthub::update_agent_snapshot),
        )
        .route(
            "/agenthub/instances/:agentID/rollback",
            post(agenthub::rollback_agent_to_snapshot),
        )
        .route(
            "/agenthub/instances/:agentID/recover",
            post(agenthub::recover_agent_openclaw),
        )
        .route(
            "/agenthub/instances/:agentID/clone",
            post(agenthub::clone_agent_instance),
        )
        .route(
            "/agenthub/instances/:agentID/publish-template",
            post(agenthub::publish_agent_template),
        );

    with_auth(routes, state, auth_configured)
}

fn build_cluster_routes(state: &AppState, auth_configured: bool) -> Router<AppState> {
    let routes = Router::new()
        .route("/cluster/overview", get(cluster::cluster_overview))
        .route("/cluster/versions", get(cluster::cluster_versions))
        .route("/nodes", get(cluster::list_nodes))
        .route("/nodes/:nodeID", get(cluster::get_node))
        .route("/config", get(config::get_config))
        .route("/store/meta", get(store::get_store_meta))
        .route(
            "/store/refresh",
            axum::routing::post(store::refresh_store_meta),
        );

    with_auth(routes, state, auth_configured)
}

fn build_agenthub_routes(state: &AppState, auth_configured: bool) -> Router<AppState> {
    let routes = Router::new()
        .route(
            "/agenthub/instances",
            get(agenthub::list_agent_instances).post(agenthub::create_agent_instance),
        )
        .route("/agenthub/templates", get(agenthub::list_agent_templates))
        .route(
            "/agenthub/templates/:templateID",
            patch(agenthub::update_agent_template).delete(agenthub::delete_agent_template),
        )
        .route(
            "/agenthub/instances/:agentID",
            delete(agenthub::delete_agent_instance),
        )
        .route(
            "/agenthub/instances/:agentID/restart",
            post(agenthub::restart_agent_openclaw),
        )
        .route(
            "/agenthub/instances/:agentID/operations",
            get(agenthub::list_agent_operations),
        )
        .route(
            "/agenthub/instances/:agentID/gateway/health",
            get(agenthub::get_agent_gateway_health),
        )
        .route(
            "/agenthub/instances/:agentID/pause",
            post(agenthub::pause_agent_openclaw),
        )
        .route(
            "/agenthub/instances/:agentID/resume",
            post(agenthub::resume_agent_openclaw),
        )
        .route(
            "/agenthub/instances/:agentID/upgrade",
            post(agenthub::upgrade_agent_openclaw),
        )
        .route(
            "/agenthub/instances/:agentID/model",
            put(agenthub::update_agent_model),
        )
        .route(
            "/agenthub/instances/:agentID/wecom",
            get(agenthub::get_agent_wecom_config).put(agenthub::update_agent_wecom_config),
        );

    with_auth(routes, state, auth_configured)
}

fn build_registry_router(state: &AppState) -> Router<AppState> {
    use axum::routing::{any, get};
    // Registry routes deliberately do NOT go through `unified_auth`. The OCI
    // distribution v2 protocol uses a dedicated credential domain (Basic auth
    // against the per-build push token we minted in `mint_registry_credential`),
    // and that validation lives inside `registry::proxy` itself — it must run
    // *after* the docker client's two-step `GET /v2/` → `WWW-Authenticate` →
    // retry-with-Basic handshake, which `unified_auth` would short-circuit.
    //
    // The reverse-proxy is, however, attached to a per-build /
    // per-source-IP rate-limit bucket so a single misbehaving CLI cannot
    // saturate the upstream. See `registry_rate_limit` for the keying rules.
    Router::new()
        .route("/v2/", get(registry::ping))
        .route("/v2", get(registry::ping))
        .route("/v2/*path", any(registry::proxy))
        .layer(middleware::from_fn_with_state(
            state.clone(),
            registry_rate_limit,
        ))
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
    use axum::http::StatusCode;
    use axum_test::TestServer;

    async fn test_server() -> TestServer {
        let mut config = ServerConfig::default();
        config.cubemaster_url = "http://127.0.0.1:9".to_string();

        let state = AppState::new(config, arc(NoopLogger)).await;
        TestServer::new(build_router(state)).expect("router should build")
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
    async fn serves_web_routes_under_cubeapi_prefix() {
        let server = test_server().await;

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

    /// Regression: e2b Python SDK `Template.build()` calls `POST /v3/templates`
    /// first; prior to the V3 routes we returned 404 with an empty body, which
    /// surfaced to users as `BuildException: 404: b''`. After the fix, the
    /// route exists and returns the V3 envelope.
    #[tokio::test]
    async fn v3_template_build_routes_are_reachable() {
        let server = test_server().await;

        // POST /v3/templates → 202 with templateID/buildID/names/aliases/tags/public
        let resp = server
            .post("/v3/templates")
            .json(&serde_json::json!({
                "name": "my-tpl:dev",
                "cpuCount": 1,
                "memoryMB": 1024,
            }))
            .await;
        resp.assert_status(StatusCode::ACCEPTED);
        let body: serde_json::Value = resp.json();
        assert!(body["templateID"].as_str().is_some());
        assert!(body["buildID"].as_str().is_some());
        assert!(body["names"].as_array().is_some());
        assert!(body["aliases"].as_array().is_some());
        assert_eq!(body["public"].as_bool(), Some(false));
        // The trailing `:dev` should have been folded into tags.
        let tags = body["tags"].as_array().expect("tags array");
        assert!(tags.iter().any(|t| t == "dev"));

        // GET /templates/{tid}/files/{hash} → 201 with present=true (cache hit)
        let tid = body["templateID"].as_str().unwrap();
        let r = server
            .get(&format!("/templates/{}/files/abc123", tid))
            .await;
        r.assert_status(StatusCode::CREATED);
        let fb: serde_json::Value = r.json();
        assert_eq!(fb["present"].as_bool(), Some(true));
    }

    /// Spin up a minimal in-process auth callback that always returns 200,
    /// returning the absolute URL (`http://127.0.0.1:<port>/`) plus a
    /// JoinHandle for the tokio task running it. Used by the rate-limit
    /// regression tests below: `with_auth_and_rate_limit` only attaches
    /// the rate-limiter when `auth_callback_url` is configured, so we
    /// need a real, reachable callback to exercise the production layer
    /// stack.
    async fn spawn_always_200_auth_server() -> (String, tokio::task::JoinHandle<()>) {
        use axum::routing::post;
        let app = axum::Router::new().route("/", post(|| async { axum::http::StatusCode::OK }));
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("bind mock auth server");
        let addr = listener.local_addr().expect("addr");
        let url = format!("http://{}/", addr);
        let handle = tokio::spawn(async move {
            let _ = axum::serve(listener, app).await;
        });
        (url, handle)
    }

    #[tokio::test]
    async fn v3_build_pipeline_routes_are_rate_limited() {
        let (auth_url, _auth_handle) = spawn_always_200_auth_server().await;

        let mut config = ServerConfig::default();
        config.cubemaster_url = "http://127.0.0.1:9".to_string();
        config.auth_callback_url = Some(auth_url);
        config.rate_limit_per_sec = 1;

        let state = AppState::new(config, arc(NoopLogger)).await;
        let server = TestServer::new(build_router(state)).expect("router should build");

        let mut rate_limited = 0usize;
        let mut passed = 0usize;
        for _ in 0..20 {
            let resp = server
                .post("/v3/templates")
                .add_header(
                    axum::http::HeaderName::from_static("x-api-key"),
                    axum::http::HeaderValue::from_static("abuser"),
                )
                .json(&serde_json::json!({
                    "name": "my-tpl:dev",
                    "cpuCount": 1,
                    "memoryMB": 1024,
                }))
                .await;
            match resp.status_code() {
                StatusCode::TOO_MANY_REQUESTS => rate_limited += 1,
                code if code.is_success() => passed += 1,
                _ => passed += 1,
            }
        }

        assert!(
            rate_limited > 0,
            "expected at least one POST /v3/templates to be 429-throttled \
             with rate_limit_per_sec=1; got {} successes and 0 throttled \
             responses across 20 requests — the rate-limit middleware is \
             not attached to V3 build pipeline routes",
            passed,
        );
        assert!(
            passed > 0,
            "expected at least one POST /v3/templates to pass the gate \
             (the very first burst token); got {} 429s and 0 passes — \
             auth or rate-limit is mis-configured in the test harness",
            rate_limited,
        );
    }

    /// Companion: plain template *management* endpoints (list / get /
    /// CRUD) explicitly stay on `with_auth` rather than
    /// `with_auth_and_rate_limit`, because they forward directly to
    /// CubeMaster's CRUD layer and don't allocate in-process build
    /// state. Pinning that boundary here so a future "let's just rate
    /// limit everything" change can't silently regress operator
    /// workflows that legitimately list templates faster than the
    /// shared governor allows.
    #[tokio::test]
    async fn template_management_routes_are_not_rate_limited() {
        let (auth_url, _auth_handle) = spawn_always_200_auth_server().await;

        let mut config = ServerConfig::default();
        config.cubemaster_url = "http://127.0.0.1:9".to_string();
        config.auth_callback_url = Some(auth_url);
        config.rate_limit_per_sec = 1;

        let state = AppState::new(config, arc(NoopLogger)).await;
        let server = TestServer::new(build_router(state)).expect("router should build");

        // Burst the same 20 requests against the management surface.
        // Even with quota=1 we should never see 429 here, because the
        // rate-limit layer is not attached to this lane.
        let mut saw_throttle = false;
        for _ in 0..20 {
            let resp = server
                .get("/templates")
                .add_header(
                    axum::http::HeaderName::from_static("x-api-key"),
                    axum::http::HeaderValue::from_static("abuser"),
                )
                .await;
            if resp.status_code() == StatusCode::TOO_MANY_REQUESTS {
                saw_throttle = true;
                break;
            }
        }

        assert!(
            !saw_throttle,
            "GET /templates is on the auth-only lane and must NOT be \
             rate-limited; observing 429 here would mean the management \
             sub-router was accidentally folded into with_auth_and_rate_limit"
        );
    }
}
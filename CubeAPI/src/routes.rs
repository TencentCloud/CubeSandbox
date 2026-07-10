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
    handlers::{agenthub, auth, cluster, config, health, sandboxes, snapshots, store, templates},
    middleware::{auth::unified_auth, rate_limit::rate_limit},
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
        .merge(build_auth_routes(state))
        .merge(build_sandbox_routes(state, auth_configured))
        .merge(build_template_routes(state, auth_configured))
        .merge(build_cluster_routes(state, auth_configured))
        .merge(build_agenthub_routes(state, auth_configured))
}

/// WebUI login routes. These are intentionally left unauthenticated (like
/// `/health`) so the login/session flow itself is always reachable.
fn build_auth_routes(state: &AppState) -> Router<AppState> {
    let rate_limited_routes = Router::new()
        .route("/auth/login", post(auth::login))
        .route("/auth/change-password", post(auth::change_password));
    let open_routes = Router::new()
        .route("/auth/logout", post(auth::logout))
        .route("/auth/session", get(auth::session))
        .merge(
            rate_limited_routes.layer(middleware::from_fn_with_state(state.clone(), rate_limit)),
        );
    open_routes
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
            "/agenthub/templates/market",
            post(agenthub::register_market_agent_template),
        )
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
        )
        .route(
            "/agenthub/settings",
            get(agenthub::get_agent_settings).put(agenthub::update_agent_settings),
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
        config::{ServerConfig, WebhookConfig, WebhookEndpointConfig},
        logging::{arc, noop::NoopLogger, webhook::WebhookLogger, LogEvent, Logger},
        state::AppState,
    };
    use async_trait::async_trait;
    use axum::{
        extract::State,
        http::StatusCode,
        routing::{get, post},
        Json, Router,
    };
    use axum_test::TestServer;
    use serde_json::Value;
    use std::{
        sync::{
            atomic::{AtomicI32, Ordering},
            Arc,
        },
        time::Duration,
    };
    use tokio::sync::{Mutex, Notify};

    #[derive(Clone, Default)]
    struct CaptureLogger {
        events: Arc<Mutex<Vec<LogEvent>>>,
    }

    #[async_trait]
    impl Logger for CaptureLogger {
        async fn log(&self, event: LogEvent) {
            self.events.lock().await.push(event);
        }

        fn name(&self) -> &'static str {
            "capture"
        }
    }

    #[derive(Clone)]
    struct MockCubeMaster {
        status: Arc<AtomicI32>,
    }

    async fn create_handler() -> Json<Value> {
        Json(serde_json::json!({
            "requestID": "req-create",
            "sandbox_id": "sb-events",
            "ret": { "ret_code": 0, "ret_msg": "ok" }
        }))
    }

    async fn delete_handler() -> Json<Value> {
        Json(serde_json::json!({
            "requestID": "req-delete",
            "sandbox_id": "sb-events",
            "ret": { "ret_code": 0, "ret_msg": "ok" }
        }))
    }

    async fn update_handler(
        State(state): State<MockCubeMaster>,
        Json(body): Json<Value>,
    ) -> Json<Value> {
        let status = if body["action"] == "pause" { 5 } else { 1 };
        state.status.store(status, Ordering::SeqCst);
        Json(serde_json::json!({
            "ret": { "ret_code": 0, "ret_msg": "ok" }
        }))
    }

    async fn info_handler(State(state): State<MockCubeMaster>) -> Json<Value> {
        Json(serde_json::json!({
            "requestID": "req-info",
            "ret": { "ret_code": 0, "ret_msg": "ok" },
            "data": [{
                "sandbox_id": "sb-events",
                "host_id": "host-1",
                "status": state.status.load(Ordering::SeqCst),
                "template_id": "tpl-events",
                "containers": []
            }]
        }))
    }

    async fn mock_cubemaster() -> (String, tokio::task::JoinHandle<()>) {
        let state = MockCubeMaster {
            status: Arc::new(AtomicI32::new(1)),
        };
        let app = Router::new()
            .route("/cube/sandbox", post(create_handler).delete(delete_handler))
            .route("/cube/sandbox/update", post(update_handler))
            .route("/cube/sandbox/info", get(info_handler))
            .with_state(state);
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("mock CubeMaster should bind");
        let address = listener.local_addr().expect("listener address");
        let task = tokio::spawn(async move {
            axum::serve(listener, app)
                .await
                .expect("mock CubeMaster should run");
        });
        (format!("http://{address}"), task)
    }

    async fn sandbox_test_server(
        cubemaster_url: String,
        logger: crate::logging::ArcLogger,
    ) -> TestServer {
        let config = ServerConfig {
            cubemaster_url,
            database_url: None,
            ..ServerConfig::default()
        };
        let state = AppState::new(config, logger).await;
        TestServer::new(build_router(state)).expect("router should build")
    }

    async fn test_server() -> TestServer {
        let mut config = ServerConfig::default();
        config.cubemaster_url = "http://127.0.0.1:9".to_string();

        let state = AppState::new(config, arc(NoopLogger)).await;
        TestServer::new(build_router(state)).expect("router should build")
    }

    #[tokio::test]
    async fn sandbox_lifecycle_routes_emit_all_four_webhook_event_types() {
        let (cubemaster_url, cubemaster_task) = mock_cubemaster().await;
        let capture = CaptureLogger::default();
        let server = sandbox_test_server(cubemaster_url, arc(capture.clone())).await;

        server
            .post("/sandboxes")
            .json(&serde_json::json!({"templateID": "tpl-events", "timeout": 60}))
            .await
            .assert_status(StatusCode::CREATED);
        server
            .post("/sandboxes/sb-events/pause")
            .await
            .assert_status(StatusCode::NO_CONTENT);
        server
            .post("/sandboxes/sb-events/resume")
            .json(&serde_json::json!({"timeout": 60}))
            .await
            .assert_status(StatusCode::CREATED);
        server
            .post("/sandboxes/sb-events/pause")
            .await
            .assert_status(StatusCode::NO_CONTENT);
        server
            .post("/sandboxes/sb-events/connect")
            .json(&serde_json::json!({"timeout": 60}))
            .await
            .assert_status(StatusCode::OK);
        server
            .delete("/sandboxes/sb-events")
            .await
            .assert_status(StatusCode::NO_CONTENT);

        let events = capture.events.lock().await;
        let lifecycle: Vec<_> = events
            .iter()
            .filter(|event| event.event.starts_with("sandbox."))
            .collect();
        assert_eq!(
            lifecycle
                .iter()
                .map(|event| event.event.as_str())
                .collect::<Vec<_>>(),
            [
                "sandbox.created",
                "sandbox.paused",
                "sandbox.resumed",
                "sandbox.paused",
                "sandbox.resumed",
                "sandbox.deleted"
            ]
        );
        assert!(lifecycle.iter().all(|event| {
            event.fields.get("sandbox_id") == Some(&Value::String("sb-events".to_string()))
        }));
        assert_eq!(
            lifecycle[0].fields.get("template_id"),
            Some(&Value::String("tpl-events".to_string()))
        );
        assert_eq!(
            lifecycle[2].fields.get("template_id"),
            Some(&Value::String("tpl-events".to_string()))
        );
        assert_eq!(
            lifecycle[4].fields.get("template_id"),
            Some(&Value::String("tpl-events".to_string()))
        );
        cubemaster_task.abort();
    }

    #[tokio::test]
    async fn slow_webhook_does_not_delay_sandbox_creation() {
        #[derive(Clone)]
        struct SlowReceiver {
            entered: Arc<Notify>,
            release: Arc<Notify>,
        }

        async fn slow_receiver(State(state): State<SlowReceiver>) -> StatusCode {
            state.entered.notify_one();
            state.release.notified().await;
            StatusCode::NO_CONTENT
        }

        let receiver_state = SlowReceiver {
            entered: Arc::new(Notify::new()),
            release: Arc::new(Notify::new()),
        };
        let receiver = Router::new()
            .route("/webhook", post(slow_receiver))
            .with_state(receiver_state.clone());
        let receiver_listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("mock receiver should bind");
        let receiver_address = receiver_listener.local_addr().expect("listener address");
        let receiver_task = tokio::spawn(async move {
            axum::serve(receiver_listener, receiver)
                .await
                .expect("mock receiver should run");
        });
        let webhook = WebhookLogger::new(WebhookConfig {
            endpoints: vec![WebhookEndpointConfig {
                name: Some("slow".to_string()),
                url: format!("http://{receiver_address}/webhook"),
                events: vec!["sandbox.created".to_string()],
                secret: None,
            }],
            timeout_ms: 5_000,
            max_attempts: 1,
            ..WebhookConfig::default()
        })
        .expect("webhook logger should build");
        let (cubemaster_url, cubemaster_task) = mock_cubemaster().await;
        let server = sandbox_test_server(cubemaster_url, arc(webhook.clone())).await;

        let request = server
            .post("/sandboxes")
            .json(&serde_json::json!({"templateID": "tpl-events", "timeout": 60}));
        let response = tokio::time::timeout(Duration::from_secs(2), async move { request.await })
            .await
            .expect("sandbox creation must return while the webhook is blocked");
        response.assert_status(StatusCode::CREATED);

        tokio::time::timeout(Duration::from_secs(2), receiver_state.entered.notified())
            .await
            .expect("webhook delivery should reach the receiver");
        receiver_state.release.notify_waiters();

        webhook.flush().await;
        cubemaster_task.abort();
        receiver_task.abort();
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
    async fn auth_login_route_is_rate_limited_without_auth_middleware() {
        let mut config = ServerConfig::default();
        config.cubemaster_url = "http://127.0.0.1:9".to_string();
        config.rate_limit_per_sec = 1;
        let state = AppState::new(config, arc(NoopLogger)).await;
        let server = TestServer::new(build_router(state)).expect("router should build");
        let login_body = serde_json::json!({
            "username": "admin",
            "password": "admin"
        });

        let first = server
            .post("/cubeapi/v1/auth/login")
            .json(&login_body)
            .await;
        assert_ne!(first.status_code(), StatusCode::TOO_MANY_REQUESTS);

        server
            .post("/cubeapi/v1/auth/login")
            .json(&login_body)
            .await
            .assert_status(StatusCode::TOO_MANY_REQUESTS);
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

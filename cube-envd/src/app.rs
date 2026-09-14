// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::sync::Arc;

use axum::{
    body::{to_bytes, Body},
    extract::{Request, State},
    http::StatusCode,
    response::{IntoResponse, Response},
    routing::{get, post},
    Router,
};
use tokio_util::sync::CancellationToken;

use crate::{
    connect::{require_unary, Code, RpcError, MAX_UNARY_JSON_BYTES, MAX_UNARY_JSON_MIB},
    filesystem,
    init::{Environment, InitRequest},
    process,
    process::ProcessRegistry,
};

#[derive(Clone)]
/// 保存 HTTP 路由共享的环境、进程注册表和关闭信号。
pub struct AppState {
    /// 保存由 /init 写入的沙箱环境变量快照。
    pub(crate) environment: Arc<Environment>,
    /// 跟踪由 envd 启动并负责回收的进程。
    pub(crate) processes: ProcessRegistry,
    /// 向长连接和后台任务广播优雅关闭请求。
    pub(crate) shutdown: CancellationToken,
}

/// 以参考实现的初始状态（`E2B_SANDBOX` 已种入）创建应用状态。
impl Default for AppState {
    fn default() -> Self {
        Self {
            environment: Arc::new(Environment::new()),
            processes: ProcessRegistry::default(),
            shutdown: CancellationToken::new(),
        }
    }
}

/// 提供应用状态的关闭协调操作。
impl AppState {
    /// 广播关闭信号以停止接收新的流式工作。
    pub fn begin_shutdown(&self) {
        self.shutdown.cancel();
    }

    /// 终止并等待所有仍由 envd 管理的子进程。
    pub async fn shutdown_processes(&self) {
        self.processes.shutdown().await;
    }
}

/// 使用默认应用状态创建全部 HTTP 路由。
pub fn router() -> Router {
    router_with_state(AppState::default())
}

/// 使用调用方提供的状态创建全部 HTTP 路由。
///
/// 层序（由外到内）：CORS → panic 兜底 → 路由。上游把 CORS 包在整个 server 外层
/// （预检不进入路由），而 panic 兜底必须包住 handler，两者顺序不可交换。
pub fn router_with_state(state: AppState) -> Router {
    let inner: Router = Router::new()
        .route("/health", get(health))
        .route("/init", post(init))
        .route("/envs", get(envs))
        .route("/metrics", get(metrics_unimplemented))
        .route(
            "/files",
            get(filesystem::files::download).post(filesystem::files::upload),
        )
        .route("/files/compose", post(files_compose_unimplemented))
        .route("/process.Process/Start", post(process::start))
        .route("/process.Process/List", post(process::list))
        .route("/process.Process/Connect", post(process::connect))
        .route("/process.Process/Update", post(process::update))
        .route("/process.Process/StreamInput", post(process::stream_input))
        .route("/process.Process/SendInput", post(process::send_input))
        .route("/process.Process/SendSignal", post(process::send_signal))
        .route("/process.Process/CloseStdin", post(process::close_stdin))
        .route("/filesystem.Filesystem/Stat", post(filesystem::stat))
        .route("/filesystem.Filesystem/MakeDir", post(filesystem::make_dir))
        .route("/filesystem.Filesystem/Move", post(filesystem::move_entry))
        .route("/filesystem.Filesystem/ListDir", post(filesystem::list_dir))
        .route("/filesystem.Filesystem/Remove", post(filesystem::remove))
        .route(
            "/filesystem.Filesystem/WatchDir",
            post(filesystem::watch_dir),
        )
        .route("/filesystem.Filesystem/CreateWatcher", post(unimplemented))
        .route(
            "/filesystem.Filesystem/GetWatcherEvents",
            post(unimplemented),
        )
        .route("/filesystem.Filesystem/RemoveWatcher", post(unimplemented))
        .layer(tower_http::catch_panic::CatchPanicLayer::custom(
            panic_response,
        ))
        .with_state(state);

    // CORS 必须在**路由之外**：参考实现把它包在整个 server 外层，预检请求根本不会
    // 进入路由（因此不会带上路由层为 OPTIONS 补的 `Allow`，也不是 405）。
    // 外层 router 没有自己的路由，全部请求经 fallback 交给内层，于是 CORS 层
    // 稳定地跑在路由之前。
    Router::new()
        .fallback_service(inner)
        // 只压缩缓冲型 JSON 响应：参考实现对 `application/json` 压缩，对流式
        // `application/connect+json` 不压缩，且不会给其他响应加 `Vary`。
        .layer(axum::middleware::from_fn(crate::compress::middleware))
        .layer(axum::middleware::from_fn(crate::cors::middleware))
}

/// 把 handler panic 变成稳定的 500，而不是断开连接。
///
/// 上游 Go 的 HTTP server 会为每个请求 recover（`net/http` 的 per-request panic
/// 捕获），客户端同样得到 500；Rust 侧若不显式兜底，panic 会沿任务展开并直接丢掉
/// 连接，客户端看到连接中断——这是"不支持的面必须返回稳定错误、不得 panic"这条
/// 承诺里最容易漏掉的一格。
pub fn panic_response(error: Box<dyn std::any::Any + Send + 'static>) -> Response {
    let detail = error
        .downcast_ref::<&str>()
        .map(|value| (*value).to_owned())
        .or_else(|| error.downcast_ref::<String>().cloned())
        .unwrap_or_else(|| "unknown panic".to_owned());
    tracing::error!(detail = %detail, "handler panicked; answering 500");

    RpcError::new(Code::Internal, "internal error").into_response()
}

/// 返回 GET /metrics 的稳定"未实现"错误。
///
/// 上游 Go envd 暴露 Prometheus 指标；cube-envd 的 MVP 不提供该面，也不在沙箱内
/// 采集指标（仓库内没有消费者：`Cubelet` 只解析 `envd --version`）。这里返回显式的
/// 501，而不是让路由缺失导致的 404——声明过的边界必须是一个稳定错误。
async fn metrics_unimplemented() -> RpcError {
    RpcError::unimplemented("/metrics is not implemented by cube-envd")
}

/// 返回 POST /files/compose 的稳定"未实现"错误。
async fn files_compose_unimplemented() -> RpcError {
    RpcError::unimplemented("/files/compose is not implemented by cube-envd")
}

/// 返回无响应体的健康检查成功状态。
///
/// 参考实现（`internal/api/store.go:GetHealth`）在 204 上显式写
/// `Cache-Control: no-store` 与空的 `Content-Type`；SDK 与探针不看这两个头，
/// 但它们属于可观察面，且对照套件逐头对比，因此保持一致。
async fn health() -> Response {
    no_content_response()
}

/// 构造参考实现风格的 204 响应（`Cache-Control: no-store` + 空 `Content-Type`）。
fn no_content_response() -> Response {
    (
        StatusCode::NO_CONTENT,
        [
            (
                axum::http::header::CACHE_CONTROL,
                axum::http::HeaderValue::from_static("no-store"),
            ),
            (
                axum::http::header::CONTENT_TYPE,
                axum::http::HeaderValue::from_static(""),
            ),
        ],
    )
        .into_response()
}

/// 校验并原子替换由 /init 提交的环境变量快照。
async fn init(
    State(state): State<AppState>,
    body: Body,
) -> Result<Response, crate::rest::RestError> {
    let body = to_bytes(body, MAX_UNARY_JSON_BYTES).await.map_err(|_| {
        crate::rest::RestError::new(
            StatusCode::PAYLOAD_TOO_LARGE,
            format!("init request exceeds {MAX_UNARY_JSON_MIB} MiB"),
        )
    })?;
    let request = if body.is_empty() {
        InitRequest::default()
    } else {
        serde_json::from_slice(&body).map_err(invalid_init_request)?
    };

    // 逐键合并：参考实现用 Store 覆盖单键，未提及的既有变量必须保留。
    if let Some(env_vars) = request.env_vars {
        state.environment.merge(env_vars);
    }

    Ok(no_content_response())
}

/// 返回当前保存的环境变量集合。
///
/// 参考实现（`internal/api/envs.go`）设置 `Cache-Control: no-store`，并用
/// `json.Encoder` 写出（末尾带换行）。两者都在 SDK 可观察面上。
async fn envs(State(state): State<AppState>) -> Response {
    let mut body =
        serde_json::to_vec(&state.environment.snapshot()).unwrap_or_else(|_| b"{}".to_vec());
    body.push(b'\n');
    (
        [
            (
                axum::http::header::CACHE_CONTROL,
                axum::http::HeaderValue::from_static("no-store"),
            ),
            (
                axum::http::header::CONTENT_TYPE,
                axum::http::HeaderValue::from_static("application/json"),
            ),
        ],
        body,
    )
        .into_response()
}

/// 将 JSON 反序列化错误转换为稳定的 init 请求错误响应。
fn invalid_init_request(error: serde_json::Error) -> crate::rest::RestError {
    crate::rest::RestError::invalid_argument(format!("invalid init request: {error}"))
}

/// 为尚未实现的持久 watcher RPC 返回 Connect 协议错误。
async fn unimplemented(request: Request) -> Result<(), RpcError> {
    require_unary(request.headers())?;
    Err(RpcError::unimplemented(
        "persistent filesystem watchers are not supported",
    ))
}

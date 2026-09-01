use std::sync::Arc;

use axum::{
    body::{to_bytes, Body},
    extract::{Request, State},
    http::StatusCode,
    response::IntoResponse,
    routing::{get, post},
    Json, Router,
};
use tokio_util::sync::CancellationToken;

use crate::{
    connect::{require_unary, RpcError, MAX_UNARY_JSON_BYTES},
    filesystem,
    init::{Environment, InitRequest},
    process,
    process::ProcessRegistry,
};

#[derive(Clone, Default)]
/// 保存 HTTP 路由共享的环境、进程注册表和关闭信号。
pub struct AppState {
    /// 保存由 /init 写入的沙箱环境变量快照。
    pub(crate) environment: Arc<Environment>,
    /// 跟踪由 envd 启动并负责回收的进程。
    pub(crate) processes: ProcessRegistry,
    /// 向长连接和后台任务广播优雅关闭请求。
    pub(crate) shutdown: CancellationToken,
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

/// 使用调用方提供的状态创建 HTTP 路由，便于测试和嵌入。
pub fn router_with_state(state: AppState) -> Router {
    Router::new()
        .route("/health", get(health))
        .route("/init", post(init))
        .route("/envs", get(envs))
        .route(
            "/files",
            get(filesystem::files::download).post(filesystem::files::upload),
        )
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
        .with_state(state)
}

/// 返回无响应体的健康检查成功状态。
async fn health() -> StatusCode {
    StatusCode::NO_CONTENT
}

/// 校验并原子替换由 /init 提交的环境变量快照。
async fn init(
    State(state): State<AppState>,
    body: Body,
) -> Result<StatusCode, (StatusCode, String)> {
    let body = to_bytes(body, MAX_UNARY_JSON_BYTES).await.map_err(|_| {
        (
            StatusCode::PAYLOAD_TOO_LARGE,
            "init request exceeds 1 MiB".to_owned(),
        )
    })?;
    let request = if body.is_empty() {
        InitRequest::default()
    } else {
        serde_json::from_slice(&body).map_err(invalid_init_request)?
    };

    if let Some(env_vars) = request.env_vars {
        state.environment.replace(env_vars);
    }

    Ok(StatusCode::NO_CONTENT)
}

/// 返回当前保存的环境变量快照。
async fn envs(State(state): State<AppState>) -> impl IntoResponse {
    Json(state.environment.snapshot())
}

/// 将 JSON 反序列化错误转换为稳定的 init 请求错误响应。
fn invalid_init_request(error: serde_json::Error) -> (StatusCode, String) {
    (
        StatusCode::BAD_REQUEST,
        format!("invalid init request: {error}"),
    )
}

/// 为尚未实现的持久 watcher RPC 返回 Connect 协议错误。
async fn unimplemented(request: Request) -> Result<(), RpcError> {
    require_unary(request.headers())?;
    Err(RpcError::unimplemented(
        "persistent filesystem watchers are not supported",
    ))
}

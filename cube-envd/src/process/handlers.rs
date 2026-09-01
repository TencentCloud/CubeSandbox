use std::sync::Arc;

use axum::{
    extract::Request,
    response::{IntoResponse, Response},
};

use crate::{
    app::AppState,
    auth::request_user,
    connect::{
        decode_request_frame, keepalive_interval, require_streaming, require_unary, Code,
        RequestFrameReader, RpcError,
    },
    generated::process as proto,
    wire,
};

use super::*;

pub async fn start(
    axum::extract::State(state): axum::extract::State<AppState>,
    request: Request,
) -> Result<Response, RpcError> {
    require_streaming(request.headers())?;
    let user = request_user(request.headers())
        .map_err(|error| RpcError::new(Code::Unauthenticated, error.to_string()))?;
    let timeout = parse_timeout(request.headers())?;
    let keepalive = keepalive_interval(request.headers());
    let frame = decode_request_frame(request.into_body()).await?;
    if frame.flags != 0 {
        return Err(RpcError::invalid_argument(
            "Start request frame must use flags 0",
        ));
    }
    let request: proto::StartRequest = wire::decode_json(&frame.payload, "Start request")?;
    let request = start_request_from_proto(request);
    let config = request
        .process
        .ok_or_else(|| RpcError::invalid_argument("Start request requires process"))?;
    let launch = state
        .processes
        .start(StartOptions {
            config,
            tag: request.tag,
            keep_stdin: request.stdin.unwrap_or(true),
            pty: request.pty,
            user,
            defaults: state.environment.snapshot(),
            timeout,
        })
        .await?;
    Ok(process_response(
        launch.pid,
        launch.receiver,
        keepalive,
        state.shutdown.clone(),
    ))
}

/// 列出当前仍由 envd 管理的进程。
pub async fn list(
    axum::extract::State(state): axum::extract::State<AppState>,
    request: Request,
) -> Result<Response, RpcError> {
    require_unary(request.headers())?;
    request_user(request.headers())
        .map_err(|error| RpcError::new(Code::Unauthenticated, error.to_string()))?;
    let processes = state.processes.list().await;
    Ok(axum::Json(proto::ListResponse { processes }).into_response())
}

/// 订阅存活进程输出或回放刚结束进程的事件。
pub async fn connect(
    axum::extract::State(state): axum::extract::State<AppState>,
    request: Request,
) -> Result<Response, RpcError> {
    require_streaming(request.headers())?;
    request_user(request.headers())
        .map_err(|error| RpcError::new(Code::Unauthenticated, error.to_string()))?;
    let keepalive = keepalive_interval(request.headers());
    let frame = decode_request_frame(request.into_body()).await?;
    let request: proto::ConnectRequest = wire::decode_json(&frame.payload, "Connect request")?;
    let request = selector_request_from_proto(request.process);
    match state.processes.subscribe(request.process.as_ref()).await? {
        Subscription::Live { pid, receiver } => Ok(process_response(
            pid,
            receiver,
            keepalive,
            state.shutdown.clone(),
        )),
        Subscription::Terminal(record) => Ok(terminal_response(record)),
    }
}

/// 向指定进程的一元 stdin 或 PTY 通道写入数据。
pub async fn send_input(
    axum::extract::State(state): axum::extract::State<AppState>,
    request: Request,
) -> Result<Response, RpcError> {
    let (_, body) = unary_with_user(request).await?;
    let request: proto::SendInputRequest = wire::decode_json(&body, "SendInput request")?;
    let request = send_input_request_from_proto(request);
    write_input(&state.processes, request.process.as_ref(), request.input).await?;
    Ok(axum::Json(proto::SendInputResponse {}).into_response())
}

/// 消费多帧 Connect 客户端流并持续写入进程输入。
pub async fn stream_input(
    axum::extract::State(state): axum::extract::State<AppState>,
    request: Request,
) -> Result<Response, RpcError> {
    require_streaming(request.headers())?;
    request_user(request.headers())
        .map_err(|error| RpcError::new(Code::Unauthenticated, error.to_string()))?;
    let mut frames = RequestFrameReader::new(request.into_body());
    let mut selector = None;
    while let Some(frame) = frames.next_frame().await? {
        if frame.flags != 0 {
            return Err(RpcError::invalid_argument(
                "StreamInput request frames must use flags 0",
            ));
        }
        let frame: proto::StreamInputRequest =
            wire::decode_json(&frame.payload, "StreamInput request")?;
        let frame = stream_input_request_from_proto(frame);
        match (frame.start, frame.data, frame.keepalive) {
            (Some(start), None, None) if selector.is_none() => {
                selector = Some(start.process.ok_or_else(|| {
                    RpcError::invalid_argument("StreamInput start requires process")
                })?);
            }
            (None, Some(data), None) => {
                let selector = selector.as_ref().ok_or_else(|| {
                    RpcError::invalid_argument("StreamInput data requires a preceding start frame")
                })?;
                write_input(&state.processes, Some(selector), data.input).await?;
            }
            (None, None, Some(_)) if selector.is_some() => {}
            (Some(_), _, _) => {
                return Err(RpcError::invalid_argument(
                    "StreamInput start frame may appear only once",
                ))
            }
            _ => return Err(RpcError::invalid_argument("invalid StreamInput frame")),
        }
    }
    if selector.is_none() {
        return Err(RpcError::invalid_argument(
            "StreamInput requires a start frame",
        ));
    }

    Ok(axum::Json(proto::StreamInputResponse {}).into_response())
}

/// 关闭普通进程的 stdin，PTY 进程不支持此操作。
pub async fn close_stdin(
    axum::extract::State(state): axum::extract::State<AppState>,
    request: Request,
) -> Result<Response, RpcError> {
    let (_, body) = unary_with_user(request).await?;
    let request: proto::CloseStdinRequest = wire::decode_json(&body, "CloseStdin request")?;
    let request = selector_request_from_proto(request.process);
    let handle = state.processes.get_live(request.process.as_ref()).await?;
    let mut input = handle.input.lock().await;
    match &*input {
        ProcessInput::Stdin(_) => *input = ProcessInput::Closed,
        ProcessInput::Pty(_) => {
            return Err(RpcError::invalid_argument(
                "CloseStdin does not apply to PTY processes",
            ))
        }
        ProcessInput::Closed => {}
    }
    Ok(axum::Json(proto::CloseStdinResponse {}).into_response())
}

/// 向指定进程组发送 SIGTERM 或 SIGKILL。
pub async fn send_signal(
    axum::extract::State(state): axum::extract::State<AppState>,
    request: Request,
) -> Result<Response, RpcError> {
    let (_, body) = unary_with_user(request).await?;
    let request: proto::SendSignalRequest = wire::decode_json(&body, "SendSignal request")?;
    let request = send_signal_request_from_proto(request);
    let handle = state.processes.get_live(request.process.as_ref()).await?;
    let signal = match request.signal.as_str() {
        "SIGNAL_SIGTERM" => libc::SIGTERM,
        "SIGNAL_SIGKILL" => libc::SIGKILL,
        _ => {
            return Err(RpcError::unimplemented(
                "only SIGNAL_SIGTERM and SIGNAL_SIGKILL are supported",
            ))
        }
    };
    send_group_signal(handle.pid, signal)
        .map_err(|error| RpcError::new(Code::Internal, format!("signal process group: {error}")))?;
    Ok(axum::Json(proto::SendSignalResponse {}).into_response())
}

/// 更新 PTY 进程的终端尺寸。
pub async fn update(
    axum::extract::State(state): axum::extract::State<AppState>,
    request: Request,
) -> Result<Response, RpcError> {
    let (_, body) = unary_with_user(request).await?;
    let request: proto::UpdateRequest = wire::decode_json(&body, "Update request")?;
    let request = update_request_from_proto(request);
    let handle = state.processes.get_live(request.process.as_ref()).await?;
    let pty = handle
        .pty
        .as_ref()
        .ok_or_else(|| RpcError::invalid_argument("Update requires a PTY process"))?;
    let size = parse_pty_size(request.pty)?;
    let pty = Arc::clone(pty);
    tokio::task::spawn_blocking(move || {
        pty.lock()
            .map_err(|_| RpcError::new(Code::Internal, "PTY mutex poisoned"))?
            .resize(size)
            .map_err(|error| RpcError::new(Code::Internal, format!("resize PTY: {error}")))
    })
    .await
    .map_err(|error| RpcError::new(Code::Internal, format!("join PTY resize task: {error}")))??;
    Ok(axum::Json(proto::UpdateResponse {}).into_response())
}

// 提供进程生命周期、标签和订阅的协调操作。

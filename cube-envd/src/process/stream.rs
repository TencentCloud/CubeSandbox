use std::{
    future::Future,
    io::{Read, Write},
    pin::Pin,
    sync::Arc,
    time::Duration,
};

use axum::{
    body::Body,
    extract::Request,
    response::{IntoResponse, Response},
};
use bytes::Bytes;
use futures_util::stream::Stream;
use portable_pty::{CommandBuilder, PtySize};
use serde::Serialize;
use tokio::{
    io::{AsyncRead, AsyncReadExt, AsyncWriteExt},
    process::Command,
    sync::broadcast,
    time,
};
use tokio_stream::wrappers::{errors::BroadcastStreamRecvError, BroadcastStream};

use crate::{
    auth::{request_user, LocalUser},
    connect::{encode_frame, end_stream, require_unary, Code, RpcError},
    generated::process as proto,
    paths::resolve_path,
    wire,
};

use super::model::{
    EndEvent, Input, OUTPUT_CHUNK_BYTES, PTY_OUTPUT_CHUNK_BYTES, ProcessConfig, ProcessEvent,
    ProcessInput, ProcessRegistry, PtyRequest, Selector, TerminalRecord,
};

/// 启动异步任务读取普通进程的 stdout 或 stderr，并返回等待输出耗尽的句柄。
pub(super) fn spawn_reader<R>(
    mut reader: R,
    sender: broadcast::Sender<ProcessEvent>,
    stdout: bool,
) -> tokio::task::JoinHandle<()>
where
    R: AsyncRead + Unpin + Send + 'static,
{
    tokio::spawn(async move {
        let mut buffer = vec![0; OUTPUT_CHUNK_BYTES];
        loop {
            match reader.read(&mut buffer).await {
                Ok(0) | Err(_) => return,
                Ok(size) => {
                    let event = if stdout {
                        ProcessEvent::Stdout(buffer[..size].to_vec())
                    } else {
                        ProcessEvent::Stderr(buffer[..size].to_vec())
                    };
                    let _ = sender.send(event);
                }
            }
        }
    })
}

/// 在线程池中阻塞读取 PTY 输出，并返回等待输出耗尽的句柄。
pub(super) fn spawn_pty_reader(
    mut reader: Box<dyn Read + Send>,
    sender: broadcast::Sender<ProcessEvent>,
) -> tokio::task::JoinHandle<()> {
    tokio::task::spawn_blocking(move || {
        let mut buffer = vec![0; PTY_OUTPUT_CHUNK_BYTES];
        loop {
            match reader.read(&mut buffer) {
                Ok(0) | Err(_) => return,
                Ok(size) => {
                    let _ = sender.send(ProcessEvent::Pty(buffer[..size].to_vec()));
                }
            }
        }
    })
}

/// 将 Tokio 子进程回收结果转换为协议结束事件。
pub(super) fn end_event(result: std::io::Result<std::process::ExitStatus>) -> EndEvent {
    match result {
        Ok(status) => {
            #[cfg(unix)]
            if let Some(signal) = std::os::unix::process::ExitStatusExt::signal(&status) {
                return EndEvent {
                    exit_code: 128 + signal,
                    exited: false,
                    status: format!("terminated by signal {signal}"),
                    error: Some(format!("terminated by signal {signal}")),
                };
            }
            let code = status.code().unwrap_or(-1);
            EndEvent {
                exit_code: code,
                exited: true,
                status: format!("exit status {code}"),
                error: None,
            }
        }
        Err(error) => EndEvent {
            exit_code: -1,
            exited: false,
            status: "failed to reap process".into(),
            error: Some(error.to_string()),
        },
    }
}

/// 将 portable-pty 的退出状态转换为协议结束事件。
pub(super) fn pty_end_event(status: portable_pty::ExitStatus) -> EndEvent {
    let code = status.exit_code() as i32;
    EndEvent {
        exit_code: code,
        exited: status.signal().is_none(),
        status: status.to_string(),
        error: status.signal().map(str::to_owned),
    }
}

/// 校验请求中的 PTY 尺寸并转换为 portable-pty 类型。
pub(super) fn parse_pty_size(pty: Option<PtyRequest>) -> Result<PtySize, RpcError> {
    let size = pty
        .and_then(|pty| pty.size)
        .ok_or_else(|| RpcError::invalid_argument("PTY requires size"))?;
    let rows =
        u16::try_from(size.rows).map_err(|_| RpcError::invalid_argument("PTY rows exceed u16"))?;
    let cols =
        u16::try_from(size.cols).map_err(|_| RpcError::invalid_argument("PTY cols exceed u16"))?;
    if rows == 0 || cols == 0 {
        return Err(RpcError::invalid_argument(
            "PTY rows and cols must be positive",
        ));
    }
    Ok(PtySize {
        rows,
        cols,
        pixel_width: 0,
        pixel_height: 0,
    })
}

/// 按执行用户的路径规则解析可选工作目录。
pub(super) fn process_cwd(
    config: &ProcessConfig,
    user: &LocalUser,
) -> Result<Option<std::path::PathBuf>, RpcError> {
    config
        .cwd
        .as_deref()
        .map(|path| {
            resolve_path(path, user).map_err(|error| RpcError::invalid_argument(error.to_string()))
        })
        .transpose()
}

/// 判断目标用户是否就是当前 envd 进程的用户。
fn same_identity(user: &LocalUser) -> bool {
    nix::unistd::getuid().as_raw() == user.uid && nix::unistd::getgid().as_raw() == user.gid
}

/// 为普通管道进程构建清空环境且已切换用户的 Tokio Command。
pub(super) fn pipe_command(
    config: &ProcessConfig,
    defaults: std::collections::BTreeMap<String, String>,
    cwd: Option<&std::path::Path>,
    user: &LocalUser,
) -> Command {
    let mut command = if same_identity(user) {
        let mut command = Command::new(&config.cmd);
        command.args(&config.args);
        command
    } else {
        let mut command = Command::new("/usr/bin/setpriv");
        command
            .arg(format!("--reuid={}", user.uid))
            .arg(format!("--regid={}", user.gid))
            .arg("--init-groups")
            .arg("--")
            .arg(&config.cmd)
            .args(&config.args);
        command
    };
    command.env_clear().envs(defaults).envs(&config.envs);
    if let Some(cwd) = cwd {
        command.current_dir(cwd);
    }
    command
}

/// 为 PTY 子进程构建清空环境且已切换用户的 CommandBuilder。
pub(super) fn pty_command(
    config: &ProcessConfig,
    defaults: &std::collections::BTreeMap<String, String>,
    cwd: Option<&std::path::Path>,
    user: &LocalUser,
) -> CommandBuilder {
    let mut command = if same_identity(user) {
        let mut command = CommandBuilder::new(&config.cmd);
        command.args(&config.args);
        command
    } else {
        let mut command = CommandBuilder::new("/usr/bin/setpriv");
        command.args([
            format!("--reuid={}", user.uid),
            format!("--regid={}", user.gid),
            "--init-groups".into(),
            "--".into(),
            config.cmd.clone(),
        ]);
        command.args(&config.args);
        command
    };
    command.env_clear();
    for (key, value) in defaults.iter().chain(config.envs.iter()) {
        command.env(key, value);
    }
    if let Some(cwd) = cwd {
        command.cwd(cwd.as_os_str());
    }
    command
}

/// 从 Connect-Timeout-Ms 请求头解析可选进程超时。
pub(super) fn parse_timeout(headers: &axum::http::HeaderMap) -> Result<Option<Duration>, RpcError> {
    let Some(value) = headers.get("Connect-Timeout-Ms") else {
        return Ok(None);
    };
    let milliseconds = value
        .to_str()
        .ok()
        .and_then(|value| value.parse::<u64>().ok())
        .ok_or_else(|| {
            RpcError::invalid_argument("Connect-Timeout-Ms must be an unsigned integer")
        })?;
    Ok((milliseconds > 0).then(|| Duration::from_millis(milliseconds)))
}

/// 校验一元请求、解析执行用户并读取有大小限制的 JSON 主体。
pub(super) async fn unary_with_user(request: Request) -> Result<(LocalUser, Bytes), RpcError> {
    require_unary(request.headers())?;
    let user = request_user(request.headers())
        .map_err(|error| RpcError::new(Code::Unauthenticated, error.to_string()))?;
    let body = axum::body::to_bytes(request.into_body(), crate::connect::MAX_UNARY_JSON_BYTES)
        .await
        .map_err(|_| RpcError::new(Code::ResourceExhausted, "unary JSON request exceeds 1 MiB"))?;
    Ok((user, body))
}

/// 校验输入 oneof 并将已解码的 protobuf bytes 写入匹配的 stdin 或 PTY 通道。
pub(super) async fn write_input(
    registry: &ProcessRegistry,
    selector: Option<&Selector>,
    input: Option<Input>,
) -> Result<(), RpcError> {
    let handle = registry.get_live(selector).await?;
    let input = input.ok_or_else(|| RpcError::invalid_argument("SendInput requires input"))?;
    let (bytes, expects_pty) = match (input.stdin, input.pty) {
        (Some(stdin), None) => (stdin, false),
        (None, Some(pty)) => (pty, true),
        _ => {
            return Err(RpcError::invalid_argument(
                "SendInput requires exactly one of stdin or pty",
            ))
        }
    };
    let mut input = handle.input.lock().await;
    match &mut *input {
        ProcessInput::Stdin(_) if expects_pty => Err(RpcError::invalid_argument(
            "PTY input requires a PTY process",
        )),
        ProcessInput::Pty(_) if !expects_pty => Err(RpcError::invalid_argument(
            "stdin input requires a non-PTY process",
        )),
        ProcessInput::Stdin(stdin) => stdin.write_all(&bytes).await.map_err(|error| {
            RpcError::new(Code::Internal, format!("write process stdin: {error}"))
        }),
        ProcessInput::Pty(writer) => {
            let writer = Arc::clone(writer);
            drop(input);
            tokio::task::spawn_blocking(move || {
                let mut writer = writer
                    .lock()
                    .map_err(|_| RpcError::new(Code::Internal, "PTY writer mutex poisoned"))?;
                writer
                    .write_all(&bytes)
                    .and_then(|()| writer.flush())
                    .map_err(|error| {
                        RpcError::new(Code::Internal, format!("write PTY input: {error}"))
                    })
            })
            .await
            .map_err(|error| {
                RpcError::new(Code::Internal, format!("join PTY input task: {error}"))
            })?
        }
        ProcessInput::Closed => Err(RpcError::invalid_argument("process stdin is closed")),
    }
}

/// 向由进程组组长 PID 标识的整个进程组发送信号。
pub(super) fn send_group_signal(pid: u32, signal: i32) -> nix::Result<()> {
    nix::sys::signal::kill(
        nix::unistd::Pid::from_raw(-(pid as i32)),
        nix::sys::signal::Signal::try_from(signal).expect("valid signal"),
    )
}

/// 将进程事件订阅包装为 Connect 流式 HTTP 响应。
pub(super) fn process_response(
    pid: u32,
    receiver: broadcast::Receiver<ProcessEvent>,
    keepalive: Duration,
    shutdown: tokio_util::sync::CancellationToken,
) -> Response {
    let stream = ProcessStream {
        start: Some(pid),
        receiver: BroadcastStream::new(receiver),
        shutdown_wait: Box::pin(shutdown.cancelled_owned()),
        keepalive,
        keepalive_sleep: Box::pin(time::sleep(keepalive)),
        terminal: false,
        end_sent: false,
    };
    (
        [("content-type", "application/connect+json")],
        Body::from_stream(stream),
    )
        .into_response()
}

/// 将缓存的结束记录包装为包含 start、end 和结束帧的短流响应。
pub(super) fn terminal_response(record: TerminalRecord) -> Response {
    let stream = TerminalStream {
        pid: record.pid,
        event: Some(record.event),
        state: 0,
    };
    (
        [("content-type", "application/connect+json")],
        Body::from_stream(stream),
    )
        .into_response()
}

/// 将存活进程广播事件转换为 Connect 服务端流。
struct ProcessStream {
    /// 尚未发送的初始 PID 事件。
    start: Option<u32>,
    /// 接收进程输出和结束事件的广播流。
    receiver: BroadcastStream<ProcessEvent>,
    /// 等待应用全局关闭信号。
    shutdown_wait: Pin<Box<dyn Future<Output = ()> + Send>>,
    /// 两次保活事件之间的间隔。
    keepalive: Duration,
    /// 调度下一次保活事件的计时器。
    keepalive_sleep: Pin<Box<time::Sleep>>,
    /// 是否已经收到进程结束事件。
    terminal: bool,
    /// 是否已经发送 Connect 结束帧。
    end_sent: bool,
}

/// 实现 Connect 响应体所需的异步字节流。
impl Stream for ProcessStream {
    type Item = Result<Bytes, std::convert::Infallible>;

    /// 按 start、结束、关闭、保活和广播事件的优先级产出下一帧。
    fn poll_next(
        mut self: std::pin::Pin<&mut Self>,
        context: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<Self::Item>> {
        if let Some(pid) = self.start.take() {
            return std::task::Poll::Ready(Some(Ok(Bytes::from(encode_output(
                &start_response(pid),
            )))));
        }
        if self.terminal && !self.end_sent {
            self.end_sent = true;
            return std::task::Poll::Ready(Some(Ok(Bytes::from(end_stream(None)))));
        }
        if self.end_sent {
            return std::task::Poll::Ready(None);
        }
        if self.shutdown_wait.as_mut().poll(context).is_ready() {
            self.end_sent = true;
            return std::task::Poll::Ready(Some(Ok(Bytes::from(end_stream(None)))));
        }
        if self.keepalive_sleep.as_mut().poll(context).is_ready() {
            let keepalive = self.keepalive;
            self.keepalive_sleep
                .as_mut()
                .reset(time::Instant::now() + keepalive);
            return std::task::Poll::Ready(Some(Ok(Bytes::from(encode_output(
                &keepalive_response(),
            )))));
        }
        match std::pin::Pin::new(&mut self.receiver).poll_next(context) {
            std::task::Poll::Ready(Some(Ok(event))) => {
                let terminal = matches!(event, ProcessEvent::End(_));
                let payload = event_response(event);
                let keepalive = self.keepalive;
                self.keepalive_sleep
                    .as_mut()
                    .reset(time::Instant::now() + keepalive);
                if terminal {
                    self.terminal = true;
                }
                std::task::Poll::Ready(Some(Ok(Bytes::from(encode_output(&payload)))))
            }
            std::task::Poll::Ready(Some(Err(BroadcastStreamRecvError::Lagged(_)))) => {
                self.end_sent = true;
                std::task::Poll::Ready(Some(Ok(Bytes::from(end_stream(Some(RpcError::new(
                    Code::ResourceExhausted,
                    "process subscriber is too slow",
                )))))))
            }
            std::task::Poll::Ready(None) => std::task::Poll::Pending,
            std::task::Poll::Pending => std::task::Poll::Pending,
        }
    }
}

/// 为已结束进程回放固定的 start、end 和流结束帧序列。
struct TerminalStream {
    /// 已结束进程的 PID。
    pid: u32,
    /// 尚未回放的结束事件。
    event: Option<ProcessEvent>,
    /// 当前回放阶段。
    state: u8,
}

/// 实现已结束进程的有限 Connect 回放流。
impl Stream for TerminalStream {
    type Item = Result<Bytes, std::convert::Infallible>;
    /// 依次生成 start、结束事件和 Connect 结束帧。
    fn poll_next(
        mut self: std::pin::Pin<&mut Self>,
        _: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<Self::Item>> {
        let message = match self.state {
            0 => {
                self.state = 1;
                Some(encode_output(
                    &start_response(self.pid),
                ))
            }
            1 => {
                self.state = 2;
                self.event
                    .take()
                    .map(|event| encode_output(&event_response(event)))
            }
            2 => {
                self.state = 3;
                Some(end_stream(None))
            }
            _ => None,
        };
        std::task::Poll::Ready(message.map(|message| Ok(Bytes::from(message))))
    }
}

/// 构造由生成 protobuf 类型表示的进程启动事件。
fn start_response(pid: u32) -> proto::StartResponse {
    proto::StartResponse {
        event: Some(proto::ProcessEvent {
            event: Some(proto::process_event::Event::Start(
                proto::process_event::StartEvent { pid },
            )),
        }),
    }
}

/// 构造由生成 protobuf 类型表示的进程流保活事件。
fn keepalive_response() -> proto::StartResponse {
    proto::StartResponse {
        event: Some(proto::ProcessEvent {
            event: Some(proto::process_event::Event::Keepalive(
                proto::process_event::KeepAlive {},
            )),
        }),
    }
}

/// 将内部进程事件转换为由生成 protobuf 类型表示的流响应。
fn event_response(event: ProcessEvent) -> proto::StartResponse {
    let event = match event {
        ProcessEvent::Stdout(bytes) => proto::process_event::Event::Data(
            proto::process_event::DataEvent {
                output: Some(proto::process_event::data_event::Output::Stdout(bytes)),
            },
        ),
        ProcessEvent::Stderr(bytes) => proto::process_event::Event::Data(
            proto::process_event::DataEvent {
                output: Some(proto::process_event::data_event::Output::Stderr(bytes)),
            },
        ),
        ProcessEvent::Pty(bytes) => proto::process_event::Event::Data(
            proto::process_event::DataEvent {
                output: Some(proto::process_event::data_event::Output::Pty(bytes)),
            },
        ),
        ProcessEvent::End(end) => proto::process_event::Event::End(
            proto::process_event::EndEvent {
                exit_code: end.exit_code,
                exited: end.exited,
                status: end.status,
                error: end.error,
            },
        ),
    };
    proto::StartResponse {
        event: Some(proto::ProcessEvent { event: Some(event) }),
    }
}

/// 将生成的 protobuf 流响应编码为非结束 Connect 数据帧。
fn encode_output<T>(payload: &T) -> Vec<u8>
where
    T: Serialize,
{
    let payload = wire::encode_json(payload).expect("serialize process event");
    encode_frame(0, &payload).expect("bounded process frame")
}

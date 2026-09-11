// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

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
    time,
};

use crate::{
    auth::{request_user, LocalUser},
    connect::{encode_frame, end_stream, require_unary, Code, RpcError},
    generated::process as proto,
    paths::resolve_path,
    wire,
};

use super::{
    fanout::{OutputFanout, OutputSubscription},
    model::{
        EndEvent, Input, ProcessConfig, ProcessEvent, ProcessInput, ProcessRegistry, PtyRequest,
        Selector, TerminalRecord, OUTPUT_CHUNK_BYTES, PTY_OUTPUT_CHUNK_BYTES,
    },
};

/// 启动异步任务读取普通进程的 stdout 或 stderr，并返回等待输出耗尽的句柄。
pub(super) fn spawn_reader<R>(
    mut reader: R,
    fanout: OutputFanout,
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
                    fanout.send(event).await;
                }
            }
        }
    })
}

/// 在专用 OS 线程中阻塞读取 PTY 输出。
///
/// 投递经 `Handle::block_on` 走扇出器：慢订阅者时该线程停读 PTY，PTY 缓冲随之填满，
/// 把背压传导给子进程。
///
/// 之所以用专用线程而不是 `spawn_blocking`：子进程已回收但孙进程仍持有 PTY slave
/// 时，master 读端会一直阻塞，阻塞线程无法被 `abort()` 中断。若这类线程落在 tokio
/// 的阻塞池里，重复启动 `ptyt 命令 &` 就能耗尽池槽位，从而拖垮所有依赖阻塞池的操作
/// （PTY 创建/写入/resize、上传 chown 等）。专用线程把影响限制为"每个被 pin 的 PTY
/// 一个线程"，不再挤占共享池。
///
/// `interrupt` 置位后线程会在下一次循环检查时退出（无法中断阻塞中的 read，但配合
/// `done` 通知，reaper 无需等待它即可继续收尾）。退出前通过 `done` 唤醒等待者。
pub(super) fn spawn_pty_reader(
    mut reader: Box<dyn Read + Send>,
    interrupt: Arc<std::sync::atomic::AtomicBool>,
    done: Arc<tokio::sync::Notify>,
    fanout: OutputFanout,
) {
    let runtime = tokio::runtime::Handle::current();
    let thread_done = Arc::clone(&done);
    let spawned = std::thread::Builder::new()
        .name("cube-envd-pty".into())
        .spawn(move || {
            let mut buffer = vec![0; PTY_OUTPUT_CHUNK_BYTES];
            loop {
                if interrupt.load(std::sync::atomic::Ordering::Relaxed) {
                    break;
                }
                match reader.read(&mut buffer) {
                    Ok(0) | Err(_) => break,
                    Ok(size) => {
                        runtime.block_on(fanout.send(ProcessEvent::Pty(buffer[..size].to_vec())));
                    }
                }
            }
            thread_done.notify_one();
        });

    if spawned.is_err() {
        // 线程创建失败（极端资源枯竭）：立即唤醒等待者，让收尾流程继续。
        done.notify_one();
    }
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

/// 等待 PTY 子进程结束并转换为终态事件。
///
/// portable-pty 的 `ExitStatus` 没有信号编号：信号终止时把 `code` 压成 1、只留下
/// 一个本地化信号名。因此这里优先 downcast 回 `std::process::Child` 取原始退出
/// 状态（portable-pty 的 unix 后端就是用它承载子进程的），使 PTY 与管道路径共用
/// 同一套终态形状——信号终止统一上报 `128 + N`。downcast 失败时退回 portable-pty
/// 的转换结果。
pub(super) fn wait_pty_child(
    mut child: Box<dyn portable_pty::Child + Send + Sync>,
) -> std::io::Result<EndEvent> {
    let child_ref: &mut dyn portable_pty::Child = &mut *child;
    if let Some(std_child) = child_ref.downcast_mut::<std::process::Child>() {
        return std_child.wait().map(|status| end_event(Ok(status)));
    }

    child.wait().map(pty_end_event)
}

/// 将 portable-pty 的退出状态转换为协议结束事件。
///
/// 仅在无法取得原始退出状态时使用（见 [`wait_pty_child`]）：信号终止只能退化为
/// portable-pty 提供的描述性文案。
pub(super) fn pty_end_event(status: portable_pty::ExitStatus) -> EndEvent {
    match status.signal() {
        Some(name) => EndEvent {
            exit_code: status.exit_code() as i32,
            exited: false,
            status: format!("terminated by signal {name}"),
            error: Some(format!("terminated by signal {name}")),
        },
        None => {
            let code = status.exit_code() as i32;
            EndEvent {
                exit_code: code,
                exited: true,
                status: format!("exit status {code}"),
                error: None,
            }
        }
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

/// 按执行用户的路径规则解析工作目录，缺省时回落到该用户的主目录。
///
/// 上游 envd 在 cwd 为空时使用 `/init` 的 defaultWorkdir，未设置时回落到用户
/// HOME；cube-envd 不实现 defaultWorkdir，因此缺省值恒为 HOME——而不是继承
/// cube-envd 自身进程的工作目录。
pub(super) fn process_cwd(
    config: &ProcessConfig,
    user: &LocalUser,
) -> Result<Option<std::path::PathBuf>, RpcError> {
    let Some(path) = config.cwd.as_deref() else {
        return Ok(Some(user.home.clone()));
    };
    resolve_path(path, user)
        .map(Some)
        .map_err(|error| RpcError::invalid_argument(error.to_string()))
}

/// 判断目标用户是否就是当前 envd 进程的用户。
fn same_identity(user: &LocalUser) -> bool {
    nix::unistd::getuid().as_raw() == user.uid && nix::unistd::getgid().as_raw() == user.gid
}

/// envd 自身未设置 PATH 时使用的兜底搜索路径。
const FALLBACK_PATH: &str = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin";

/// 构造上游 envd 语义的基础环境：PATH 取 envd 自身的值，HOME/USER/LOGNAME 取
/// 目标用户的 passwd 条目；随后由 `/init` 快照与请求级 `envs` 依次覆盖。
///
/// 上游取 `os.Getenv("PATH")` 并原样注入（含未设置时的空值）；这里唯一的有意
/// 差别是在 envd 自身 PATH 缺失时退化为标准搜索路径，避免子进程完全无法查找
/// 可执行文件。
fn base_environment(user: &LocalUser) -> Vec<(&'static str, String)> {
    let path = std::env::var("PATH").unwrap_or_else(|_| FALLBACK_PATH.to_owned());
    vec![
        ("PATH", path),
        ("HOME", user.home.display().to_string()),
        ("USER", user.name.clone()),
        ("LOGNAME", user.name.clone()),
    ]
}

/// `setpriv` 的候选绝对路径，按优先级排列。
///
/// 刻意不做 `PATH` 查找：守护进程以 root 运行，`PATH` 可能被注入；而且 Alpine 等
/// 镜像的 `PATH` 会先命中 busybox 自带的同名 applet（见
/// [`supports_credential_switching`]），那正是本实现不能用的那个。
const SETPRIV_CANDIDATES: &[&str] = &[
    "/usr/bin/setpriv",
    "/bin/setpriv",
    "/sbin/setpriv",
    "/usr/sbin/setpriv",
];

/// 判断候选文件是否为支持 `--reuid/--regid/--init-groups` 的可用 `setpriv`。
///
/// 只看文件名不够：busybox 与 Alpine 默认镜像在 `/bin/setpriv` 提供同名 applet，
/// 它只支持 capabilities 相关选项，遇到 `--reuid` 会以 “unrecognized option” 失败。
/// 这里直接问它自己是否认识这三个参数。
pub(super) fn supports_credential_switching(candidate: &std::path::Path) -> bool {
    if !candidate.is_file() {
        return false;
    }
    let Ok(output) = std::process::Command::new(candidate).arg("--help").output() else {
        return false;
    };
    let mut text = output.stdout;
    text.extend_from_slice(&output.stderr);
    let text = String::from_utf8_lossy(&text);
    ["--reuid", "--regid", "--init-groups"]
        .iter()
        .all(|flag| text.contains(flag))
}

/// 解析可用于切换凭据的 `setpriv`，进程内只探测一次。
///
/// 上游 Go envd 通过 `SysProcAttr.Credential` 在进程内完成切换，不需要外部程序；
/// Rust 稳定版只暴露 `CommandExt::uid/gid`（`groups` 仍是 unstable），而 PTY 路径
/// 使用的 portable-pty 不提供任何凭据钩子，因此这里必须委派外部助手。
fn credential_helper() -> Option<&'static std::path::Path> {
    static RESOLVED: std::sync::OnceLock<Option<std::path::PathBuf>> = std::sync::OnceLock::new();
    RESOLVED
        .get_or_init(|| {
            SETPRIV_CANDIDATES
                .iter()
                .map(std::path::Path::new)
                .find(|candidate| supports_credential_switching(candidate))
                .map(std::path::Path::to_path_buf)
        })
        .as_deref()
}

/// 镜像里没有可用的 `setpriv` 时给出的可定位错误。
///
/// 默认只会在 spawn 处冒出一个 `No such file or directory (os error 2)`，调用方
/// 无从得知是缺包还是路径不对，这里点名缺失的工具与需要安装的包。
fn missing_credential_helper() -> RpcError {
    RpcError::invalid_argument(
        "switching users requires a util-linux setpriv supporting \
         --reuid/--regid/--init-groups, but none was found in \
         /usr/bin, /bin, /sbin or /usr/sbin; install util-linux in the sandbox image \
         (on Alpine: apk add util-linux; the busybox setpriv applet is not sufficient)",
    )
}

/// 按目标用户解析凭据助手：同身份时返回 `None`（不 exec 任何助手，也不做探测）。
pub(super) fn credential_helper_for(
    user: &LocalUser,
) -> Result<Option<&'static std::path::Path>, RpcError> {
    if same_identity(user) {
        return Ok(None);
    }
    credential_helper()
        .map(Some)
        .ok_or_else(missing_credential_helper)
}

/// 为普通管道进程构建清空环境且已切换用户的 Tokio Command。
pub(super) fn pipe_command(
    config: &ProcessConfig,
    defaults: std::collections::BTreeMap<String, String>,
    cwd: Option<&std::path::Path>,
    user: &LocalUser,
    helper: Option<&std::path::Path>,
) -> Command {
    let mut command = match helper {
        Some(helper) => {
            let mut command = Command::new(helper);
            command
                .arg(format!("--reuid={}", user.uid))
                .arg(format!("--regid={}", user.gid))
                .arg("--init-groups")
                .arg("--")
                .arg(&config.cmd)
                .args(&config.args);
            command
        }
        None => {
            let mut command = Command::new(&config.cmd);
            command.args(&config.args);
            command
        }
    };
    command.env_clear();
    for (key, value) in base_environment(user) {
        command.env(key, value);
    }
    command.envs(defaults).envs(&config.envs);
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
    helper: Option<&std::path::Path>,
) -> CommandBuilder {
    let mut command = match helper {
        Some(helper) => {
            let mut command = CommandBuilder::new(helper);
            command.args([
                format!("--reuid={}", user.uid),
                format!("--regid={}", user.gid),
                "--init-groups".into(),
                "--".into(),
                config.cmd.clone(),
            ]);
            command.args(&config.args);
            command
        }
        None => {
            let mut command = CommandBuilder::new(&config.cmd);
            command.args(&config.args);
            command
        }
    };
    command.env_clear();
    for (key, value) in base_environment(user) {
        command.env(key, value);
    }
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
    receiver: OutputSubscription,
    keepalive: Duration,
    shutdown: tokio_util::sync::CancellationToken,
) -> Response {
    let stream = ProcessStream {
        start: Some(pid),
        receiver,
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

/// 将存活进程事件队列转换为 Connect 服务端流。
struct ProcessStream {
    /// 尚未发送的初始 PID 事件。
    start: Option<u32>,
    /// 接收进程输出和结束事件的订阅队列。
    receiver: OutputSubscription,
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
            return std::task::Poll::Ready(Some(Ok(Bytes::from(encode_output(&start_response(
                pid,
            ))))));
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
        match self.receiver.poll_recv(context) {
            std::task::Poll::Ready(Some(event)) => {
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
            // 队列关闭：正常路径 End 帧已先行发送，此处仅作异常兜底结束流。
            std::task::Poll::Ready(None) => {
                self.end_sent = true;
                std::task::Poll::Ready(Some(Ok(Bytes::from(end_stream(None)))))
            }
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
                Some(encode_output(&start_response(self.pid)))
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
        ProcessEvent::Stdout(bytes) => {
            proto::process_event::Event::Data(proto::process_event::DataEvent {
                output: Some(proto::process_event::data_event::Output::Stdout(bytes)),
            })
        }
        ProcessEvent::Stderr(bytes) => {
            proto::process_event::Event::Data(proto::process_event::DataEvent {
                output: Some(proto::process_event::data_event::Output::Stderr(bytes)),
            })
        }
        ProcessEvent::Pty(bytes) => {
            proto::process_event::Event::Data(proto::process_event::DataEvent {
                output: Some(proto::process_event::data_event::Output::Pty(bytes)),
            })
        }
        ProcessEvent::End(end) => {
            proto::process_event::Event::End(proto::process_event::EndEvent {
                exit_code: end.exit_code,
                exited: end.exited,
                status: end.status,
                error: end.error,
            })
        }
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

use std::{
    collections::HashMap,
    io::Write,
    sync::Arc,
    time::Duration,
};

use portable_pty::MasterPty;
use serde::{Deserialize, Serialize};
use tokio::{
    process::ChildStdin,
    sync::{broadcast, Mutex, RwLock},
    time,
};

use crate::{
    auth::LocalUser,
    generated::process as proto,
};

/// 限制每个进程事件订阅者可积压的输出事件数。
pub(super) const OUTPUT_CAPACITY: usize = 64;
/// 限制普通 stdout 和 stderr 单次读取的最大字节数。
pub(super) const OUTPUT_CHUNK_BYTES: usize = 32 * 1024;
/// 限制 PTY 单次阻塞读取的最大字节数。
pub(super) const PTY_OUTPUT_CHUNK_BYTES: usize = 16 * 1024;
/// 限制已结束进程缓存的最大记录数。
pub(super) const TERMINAL_CACHE_LIMIT: usize = 256;
/// 定义已结束进程可按 PID 或标签重连的缓存时长。
pub(super) const TERMINAL_CACHE_TTL: Duration = Duration::from_secs(60);
/// 定义优雅关闭时 TERM 和 KILL 之间的等待时长。
pub(super) const SHUTDOWN_GRACE: Duration = Duration::from_secs(2);

#[derive(Clone, Default)]
/// 管理存活进程、标签保留和已结束进程缓存。
pub struct ProcessRegistry {
    /// 以 PID 索引当前存活进程。
    pub(crate) live: Arc<RwLock<HashMap<u32, Arc<ProcessHandle>>>>,
    /// 将唯一标签映射到存活或已保留进程的 PID。
    pub(crate) tags: Arc<RwLock<HashMap<String, u32>>>,
    /// 暂存刚结束进程的结束事件以支持短暂重连。
    pub(crate) terminal: Arc<Mutex<Vec<TerminalRecord>>>,
}

/// 保存一个存活进程的控制句柄和输出订阅通道。
pub(crate) struct ProcessHandle {
    /// 进程组组长 PID。
    pub(crate) pid: u32,
    /// 调用方提供的可选唯一标签。
    pub(crate) tag: Option<String>,
    /// 启动进程时使用的配置快照。
    pub(crate) config: ProcessConfig,
    /// 管理 stdin 或 PTY 写端的互斥输入通道。
    pub(crate) input: Mutex<ProcessInput>,
    /// PTY 进程的主端，用于调整终端大小。
    pub(crate) pty: Option<Arc<std::sync::Mutex<Box<dyn MasterPty + Send>>>>,
    /// 广播输出和结束事件给所有订阅者。
    pub(crate) output: broadcast::Sender<ProcessEvent>,
}

/// 表示进程当前可写入的输入端或已关闭状态。
pub(crate) enum ProcessInput {
    /// 普通子进程的异步 stdin。
    Stdin(ChildStdin),
    /// PTY 子进程的同步写端。
    Pty(Arc<std::sync::Mutex<Box<dyn Write + Send>>>),
    /// 不再允许写入输入。
    Closed,
}

#[derive(Clone)]
/// 保存最近结束进程的事件及其过期时间。
pub(crate) struct TerminalRecord {
    /// 已结束进程的 PID。
    pub(super) pid: u32,
    /// 已结束进程的可选标签。
    pub(super) tag: Option<String>,
    /// 供重连客户端回放的结束事件。
    pub(super) event: ProcessEvent,
    /// 该缓存记录的失效时间。
    pub(super) expires: time::Instant,
}

#[derive(Clone, Deserialize, Serialize)]
/// 表示启动或列出进程时使用的命令配置。
pub(crate) struct ProcessConfig {
    /// 要执行的程序路径或命令。
    pub(super) cmd: String,
    /// 传递给程序的参数列表。
    #[serde(default)]
    pub(super) args: Vec<String>,
    /// 覆盖默认环境变量的进程环境。
    #[serde(default)]
    pub(super) envs: HashMap<String, String>,
    /// 可选的工作目录。
    #[serde(default)]
    pub(super) cwd: Option<String>,
}

#[derive(Deserialize)]
/// 表示 Process.Start 的流式请求内容。
pub(super) struct StartRequest {
    /// 必填的进程命令配置。
    pub(super) process: Option<ProcessConfig>,
    /// 可选的伪终端配置。
    #[serde(default)]
    pub(super) pty: Option<PtyRequest>,
    /// 可选的唯一进程标签。
    #[serde(default)]
    pub(super) tag: Option<String>,
    /// 是否保留普通 stdin 供后续输入。
    #[serde(default)]
    pub(super) stdin: Option<bool>,
}

/// 汇总启动流程内部需要的已验证选项。
pub(super) struct StartOptions {
    /// 要执行的进程配置。
    pub(super) config: ProcessConfig,
    /// 要预留和绑定的可选标签。
    pub(super) tag: Option<String>,
    /// 是否为普通进程创建 stdin 管道。
    pub(super) keep_stdin: bool,
    /// 可选的 PTY 请求。
    pub(super) pty: Option<PtyRequest>,
    /// 要降低权限运行的本地用户。
    pub(super) user: LocalUser,
    /// 从应用环境快照继承的默认变量。
    pub(super) defaults: std::collections::BTreeMap<String, String>,
    /// 可选的最长运行时间。
    pub(super) timeout: Option<Duration>,
}

#[derive(Deserialize)]
/// 表示 PTY 启动或更新请求。
pub(super) struct PtyRequest {
    /// 可选的终端尺寸。
    pub(super) size: Option<PtySizeRequest>,
}

#[derive(Deserialize)]
/// 表示 PTY 的列数和行数。
pub(super) struct PtySizeRequest {
    /// 终端列数。
    pub(super) cols: u32,
    /// 终端行数。
    pub(super) rows: u32,
}

#[derive(Deserialize)]
/// 表示 Process.Update 的选择器和 PTY 尺寸更新。
pub(super) struct UpdateRequest {
    /// 要更新的进程。
    pub(super) process: Option<Selector>,
    /// 要应用的新 PTY 配置。
    pub(super) pty: Option<PtyRequest>,
}

#[derive(Deserialize)]
/// 表示只需定位进程的请求。
pub(super) struct SelectorRequest {
    /// 以 PID 或标签定位的进程。
    pub(super) process: Option<Selector>,
}

#[derive(Deserialize)]
/// 表示 Process.SendInput 的进程和输入数据。
pub(super) struct SendInputRequest {
    /// 要写入的进程。
    pub(super) process: Option<Selector>,
    /// 经过 base64 编码的输入数据。
    pub(super) input: Option<Input>,
}

#[derive(Deserialize)]
/// 表示 Process.SendSignal 的进程和信号名称。
pub(super) struct SendSignalRequest {
    /// 要发送信号的进程。
    pub(super) process: Option<Selector>,
    /// 协议定义的信号枚举名称。
    pub(super) signal: String,
}

#[derive(Deserialize)]
/// 表示写入普通 stdin 或 PTY 的 base64 数据。
pub(super) struct Input {
    /// 普通 stdin 的 base64 数据。
    pub(super) stdin: Option<Vec<u8>>,
    /// PTY 输入的 base64 数据。
    pub(super) pty: Option<Vec<u8>>,
}

#[derive(Deserialize)]
/// 表示 Process.StreamInput 中的一帧客户端事件。
pub(super) struct StreamInputRequest {
    /// 初始化一次流输入会话的开始事件。
    pub(super) start: Option<StreamInputStart>,
    /// 向已选择进程写入数据的事件。
    pub(super) data: Option<StreamInputData>,
    /// 用于维持空闲客户端流的保活事件。
    pub(super) keepalive: Option<serde_json::Value>,
}

#[derive(Deserialize)]
/// 表示 StreamInput 起始帧中的进程选择器。
pub(super) struct StreamInputStart {
    /// 要在后续数据帧中使用的进程。
    pub(super) process: Option<Selector>,
}

#[derive(Deserialize)]
/// 表示 StreamInput 数据帧。
pub(super) struct StreamInputData {
    /// 要写入进程的输入内容。
    pub(super) input: Option<Input>,
}

#[derive(Deserialize)]
/// 以 PID 或标签唯一定位进程。
pub(crate) struct Selector {
    /// 可选的进程 PID。
    pub(crate) pid: Option<u32>,
    /// 可选的进程标签。
    pub(crate) tag: Option<String>,
}

#[derive(Clone)]
/// 表示广播给进程订阅者的输出或结束事件。
pub(crate) enum ProcessEvent {
    /// 普通 stdout 输出片段。
    Stdout(Vec<u8>),
    /// 普通 stderr 输出片段。
    Stderr(Vec<u8>),
    /// PTY 复用输出片段。
    Pty(Vec<u8>),
    /// 进程退出或回收失败事件。
    End(EndEvent),
}

#[derive(Clone)]
/// 描述进程结束状态和可选错误。
pub(crate) struct EndEvent {
    /// 退出码，回收失败时为 -1。
    pub(crate) exit_code: i32,
    /// 是否以正常退出状态结束。
    pub(crate) exited: bool,
    /// 人类可读的退出状态。
    pub(crate) status: String,
    /// 回收或异常结束时的详细错误。
    pub(crate) error: Option<String>,
}

/// 将生成的 protobuf 进程配置转换为注册表使用的领域模型。
fn config_from_proto(config: proto::ProcessConfig) -> ProcessConfig {
    ProcessConfig {
        cmd: config.cmd,
        args: config.args,
        envs: config.envs,
        cwd: config.cwd,
    }
}

/// 将注册表保存的领域配置转换为生成的 protobuf 类型。
pub(super) fn config_to_proto(config: ProcessConfig) -> proto::ProcessConfig {
    proto::ProcessConfig {
        cmd: config.cmd,
        args: config.args,
        envs: config.envs,
        cwd: config.cwd,
    }
}

/// 将生成的 protobuf PTY 配置转换为领域模型。
fn pty_from_proto(pty: proto::Pty) -> PtyRequest {
    PtyRequest {
        size: pty.size.map(|size| PtySizeRequest {
            cols: size.cols,
            rows: size.rows,
        }),
    }
}

/// 将生成的 protobuf 进程选择器转换为领域模型。
fn selector_from_proto(selector: proto::ProcessSelector) -> Selector {
    match selector.selector {
        Some(proto::process_selector::Selector::Pid(pid)) => Selector {
            pid: Some(pid),
            tag: None,
        },
        Some(proto::process_selector::Selector::Tag(tag)) => Selector {
            pid: None,
            tag: Some(tag),
        },
        None => Selector {
            pid: None,
            tag: None,
        },
    }
}

/// 将生成的 protobuf 输入 oneof 转换为领域模型。
fn input_from_proto(input: proto::ProcessInput) -> Input {
    match input.input {
        Some(proto::process_input::Input::Stdin(stdin)) => Input {
            stdin: Some(stdin),
            pty: None,
        },
        Some(proto::process_input::Input::Pty(pty)) => Input {
            stdin: None,
            pty: Some(pty),
        },
        None => Input {
            stdin: None,
            pty: None,
        },
    }
}

/// 将生成的 protobuf 启动请求转换为领域模型。
pub(super) fn start_request_from_proto(request: proto::StartRequest) -> StartRequest {
    StartRequest {
        process: request.process.map(config_from_proto),
        pty: request.pty.map(pty_from_proto),
        tag: request.tag,
        stdin: request.stdin,
    }
}

/// 将生成的 protobuf PTY 更新请求转换为领域模型。
pub(super) fn update_request_from_proto(request: proto::UpdateRequest) -> UpdateRequest {
    UpdateRequest {
        process: request.process.map(selector_from_proto),
        pty: request.pty.map(pty_from_proto),
    }
}

/// 将生成的 protobuf 选择请求转换为领域模型。
pub(super) fn selector_request_from_proto(process: Option<proto::ProcessSelector>) -> SelectorRequest {
    SelectorRequest {
        process: process.map(selector_from_proto),
    }
}

/// 将生成的 protobuf 一元输入请求转换为领域模型。
pub(super) fn send_input_request_from_proto(request: proto::SendInputRequest) -> SendInputRequest {
    SendInputRequest {
        process: request.process.map(selector_from_proto),
        input: request.input.map(input_from_proto),
    }
}

/// 将生成的 protobuf 信号请求转换为领域模型。
pub(super) fn send_signal_request_from_proto(request: proto::SendSignalRequest) -> SendSignalRequest {
    SendSignalRequest {
        process: request.process.map(selector_from_proto),
        signal: proto::Signal::try_from(request.signal)
            .map(|signal| signal.as_str_name().to_owned())
            .unwrap_or_default(),
    }
}

/// 将生成的 protobuf 流输入请求转换为领域模型。
pub(super) fn stream_input_request_from_proto(request: proto::StreamInputRequest) -> StreamInputRequest {
    match request.event {
        Some(proto::stream_input_request::Event::Start(start)) => StreamInputRequest {
            start: Some(StreamInputStart {
                process: start.process.map(selector_from_proto),
            }),
            data: None,
            keepalive: None,
        },
        Some(proto::stream_input_request::Event::Data(data)) => StreamInputRequest {
            start: None,
            data: Some(StreamInputData {
                input: data.input.map(input_from_proto),
            }),
            keepalive: None,
        },
        Some(proto::stream_input_request::Event::Keepalive(_)) => StreamInputRequest {
            start: None,
            data: None,
            keepalive: Some(serde_json::Value::Null),
        },
        None => StreamInputRequest {
            start: None,
            data: None,
            keepalive: None,
        },
    }
}

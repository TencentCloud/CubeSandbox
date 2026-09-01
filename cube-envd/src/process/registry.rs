use std::{
    sync::{Arc, Mutex as StdMutex},
    time::Duration,
};

use portable_pty::{native_pty_system, PtySize};
use tokio::{
    sync::{broadcast, Mutex},
    time,
};

use crate::{
    connect::{Code, RpcError},
    generated::process as proto,
};

use super::{
    model::{
        config_to_proto, EndEvent, OUTPUT_CAPACITY, ProcessEvent, ProcessHandle, ProcessInput,
        ProcessRegistry, Selector, SHUTDOWN_GRACE, StartOptions, TERMINAL_CACHE_LIMIT,
        TERMINAL_CACHE_TTL, TerminalRecord,
    },
    stream::{
        end_event, parse_pty_size, pipe_command, process_cwd, pty_command, pty_end_event,
        send_group_signal, spawn_pty_reader, spawn_reader,
    },
};


impl ProcessRegistry {
    /// 按 TERM 后 KILL 的顺序关闭全部存活进程组。
    pub async fn shutdown(&self) {
        let pids = self.live_pids().await;
        for pid in pids {
            let _ = send_group_signal(pid, libc::SIGTERM);
        }

        if !self.wait_for_empty(SHUTDOWN_GRACE).await {
            for pid in self.live_pids().await {
                let _ = send_group_signal(pid, libc::SIGKILL);
            }
            let _ = self.wait_for_empty(SHUTDOWN_GRACE).await;
        }
    }

    /// 校验并预留标签后启动进程，失败时释放标签保留。
    pub(super) async fn start(&self, options: StartOptions) -> Result<Launch, RpcError> {
        if options.config.cmd.is_empty() {
            return Err(RpcError::invalid_argument("process.cmd must not be empty"));
        }
        if let Some(tag) = &options.tag {
            if tag.is_empty() {
                return Err(RpcError::invalid_argument("process tag must not be empty"));
            }
            if self.tags.write().await.insert(tag.clone(), 0).is_some() {
                return Err(RpcError::invalid_argument(format!(
                    "process tag {tag:?} already exists"
                )));
            }
        }
        let reserved_tag = options.tag.clone();
        let result = self.start_reserved(options).await;
        if result.is_err() {
            self.release_tag_reservation(reserved_tag.as_deref()).await;
        }
        result
    }

    /// 使用已保留的标签启动普通管道进程或 PTY 进程。
    async fn start_reserved(&self, mut options: StartOptions) -> Result<Launch, RpcError> {
        let cwd = process_cwd(&options.config, &options.user)?;
        if let Some(cwd) = &cwd {
            let metadata = tokio::fs::metadata(cwd).await.map_err(|error| {
                RpcError::invalid_argument(format!(
                    "invalid process cwd {}: {error}",
                    cwd.display()
                ))
            })?;
            if !metadata.is_dir() {
                return Err(RpcError::invalid_argument(format!(
                    "process cwd {} is not a directory",
                    cwd.display()
                )));
            }
        }

        let (sender, receiver) = broadcast::channel(OUTPUT_CAPACITY);
        if let Some(pty) = options.pty.take() {
            return self
                .start_pty(options, parse_pty_size(Some(pty))?, sender, receiver)
                .await;
        }
        let mut command = pipe_command(
            &options.config,
            options.defaults,
            cwd.as_deref(),
            &options.user,
        );
        command
            .stdin(if options.keep_stdin {
                std::process::Stdio::piped()
            } else {
                std::process::Stdio::null()
            })
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::piped())
            .process_group(0);
        let mut child = command
            .spawn()
            .map_err(|error| RpcError::invalid_argument(format!("start process: {error}")))?;
        let pid = child
            .id()
            .ok_or_else(|| RpcError::new(Code::Internal, "spawned process has no pid"))?;
        let stdout = child.stdout.take().expect("piped stdout");
        let stderr = child.stderr.take().expect("piped stderr");
        let stdin = child.stdin.take();
        let input = stdin
            .map(ProcessInput::Stdin)
            .unwrap_or(ProcessInput::Closed);
        let handle = Arc::new(ProcessHandle {
            pid,
            tag: options.tag.clone(),
            config: options.config.clone(),
            input: Mutex::new(input),
            pty: None,
            output: sender.clone(),
        });
        self.live.write().await.insert(pid, handle.clone());
        self.bind_tag_reservation(options.tag.as_deref(), pid).await;
        self.remove_terminal_for(pid, options.tag.as_deref()).await;

        let stdout_reader = spawn_reader(stdout, sender.clone(), true);
        let stderr_reader = spawn_reader(stderr, sender.clone(), false);
        let registry = self.clone();
        tokio::spawn(async move {
            let status = child.wait().await;
            let _ = stdout_reader.await;
            let _ = stderr_reader.await;
            let end = end_event(status);
            let event = ProcessEvent::End(end);
            let _ = sender.send(event.clone());
            registry.finish(handle, event).await;
        });
        if let Some(timeout) = options.timeout {
            self.arm_timeout(pid, timeout);
        }
        Ok(Launch { pid, receiver })
    }

    /// 在线程池中创建 PTY、启动子进程并建立 PTY 输入输出通道。
    async fn start_pty(
        &self,
        options: StartOptions,
        size: PtySize,
        sender: broadcast::Sender<ProcessEvent>,
        receiver: broadcast::Receiver<ProcessEvent>,
    ) -> Result<Launch, RpcError> {
        let StartOptions {
            config,
            tag,
            user,
            defaults,
            timeout,
            ..
        } = options;
        let cwd = process_cwd(&config, &user)?;
        let setup_config = config.clone();
        let setup = tokio::task::spawn_blocking(move || {
            let system = native_pty_system();
            let pair = system
                .openpty(size)
                .map_err(|error| format!("create PTY: {error}"))?;
            let command = pty_command(&setup_config, &defaults, cwd.as_deref(), &user);
            let child = pair
                .slave
                .spawn_command(command)
                .map_err(|error| format!("start PTY process: {error}"))?;
            let pid = child
                .process_id()
                .ok_or_else(|| "PTY child has no pid".to_string())?;
            let reader = pair
                .master
                .try_clone_reader()
                .map_err(|error| format!("clone PTY reader: {error}"))?;
            let writer = pair
                .master
                .take_writer()
                .map_err(|error| format!("take PTY writer: {error}"))?;
            Ok::<_, String>((pid, child, pair.master, reader, writer))
        })
        .await
        .map_err(|error| RpcError::new(Code::Internal, format!("join PTY setup task: {error}")))?
        .map_err(RpcError::invalid_argument)?;
        let (pid, mut child, master, reader, writer) = setup;
        let master = Arc::new(StdMutex::new(master));
        let writer = Arc::new(StdMutex::new(writer));
        let handle = Arc::new(ProcessHandle {
            pid,
            tag: tag.clone(),
            config: config.clone(),
            input: Mutex::new(ProcessInput::Pty(Arc::clone(&writer))),
            pty: Some(Arc::clone(&master)),
            output: sender.clone(),
        });
        self.live.write().await.insert(pid, handle.clone());
        if let Some(tag) = &tag {
            self.bind_tag_reservation(Some(tag), pid).await;
        }
        self.remove_terminal_for(pid, tag.as_deref()).await;

        let pty_reader = spawn_pty_reader(reader, sender.clone());
        let registry = self.clone();
        tokio::spawn(async move {
            let end = match tokio::task::spawn_blocking(move || child.wait()).await {
                Ok(Ok(status)) => pty_end_event(status),
                Ok(Err(error)) => EndEvent {
                    exit_code: -1,
                    exited: false,
                    status: "failed to reap PTY process".into(),
                    error: Some(error.to_string()),
                },
                Err(error) => EndEvent {
                    exit_code: -1,
                    exited: false,
                    status: "failed to join PTY reaper".into(),
                    error: Some(error.to_string()),
                },
            };
            let _ = pty_reader.await;
            let event = ProcessEvent::End(end);
            let _ = sender.send(event.clone());
            registry.finish(handle, event).await;
        });
        if let Some(timeout) = timeout {
            self.arm_timeout(pid, timeout);
        }
        Ok(Launch { pid, receiver })
    }

    /// 为存活进程安排超时后的 TERM 和兜底 KILL。
    fn arm_timeout(&self, pid: u32, timeout: Duration) {
        let registry = self.clone();
        tokio::spawn(async move {
            time::sleep(timeout).await;
            let handle = {
                let live = registry.live.read().await;
                live.get(&pid).cloned()
            };
            if let Some(handle) = handle {
                let _ = send_group_signal(handle.pid, libc::SIGTERM);
                time::sleep(Duration::from_secs(2)).await;
                if registry.live.read().await.contains_key(&pid) {
                    let _ = send_group_signal(pid, libc::SIGKILL);
                }
            }
        });
    }

    /// 从存活表移除进程，缓存结束事件后才释放关联标签。
    pub(crate) async fn finish(&self, handle: Arc<ProcessHandle>, event: ProcessEvent) {
        self.live.write().await.remove(&handle.pid);
        let mut tags = self.tags.write().await;
        let mut terminal = self.terminal.lock().await;
        terminal.retain(|record| record.expires > time::Instant::now());
        terminal.push(TerminalRecord {
            pid: handle.pid,
            tag: handle.tag.clone(),
            event,
            expires: time::Instant::now() + TERMINAL_CACHE_TTL,
        });
        if terminal.len() > TERMINAL_CACHE_LIMIT {
            terminal.remove(0);
        }
        if let Some(tag) = &handle.tag {
            if tags.get(tag) == Some(&handle.pid) {
                tags.remove(tag);
            }
        }
    }

    /// 删除同一 PID 或标签的旧结束缓存，避免新旧进程混淆。
    async fn remove_terminal_for(&self, pid: u32, tag: Option<&str>) {
        self.terminal.lock().await.retain(|record| {
            record.pid != pid && tag.is_none_or(|tag| record.tag.as_deref() != Some(tag))
        });
    }

    /// 将启动前的标签占位符更新为实际 PID。
    async fn bind_tag_reservation(&self, tag: Option<&str>, pid: u32) {
        if let Some(tag) = tag {
            self.tags.write().await.insert(tag.to_owned(), pid);
        }
    }

    /// 在启动失败时仅释放尚未绑定 PID 的标签占位符。
    async fn release_tag_reservation(&self, tag: Option<&str>) {
        if let Some(tag) = tag {
            let mut tags = self.tags.write().await;
            if tags.get(tag) == Some(&0) {
                tags.remove(tag);
            }
        }
    }

    /// 返回当前存活进程 PID 的快照。
    async fn live_pids(&self) -> Vec<u32> {
        self.live.read().await.keys().copied().collect()
    }

    /// 轮询等待存活进程表清空，超时则返回 false。
    async fn wait_for_empty(&self, timeout: Duration) -> bool {
        let deadline = time::Instant::now() + timeout;
        loop {
            if self.live.read().await.is_empty() {
                return true;
            }
            if time::Instant::now() >= deadline {
                return false;
            }
            time::sleep(Duration::from_millis(10)).await;
        }
    }

    /// 按 PID 稳定排序后生成由 protobuf 类型表示的进程列表响应条目。
    pub(super) async fn list(&self) -> Vec<proto::ProcessInfo> {
        let mut handles: Vec<_> = self.live.read().await.values().cloned().collect();
        handles.sort_by_key(|handle| handle.pid);
        handles
            .into_iter()
            .map(|handle| proto::ProcessInfo {
                config: Some(config_to_proto(handle.config.clone())),
                pid: handle.pid,
                tag: handle.tag.clone(),
            })
            .collect()
    }

    /// 为存活进程创建输出订阅，或返回缓存的结束记录。
    pub(super) async fn subscribe(
        &self,
        selector: Option<&Selector>,
    ) -> Result<Subscription, RpcError> {
        let pid = self.resolve_selector(selector).await?;
        if let Some(handle) = self.live.read().await.get(&pid).cloned() {
            return Ok(Subscription::Live {
                pid: handle.pid,
                receiver: handle.output.subscribe(),
            });
        }
        let mut terminal = self.terminal.lock().await;
        terminal.retain(|record| record.expires > time::Instant::now());
        if let Some(record) = terminal.iter().find(|record| record.pid == pid).cloned() {
            return Ok(Subscription::Terminal(record));
        }
        Err(RpcError::new(
            Code::NotFound,
            format!("process {pid} was not found"),
        ))
    }

    /// 按选择器查找仍可控制的存活进程句柄。
    pub(super) async fn get_live(
        &self,
        selector: Option<&Selector>,
    ) -> Result<Arc<ProcessHandle>, RpcError> {
        let pid = self.resolve_selector(selector).await?;
        self.live
            .read()
            .await
            .get(&pid)
            .cloned()
            .ok_or_else(|| RpcError::new(Code::NotFound, format!("process {pid} was not found")))
    }

    /// 校验 PID 与标签二选一，并解析为对应的进程 PID。
    pub(crate) async fn resolve_selector(
        &self,
        selector: Option<&Selector>,
    ) -> Result<u32, RpcError> {
        let selector =
            selector.ok_or_else(|| RpcError::invalid_argument("process selector is required"))?;
        match (&selector.pid, &selector.tag) {
            (Some(pid), None) => Ok(*pid),
            (None, Some(tag)) if !tag.is_empty() => {
                if let Some(pid) = self.tags.read().await.get(tag).copied() {
                    return Ok(pid);
                }

                let mut terminal = self.terminal.lock().await;
                terminal.retain(|record| record.expires > time::Instant::now());
                terminal
                    .iter()
                    .find(|record| record.tag.as_deref() == Some(tag))
                    .map(|record| record.pid)
                    .ok_or_else(|| {
                        RpcError::new(Code::NotFound, format!("process tag {tag:?} was not found"))
                    })
            }
            _ => Err(RpcError::invalid_argument(
                "process selector requires exactly one of pid or tag",
            )),
        }
    }
}

/// 表示订阅命中存活进程还是短暂缓存的结束进程。
pub(super) enum Subscription {
    /// 存活进程的 PID 和事件订阅者。
    Live {
        /// 存活进程的 PID。
        pid: u32,
        /// 接收输出和结束事件的广播订阅者。
        receiver: broadcast::Receiver<ProcessEvent>,
    },
    /// 已结束进程的可回放记录。
    Terminal(TerminalRecord),
}

/// 表示刚启动进程返回给 Start 流的 PID 和事件接收者。
pub(super) struct Launch {
    /// 新启动进程的 PID。
    pub(super) pid: u32,
    /// 订阅该进程输出的接收者。
    pub(super) receiver: broadcast::Receiver<ProcessEvent>,
}

// 启动异步任务读取普通进程的 stdout 或 stderr 并广播输出片段。

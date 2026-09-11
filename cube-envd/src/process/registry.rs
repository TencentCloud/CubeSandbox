// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::{
    sync::{Arc, Mutex as StdMutex},
    time::Duration,
};

use portable_pty::{native_pty_system, PtySize};
use tokio::{sync::Mutex, time};

use crate::{
    connect::{Code, RpcError},
    generated::process as proto,
};

use super::{
    fanout::{OutputFanout, OutputSubscription},
    model::{
        config_to_proto, EndEvent, ProcessEvent, ProcessHandle, ProcessInput, ProcessRegistry,
        Selector, StartOptions, TerminalRecord, EVENT_CHILD_FLUSH_GRACE, SHUTDOWN_GRACE,
        TERMINAL_CACHE_LIMIT, TERMINAL_CACHE_TTL,
    },
    stream::{
        credential_helper_for, end_event, parse_pty_size, pipe_command, process_cwd, pty_command,
        send_group_signal, spawn_pty_reader, spawn_reader, wait_pty_child,
    },
};

impl ProcessRegistry {
    /// 按 TERM 后 KILL 的顺序关闭全部存活进程组。
    ///
    /// 以句柄快照为操作对象并在 KILL 前按 Arc 身份复核，避免信号落到
    /// 已退出并被内核复用 PID 的新进程组上。
    pub async fn shutdown(&self) {
        let handles: Vec<Arc<ProcessHandle>> =
            { self.live.read().await.values().cloned().collect() };
        for handle in &handles {
            let _ = send_group_signal(handle.pid, libc::SIGTERM);
        }

        if !self.wait_for_empty(SHUTDOWN_GRACE).await {
            let handles: Vec<Arc<ProcessHandle>> =
                { self.live.read().await.values().cloned().collect() };
            for handle in &handles {
                if self.is_live_handle(handle).await {
                    let _ = send_group_signal(handle.pid, libc::SIGKILL);
                }
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
            // 与上游一致：只有"路径不存在"才在启动前拒绝。校验使用 envd 自身
            // 凭据，因此其余 stat 失败（例如以非 root 身份校验 root 的 0700 主
            // 目录）一律放行，真正的失败由子进程 exec 阶段上报。
            match tokio::fs::metadata(cwd).await {
                Ok(metadata) if !metadata.is_dir() => {
                    return Err(RpcError::invalid_argument(format!(
                        "process cwd {} is not a directory",
                        cwd.display()
                    )));
                }
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                    return Err(RpcError::invalid_argument(format!(
                        "process cwd {} does not exist",
                        cwd.display()
                    )));
                }
                Ok(_) | Err(_) => {}
            }
        }

        let (fanout, subscription) = OutputFanout::new();
        if let Some(pty) = options.pty.take() {
            return self
                .start_pty(options, parse_pty_size(Some(pty))?, fanout, subscription)
                .await;
        }
        let helper = credential_helper_for(&options.user)?;
        let mut command = pipe_command(
            &options.config,
            options.defaults,
            cwd.as_deref(),
            &options.user,
            helper,
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
            output: fanout.clone(),
        });
        let arm_handle = Arc::clone(&handle);
        self.live.write().await.insert(pid, handle.clone());
        self.bind_tag_reservation(options.tag.as_deref(), pid).await;
        self.remove_terminal_for(pid, options.tag.as_deref()).await;

        let mut stdout_reader = spawn_reader(stdout, fanout.clone(), true);
        let mut stderr_reader = spawn_reader(stderr, fanout.clone(), false);
        let registry = self.clone();
        tokio::spawn(async move {
            let status = child.wait().await;

            // 进入结束排空：订阅者停读时投递按 EOL_SEND_BUDGET 放弃，绝不卡死 End/收尾。
            fanout.begin_eol();
            // 排空已写入的输出再收尾：正常退出时子进程是写端唯一持有者，
            // 退出即 EOF，宽限内完成；孙进程仍持有写端（如 `sleep 300 &`）时
            // reader 阻塞在 read，超时——封住输出并中断 reader，让 End 及时发出
            // 而非无限挂起。订阅者背压导致的 reader 阻塞会被 EOL 预算提前解除。
            let drain = async {
                let _ = (&mut stdout_reader).await;
                let _ = (&mut stderr_reader).await;
            };
            if time::timeout(EVENT_CHILD_FLUSH_GRACE, drain).await.is_err() {
                fanout.seal();
                stdout_reader.abort();
                stderr_reader.abort();
            }

            let end = end_event(status);
            let event = ProcessEvent::End(end);
            // finish 内部完成：记录写入 → End 槽广播 → 身份摘除 → 队列关闭，
            // 全程不等待任何订阅者排空。
            registry.finish(handle, event).await;
        });
        if let Some(timeout) = options.timeout {
            self.arm_timeout(arm_handle, timeout);
        }
        Ok(Launch {
            pid,
            receiver: subscription,
        })
    }

    /// 在线程池中创建 PTY、启动子进程并建立 PTY 输入输出通道。
    async fn start_pty(
        &self,
        options: StartOptions,
        size: PtySize,
        fanout: OutputFanout,
        subscription: OutputSubscription,
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
        let helper = credential_helper_for(&user)?;
        let setup_config = config.clone();
        let setup = tokio::task::spawn_blocking(move || {
            let system = native_pty_system();
            let pair = system
                .openpty(size)
                .map_err(|error| format!("create PTY: {error}"))?;
            let command = pty_command(&setup_config, &defaults, cwd.as_deref(), &user, helper);
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
        let (pid, child, master, reader, writer) = setup;
        let master = Arc::new(StdMutex::new(master));
        let writer = Arc::new(StdMutex::new(writer));
        let handle = Arc::new(ProcessHandle {
            pid,
            tag: tag.clone(),
            config: config.clone(),
            input: Mutex::new(ProcessInput::Pty(Arc::clone(&writer))),
            pty: Some(Arc::clone(&master)),
            output: fanout.clone(),
        });
        self.live.write().await.insert(pid, handle.clone());
        if let Some(tag) = &tag {
            self.bind_tag_reservation(Some(tag), pid).await;
        }
        self.remove_terminal_for(pid, tag.as_deref()).await;

        let arm_handle = Arc::clone(&handle);
        let interrupt = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let reader_done = Arc::new(tokio::sync::Notify::new());
        spawn_pty_reader(
            reader,
            Arc::clone(&interrupt),
            Arc::clone(&reader_done),
            fanout.clone(),
        );
        let registry = self.clone();
        tokio::spawn(async move {
            let end = match tokio::task::spawn_blocking(move || wait_pty_child(child)).await {
                Ok(Ok(event)) => event,
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

            // 与普通 reaper 相同的语义：孙进程持有 PTY slave 时 master 读端不会 EIO，
            // 宽限超时后封住输出。读线程必须结束才说明尾部输出已投递完毕；通知丢失
            // 或线程被 pin 时不会拖住收尾——超时即置中断标志并封住输出，End 照发。
            fanout.begin_eol();
            if time::timeout(EVENT_CHILD_FLUSH_GRACE, reader_done.notified())
                .await
                .is_err()
            {
                interrupt.store(true, std::sync::atomic::Ordering::Relaxed);
                fanout.seal();
            }

            let event = ProcessEvent::End(end);
            registry.finish(handle, event).await;
        });
        if let Some(timeout) = timeout {
            self.arm_timeout(arm_handle, timeout);
        }
        Ok(Launch {
            pid,
            receiver: subscription,
        })
    }

    /// 为存活进程安排超时后的 TERM 和兜底 KILL。
    ///
    /// 直接捕获进程句柄并按 Arc 身份复核：即使进程退出后 PID 被内核复用于
    /// 新进程，也绝不会把信号发到新进程的进程组上。
    fn arm_timeout(&self, handle: Arc<ProcessHandle>, timeout: Duration) {
        let registry = self.clone();
        tokio::spawn(async move {
            time::sleep(timeout).await;
            if !registry.is_live_handle(&handle).await {
                return;
            }
            let _ = send_group_signal(handle.pid, libc::SIGTERM);
            time::sleep(Duration::from_secs(2)).await;
            if registry.is_live_handle(&handle).await {
                let _ = send_group_signal(handle.pid, libc::SIGKILL);
            }
        });
    }

    /// 判断句柄是否仍是 live 表中该 PID 的当前所有者（Arc 身份比较）。
    async fn is_live_handle(&self, handle: &Arc<ProcessHandle>) -> bool {
        self.live
            .read()
            .await
            .get(&handle.pid)
            .is_some_and(|owner| Arc::ptr_eq(owner, handle))
    }

    /// 收尾一个已结束进程：写回放记录、广播 End、按身份摘除并关闭扇出。
    ///
    /// 加锁顺序保持 live → tags → terminal（与既有约定一致，标签在记录写入前
    /// 保持占用，杜绝新旧进程的标签竞态）。收尾**不等待任何订阅者排空**：
    /// End 经 End 槽广播（同步登记），因此停读订阅者无法阻塞本函数。
    ///
    /// PID 复用防护：仅当 live 表中该 PID 的当前所有者就是本句柄（Arc 身份）
    /// 时才摘除并写入回放记录；若 PID 已被更新的进程占用，则保留新句柄，
    /// 也不写入可能遮蔽新进程的陈旧记录。
    pub(crate) async fn finish(&self, handle: Arc<ProcessHandle>, event: ProcessEvent) {
        let mut live = self.live.write().await;
        let matched = live
            .get(&handle.pid)
            .is_some_and(|owner| Arc::ptr_eq(owner, &handle));
        let mut tags = self.tags.write().await;
        let mut terminal = self.terminal.lock().await;
        if matched {
            // 同 PID 的陈旧记录先清理（同一 PID 同一时刻至多一条记录），
            // 记录在本临界区内先于摘除写入：摘除后按 PID 的 Connect 必然命中回放。
            terminal
                .retain(|record| record.expires > time::Instant::now() && record.pid != handle.pid);
            terminal.push(TerminalRecord {
                pid: handle.pid,
                tag: handle.tag.clone(),
                event: event.clone(),
                expires: time::Instant::now() + TERMINAL_CACHE_TTL,
            });
            if terminal.len() > TERMINAL_CACHE_LIMIT {
                terminal.remove(0);
            }
        } else {
            terminal.retain(|record| record.expires > time::Instant::now());
        }
        drop(terminal);
        if matched {
            live.remove(&handle.pid);
        }
        if let Some(tag) = &handle.tag {
            if tags.get(tag) == Some(&handle.pid) {
                tags.remove(tag);
            }
        }
        drop(tags);
        drop(live);

        // 无论身份是否匹配（PID 被复用不影响本进程自己的订阅者），
        // 都向本句柄的订阅者广播 End 并关闭扇出。
        handle.output.mark_ended(&event);
        handle.output.close();
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
        let handle = self.live.read().await.get(&pid).cloned();
        if let Some(handle) = handle {
            let (receiver, ended) = handle.output.subscribe();
            if !ended {
                return Ok(Subscription::Live { pid, receiver });
            }
            // 进程在"查 live 表"与"挂载订阅者"之间完成了收尾：End 已广播且
            // 记录必然已写入（finish 先写记录、后摘除），转终端回放路径。
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
        /// 接收输出和结束事件的订阅队列。
        receiver: OutputSubscription,
    },
    /// 已结束进程的可回放记录。
    Terminal(TerminalRecord),
}

/// 表示刚启动进程返回给 Start 流的 PID 和事件接收者。
pub(super) struct Launch {
    /// 新启动进程的 PID。
    pub(super) pid: u32,
    /// 订阅该进程输出的接收者。
    pub(super) receiver: OutputSubscription,
}

// 启动异步任务读取普通进程的 stdout 或 stderr 并广播输出片段。

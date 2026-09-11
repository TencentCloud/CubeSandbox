// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

pub(crate) mod input;
mod linux;
pub(crate) mod output;

use std::collections::BTreeMap;
use std::ffi::{CString, OsString};
use std::os::unix::ffi::OsStrExt;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use tokio::time::Instant;

#[cfg(test)]
mod snapshot_test;

use futures::FutureExt;

use self::linux as process_linux;
use self::linux::{setup_error, Spawn};
use crate::cgroup::{CgroupManager, ProcessClass};
use crate::error::DomainError;
use crate::proto::process::{ListResponse, ProcessInfo, StartRequest};
use crate::runtime::{ProcessUser, RuntimeState, UserDatabase};
use crate::transport::timeout::{parse_connect_timeout_ms, ConnectTimeoutPolicy};

/// Only successfully published leaders belong in this registry. In particular,
/// listing never discovers or adopts processes from the guest PID namespace.
pub struct ProcessManager {
    registry: Arc<Mutex<Registry>>,
    daemon_path: OsString,
    cgroups: CgroupManager,
    #[cfg(test)]
    fault: Mutex<Option<SetupFault>>,
}

#[derive(Default)]
struct Registry {
    live: BTreeMap<u32, LiveProcess>,
    tags: BTreeMap<String, usize>,
}

impl Registry {
    fn select(
        &self,
        selector: &crate::proto::process::ProcessSelector,
    ) -> Result<&LiveProcess, DomainError> {
        use crate::proto::process::process_selector::Selector;
        match selector.selector.as_ref() {
            Some(Selector::Pid(pid)) => self
                .live
                .get(pid)
                .ok_or_else(|| DomainError::NotFound(format!("process with pid {pid} not found"))),
            Some(Selector::Tag(tag)) => self
                .live
                .values()
                .find(|process| process.info.tag.as_ref() == Some(tag))
                .ok_or_else(|| DomainError::NotFound(format!("process with tag {tag} not found"))),
            None => Err(DomainError::Unimplemented(
                "invalid input type *process.ProcessSelector".into(),
            )),
        }
    }
}

struct LiveProcess {
    info: ProcessInfo,
    wait_owner: tokio::task::AbortHandle,
    descriptors: Vec<Arc<process_linux::DescriptorCheck>>,
    output: Arc<crate::process::output::Output>,
    input: crate::process::input::Input,
    pty: Option<Arc<process_linux::Pty>>,
}

struct Reservation {
    registry: Arc<Mutex<Registry>>,
    tag: Option<String>,
    pid: Option<u32>,
}

impl Reservation {
    fn release(&mut self, registry: &mut Registry) {
        if let Some(pid) = self.pid.take() {
            registry.live.remove(&pid);
        }
        if let Some(tag) = self.tag.take() {
            if let Some(count) = registry.tags.get_mut(&tag) {
                *count -= 1;
                if *count == 0 {
                    registry.tags.remove(&tag);
                }
            }
        }
    }
}

impl Drop for Reservation {
    fn drop(&mut self) {
        let registry = self.registry.clone();
        if let Ok(mut registry) = registry.lock() {
            self.release(&mut registry);
        };
    }
}

impl Default for ProcessManager {
    fn default() -> Self {
        Self {
            registry: Arc::new(Mutex::new(Registry::default())),
            daemon_path: std::env::var_os("PATH").unwrap_or_default(),
            cgroups: CgroupManager::default(),
            #[cfg(test)]
            fault: Mutex::new(None),
        }
    }
}

#[cfg(test)]
#[derive(Clone, Copy, Debug, PartialEq)]
enum SetupStage {
    Reserved,
    Cwd,
    Resources,
    Forked,
    Execed,
    Publication,
}

#[cfg(test)]
struct SetupFault {
    stage: SetupStage,
    entered: tokio::sync::oneshot::Sender<()>,
    release: tokio::sync::oneshot::Receiver<Result<(), DomainError>>,
}

impl ProcessManager {
    pub(crate) fn with_cgroup_root(root: impl Into<PathBuf>) -> Self {
        Self {
            cgroups: CgroupManager::new(root),
            ..Self::default()
        }
    }

    pub(crate) async fn initialize_cgroups(&self) -> Result<(), crate::cgroup::CgroupError> {
        self.cgroups.initialize().await
    }

    #[cfg(test)]
    async fn setup_stage(
        &self,
        stage: SetupStage,
        deadline: Option<Instant>,
        tag: Option<&str>,
    ) -> Result<(), DomainError> {
        let fault = {
            let mut pending = self.fault.lock().map_err(|_| DomainError::Internal)?;
            if pending.as_ref().is_some_and(|fault| fault.stage == stage) {
                pending.take()
            } else {
                None
            }
        };
        if let Some(fault) = fault {
            let _ = fault.entered.send(());
            let result = if let Some(deadline) = deadline {
                tokio::time::timeout_at(deadline, fault.release)
                    .await
                    .map_err(|_| DomainError::DeadlineExceeded)?
            } else {
                fault.release.await
            };
            result.map_err(|_| DomainError::Internal)??;
        }
        snapshot_test::barrier(&format!("{stage:?}"), tag, deadline).await
    }

    pub async fn start(
        self: &Arc<Self>,
        request: StartRequest,
        headers: &http::HeaderMap,
        snapshot: Arc<RuntimeState>,
        users: &UserDatabase,
        lifecycle: Arc<crate::server::LifecycleState>,
    ) -> Result<crate::process::output::Subscription, DomainError> {
        let validated = self.validate(request, headers, snapshot, users).await?;
        let manager = self.clone();
        let failure_lifecycle = lifecycle.clone();
        // Transport cancellation may lose the response, but cannot cancel
        // setup halfway through or take away the child's wait owner.
        tokio::spawn(async move {
            std::panic::AssertUnwindSafe(manager.prepare(validated, lifecycle))
                .catch_unwind()
                .await
                .unwrap_or_else(|_| {
                    failure_lifecycle.fail_with(crate::server::EnvdFailure::panic("process setup"));
                    Err(DomainError::Internal)
                })
        })
        .await
        .map_err(|_| DomainError::Internal)?
    }

    pub fn connect(
        &self,
        request: crate::proto::process::ConnectRequest,
    ) -> Result<crate::process::output::Subscription, DomainError> {
        let registry = self.registry.lock().map_err(|_| DomainError::Internal)?;
        let process = registry.select(&request.process)?;
        // Registration and terminal removal linearize under the same lock.
        process.output.subscribe(process.info.pid)
    }

    pub async fn send_input(
        &self,
        request: crate::proto::process::SendInputRequest,
    ) -> Result<(), DomainError> {
        self.input_target(&request.process)?
            .send(request.input.into_option())
            .await
    }

    pub(crate) fn input_target(
        &self,
        selector: &crate::proto::process::ProcessSelector,
    ) -> Result<crate::process::input::InputTarget, DomainError> {
        let registry = self.registry.lock().map_err(|_| DomainError::Internal)?;
        let process = registry.select(selector)?;
        Ok(crate::process::input::InputTarget {
            input: process.input.clone(),
            pty: process.pty.is_some(),
        })
    }

    pub async fn close_stdin(
        &self,
        request: crate::proto::process::CloseStdinRequest,
    ) -> Result<(), DomainError> {
        let input = {
            let registry = self.registry.lock().map_err(|_| DomainError::Internal)?;
            let process = registry.select(&request.process)?;
            if process.pty.is_some() {
                return Err(DomainError::PtyCloseUnsupported);
            }
            process.input.clone()
        };
        input.close().await
    }

    pub fn update(&self, request: crate::proto::process::UpdateRequest) -> Result<(), DomainError> {
        let registry = self.registry.lock().map_err(|_| DomainError::Internal)?;
        let process = registry.select(&request.process)?;
        if !request.pty.is_set() {
            return Ok(());
        }
        let pty = process.pty.as_ref().ok_or_else(|| {
            DomainError::FailedPrecondition(
                "error resizing tty: tty not assigned to process".into(),
            )
        })?;
        pty.resize(&request.pty.size)
    }

    pub fn send_signal(
        &self,
        request: crate::proto::process::SendSignalRequest,
    ) -> Result<(), DomainError> {
        #[cfg(test)]
        snapshot_test::signal_barrier(&request.process)?;
        let registry = self.registry.lock().map_err(|_| DomainError::Internal)?;
        let process = registry.select(&request.process)?;
        let signal = match request.signal.to_i32() {
            libc::SIGTERM => libc::SIGTERM,
            libc::SIGKILL => libc::SIGKILL,
            _ => return Err(DomainError::Unimplemented("signal is not supported".into())),
        };
        // Hold the same lock as reap/removal. The unreaped, exclusively owned
        // leader cannot be reused by the kernel until this syscall completes.
        process.output.cancel();
        process_linux::signal(process.info.pid, signal)
    }

    pub fn list(&self) -> Result<ListResponse, DomainError> {
        let registry = self.registry.lock().map_err(|_| DomainError::Internal)?;
        Ok(ListResponse {
            processes: registry
                .live
                .values()
                .map(|process| process.info.clone())
                .collect(),
            ..Default::default()
        })
    }

    async fn prepare(
        self: Arc<Self>,
        start: ValidatedStart,
        lifecycle: Arc<crate::server::LifecycleState>,
    ) -> Result<crate::process::output::Subscription, DomainError> {
        start.check_deadline()?;
        let mut reservation = {
            let mut registry = self.registry.lock().map_err(|_| DomainError::Internal)?;
            if let Some(tag) = &start.request.tag {
                *registry.tags.entry(tag.clone()).or_default() += 1;
            }
            Reservation {
                registry: self.registry.clone(),
                tag: start.request.tag.clone(),
                pid: None,
            }
        };
        #[cfg(test)]
        self.setup_stage(
            SetupStage::Reserved,
            start.deadline,
            start.request.tag.as_deref(),
        )
        .await?;
        let cwd = start.cwd.clone();
        let environment = start.environment(&self.daemon_path)?;
        // Resolve the user command with the child's PATH, after publishing the
        // wrapper process, as upstream does. Pre-exec resource policy still runs
        // before nice or any user code; a zero relative adjustment preserves it.
        let executable = PathBuf::from("/usr/bin/nice");
        #[cfg(test)]
        self.setup_stage(
            SetupStage::Cwd,
            start.deadline,
            start.request.tag.as_deref(),
        )
        .await?;
        start.check_deadline()?;
        let class = if start.request.pty.is_set() {
            ProcessClass::Pty
        } else {
            ProcessClass::User
        };
        let mut cgroup = self.cgroups.acquire(class, start.deadline).await?;
        let cgroup_procs = cgroup.group.clone();
        #[cfg(test)]
        self.setup_stage(
            SetupStage::Resources,
            start.deadline,
            start.request.tag.as_deref(),
        )
        .await?;
        start.check_deadline()?;
        let argv = [
            "/usr/bin/nice",
            "-n",
            "0",
            start.request.process.cmd.as_str(),
        ]
        .into_iter()
        .chain(start.request.process.args.iter().map(String::as_str))
        .map(|arg| {
            CString::new(arg.as_bytes())
                .map_err(|_| start.failed_start("fork/exec /bin/sh: invalid argument"))
        })
        .collect::<Result<_, _>>()?;
        let (stdin, stdout, stderr, readers, input_pipe, pty) = if start.request.pty.is_set() {
            let (pty, slave) = process_linux::Pty::open(&start.request.pty.size)?;
            let stdout = slave
                .try_clone()
                .map_err(|error| setup_error(error, false))?;
            let stderr = slave
                .try_clone()
                .map_err(|error| setup_error(error, false))?;
            (
                Some(slave),
                stdout,
                stderr,
                crate::process::output::Readers::Pty(pty.clone()),
                Some(crate::process::input::Target::Pty(pty.clone())),
                Some(pty),
            )
        } else {
            let (stdout_send, stdout) =
                tokio::net::unix::pipe::pipe().map_err(|error| setup_error(error, false))?;
            let (stderr_send, stderr) =
                tokio::net::unix::pipe::pipe().map_err(|error| setup_error(error, false))?;
            let (stdin, input_pipe) = if start.request.stdin.unwrap_or(true) {
                let (send, receive) =
                    tokio::net::unix::pipe::pipe().map_err(|error| setup_error(error, false))?;
                (
                    Some(
                        process_linux::above_stdio(
                            receive
                                .into_blocking_fd()
                                .map_err(|error| setup_error(error, false))?,
                        )
                        .map_err(|error| setup_error(error, false))?,
                    ),
                    Some(crate::process::input::Target::Pipe(
                        process_linux::ProcessFd::new(send, Some(libc::O_WRONLY)),
                    )),
                )
            } else {
                (None, None)
            };
            (
                stdin,
                process_linux::above_stdio(
                    stdout_send
                        .into_blocking_fd()
                        .map_err(|error| setup_error(error, false))?,
                )
                .map_err(|error| setup_error(error, false))?,
                process_linux::above_stdio(
                    stderr_send
                        .into_blocking_fd()
                        .map_err(|error| setup_error(error, false))?,
                )
                .map_err(|error| setup_error(error, false))?,
                crate::process::output::Readers::Pipes(
                    process_linux::ProcessFd::new(stdout, Some(libc::O_RDONLY)),
                    process_linux::ProcessFd::new(stderr, Some(libc::O_RDONLY)),
                ),
                input_pipe,
                None,
            )
        };
        let mut descriptors = match &readers {
            crate::process::output::Readers::Pipes(stdout, stderr) => {
                vec![stdout.readiness(), stderr.readiness()]
            }
            crate::process::output::Readers::Pty(_) => Vec::new(),
        };
        if let Some(crate::process::input::Target::Pipe(pipe)) = &input_pipe {
            descriptors.push(pipe.readiness());
        }
        let spawn = Spawn {
            stdin,
            stdout,
            stderr,
            pty: pty.is_some(),
            executable: CString::new(executable.as_os_str().as_bytes())
                .map_err(|_| invalid("invalid executable"))?,
            argv,
            environment,
            cwd: CString::new(cwd.as_os_str().as_bytes())
                .map_err(|_| start.failed_start("fork/exec /bin/sh: invalid argument"))?,
            user: start.user,
            cgroup: cgroup_procs,
        };
        let (mut child, mut channel) = spawn.begin()?;
        descriptors.push(child.readiness());
        #[cfg(test)]
        self.setup_stage(
            SetupStage::Forked,
            start.deadline,
            start.request.tag.as_deref(),
        )
        .await?;
        let result = if let Some(deadline) = start.deadline {
            tokio::time::timeout_at(deadline, process_linux::exec_result(&mut channel))
                .await
                .map_err(|_| DomainError::DeadlineExceeded)
                .and_then(|result| result)
        } else {
            process_linux::exec_result(&mut channel).await
        };
        let result = result.and_then(|()| {
            if start
                .deadline
                .is_some_and(|deadline| Instant::now() >= deadline)
            {
                Err(DomainError::DeadlineExceeded)
            } else {
                Ok(())
            }
        });
        if let Err(error) = result {
            child.kill_and_reap().await?;
            return Err(error);
        }
        #[cfg(test)]
        self.setup_stage(
            SetupStage::Execed,
            start.deadline,
            start.request.tag.as_deref(),
        )
        .await?;
        #[cfg(test)]
        self.setup_stage(
            SetupStage::Publication,
            start.deadline,
            start.request.tag.as_deref(),
        )
        .await?;
        let pid = child.pid;
        let (input, writer) = crate::process::input::Input::new(input_pipe, pid);
        let output = Arc::new(crate::process::output::Output::default());
        let subscription = output.subscribe(pid)?;
        let wait_output = output.clone();
        reservation.pid = Some(pid);
        let info = ProcessInfo {
            pid,
            tag: start.request.tag,
            config: start.request.process,
            ..Default::default()
        };
        let (publish, published) =
            tokio::sync::oneshot::channel::<(process_linux::Child, Reservation)>();
        let deadline = start.deadline;
        // This task is the sole wait owner before the registry becomes visible.
        // It is deliberately not a server CriticalTask: user exits are normal.
        child.lifecycle = Some(lifecycle.clone());
        let wait_owner = tokio::spawn(async move {
            let Ok((mut child, mut reservation)) = published.await else {
                return;
            };
            let result = std::panic::AssertUnwindSafe(async {
                let mut reading = Box::pin(wait_output.read(readers));
                let mut writing = Box::pin(writer.run());
                let mut read_result = None;
                {
                    let waiting = child.exited();
                    tokio::pin!(waiting);
                    let expiry = async {
                        if let Some(deadline) = deadline { tokio::time::sleep_until(deadline).await; }
                        else { std::future::pending::<()>().await; }
                    };
                    tokio::pin!(expiry);
                    let cancelled = wait_output.cancelled();
                    tokio::pin!(cancelled);
                    let (mut exited, mut expired, mut output_cancelled) = (false, false, false);
                    // Keep the single writer live while draining output. Signal
                    // and deadline cancellation never wait for a subscriber.
                    while !exited || (read_result.is_none() && !output_cancelled) {
                        tokio::select! {
                            result = &mut waiting, if !exited => { result?; exited = true; }
                            result = &mut reading, if read_result.is_none() => read_result = Some(result),
                            _ = &mut writing => return Err(DomainError::Internal),
                            _ = &mut cancelled, if !output_cancelled => output_cancelled = true,
                            _ = &mut expiry, if !expired => {
                                expired = true;
                                wait_output.cancel();
                                match process_linux::signal(pid, libc::SIGKILL) {
                                    Ok(()) | Err(DomainError::NotFound(_)) => {}
                                    Err(error) => return Err(error),
                                }
                            }
                        }
                    }
                }
                #[cfg(test)]
                snapshot_test::barrier("Terminal", reservation.tag.as_deref(), None).await?;
                let status = {
                    let registry = reservation.registry.clone();
                    let mut registry = registry.lock().map_err(|_| DomainError::Internal)?;
                    // Reaping and selector removal share the publication lock.
                    let status = child.reap()?;
                    reservation.release(&mut registry);
                    status
                };
                // Only authoritative wait completion cancels blocked input.
                drop(writing);
                drop(reading);
                let terminal = if matches!(read_result, Some(Err(_))) {
                    Err(DomainError::Internal)
                } else {
                    Ok(process_linux::end_event(status))
                };
                wait_output.finish(terminal);
                Ok::<(), DomainError>(())
            })
            .catch_unwind()
            .await;
            if !matches!(result, Ok(Ok(()))) {
                lifecycle.fail_with(crate::server::EnvdFailure::invariant("process wait"));
            }
        });
        let publication = {
            let mut registry = self.registry.lock().map_err(|_| DomainError::Internal)?;
            if deadline.is_some_and(|deadline| Instant::now() >= deadline) {
                Err((DomainError::DeadlineExceeded, child, reservation))
            } else {
                child.live = true;
                registry.live.insert(
                    pid,
                    LiveProcess {
                        info,
                        descriptors,
                        output,
                        input,
                        pty,
                        wait_owner: wait_owner.abort_handle(),
                    },
                );
                match publish.send((child, reservation)) {
                    Ok(()) => Ok(pid),
                    Err((child, reservation)) => {
                        registry.live.remove(&pid);
                        Err((DomainError::Internal, child, reservation))
                    }
                }
            }
        };
        match publication {
            Ok(_) => {
                cgroup.commit();
                Ok(subscription)
            }
            Err((error, mut child, _reservation)) => {
                child.live = false;
                child.kill_and_reap().await?;
                Err(error)
            }
        }
    }

    async fn validate(
        &self,
        request: StartRequest,
        headers: &http::HeaderMap,
        snapshot: Arc<RuntimeState>,
        users: &UserDatabase,
    ) -> Result<ValidatedStart, DomainError> {
        let explicit_user = crate::runtime::explicit_username(headers)?;
        let user = users
            .resolve_process_user(explicit_user.as_deref().unwrap_or(snapshot.default_user()))
            .await?;
        let timeout = headers
            .get("connect-timeout-ms")
            .map(|value| value.to_str().map_err(|_| invalid("invalid timeout")))
            .transpose()?;
        let duration = match parse_connect_timeout_ms(timeout) {
            ConnectTimeoutPolicy::NoDeadline => None,
            ConnectTimeoutPolicy::ActiveVmDeadline(duration) => Some(duration),
            ConnectTimeoutPolicy::Invalid => {
                let raw = timeout.unwrap_or_default();
                let reason = match raw.parse::<i64>().unwrap_err().kind() {
                    std::num::IntErrorKind::PosOverflow | std::num::IntErrorKind::NegOverflow => {
                        "value out of range"
                    }
                    _ => "invalid syntax",
                };
                return Err(invalid(&format!("strconv.Atoi: parsing {raw:?}: {reason}")));
            }
        };
        let config = &request.process;
        let logical_cwd = config
            .cwd
            .as_deref()
            .filter(|cwd| !cwd.is_empty())
            .or(snapshot.default_workdir())
            .unwrap_or("");
        if logical_cwd.starts_with('~')
            && logical_cwd != "~"
            && !logical_cwd.starts_with("~/")
            && !logical_cwd.starts_with("~\\")
        {
            return Err(invalid("user-specific home expansion is unsupported"));
        }
        let cwd = if logical_cwd == "~" {
            user.home.clone()
        } else if logical_cwd.starts_with("~/") || logical_cwd.starts_with("~\\") {
            user.home.join(&logical_cwd[2..])
        } else {
            user.home.join(logical_cwd)
        };
        // Upstream reports a missing working directory before exec argument or
        // environment validation. Other stat errors still reach process setup.
        if tokio::fs::metadata(&cwd)
            .await
            .is_err_and(|error| error.raw_os_error() == Some(libc::ENOENT))
        {
            return Err(invalid(&format!("cwd '{}' does not exist", cwd.display())));
        }
        let deadline = duration
            .map(|duration| {
                Instant::now()
                    .checked_add(duration)
                    .ok_or_else(|| invalid("invalid timeout"))
            })
            .transpose()?;
        Ok(ValidatedStart {
            request,
            snapshot,
            user,
            deadline,
            cwd,
        })
    }
}

impl crate::server::ReadinessCheck for ProcessManager {
    fn self_check(&self) -> futures::future::BoxFuture<'_, bool> {
        Box::pin(async move {
            self.registry.lock().is_ok_and(|registry| {
                registry.live.iter().all(|(pid, process)| {
                    *pid == process.info.pid
                        && !process.wait_owner.is_finished()
                        && process.descriptors.iter().all(|fd| fd.self_check())
                        && process.pty.as_ref().is_none_or(|pty| pty.self_check())
                        && process
                            .info
                            .tag
                            .as_ref()
                            .is_none_or(|tag| registry.tags.contains_key(tag))
                })
            })
        })
    }
}

struct ValidatedStart {
    request: StartRequest,
    snapshot: Arc<RuntimeState>,
    user: ProcessUser,
    deadline: Option<Instant>,
    cwd: PathBuf,
}

impl ValidatedStart {
    fn failed_start(&self, detail: &str) -> DomainError {
        let command = std::iter::once(self.request.process.cmd.as_str())
            .chain(self.request.process.args.iter().map(String::as_str))
            .collect::<Vec<_>>()
            .join(" ");
        invalid(&format!("error starting process '{command}': {detail}"))
    }

    fn check_deadline(&self) -> Result<(), DomainError> {
        if self
            .deadline
            .is_some_and(|deadline| Instant::now() >= deadline)
        {
            return Err(DomainError::DeadlineExceeded);
        }
        Ok(())
    }

    fn environment(&self, daemon_path: &std::ffi::OsStr) -> Result<Vec<CString>, DomainError> {
        use std::os::unix::ffi::OsStrExt;
        let path =
            std::str::from_utf8(daemon_path.as_bytes()).map_err(|_| DomainError::Internal)?;
        let home = self.user.home.to_str().ok_or(DomainError::Internal)?;
        let mut environment = BTreeMap::from([
            ("PATH".to_owned(), path.to_owned()),
            ("HOME".to_owned(), home.to_owned()),
            ("USER".to_owned(), self.user.name.clone()),
            ("LOGNAME".to_owned(), self.user.name.clone()),
        ]);
        // Go rejects NUL in every supplied entry before deduplicating keys,
        // including defaults that a per-process value would otherwise replace.
        if self
            .snapshot
            .environment()
            .iter()
            .chain(self.request.process.envs.iter())
            .any(|(key, value)| key.contains('\0') || value.contains('\0'))
        {
            return Err(self.failed_start("exec: environment variable contains NUL"));
        }
        environment.extend(self.snapshot.environment().clone());
        environment.extend(self.request.process.envs.clone());
        environment
            .into_iter()
            .map(|(key, value)| {
                CString::new(format!("{key}={value}"))
                    .map_err(|_| self.failed_start("exec: environment variable contains NUL"))
            })
            .collect()
    }
}

fn invalid(message: &str) -> DomainError {
    DomainError::InvalidArgument(message.into())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime::RuntimeStateStore;
    use crate::server::{LifecycleState, ServerPhase};
    use std::time::Duration;

    #[tokio::test]
    async fn cancelled_live_wait_owner_revokes_public_health() {
        assert_process_fault_revokes_public_health(ProcessFault::Wait).await;
    }

    #[tokio::test]
    async fn blocking_pty_descriptor_revokes_public_health() {
        assert_process_fault_revokes_public_health(ProcessFault::Pty).await;
    }

    #[tokio::test]
    async fn blocking_stdout_descriptor_revokes_public_health() {
        assert_process_fault_revokes_public_health(ProcessFault::Pipe(1)).await;
    }

    #[tokio::test]
    async fn blocking_stdin_descriptor_revokes_public_health() {
        assert_process_fault_revokes_public_health(ProcessFault::Pipe(0)).await;
    }

    #[tokio::test]
    async fn blocking_stderr_descriptor_revokes_public_health() {
        assert_process_fault_revokes_public_health(ProcessFault::Pipe(2)).await;
    }

    #[tokio::test]
    async fn inheritable_pidfd_revokes_public_health() {
        assert_process_fault_revokes_public_health(ProcessFault::PidFd).await;
    }

    enum ProcessFault {
        Wait,
        Pty,
        Pipe(i32),
        PidFd,
    }

    async fn assert_process_fault_revokes_public_health(fault: ProcessFault) {
        use axum::{body::Body, http::Request};
        use futures::StreamExt;
        use tower::ServiceExt;

        let lifecycle = Arc::new(LifecycleState::new());
        assert!(lifecycle.try_transition(ServerPhase::Listening));
        assert!(lifecycle.try_transition(ServerPhase::Ready));
        let state = crate::transport::new_app_state(lifecycle);
        let router = crate::transport::build_router(state.clone());
        let mut request =
            serde_json::json!({"process":{"cmd":"/bin/sleep","args":["60"]},"tag":"owned-wait"});
        if matches!(fault, ProcessFault::Pty) {
            request["pty"] = serde_json::json!({"size":{"rows":24,"cols":80}});
        }
        let json = serde_json::to_vec(&request).unwrap();
        let mut envelope = vec![0];
        envelope.extend_from_slice(&(json.len() as u32).to_be_bytes());
        envelope.extend_from_slice(&json);
        let response = router
            .clone()
            .oneshot(
                Request::post("/process.Process/Start")
                    .header("content-type", "application/connect+json")
                    .header("connect-protocol-version", "1")
                    .body(Body::from(envelope))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), http::StatusCode::OK);
        let mut body = response.into_body().into_data_stream();
        let frame = body.next().await.unwrap().unwrap();
        let event: serde_json::Value = serde_json::from_slice(&frame[5..]).unwrap();
        let pid = event["event"]["start"]["pid"].as_u64().unwrap() as u32;
        // The injected cancellation loses the sole wait owner. Clean up this
        // test-owned, still-unreaped child even if an assertion fails.
        struct Cleanup(u32);
        impl Drop for Cleanup {
            fn drop(&mut self) {
                unsafe {
                    libc::kill(self.0 as i32, libc::SIGKILL);
                    while libc::waitpid(self.0 as i32, std::ptr::null_mut(), 0) < 0 {
                        if std::io::Error::last_os_error().raw_os_error() != Some(libc::EINTR) {
                            break;
                        }
                    }
                }
            }
        }
        let _cleanup = Cleanup(pid);
        let owner = state.processes.registry.lock().unwrap().live[&pid]
            .wait_owner
            .clone();
        assert_eq!(
            router
                .clone()
                .oneshot(Request::get("/health").body(Body::empty()).unwrap())
                .await
                .unwrap()
                .status(),
            http::StatusCode::NO_CONTENT
        );
        let response = if matches!(fault, ProcessFault::Pipe(_) | ProcessFault::PidFd) {
            // Identify this child's actual daemon descriptor; neither duplicate
            // nor close it. Restore flags before the test removes its wait owner.
            let pipe = if let ProcessFault::Pipe(child_fd) = fault {
                Some(std::fs::read_link(format!("/proc/{pid}/fd/{child_fd}")).unwrap())
            } else {
                None
            };
            let fd = std::fs::read_dir("/proc/self/fd")
                .unwrap()
                .filter_map(Result::ok)
                .find_map(|entry| {
                    let fd = entry.file_name().to_str()?.parse::<i32>().ok()?;
                    let matches = if let ProcessFault::Pipe(child_fd) = fault {
                        let flags = unsafe { libc::fcntl(fd, libc::F_GETFL) };
                        let access = if child_fd == 0 {
                            libc::O_WRONLY
                        } else {
                            libc::O_RDONLY
                        };
                        std::fs::read_link(entry.path()).ok() == pipe
                            && flags >= 0
                            && flags & libc::O_ACCMODE == access
                    } else {
                        std::fs::read_to_string(format!("/proc/self/fdinfo/{fd}"))
                            .ok()?
                            .lines()
                            .any(|line| {
                                line.strip_prefix("Pid:").is_some_and(|value| {
                                    value.trim().parse::<u32>().ok() == Some(pid)
                                })
                            })
                    };
                    matches.then_some(fd)
                })
                .expect("daemon owns matching child descriptor");
            let (get, set, bit) = if matches!(fault, ProcessFault::PidFd) {
                (libc::F_GETFD, libc::F_SETFD, libc::FD_CLOEXEC)
            } else {
                (libc::F_GETFL, libc::F_SETFL, libc::O_NONBLOCK)
            };
            let flags = unsafe { libc::fcntl(fd, get) };
            struct RestoreFlags(i32, i32, i32);
            impl Drop for RestoreFlags {
                fn drop(&mut self) {
                    unsafe {
                        libc::fcntl(self.0, self.1, self.2);
                    }
                }
            }
            let _restore = RestoreFlags(fd, set, flags);
            assert_eq!(unsafe { libc::fcntl(fd, set, flags & !bit) }, 0);
            router
                .clone()
                .oneshot(Request::get("/health").body(Body::empty()).unwrap())
                .await
                .unwrap()
        } else if matches!(fault, ProcessFault::Pty) {
            let pty = state.processes.registry.lock().unwrap().live[&pid]
                .pty
                .clone()
                .unwrap();
            pty.set_blocking_for_test(true);
            let response = router
                .clone()
                .oneshot(Request::get("/health").body(Body::empty()).unwrap())
                .await
                .unwrap();
            pty.set_blocking_for_test(false);
            response
        } else {
            owner.abort();
            tokio::time::timeout(Duration::from_secs(2), async {
                while !owner.is_finished() {
                    tokio::task::yield_now().await;
                }
            })
            .await
            .unwrap();
            assert!(state.processes.registry.lock().unwrap().live.is_empty());
            router
                .oneshot(Request::get("/health").body(Body::empty()).unwrap())
                .await
                .unwrap()
        };
        // Remove the wait authority before the test cleanup reaps the PID.
        owner.abort();
        tokio::time::timeout(Duration::from_secs(2), async {
            while !owner.is_finished() {
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap();
        assert_eq!(response.status(), http::StatusCode::SERVICE_UNAVAILABLE);
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn terminal_reaping_waits_for_atomic_registry_removal() {
        let directory = tempfile::tempdir().unwrap();
        let release = directory.path().join("release");
        let manager = Arc::new(ProcessManager::default());
        let request = serde_json::from_value(serde_json::json!({"process":{
            "cmd":"/bin/sh","args":["-c","while [ ! -e \"$1\" ]; do /bin/sleep .01; done","hold",release]
        },"tag":"terminal"})).unwrap();
        let pid = manager
            .start(
                request,
                &http::HeaderMap::new(),
                RuntimeStateStore::new().snapshot().await,
                &UserDatabase::system(),
                Arc::new(LifecycleState::new()),
            )
            .await
            .unwrap();
        // Keep publication/removal locked while the leader exits. Its PID must
        // remain reserved by the kernel until the registry can remove it.
        let pid = pid.pid;
        let status = format!("/proc/{pid}/stat");
        {
            let registry = manager.registry.lock().unwrap();
            std::fs::write(release, "").unwrap();
            let end = std::time::Instant::now() + Duration::from_secs(2);
            loop {
                let contents = std::fs::read_to_string(&status)
                    .expect("PID must not be reaped before removal");
                if contents.split_once(") ").unwrap().1.starts_with('Z') {
                    break;
                }
                assert!(std::time::Instant::now() < end);
                std::thread::sleep(Duration::from_millis(5));
            }
            std::thread::sleep(Duration::from_millis(50));
            assert!(
                std::path::Path::new(&status).exists(),
                "old PID must not become reusable while its registry entry remains"
            );
            assert!(registry.live.contains_key(&pid));
        }
        tokio::time::timeout(Duration::from_secs(2), async {
            while !manager.list().unwrap().processes.is_empty() {
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .unwrap();
        assert!(!std::path::Path::new(&status).exists());
    }

    #[tokio::test]
    async fn preparing_failures_are_invisible_and_release_every_reservation() {
        for stage in [
            SetupStage::Reserved,
            SetupStage::Cwd,
            SetupStage::Resources,
            SetupStage::Forked,
            SetupStage::Execed,
            SetupStage::Publication,
        ] {
            let manager = Arc::new(ProcessManager::default());
            let runtime = RuntimeStateStore::new();
            let lifecycle = Arc::new(LifecycleState::new());
            lifecycle.try_transition(ServerPhase::Listening);
            lifecycle.try_transition(ServerPhase::Ready);
            let request: StartRequest = serde_json::from_value(
                serde_json::json!({"process":{"cmd":"/bin/sleep","args":["0.5"]},"tag":"reuse"}),
            )
            .unwrap();
            let (entered, reached) = tokio::sync::oneshot::channel();
            let (release, released) = tokio::sync::oneshot::channel();
            *manager.fault.lock().unwrap() = Some(SetupFault {
                stage,
                entered,
                release: released,
            });
            let starting = {
                let manager = manager.clone();
                let snapshot = runtime.snapshot().await;
                let lifecycle = lifecycle.clone();
                let request = request.clone();
                tokio::spawn(async move {
                    manager
                        .start(
                            request,
                            &http::HeaderMap::new(),
                            snapshot,
                            &UserDatabase::system(),
                            lifecycle,
                        )
                        .await
                })
            };
            tokio::time::timeout(Duration::from_secs(2), reached)
                .await
                .expect("Preparing stage must be controllable")
                .unwrap();
            assert!(manager.list().unwrap().processes.is_empty(), "{stage:?}");
            release.send(Err(DomainError::Internal)).unwrap();
            assert!(matches!(
                starting.await.unwrap(),
                Err(DomainError::Internal)
            ));
            assert!(
                manager.registry.lock().unwrap().tags.is_empty(),
                "{stage:?}"
            );
            let pid = manager
                .start(
                    request,
                    &http::HeaderMap::new(),
                    runtime.snapshot().await,
                    &UserDatabase::system(),
                    lifecycle.clone(),
                )
                .await
                .unwrap();
            tokio::time::timeout(Duration::from_secs(3), async {
                while !manager.list().unwrap().processes.is_empty() {
                    tokio::time::sleep(Duration::from_millis(10)).await;
                }
            })
            .await
            .unwrap();
            assert!(!std::path::Path::new(&format!("/proc/{}", pid.pid)).exists());
            assert!(manager.registry.lock().unwrap().tags.is_empty());
            assert!(lifecycle.is_ready());
        }
    }

    #[tokio::test]
    async fn concurrent_init_cannot_change_a_preparing_snapshot() {
        let first = tempfile::tempdir().unwrap();
        let second = tempfile::tempdir().unwrap();
        let manager = Arc::new(ProcessManager::default());
        let runtime = RuntimeStateStore::new();
        let users = UserDatabase::system();
        runtime
            .apply(
                serde_json::from_value(serde_json::json!({
                    "defaultWorkdir":first.path(),"envVars":{"SNAPSHOT":"first"}
                }))
                .unwrap(),
                &users,
            )
            .await
            .unwrap();
        let request: StartRequest = serde_json::from_value(serde_json::json!({
            "process":{"cmd":"/bin/sh","args":["-c","printf '%s' \"$SNAPSHOT\" > observed; exec /bin/sleep 0.5"]}
        })).unwrap();
        let original = request.process.clone();
        let (entered, reached) = tokio::sync::oneshot::channel();
        let (release, released) = tokio::sync::oneshot::channel();
        *manager.fault.lock().unwrap() = Some(SetupFault {
            stage: SetupStage::Reserved,
            entered,
            release: released,
        });
        let starting = {
            let manager = manager.clone();
            let snapshot = runtime.snapshot().await;
            tokio::spawn(async move {
                manager
                    .start(
                        request,
                        &http::HeaderMap::new(),
                        snapshot,
                        &UserDatabase::system(),
                        Arc::new(LifecycleState::new()),
                    )
                    .await
            })
        };
        reached.await.unwrap();
        runtime
            .apply(
                serde_json::from_value(serde_json::json!({
                    "defaultWorkdir":second.path(),"envVars":{"SNAPSHOT":"second"}
                }))
                .unwrap(),
                &users,
            )
            .await
            .unwrap();
        release.send(Ok(())).unwrap();
        starting.await.unwrap().unwrap();
        assert_eq!(manager.list().unwrap().processes[0].config, original);
        tokio::time::timeout(Duration::from_secs(2), async {
            while std::fs::read(first.path().join("observed")).unwrap_or_default() != b"first" {
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .unwrap();
        assert!(!second.path().join("observed").exists());
    }

    #[tokio::test]
    async fn deadline_at_every_preparing_stage_reaps_child_before_returning() {
        for stage in [
            SetupStage::Reserved,
            SetupStage::Cwd,
            SetupStage::Resources,
            SetupStage::Forked,
            SetupStage::Execed,
            SetupStage::Publication,
        ] {
            let directory = tempfile::tempdir().unwrap();
            let marker = directory.path().join("pid");
            let manager = Arc::new(ProcessManager::default());
            let runtime = RuntimeStateStore::new();
            let lifecycle = Arc::new(LifecycleState::new());
            let request: StartRequest = serde_json::from_value(serde_json::json!({
                "process":{"cmd":"/bin/sh","args":["-c","echo $$ > \"$1\"; exec /bin/sleep 30","test",marker]},"tag":"deadline"
            })).unwrap();
            let (entered, reached) = tokio::sync::oneshot::channel();
            let (_release, released) = tokio::sync::oneshot::channel();
            *manager.fault.lock().unwrap() = Some(SetupFault {
                stage,
                entered,
                release: released,
            });
            let starting = {
                let manager = manager.clone();
                let snapshot = runtime.snapshot().await;
                tokio::spawn(async move {
                    let mut headers = http::HeaderMap::new();
                    headers.insert("connect-timeout-ms", "10000".parse().unwrap());
                    manager
                        .start(
                            request,
                            &headers,
                            snapshot,
                            &UserDatabase::system(),
                            lifecycle,
                        )
                        .await
                })
            };
            tokio::time::timeout(Duration::from_secs(2), reached)
                .await
                .unwrap_or_else(|error| panic!("{stage:?}: {error}"))
                .unwrap();
            let child = if matches!(
                stage,
                SetupStage::Forked | SetupStage::Execed | SetupStage::Publication
            ) {
                Some(
                    tokio::time::timeout(Duration::from_secs(2), async {
                        loop {
                            if let Ok(contents) = std::fs::read_to_string(&marker) {
                                if let Ok(pid) = contents.trim().parse::<u32>() {
                                    break pid;
                                }
                            }
                            tokio::time::sleep(Duration::from_millis(5)).await;
                        }
                    })
                    .await
                    .unwrap(),
                )
            } else {
                None
            };
            assert!(manager.list().unwrap().processes.is_empty());
            tokio::time::pause();
            tokio::time::advance(Duration::from_secs(20)).await;
            assert!(
                matches!(starting.await.unwrap(), Err(DomainError::DeadlineExceeded)),
                "{stage:?}"
            );
            tokio::time::resume();
            assert!(manager.registry.lock().unwrap().tags.is_empty());
            if let Some(pid) = child {
                assert!(
                    !std::path::Path::new(&format!("/proc/{pid}")).exists(),
                    "{stage:?}"
                );
            }
        }
    }
}

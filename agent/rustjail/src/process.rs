// Copyright (c) 2019 Ant Financial
//
// SPDX-License-Identifier: Apache-2.0
//

use std::collections::HashMap;
use std::fs::File;
use std::os::unix::io::{AsRawFd, RawFd};
use std::os::unix::net::UnixStream;
use std::result;
use std::sync::Arc;

use awaitgroup::WaitGroup;
use libc::pid_t;
use nix::errno::Errno;
use nix::fcntl::{self, FcntlArg, FdFlag, OFlag};
use nix::pty;
use nix::sys::socket::{sendmsg, ControlMessage, MsgFlags};
use nix::sys::wait::{self, WaitStatus};
use nix::unistd::{self, Pid};
use nix::Result;
use oci::Process as OCIProcess;
use slog::{debug, info, warn, Logger};
use tokio::io::{split, ReadHalf, WriteHalf};
use tokio::sync::mpsc::Sender;
use tokio::sync::Mutex;
use tokio::sync::Notify;

use crate::pipestream::PipeStream;

macro_rules! close_process_stream {
    ($self: ident, $stream:ident, $stream_type: ident) => {
        if $self.$stream.is_some() {
            $self.close_stream(StreamType::$stream_type);
            let _ = unistd::close($self.$stream.unwrap());
            $self.$stream = None;
        }
    };
}

fn set_log_pipe_size(fd: RawFd, requested: i32, logger: &Logger, label: &str) {
    match fcntl::fcntl(fd, FcntlArg::F_SETPIPE_SZ(requested)) {
        Ok(actual) if actual < requested => {
            warn!(
                logger,
                "{} pipe buffer clamped to {} bytes (requested {})", label, actual, requested
            );
        }
        Err(e) => {
            warn!(logger, "F_SETPIPE_SZ {} pipe failed: {:?}", label, e);
        }
        _ => {}
    }
}

#[derive(Debug, PartialEq, Eq, Hash, Clone)]
pub enum StreamType {
    Stdin,
    Stdout,
    Stderr,
    TermMaster,
    ParentStdin,
    ParentStdout,
    ParentStderr,
}

type Reader = Arc<Mutex<ReadHalf<PipeStream>>>;
type Writer = Arc<Mutex<WriteHalf<PipeStream>>>;

#[derive(Debug)]
pub struct Process {
    pub container_id: String,
    pub exec_id: String,
    pub stdin: Option<RawFd>,
    pub stdout: Option<RawFd>,
    pub stderr: Option<RawFd>,
    pub exit_tx: Option<tokio::sync::watch::Sender<bool>>,
    pub exit_rx: Option<tokio::sync::watch::Receiver<bool>>,
    pub extra_files: Vec<File>,
    pub cubemsg_dev: Option<File>,
    pub term_master: Option<RawFd>,
    pub term_slave: Option<RawFd>,
    pub tty: bool,
    /// Init process only.  Set by rpc.do_create_container from the
    /// `cube.container.log_forwarding` annotation.  When true, open_io()
    /// creates stdout/stderr log pipes for the init process.  Exec processes
    /// (`init == false`) never consult this flag.
    pub log_forwarding: bool,
    pub parent_stdin: Option<RawFd>,
    pub parent_stdout: Option<RawFd>,
    pub parent_stderr: Option<RawFd>,
    pub init: bool,
    // pid of the init/exec process. since we have no command
    // struct to store pid, we must store pid here.
    pub pid: pid_t,

    pub exit_code: i32,
    pub exited: bool,
    pub exit_watchers: Vec<Sender<i32>>,
    pub oci: OCIProcess,
    pub logger: Logger,
    pub term_exit_notifier: Arc<Notify>,
    pub readers: HashMap<StreamType, Reader>,
    pub writers: HashMap<StreamType, Writer>,
    pub proc_io: Option<ProcessIo>,
    pub passfd_stdin_task: Option<tokio::task::JoinHandle<()>>,
    pub passfd_tasks: Vec<tokio::task::JoinHandle<()>>,
}

#[derive(Debug)]
pub struct ProcessIo {
    pub stdin: Option<tokio_vsock::VsockStream>,
    pub stdout: Option<tokio_vsock::VsockStream>,
    pub stderr: Option<tokio_vsock::VsockStream>,
    /// WaitGroup for output streams (stdout/stderr), used to ensure all output
    /// is copied to vsock streams before process exits. Used in both tty and non-tty modes.
    pub wg_output: WaitGroup,
}

impl ProcessIo {
    pub fn new(
        stdin: Option<tokio_vsock::VsockStream>,
        stdout: Option<tokio_vsock::VsockStream>,
        stderr: Option<tokio_vsock::VsockStream>,
    ) -> Self {
        Self {
            stdin,
            stdout,
            stderr,
            wg_output: WaitGroup::new(),
        }
    }
}

pub trait ProcessOperations {
    fn pid(&self) -> Pid;
    fn wait(&self) -> Result<WaitStatus>;
    fn signal(&self, sig: libc::c_int) -> Result<()>;
}

impl ProcessOperations for Process {
    fn pid(&self) -> Pid {
        Pid::from_raw(self.pid)
    }

    fn wait(&self) -> Result<WaitStatus> {
        wait::waitpid(Some(self.pid()), None)
    }

    fn signal(&self, sig: libc::c_int) -> Result<()> {
        let res = unsafe { libc::kill(self.pid().into(), sig) };

        Errno::result(res).map(drop)
    }
}

fn send_fd(socket_path: &str, fd: RawFd) -> result::Result<(), String> {
    // Connect to the Unix socket
    let stream = UnixStream::connect(socket_path)
        .map_err(|e| format!("Failed to connect to {}, err:{:?}", socket_path, e))?;

    let binding = [fd];
    // Prepare the control message with the file descriptor
    let cmsg = ControlMessage::ScmRights(&binding);
    let iov = [nix::sys::uio::IoVec::from_slice(&[0u8])];
    // Send the file descriptor
    sendmsg(stream.as_raw_fd(), &iov, &[cmsg], MsgFlags::empty(), None)
        .map_err(|e| format!("Failed to sendmsg to {}, err:{:?}", socket_path, e))?;

    Ok(())
}

impl Process {
    pub fn new(
        logger: &Logger,
        ocip: &OCIProcess,
        id: &str,
        init: bool,
        _pipe_size: i32,
    ) -> Result<Self> {
        let logger = logger.new(o!("subsystem" => "process"));
        let (exit_tx, exit_rx) = tokio::sync::watch::channel(false);

        let p = Process {
            container_id: String::new(),
            exec_id: String::from(id),
            stdin: None,
            stdout: None,
            stderr: None,
            exit_tx: Some(exit_tx),
            exit_rx: Some(exit_rx),
            extra_files: Vec::new(),
            tty: ocip.terminal,
            log_forwarding: false,
            term_master: None,
            term_slave: None,
            cubemsg_dev: None,
            parent_stdin: None,
            parent_stdout: None,
            parent_stderr: None,
            init,
            pid: -1,
            exit_code: 0,
            exited: false,
            exit_watchers: Vec::new(),
            oci: ocip.clone(),
            logger: logger.clone(),
            term_exit_notifier: Arc::new(Notify::new()),
            readers: HashMap::new(),
            writers: HashMap::new(),
            proc_io: None,
            passfd_stdin_task: None,
            passfd_tasks: Vec::new(),
        };

        Ok(p)
    }

    pub fn open_io(
        &mut self,
        logger: &Logger,
        target: Option<&String>,
    ) -> result::Result<(), String> {
        if self.tty {
            debug!(logger, "tty is true");
            let pseudo = pty::openpty(None, None).map_err(|e| format!("openpty failed:{:?}", e))?;
            let _ = fcntl::fcntl(pseudo.master, FcntlArg::F_SETFD(FdFlag::FD_CLOEXEC))
                .map_err(|e| format!("fnctl pseudo.master {:?}", e));
            let _ = fcntl::fcntl(pseudo.slave, FcntlArg::F_SETFD(FdFlag::FD_CLOEXEC))
                .map_err(|e| format!("fcntl pseudo.slave {:?}", e));
            self.term_master = Some(pseudo.master);
            self.term_slave = Some(pseudo.slave);
            self.stdin = Some(pseudo.slave);
            self.stdout = Some(pseudo.slave);
            self.stderr = Some(pseudo.slave);

            if let Some(sock_addr) = target {
                send_fd(&sock_addr, pseudo.master)
                    .map_err(|e| format!("send pty to runtime socket failed {:?}", e))?;
            }
            return Ok(());
        }

        // If passfd is enabled, conditionally create pipes for stdin, stdout, stderr
        if let Some(proc_io) = &self.proc_io {
            let io_configs = [
                (
                    proc_io.stdin.is_some(),
                    &mut self.stdin,
                    &mut self.parent_stdin,
                    "stdin",
                    true,
                ),
                (
                    proc_io.stdout.is_some(),
                    &mut self.stdout,
                    &mut self.parent_stdout,
                    "stdout",
                    false,
                ),
                (
                    proc_io.stderr.is_some(),
                    &mut self.stderr,
                    &mut self.parent_stderr,
                    "stderr",
                    false,
                ),
            ];

            for (enabled, child_fd, parent_fd, name, is_stdin) in io_configs {
                if enabled {
                    let (r, w) = unistd::pipe2(OFlag::O_CLOEXEC)
                        .map_err(|e| format!("create {} pipe failed: {:?}", name, e))?;
                    let (child, parent) = if is_stdin { (r, w) } else { (w, r) };
                    fcntl::fcntl(child, FcntlArg::F_SETFD(FdFlag::empty()))
                        .map_err(|e| format!("set {} fd flag failed: {:?}", name, e))?;
                    *child_fd = Some(child);
                    *parent_fd = Some(parent);
                }
            }

            return Ok(());
        }

        // Exec processes: unchanged from pre-log-forwarding (no agent-side pipes).
        if !self.init {
            return Ok(());
        }

        // Init process: create log pipes only when log forwarding is enabled.
        if !self.log_forwarding {
            return Ok(());
        }

        // Init log-forwarding path: create pipes so the shim can poll container
        // stdout/stderr via do_read_stream over vsock.
        //
        // Pipe layout:
        //   container process  --> [child_w]  pipe  [parent_r] --> agent do_read_stream
        //
        // The write end (child_w) is NOT O_CLOEXEC so the child process
        // inherits it; the read end (parent_r) IS O_CLOEXEC so it stays
        // only in the agent.
        //
        // We intentionally set O_NONBLOCK on the write end: during snapshot
        // restore there is a window between the container resuming and the shim
        // calling start_log_forward.  If the pipe fills up in that window,
        // O_NONBLOCK makes the container's write() return EAGAIN (log line
        // dropped) rather than blocking the container process indefinitely.
        //
        // Request a 1 MiB pipe buffer to reduce drops during the restore window.
        // This matches the kernel's /proc/sys/fs/pipe-max-size limit (1 MiB),
        // so no clamping occurs.
        const LOG_PIPE_SIZE: i32 = 1024 * 1024; // 1 MiB

        let (parent_stdout_r, child_stdout_w) = unistd::pipe2(OFlag::O_CLOEXEC)
            .map_err(|e| format!("create stdout pipe failed: {:?}", e))?;
        set_log_pipe_size(child_stdout_w, LOG_PIPE_SIZE, logger, "stdout");
        // Clear O_CLOEXEC on the write end so the container inherits it.
        fcntl::fcntl(child_stdout_w, FcntlArg::F_SETFD(FdFlag::empty()))
            .map_err(|e| format!("set stdout fd flag failed: {:?}", e))?;
        fcntl::fcntl(child_stdout_w, FcntlArg::F_SETFL(OFlag::O_NONBLOCK))
            .map_err(|e| format!("set stdout nonblock failed: {:?}", e))?;

        let (parent_stderr_r, child_stderr_w) = match unistd::pipe2(OFlag::O_CLOEXEC) {
            Ok(fds) => fds,
            Err(e) => {
                let _ = unistd::close(parent_stdout_r);
                let _ = unistd::close(child_stdout_w);
                return Err(format!("create stderr pipe failed: {:?}", e));
            }
        };
        set_log_pipe_size(child_stderr_w, LOG_PIPE_SIZE, logger, "stderr");
        fcntl::fcntl(child_stderr_w, FcntlArg::F_SETFD(FdFlag::empty()))
            .map_err(|e| format!("set stderr fd flag failed: {:?}", e))?;
        fcntl::fcntl(child_stderr_w, FcntlArg::F_SETFL(OFlag::O_NONBLOCK))
            .map_err(|e| format!("set stderr nonblock failed: {:?}", e))?;

        debug!(
            logger,
            "container log pipes created: \
             stdout child_w={} parent_r={}, stderr child_w={} parent_r={}",
            child_stdout_w,
            parent_stdout_r,
            child_stderr_w,
            parent_stderr_r,
        );

        self.stdout = Some(child_stdout_w);
        self.stderr = Some(child_stderr_w);
        self.parent_stdout = Some(parent_stdout_r);
        self.parent_stderr = Some(parent_stderr_r);

        Ok(())
    }

    /// Internal helper to wire passfd streams to process pipes/pty.
    /// Called by both setup_passfd_io and reconnect_passfd.
    fn wire_passfd_streams(&mut self) {
        let tty = self.tty;
        let term_master = self.term_master;
        let parent_stdin = self.parent_stdin;
        let parent_stdout = self.parent_stdout;
        let parent_stderr = self.parent_stderr;
        let logger = self.logger.clone();

        if let Some(proc_io) = &mut self.proc_io {
            if tty {
                let stdin_stream = proc_io.stdin.take();
                let output_stream = match (proc_io.stdout.take(), proc_io.stderr.take()) {
                    (Some(stdout), Some(_stderr)) => {
                        warn!(logger, "TTY passfd received both stdout and stderr; using combined stdout stream"; "container_id" => self.container_id.clone());
                        Some(stdout)
                    }
                    (Some(stdout), None) => Some(stdout),
                    (None, stderr) => stderr,
                };

                match (stdin_stream, output_stream, term_master) {
                    (Some(stream), None, Some(tm)) => {
                        match (nix::unistd::dup(tm), nix::unistd::dup(tm)) {
                            (Ok(input_fd), Ok(output_fd)) => {
                                let input_pty = PipeStream::from_fd(input_fd);
                                let mut output_pty = PipeStream::from_fd(output_fd);
                                let (stream_r, mut stream_w) = tokio::io::split(stream);

                                let logger_clone = logger.clone();
                                let input_task = tokio::spawn(async move {
                                    copy_passfd_stdin(
                                        stream_r,
                                        input_pty,
                                        logger_clone,
                                        "tty-combined",
                                    )
                                    .await;
                                });
                                self.passfd_stdin_task = Some(input_task);

                                let wg_worker = proc_io.wg_output.worker();
                                let task = tokio::spawn(async move {
                                    let _ = tokio::io::copy(&mut output_pty, &mut stream_w).await;
                                    let _ = tokio::io::AsyncWriteExt::shutdown(&mut stream_w).await;
                                    wg_worker.done();
                                });
                                self.passfd_tasks.push(task);
                            }
                            (Ok(input_fd), Err(_)) => {
                                let _ = nix::unistd::close(input_fd);
                                warn!(logger, "Failed to dup term_master for passfd stream");
                            }
                            (Err(_), Ok(output_fd)) => {
                                let _ = nix::unistd::close(output_fd);
                                warn!(logger, "Failed to dup term_master for passfd stream");
                            }
                            (Err(_), Err(_)) => {
                                warn!(logger, "Failed to dup term_master for passfd stream");
                            }
                        }
                    }
                    (stdin_stream, output_stream, Some(tm)) => {
                        if let Some(stream) = stdin_stream {
                            if let Ok(dup_fd) = nix::unistd::dup(tm) {
                                let input_pty = PipeStream::from_fd(dup_fd);
                                let logger_clone = logger.clone();
                                let task = tokio::spawn(async move {
                                    copy_passfd_stdin(stream, input_pty, logger_clone, "tty").await;
                                });
                                self.passfd_stdin_task = Some(task);
                            } else {
                                warn!(logger, "Failed to dup term_master for passfd stdin");
                            }
                        }

                        if let Some(mut stream) = output_stream {
                            if let Ok(dup_fd) = nix::unistd::dup(tm) {
                                let wg_worker = proc_io.wg_output.worker();
                                let mut output_pty = PipeStream::from_fd(dup_fd);
                                let task = tokio::spawn(async move {
                                    let _ = tokio::io::copy(&mut output_pty, &mut stream).await;
                                    let _ = tokio::io::AsyncWriteExt::shutdown(&mut stream).await;
                                    wg_worker.done();
                                });
                                self.passfd_tasks.push(task);
                            } else {
                                warn!(logger, "Failed to dup term_master for passfd stdout");
                            }
                        }
                    }
                    _ => {}
                }
            } else {
                // Non-TTY mode: separate streams for stdin/stdout/stderr
                if let (Some(stream), Some(parent_stdin)) = (proc_io.stdin.take(), parent_stdin) {
                    if let Ok(dup_fd) = nix::unistd::dup(parent_stdin) {
                        let stdin_pipe = PipeStream::from_fd(dup_fd);
                        let logger_clone = logger.clone();
                        let task = tokio::spawn(async move {
                            copy_passfd_stdin(stream, stdin_pipe, logger_clone, "pipe").await;
                        });
                        self.passfd_stdin_task = Some(task);
                        // Keep the original writer for CloseIO and resume-time reconnection.
                    } else {
                        warn!(logger, "Failed to dup parent_stdin for passfd stream");
                    }
                }
                if let (Some(mut stream), Some(parent_stdout)) =
                    (proc_io.stdout.take(), parent_stdout)
                {
                    if let Ok(dup_fd) = nix::unistd::dup(parent_stdout) {
                        let wg_worker = proc_io.wg_output.worker();
                        let mut stdout_pipe = PipeStream::from_fd(dup_fd);
                        let task = tokio::spawn(async move {
                            let _ = tokio::io::copy(&mut stdout_pipe, &mut stream).await;
                            let _ = tokio::io::AsyncWriteExt::shutdown(&mut stream).await;
                            wg_worker.done();
                        });
                        self.passfd_tasks.push(task);
                    } else {
                        warn!(logger, "Failed to dup parent_stdout for passfd stream");
                    }
                }
                if let (Some(mut stream), Some(parent_stderr)) =
                    (proc_io.stderr.take(), parent_stderr)
                {
                    if let Ok(dup_fd) = nix::unistd::dup(parent_stderr) {
                        let wg_worker = proc_io.wg_output.worker();
                        let mut stderr_pipe = PipeStream::from_fd(dup_fd);
                        let task = tokio::spawn(async move {
                            let _ = tokio::io::copy(&mut stderr_pipe, &mut stream).await;
                            let _ = tokio::io::AsyncWriteExt::shutdown(&mut stream).await;
                            wg_worker.done();
                        });
                        self.passfd_tasks.push(task);
                    } else {
                        warn!(logger, "Failed to dup parent_stderr for passfd stream");
                    }
                }
            }
        }
    }

    pub async fn setup_passfd_io(&mut self) {
        self.wire_passfd_streams();
    }

    pub async fn reconnect_passfd(&mut self, new_io: ProcessIo) -> anyhow::Result<()> {
        self.validate_reconnect_passfd(&new_io)?;

        // Wait for old tasks to stop before new readers can consume the same pipe or PTY.
        self.abort_and_wait_passfd_tasks().await;

        // Replace proc_io with new_io, keeping wg_output accessible
        self.proc_io = Some(new_io);
        self.wire_passfd_streams();
        Ok(())
    }

    fn validate_reconnect_passfd(&self, new_io: &ProcessIo) -> anyhow::Result<()> {
        if self.tty {
            if (new_io.stdin.is_some() || new_io.stdout.is_some() || new_io.stderr.is_some())
                && self.term_master.is_none()
            {
                anyhow::bail!("cannot reconnect passfd IO without the existing terminal master");
            }
        } else {
            if new_io.stdin.is_some() && self.parent_stdin.is_none() {
                anyhow::bail!("cannot reconnect passfd stdin: process has no stdin endpoint");
            }
            if new_io.stdout.is_some() && self.parent_stdout.is_none() {
                anyhow::bail!("cannot reconnect passfd stdout: process has no stdout endpoint");
            }
            if new_io.stderr.is_some() && self.parent_stderr.is_none() {
                anyhow::bail!("cannot reconnect passfd stderr: process has no stderr endpoint");
            }
        }
        Ok(())
    }

    async fn abort_and_wait_passfd_tasks(&mut self) {
        if let Some(task) = self.passfd_stdin_task.take() {
            task.abort();
            let _ = task.await;
        }
        for task in self.passfd_tasks.drain(..) {
            task.abort();
            let _ = task.await;
        }
    }

    pub fn abort_passfd_tasks(&mut self) {
        if let Some(task) = self.passfd_stdin_task.take() {
            task.abort();
        }
        for task in self.passfd_tasks.drain(..) {
            task.abort();
        }
    }

    pub fn notify_term_close(&mut self) {
        let notify = self.term_exit_notifier.clone();
        notify.notify_one();
    }

    pub async fn close_stdin(&mut self) {
        if let Some(task) = self.passfd_stdin_task.take() {
            task.abort();
            let _ = task.await;
        }
        close_process_stream!(self, term_master, TermMaster);
        close_process_stream!(self, parent_stdin, ParentStdin);

        self.notify_term_close();
    }

    /// Close the agent's copy of child-side stdio fds after spawn.
    /// The child keeps its duplicated fds; the agent only retains parent_* ends
    /// for passfd/log forwarding.
    pub fn close_inherited_write_ends(&mut self) {
        if self.tty {
            self.close_stream(StreamType::Stdin);
            self.close_stream(StreamType::Stdout);
            self.close_stream(StreamType::Stderr);

            let mut fds = Vec::new();
            for fd in [
                self.term_slave.take(),
                self.stdin.take(),
                self.stdout.take(),
                self.stderr.take(),
            ]
            .into_iter()
            .flatten()
            {
                if !fds.contains(&fd) {
                    fds.push(fd);
                }
            }
            for fd in fds {
                let _ = unistd::close(fd);
            }
            return;
        }

        if !self.log_forwarding && self.proc_io.is_none() {
            return;
        }

        if self.proc_io.is_some() {
            close_process_stream!(self, stdin, Stdin);
        }
        close_process_stream!(self, stdout, Stdout);
        close_process_stream!(self, stderr, Stderr);
    }

    pub fn cleanup_process_stream(&mut self) {
        self.abort_passfd_tasks();

        // In passfd mode, drop VsockStreams and close the agent-owned process fds.
        // Copy tasks use dup'd fds, so closing these originals here is safe.
        if let Some(proc_io) = self.proc_io.take() {
            drop(proc_io);
            close_process_stream!(self, parent_stdin, ParentStdin);
            close_process_stream!(self, parent_stdout, ParentStdout);
            close_process_stream!(self, parent_stderr, ParentStderr);
            close_process_stream!(self, term_master, TermMaster);
            return;
        }

        // legacy io mode
        close_process_stream!(self, parent_stdin, ParentStdin);
        close_process_stream!(self, parent_stdout, ParentStdout);
        close_process_stream!(self, parent_stderr, ParentStderr);
        close_process_stream!(self, term_master, TermMaster);
        self.close_inherited_write_ends();

        self.notify_term_close();
    }

    fn get_fd(&self, stream_type: &StreamType) -> Option<RawFd> {
        match stream_type {
            StreamType::Stdin => self.stdin,
            StreamType::Stdout => self.stdout,
            StreamType::Stderr => self.stderr,
            StreamType::TermMaster => self.term_master,
            StreamType::ParentStdin => self.parent_stdin,
            StreamType::ParentStdout => self.parent_stdout,
            StreamType::ParentStderr => self.parent_stderr,
        }
    }

    fn get_stream_and_store(&mut self, stream_type: StreamType) -> Option<(Reader, Writer)> {
        let fd = self.get_fd(&stream_type)?;
        let stream = PipeStream::from_fd(fd);

        let (reader, writer) = split(stream);
        let reader = Arc::new(Mutex::new(reader));
        let writer = Arc::new(Mutex::new(writer));

        self.readers.insert(stream_type.clone(), reader.clone());
        self.writers.insert(stream_type, writer.clone());

        Some((reader, writer))
    }

    pub fn get_reader(&mut self, stream_type: StreamType) -> Option<Reader> {
        if let Some(reader) = self.readers.get(&stream_type) {
            return Some(reader.clone());
        }

        let (reader, _) = self.get_stream_and_store(stream_type)?;
        Some(reader)
    }

    /// After snapshot restore, cached Tokio `PipeStream` wrappers still point
    /// at the pre-snapshot reactor. Dup the log-pipe read ends and drop the
    /// cache so the next `read_stdout` / `read_stderr` attaches cleanly.
    /// The write ends inherited by the init process are unchanged.
    pub fn reset_log_stream_readers(&mut self) -> result::Result<(), String> {
        if let Some(fd) = self.parent_stdout {
            let dup = unistd::dup(fd).map_err(|e| format!("dup parent_stdout: {:?}", e))?;
            self.parent_stdout = Some(dup);
        }
        if let Some(fd) = self.parent_stderr {
            let dup = unistd::dup(fd).map_err(|e| format!("dup parent_stderr: {:?}", e))?;
            self.parent_stderr = Some(dup);
        }
        self.readers.clear();
        self.writers.clear();
        Ok(())
    }

    pub fn get_writer(&mut self, stream_type: StreamType) -> Option<Writer> {
        if let Some(writer) = self.writers.get(&stream_type) {
            return Some(writer.clone());
        }

        let (_, writer) = self.get_stream_and_store(stream_type)?;
        Some(writer)
    }

    pub fn close_stream(&mut self, stream_type: StreamType) {
        let _ = self.readers.remove(&stream_type);
        let _ = self.writers.remove(&stream_type);
    }
}

/*
fn create_extended_pipe(flags: OFlag, pipe_size: i32) -> Result<(RawFd, RawFd)> {
    let (r, w) = unistd::pipe2(flags)?;
    if pipe_size > 0 {
        fcntl::fcntl(w, FcntlArg::F_SETPIPE_SZ(pipe_size))?;
    }
    Ok((r, w))
}*/

async fn copy_passfd_stdin<R, W>(mut reader: R, mut writer: W, logger: Logger, label: &'static str)
where
    R: tokio::io::AsyncRead + Unpin,
    W: tokio::io::AsyncWrite + Unpin,
{
    match tokio::io::copy(&mut reader, &mut writer).await {
        Ok(bytes) => {
            info!(logger, "passfd stdin copy finished"; "label" => label, "bytes" => bytes);
        }
        Err(err) => {
            warn!(
                logger,
                "passfd stdin copy failed";
                "label" => label,
                "error" => format!("{}", err)
            );
        }
    }
}

#[cfg(test)]
mod tests {
    use std::os::unix::io::AsRawFd;

    use super::*;

    /*
    #[test]
    fn test_create_extended_pipe() {
        // Test the default
        let (_r, _w) = create_extended_pipe(OFlag::O_CLOEXEC, 0).unwrap();

        // Test setting to the max size
        let max_size = get_pipe_max_size();
        let (_, w) = create_extended_pipe(OFlag::O_CLOEXEC, max_size).unwrap();
        let actual_size = get_pipe_size(w);
        assert_eq!(max_size, actual_size);
    }*/

    #[test]
    fn test_process() {
        let id = "abc123rgb";
        let init = true;
        let process = Process::new(
            &Logger::root(slog::Discard, o!("source" => "unit-test")),
            &OCIProcess::default(),
            id,
            init,
            32,
        );

        let mut process = process.unwrap();
        assert_eq!(process.exec_id, id);
        assert_eq!(process.init, init);

        // -1 by default
        assert_eq!(process.pid, -1);
        // signal to every process in the process
        // group of the calling process.
        process.pid = 0;
        assert!(process.signal(libc::SIGCONT).is_ok());

        if cfg!(feature = "standard-oci-runtime") {
            assert_eq!(process.stdin.unwrap(), std::io::stdin().as_raw_fd());
            assert_eq!(process.stdout.unwrap(), std::io::stdout().as_raw_fd());
            assert_eq!(process.stderr.unwrap(), std::io::stderr().as_raw_fd());
        }
    }

    #[test]
    fn test_passfd_open_io_without_streams_does_not_create_stdio_pipes() {
        let id = "passfd-no-streams";
        let logger = Logger::root(slog::Discard, o!("source" => "unit-test"));
        let process = Process::new(&logger, &OCIProcess::default(), id, true, 32);

        let mut process = process.unwrap();
        process.proc_io = Some(ProcessIo::new(None, None, None));

        process.open_io(&logger, None).unwrap();

        assert!(process.stdin.is_none());
        assert!(process.stdout.is_none());
        assert!(process.stderr.is_none());
        assert!(process.parent_stdin.is_none());
        assert!(process.parent_stdout.is_none());
        assert!(process.parent_stderr.is_none());
    }

    #[test]
    fn test_reset_log_stream_readers_keeps_pipe_readable() {
        let (r, w) = unistd::pipe2(OFlag::O_CLOEXEC).unwrap();
        let logger = Logger::root(slog::Discard, o!("source" => "unit-test"));
        let mut process =
            Process::new(&logger, &OCIProcess::default(), "reset-io", true, 32).unwrap();
        process.parent_stdout = Some(r);
        process.reset_log_stream_readers().unwrap();
        assert!(process.readers.is_empty());
        assert!(process.writers.is_empty());
        let new_fd = process.parent_stdout.expect("duped stdout");
        assert_ne!(new_fd, r);
        unistd::write(w, b"x").unwrap();
        let mut buf = [0u8; 1];
        assert_eq!(unistd::read(new_fd, &mut buf).unwrap(), 1);
        assert_eq!(&buf, b"x");
        let _ = unistd::close(w);
        let _ = unistd::close(new_fd);
    }
}

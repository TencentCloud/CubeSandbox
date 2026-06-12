// Copyright (c) 2019 Ant Financial
//
// SPDX-License-Identifier: Apache-2.0
//

use libc::pid_t;
use std::fs::File;
use std::os::unix::io::{AsRawFd, RawFd};
use tokio::sync::mpsc::Sender;

use nix::errno::Errno;
use nix::fcntl::{self, FcntlArg, FdFlag, OFlag};
use nix::pty;
use nix::sys::wait::{self, WaitStatus};
use nix::unistd::{self, Pid};
use nix::Result;
use oci::Process as OCIProcess;
use slog::{debug, warn, Logger};
use std::result;

use crate::pipestream::PipeStream;
use nix::sys::socket::{sendmsg, ControlMessage, MsgFlags};
use std::collections::HashMap;
use std::os::unix::net::UnixStream;
use std::sync::Arc;
use tokio::io::{split, ReadHalf, WriteHalf};
use tokio::sync::Mutex;
use tokio::sync::Notify;

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
    pub exit_watchers: Vec<Sender<i32>>,
    pub oci: OCIProcess,
    pub logger: Logger,
    pub term_exit_notifier: Arc<Notify>,

    readers: HashMap<StreamType, Reader>,
    writers: HashMap<StreamType, Writer>,
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
            exit_watchers: Vec::new(),
            oci: ocip.clone(),
            logger: logger.clone(),
            term_exit_notifier: Arc::new(Notify::new()),
            readers: HashMap::new(),
            writers: HashMap::new(),
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
        let _ = fcntl::fcntl(child_stdout_w, FcntlArg::F_SETFD(FdFlag::empty()));
        let _ = fcntl::fcntl(child_stdout_w, FcntlArg::F_SETFL(OFlag::O_NONBLOCK));

        let (parent_stderr_r, child_stderr_w) = match unistd::pipe2(OFlag::O_CLOEXEC) {
            Ok(fds) => fds,
            Err(e) => {
                let _ = unistd::close(parent_stdout_r);
                let _ = unistd::close(child_stdout_w);
                return Err(format!("create stderr pipe failed: {:?}", e));
            }
        };
        set_log_pipe_size(child_stderr_w, LOG_PIPE_SIZE, logger, "stderr");
        let _ = fcntl::fcntl(child_stderr_w, FcntlArg::F_SETFD(FdFlag::empty()));
        let _ = fcntl::fcntl(child_stderr_w, FcntlArg::F_SETFL(OFlag::O_NONBLOCK));

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

    pub fn notify_term_close(&mut self) {
        let notify = self.term_exit_notifier.clone();
        notify.notify_one();
    }

    pub fn close_stdin(&mut self) {
        close_process_stream!(self, term_master, TermMaster);
        close_process_stream!(self, parent_stdin, ParentStdin);

        self.notify_term_close();
    }

    /// Close the agent's copy of container stdout/stderr write ends after spawn.
    /// The child keeps its inherited fds; the agent only retains parent_* read ends
    /// for log forwarding via do_read_stream.
    pub fn close_inherited_write_ends(&mut self) {
        if self.tty || !self.log_forwarding {
            return;
        }
        close_process_stream!(self, stdout, Stdout);
        close_process_stream!(self, stderr, Stderr);
    }

    pub fn cleanup_process_stream(&mut self) {
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::io::AsRawFd;

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
}

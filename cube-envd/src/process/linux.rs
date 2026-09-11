// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

//! Linux creation handshake. The fork child only uses raw, non-unwinding
//! syscalls; the parent owns every allocation and the one waitpid authority.

use std::ffi::CString;
use std::fs::File;
use std::io;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::os::unix::net::UnixStream;
use std::sync::{Arc, Mutex};

use tokio::io::unix::AsyncFd;
use tokio::io::AsyncReadExt;

use crate::error::DomainError;
use crate::runtime::ProcessUser;

// Readiness borrows the actual descriptor while its owner can still close it.
// The observation never duplicates an fd or keeps stdin/output open. Clearing
// it under the same lock before T drops prevents fd-number reuse during fcntl.
pub(crate) struct ProcessFd<T> {
    inner: T,
    readiness: Arc<DescriptorCheck>,
}

pub(crate) struct DescriptorCheck {
    fd: Mutex<Option<RawFd>>,
    // Pipes may normally close before their process exits. The live pidfd may
    // not; it is dropped only after atomic registry removal by the wait owner.
    pipe_access: Option<i32>,
}

impl<T: AsRawFd> ProcessFd<T> {
    pub fn new(inner: T, pipe_access: Option<i32>) -> Self {
        let readiness = Arc::new(DescriptorCheck {
            fd: Mutex::new(Some(inner.as_raw_fd())),
            pipe_access,
        });
        Self { inner, readiness }
    }

    pub fn readiness(&self) -> Arc<DescriptorCheck> {
        self.readiness.clone()
    }
}

impl<T> std::ops::Deref for ProcessFd<T> {
    type Target = T;
    fn deref(&self) -> &T {
        &self.inner
    }
}
impl<T> std::ops::DerefMut for ProcessFd<T> {
    fn deref_mut(&mut self) -> &mut T {
        &mut self.inner
    }
}
impl<T> Drop for ProcessFd<T> {
    fn drop(&mut self) {
        if let Ok(mut fd) = self.readiness.fd.lock() {
            *fd = None;
        }
    }
}
impl DescriptorCheck {
    pub fn self_check(&self) -> bool {
        let Ok(fd) = self.fd.lock() else {
            return false;
        };
        let Some(fd) = *fd else {
            return self.pipe_access.is_some();
        };
        let flags = unsafe { libc::fcntl(fd, libc::F_GETFL) };
        let descriptor_flags = unsafe { libc::fcntl(fd, libc::F_GETFD) };
        flags >= 0
            && descriptor_flags >= 0
            && descriptor_flags & libc::FD_CLOEXEC != 0
            && self.pipe_access.is_none_or(|access| {
                flags & libc::O_NONBLOCK != 0 && flags & libc::O_ACCMODE == access
            })
    }
}

pub(crate) struct Child {
    pub pid: u32,
    pidfd: ProcessFd<AsyncFd<OwnedFd>>,
    reaped: bool,
    pub live: bool,
    pub lifecycle: Option<std::sync::Arc<crate::server::LifecycleState>>,
    _cgroup: std::sync::Arc<crate::cgroup::ProcessCgroup>,
}

impl Child {
    pub fn readiness(&self) -> Arc<DescriptorCheck> {
        self.pidfd.readiness()
    }

    pub async fn exited(&self) -> Result<(), DomainError> {
        let _ready = self
            .pidfd
            .readable()
            .await
            .map_err(|_| DomainError::Internal)?;
        Ok(())
    }

    pub fn reap(&mut self) -> Result<i32, DomainError> {
        loop {
            let mut status = 0;
            let result = unsafe { libc::waitpid(self.pid as i32, &mut status, libc::WNOHANG) };
            if result == self.pid as i32 {
                self.reaped = true;
                return Ok(status);
            }
            if result >= 0 || io::Error::last_os_error().raw_os_error() != Some(libc::EINTR) {
                return Err(DomainError::Internal);
            }
        }
    }

    pub async fn kill_and_reap(&mut self) -> Result<(), DomainError> {
        // An unreaped, exclusively owned child cannot have its PID reused.
        unsafe {
            libc::kill(self.pid as i32, libc::SIGKILL);
        }
        self.exited().await?;
        self.reap().map(|_| ())
    }
}

pub(crate) fn signal(pid: u32, signal: i32) -> Result<(), DomainError> {
    if unsafe { libc::kill(pid as i32, signal) } == 0 {
        return Ok(());
    }
    match io::Error::last_os_error().raw_os_error() {
        Some(libc::ESRCH) => Err(DomainError::NotFound("process not found".into())),
        _ => Err(DomainError::Internal),
    }
}

/// Match Go ProcessState's Linux wait-status fields, including its signal
/// descriptions. libc strsignal varies by libc/locale (the candidate is musl).
pub(crate) fn end_event(wait: i32) -> crate::proto::process::process_event::EndEvent {
    let exited = libc::WIFEXITED(wait);
    let exit_code = if exited { libc::WEXITSTATUS(wait) } else { -1 };
    let status = if exited {
        format!("exit status {exit_code}")
    } else {
        const SIGNALS: [&str; 32] = [
            "",
            "hangup",
            "interrupt",
            "quit",
            "illegal instruction",
            "trace/breakpoint trap",
            "aborted",
            "bus error",
            "floating point exception",
            "killed",
            "user defined signal 1",
            "segmentation fault",
            "user defined signal 2",
            "broken pipe",
            "alarm clock",
            "terminated",
            "stack fault",
            "child exited",
            "continued",
            "stopped (signal)",
            "stopped",
            "stopped (tty input)",
            "stopped (tty output)",
            "urgent I/O condition",
            "CPU time limit exceeded",
            "file size limit exceeded",
            "virtual timer expired",
            "profiling timer expired",
            "window changed",
            "I/O possible",
            "power failure",
            "bad system call",
        ];
        let signal = libc::WTERMSIG(wait) as usize;
        let name = SIGNALS
            .get(signal)
            .map_or_else(|| format!("signal {signal}"), |name| (*name).to_owned());
        format!(
            "signal: {name}{}",
            if libc::WCOREDUMP(wait) {
                " (core dumped)"
            } else {
                ""
            }
        )
    };
    crate::proto::process::process_event::EndEvent {
        exit_code,
        exited,
        error: (exit_code != 0).then(|| status.clone()),
        status,
        ..Default::default()
    }
}

impl Drop for Child {
    fn drop(&mut self) {
        // A live child is never signalled by daemon shutdown. Before publication,
        // cancellation must instead roll back the private child completely.
        if self.live && !self.reaped {
            if let Some(lifecycle) = &self.lifecycle {
                if lifecycle.is_ready() {
                    lifecycle.fail_with(crate::server::EnvdFailure::invariant("process wait"));
                }
            }
        }
        if !self.live && !self.reaped {
            unsafe {
                kill_reap(self.pid as i32);
            }
        }
    }
}

unsafe fn kill_reap(pid: i32) {
    libc::kill(pid, libc::SIGKILL);
    while libc::waitpid(pid, std::ptr::null_mut(), 0) < 0 {
        if io::Error::last_os_error().raw_os_error() != Some(libc::EINTR) {
            break;
        }
    }
}

pub(crate) struct Spawn {
    pub executable: CString,
    pub argv: Vec<CString>,
    pub environment: Vec<CString>,
    pub cwd: CString,
    pub user: ProcessUser,
    pub stdin: Option<OwnedFd>,
    pub pty: bool,
    pub stdout: OwnedFd,
    pub stderr: OwnedFd,
    pub cgroup: std::sync::Arc<crate::cgroup::ProcessCgroup>,
}

impl Spawn {
    pub fn begin(self) -> Result<(Child, tokio::net::UnixStream), DomainError> {
        let cgroup = above_stdio(
            self.cgroup
                .file
                .try_clone()
                .map_err(|error| setup_error(error, false))?
                .into(),
        )
        .map_err(|error| setup_error(error, false))?;
        let (parent, child) = UnixStream::pair().map_err(|error| setup_error(error, false))?;
        let parent = UnixStream::from(
            above_stdio(parent.into()).map_err(|error| setup_error(error, false))?,
        );
        let child =
            UnixStream::from(above_stdio(child.into()).map_err(|error| setup_error(error, false))?);
        let null = File::options()
            .read(true)
            .write(true)
            .open("/dev/null")
            .map_err(|error| setup_error(error, false))?;
        let null = File::from(above_stdio(null.into()).map_err(|error| setup_error(error, false))?);
        let argv: Vec<_> = self
            .argv
            .iter()
            .map(|arg| arg.as_ptr())
            .chain(std::iter::once(std::ptr::null()))
            .collect();
        let environment: Vec<_> = self
            .environment
            .iter()
            .map(|var| var.as_ptr())
            .chain(std::iter::once(std::ptr::null()))
            .collect();
        // SAFETY: child_exec never allocates, locks, panics, returns, or drops
        // parent-owned Rust values. Every pointer and descriptor remains valid
        // in the child's private memory until execve/_exit.
        let pid = unsafe { libc::fork() };
        if pid == 0 {
            unsafe {
                child_exec(
                    &self,
                    &argv,
                    &environment,
                    child.as_raw_fd(),
                    parent.as_raw_fd(),
                    null.as_raw_fd(),
                    cgroup.as_raw_fd(),
                );
            }
        }
        if pid < 0 {
            return Err(setup_error(io::Error::last_os_error(), false));
        }
        drop(child);
        let fd = unsafe { libc::syscall(libc::SYS_pidfd_open, pid, 0) };
        if fd < 0 {
            let error = setup_error(io::Error::last_os_error(), false);
            unsafe {
                kill_reap(pid);
            }
            return Err(error);
        }
        let owned = unsafe { OwnedFd::from_raw_fd(fd as i32) };
        let pidfd = match AsyncFd::new(owned) {
            Ok(fd) => fd,
            Err(error) => {
                unsafe {
                    kill_reap(pid);
                }
                return Err(setup_error(error, false));
            }
        };
        let guard = Child {
            pid: pid as u32,
            pidfd: ProcessFd::new(pidfd, None),
            reaped: false,
            live: false,
            lifecycle: None,
            _cgroup: self.cgroup,
        };
        parent
            .set_nonblocking(true)
            .map_err(|error| setup_error(error, false))?;
        let channel =
            tokio::net::UnixStream::from_std(parent).map_err(|error| setup_error(error, false))?;
        Ok((guard, channel))
    }
}

pub(crate) async fn exec_result(channel: &mut tokio::net::UnixStream) -> Result<(), DomainError> {
    let mut report = [0_u8; 8];
    let mut used = 0;
    let mut setup_complete = false;
    loop {
        let count = channel
            .read(&mut report[used..])
            .await
            .map_err(|_| DomainError::Internal)?;
        if count == 0 {
            return if used == 0 && setup_complete {
                Ok(())
            } else {
                Err(DomainError::Internal)
            };
        }
        used += count;
        if used == report.len() {
            let errno =
                i32::from_ne_bytes(report[..4].try_into().map_err(|_| DomainError::Internal)?);
            let caller =
                i32::from_ne_bytes(report[4..].try_into().map_err(|_| DomainError::Internal)?) != 0;
            if errno == 0 && !setup_complete {
                setup_complete = true;
                used = 0;
                continue;
            }
            return Err(setup_error(io::Error::from_raw_os_error(errno), caller));
        }
    }
}

unsafe fn child_exec(
    spawn: &Spawn,
    argv: &[*const libc::c_char],
    environment: &[*const libc::c_char],
    report: i32,
    parent: i32,
    null: i32,
    cgroup: i32,
) -> ! {
    libc::close(parent);
    for (target, source) in [
        (0, spawn.stdin.as_ref().map_or(null, AsRawFd::as_raw_fd)),
        (1, spawn.stdout.as_raw_fd()),
        (2, spawn.stderr.as_raw_fd()),
    ] {
        if libc::dup2(source, target) < 0 {
            child_error(report, false);
        }
    }
    if spawn.pty && (libc::setsid() < 0 || libc::ioctl(0, libc::TIOCSCTTY, 0) < 0) {
        child_error(report, false);
    }
    // Establish resource policy before credentials or any user executable.
    if libc::write(cgroup, b"0".as_ptr().cast(), 1) != 1 {
        child_error(report, false);
    }
    if libc::setpriority(libc::PRIO_PROCESS, 0, 0) < 0 {
        child_error(report, false);
    }
    let oom = libc::open(
        c"/proc/self/oom_score_adj".as_ptr(),
        libc::O_WRONLY | libc::O_CLOEXEC,
    );
    if oom < 0 {
        child_error(report, false);
    }
    if libc::write(oom, b"100".as_ptr().cast(), 3) != 3 {
        child_error(report, false);
    }
    libc::close(oom);
    if libc::setgroups(spawn.user.groups.len(), spawn.user.groups.as_ptr()) < 0 {
        child_error(report, false);
    }
    if libc::setgid(spawn.user.gid) < 0 {
        child_error(report, false);
    }
    if libc::setuid(spawn.user.uid) < 0 {
        child_error(report, false);
    }
    if libc::chdir(spawn.cwd.as_ptr()) < 0 {
        child_error(report, true);
    }
    for signal in [libc::SIGPIPE, libc::SIGINT, libc::SIGTERM, libc::SIGHUP] {
        if libc::signal(signal, libc::SIG_DFL) == libc::SIG_ERR {
            child_error(report, false);
        }
    }
    let mut signals: libc::sigset_t = std::mem::zeroed();
    libc::sigemptyset(&mut signals);
    if libc::sigprocmask(libc::SIG_SETMASK, &signals, std::ptr::null_mut()) < 0 {
        child_error(report, false);
    }
    child_report(report, [0, 0]);
    libc::execve(
        spawn.executable.as_ptr(),
        argv.as_ptr(),
        environment.as_ptr(),
    );
    child_error(report, true);
}

unsafe fn child_error(report: i32, caller: bool) -> ! {
    let error = [*libc::__errno_location(), i32::from(caller)];
    child_report(report, error);
    libc::_exit(126);
}

unsafe fn child_report(report: i32, message: [i32; 2]) {
    let mut written = 0;
    while written < std::mem::size_of_val(&message) {
        let count = libc::write(
            report,
            message.as_ptr().cast::<u8>().add(written).cast(),
            std::mem::size_of_val(&message) - written,
        );
        if count > 0 {
            written += count as usize;
        } else if count < 0 && *libc::__errno_location() == libc::EINTR {
            continue;
        } else {
            libc::_exit(126);
        }
    }
}

pub(crate) fn above_stdio(fd: OwnedFd) -> io::Result<OwnedFd> {
    if fd.as_raw_fd() > 2 {
        return Ok(fd);
    }
    let duplicate = unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_DUPFD_CLOEXEC, 3) };
    if duplicate < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(unsafe { OwnedFd::from_raw_fd(duplicate) })
}

pub(crate) fn setup_error(error: io::Error, caller: bool) -> DomainError {
    match error.raw_os_error() {
        Some(
            libc::EAGAIN | libc::EMFILE | libc::ENFILE | libc::ENOMEM | libc::ENOSPC | libc::EDQUOT,
        ) => DomainError::ResourceExhausted("process setup resources exhausted".into()),
        Some(
            libc::ENOENT
            | libc::ENOTDIR
            | libc::EACCES
            | libc::ENOEXEC
            | libc::E2BIG
            | libc::ELOOP
            | libc::ENAMETOOLONG
            | libc::EINVAL,
        ) if caller => {
            DomainError::InvalidArgument("invalid executable or working directory".into())
        }
        _ => DomainError::Internal,
    }
}

/// One nonblocking master is shared by input, resize and the output reader.
/// The last owner closes it after wait-authoritative bounded drain.
pub(crate) struct Pty(AsyncFd<OwnedFd>);
impl Pty {
    pub(crate) fn self_check(&self) -> bool {
        // Query only: neither read the terminal nor alter its flags or size.
        let fd = self.0.as_raw_fd();
        let flags = unsafe { libc::fcntl(fd, libc::F_GETFL) };
        let descriptor_flags = unsafe { libc::fcntl(fd, libc::F_GETFD) };
        flags >= 0
            && flags & libc::O_NONBLOCK != 0
            && flags & libc::O_ACCMODE == libc::O_RDWR
            && descriptor_flags >= 0
            && descriptor_flags & libc::FD_CLOEXEC != 0
    }

    #[cfg(test)]
    pub(crate) fn set_blocking_for_test(&self, blocking: bool) {
        let fd = self.0.as_raw_fd();
        let flags = unsafe { libc::fcntl(fd, libc::F_GETFL) };
        assert!(flags >= 0);
        let flags = if blocking {
            flags & !libc::O_NONBLOCK
        } else {
            flags | libc::O_NONBLOCK
        };
        assert_eq!(unsafe { libc::fcntl(fd, libc::F_SETFL, flags) }, 0);
    }

    pub fn open(
        size: &crate::proto::process::pty::Size,
    ) -> Result<(std::sync::Arc<Self>, OwnedFd), DomainError> {
        // Create both descriptors with CLOEXEC atomically: another concurrent
        // Start may fork between PTY creation and any later fcntl call.
        let master = unsafe {
            libc::open(
                c"/dev/ptmx".as_ptr(),
                libc::O_RDWR | libc::O_NOCTTY | libc::O_CLOEXEC | libc::O_NONBLOCK,
            )
        };
        if master < 0 {
            return Err(setup_error(io::Error::last_os_error(), false));
        }
        let master = unsafe { OwnedFd::from_raw_fd(master) };
        let unlock: libc::c_int = 0;
        if unsafe { libc::ioctl(master.as_raw_fd(), libc::TIOCSPTLCK, &unlock) } < 0 {
            return Err(setup_error(io::Error::last_os_error(), false));
        }
        let slave = unsafe {
            libc::ioctl(
                master.as_raw_fd(),
                libc::TIOCGPTPEER,
                libc::O_RDWR | libc::O_NOCTTY | libc::O_CLOEXEC,
            )
        };
        if slave < 0 {
            return Err(setup_error(io::Error::last_os_error(), false));
        }
        let slave = unsafe { OwnedFd::from_raw_fd(slave) };
        let size = winsize(size);
        if unsafe { libc::ioctl(master.as_raw_fd(), libc::TIOCSWINSZ, &size) } < 0 {
            return Err(setup_error(io::Error::last_os_error(), false));
        }
        let master = AsyncFd::new(above_stdio(master).map_err(|error| setup_error(error, false))?)
            .map_err(|error| setup_error(error, false))?;
        Ok((
            std::sync::Arc::new(Self(master)),
            above_stdio(slave).map_err(|error| setup_error(error, false))?,
        ))
    }
    pub async fn read(&self, bytes: &mut [u8]) -> io::Result<usize> {
        loop {
            let mut ready = self.0.readable().await?;
            match ready.try_io(|fd| {
                let count =
                    unsafe { libc::read(fd.as_raw_fd(), bytes.as_mut_ptr().cast(), bytes.len()) };
                if count >= 0 {
                    Ok(count as usize)
                } else {
                    let error = io::Error::last_os_error();
                    // Linux PTY EOF is EIO when the final slave closes.
                    if error.raw_os_error() == Some(libc::EIO) {
                        Ok(0)
                    } else {
                        Err(error)
                    }
                }
            }) {
                Ok(Err(error)) if error.kind() == io::ErrorKind::Interrupted => continue,
                Ok(result) => return result,
                Err(_) => continue,
            }
        }
    }
    pub async fn write(&self, bytes: &[u8]) -> io::Result<usize> {
        loop {
            let mut ready = self.0.writable().await?;
            match ready.try_io(|fd| {
                let count =
                    unsafe { libc::write(fd.as_raw_fd(), bytes.as_ptr().cast(), bytes.len()) };
                if count < 0 {
                    Err(io::Error::last_os_error())
                } else {
                    Ok(count as usize)
                }
            }) {
                Ok(Err(error)) if error.kind() == io::ErrorKind::Interrupted => continue,
                Ok(result) => return result,
                Err(_) => continue,
            }
        }
    }
    pub fn resize(&self, size: &crate::proto::process::pty::Size) -> Result<(), DomainError> {
        let size = winsize(size);
        if unsafe { libc::ioctl(self.0.as_raw_fd(), libc::TIOCSWINSZ, &size) } < 0 {
            return Err(DomainError::Internal);
        }
        Ok(())
    }
}
fn winsize(size: &crate::proto::process::pty::Size) -> libc::winsize {
    libc::winsize {
        ws_row: size.rows as u16,
        ws_col: size.cols as u16,
        ws_xpixel: 0,
        ws_ypixel: 0,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::AsyncWriteExt;

    #[tokio::test]
    async fn exec_handshake_requires_complete_setup_and_preserves_errors() {
        for (wire, expected) in [
            (vec![], Err(DomainError::Internal)),
            (vec![0; 7], Err(DomainError::Internal)),
            (vec![0; 8], Ok(())),
            (
                [0_i32, 0, libc::ENOENT, 1]
                    .into_iter()
                    .flat_map(i32::to_ne_bytes)
                    .collect(),
                Err(DomainError::InvalidArgument(
                    "invalid executable or working directory".into(),
                )),
            ),
        ] {
            let (mut reader, mut writer) = tokio::net::UnixStream::pair().unwrap();
            writer.write_all(&wire).await.unwrap();
            drop(writer);
            assert_eq!(exec_result(&mut reader).await, expected);
        }
    }

    #[test]
    fn setup_error_classification_distinguishes_exhaustion_and_runtime_failure() {
        for errno in [
            libc::EAGAIN,
            libc::EMFILE,
            libc::ENFILE,
            libc::ENOMEM,
            libc::ENOSPC,
            libc::EDQUOT,
        ] {
            assert!(matches!(
                setup_error(io::Error::from_raw_os_error(errno), false),
                DomainError::ResourceExhausted(_)
            ));
        }
        assert_eq!(
            setup_error(io::Error::from_raw_os_error(libc::EIO), false),
            DomainError::Internal
        );
        assert!(matches!(
            setup_error(io::Error::from_raw_os_error(libc::ENOENT), true),
            DomainError::InvalidArgument(_)
        ));
    }
}

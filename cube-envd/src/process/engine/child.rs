// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Child-process primitives: how a command becomes a real process, and how its
//! exit status comes back.
//!
//! Contract: `clone(2)` with `CLONE_VM|CLONE_VFORK|SIGCHLD` — the primitive
//! Go's `os/exec` and libc's `posix_spawn` use internally — plus the `execvp`
//! semantics `std::process` provides on the fork path:
//!
//! - `SIGPIPE` is reset to `SIG_DFL` for the child, as `std::process` does;
//! - a bare program name is resolved against a caller-supplied `PATH`, with an
//!   empty element meaning the current directory, and `EACCES` winning over
//!   `ENOENT` when nothing succeeds. std repoints `environ` for this; a child
//!   that shares this address space must not, so the candidate list is built
//!   here, on the parent side, before the clone.
//!
//! Deliberate deviations:
//!
//! - The child runs on a private stack (`ChildStack`) and enters at
//!   [`child_main`]. It must: a null `child_stack` puts it on the daemon's
//!   stack, where its first call pushes a return address over the one the
//!   suspended parent left for the clone wrapper, and the parent resumes into
//!   it.
//! - A close-on-exec report pipe carries a failing `errno` back to the parent,
//!   so a setup failure and an `execve` failure both surface as spawn errors.
//! - The child is reaped by pidfd, falling back to a blocking `waitpid` where
//!   `pidfd_open` is unavailable. tokio's SIGCHLD reaper only waits on pids it
//!   created, so this pid cannot be reaped out from under us.
//!
//! Non-goals: this module knows nothing about envd's spawn policy. Credentials,
//! process group/session, cgroup placement and `chdir` are the caller's
//! [`ChildSpec::before_exec`], which must stay allocation-free — the child
//! shares this address space until `execve`.

use std::collections::HashMap;
use std::ffi::CString;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::os::unix::process::ExitStatusExt;
use std::process::{ChildStderr, ChildStdin, ChildStdout, ExitStatus};

/// How to start one child.
pub(super) struct ChildSpec<'a> {
    /// The program as the caller named it; `argv[0]` keeps this spelling even
    /// when a `PATH` candidate is what actually gets executed.
    pub(super) cmd: &'a str,
    pub(super) args: &'a [String],
    /// The child's complete environment (`envp`).
    pub(super) env: &'a HashMap<String, String>,
    /// `PATH` for resolving a bare `cmd`, already resolved by the caller.
    pub(super) path: &'a str,
    pub(super) stdin_enabled: bool,
    /// Runs in the child before `execve`: cgroup placement, session and process
    /// group, credential drop, `chdir`.
    pub(super) before_exec: &'a mut dyn FnMut() -> std::io::Result<()>,
}

/// Why [`spawn_without_fork`] failed. The caller may only silently fall back to
/// `fork` when the fork-free machinery itself is unavailable (a rejected clone
/// or an unmappable child stack); a failure that came back through the child's
/// report pipe is a real command error and must be surfaced (falling back would
/// run the command twice).
pub(super) enum SpawnFailure {
    Unsupported(std::io::Error),
    Child(std::io::Error),
}

/// A child created by [`spawn_without_fork`].
pub(super) struct RawChild {
    pub(super) pid: u32,
    /// Readable exactly when the child exits; `None` if the kernel or sandbox
    /// rejected `pidfd_open`, in which case reaping falls back to a blocking
    /// thread.
    pub(super) pidfd: Option<OwnedFd>,
    pub(super) stdin: Option<tokio::process::ChildStdin>,
    pub(super) stdout: Option<tokio::process::ChildStdout>,
    pub(super) stderr: Option<tokio::process::ChildStderr>,
}

/// A child whose exit status is collected by us rather than by tokio.
pub(super) enum ChildHandle {
    Command(tokio::process::Child),
    /// Fork-free spawn. tokio's SIGCHLD reaper only ever waits on pids it
    /// created (`orphan::drain_orphan_queue` walks its own queue), so this pid
    /// cannot be reaped out from under us.
    Raw {
        pid: u32,
        pidfd: Option<OwnedFd>,
    },
}

impl ChildHandle {
    pub(super) fn id(&self) -> Option<u32> {
        match self {
            ChildHandle::Command(child) => child.id(),
            ChildHandle::Raw { pid, .. } => Some(*pid),
        }
    }

    pub(super) async fn wait(&mut self) -> std::io::Result<ExitStatus> {
        match self {
            ChildHandle::Command(child) => child.wait().await,
            ChildHandle::Raw { pid, pidfd } => {
                let pid = *pid;
                if let Some(fd) = pidfd.take() {
                    // A pidfd polls readable exactly when the child exits, so
                    // the waitpid below cannot block.
                    if let Ok(ready) = tokio::io::unix::AsyncFd::new(fd) {
                        // The guard is dropped immediately: readiness is only
                        // used as "the child exited", never re-polled.
                        let _ = ready.readable().await?;
                        return reap(pid);
                    }
                }
                tokio::task::spawn_blocking(move || reap(pid))
                    .await
                    .map_err(std::io::Error::other)?
            }
        }
    }
}

/// Collect a child that has already been signalled as exited (or is about to).
fn reap(pid: u32) -> std::io::Result<ExitStatus> {
    let mut status = 0;
    loop {
        // SAFETY: waitpid with a pid we own and a valid out-parameter.
        let ret = unsafe { libc::waitpid(pid as libc::pid_t, &mut status, 0) };
        if ret == pid as libc::pid_t {
            return Ok(ExitStatus::from_raw(status));
        }
        if ret < 0 {
            let e = std::io::Error::last_os_error();
            if e.kind() == std::io::ErrorKind::Interrupted {
                continue;
            }
            return Err(e);
        }
    }
}

/// A pidfd for `pid`, or `None` when the kernel or sandbox rejects
/// `pidfd_open` (pre-5.3 kernels, restrictive seccomp). The fd is close-on-exec
/// by default.
fn pidfd_open(pid: u32) -> Option<OwnedFd> {
    // SAFETY: pidfd_open(pid, 0) returns a fresh fd or -1.
    let fd = unsafe { libc::syscall(libc::SYS_pidfd_open, pid as libc::c_long, 0 as libc::c_long) };
    if fd < 0 {
        None
    } else {
        // SAFETY: fd is a fresh descriptor owned here.
        Some(unsafe { OwnedFd::from_raw_fd(fd as RawFd) })
    }
}

/// Both ends of a close-on-exec pipe.
fn pipe_cloexec() -> std::io::Result<(OwnedFd, OwnedFd)> {
    let mut fds = [0 as libc::c_int; 2];
    // SAFETY: pipe2 fills the two-element array.
    if unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_CLOEXEC) } != 0 {
        return Err(std::io::Error::last_os_error());
    }
    // SAFETY: both descriptors are fresh and owned here.
    unsafe { Ok((OwnedFd::from_raw_fd(fds[0]), OwnedFd::from_raw_fd(fds[1]))) }
}

/// `/dev/null` for a child whose stdin is disabled, standing in for
/// `Stdio::null()`.
fn open_devnull() -> std::io::Result<OwnedFd> {
    let path = b"/dev/null\0";
    // SAFETY: a NUL-terminated literal path and a flags argument.
    let fd = unsafe { libc::open(path.as_ptr().cast(), libc::O_RDONLY | libc::O_CLOEXEC) };
    if fd < 0 {
        return Err(std::io::Error::last_os_error());
    }
    // SAFETY: a fresh descriptor owned here.
    Ok(unsafe { OwnedFd::from_raw_fd(fd) })
}

/// Resolve `cmd` the way `execvp` would, but entirely on the parent side: the
/// child gets a ready-made candidate list it can just try in order.
///
/// `execvp` reads the global `environ` to find `PATH`, and a
/// shared-address-space child must not repoint that: after a successful `execve`
/// the parent would resume with a pointer into this function's stack.
fn exec_candidates(cmd: &str, path: &str) -> std::io::Result<Vec<CString>> {
    let nul = |what: &str| {
        std::io::Error::new(
            std::io::ErrorKind::InvalidInput,
            format!("{what} contains NUL"),
        )
    };
    if cmd.contains('/') {
        return Ok(vec![
            CString::new(cmd.as_bytes()).map_err(|_| nul("command"))?
        ]);
    }
    let mut candidates = Vec::new();
    for dir in path.split(':') {
        // An empty PATH element means the current directory, as in execvp.
        let dir = if dir.is_empty() { "." } else { dir };
        candidates.push(CString::new(format!("{dir}/{cmd}")).map_err(|_| nul("command"))?);
    }
    Ok(candidates)
}

/// Point fds 0/1/2 at the pipe ends. Runs in the child: `dup2` clears
/// close-on-exec on the copies, which is what lets them survive `execve`.
fn install_stdio(stdin_fd: RawFd, stdout_fd: RawFd, stderr_fd: RawFd) -> std::io::Result<()> {
    for (from, to) in [
        (stdin_fd, libc::STDIN_FILENO),
        (stdout_fd, libc::STDOUT_FILENO),
        (stderr_fd, libc::STDERR_FILENO),
    ] {
        // SAFETY: dup2 with descriptors we own; `to` is a standard fd.
        if unsafe { libc::dup2(from, to) } < 0 {
            return Err(std::io::Error::last_os_error());
        }
    }
    Ok(())
}

/// Hand the failing errno to the parent over the close-on-exec report pipe.
///
/// # Safety
/// Runs in the shared-address-space child: `fd` must be the report pipe's write
/// end and the call must not allocate.
unsafe fn report(fd: RawFd, code: i32) {
    let bytes = code.to_ne_bytes();
    let mut off = 0;
    while off < bytes.len() {
        // SAFETY: writing our own stack buffer to a descriptor we own.
        let n = libc::write(fd, bytes[off..].as_ptr().cast(), bytes.len() - off);
        if n <= 0 {
            break;
        }
        off += n as usize;
    }
}

/// Everything the child of [`libc::clone`] needs, handed over by pointer: the
/// child shares this address space until it execs, so a reference is enough and
/// nothing has to be copied.
struct ChildPlan<'a> {
    candidates: &'a [CString],
    argv: &'a [*const libc::c_char],
    envp: &'a [*const libc::c_char],
    before_exec: &'a mut dyn FnMut() -> std::io::Result<()>,
    stdin_fd: RawFd,
    stdout_fd: RawFd,
    stderr_fd: RawFd,
    report_fd: RawFd,
}

/// A private stack for one child of [`libc::clone`].
///
/// The child shares the daemon's address space until `execve`, so it must not
/// run on the daemon's stack: its very first call would push a return address
/// over the one the suspended parent left for the clone wrapper, and the parent
/// would resume into that garbage.
struct ChildStack {
    base: *mut libc::c_void,
    len: usize,
}

impl ChildStack {
    /// Enough for the `dup2`/`setgroups`/`setuid`/`chdir`/`execve` wrappers;
    /// the pages are only committed as the child touches them.
    const SIZE: usize = 64 * 1024;

    /// One inaccessible page below the stack. The child shares the daemon's
    /// address space until `execve`, so an overflow would otherwise write into
    /// whatever mapping happens to sit below — most likely the daemon's own
    /// memory, which is silent corruption rather than a clean failure. With the
    /// guard it is a SIGSEGV in the child, which the parent already treats as a
    /// spawn that died.
    const GUARD: usize = 4096;

    fn new() -> std::io::Result<Self> {
        let len = Self::GUARD + Self::SIZE;
        // Map the whole range inaccessible and then open only the stack part, so
        // the guard page is never writable, not even transiently.
        // SAFETY: an anonymous private mapping owned by this value.
        let base = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                len,
                libc::PROT_NONE,
                libc::MAP_PRIVATE | libc::MAP_ANONYMOUS | libc::MAP_STACK,
                -1,
                0,
            )
        };
        if base == libc::MAP_FAILED {
            return Err(std::io::Error::last_os_error());
        }
        // SAFETY: the second page of our own mapping, `SIZE` bytes of it.
        let stack = unsafe { base.cast::<u8>().add(Self::GUARD).cast() };
        if unsafe { libc::mprotect(stack, Self::SIZE, libc::PROT_READ | libc::PROT_WRITE) } != 0 {
            let err = std::io::Error::last_os_error();
            // SAFETY: the mapping above, which nothing else references yet.
            unsafe { libc::munmap(base, len) };
            return Err(err);
        }
        Ok(Self { base, len })
    }

    /// Stacks grow down; the entry stack pointer is one past the mapping, which
    /// is 16-byte aligned because the mapping and its length are page-sized.
    fn top(&self) -> *mut libc::c_void {
        // SAFETY: `base + len` is one past the mapping.
        unsafe { self.base.cast::<u8>().add(self.len).cast() }
    }
}

impl Drop for ChildStack {
    fn drop(&mut self) {
        // SAFETY: the mapping came from `mmap`, and the child that shared it has
        // already exec'd (new address space) or exited, because `CLONE_VFORK`
        // keeps the parent parked until then.
        unsafe { libc::munmap(self.base, self.len) };
    }
}

/// Entry point for the child of [`libc::clone`], running on its own stack.
///
/// It must not allocate and must not unwind — until `execve` it shares the
/// daemon's address space — and it never returns: any failure is reported to
/// the parent over the report pipe and this process exits.
extern "C" fn child_main(ctx: *mut libc::c_void) -> libc::c_int {
    // SAFETY: the parent hands us a live `ChildPlan` and stays parked inside
    // `clone` until we exec or exit, so it outlives us.
    let plan = unsafe { &mut *(ctx as *mut ChildPlan<'_>) };

    let failure = install_stdio(plan.stdin_fd, plan.stdout_fd, plan.stderr_fd)
        .and_then(|()| {
            // std::process resets SIGPIPE for the child; the daemon ignores it.
            // SAFETY: a plain `sigaction` on a fixed signal number.
            unsafe { libc::signal(libc::SIGPIPE, libc::SIG_DFL) };
            (plan.before_exec)()
        })
        .err()
        // Every setup step here is a syscall, so an error normally carries an
        // errno. One that does not — or that carries zero — must still stop the
        // child: falling through to `execve` would run the command with the
        // credential drop, process group, cgroup placement or `chdir` only
        // half-applied, and silently. `EIO` is the honest report for that.
        .map(|e| {
            e.raw_os_error()
                .filter(|code| *code != 0)
                .unwrap_or(libc::EIO)
        });
    if let Some(code) = failure {
        // SAFETY: the report pipe's write end belongs to this child.
        unsafe { report(plan.report_fd, code) };
        unsafe { libc::_exit(127) };
    }

    // execvp semantics: try every candidate, and let EACCES (a real file we may
    // not execute) win over ENOENT when nothing succeeded.
    let mut denied = 0;
    for candidate in plan.candidates {
        // SAFETY: both lists were built by the parent and are still live.
        unsafe { libc::execve(candidate.as_ptr(), plan.argv.as_ptr(), plan.envp.as_ptr()) };
        let code = std::io::Error::last_os_error()
            .raw_os_error()
            .unwrap_or(libc::ENOENT);
        if code == libc::EACCES {
            denied = code;
        } else if code != libc::ENOENT && code != libc::ENOTDIR {
            // SAFETY: as above.
            unsafe { report(plan.report_fd, code) };
            unsafe { libc::_exit(127) };
        }
    }
    // SAFETY: as above.
    unsafe {
        report(
            plan.report_fd,
            if denied != 0 { denied } else { libc::ENOENT },
        )
    };
    unsafe { libc::_exit(127) }
}

/// Create a child without forking: `clone(CLONE_VM|CLONE_VFORK|SIGCHLD)`, the
/// caller's `before_exec` in the child, then `execve`. The parent's page tables
/// are never written, so it takes no copy-on-write fault for the pages it
/// rewrites afterwards — which is the whole reason this exists.
///
/// Everything the child will touch is built here, on the parent side: until it
/// execs it shares this address space and must not allocate.
pub(super) fn spawn_without_fork(spec: ChildSpec<'_>) -> Result<RawChild, SpawnFailure> {
    let ChildSpec {
        cmd,
        args,
        env,
        path,
        stdin_enabled,
        before_exec,
    } = spec;
    let nul = |what: &str| {
        SpawnFailure::Child(std::io::Error::new(
            std::io::ErrorKind::InvalidInput,
            format!("{what} contains NUL"),
        ))
    };

    // Everything the child touches is built here, on the parent side: the child
    // shares this address space and must not allocate.
    let candidates = exec_candidates(cmd, path).map_err(SpawnFailure::Child)?;
    let mut argv: Vec<CString> = Vec::with_capacity(args.len() + 1);
    argv.push(CString::new(cmd.as_bytes()).map_err(|_| nul("command"))?);
    for arg in args {
        argv.push(CString::new(arg.as_bytes()).map_err(|_| nul("argument"))?);
    }
    let mut argv_ptrs: Vec<*const libc::c_char> = argv.iter().map(|c| c.as_ptr()).collect();
    argv_ptrs.push(std::ptr::null());

    let mut envp: Vec<CString> = Vec::with_capacity(env.len());
    for (key, value) in env {
        envp.push(CString::new(format!("{key}={value}")).map_err(|_| nul("environment"))?);
    }
    let mut envp_ptrs: Vec<*const libc::c_char> = envp.iter().map(|c| c.as_ptr()).collect();
    envp_ptrs.push(std::ptr::null());

    // The child's ends stay open until the clone so it inherits them; each
    // carries close-on-exec, so execve drops them there. The parent's ends are
    // adopted into tokio before the clone, so a registration failure cannot
    // strand a running child.
    let (stdin_child, stdin) = if stdin_enabled {
        let (read, write) = pipe_cloexec().map_err(SpawnFailure::Child)?;
        let writer = tokio::process::ChildStdin::from_std(ChildStdin::from(write))
            .map_err(SpawnFailure::Child)?;
        (Some(read), Some(writer))
    } else {
        (Some(open_devnull().map_err(SpawnFailure::Child)?), None)
    };
    let (stdout_read, stdout_write) = pipe_cloexec().map_err(SpawnFailure::Child)?;
    let (stderr_read, stderr_write) = pipe_cloexec().map_err(SpawnFailure::Child)?;
    let (report_read, report_write) = pipe_cloexec().map_err(SpawnFailure::Child)?;
    let stdout = tokio::process::ChildStdout::from_std(ChildStdout::from(stdout_read))
        .map_err(SpawnFailure::Child)?;
    let stderr = tokio::process::ChildStderr::from_std(ChildStderr::from(stderr_read))
        .map_err(SpawnFailure::Child)?;

    // A private stack for the child: see `ChildStack` for why the daemon's own
    // stack cannot be shared. Failing to map it is a resource problem, not a
    // command error, so it degrades to the fork path like a rejected clone.
    let stack = ChildStack::new().map_err(SpawnFailure::Unsupported)?;
    let mut plan = ChildPlan {
        candidates: &candidates,
        argv: &argv_ptrs,
        envp: &envp_ptrs,
        before_exec,
        stdin_fd: stdin_child.as_ref().map_or(-1, |fd| fd.as_raw_fd()),
        stdout_fd: stdout_write.as_raw_fd(),
        stderr_fd: stderr_write.as_raw_fd(),
        report_fd: report_write.as_raw_fd(),
    };

    // SAFETY: CLONE_VM shares this address space with the child and CLONE_VFORK
    // parks us until the child execs or exits, so `plan` and everything it
    // points at stay live for the child's whole life. The child runs
    // `child_main` on its own stack and never returns.
    let pid = unsafe {
        libc::clone(
            child_main,
            stack.top(),
            (libc::CLONE_VM | libc::CLONE_VFORK | libc::SIGCHLD) as libc::c_int,
            (&mut plan as *mut ChildPlan<'_>).cast(),
        )
    };
    if pid < 0 {
        return Err(SpawnFailure::Unsupported(std::io::Error::last_os_error()));
    }
    // The child has exec'd or exited by now, so its stack can go.
    drop(stack);

    // Parent. CLONE_VFORK resumed us only after the child exec'd or died, so
    // closing the child's ends and draining the report cannot block.
    drop(stdin_child);
    drop(stdout_write);
    drop(stderr_write);
    drop(report_write);
    let mut errno_bytes = [0u8; 4];
    // SAFETY: report_read owns one end of the pipe; the other end is either
    // closed by execve (EOF) or carries exactly one errno.
    let read = loop {
        let n = unsafe {
            libc::read(
                report_read.as_raw_fd(),
                errno_bytes.as_mut_ptr().cast(),
                errno_bytes.len(),
            )
        };
        if n < 0 && std::io::Error::last_os_error().kind() == std::io::ErrorKind::Interrupted {
            continue;
        }
        break n;
    };
    if read != 0 {
        let _ = reap(pid as u32);
        // Zero is EOF: the child exec'd, and the write end closed with it. Any
        // other length is the errno, and a partial one cannot happen — the
        // child's four-byte pipe write is atomic and completes before
        // CLONE_VFORK resumes us — so reading it as an errno would be parsing
        // whatever arrived. Report a broken report instead of proceeding.
        let errno = if read == 4 {
            i32::from_ne_bytes(errno_bytes)
        } else {
            libc::EIO
        };
        return Err(SpawnFailure::Child(std::io::Error::from_raw_os_error(
            errno,
        )));
    }

    let pid = pid as u32;
    Ok(RawChild {
        pid,
        pidfd: pidfd_open(pid),
        stdin,
        stdout: Some(stdout),
        stderr: Some(stderr),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The fork path delegates program resolution to libc's `execvp`; this path
    /// resolves it itself, so the semantics need pinning: a bare name searches
    /// the caller's PATH, anything with a slash is used as-is, and an empty PATH
    /// element means the current directory.
    #[test]
    fn exec_candidates_matches_execvp_resolution() {
        let names = |cmd: &str, path: &str| -> Vec<String> {
            exec_candidates(cmd, path)
                .unwrap()
                .iter()
                .map(|c| c.to_str().unwrap().to_string())
                .collect()
        };
        assert_eq!(names("sh", "/usr/bin:/bin"), ["/usr/bin/sh", "/bin/sh"]);
        assert_eq!(names("./sh", "/usr/bin:/bin"), ["./sh"]);
        assert_eq!(names("/bin/sh", "/usr/bin:/bin"), ["/bin/sh"]);
        // An empty PATH element means the current directory, as in execvp.
        assert_eq!(names("sh", ":/bin"), ["./sh", "/bin/sh"]);
        // A caller that hands over an empty PATH must still get one candidate,
        // or the spawn would have no errno to report.
        assert_eq!(names("sh", ""), ["./sh"]);
    }

    /// The whole point of the report pipe: a setup step that fails must stop the
    /// child, including one whose error carries no errno. Otherwise it would
    /// fall through to `execve` with the credential drop, process group, cgroup
    /// placement or `chdir` only partly applied, and say nothing.
    #[tokio::test]
    async fn a_setup_failure_without_an_errno_still_stops_the_child() {
        let env = HashMap::new();
        let mut before_exec = || {
            Err(std::io::Error::new(
                std::io::ErrorKind::InvalidInput,
                "no errno",
            ))
        };
        let failure = match spawn_without_fork(ChildSpec {
            cmd: "/bin/true",
            args: &[],
            env: &env,
            path: "/usr/bin:/bin",
            stdin_enabled: false,
            before_exec: &mut before_exec,
        }) {
            Ok(_) => panic!("a failing setup must fail the spawn"),
            Err(e) => e,
        };
        match failure {
            SpawnFailure::Child(e) => assert_eq!(e.raw_os_error(), Some(libc::EIO)),
            SpawnFailure::Unsupported(e) => panic!("unexpected: {e}"),
        }
    }

    /// The child shares the daemon's address space, so its stack has to be
    /// bounded by something the kernel will fault on. Without the guard an
    /// overflow writes into whatever mapping sits below — the daemon's own
    /// memory — instead of killing the child.
    #[test]
    fn child_stack_keeps_a_guard_page_below_it() {
        fn perms_of(maps: &str, addr: usize) -> Option<String> {
            for line in maps.lines() {
                let mut fields = line.split_whitespace();
                let Some((start, end)) = fields.next().and_then(|r| r.split_once('-')) else {
                    continue;
                };
                let (Ok(start), Ok(end)) = (
                    usize::from_str_radix(start, 16),
                    usize::from_str_radix(end, 16),
                ) else {
                    continue;
                };
                if (start..end).contains(&addr) {
                    return fields.next().map(str::to_string);
                }
            }
            None
        }

        let stack = ChildStack::new().unwrap();
        let maps = std::fs::read_to_string("/proc/self/maps").unwrap();
        let base = stack.base as usize;
        assert_eq!(
            perms_of(&maps, base).as_deref(),
            Some("---p"),
            "the page below the child stack must be inaccessible"
        );
        assert_eq!(
            perms_of(&maps, base + ChildStack::GUARD).as_deref(),
            Some("rw-p"),
            "the child stack itself must be writable"
        );
    }
}

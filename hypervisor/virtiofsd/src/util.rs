// Copyright 2022 Red Hat, Inc. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

use std::fs::{File, OpenOptions};
use std::io::{Error, Write};
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
use std::os::unix::io::{AsRawFd, FromRawFd};
use std::path::Path;
use std::{fs, io, process};

fn try_lock_file(file: &File) -> Result<(), Error> {
    // Safe because 'file' must exist and we check the return value.
    let file_fd = file.as_raw_fd();
    let ret = unsafe { libc::flock(file_fd, libc::LOCK_EX | libc::LOCK_NB) };
    if ret == -1 {
        return Err(Error::last_os_error());
    }
    Ok(())
}

pub fn write_pid_file(pid_file_name: &Path) -> Result<File, std::io::Error> {
    let mut pid_file = loop {
        let file = OpenOptions::new()
            .mode(libc::S_IRUSR | libc::S_IWUSR)
            .custom_flags(libc::O_CLOEXEC)
            .write(true)
            .create(true)
            .open(pid_file_name)?;

        try_lock_file(&file)?;

        // Let's make sure the file we locked still exists in the filesystem.
        let locked = file.metadata()?.ino();
        let current = match fs::metadata(pid_file_name) {
            Ok(stat) => stat.ino(),
            _ => continue, // the pid file got removed or some error happened, try again.
        };

        if locked == current {
            break file; // lock successfully acquired.
        }
        // the file changed, other process is racing with us, so try again.
    };

    let pid = format!("{}\n", process::id());
    pid_file.write_all(pid.as_bytes())?;

    Ok(pid_file)
}

unsafe fn pidfd_open(pid: libc::pid_t, flags: libc::c_uint) -> libc::c_int {
    libc::syscall(libc::SYS_pidfd_open, pid, flags) as libc::c_int
}

/// Helper function to create a process and sets the parent process
/// death signal SIGTERM
pub fn sfork() -> io::Result<i32> {
    let cur_pid = unsafe { libc::getpid() };

    // We use pidfd_open(2) to check the parent's pid because if the
    // child is created inside a pid namespace, getppid(2) will always
    // return 0
    let parent_pidfd = unsafe { pidfd_open(cur_pid, 0) };
    if parent_pidfd == -1 {
        return Err(Error::last_os_error());
    }

    // We wrap the parent PID file descriptor in a File object to ensure that is
    // auto-closed when it goes out of scope. But, since nothing can be read, using read(2),
    // from a PID file descriptor returned by pidfd_open(2) (it fails with EINVAL), we
    // use a new type PidFd to prevent using the File's methods directly, and in the hope
    // that whoever wants to do so will read this first.
    // This is a temporary solution until OwnedFd is stabilized.
    #[allow(dead_code)]
    struct PidFd(File);
    let _pidfd = unsafe { PidFd(File::from_raw_fd(parent_pidfd)) };

    let child_pid = unsafe { libc::fork() };
    if child_pid == -1 {
        return Err(Error::last_os_error());
    }

    if child_pid == 0 {
        // Request to receive SIGTERM on parent's death.
        let ret = unsafe { libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGTERM) };
        assert_eq!(ret, 0); // This shouldn't fail because libc::SIGTERM is a valid signal number

        // Check if the original parent died before libc::prctl() was called
        let mut pollfds = libc::pollfd {
            fd: parent_pidfd,
            events: libc::POLLIN,
            revents: 0,
        };
        let num_fds = unsafe { libc::poll(&mut pollfds, 1, 0) };
        if num_fds == -1 {
            return Err(io::Error::last_os_error());
        }
        if num_fds != 0 {
            // The original parent died
            return Err(other_io_error("Parent process died unexpectedly"));
        }
    }
    Ok(child_pid)
}

pub fn wait_for_child(pid: i32) -> ! {
    // Drop all capabilities, since the parent doesn't require any
    // capabilities, as it'd be just waiting for the child to exit.
    capng::clear(capng::Set::BOTH);
    if let Err(e) = capng::apply(capng::Set::BOTH) {
        // Don't exit the process here since we already have a child.
        error!("warning: can't apply the parent capabilities: {}", e);
    }

    let mut status = 0;
    // On success, `libc::waitpid()` returns the PID of the child.
    if unsafe { libc::waitpid(pid, &mut status, 0) } != pid {
        error!("Error during waitpid()");
        process::exit(1);
    }

    let exit_code = if libc::WIFEXITED(status) {
        libc::WEXITSTATUS(status)
    } else if libc::WIFSIGNALED(status) {
        let signal = libc::WTERMSIG(status);
        error!("Child process terminated by signal {}", signal);
        -signal
    } else {
        error!("Unexpected waitpid status: {:#X}", status);
        libc::EXIT_FAILURE
    };

    process::exit(exit_code);
}

/// Add a capability to the effective set
/// # Errors
/// An error variant will be returned:
/// - if the input string does not match the name, without the 'CAP_' prefix,
/// of any of the capability defined in `linux/capabiliy.h`.
/// - if `capng::get_caps_process()` cannot get  the  capabilities  and bounding set of the process.
/// - if `capng::update()` fails to update the internal posix capabilities settings.
/// - if `capng::apply()` fails to transfer the specified internal posix capabilities
///   settings to the kernel.
pub fn add_cap_to_eff(cap_name: &str) -> capng::Result<()> {
    use capng::{Action, CUpdate, Set, Type};
    let cap = capng::name_to_capability(cap_name)?;
    capng::get_caps_process()?;

    let req = vec![CUpdate {
        action: Action::ADD,
        cap_type: Type::EFFECTIVE,
        capability: cap,
    }];

    capng::update(req)?;
    capng::apply(Set::CAPS)?;

    Ok(())
}

/// Same as `io::Error::other()`, but the respective io_error_other feature has only been
/// stabilized in Rust 1.74.0, which is too new for our intended targets.
pub fn other_io_error<E: Into<Box<dyn std::error::Error + Send + Sync>>>(err: E) -> io::Error {
    io::Error::other(err)
}

/// Trait for `Error` object that allows prepending the error message by something that gives
/// context
pub trait ErrorContext {
    fn context<C: std::fmt::Display>(self, context: C) -> Self;
}

impl ErrorContext for io::Error {
    fn context<C: std::fmt::Display>(self, context: C) -> Self {
        io::Error::new(self.kind(), format!("{context}: {self}"))
    }
}

/// Lifts the `ErrorContext` trait to `Result` types
pub trait ResultErrorContext {
    fn err_context<C: std::fmt::Display, F: FnOnce() -> C>(self, context: F) -> Self;
}

impl<V, E: ErrorContext> ResultErrorContext for Result<V, E> {
    fn err_context<C: std::fmt::Display, F: FnOnce() -> C>(self, context: F) -> Self {
        self.map_err(|err| err.context(context()))
    }
}

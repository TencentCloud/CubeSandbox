// Copyright (c) 2021 Ant Group
// Copyright (c) 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use crate::util;
use anyhow::{anyhow, Result};
use nix::fcntl::{self, FcntlArg, FdFlag, OFlag};
use nix::libc::{STDERR_FILENO, STDIN_FILENO, STDOUT_FILENO};
use nix::pty::{openpty, OpenptyResult};
use nix::sys::signal;
use nix::sys::socket::{self, AddressFamily, SockAddr, SockFlag, SockType};
use nix::sys::stat::Mode;
use nix::sys::wait;
use nix::unistd::{self, close, dup2, fork, setsid, ForkResult, Pid};
use rustjail::pipestream::PipeStream;
use slog::Logger;
use std::ffi::CString;
use std::os::unix::io::{FromRawFd, RawFd};
use std::path::PathBuf;
use std::process::Stdio;
use std::sync::Arc;
use std::sync::Mutex as SyncMutex;

use futures::StreamExt;
use tokio::io::{AsyncRead, AsyncWrite};
use tokio::select;
use tokio::sync::watch::Receiver;

const CONSOLE_PATH: &str = "/dev/console";

lazy_static! {
    static ref SHELLS: Arc<SyncMutex<Vec<String>>> = {
        let mut v = Vec::new();

        if !cfg!(test) {
            v.push("/bin/bash".to_string());
            v.push("/bin/sh".to_string());
        }

        Arc::new(SyncMutex::new(v))
    };
}

pub fn initialize() {
    lazy_static::initialize(&SHELLS);
}

pub async fn debug_console_handler(
    logger: Logger,
    port: u32,
    mut shutdown: Receiver<bool>,
) -> Result<()> {
    let logger = logger.new(o!("subsystem" => "debug-console"));

    let shells = SHELLS.lock().unwrap().to_vec();

    let shell = shells
        .into_iter()
        .find(|sh| PathBuf::from(sh).exists())
        .ok_or_else(|| anyhow!("no shell found to launch debug console"))?;

    if port > 0 {
        let listenfd = socket::socket(
            AddressFamily::Vsock,
            SockType::Stream,
            SockFlag::SOCK_CLOEXEC,
            None,
        )?;
        let addr = SockAddr::new_vsock(libc::VMADDR_CID_ANY, port);
        socket::bind(listenfd, &addr)?;
        socket::listen(listenfd, 1)?;

        let mut incoming = util::get_vsock_incoming(listenfd);

        loop {
            select! {
                _ = shutdown.changed() => {
                    info!(logger, "debug console got shutdown request");
                    break;
                }

                conn = incoming.next() => {
                    if let Some(conn) = conn {
                        // Accept a new connection
                        match conn {
                            Ok(stream) => {
                                let logger = logger.clone();
                                let shell = shell.clone();
                                // Do not block(await) here, or we'll never receive the shutdown signal
                                tokio::spawn(async move {
                                    if let Err(e) =  run_debug_console_vsock(logger.clone(), shell, stream).await {
                                        error!(logger, "debug console aborted:{}", e);
                                    }
                                });
                            }
                            Err(e) => {
                                error!(logger, "{:?}", e);
                            }
                        }
                    } else {
                        break;
                    }
                }
            }
        }
    } else {
        let mut flags = OFlag::empty();
        flags.insert(OFlag::O_RDWR);
        flags.insert(OFlag::O_CLOEXEC);

        let fd = fcntl::open(CONSOLE_PATH, flags, Mode::empty())?;

        select! {
            _ = shutdown.changed() => {
                info!(logger, "debug console got shutdown request");
            }

            result = run_debug_console_serial(shell.clone(), fd) => {
               match result {
                   Ok(_) => {
                       info!(logger, "run_debug_console_shell session finished");
                   }
                   Err(err) => {
                       error!(logger, "run_debug_console_shell failed: {:?}", err);
                   }
               }
            }
        }
    };

    Ok(())
}

fn run_in_child(slave_fd: libc::c_int, shell: String) -> Result<()> {
    // create new session with child as session leader
    setsid()?;

    // dup stdin, stdout, stderr to let child act as a terminal
    dup2(slave_fd, STDIN_FILENO)?;
    dup2(slave_fd, STDOUT_FILENO)?;
    dup2(slave_fd, STDERR_FILENO)?;

    // set tty
    unsafe {
        libc::ioctl(0, libc::TIOCSCTTY);
    }

    let cmd = CString::new(shell).unwrap();
    let args: Vec<CString> = Vec::new();

    // run shell
    let _ = unistd::execvp(cmd.as_c_str(), &args).map_err(|e| match e {
        nix::Error::UnknownErrno => std::process::exit(-2),
        _ => std::process::exit(e as i32),
    });

    Ok(())
}

async fn run_in_parent<T: AsyncRead + AsyncWrite>(
    logger: Logger,
    stream: T,
    pseudo: OpenptyResult,
    child_pid: Pid,
) -> Result<()> {
    info!(logger, "get debug shell pid {:?}", child_pid);

    let master_fd = pseudo.master;
    let _ = close(pseudo.slave);

    let (mut socket_reader, mut socket_writer) = tokio::io::split(stream);
    let (mut master_reader, mut master_writer) = tokio::io::split(PipeStream::from_fd(master_fd));

    select! {
        res = tokio::io::copy(&mut master_reader, &mut socket_writer) => {
            debug!(
                logger,
                "master closed: {:?}", res
            );
        }
        res = tokio::io::copy(&mut socket_reader, &mut master_writer) => {
            info!(
                logger,
                "socket closed: {:?}", res
            );
        }
    }
    let _ret = signal::kill(child_pid, signal::SIGKILL);
    let wait_status = wait::waitpid(child_pid, None);
    info!(logger, "debug console process exit code: {:?}", wait_status);

    Ok(())
}

async fn run_debug_console_vsock<T: AsyncRead + AsyncWrite>(
    logger: Logger,
    shell: String,
    stream: T,
) -> Result<()> {
    let logger = logger.new(o!("subsystem" => "debug-console-shell"));

    let pseudo = openpty(None, None).map_err(|e| anyhow!("openpty failed:{}", e))?;
    let _ = fcntl::fcntl(pseudo.master, FcntlArg::F_SETFD(FdFlag::FD_CLOEXEC));
    let _ = fcntl::fcntl(pseudo.slave, FcntlArg::F_SETFD(FdFlag::FD_CLOEXEC));

    let slave_fd = pseudo.slave;
    match unsafe { fork() } {
        Ok(ForkResult::Child) => run_in_child(slave_fd, shell),
        Ok(ForkResult::Parent { child: child_pid }) => {
            run_in_parent(logger.clone(), stream, pseudo, child_pid).await
        }
        Err(err) => Err(anyhow!("fork error: {:?}", err)),
    }
}

async fn run_debug_console_serial(shell: String, fd: RawFd) -> Result<()> {
    let mut child = match tokio::process::Command::new(shell)
        .arg("-i")
        .kill_on_drop(true)
        .stdin(unsafe { Stdio::from_raw_fd(fd) })
        .stdout(unsafe { Stdio::from_raw_fd(fd) })
        .stderr(unsafe { Stdio::from_raw_fd(fd) })
        .spawn()
    {
        Ok(c) => c,
        Err(_) => return Err(anyhow!("failed to spawn shell")),
    };

    child.wait().await?;

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::tempdir;
    use tokio::sync::watch;

    #[tokio::test]
    async fn test_setup_debug_console_no_shells() {
        {
            // Guarantee no shells have been added
            // (required to avoid racing with
            // test_setup_debug_console_invalid_shell()).
            let shells_ref = SHELLS.clone();
            let mut shells = shells_ref.lock().unwrap();
            shells.clear();
        }

        let logger = slog_scope::logger();

        let (_, rx) = watch::channel(true);
        let result = debug_console_handler(logger, 0, rx).await;

        assert!(result.is_err());
        assert_eq!(
            result.unwrap_err().to_string(),
            "no shell found to launch debug console"
        );
    }

    #[tokio::test]
    async fn test_setup_debug_console_invalid_shell() {
        {
            let shells_ref = SHELLS.clone();
            let mut shells = shells_ref.lock().unwrap();

            let dir = tempdir().expect("failed to create tmpdir");

            // Add an invalid shell
            let shell = dir
                .path()
                .join("enoent")
                .to_str()
                .expect("failed to construct shell path")
                .to_string();

            shells.push(shell);
        }

        let logger = slog_scope::logger();

        let (_, rx) = watch::channel(true);
        let result = debug_console_handler(logger, 0, rx).await;

        assert!(result.is_err());
        assert_eq!(
            result.unwrap_err().to_string(),
            "no shell found to launch debug console"
        );
    }
}

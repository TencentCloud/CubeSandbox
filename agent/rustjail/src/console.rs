// SPDX-License-Identifier: Apache-2.0
//
// Copyright 2021 Sony Group Corporation
//

use anyhow::{anyhow, Result};
use nix::errno::Errno;
use nix::pty;
use nix::sys::{socket, uio};
use nix::unistd::{self, dup2};
use std::os::unix::io::{AsRawFd, RawFd};
use std::path::Path;

pub fn setup_console_socket(csocket_path: &str) -> Result<Option<RawFd>> {
    if csocket_path.is_empty() {
        return Ok(None);
    }

    let socket_fd = socket::socket(
        socket::AddressFamily::Unix,
        socket::SockType::Stream,
        socket::SockFlag::empty(),
        None,
    )?;

    match socket::connect(
        socket_fd,
        &socket::SockAddr::Unix(socket::UnixAddr::new(Path::new(csocket_path))?),
    ) {
        Ok(()) => Ok(Some(socket_fd)),
        Err(errno) => Err(anyhow!("failed to open console fd: {}", errno)),
    }
}

pub fn setup_master_console(socket_fd: RawFd) -> Result<()> {
    let pseudo = pty::openpty(None, None)?;

    let pty_name: &[u8] = b"/dev/ptmx";
    let iov = [uio::IoVec::from_slice(pty_name)];
    let fds = [pseudo.master];
    let cmsg = socket::ControlMessage::ScmRights(&fds);

    socket::sendmsg(socket_fd, &iov, &[cmsg], socket::MsgFlags::empty(), None)?;

    unistd::setsid()?;
    let ret = unsafe { libc::ioctl(pseudo.slave, libc::TIOCSCTTY) };
    Errno::result(ret).map_err(|e| anyhow!(e).context("ioctl TIOCSCTTY"))?;

    dup2(pseudo.slave, std::io::stdin().as_raw_fd())?;
    dup2(pseudo.slave, std::io::stdout().as_raw_fd())?;
    dup2(pseudo.slave, std::io::stderr().as_raw_fd())?;

    unistd::close(socket_fd)?;

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::net::UnixListener;
    use tempfile::{self, tempdir};

    const CONSOLE_SOCKET: &str = "console-socket";

    #[test]
    fn test_setup_console_socket() {
        let dir = tempdir()
            .map_err(|e| anyhow!(e).context("tempdir failed"))
            .unwrap();
        let socket_path = dir.path().join(CONSOLE_SOCKET);

        let _listener = UnixListener::bind(&socket_path).unwrap();

        let ret = setup_console_socket(socket_path.to_str().unwrap());

        assert!(ret.is_ok());
    }
}

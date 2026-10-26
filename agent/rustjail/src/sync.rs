// Copyright (c) 2019 Ant Financial
//
// SPDX-License-Identifier: Apache-2.0
//

use std::mem;
use std::os::unix::io::RawFd;
use std::time::Duration;

use anyhow::{anyhow, Result};
use nix::poll::{poll, PollFd, PollFlags};
use nix::unistd;

pub const SYNC_SUCCESS: i32 = 1;
pub const SYNC_FAILED: i32 = 2;
pub const SYNC_DATA: i32 = 3;

pub const DATA_SIZE: usize = 100;
pub const MSG_SIZE: usize = mem::size_of::<i32>();

#[macro_export]
macro_rules! log_child {
    ($fd:expr, $($arg:tt)+) => ({
        let lfd = $fd;
        let mut log_str = format_args!($($arg)+).to_string();
        log_str.push('\n');
        // Ignore error writing to the logger, not much we can do
        let _ = write_count(lfd, log_str.as_bytes(), log_str.len());
    })
}

pub fn write_count(fd: RawFd, buf: &[u8], count: usize) -> Result<usize> {
    let mut len = 0;

    loop {
        match unistd::write(fd, &buf[len..]) {
            Ok(l) => {
                len += l;
                if len == count {
                    break;
                }
            }

            Err(e) => {
                if e != nix::Error::EINTR {
                    return Err(e.into());
                }
            }
        }
    }

    Ok(len)
}

fn read_count(fd: RawFd, count: usize) -> Result<Vec<u8>> {
    let mut v: Vec<u8> = vec![0; count];
    let mut len = 0;

    loop {
        match unistd::read(fd, &mut v[len..]) {
            Ok(l) => {
                len += l;
                if len == count || l == 0 {
                    break;
                }
            }

            Err(e) => {
                if e != nix::Error::EINTR {
                    return Err(e.into());
                }
            }
        }
    }

    if len != count {
        Err(anyhow::anyhow!(
            "invalid read count expect {} get {}",
            count,
            len
        ))
    } else {
        Ok(v[0..len].to_vec())
    }
}

pub fn read_sync(fd: RawFd) -> Result<Vec<u8>> {
    let buf = read_count(fd, MSG_SIZE)?;
    if buf.len() != MSG_SIZE {
        return Err(anyhow!(
            "process: {} failed to receive sync message from peer: got msg length: {}, expected: {}",
            std::process::id(),
            buf.len(),
            MSG_SIZE
        ));
    }
    let buf_array: [u8; MSG_SIZE] = [buf[0], buf[1], buf[2], buf[3]];
    let msg: i32 = i32::from_be_bytes(buf_array);
    match msg {
        SYNC_SUCCESS => Ok(Vec::new()),
        SYNC_DATA => {
            let buf = read_count(fd, MSG_SIZE)?;
            let buf_array: [u8; MSG_SIZE] = [buf[0], buf[1], buf[2], buf[3]];
            let msg_length: i32 = i32::from_be_bytes(buf_array);
            let data_buf = read_count(fd, msg_length as usize)?;

            Ok(data_buf)
        }
        SYNC_FAILED => {
            let mut error_buf = vec![];
            loop {
                let buf = read_count(fd, DATA_SIZE)?;

                error_buf.extend(&buf);
                if DATA_SIZE == buf.len() {
                    continue;
                } else {
                    break;
                }
            }

            let error_str = match std::str::from_utf8(&error_buf) {
                Ok(v) => String::from(v),
                Err(e) => {
                    return Err(
                        anyhow!(e).context("receive error message from child process failed")
                    );
                }
            };

            Err(anyhow!(error_str))
        }
        _ => Err(anyhow!("error in receive sync message")),
    }
}

pub fn read_sync_with_timeout(fd: RawFd, timeout: Duration) -> Result<Vec<u8>> {
    let start = std::time::Instant::now();

    loop {
        let elapsed = start.elapsed();
        let Some(remaining) = timeout.checked_sub(elapsed) else {
            return Err(anyhow!(
                "process: {} timed out waiting for sync message after {}ms",
                std::process::id(),
                timeout.as_millis()
            ));
        };

        let timeout_ms = remaining.as_millis().min(i32::MAX as u128) as i32;
        let mut poll_fds = [PollFd::new(fd, PollFlags::POLLIN)];

        match poll(&mut poll_fds, timeout_ms) {
            Ok(0) => {
                return Err(anyhow!(
                    "process: {} timed out waiting for sync message after {}ms",
                    std::process::id(),
                    timeout.as_millis()
                ))
            }
            Ok(_) => return read_sync(fd),
            Err(nix::Error::EINTR) => continue,
            Err(e) => return Err(e.into()),
        }
    }
}

pub fn write_sync(fd: RawFd, msg_type: i32, data_str: &str) -> Result<()> {
    let buf = msg_type.to_be_bytes();

    let count = write_count(fd, &buf, MSG_SIZE)?;
    if count != MSG_SIZE {
        return Err(anyhow!("error in send sync message"));
    }

    match msg_type {
        SYNC_FAILED => match write_count(fd, data_str.as_bytes(), data_str.len()) {
            Ok(_count) => unistd::close(fd)?,
            Err(e) => {
                unistd::close(fd)?;
                return Err(anyhow!(e).context("error in send message to process"));
            }
        },
        SYNC_DATA => {
            let length: i32 = data_str.len() as i32;
            write_count(fd, &length.to_be_bytes(), MSG_SIZE).or_else(|e| {
                unistd::close(fd)?;
                Err(anyhow!(e).context("error in send message to process"))
            })?;

            write_count(fd, data_str.as_bytes(), data_str.len()).or_else(|e| {
                unistd::close(fd)?;
                Err(anyhow!(e).context("error in send message to process"))
            })?;
        }

        _ => (),
    };

    Ok(())
}

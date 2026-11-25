// Copyright 2026 Tencent Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//! NBD connection establishment, reconnection helpers, and synchronous commands.

use std::io::{self, Read, Write};
use std::net::{Shutdown, TcpStream, ToSocketAddrs};
use std::os::unix::io::{AsRawFd, RawFd};
use std::os::unix::net::UnixStream;
use std::thread;
use std::time::{Duration, Instant};
use std::{fmt, mem};

use log::{info, warn};
use thiserror::Error;

use crate::handshake::{negotiate, NbdExportInfo, NbdHandshakeError};
use crate::proto::*;
use crate::uri::{NbdAddress, NbdUri};

/// Default timeout applied to connect and every handshake read/write.
pub const DEFAULT_HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(10);
/// Default TCP keep-alive idle time in seconds.
pub const DEFAULT_KEEPALIVE_IDLE: u32 = 30;
/// Default TCP keep-alive probe interval in seconds.
pub const DEFAULT_KEEPALIVE_INTERVAL: u32 = 10;
/// Default number of unanswered TCP keep-alive probes.
pub const DEFAULT_KEEPALIVE_COUNT: u32 = 3;

/// Largest TCP keep-alive idle time or interval accepted by Linux, in seconds.
pub const MAX_TCP_KEEPALIVE_SECS: u32 = 32767;
/// Largest number of TCP keep-alive probes accepted by Linux.
pub const MAX_TCP_KEEPALIVE_COUNT: u32 = 127;

/// TCP keep-alive settings of an NBD connection.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct TcpKeepalive {
    pub idle: u32,
    pub interval: u32,
    pub count: u32,
}

impl TcpKeepalive {
    pub fn new(idle: Option<u32>, interval: Option<u32>, count: Option<u32>) -> Self {
        Self {
            idle: idle.unwrap_or(DEFAULT_KEEPALIVE_IDLE),
            interval: interval.unwrap_or(DEFAULT_KEEPALIVE_INTERVAL),
            count: count.unwrap_or(DEFAULT_KEEPALIVE_COUNT),
        }
    }
}

/// Socket and fault-recovery settings of an NBD connection.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct NbdSocketOptions {
    /// Bounds TCP connect and every handshake read/write.
    pub handshake_timeout: Duration,
    /// Bounds every socket operation after the handshake; `None` waits indefinitely.
    pub socket_timeout: Option<Duration>,
    /// Bounds automatic reconnect + replay when a connection drops; `None` disables reconnect.
    pub reconnect_timeout: Option<Duration>,
    /// TCP keep-alive settings; `None` disables keep-alive.
    pub tcp_keepalive: Option<TcpKeepalive>,
}

impl NbdSocketOptions {
    pub fn new(
        handshake_timeout: Option<Duration>,
        socket_timeout: Option<Duration>,
        tcp_keepalive: Option<TcpKeepalive>,
    ) -> Self {
        Self {
            handshake_timeout: handshake_timeout.unwrap_or(DEFAULT_HANDSHAKE_TIMEOUT),
            socket_timeout,
            reconnect_timeout: None,
            tcp_keepalive,
        }
    }

    pub fn with_reconnect_timeout(mut self, reconnect_timeout: Option<Duration>) -> Self {
        self.reconnect_timeout = reconnect_timeout;
        self
    }
}

impl Default for NbdSocketOptions {
    fn default() -> Self {
        Self::new(None, None, None)
    }
}

#[derive(Error, Debug)]
pub enum NbdConnectError {
    #[error("Failed to connect to NBD server {0}")]
    Connect(String, #[source] io::Error),
    #[error("Failed to configure NBD socket")]
    Configure(#[source] io::Error),
    #[error("NBD handshake with {0} failed")]
    Handshake(String, #[source] NbdHandshakeError),
    #[error("Reconnected NBD export {0} changed size or read-only flags")]
    ExportChanged(String),
}

/// Connected stream to an NBD server.
#[derive(Debug)]
pub(crate) enum NbdStream {
    Tcp(TcpStream),
    Unix(UnixStream),
}

impl NbdStream {
    fn set_timeouts(&self, timeout: Option<Duration>) -> io::Result<()> {
        match self {
            Self::Tcp(s) => {
                s.set_read_timeout(timeout)?;
                s.set_write_timeout(timeout)
            }
            Self::Unix(s) => {
                s.set_read_timeout(timeout)?;
                s.set_write_timeout(timeout)
            }
        }
    }

    pub(crate) fn shutdown(&self) {
        let _ = match self {
            Self::Tcp(s) => s.shutdown(Shutdown::Both),
            Self::Unix(s) => s.shutdown(Shutdown::Both),
        };
    }
}

impl Read for NbdStream {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        match self {
            Self::Tcp(s) => s.read(buf),
            Self::Unix(s) => s.read(buf),
        }
    }
}

impl Write for NbdStream {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        match self {
            Self::Tcp(s) => s.write(buf),
            Self::Unix(s) => s.write(buf),
        }
    }

    fn flush(&mut self) -> io::Result<()> {
        match self {
            Self::Tcp(s) => s.flush(),
            Self::Unix(s) => s.flush(),
        }
    }
}

impl AsRawFd for NbdStream {
    fn as_raw_fd(&self) -> RawFd {
        match self {
            Self::Tcp(s) => s.as_raw_fd(),
            Self::Unix(s) => s.as_raw_fd(),
        }
    }
}

fn set_int_option(fd: RawFd, level: i32, name: i32, value: u32) -> io::Result<()> {
    let value = value as libc::c_int;
    let ret = unsafe {
        libc::setsockopt(
            fd,
            level,
            name,
            (&value as *const libc::c_int).cast(),
            mem::size_of::<libc::c_int>() as libc::socklen_t,
        )
    };
    if ret < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

fn set_keepalive(s: &TcpStream, keepalive: &TcpKeepalive) -> io::Result<()> {
    let fd = s.as_raw_fd();
    set_int_option(fd, libc::SOL_SOCKET, libc::SO_KEEPALIVE, 1)?;
    set_int_option(fd, libc::IPPROTO_TCP, libc::TCP_KEEPIDLE, keepalive.idle)?;
    set_int_option(
        fd,
        libc::IPPROTO_TCP,
        libc::TCP_KEEPINTVL,
        keepalive.interval,
    )?;
    set_int_option(fd, libc::IPPROTO_TCP, libc::TCP_KEEPCNT, keepalive.count)
}

fn connect_stream(address: &NbdAddress, options: &NbdSocketOptions) -> io::Result<NbdStream> {
    let timeout = options.handshake_timeout;
    let stream = match address {
        NbdAddress::Tcp { host, port } => {
            let mut last_error = io::Error::new(io::ErrorKind::NotFound, "no address resolved");
            let mut stream = None;
            for addr in (host.as_str(), *port).to_socket_addrs()? {
                match TcpStream::connect_timeout(&addr, timeout) {
                    Ok(s) => {
                        stream = Some(s);
                        break;
                    }
                    Err(e) => last_error = e,
                }
            }
            let s = stream.ok_or(last_error)?;
            s.set_nodelay(true)?;
            if let Some(keepalive) = &options.tcp_keepalive {
                set_keepalive(&s, keepalive)?;
            }
            NbdStream::Tcp(s)
        }
        NbdAddress::Unix { path } => NbdStream::Unix(UnixStream::connect(path)?),
    };
    stream.set_timeouts(Some(timeout))?;
    Ok(stream)
}

/// Returns a copy of `iovs` restricted to the byte range `[start, start + len)`.
pub(crate) fn sub_iovecs(iovs: &[libc::iovec], start: usize, len: usize) -> Vec<libc::iovec> {
    let mut out = Vec::new();
    let mut skip = start;
    let mut left = len;
    for iov in iovs {
        if left == 0 {
            break;
        }
        if skip >= iov.iov_len {
            skip -= iov.iov_len;
            continue;
        }
        let n = (iov.iov_len - skip).min(left);
        out.push(libc::iovec {
            iov_base: unsafe { iov.iov_base.cast::<u8>().add(skip) }.cast(),
            iov_len: n,
        });
        left -= n;
        skip = 0;
    }
    debug_assert_eq!(left, 0, "iovec range out of bounds");
    out
}

/// Drops the first `n` bytes from `iovs`.
pub(crate) fn advance_iovecs(iovs: &mut Vec<libc::iovec>, n: usize) {
    let total: usize = iovs.iter().map(|iov| iov.iov_len).sum();
    *iovs = sub_iovecs(iovs, n, total - n);
}

/// Returns the number of bytes described by `iovs`.
pub(crate) fn iovecs_len(iovs: &[libc::iovec]) -> usize {
    iovs.iter().map(|iov| iov.iov_len).sum()
}

/// Builds a `msghdr` describing `iovs`.
pub(crate) fn msghdr_for(iovs: &mut [libc::iovec]) -> libc::msghdr {
    let mut msg: libc::msghdr = unsafe { mem::zeroed() };
    msg.msg_iov = iovs.as_mut_ptr();
    msg.msg_iovlen = iovs.len() as _;
    msg
}

/// Attempts to reconnect to `uri` within `budget` using exponential backoff,
/// verifying that export size and writeability match `expected`.
pub(crate) fn reconnect_with_backoff(
    uri: &NbdUri,
    options: &NbdSocketOptions,
    expected: &NbdExportInfo,
    readonly: bool,
    budget: Duration,
) -> Result<NbdConn, NbdConnectError> {
    let deadline = Instant::now() + budget;
    let mut delay = Duration::from_millis(10);
    let max_delay = Duration::from_millis(200);
    let mut attempt = 0u32;

    loop {
        attempt += 1;
        let now = Instant::now();
        let remaining = deadline.saturating_duration_since(now);
        let mut attempt_opts = *options;
        if remaining > Duration::ZERO && remaining < attempt_opts.handshake_timeout {
            attempt_opts.handshake_timeout = remaining;
        }

        match NbdConn::connect_with_mode(uri.clone(), attempt_opts, readonly) {
            Ok(conn) => {
                let new_info = conn.info();
                if new_info.size != expected.size
                    || (!readonly && new_info.has_flag(NBD_FLAG_READ_ONLY))
                {
                    return Err(NbdConnectError::ExportChanged(uri.to_string()));
                }
                info!(
                    "Reconnected to NBD export {} after {} attempt(s)",
                    uri, attempt
                );
                return Ok(conn);
            }
            Err(err) => {
                let now = Instant::now();
                if now >= deadline {
                    warn!(
                        "Failed to reconnect to NBD export {} within {:?} ({} attempts): {}",
                        uri, budget, attempt, err
                    );
                    return Err(err);
                }
                let sleep_for = delay.min(deadline.saturating_duration_since(now));
                if !sleep_for.is_zero() {
                    thread::sleep(sleep_for);
                }
                delay = (delay * 2).min(max_delay);
            }
        }
    }
}

/// One NBD connection in the transmission phase.
///
/// Can be used directly as a standalone synchronous NBD client (`read_at`,
/// `write_at`, `flush`, `disconnect`), or handed to [`crate::NbdSyncEngine`] /
/// [`crate::NbdUringEngine`].
#[derive(Debug)]
pub struct NbdConn {
    uri: Option<NbdUri>,
    options: NbdSocketOptions,
    readonly: bool,
    stream: NbdStream,
    info: NbdExportInfo,
    next_cookie: u64,
    dead: bool,
}

impl NbdConn {
    /// Connects to `uri` and completes the NBD handshake.
    pub fn connect(uri: &NbdUri, options: &NbdSocketOptions) -> Result<Self, NbdConnectError> {
        Self::connect_with_mode(uri.clone(), *options, false)
    }

    pub(crate) fn connect_with_mode(
        uri: NbdUri,
        options: NbdSocketOptions,
        readonly: bool,
    ) -> Result<Self, NbdConnectError> {
        let mut stream = connect_stream(&uri.address, &options)
            .map_err(|e| NbdConnectError::Connect(uri.to_string(), e))?;
        let info = negotiate(&mut stream, &uri.export)
            .map_err(|e| NbdConnectError::Handshake(uri.to_string(), e))?;
        stream
            .set_timeouts(options.socket_timeout)
            .map_err(NbdConnectError::Configure)?;
        Ok(Self {
            uri: Some(uri),
            options,
            readonly,
            stream,
            info,
            next_cookie: 1,
            dead: false,
        })
    }

    pub fn info(&self) -> &NbdExportInfo {
        &self.info
    }

    pub fn uri(&self) -> Option<&NbdUri> {
        self.uri.as_ref()
    }

    pub fn options(&self) -> &NbdSocketOptions {
        &self.options
    }

    pub fn readonly(&self) -> bool {
        self.readonly
    }

    pub(crate) fn fd(&self) -> RawFd {
        self.stream.as_raw_fd()
    }

    pub(crate) fn timeout_error(&self) -> io::Error {
        io::Error::new(
            io::ErrorKind::TimedOut,
            format!(
                "NBD socket operation timed out after {:?}",
                self.options.socket_timeout.unwrap_or_default()
            ),
        )
    }

    pub fn is_dead(&self) -> bool {
        self.dead
    }

    pub(crate) fn mark_dead(&mut self, reason: &dyn fmt::Display) {
        if !self.dead {
            warn!("NBD connection lost: {reason}");
            self.dead = true;
            self.stream.shutdown();
        }
    }

    /// Tries to re-establish this connection when `reconnect_timeout` is configured.
    pub fn try_reconnect(&mut self) -> bool {
        let (uri, budget) = match (&self.uri, self.options.reconnect_timeout) {
            (Some(u), Some(b)) if !b.is_zero() => (u.clone(), b),
            _ => return false,
        };
        self.close();
        match reconnect_with_backoff(&uri, &self.options, &self.info, self.readonly, budget) {
            Ok(new_conn) => {
                *self = new_conn;
                true
            }
            Err(_) => false,
        }
    }

    pub(crate) fn next_cookie(&mut self) -> u64 {
        let cookie = self.next_cookie;
        self.next_cookie = self.next_cookie.wrapping_add(1);
        cookie
    }

    /// Reads `buf.len()` bytes starting at `offset`, splitting into chunks if needed.
    pub fn read_at(&mut self, mut offset: u64, mut buf: &mut [u8]) -> io::Result<()> {
        let max_chunk = (self.info.max_block / self.info.min_block * self.info.min_block).max(512);
        while !buf.is_empty() {
            let take = buf.len().min(max_chunk as usize);
            let (head, tail) = buf.split_at_mut(take);
            let iov = libc::iovec {
                iov_base: head.as_mut_ptr().cast(),
                iov_len: head.len(),
            };
            let errno = unsafe { self.command_iov(NBD_CMD_READ, offset, take as u32, &[iov])? };
            if errno != 0 {
                return Err(io::Error::from_raw_os_error(errno));
            }
            offset += take as u64;
            buf = tail;
        }
        Ok(())
    }

    /// Writes `buf` starting at `offset`, splitting into chunks if needed.
    pub fn write_at(&mut self, mut offset: u64, mut buf: &[u8]) -> io::Result<()> {
        let max_chunk = (self.info.max_block / self.info.min_block * self.info.min_block).max(512);
        while !buf.is_empty() {
            let take = buf.len().min(max_chunk as usize);
            let (head, tail) = buf.split_at(take);
            let iov = libc::iovec {
                iov_base: head.as_ptr() as *mut libc::c_void,
                iov_len: head.len(),
            };
            let errno = unsafe { self.command_iov(NBD_CMD_WRITE, offset, take as u32, &[iov])? };
            if errno != 0 {
                return Err(io::Error::from_raw_os_error(errno));
            }
            offset += take as u64;
            buf = tail;
        }
        Ok(())
    }

    /// Flushes server-side caches if `NBD_FLAG_SEND_FLUSH` was negotiated.
    pub fn flush(&mut self) -> io::Result<()> {
        if !self.info.has_flag(NBD_FLAG_SEND_FLUSH) {
            return Ok(());
        }
        let errno = unsafe { self.command_iov(NBD_CMD_FLUSH, 0, 0, &[])? };
        if errno != 0 {
            return Err(io::Error::from_raw_os_error(errno));
        }
        Ok(())
    }

    unsafe fn send_all(&mut self, mut iovs: Vec<libc::iovec>) -> io::Result<()> {
        while iovecs_len(&iovs) > 0 {
            let msg = msghdr_for(&mut iovs);
            let n = unsafe { libc::sendmsg(self.fd(), &msg, libc::MSG_NOSIGNAL) };
            if n < 0 {
                match io::Error::last_os_error() {
                    e if e.kind() == io::ErrorKind::Interrupted => continue,
                    e if e.kind() == io::ErrorKind::WouldBlock => return Err(self.timeout_error()),
                    e => return Err(e),
                }
            }
            if n == 0 {
                return Err(io::Error::new(
                    io::ErrorKind::WriteZero,
                    "NBD socket accepted no data",
                ));
            }
            advance_iovecs(&mut iovs, n as usize);
        }
        Ok(())
    }

    unsafe fn recv_all(&mut self, mut iovs: Vec<libc::iovec>) -> io::Result<()> {
        while iovecs_len(&iovs) > 0 {
            let mut msg = msghdr_for(&mut iovs);
            let n = unsafe { libc::recvmsg(self.fd(), &mut msg, libc::MSG_WAITALL) };
            if n < 0 {
                match io::Error::last_os_error() {
                    e if e.kind() == io::ErrorKind::Interrupted => continue,
                    e if e.kind() == io::ErrorKind::WouldBlock => return Err(self.timeout_error()),
                    e => return Err(e),
                }
            }
            if n == 0 {
                return Err(io::ErrorKind::UnexpectedEof.into());
            }
            advance_iovecs(&mut iovs, n as usize);
        }
        Ok(())
    }

    fn recv_exact(&mut self, buf: &mut [u8]) -> io::Result<()> {
        let iov = libc::iovec {
            iov_base: buf.as_mut_ptr().cast(),
            iov_len: buf.len(),
        };
        unsafe { self.recv_all(vec![iov]) }
    }

    /// Sends one NBD command and waits for its simple reply, reconnecting once
    /// if `reconnect_timeout` is configured and the transport drops.
    ///
    /// # Safety
    ///
    /// `payload` must describe valid memory for reads (`NBD_CMD_WRITE`) or
    /// writes (`NBD_CMD_READ`) of length `length` for the duration of the call.
    pub unsafe fn command_iov(
        &mut self,
        command: u16,
        offset: u64,
        length: u32,
        payload: &[libc::iovec],
    ) -> io::Result<i32> {
        if self.dead && !self.try_reconnect() {
            return Err(io::Error::from_raw_os_error(libc::EIO));
        }

        match unsafe { self.command_inner(command, offset, length, payload) } {
            Ok(errno)
                if errno == libc::ESHUTDOWN
                    && command != NBD_CMD_DISC
                    && self.options.reconnect_timeout.is_some() =>
            {
                let shutdown_err = io::Error::from_raw_os_error(libc::ESHUTDOWN);
                self.mark_dead(&shutdown_err);
                if self.try_reconnect() {
                    unsafe { self.command_inner(command, offset, length, payload) }.inspect_err(
                        |e2| {
                            self.mark_dead(e2);
                        },
                    )
                } else {
                    Ok(errno)
                }
            }
            Ok(errno) => Ok(errno),
            Err(e) => {
                self.mark_dead(&e);
                if command != NBD_CMD_DISC && self.try_reconnect() {
                    match unsafe { self.command_inner(command, offset, length, payload) } {
                        Ok(errno) => Ok(errno),
                        Err(e2) => {
                            self.mark_dead(&e2);
                            Err(e2)
                        }
                    }
                } else {
                    Err(e)
                }
            }
        }
    }

    unsafe fn command_inner(
        &mut self,
        command: u16,
        offset: u64,
        length: u32,
        payload: &[libc::iovec],
    ) -> io::Result<i32> {
        let cookie = self.next_cookie();
        let mut header = NbdRequest {
            flags: 0,
            command,
            cookie,
            offset,
            length,
        }
        .encode();
        let mut iovs = vec![libc::iovec {
            iov_base: header.as_mut_ptr().cast(),
            iov_len: header.len(),
        }];
        if command == NBD_CMD_WRITE {
            iovs.extend_from_slice(payload);
        }
        unsafe { self.send_all(iovs)? };
        if command == NBD_CMD_DISC {
            return Ok(0);
        }

        let mut reply = [0u8; NBD_SIMPLE_REPLY_SIZE];
        self.recv_exact(&mut reply)?;
        let reply = NbdSimpleReply::decode(&reply)
            .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "bad NBD reply magic"))?;
        if reply.cookie != cookie {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("unexpected NBD reply cookie {}", reply.cookie),
            ));
        }
        let errno = nbd_errno_to_errno(reply.error);
        if command == NBD_CMD_READ && errno == 0 && length > 0 {
            unsafe { self.recv_all(payload.to_vec())? };
        }
        Ok(errno)
    }

    /// Sends `NBD_CMD_DISC` if alive and shuts down the socket.
    pub fn disconnect(&mut self) {
        if !self.dead {
            let _ = unsafe { self.command_inner(NBD_CMD_DISC, 0, 0, &[]) };
        }
        self.close();
    }

    /// Shuts down the socket without sending `NBD_CMD_DISC`.
    pub fn close(&mut self) {
        self.dead = true;
        self.stream.shutdown();
    }
}

#[cfg(test)]
mod tests {
    use std::net::TcpListener;

    use super::*;

    #[test]
    fn sub_iovecs_ranges() {
        let mut a = [0u8; 4];
        let mut b = [0u8; 6];
        let iovs = [
            libc::iovec {
                iov_base: a.as_mut_ptr().cast(),
                iov_len: a.len(),
            },
            libc::iovec {
                iov_base: b.as_mut_ptr().cast(),
                iov_len: b.len(),
            },
        ];
        let sub = sub_iovecs(&iovs, 2, 5);
        assert_eq!(sub.len(), 2);
        assert_eq!(sub[0].iov_len, 2);
        assert_eq!(sub[0].iov_base as usize, a.as_ptr() as usize + 2);
        assert_eq!(sub[1].iov_len, 3);
        assert_eq!(sub[1].iov_base as usize, b.as_ptr() as usize);

        let sub = sub_iovecs(&iovs, 4, 6);
        assert_eq!(sub.len(), 1);
        assert_eq!(sub[0].iov_base as usize, b.as_ptr() as usize);

        let mut v = iovs.to_vec();
        advance_iovecs(&mut v, 5);
        assert_eq!(iovecs_len(&v), 5);
        assert_eq!(v[0].iov_base as usize, b.as_ptr() as usize + 1);
    }

    fn int_option(fd: RawFd, level: i32, name: i32) -> i32 {
        let mut value: libc::c_int = 0;
        let mut len = mem::size_of::<libc::c_int>() as libc::socklen_t;
        let ret = unsafe {
            libc::getsockopt(
                fd,
                level,
                name,
                (&mut value as *mut libc::c_int).cast(),
                &mut len,
            )
        };
        assert_eq!(ret, 0, "{}", io::Error::last_os_error());
        value
    }

    fn keepalive_of(options: &NbdSocketOptions) -> (i32, i32, i32, i32) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = NbdAddress::Tcp {
            host: "127.0.0.1".to_string(),
            port: listener.local_addr().unwrap().port(),
        };
        let stream = connect_stream(&address, options).unwrap();
        let fd = stream.as_raw_fd();
        (
            int_option(fd, libc::SOL_SOCKET, libc::SO_KEEPALIVE),
            int_option(fd, libc::IPPROTO_TCP, libc::TCP_KEEPIDLE),
            int_option(fd, libc::IPPROTO_TCP, libc::TCP_KEEPINTVL),
            int_option(fd, libc::IPPROTO_TCP, libc::TCP_KEEPCNT),
        )
    }

    #[test]
    fn tcp_keepalive_options() {
        let disabled = keepalive_of(&NbdSocketOptions::default());
        assert_eq!(disabled.0, 0);

        let defaults = NbdSocketOptions::new(None, None, Some(TcpKeepalive::new(None, None, None)));
        assert_eq!(keepalive_of(&defaults), (1, 30, 10, 3));

        let explicit = NbdSocketOptions::new(
            None,
            None,
            Some(TcpKeepalive::new(Some(60), Some(5), Some(7))),
        );
        assert_eq!(keepalive_of(&explicit), (1, 60, 5, 7));
    }

    #[test]
    fn handshake_timeout() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let uri: NbdUri = format!("nbd://127.0.0.1:{port}/x").parse().unwrap();
        let timeout = Duration::from_millis(200);
        let options = NbdSocketOptions::new(Some(timeout), None, None);

        let start = Instant::now();
        let result = NbdConn::connect(&uri, &options);
        let elapsed = start.elapsed();
        assert!(
            matches!(result, Err(NbdConnectError::Handshake(..))),
            "{result:?}"
        );
        assert!(elapsed >= timeout, "{elapsed:?}");
        assert!(elapsed < Duration::from_secs(2), "{elapsed:?}");
    }
}

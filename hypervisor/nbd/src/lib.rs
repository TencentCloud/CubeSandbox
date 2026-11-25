// Copyright 2026 Tencent Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//! General-purpose Network Block Device (NBD) client library.
//!
//! Provides:
//! - [`NbdUri`] / [`is_nbd_uri`] — NBD URI parser (`nbd://` and `nbd+unix://`, plus timeout/reconnect query params)
//! - [`NbdConn`] — standalone synchronous client (`read_at`, `write_at`, `flush`, `disconnect`)
//! - [`NbdClient`] — multi-queue client factory managing probe connection reuse and per-queue engines
//! - [`NbdSyncEngine`] & [`NbdUringEngine`] — eventfd-notified scatter/gather engines with transparent reconnect

pub mod conn;
pub mod handshake;
pub mod proto;
pub mod sync_engine;
pub(crate) mod tracker;
pub mod uri;
pub mod uring_engine;

use std::io;
use std::sync::Mutex;

use log::info;
use thiserror::Error;
use vmm_sys_util::eventfd::EventFd;

pub use crate::conn::{
    NbdConn, NbdConnectError, NbdSocketOptions, TcpKeepalive, DEFAULT_HANDSHAKE_TIMEOUT,
    MAX_TCP_KEEPALIVE_COUNT, MAX_TCP_KEEPALIVE_SECS,
};
pub use crate::handshake::{negotiate, NbdExportInfo, NbdHandshakeError};
pub use crate::proto::*;
pub use crate::sync_engine::NbdSyncEngine;
pub use crate::uri::{is_nbd_uri, NbdAddress, NbdUri, NbdUriError};
pub use crate::uring_engine::{uring_supported, NbdUringEngine};

#[derive(Error, Debug)]
pub enum NbdError {
    #[error("Invalid NBD URI: {0}")]
    Uri(#[from] NbdUriError),
    #[error("Failed to connect to NBD export: {0}")]
    Connect(#[from] NbdConnectError),
    #[error("NBD export {0} is read-only, cannot open as writable")]
    ReadOnlyExport(String),
    #[error("NBD export {0} does not support multi-conn, cannot open {1} queues")]
    MultiConnUnsupported(String, usize),
    #[error("NBD export {0} changed size or flags across queue connections")]
    ExportChanged(String),
    #[error("Failed to initialize NBD I/O engine: {0}")]
    Engine(#[source] io::Error),
}

impl From<NbdError> for io::Error {
    fn from(err: NbdError) -> Self {
        let kind = match &err {
            NbdError::Uri(_) => io::ErrorKind::InvalidInput,
            NbdError::ReadOnlyExport(_) => io::ErrorKind::PermissionDenied,
            NbdError::MultiConnUnsupported(..) => io::ErrorKind::Unsupported,
            _ => io::ErrorKind::Other,
        };
        io::Error::new(kind, err)
    }
}

/// Unified per-queue NBD engine wrapping either `io_uring` or synchronous I/O.
pub enum NbdEngine {
    Uring(NbdUringEngine),
    Sync(NbdSyncEngine),
}

impl NbdEngine {
    pub fn notifier(&self) -> &EventFd {
        match self {
            Self::Uring(e) => e.notifier(),
            Self::Sync(e) => e.notifier(),
        }
    }

    /// # Safety
    ///
    /// `iovecs` must describe writable memory valid until `user_data` is returned by [`Self::complete`].
    pub unsafe fn read_vectored(
        &mut self,
        offset: u64,
        iovecs: Vec<libc::iovec>,
        user_data: u64,
    ) -> io::Result<()> {
        match self {
            Self::Uring(e) => unsafe { e.read_vectored(offset, iovecs, user_data) },
            Self::Sync(e) => unsafe { e.read_vectored(offset, iovecs, user_data) },
        }
    }

    /// # Safety
    ///
    /// `iovecs` must describe readable memory valid until `user_data` is returned by [`Self::complete`].
    pub unsafe fn write_vectored(
        &mut self,
        offset: u64,
        iovecs: Vec<libc::iovec>,
        user_data: u64,
    ) -> io::Result<()> {
        match self {
            Self::Uring(e) => unsafe { e.write_vectored(offset, iovecs, user_data) },
            Self::Sync(e) => unsafe { e.write_vectored(offset, iovecs, user_data) },
        }
    }

    pub fn fsync(&mut self, user_data: Option<u64>) -> io::Result<()> {
        match self {
            Self::Uring(e) => e.fsync(user_data),
            Self::Sync(e) => e.fsync(user_data),
        }
    }

    pub fn trim(&mut self, offset: u64, length: u64, user_data: u64) -> io::Result<()> {
        match self {
            Self::Uring(e) => e.trim(offset, length, user_data),
            Self::Sync(e) => e.trim(offset, length, user_data),
        }
    }

    pub fn write_zeroes(&mut self, offset: u64, length: u64, user_data: u64) -> io::Result<()> {
        match self {
            Self::Uring(e) => e.write_zeroes(offset, length, user_data),
            Self::Sync(e) => e.write_zeroes(offset, length, user_data),
        }
    }

    pub fn complete(&mut self) -> Vec<(u64, i32)> {
        match self {
            Self::Uring(e) => e.complete(),
            Self::Sync(e) => e.complete(),
        }
    }
}

/// Options for opening an [`NbdClient`].
#[derive(Clone, Debug)]
pub struct NbdClientOptions {
    pub readonly: bool,
    pub num_queues: usize,
    pub prefer_uring: bool,
    /// When `Some`, overrides the socket options parsed from the URI query string.
    pub socket_override: Option<NbdSocketOptions>,
}

impl Default for NbdClientOptions {
    fn default() -> Self {
        Self {
            readonly: false,
            num_queues: 1,
            prefer_uring: true,
            socket_override: None,
        }
    }
}

/// Multi-queue NBD client handle.
///
/// Opens a probe connection up front to validate export metadata, then yields
/// one independent [`NbdEngine`] per queue (reusing the probe connection for the
/// first queue).
#[derive(Debug)]
pub struct NbdClient {
    uri: NbdUri,
    socket: NbdSocketOptions,
    readonly: bool,
    use_uring: bool,
    info: NbdExportInfo,
    probe: Mutex<Option<NbdConn>>,
}

impl NbdClient {
    pub fn open(uri_str: &str, opts: NbdClientOptions) -> Result<Self, NbdError> {
        let uri: NbdUri = uri_str.parse()?;
        let socket = opts.socket_override.unwrap_or(uri.socket_options);
        let conn = NbdConn::connect_with_mode(uri.clone(), socket, opts.readonly)?;
        let info = *conn.info();

        if !opts.readonly && info.has_flag(NBD_FLAG_READ_ONLY) {
            return Err(NbdError::ReadOnlyExport(uri.to_string()));
        }
        let effective_readonly = opts.readonly || info.has_flag(NBD_FLAG_READ_ONLY);
        if !effective_readonly && !info.allows_connections(opts.num_queues) {
            return Err(NbdError::MultiConnUnsupported(
                uri.to_string(),
                opts.num_queues,
            ));
        }

        let use_uring = opts.prefer_uring && uring_supported(socket.socket_timeout.is_some());
        info!(
            "Connected to NBD export {}: size={} flags={:#x} backend={}",
            uri,
            info.size,
            info.flags,
            if use_uring { "io_uring" } else { "sync" }
        );

        Ok(Self {
            uri,
            socket,
            readonly: opts.readonly,
            use_uring,
            info,
            probe: Mutex::new(Some(conn)),
        })
    }

    pub fn export_info(&self) -> &NbdExportInfo {
        &self.info
    }

    pub fn uri(&self) -> &NbdUri {
        &self.uri
    }

    pub fn uses_uring(&self) -> bool {
        self.use_uring
    }

    fn take_or_connect(&self) -> Result<NbdConn, NbdError> {
        if let Some(conn) = self.probe.lock().unwrap().take() {
            return Ok(conn);
        }
        let conn = NbdConn::connect_with_mode(self.uri.clone(), self.socket, self.readonly)?;
        let info = conn.info();
        if info.size != self.info.size || info.flags != self.info.flags {
            return Err(NbdError::ExportChanged(self.uri.to_string()));
        }
        Ok(conn)
    }

    /// Creates one independent per-queue I/O engine.
    pub fn new_engine(&self) -> Result<NbdEngine, NbdError> {
        let conn = self.take_or_connect()?;
        if self.use_uring {
            Ok(NbdEngine::Uring(
                NbdUringEngine::new(conn).map_err(NbdError::Engine)?,
            ))
        } else {
            Ok(NbdEngine::Sync(
                NbdSyncEngine::new(conn).map_err(NbdError::Engine)?,
            ))
        }
    }
}

#[cfg(test)]
mod tests {
    use std::io::{Read, Write};
    use std::os::unix::io::AsRawFd;
    use std::os::unix::net::{UnixListener, UnixStream};
    use std::slice;
    use std::sync::{Arc, Mutex};
    use std::thread;
    use std::time::Duration;

    use vmm_sys_util::tempdir::TempDir;

    use super::*;
    use crate::handshake::tests::ServerHandshake;

    const TEST_SIZE: usize = 128 * 1024;

    fn wait_completions(engine: &mut NbdEngine, expected: usize) -> Vec<(u64, i32)> {
        let mut got = Vec::new();
        let mut pollfd = libc::pollfd {
            fd: engine.notifier().as_raw_fd(),
            events: libc::POLLIN,
            revents: 0,
        };
        while got.len() < expected {
            got.extend(engine.complete());
            if got.len() >= expected {
                break;
            }
            let r = unsafe { libc::poll(&mut pollfd, 1, 5000) };
            assert!(r > 0, "timed out waiting for NBD completions");
            let _ = engine.notifier().read();
        }
        got
    }

    fn serve_connection(
        mut s: UnixStream,
        data: &Mutex<Vec<u8>>,
        reverse_batch: usize,
        byte_by_byte: bool,
        drop_after_reqs: Option<usize>,
    ) {
        let hs = ServerHandshake {
            flags: NBD_FLAG_HAS_FLAGS | NBD_FLAG_SEND_FLUSH | NBD_FLAG_CAN_MULTI_CONN,
            size: TEST_SIZE as u64,
            export: "vol0",
            support_go: true,
            block_size: Some((512, 4096, 16 * 1024)),
        };
        if hs.run(&mut s).is_err() {
            return;
        }

        let mut queued_replies: Vec<Vec<u8>> = Vec::new();
        let mut seen = 0usize;
        loop {
            let mut hdr = [0u8; NBD_REQUEST_SIZE];
            if s.read_exact(&mut hdr).is_err() {
                return;
            }
            let req = NbdRequest::decode(&hdr).expect("bad request magic");
            if req.command == NBD_CMD_DISC {
                return;
            }
            seen += 1;
            let range = req.offset as usize..(req.offset as usize + req.length as usize);
            let mut payload_in = Vec::new();
            if req.command == NBD_CMD_WRITE {
                payload_in.resize(req.length as usize, 0);
                if s.read_exact(&mut payload_in).is_err() {
                    return;
                }
            }
            if drop_after_reqs.is_some_and(|limit| seen > limit) {
                // Drop mid-flight without replying to trigger client reconnect + replay!
                return;
            }

            let mut reply = NbdSimpleReply {
                error: NBD_SUCCESS,
                cookie: req.cookie,
            }
            .encode()
            .to_vec();

            match req.command {
                NBD_CMD_WRITE => {
                    data.lock().unwrap()[range].copy_from_slice(&payload_in);
                }
                NBD_CMD_READ => {
                    reply.extend_from_slice(&data.lock().unwrap()[range]);
                }
                NBD_CMD_FLUSH => {}
                _ => unreachable!(),
            }

            queued_replies.push(reply);
            if queued_replies.len() >= reverse_batch.max(1) {
                for r in queued_replies.drain(..).rev() {
                    if byte_by_byte {
                        for b in &r {
                            if s.write_all(slice::from_ref(b)).is_err() {
                                return;
                            }
                        }
                    } else if s.write_all(&r).is_err() {
                        return;
                    }
                }
            }
        }
    }

    #[test]
    fn pipelined_out_of_order_chunked_and_short_reads() {
        for prefer_uring in [false, true] {
            let dir = TempDir::new().unwrap();
            let sock_path = dir.as_path().join("nbd.sock");
            let listener = UnixListener::bind(&sock_path).unwrap();
            let backing = Arc::new(Mutex::new(vec![0u8; TEST_SIZE]));

            let uri = format!("nbd+unix:///vol0?socket={}", sock_path.display());
            let server_backing = Arc::clone(&backing);
            // When io_uring is active, batch 2 chunks and reply in reverse order,
            // byte-by-byte, to exercise pipelined sends, out-of-order cookies,
            // short reads, and 16KiB chunk splitting.
            let use_uring = prefer_uring && uring_supported(false);
            let reverse_batch = if use_uring { 2 } else { 1 };
            let server_thread = thread::spawn(move || {
                let (stream, _) = listener.accept().unwrap();
                serve_connection(stream, &server_backing, reverse_batch, true, None);
            });

            let client = NbdClient::open(
                &uri,
                NbdClientOptions {
                    readonly: false,
                    num_queues: 1,
                    prefer_uring,
                    socket_override: None,
                },
            )
            .unwrap();
            let mut engine = client.new_engine().unwrap();

            // Write 32 KiB (splits into two 16 KiB chunks).
            let write_data: Vec<u8> = (0..32 * 1024).map(|i| (i % 251) as u8).collect();
            let write_iov = vec![libc::iovec {
                iov_base: write_data.as_ptr() as *mut libc::c_void,
                iov_len: write_data.len(),
            }];
            unsafe { engine.write_vectored(4096, write_iov, 101).unwrap() };
            let done = wait_completions(&mut engine, 1);
            assert_eq!(done, vec![(101, 32 * 1024)]);

            // Note: fsync is 1 chunk, so only run it when reverse_batch == 1, or send 2 flushes!
            engine.fsync(Some(102)).unwrap();
            if reverse_batch == 2 {
                engine.fsync(Some(103)).unwrap();
                let mut flushes = wait_completions(&mut engine, 2);
                flushes.sort_unstable();
                assert_eq!(flushes, vec![(102, 0), (103, 0)]);
            } else {
                assert_eq!(wait_completions(&mut engine, 1), vec![(102, 0)]);
            }

            // Read back into two scattered buffers.
            let mut part1 = vec![0u8; 20 * 1024];
            let mut part2 = vec![0u8; 12 * 1024];
            let read_iov = vec![
                libc::iovec {
                    iov_base: part1.as_mut_ptr().cast(),
                    iov_len: part1.len(),
                },
                libc::iovec {
                    iov_base: part2.as_mut_ptr().cast(),
                    iov_len: part2.len(),
                },
            ];
            unsafe { engine.read_vectored(4096, read_iov, 104).unwrap() };
            assert_eq!(wait_completions(&mut engine, 1), vec![(104, 32 * 1024)]);
            assert_eq!(&part1[..], &write_data[..20 * 1024]);
            assert_eq!(&part2[..], &write_data[20 * 1024..]);

            drop(engine);
            drop(client);
            server_thread.join().unwrap();
        }
    }

    #[test]
    fn transparent_reconnect_and_replay_mid_flight() {
        for prefer_uring in [false, true] {
            let dir = TempDir::new().unwrap();
            let sock_path = dir.as_path().join("reconnect.sock");
            let listener = UnixListener::bind(&sock_path).unwrap();
            let backing = Arc::new(Mutex::new(vec![0u8; TEST_SIZE]));

            let server_backing = Arc::clone(&backing);
            let server_thread = thread::spawn(move || {
                // First connection drops right after receiving the 2nd chunk!
                let (s1, _) = listener.accept().unwrap();
                serve_connection(s1, &server_backing, 1, false, Some(1));

                // Pause briefly to exercise backoff retry, then serve the reconnected client.
                thread::sleep(Duration::from_millis(40));
                let (s2, _) = listener.accept().unwrap();
                serve_connection(s2, &server_backing, 1, false, None);
            });

            let uri = format!(
                "nbd+unix:///vol0?socket={}&reconnect_timeout=3",
                sock_path.display()
            );
            let client = NbdClient::open(
                &uri,
                NbdClientOptions {
                    readonly: false,
                    num_queues: 1,
                    prefer_uring,
                    socket_override: None,
                },
            )
            .unwrap();
            let mut engine = client.new_engine().unwrap();

            // 32 KiB write splits into two 16 KiB chunks; server drops on chunk #2!
            let payload: Vec<u8> = (0..32 * 1024).map(|i| ((i * 7) & 0xff) as u8).collect();
            let w_iov = vec![libc::iovec {
                iov_base: payload.as_ptr() as *mut libc::c_void,
                iov_len: payload.len(),
            }];
            unsafe { engine.write_vectored(0, w_iov, 1).unwrap() };
            assert_eq!(wait_completions(&mut engine, 1), vec![(1, 32 * 1024)]);

            let mut readback = vec![0u8; 32 * 1024];
            let r_iov = vec![libc::iovec {
                iov_base: readback.as_mut_ptr().cast(),
                iov_len: readback.len(),
            }];
            unsafe { engine.read_vectored(0, r_iov, 2).unwrap() };
            assert_eq!(wait_completions(&mut engine, 1), vec![(2, 32 * 1024)]);
            assert_eq!(readback, payload);

            drop(engine);
            drop(client);
            server_thread.join().unwrap();
        }
    }
}

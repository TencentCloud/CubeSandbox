// Copyright 2026 Tencent Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//! `io_uring` pipelined NBD I/O engine with zero-copy scatter/gather and
//! automatic reconnect + in-flight request replay.

use std::collections::{HashMap, VecDeque};
use std::convert::TryInto;
use std::os::unix::io::AsRawFd;
use std::{io, mem};

use io_uring::{opcode, squeue, types, IoUring, Probe};
use log::{error, warn};
use vmm_sys_util::eventfd::EventFd;

use crate::conn::{advance_iovecs, iovecs_len, msghdr_for, sub_iovecs, NbdConn};
use crate::proto::*;
use crate::tracker::{Added, Chunk, Finished, Tracker};

const SEND_TAG: u64 = 1;
const RECV_TAG: u64 = 2;
const TIMEOUT_TAG: u64 = 3;
const RING_ENTRIES: u32 = 8;

/// Returns `true` when the running kernel supports the `io_uring` socket
/// opcodes required by [`NbdUringEngine`].
pub fn uring_supported(socket_timeout: bool) -> bool {
    let Ok(ring) = IoUring::new(1) else {
        return false;
    };
    let mut probe = Probe::new();
    ring.submitter().register_probe(&mut probe).is_ok()
        && probe.is_supported(opcode::SendMsg::CODE)
        && probe.is_supported(opcode::RecvMsg::CODE)
        && (!socket_timeout || probe.is_supported(opcode::LinkTimeout::CODE))
}

#[derive(Clone, Copy, Debug)]
enum Phase {
    Send { cookie: u64 },
    RecvHeader,
    RecvPayload { cookie: u64 },
}

/// Boxed state of one outstanding `SENDMSG` or `RECVMSG` so `header`, `iovs`,
/// and `msg` keep stable addresses while referenced by the kernel.
struct Transfer {
    header: [u8; NBD_REQUEST_SIZE],
    iovs: Vec<libc::iovec>,
    msg: libc::msghdr,
    phase: Phase,
}

unsafe impl Send for Transfer {}

impl Transfer {
    fn new(header: &[u8], payload: Vec<libc::iovec>, phase: Phase) -> Box<Self> {
        let mut t = Box::new(Transfer {
            header: [0; NBD_REQUEST_SIZE],
            iovs: Vec::new(),
            msg: unsafe { mem::zeroed() },
            phase,
        });
        t.header[..header.len()].copy_from_slice(header);
        let mut iovs = Vec::with_capacity(1 + payload.len());
        iovs.push(libc::iovec {
            iov_base: t.header.as_mut_ptr().cast(),
            iov_len: header.len(),
        });
        iovs.extend(payload);
        t.set_iovs(iovs);
        t
    }

    fn set_iovs(&mut self, iovs: Vec<libc::iovec>) {
        self.iovs = iovs;
        self.msg = msghdr_for(&mut self.iovs);
    }

    fn advance(&mut self, n: usize) -> bool {
        let mut iovs = mem::take(&mut self.iovs);
        advance_iovecs(&mut iovs, n);
        let more = iovecs_len(&iovs) > 0;
        self.set_iovs(iovs);
        more
    }
}

pub struct NbdUringEngine {
    conn: NbdConn,
    ring: IoUring,
    eventfd: EventFd,
    completions: Vec<(u64, i32)>,
    tracker: Tracker,
    chunks: HashMap<u64, Chunk>,
    queue: VecDeque<u64>,
    send: Option<Box<Transfer>>,
    recv: Option<Box<Transfer>>,
    deferred: Vec<(u64, i32)>,
    internal: HashMap<u64, i32>,
    timeout: Option<types::Timespec>,
}

impl NbdUringEngine {
    pub fn new(conn: NbdConn) -> io::Result<Self> {
        let ring = IoUring::new(RING_ENTRIES)?;
        let eventfd = EventFd::new(libc::EFD_NONBLOCK)?;
        ring.submitter().register_eventfd(eventfd.as_raw_fd())?;
        let timeout = conn.socket_timeout().map(|d| {
            types::Timespec::new()
                .sec(d.as_secs())
                .nsec(d.subsec_nanos())
        });
        Ok(Self {
            tracker: Tracker::new(conn.info()),
            timeout,
            conn,
            ring,
            eventfd,
            completions: Vec::new(),
            chunks: HashMap::new(),
            queue: VecDeque::new(),
            send: None,
            recv: None,
            deferred: Vec::new(),
            internal: HashMap::new(),
        })
    }

    pub fn notifier(&self) -> &EventFd {
        &self.eventfd
    }

    fn push_completion(&mut self, user_data: u64, result: i32) {
        self.completions.push((user_data, result));
        let _ = self.eventfd.write(1);
    }

    fn idle(&self) -> bool {
        self.send.is_none() && self.recv.is_none() && self.chunks.is_empty()
    }

    fn validate_bounds(&self, offset: u64, len: u64) -> io::Result<()> {
        let min_block = u64::from(self.conn.info().min_block);
        if offset % min_block != 0 || len % min_block != 0 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!(
                    "NBD I/O offset {offset} or length {len} not aligned to min_block {min_block}"
                ),
            ));
        }
        let end = offset.checked_add(len).ok_or_else(|| {
            io::Error::new(io::ErrorKind::InvalidInput, "NBD I/O offset overflow")
        })?;
        if end > self.conn.info().size {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!(
                    "NBD I/O [{offset}, {end}) exceeds export size {}",
                    self.conn.info().size
                ),
            ));
        }
        Ok(())
    }

    fn push_sqe(&mut self, entry: squeue::Entry) -> io::Result<()> {
        let result = match &self.timeout {
            Some(timeout) => unsafe {
                self.ring.submission().push_multiple(&[
                    entry.flags(squeue::Flags::IO_LINK),
                    opcode::LinkTimeout::new(timeout)
                        .build()
                        .user_data(TIMEOUT_TAG),
                ])
            },
            None => unsafe { self.ring.submission().push(&entry) },
        };
        result.map_err(|_| io::Error::other("io_uring submission queue is full"))
    }

    fn submit_send(&mut self) -> io::Result<()> {
        let t = self.send.as_ref().unwrap();
        let entry = opcode::SendMsg::new(types::Fd(self.conn.fd()), &t.msg)
            .flags(libc::MSG_NOSIGNAL as u32)
            .build()
            .user_data(SEND_TAG);
        self.push_sqe(entry).inspect_err(|_| self.send = None)
    }

    fn submit_recv(&mut self) -> io::Result<()> {
        let t = self.recv.as_mut().unwrap();
        let entry = opcode::RecvMsg::new(types::Fd(self.conn.fd()), &mut t.msg)
            .flags(libc::MSG_WAITALL as u32)
            .build()
            .user_data(RECV_TAG);
        self.push_sqe(entry).inspect_err(|_| self.recv = None)
    }

    fn socket_error(&self, res: i32) -> io::Error {
        if res == -libc::ECANCELED && self.timeout.is_some() {
            self.conn.timeout_error()
        } else {
            io::Error::from_raw_os_error(-res)
        }
    }

    fn deliver(&mut self, finished: Finished) {
        match finished {
            Finished::Completion { user_data, result } => self.push_completion(user_data, result),
            Finished::Internal { id, errno } => {
                self.internal.insert(id, errno);
            }
        }
    }

    fn finish_chunk(&mut self, cookie: u64, errno: i32) {
        if let Some(t) = &self.send {
            if matches!(t.phase, Phase::Send { cookie: c } if c == cookie) {
                self.deferred.push((cookie, errno));
                return;
            }
        }
        if let Some(chunk) = self.chunks.remove(&cookie) {
            if let Some(finished) = self.tracker.chunk_done(chunk, errno) {
                self.deliver(finished);
            }
        }
    }

    fn try_submit_pending(&mut self) -> io::Result<()> {
        if self.send.is_none() {
            while let Some(cookie) = self.queue.pop_front() {
                let Some(chunk) = self.chunks.get(&cookie).copied() else {
                    continue;
                };
                let header = NbdRequest {
                    flags: 0,
                    command: chunk.command,
                    cookie,
                    offset: chunk.offset,
                    length: chunk.length,
                }
                .encode();
                let payload = if chunk.command == NBD_CMD_WRITE {
                    sub_iovecs(
                        self.tracker.iovecs(chunk.id),
                        chunk.iov_start,
                        chunk.length as usize,
                    )
                } else {
                    Vec::new()
                };
                self.send = Some(Transfer::new(&header, payload, Phase::Send { cookie }));
                self.submit_send()?;
                break;
            }
        }
        if self.recv.is_none() && !self.chunks.is_empty() {
            self.recv = Some(Transfer::new(
                &[0; NBD_SIMPLE_REPLY_SIZE],
                Vec::new(),
                Phase::RecvHeader,
            ));
            self.submit_recv()?;
        }
        self.ring.submit()?;
        Ok(())
    }

    fn submit_pending(&mut self) {
        if let Err(e) = self.try_submit_pending() {
            self.recover_or_fail(&e);
        }
    }

    fn on_send(&mut self, res: i32) -> io::Result<()> {
        if res == -libc::EINTR || res == -libc::EAGAIN {
            return self.submit_send();
        }
        if res <= 0 {
            return Err(if res == 0 {
                io::Error::new(io::ErrorKind::WriteZero, "NBD socket accepted no data")
            } else {
                self.socket_error(res)
            });
        }
        if self.send.as_mut().unwrap().advance(res as usize) {
            return self.submit_send();
        }
        self.send = None;
        for (cookie, errno) in mem::take(&mut self.deferred) {
            self.finish_chunk(cookie, errno);
        }
        Ok(())
    }

    fn on_recv(&mut self, res: i32) -> io::Result<()> {
        if res == -libc::EINTR || res == -libc::EAGAIN {
            return self.submit_recv();
        }
        if res < 0 {
            return Err(self.socket_error(res));
        }
        if res == 0 {
            return Err(io::Error::new(
                io::ErrorKind::UnexpectedEof,
                "NBD server closed the connection",
            ));
        }
        let t = self.recv.as_mut().unwrap();
        if t.advance(res as usize) {
            return self.submit_recv();
        }

        match t.phase {
            Phase::RecvHeader => {
                let header: [u8; NBD_SIMPLE_REPLY_SIZE] =
                    t.header[..NBD_SIMPLE_REPLY_SIZE].try_into().unwrap();
                let reply = NbdSimpleReply::decode(&header).ok_or_else(|| {
                    io::Error::new(io::ErrorKind::InvalidData, "bad NBD reply magic")
                })?;
                let chunk = self.chunks.get(&reply.cookie).copied().ok_or_else(|| {
                    io::Error::new(
                        io::ErrorKind::InvalidData,
                        format!("unexpected NBD reply cookie {}", reply.cookie),
                    )
                })?;
                let errno = nbd_errno_to_errno(reply.error);
                if errno == libc::ESHUTDOWN && self.conn.reconnect_timeout().is_some() {
                    return Err(io::Error::from_raw_os_error(libc::ESHUTDOWN));
                }
                if chunk.command == NBD_CMD_READ && errno == 0 && chunk.length > 0 {
                    let payload = sub_iovecs(
                        self.tracker.iovecs(chunk.id),
                        chunk.iov_start,
                        chunk.length as usize,
                    );
                    let t = self.recv.as_mut().unwrap();
                    t.phase = Phase::RecvPayload {
                        cookie: reply.cookie,
                    };
                    t.set_iovs(payload);
                    return self.submit_recv();
                }
                self.recv = None;
                self.finish_chunk(reply.cookie, errno);
            }
            Phase::RecvPayload { cookie } => {
                self.recv = None;
                self.finish_chunk(cookie, 0);
            }
            Phase::Send { .. } => unreachable!(),
        }
        Ok(())
    }

    fn next_cqe(&mut self) -> Option<(u64, i32)> {
        self.ring
            .completion()
            .next()
            .map(|cqe| (cqe.user_data(), cqe.result()))
    }

    fn process_cqes(&mut self) -> bool {
        let mut any = false;
        while let Some((tag, res)) = self.next_cqe() {
            any = true;
            let result = match tag {
                SEND_TAG => self.on_send(res),
                RECV_TAG => self.on_recv(res),
                TIMEOUT_TAG => Ok(()),
                tag => unreachable!("unknown NBD io_uring tag {tag}"),
            };
            if let Err(e) = result {
                match tag {
                    SEND_TAG => self.send = None,
                    _ => self.recv = None,
                }
                self.recover_or_fail(&e);
                break;
            }
        }
        if any && !self.conn.is_dead() {
            self.submit_pending();
        }
        any
    }

    fn drain_kernel(&mut self) {
        while self.send.is_some() || self.recv.is_some() {
            if let Err(e) = self.ring.submit_and_wait(1) {
                if e.kind() == io::ErrorKind::Interrupted {
                    continue;
                }
                error!("Failed waiting for NBD io_uring completions, leaking buffers: {e}");
                mem::forget(self.send.take());
                mem::forget(self.recv.take());
                let tracker = Tracker::new(self.conn.info());
                mem::forget(mem::replace(&mut self.tracker, tracker));
                self.chunks.clear();
                return;
            }
            while let Some((tag, _)) = self.next_cqe() {
                match tag {
                    SEND_TAG => self.send = None,
                    RECV_TAG => self.recv = None,
                    _ => {}
                }
            }
        }
    }

    /// Attempts transparent reconnection and replay of all unfinished chunks when
    /// `reconnect_timeout` is configured; falls back to failing pending I/O otherwise.
    fn recover_or_fail(&mut self, e: &io::Error) {
        self.conn.mark_dead(e);
        self.drain_kernel();

        // Settle any replies that were already completely received before the drop.
        for (cookie, errno) in mem::take(&mut self.deferred) {
            self.finish_chunk(cookie, errno);
        }

        if self.conn.reconnect_timeout().is_some() {
            let mut replay_ok = false;
            if self.conn.try_reconnect() {
                self.chunks.clear();
                self.queue.clear();
                for chunk in self.tracker.remaining_chunks() {
                    let cookie = self.conn.next_cookie();
                    self.chunks.insert(cookie, chunk);
                    self.queue.push_back(cookie);
                }
                match self.try_submit_pending() {
                    Ok(()) => replay_ok = true,
                    Err(submit_err) => {
                        warn!("NBD replay submit failed after reconnect: {submit_err}");
                        self.conn.mark_dead(&submit_err);
                        self.drain_kernel();
                    }
                }
            }
            if replay_ok {
                return;
            }
        }

        self.queue.clear();
        let cookies: Vec<u64> = self.chunks.keys().copied().collect();
        for cookie in cookies {
            self.finish_chunk(cookie, libc::EIO);
        }
    }

    fn start(&mut self, added: Added) -> Option<u64> {
        match added {
            Added::Done(finished) => {
                self.deliver(finished);
                None
            }
            Added::Chunks(chunks) => {
                let id = chunks[0].id;
                if self.conn.is_dead() && !self.conn.try_reconnect() {
                    for chunk in chunks {
                        if let Some(finished) = self.tracker.chunk_done(chunk, libc::EIO) {
                            self.deliver(finished);
                        }
                    }
                    return Some(id);
                }
                for chunk in chunks {
                    let cookie = self.conn.next_cookie();
                    self.chunks.insert(cookie, chunk);
                    self.queue.push_back(cookie);
                }
                self.submit_pending();
                Some(id)
            }
        }
    }

    fn wait_internal(&mut self, id: u64) -> i32 {
        loop {
            if let Some(errno) = self.internal.remove(&id) {
                return errno;
            }
            if self.conn.is_dead() && !self.chunks.values().any(|c| c.id == id) {
                return libc::EIO;
            }
            match self.ring.submit_and_wait(1) {
                Ok(_) => {}
                Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                Err(e) => {
                    self.recover_or_fail(&e);
                    continue;
                }
            }
            self.process_cqes();
        }
    }

    /// Submits a scatter-gather read.
    ///
    /// # Safety
    ///
    /// `iovecs` must point to writable memory valid until `user_data` is returned
    /// by [`Self::complete`].
    pub unsafe fn read_vectored(
        &mut self,
        offset: u64,
        iovecs: Vec<libc::iovec>,
        user_data: u64,
    ) -> io::Result<()> {
        self.validate_bounds(offset, iovecs_len(&iovecs) as u64)?;
        let added = self.tracker.add_read(offset, iovecs, user_data);
        self.start(added);
        Ok(())
    }

    /// Submits a scatter-gather write.
    ///
    /// # Safety
    ///
    /// `iovecs` must point to readable memory valid until `user_data` is returned
    /// by [`Self::complete`].
    pub unsafe fn write_vectored(
        &mut self,
        offset: u64,
        iovecs: Vec<libc::iovec>,
        user_data: u64,
    ) -> io::Result<()> {
        self.validate_bounds(offset, iovecs_len(&iovecs) as u64)?;
        let added = self.tracker.add_write(offset, iovecs, user_data);
        self.start(added);
        Ok(())
    }

    pub fn fsync(&mut self, user_data: Option<u64>) -> io::Result<()> {
        if !self.conn.info().has_flag(NBD_FLAG_SEND_FLUSH) {
            if let Some(ud) = user_data {
                self.push_completion(ud, 0);
            }
            return Ok(());
        }
        let added = self.tracker.add_flush(user_data);
        match (user_data, self.start(added)) {
            (None, Some(id)) => match self.wait_internal(id) {
                0 => Ok(()),
                errno => Err(io::Error::from_raw_os_error(errno)),
            },
            _ => Ok(()),
        }
    }

    pub fn trim(&mut self, offset: u64, length: u64, user_data: u64) -> io::Result<()> {
        self.validate_bounds(offset, length)?;
        if !self.conn.info().has_flag(NBD_FLAG_SEND_TRIM) {
            self.push_completion(user_data, 0);
            return Ok(());
        }
        let added = self.tracker.add_trim(offset, length, user_data);
        self.start(added);
        Ok(())
    }

    pub fn write_zeroes(&mut self, offset: u64, length: u64, user_data: u64) -> io::Result<()> {
        self.validate_bounds(offset, length)?;
        if !self.conn.info().has_flag(NBD_FLAG_SEND_WRITE_ZEROES) {
            self.push_completion(user_data, -libc::EOPNOTSUPP);
            return Ok(());
        }
        let added = self.tracker.add_write_zeroes(offset, length, user_data);
        self.start(added);
        Ok(())
    }

    pub fn complete(&mut self) -> Vec<(u64, i32)> {
        while self.process_cqes() {}
        mem::take(&mut self.completions)
    }
}

impl Drop for NbdUringEngine {
    fn drop(&mut self) {
        if self.idle() {
            self.conn.disconnect();
        } else {
            self.conn.close();
            self.drain_kernel();
        }
    }
}

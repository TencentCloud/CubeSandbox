// Copyright 2026 Tencent Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//! Synchronous NBD I/O engine with `EventFd` completion signaling.

use std::io;
use std::mem;

use vmm_sys_util::eventfd::EventFd;

use crate::conn::{iovecs_len, sub_iovecs, NbdConn};
use crate::proto::*;
use crate::tracker::{Added, Chunk, Finished, Tracker};

pub struct NbdSyncEngine {
    conn: NbdConn,
    eventfd: EventFd,
    completions: Vec<(u64, i32)>,
    tracker: Tracker,
}

impl NbdSyncEngine {
    pub fn new(conn: NbdConn) -> io::Result<Self> {
        let eventfd = EventFd::new(libc::EFD_NONBLOCK)?;
        Ok(Self {
            tracker: Tracker::new(conn.info()),
            conn,
            eventfd,
            completions: Vec::new(),
        })
    }

    pub fn notifier(&self) -> &EventFd {
        &self.eventfd
    }

    fn push_completion(&mut self, user_data: u64, result: i32) {
        self.completions.push((user_data, result));
        let _ = self.eventfd.write(1);
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

    fn run_chunks(&mut self, chunks: Vec<Chunk>) -> Finished {
        let mut finished = None;
        for chunk in chunks {
            let payload = sub_iovecs(
                self.tracker.iovecs(chunk.id),
                chunk.iov_start,
                if matches!(chunk.command, NBD_CMD_READ | NBD_CMD_WRITE) {
                    chunk.length as usize
                } else {
                    0
                },
            );
            let errno = unsafe {
                self.conn
                    .command_iov(chunk.command, chunk.offset, chunk.length, &payload)
            }
            .unwrap_or(libc::EIO);
            finished = self.tracker.chunk_done(chunk, errno);
        }
        finished.expect("request finishes with its last chunk")
    }

    fn execute(&mut self, added: Added) -> i32 {
        let finished = match added {
            Added::Chunks(chunks) => self.run_chunks(chunks),
            Added::Done(finished) => finished,
        };
        match finished {
            Finished::Completion { user_data, result } => {
                self.push_completion(user_data, result);
                result
            }
            Finished::Internal { errno, .. } => -errno,
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
        self.execute(added);
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
        self.execute(added);
        Ok(())
    }

    /// Submits an `NBD_CMD_FLUSH`. When `user_data` is `None`, blocks and returns
    /// any flush error directly.
    pub fn fsync(&mut self, user_data: Option<u64>) -> io::Result<()> {
        if !self.conn.info().has_flag(NBD_FLAG_SEND_FLUSH) {
            if let Some(ud) = user_data {
                self.push_completion(ud, 0);
            }
            return Ok(());
        }
        let added = self.tracker.add_flush(user_data);
        let result = self.execute(added);
        if user_data.is_none() && result < 0 {
            return Err(io::Error::from_raw_os_error(-result));
        }
        Ok(())
    }

    /// Submits an `NBD_CMD_TRIM` (discard/punch-hole).
    pub fn trim(&mut self, offset: u64, length: u64, user_data: u64) -> io::Result<()> {
        self.validate_bounds(offset, length)?;
        if !self.conn.info().has_flag(NBD_FLAG_SEND_TRIM) {
            self.push_completion(user_data, 0);
            return Ok(());
        }
        let added = self.tracker.add_trim(offset, length, user_data);
        self.execute(added);
        Ok(())
    }

    /// Submits an `NBD_CMD_WRITE_ZEROES`.
    pub fn write_zeroes(&mut self, offset: u64, length: u64, user_data: u64) -> io::Result<()> {
        self.validate_bounds(offset, length)?;
        if !self.conn.info().has_flag(NBD_FLAG_SEND_WRITE_ZEROES) {
            self.push_completion(user_data, -libc::EOPNOTSUPP);
            return Ok(());
        }
        let added = self.tracker.add_write_zeroes(offset, length, user_data);
        self.execute(added);
        Ok(())
    }

    pub fn complete(&mut self) -> Vec<(u64, i32)> {
        mem::take(&mut self.completions)
    }
}

impl Drop for NbdSyncEngine {
    fn drop(&mut self) {
        self.conn.disconnect();
    }
}

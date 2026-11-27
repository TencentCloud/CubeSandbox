// Copyright 2026 Tencent Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//! Splitting of block requests into NBD commands and aggregation of replies.

use std::collections::HashMap;

use crate::conn::iovecs_len;
use crate::handshake::NbdExportInfo;
use crate::proto::*;

/// One NBD wire command that forms part of a higher-level request.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) struct Chunk {
    /// Identifier of the parent request in [`Tracker`].
    pub(crate) id: u64,
    /// Index of this chunk within its parent request (`0..total_chunks`).
    pub(crate) chunk_idx: usize,
    pub(crate) command: u16,
    pub(crate) offset: u64,
    pub(crate) length: u32,
    /// Byte offset of this chunk's payload within the parent operation's `iovecs`.
    pub(crate) iov_start: usize,
}

struct Pending {
    user_data: Option<u64>,
    iovecs: Vec<libc::iovec>,
    total_len: usize,
    is_data: bool,
    remaining: Vec<Chunk>,
    errno: i32,
}

// SAFETY: Callers of `read_vectored` / `write_vectored` guarantee the memory
// referenced by `iovecs` remains valid and uniquely accessible until completion.
unsafe impl Send for Pending {}

impl Pending {
    fn new(
        user_data: Option<u64>,
        iovecs: Vec<libc::iovec>,
        is_data: bool,
        chunks: Vec<Chunk>,
    ) -> Self {
        let total_len = iovecs_len(&iovecs);
        Self {
            user_data,
            iovecs,
            total_len,
            is_data,
            remaining: chunks,
            errno: 0,
        }
    }

    fn finish(self, id: u64) -> Finished {
        let result = if self.errno != 0 {
            -self.errno
        } else if self.is_data {
            self.total_len as i32
        } else {
            0
        };
        match self.user_data {
            Some(user_data) => Finished::Completion { user_data, result },
            None => Finished::Internal {
                id,
                errno: self.errno,
            },
        }
    }
}

/// Outcome of a fully replied request.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Finished {
    Completion { user_data: u64, result: i32 },
    Internal { id: u64, errno: i32 },
}

/// Result of submitting a request to [`Tracker`].
pub(crate) enum Added {
    Chunks(Vec<Chunk>),
    Done(Finished),
}

pub(crate) struct Tracker {
    next_id: u64,
    pending: HashMap<u64, Pending>,
    max_data: u32,
    max_command: u32,
}

fn align_down(limit: u32, align: u32) -> u32 {
    limit / align * align
}

impl Tracker {
    pub(crate) fn new(info: &NbdExportInfo) -> Self {
        // Cap single READ/WRITE payload chunks at NBD_DEFAULT_MAX_PAYLOAD (32 MiB),
        // safely below nbdkit's MAX_REQUEST_SIZE (64 MiB), even if a plugin advertises
        // max_block == u32::MAX (-1).
        let capped_max = info.max_block.min(NBD_DEFAULT_MAX_PAYLOAD);
        Self {
            next_id: 0,
            pending: HashMap::new(),
            max_data: align_down(capped_max, info.min_block).max(info.min_block),
            max_command: align_down(u32::MAX, info.min_block).max(info.min_block),
        }
    }

    fn next_id(&mut self) -> u64 {
        let id = self.next_id;
        self.next_id = self.next_id.wrapping_add(1);
        id
    }

    fn add_split(
        &mut self,
        user_data: u64,
        iovecs: Vec<libc::iovec>,
        is_data: bool,
        command: u16,
        offset: u64,
        length: u64,
        max_chunk: u32,
    ) -> Added {
        let id = self.next_id();
        let mut chunks = Vec::new();
        let mut done = 0u64;
        let mut idx = 0usize;
        while done < length {
            let len = (length - done).min(max_chunk as u64) as u32;
            chunks.push(Chunk {
                id,
                chunk_idx: idx,
                command,
                offset: offset + done,
                length: len,
                iov_start: done as usize,
            });
            done += len as u64;
            idx += 1;
        }
        if chunks.is_empty() {
            let pending = Pending::new(Some(user_data), iovecs, is_data, Vec::new());
            return Added::Done(pending.finish(id));
        }
        let pending = Pending::new(Some(user_data), iovecs, is_data, chunks.clone());
        self.pending.insert(id, pending);
        Added::Chunks(chunks)
    }

    pub(crate) fn add_read(
        &mut self,
        offset: u64,
        iovecs: Vec<libc::iovec>,
        user_data: u64,
    ) -> Added {
        let length = iovecs_len(&iovecs) as u64;
        let max = self.max_data;
        self.add_split(user_data, iovecs, true, NBD_CMD_READ, offset, length, max)
    }

    pub(crate) fn add_write(
        &mut self,
        offset: u64,
        iovecs: Vec<libc::iovec>,
        user_data: u64,
    ) -> Added {
        let length = iovecs_len(&iovecs) as u64;
        let max = self.max_data;
        self.add_split(user_data, iovecs, true, NBD_CMD_WRITE, offset, length, max)
    }

    pub(crate) fn add_flush(&mut self, user_data: Option<u64>) -> Added {
        let id = self.next_id();
        let chunk = Chunk {
            id,
            chunk_idx: 0,
            command: NBD_CMD_FLUSH,
            offset: 0,
            length: 0,
            iov_start: 0,
        };
        let pending = Pending::new(user_data, Vec::new(), false, vec![chunk]);
        self.pending.insert(id, pending);
        Added::Chunks(vec![chunk])
    }

    pub(crate) fn add_trim(&mut self, offset: u64, length: u64, user_data: u64) -> Added {
        let max = self.max_command;
        self.add_split(
            user_data,
            Vec::new(),
            false,
            NBD_CMD_TRIM,
            offset,
            length,
            max,
        )
    }

    pub(crate) fn add_write_zeroes(&mut self, offset: u64, length: u64, user_data: u64) -> Added {
        let max = self.max_command;
        self.add_split(
            user_data,
            Vec::new(),
            false,
            NBD_CMD_WRITE_ZEROES,
            offset,
            length,
            max,
        )
    }

    pub(crate) fn iovecs(&self, id: u64) -> &[libc::iovec] {
        self.pending.get(&id).map_or(&[], |p| p.iovecs.as_slice())
    }

    /// Returns all chunks across all pending requests that have not yet completed,
    /// ordered by `(request_id, chunk_idx)` for deterministic replay after reconnect.
    pub(crate) fn remaining_chunks(&self) -> Vec<Chunk> {
        let mut ids: Vec<u64> = self.pending.keys().copied().collect();
        ids.sort_unstable();
        let mut out = Vec::new();
        for id in ids {
            if let Some(p) = self.pending.get(&id) {
                out.extend(p.remaining.iter().copied());
            }
        }
        out
    }

    /// Marks one chunk complete and returns the aggregated outcome when the last
    /// chunk of request `id` finishes.
    pub(crate) fn chunk_done(&mut self, chunk: Chunk, errno: i32) -> Option<Finished> {
        let pending = self.pending.get_mut(&chunk.id)?;
        let pos = pending
            .remaining
            .iter()
            .position(|c| c.chunk_idx == chunk.chunk_idx)?;
        pending.remaining.remove(pos);
        if pending.errno == 0 {
            pending.errno = errno;
        }
        if !pending.remaining.is_empty() {
            return None;
        }
        Some(self.pending.remove(&chunk.id).unwrap().finish(chunk.id))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn info(min: u32, max: u32) -> NbdExportInfo {
        NbdExportInfo {
            size: 1 << 30,
            flags: NBD_FLAG_HAS_FLAGS,
            min_block: min,
            opt_block: 4096,
            max_block: max,
        }
    }

    #[test]
    fn data_split_aggregate_and_replay_remaining() {
        let mut t = Tracker::new(&info(512, 4000));
        let mut buf = vec![0u8; 10000];
        let iov = vec![libc::iovec {
            iov_base: buf.as_mut_ptr().cast(),
            iov_len: buf.len(),
        }];
        let Added::Chunks(chunks) = t.add_write(4096, iov, 7) else {
            panic!("expected chunks");
        };
        let lens: Vec<_> = chunks.iter().map(|c| c.length).collect();
        assert_eq!(lens, vec![3584, 3584, 2832]);
        assert_eq!(chunks[1].offset, 4096 + 3584);
        assert_eq!(chunks[2].iov_start, 7168);

        // Complete chunk 0; remaining_chunks should only return chunks 1 and 2.
        assert!(t.chunk_done(chunks[0], 0).is_none());
        let replay = t.remaining_chunks();
        assert_eq!(replay.len(), 2);
        assert_eq!(replay[0].chunk_idx, 1);
        assert_eq!(replay[1].chunk_idx, 2);

        assert!(t.chunk_done(chunks[1], libc::ENOSPC).is_none());
        assert_eq!(
            t.chunk_done(chunks[2], libc::EIO),
            Some(Finished::Completion {
                user_data: 7,
                result: -libc::ENOSPC,
            })
        );
    }

    #[test]
    fn read_success_reports_total_length() {
        let mut t = Tracker::new(&info(1, 1 << 20));
        let mut buf = vec![0u8; 512];
        let iov = vec![libc::iovec {
            iov_base: buf.as_mut_ptr().cast(),
            iov_len: buf.len(),
        }];
        let Added::Chunks(chunks) = t.add_read(0, iov, 3) else {
            panic!("expected chunks");
        };
        assert_eq!(chunks.len(), 1);
        assert_eq!(
            t.chunk_done(chunks[0], 0),
            Some(Finished::Completion {
                user_data: 3,
                result: 512,
            })
        );
    }
}

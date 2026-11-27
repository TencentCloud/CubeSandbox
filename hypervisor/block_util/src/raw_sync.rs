// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause

use crate::async_io::{
    block_discard_limits, is_regular_xfs_or_juicefs_file, AsyncIo, AsyncIoError, AsyncIoResult,
    DiskFile, DiskFileError, DiskFileResult, DiskTopology,
};
use crate::discard::DiscardSender;
use crate::MAX_DISCARD_SECTORS;
use std::fs::File;
use std::io::{self, Seek, SeekFrom};
use std::os::fd::BorrowedFd;
use std::os::unix::io::{AsRawFd, RawFd};
use std::sync::Arc;
use vmm_sys_util::eventfd::EventFd;

fn run_vectored_io<F>(
    mut iovecs: Vec<libc::iovec>,
    mut offset: libc::off_t,
    write: bool,
    mut operation: F,
) -> io::Result<usize>
where
    F: FnMut(&[libc::iovec], libc::off_t) -> io::Result<usize>,
{
    let mut first = 0;
    let mut total = 0usize;

    loop {
        while first < iovecs.len() && iovecs[first].iov_len == 0 {
            first += 1;
        }
        if first == iovecs.len() {
            return Ok(total);
        }

        let count = match operation(&iovecs[first..], offset) {
            Err(err) if err.kind() == io::ErrorKind::Interrupted => continue,
            result => result?,
        };
        if count == 0 {
            if write {
                return Err(io::Error::new(
                    io::ErrorKind::WriteZero,
                    "failed to write whole buffer",
                ));
            }
            return Ok(total);
        }

        let remaining = iovecs[first..]
            .iter()
            .try_fold(0usize, |remaining, iovec| {
                remaining.checked_add(iovec.iov_len)
            })
            .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "byte count overflow"))?;
        if count > remaining {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "vectored I/O returned more bytes than requested",
            ));
        }

        total = total
            .checked_add(count)
            .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "byte count overflow"))?;
        let count_offset = libc::off_t::try_from(count)
            .map_err(|_| io::Error::new(io::ErrorKind::InvalidInput, "offset overflow"))?;
        offset = offset
            .checked_add(count_offset)
            .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "offset overflow"))?;

        let mut completed = count;
        while completed > 0 {
            if completed >= iovecs[first].iov_len {
                completed -= iovecs[first].iov_len;
                first += 1;
            } else {
                iovecs[first].iov_base =
                    unsafe { iovecs[first].iov_base.cast::<u8>().add(completed).cast() };
                iovecs[first].iov_len -= completed;
                completed = 0;
            }
        }
    }
}

pub struct RawFileDiskSync {
    file: File,
    discard: bool,
    block_discard: bool,
    discard_sector_alignment: u32,
    max_discard_sectors: u32,
}

impl RawFileDiskSync {
    // Probe once here so creation-time validation, the alignment hint, the
    // advertised maximum and Block::new all agree.
    pub fn with_discard(mut self, enabled: bool) -> Self {
        let limits = enabled.then(|| block_discard_limits(&self.file)).flatten();
        self.block_discard = limits.is_some();
        self.discard =
            enabled && (self.block_discard || is_regular_xfs_or_juicefs_file(&self.file));
        if let Some(limits) = limits {
            self.discard_sector_alignment = limits.alignment_sectors;
            self.max_discard_sectors = limits.max_sectors;
        }
        self
    }
    pub fn new(file: File) -> Self {
        RawFileDiskSync {
            file,
            discard: false,
            block_discard: false,
            discard_sector_alignment: 8,
            max_discard_sectors: MAX_DISCARD_SECTORS,
        }
    }
}

impl DiskFile for RawFileDiskSync {
    fn size(&mut self) -> DiskFileResult<u64> {
        self.file
            .seek(SeekFrom::End(0))
            .map_err(DiskFileError::Size)
    }

    fn new_async_io(&self, ring_depth: u32) -> DiskFileResult<Box<dyn AsyncIo>> {
        Ok(Box::new(
            RawFileSync::new(self.file.as_raw_fd())
                .with_block_discard(self.block_discard)
                .with_file_discard(self.discard && !self.block_discard)
                .with_discard_max(self.max_discard_sectors)
                .with_discard_capacity(ring_depth as usize),
        ) as Box<dyn AsyncIo>)
    }

    fn supports_discard(&self) -> bool {
        self.discard
    }

    fn discard_sector_alignment(&self) -> u32 {
        self.discard_sector_alignment
    }

    fn max_discard_sectors(&self) -> u32 {
        self.max_discard_sectors
    }

    fn topology(&mut self) -> DiskTopology {
        if let Ok(topology) = DiskTopology::probe(&mut self.file) {
            topology
        } else {
            warn!("Unable to get device topology. Using default topology");
            DiskTopology::default()
        }
    }
}

fn punch_hole(fd: RawFd, offset: u64, len: u64) -> io::Result<()> {
    loop {
        // SAFETY: `fd` is either the worker-owned duplicate or the live backend
        // descriptor, and the discard range was validated before submission.
        if unsafe {
            libc::fallocate(
                fd,
                libc::FALLOC_FL_PUNCH_HOLE | libc::FALLOC_FL_KEEP_SIZE,
                offset as libc::off_t,
                len as libc::off_t,
            )
        } == 0
        {
            return Ok(());
        }
        let error = io::Error::last_os_error();
        if error.kind() != io::ErrorKind::Interrupted {
            return Err(error);
        }
    }
}

pub struct RawFileSync {
    fd: RawFd,
    eventfd: Arc<EventFd>,
    completion_list: Vec<(u64, i32)>,
    block_discard: bool,
    file_discard: bool,
    max_discard_sectors: u32,
    discard_capacity: usize,
    discard_sender: Option<DiscardSender>,
}

impl RawFileSync {
    pub fn new(fd: RawFd) -> Self {
        RawFileSync {
            fd,
            eventfd: Arc::new(
                EventFd::new(libc::EFD_NONBLOCK).expect("Failed creating EventFd for RawFile"),
            ),
            completion_list: Vec::new(),
            block_discard: false,
            file_discard: false,
            max_discard_sectors: MAX_DISCARD_SECTORS,
            discard_capacity: 0,
            discard_sender: None,
        }
    }

    fn with_block_discard(mut self, block: bool) -> Self {
        self.block_discard = block;
        self
    }

    pub(crate) fn with_file_discard(mut self, file: bool) -> Self {
        self.file_discard = file;
        self
    }

    fn with_discard_max(mut self, max_sectors: u32) -> Self {
        self.max_discard_sectors = max_sectors;
        self
    }

    pub(crate) fn with_discard_capacity(mut self, capacity: usize) -> Self {
        self.discard_capacity = capacity.max(1);
        self
    }
}

impl AsyncIo for RawFileSync {
    fn notifier(&self) -> &EventFd {
        &self.eventfd
    }

    fn start_discard_worker(
        &mut self,
    ) -> io::Result<Option<Box<dyn FnOnce() -> io::Result<()> + Send>>> {
        if !self.block_discard && !self.file_discard {
            return Ok(None);
        }
        // The worker owns a duplicated descriptor so it can outlive every
        // borrow of the original, and duplicate before any seccomp filter.
        // SAFETY: `self.fd` stays open for the lifetime of this backend.
        let fd = unsafe { BorrowedFd::borrow_raw(self.fd) }.try_clone_to_owned()?;
        let sender = DiscardSender::new(
            self.discard_capacity,
            self.max_discard_sectors,
            Arc::clone(&self.eventfd),
        );
        let worker = sender.worker(fd);
        self.discard_sender = Some(sender);
        let block_discard = self.block_discard;
        Ok(Some(Box::new(move || {
            if block_discard {
                worker.run()
            } else {
                worker.run_with(punch_hole)
            }
        })))
    }

    fn read_vectored(
        &mut self,
        offset: libc::off_t,
        iovecs: Vec<libc::iovec>,
        user_data: u64,
    ) -> AsyncIoResult<()> {
        let result = run_vectored_io(iovecs, offset, false, |iovecs, offset| {
            let result = unsafe {
                libc::preadv(
                    self.fd as libc::c_int,
                    iovecs.as_ptr(),
                    iovecs.len() as libc::c_int,
                    offset,
                )
            };
            if result < 0 {
                Err(io::Error::last_os_error())
            } else {
                Ok(result as usize)
            }
        })
        .map_err(AsyncIoError::ReadVectored)?;
        let result = i32::try_from(result).map_err(|_| {
            AsyncIoError::ReadVectored(io::Error::new(
                io::ErrorKind::InvalidData,
                "byte count exceeds completion range",
            ))
        })?;

        self.completion_list.push((user_data, result));
        self.eventfd.write(1).unwrap();

        Ok(())
    }

    fn write_vectored(
        &mut self,
        offset: libc::off_t,
        iovecs: Vec<libc::iovec>,
        user_data: u64,
    ) -> AsyncIoResult<()> {
        let result = run_vectored_io(iovecs, offset, true, |iovecs, offset| {
            let result = unsafe {
                libc::pwritev(
                    self.fd as libc::c_int,
                    iovecs.as_ptr(),
                    iovecs.len() as libc::c_int,
                    offset,
                )
            };
            if result < 0 {
                Err(io::Error::last_os_error())
            } else {
                Ok(result as usize)
            }
        })
        .map_err(AsyncIoError::WriteVectored)?;
        let result = i32::try_from(result).map_err(|_| {
            AsyncIoError::WriteVectored(io::Error::new(
                io::ErrorKind::InvalidData,
                "byte count exceeds completion range",
            ))
        })?;

        self.completion_list.push((user_data, result));
        self.eventfd.write(1).unwrap();

        Ok(())
    }

    fn fsync(&mut self, user_data: Option<u64>) -> AsyncIoResult<()> {
        let result = unsafe { libc::fsync(self.fd as libc::c_int) };
        if result < 0 {
            return Err(AsyncIoError::Fsync(std::io::Error::last_os_error()));
        }

        if let Some(user_data) = user_data {
            self.completion_list.push((user_data, result));
            self.eventfd.write(1).unwrap();
        }

        Ok(())
    }

    fn discard(
        &mut self,
        offset: libc::off_t,
        len: libc::off_t,
        user_data: u64,
    ) -> AsyncIoResult<()> {
        if self.block_discard || self.file_discard {
            // Both BLKDISCARD and PUNCH_HOLE are synchronous; the dedicated
            // worker runs them instead of stalling this queue.
            let sender = self.discard_sender.as_ref().ok_or_else(|| {
                AsyncIoError::DiscardRejected(std::io::Error::from_raw_os_error(libc::ENOSYS))
            })?;
            return sender
                .submit(offset as u64, len as u64, user_data)
                .map_err(AsyncIoError::DiscardRejected);
        }

        // Legacy direct callers submit without a worker, so this branch runs
        // inline; the virtio path never reaches it without discard advertised.
        let status = punch_hole(self.fd, offset as u64, len as u64)
            .err()
            .map_or(0, |error| -error.raw_os_error().unwrap_or(libc::EIO));
        // Notifier first: if it fails, no completion may be retained.
        self.eventfd
            .write(1)
            .map_err(AsyncIoError::DiscardRejected)?;
        self.completion_list.push((user_data, status));
        Ok(())
    }

    fn complete(&mut self) -> Vec<(u64, i32)> {
        let mut completion_list: Vec<(u64, i32)> = self.completion_list.drain(..).collect();
        if let Some(sender) = &self.discard_sender {
            completion_list.extend(sender.drain());
        }
        completion_list
    }
}

#[cfg(test)]
mod tests {
    use super::run_vectored_io;
    use crate::async_io::AsyncIo;
    use std::io;
    use std::time::{Duration, Instant};

    fn start_file_worker(backend: &mut super::RawFileSync) -> std::thread::JoinHandle<()> {
        let runner = backend
            .start_discard_worker()
            .unwrap()
            .expect("file discard backend must expose a worker");
        std::thread::spawn(move || runner().unwrap())
    }

    fn wait_completions(backend: &mut super::RawFileSync) -> Vec<(u64, i32)> {
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            let completions = backend.complete();
            if !completions.is_empty() {
                return completions;
            }
            assert!(
                Instant::now() < deadline,
                "timed out waiting for the file discard completion"
            );
            std::thread::sleep(Duration::from_millis(1));
        }
    }

    fn iovec(buf: &mut [u8]) -> libc::iovec {
        libc::iovec {
            iov_base: buf.as_mut_ptr().cast(),
            iov_len: buf.len(),
        }
    }

    #[test]
    fn test_run_vectored_io_advances_partial_operations() {
        let mut first = [0; 3];
        let mut second = [0; 4];
        let mut third = [0; 2];
        let first_ptr = first.as_mut_ptr() as usize;
        let second_ptr = second.as_mut_ptr() as usize;
        let mut calls = Vec::new();
        let mut results = [2, 3, 4].into_iter();

        let result = run_vectored_io(
            vec![iovec(&mut first), iovec(&mut second), iovec(&mut third)],
            7,
            false,
            |iovecs, offset| {
                calls.push((
                    offset,
                    iovecs
                        .iter()
                        .map(|iov| (iov.iov_base as usize, iov.iov_len))
                        .collect::<Vec<_>>(),
                ));
                Ok(results.next().unwrap())
            },
        )
        .unwrap();

        assert_eq!(result, 9);
        assert_eq!(
            calls[0],
            (
                7,
                vec![
                    (first_ptr, 3),
                    (second_ptr, 4),
                    (third.as_mut_ptr() as usize, 2)
                ]
            )
        );
        assert_eq!(
            calls[1],
            (
                9,
                vec![
                    (first_ptr + 2, 1),
                    (second_ptr, 4),
                    (third.as_mut_ptr() as usize, 2)
                ]
            )
        );
        assert_eq!(
            calls[2],
            (
                12,
                vec![(second_ptr + 2, 2), (third.as_mut_ptr() as usize, 2)]
            )
        );
    }

    #[test]
    fn test_run_vectored_io_stops_read_at_eof() {
        let mut buf = [0; 4];
        let mut results = [2, 0].into_iter();

        let result = run_vectored_io(vec![iovec(&mut buf)], 0, false, |_, _| {
            Ok(results.next().unwrap())
        })
        .unwrap();

        assert_eq!(result, 2);
    }

    #[test]
    fn test_run_vectored_io_rejects_write_zero() {
        let mut buf = [0; 4];

        let err = run_vectored_io(vec![iovec(&mut buf)], 0, true, |_, _| Ok(0)).unwrap_err();

        assert_eq!(err.kind(), io::ErrorKind::WriteZero);
    }

    #[test]
    fn test_run_vectored_io_retries_interrupted_operation() {
        let mut buf = [0; 4];
        let base = buf.as_mut_ptr() as usize;
        let mut calls = Vec::new();

        let result = run_vectored_io(vec![iovec(&mut buf)], 5, false, |iovecs, offset| {
            calls.push((offset, iovecs[0].iov_base as usize, iovecs[0].iov_len));
            if calls.len() == 1 {
                Err(io::Error::from(io::ErrorKind::Interrupted))
            } else {
                Ok(4)
            }
        })
        .unwrap();

        assert_eq!(result, 4);
        assert_eq!(calls, vec![(5, base, 4), (5, base, 4)]);
    }

    #[test]
    fn test_run_vectored_io_ignores_empty_iovecs() {
        let mut empty = [];
        let mut calls = 0;

        let result = run_vectored_io(vec![iovec(&mut empty)], 0, true, |_, _| {
            calls += 1;
            Ok(0)
        })
        .unwrap();

        assert_eq!(result, 0);
        assert_eq!(calls, 0);
    }

    // Real kernel path: fallocate(PUNCH_HOLE) on an fd opened read-only fails
    // with -EBADF. The worker must surface that as a negative completion (never
    // as a fake success) and leave the backing file untouched. This starts the
    // low-level runner explicitly, so it does not depend on the product
    // XFS/JuiceFS capability gate or a special host volume.
    #[test]
    fn test_file_discard_on_read_only_file_reports_negative_completion() {
        use std::io::{Read, Seek, SeekFrom, Write};
        use std::os::unix::io::AsRawFd;

        let temp = vmm_sys_util::tempfile::TempFile::new().unwrap();
        let mut writable = temp.as_file().try_clone().unwrap();
        writable.write_all(&[0xa5; 8192]).unwrap();
        writable.sync_all().unwrap();

        let read_only =
            std::fs::File::open(format!("/proc/self/fd/{}", writable.as_raw_fd())).unwrap();
        let mut backend = super::RawFileSync::new(read_only.as_raw_fd())
            .with_file_discard(true)
            .with_discard_capacity(8);
        let worker = start_file_worker(&mut backend);

        backend
            .discard(0, 8192, 7)
            .expect("fallocate failure must be reported via completion, not Err");

        let completions = wait_completions(&mut backend);
        assert_eq!(completions.len(), 1);
        assert_eq!(completions[0].0, 7);
        assert!(
            completions[0].1 < 0,
            "read-only fallocate must yield a negative completion: {completions:?}"
        );
        assert!(backend.notifier().read().unwrap() > 0);

        assert_eq!(writable.metadata().unwrap().len(), 8192);
        writable.seek(SeekFrom::Start(0)).unwrap();
        let mut data = Vec::new();
        writable.read_to_end(&mut data).unwrap();
        assert_eq!(data, vec![0xa5; 8192]);

        drop(backend);
        worker.join().unwrap();
    }

    // The shared worker tolerates a saturated nonblocking notifier: the
    // completion is still retained and drained. This is the semantics the
    // synchronous file path inherits now that it uses the same worker.
    #[test]
    fn test_file_discard_saturated_notifier_retains_completion() {
        use std::os::unix::io::AsRawFd;

        let temp = vmm_sys_util::tempfile::TempFile::new().unwrap();
        let mut backend = super::RawFileSync::new(temp.as_file().as_raw_fd())
            .with_file_discard(true)
            .with_discard_capacity(8);
        let notifier = backend.notifier();
        assert!(notifier.write(u64::MAX - 1).is_ok());
        while notifier.write(1).is_ok() {}
        let worker = start_file_worker(&mut backend);

        backend
            .discard(0, 512, 13)
            .expect("a saturated notifier must not reject a queued discard");

        let completions = wait_completions(&mut backend);
        assert_eq!(completions.len(), 1, "the completion must be retained");
        assert_eq!(completions[0].0, 13);

        drop(backend);
        worker.join().unwrap();
    }

    // A pending file discard must sit in the worker queue: it may not execute
    // inline, touch the file, or arm the notifier before the runner starts.
    // Ordinary reads and writes must keep completing meanwhile.
    #[test]
    fn test_file_discard_queues_without_blocking_read_write() {
        use std::io::Write;
        use std::os::unix::fs::FileExt;
        use std::os::unix::io::AsRawFd;

        let temp = vmm_sys_util::tempfile::TempFile::new().unwrap();
        let mut file = temp.as_file().try_clone().unwrap();
        let body = vec![0xa5u8; 16384];
        file.write_all(&body).unwrap();
        file.sync_all().unwrap();

        let mut backend = super::RawFileSync::new(file.as_raw_fd())
            .with_file_discard(true)
            .with_discard_capacity(8);
        // Request the worker but hold the runner: the discard stays queued
        // while the queue-side reads and writes below must still complete.
        let runner = backend
            .start_discard_worker()
            .unwrap()
            .expect("file discard backend must expose a worker");

        backend.discard(0, 4096, 11).unwrap();
        assert!(
            backend.complete().is_empty(),
            "a queued discard must not execute inline"
        );
        assert_eq!(
            backend.notifier().read().unwrap_err().kind(),
            io::ErrorKind::WouldBlock,
            "a queued discard must not arm the notifier"
        );
        let mut pending = [0u8; 4096];
        file.read_exact_at(&mut pending, 0).unwrap();
        assert_eq!(
            pending, [0xa5; 4096],
            "the pending discard must not touch the file"
        );

        let write = [0x5au8; 4096];
        let iovec = libc::iovec {
            iov_base: write.as_ptr() as *mut libc::c_void,
            iov_len: write.len(),
        };
        backend.write_vectored(8192, vec![iovec], 12).unwrap();
        let mut read = [0u8; 4096];
        let iovec = libc::iovec {
            iov_base: read.as_mut_ptr().cast(),
            iov_len: read.len(),
        };
        backend.read_vectored(8192, vec![iovec], 13).unwrap();
        assert_eq!(
            backend.complete(),
            vec![(12, 4096), (13, 4096)],
            "reads and writes must complete while a discard is queued"
        );
        assert_eq!(read, write);

        let worker = std::thread::spawn(move || runner().unwrap());
        assert_eq!(wait_completions(&mut backend), vec![(11, 0)]);

        let mut after = vec![0u8; 16384];
        file.read_exact_at(&mut after, 0).unwrap();
        assert!(after[..4096].iter().all(|byte| *byte == 0));
        assert!(after[4096..8192].iter().all(|byte| *byte == 0xa5));
        assert_eq!(&after[8192..12288], &write[..]);
        assert!(after[12288..].iter().all(|byte| *byte == 0xa5));

        drop(backend);
        worker.join().unwrap();
    }

    #[test]
    fn test_file_discard_without_opt_in_starts_no_worker() {
        use std::os::unix::io::AsRawFd;

        let temp = vmm_sys_util::tempfile::TempFile::new().unwrap();
        let mut backend = super::RawFileSync::new(temp.as_file().as_raw_fd());
        assert!(
            backend.start_discard_worker().unwrap().is_none(),
            "a backend without discard opt-in must not start a worker"
        );
    }

    // A block-device discard must leave the queue thread immediately and only
    // ever execute on the dedicated worker. The worker completes with the real
    // ioctl status for the owned descriptor.
    #[test]
    fn test_block_discard_is_dispatched_on_worker_thread() {
        use crate::async_io::{block_discard, AsyncIo};
        use std::os::unix::io::AsRawFd;
        use std::time::{Duration, Instant};

        let temp = vmm_sys_util::tempfile::TempFile::new().unwrap();
        let file = temp.as_file().try_clone().unwrap();
        let expected = block_discard(file.as_raw_fd(), 0, 4 * crate::SECTOR_SIZE)
            .unwrap_err()
            .raw_os_error()
            .unwrap();
        let mut backend = super::RawFileSync::new(file.as_raw_fd())
            .with_block_discard(true)
            .with_discard_max(crate::MAX_DISCARD_SECTORS)
            .with_discard_capacity(8);

        let runner = backend
            .start_discard_worker()
            .unwrap()
            .expect("block backend must expose a worker");
        backend
            .discard(0, (4 * crate::SECTOR_SIZE) as libc::off_t, 21)
            .unwrap();
        assert!(
            backend.complete().is_empty(),
            "submit must not execute the ioctl inline"
        );
        assert_eq!(
            backend.notifier().read().unwrap_err().kind(),
            std::io::ErrorKind::WouldBlock,
            "submit must not arm the notifier inline"
        );

        let worker = std::thread::spawn(move || runner().unwrap());
        let deadline = Instant::now() + Duration::from_secs(5);
        let completions = loop {
            let completions = backend.complete();
            if !completions.is_empty() {
                break completions;
            }
            assert!(
                Instant::now() < deadline,
                "timed out waiting for the worker discard completion"
            );
            std::thread::sleep(Duration::from_millis(1));
        };
        assert_eq!(completions, vec![(21, -expected)]);

        drop(backend);
        worker.join().unwrap();
    }

    #[test]
    fn test_block_discard_over_backend_max_is_rejected_before_submit() {
        use crate::async_io::{AsyncIo, AsyncIoError};
        use std::os::unix::io::AsRawFd;

        let temp = vmm_sys_util::tempfile::TempFile::new().unwrap();
        let file = temp.as_file().try_clone().unwrap();
        let mut backend = super::RawFileSync::new(file.as_raw_fd())
            .with_block_discard(true)
            .with_discard_max(1)
            .with_discard_capacity(8);
        let runner = backend.start_discard_worker().unwrap().unwrap();

        let error = backend
            .discard(0, (2 * crate::SECTOR_SIZE) as libc::off_t, 5)
            .unwrap_err();
        match error {
            AsyncIoError::DiscardRejected(error) => {
                assert_eq!(error.raw_os_error(), Some(libc::EINVAL))
            }
            other => panic!("oversize discard must be rejected safely, got {other:?}"),
        }
        assert!(backend.complete().is_empty());

        drop(runner);
        drop(backend);
    }
}

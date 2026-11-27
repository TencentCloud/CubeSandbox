// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause

use crate::async_io::{
    block_discard_limits, is_regular_xfs_or_juicefs_file, AsyncIo, AsyncIoError, AsyncIoResult,
    DiskFile, DiskFileError, DiskFileResult, DiskTopology,
};
use crate::discard::DiscardSender;
use crate::MAX_DISCARD_SECTORS;
use io_uring::{opcode, squeue, types, IoUring};
use std::fs::File;
use std::io::{self, Seek, SeekFrom};
use std::os::fd::BorrowedFd;
use std::os::unix::io::{AsRawFd, RawFd};
use std::sync::Arc;
use vmm_sys_util::eventfd::EventFd;

pub struct RawFileDisk {
    file: File,
    discard: bool,
    block_discard: bool,
    discard_sector_alignment: u32,
    max_discard_sectors: u32,
}

impl RawFileDisk {
    fn discard_eligible(file: &File) -> bool {
        if !is_regular_xfs_or_juicefs_file(file) {
            return false;
        }
        let Ok(ring) = IoUring::new(1) else {
            return false;
        };
        let mut probe = io_uring::Probe::new();
        ring.submitter().register_probe(&mut probe).is_ok()
            && probe.is_supported(opcode::Fallocate::CODE)
    }

    // Probe once here so creation-time validation, the alignment hint, the
    // advertised maximum and Block::new all agree.
    pub fn with_discard(mut self, enabled: bool) -> Self {
        let limits = enabled.then(|| block_discard_limits(&self.file)).flatten();
        self.block_discard = limits.is_some();
        // BLKDISCARD is an ioctl, so the block path needs no io_uring opcode.
        self.discard = enabled && (self.block_discard || Self::discard_eligible(&self.file));
        if let Some(limits) = limits {
            self.discard_sector_alignment = limits.alignment_sectors;
            self.max_discard_sectors = limits.max_sectors;
        }
        self
    }
    pub fn new(file: File) -> Self {
        RawFileDisk {
            file,
            discard: false,
            block_discard: false,
            discard_sector_alignment: 8,
            max_discard_sectors: MAX_DISCARD_SECTORS,
        }
    }
}

impl DiskFile for RawFileDisk {
    fn size(&mut self) -> DiskFileResult<u64> {
        self.file
            .seek(SeekFrom::End(0))
            .map_err(DiskFileError::Size)
    }

    fn new_async_io(&self, ring_depth: u32) -> DiskFileResult<Box<dyn AsyncIo>> {
        Ok(Box::new(
            RawFileAsync::new(self.file.as_raw_fd(), ring_depth)
                .map(|backend| {
                    backend
                        .with_block_discard(self.block_discard)
                        .with_discard_max(self.max_discard_sectors)
                        .with_discard_capacity(ring_depth as usize)
                })
                .map_err(DiskFileError::NewAsyncIo)?,
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

pub struct RawFileAsync {
    fd: RawFd,
    io_uring: IoUring,
    eventfd: Arc<EventFd>,
    block_discard: bool,
    max_discard_sectors: u32,
    discard_capacity: usize,
    discard_sender: Option<DiscardSender>,
}

impl RawFileAsync {
    pub fn new(fd: RawFd, ring_depth: u32) -> std::io::Result<Self> {
        let io_uring = IoUring::new(ring_depth)?;
        let eventfd = Arc::new(EventFd::new(libc::EFD_NONBLOCK)?);

        // Register the io_uring eventfd that will notify when something in
        // the completion queue is ready.
        io_uring.submitter().register_eventfd(eventfd.as_raw_fd())?;

        Ok(RawFileAsync {
            fd,
            io_uring,
            eventfd,
            block_discard: false,
            max_discard_sectors: MAX_DISCARD_SECTORS,
            discard_capacity: 0,
            discard_sender: None,
        })
    }

    fn with_block_discard(mut self, block: bool) -> Self {
        self.block_discard = block;
        self
    }

    fn with_discard_max(mut self, max_sectors: u32) -> Self {
        self.max_discard_sectors = max_sectors;
        self
    }

    fn with_discard_capacity(mut self, capacity: usize) -> Self {
        self.discard_capacity = capacity.max(1);
        self
    }
}

impl AsyncIo for RawFileAsync {
    fn notifier(&self) -> &EventFd {
        &self.eventfd
    }

    fn start_discard_worker(
        &mut self,
    ) -> io::Result<Option<Box<dyn FnOnce() -> io::Result<()> + Send>>> {
        if !self.block_discard {
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
        Ok(Some(Box::new(move || worker.run())))
    }

    fn read_vectored(
        &mut self,
        offset: libc::off_t,
        iovecs: Vec<libc::iovec>,
        user_data: u64,
    ) -> AsyncIoResult<()> {
        let (submitter, mut sq, _) = self.io_uring.split();

        // Safe because we know the file descriptor is valid and we
        // relied on vm-memory to provide the buffer address.
        let _ = unsafe {
            sq.push(
                &opcode::Readv::new(types::Fd(self.fd), iovecs.as_ptr(), iovecs.len() as u32)
                    .offset(offset)
                    .build()
                    .flags(squeue::Flags::ASYNC)
                    .user_data(user_data),
            )
        };

        // Update the submission queue and submit new operations to the
        // io_uring instance.
        sq.sync();
        submitter.submit().map_err(AsyncIoError::ReadVectored)?;

        Ok(())
    }

    fn write_vectored(
        &mut self,
        offset: libc::off_t,
        iovecs: Vec<libc::iovec>,
        user_data: u64,
    ) -> AsyncIoResult<()> {
        let (submitter, mut sq, _) = self.io_uring.split();

        // Safe because we know the file descriptor is valid and we
        // relied on vm-memory to provide the buffer address.
        let _ = unsafe {
            sq.push(
                &opcode::Writev::new(types::Fd(self.fd), iovecs.as_ptr(), iovecs.len() as u32)
                    .offset(offset)
                    .build()
                    .flags(squeue::Flags::ASYNC)
                    .user_data(user_data),
            )
        };

        // Update the submission queue and submit new operations to the
        // io_uring instance.
        sq.sync();
        submitter.submit().map_err(AsyncIoError::WriteVectored)?;

        Ok(())
    }

    fn fsync(&mut self, user_data: Option<u64>) -> AsyncIoResult<()> {
        if let Some(user_data) = user_data {
            let (submitter, mut sq, _) = self.io_uring.split();

            // Safe because we know the file descriptor is valid.
            let _ = unsafe {
                sq.push(
                    &opcode::Fsync::new(types::Fd(self.fd))
                        .build()
                        .flags(squeue::Flags::ASYNC)
                        .user_data(user_data),
                )
            };

            // Update the submission queue and submit new operations to the
            // io_uring instance.
            sq.sync();
            submitter.submit().map_err(AsyncIoError::Fsync)?;
        } else {
            loop {
                // SAFETY: `self.fd` is owned by this backend and remains valid.
                if unsafe { libc::fsync(self.fd) } == 0 {
                    break;
                }
                let error = std::io::Error::last_os_error();
                if error.kind() != std::io::ErrorKind::Interrupted {
                    return Err(AsyncIoError::Fsync(error));
                }
            }
        }

        Ok(())
    }

    fn discard(
        &mut self,
        offset: libc::off_t,
        len: libc::off_t,
        user_data: u64,
    ) -> AsyncIoResult<()> {
        if self.block_discard {
            // BLKDISCARD is a synchronous ioctl; it is executed by the dedicated
            // worker rather than stalling this queue.
            let sender = self.discard_sender.as_ref().ok_or_else(|| {
                AsyncIoError::DiscardRejected(std::io::Error::from_raw_os_error(libc::ENOSYS))
            })?;
            return sender
                .submit(offset as u64, len as u64, user_data)
                .map_err(AsyncIoError::DiscardRejected);
        }

        let entry = opcode::Fallocate::new(types::Fd(self.fd), len)
            .offset64(offset)
            .mode(libc::FALLOC_FL_PUNCH_HOLE | libc::FALLOC_FL_KEEP_SIZE)
            .build()
            .flags(squeue::Flags::ASYNC)
            .user_data(user_data);
        // SAFETY: entry has no borrowed buffers and fd outlives this operation.
        // A failed push publishes no SQE, so it is the one discard failure that
        // can safely be failed immediately.
        unsafe { self.io_uring.submission().push(&entry) }.map_err(|_| {
            AsyncIoError::DiscardRejected(std::io::Error::from_raw_os_error(libc::EAGAIN))
        })?;
        loop {
            match self.io_uring.submit() {
                Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
                result => {
                    result.map_err(AsyncIoError::Discard)?;
                    break;
                }
            }
        }
        Ok(())
    }

    fn complete(&mut self) -> Vec<(u64, i32)> {
        let mut completion_list: Vec<(u64, i32)> = self
            .discard_sender
            .as_ref()
            .map(|sender| sender.drain())
            .unwrap_or_default();

        let cq = self.io_uring.completion();
        for cq_entry in cq {
            completion_list.push((cq_entry.user_data(), cq_entry.result()));
        }

        completion_list
    }
}

#[cfg(test)]
mod tests {
    use super::RawFileAsync;
    use crate::async_io::{AsyncIo, AsyncIoError};
    use io_uring::opcode;
    use std::os::unix::io::AsRawFd;
    use std::time::{Duration, Instant};
    use vmm_sys_util::tempfile::TempFile;

    // A full SQ may only be reported as a safe rejection when the entry was
    // never published. Fill a small ring with NOPs (no guest buffers touched),
    // prove the rejected discard changes neither the SQ contents nor their
    // count, then submit and drain exactly those NOPs. The bounded deadline
    // only prevents a hung test; the proof is the exact accepted-ID set.
    #[test]
    fn test_discard_sq_full_is_safely_rejected_and_not_enqueued() {
        let temp = TempFile::new().unwrap();
        let file = temp.as_file().try_clone().unwrap();
        let Ok(mut backend) = RawFileAsync::new(file.as_raw_fd(), 2) else {
            eprintln!("skipping: io_uring unavailable");
            return;
        };

        let mut accepted_ids = Vec::new();
        let mut user_data = 1000u64;
        loop {
            let entry = opcode::Nop::new().build().user_data(user_data);
            // SAFETY: NOP borrows no buffers; an accepted entry is owned by
            // the ring.
            if unsafe { backend.io_uring.submission().push(&entry) }.is_err() {
                break;
            }
            accepted_ids.push(user_data);
            user_data += 1;
        }
        assert!(
            !accepted_ids.is_empty(),
            "ring must accept at least one NOP"
        );

        let sq_length_before = backend.io_uring.submission().len();
        assert_eq!(
            sq_length_before,
            accepted_ids.len(),
            "every accepted NOP must still occupy the SQ"
        );
        assert_eq!(
            sq_length_before,
            backend.io_uring.submission().capacity(),
            "the fill loop must stop only because the SQ is full"
        );

        let error = backend.discard(0, 4096, 7).unwrap_err();
        match error {
            AsyncIoError::DiscardRejected(err) => {
                assert_eq!(err.raw_os_error(), Some(libc::EAGAIN))
            }
            other => panic!("discard must report a safe rejection, got {other:?}"),
        }
        assert_eq!(
            backend.io_uring.submission().len(),
            sq_length_before,
            "a failed discard push must not add or consume SQ entries"
        );

        let deadline = Instant::now() + Duration::from_secs(5);
        let mut completions = Vec::new();
        loop {
            backend.io_uring.submitter().submit().unwrap();
            completions.extend(backend.complete());
            let all_accepted_completed = accepted_ids.iter().all(|accepted| {
                completions
                    .iter()
                    .any(|(completed, _)| completed == accepted)
            });
            if all_accepted_completed {
                break;
            }
            assert!(
                Instant::now() < deadline,
                "timed out waiting for accepted NOPs: accepted={accepted_ids:?}, completed={completions:?}"
            );
            std::thread::sleep(Duration::from_millis(1));
        }

        assert!(
            backend.complete().is_empty(),
            "no completion may remain after all accepted NOPs"
        );
        assert!(
            backend.io_uring.submission().is_empty(),
            "the SQ must be empty after all accepted NOPs complete"
        );
        assert!(
            backend.io_uring.completion().is_empty(),
            "the CQ must be empty after draining all accepted NOPs"
        );

        let mut completed_ids: Vec<u64> = completions.iter().map(|(id, _)| *id).collect();
        completed_ids.sort_unstable();
        let mut expected_ids = accepted_ids.clone();
        expected_ids.sort_unstable();
        assert_eq!(
            completed_ids, expected_ids,
            "exactly the accepted NOP IDs must complete"
        );
        assert!(
            completions.iter().all(|(_, result)| *result == 0),
            "accepted NOPs must complete successfully: {completions:?}"
        );
        assert!(
            completions.iter().all(|(id, _)| *id != 7),
            "the rejected discard's user_data must never complete: {completions:?}"
        );
    }

    // A block-device discard must be dispatched to the dedicated worker and
    // never executed on the io_uring queue thread.
    #[test]
    fn test_block_discard_is_dispatched_on_worker_thread() {
        use crate::async_io::{block_discard, AsyncIo};
        use std::time::{Duration, Instant};

        let temp = TempFile::new().unwrap();
        let file = temp.as_file().try_clone().unwrap();
        let expected = block_discard(file.as_raw_fd(), 0, 4 * crate::SECTOR_SIZE)
            .unwrap_err()
            .raw_os_error()
            .unwrap();
        let Ok(backend) = RawFileAsync::new(file.as_raw_fd(), 8) else {
            eprintln!("skipping: io_uring unavailable");
            return;
        };
        let mut backend = backend
            .with_block_discard(true)
            .with_discard_max(crate::MAX_DISCARD_SECTORS)
            .with_discard_capacity(8);

        let runner = backend
            .start_discard_worker()
            .unwrap()
            .expect("block backend must expose a worker");
        backend
            .discard(0, (4 * crate::SECTOR_SIZE) as libc::off_t, 22)
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
        assert_eq!(completions, vec![(22, -expected)]);

        drop(backend);
        worker.join().unwrap();
    }

    #[test]
    fn test_block_discard_over_backend_max_is_rejected_before_submit() {
        use crate::async_io::{AsyncIo, AsyncIoError};

        let temp = TempFile::new().unwrap();
        let file = temp.as_file().try_clone().unwrap();
        let Ok(backend) = RawFileAsync::new(file.as_raw_fd(), 8) else {
            eprintln!("skipping: io_uring unavailable");
            return;
        };
        let mut backend = backend
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

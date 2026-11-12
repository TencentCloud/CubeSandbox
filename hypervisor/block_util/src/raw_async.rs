// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause

use crate::async_io::{
    AsyncIo, AsyncIoError, AsyncIoResult, DiskFile, DiskFileError, DiskFileResult, DiskTopology,
};
use io_uring::{opcode, squeue, types, IoUring};
use std::fs::File;
use std::io::{Seek, SeekFrom};
use std::os::unix::io::{AsRawFd, RawFd};
use vmm_sys_util::eventfd::EventFd;

pub struct RawFileDisk {
    file: File,
}

impl RawFileDisk {
    pub fn new(file: File) -> Self {
        RawFileDisk { file }
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
                .map_err(DiskFileError::NewAsyncIo)?,
        ) as Box<dyn AsyncIo>)
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
    eventfd: EventFd,
}

impl RawFileAsync {
    pub fn new(fd: RawFd, ring_depth: u32) -> std::io::Result<Self> {
        let io_uring = IoUring::new(ring_depth)?;
        let eventfd = EventFd::new(libc::EFD_NONBLOCK)?;

        // Register the io_uring eventfd that will notify when something in
        // the completion queue is ready.
        io_uring.submitter().register_eventfd(eventfd.as_raw_fd())?;

        Ok(RawFileAsync {
            fd,
            io_uring,
            eventfd,
        })
    }
}

impl AsyncIo for RawFileAsync {
    fn notifier(&self) -> &EventFd {
        &self.eventfd
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

    fn complete(&mut self) -> Vec<(u64, i32)> {
        let mut completion_list = Vec::new();

        let cq = self.io_uring.completion();
        for cq_entry in cq {
            completion_list.push((cq_entry.user_data(), cq_entry.result()));
        }

        completion_list
    }
}

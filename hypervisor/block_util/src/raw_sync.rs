// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause

use crate::async_io::{
    AsyncIo, AsyncIoError, AsyncIoResult, DiskFile, DiskFileError, DiskFileResult, DiskTopology,
};
use std::fs::File;
use std::io::{self, Seek, SeekFrom};
use std::os::unix::io::{AsRawFd, RawFd};
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
}

impl RawFileDiskSync {
    pub fn new(file: File) -> Self {
        RawFileDiskSync { file }
    }
}

impl DiskFile for RawFileDiskSync {
    fn size(&mut self) -> DiskFileResult<u64> {
        self.file
            .seek(SeekFrom::End(0))
            .map_err(DiskFileError::Size)
    }

    fn new_async_io(&self, _ring_depth: u32) -> DiskFileResult<Box<dyn AsyncIo>> {
        Ok(Box::new(RawFileSync::new(self.file.as_raw_fd())) as Box<dyn AsyncIo>)
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

pub struct RawFileSync {
    fd: RawFd,
    eventfd: EventFd,
    completion_list: Vec<(u64, i32)>,
}

impl RawFileSync {
    pub fn new(fd: RawFd) -> Self {
        RawFileSync {
            fd,
            eventfd: EventFd::new(libc::EFD_NONBLOCK).expect("Failed creating EventFd for RawFile"),
            completion_list: Vec::new(),
        }
    }
}

impl AsyncIo for RawFileSync {
    fn notifier(&self) -> &EventFd {
        &self.eventfd
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

    fn complete(&mut self) -> Vec<(u64, i32)> {
        self.completion_list.drain(..).collect()
    }
}

#[cfg(test)]
mod tests {
    use super::run_vectored_io;
    use std::io;

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
}

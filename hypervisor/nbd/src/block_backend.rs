// Copyright 2026 Tencent Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//! Optional `block_util` (`DiskFile` / `AsyncIo`) adapter packaged inside the `nbd` crate
//! behind the `block-backend` feature so external modules require near-zero glue code.

use std::io;
use std::path::{Path, PathBuf};

use block_util::async_io::{
    AsyncIo, AsyncIoError, AsyncIoResult, DiskFile, DiskFileError, DiskFileResult, DiskTopology,
};
use vmm_sys_util::eventfd::EventFd;

use crate::{is_nbd_uri, NbdClient, NbdClientOptions, NbdEngine};

/// Returns `Some(uri_str)` if `path` stores an `nbd://` or `nbd+unix://` URI.
pub fn is_nbd_path(path: &Path) -> Option<&str> {
    let s = path.to_str()?;
    is_nbd_uri(s).then_some(s)
}

pub struct NbdDisk {
    client: NbdClient,
}

impl NbdDisk {
    /// Opens an NBD URI as a `DiskFile` and returns `(Box<dyn DiskFile>, id_path)`.
    ///
    /// For `nbd+unix://` exports, `id_path` points to the underlying Unix socket so
    /// `block_util::build_disk_image_id` can stat an existing inode without modifying
    /// `block_util`.
    pub fn open_boxed(
        uri: &str,
        readonly: bool,
        direct: bool,
        num_queues: usize,
        prefer_uring: bool,
    ) -> io::Result<(Box<dyn DiskFile>, PathBuf)> {
        if direct {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "direct=on is not supported for NBD disks",
            ));
        }
        let client = NbdClient::open(
            uri,
            NbdClientOptions {
                readonly,
                num_queues,
                prefer_uring,
                socket_override: None,
            },
        )?;
        let id_path = client
            .uri()
            .socket_path()
            .map_or_else(|| PathBuf::from(uri), Path::to_path_buf);
        Ok((Box::new(Self { client }), id_path))
    }
}

impl DiskFile for NbdDisk {
    fn size(&mut self) -> DiskFileResult<u64> {
        Ok(self.client.export_info().size)
    }

    fn new_async_io(&self, _ring_depth: u32) -> DiskFileResult<Box<dyn AsyncIo>> {
        let engine = self
            .client
            .new_engine()
            .map_err(|e| DiskFileError::NewAsyncIo(e.into()))?;
        Ok(Box::new(NbdAsyncIo(engine)))
    }

    fn topology(&mut self) -> DiskTopology {
        let info = self.client.export_info();
        let logical = u64::from(info.min_block).max(512);
        let optimal = u64::from(info.opt_block);
        DiskTopology {
            logical_block_size: logical,
            physical_block_size: optimal.max(logical),
            minimum_io_size: logical,
            optimal_io_size: optimal,
        }
    }
}

struct NbdAsyncIo(NbdEngine);

impl AsyncIo for NbdAsyncIo {
    fn notifier(&self) -> &EventFd {
        self.0.notifier()
    }

    fn read_vectored(
        &mut self,
        offset: libc::off_t,
        iovecs: Vec<libc::iovec>,
        user_data: u64,
    ) -> AsyncIoResult<()> {
        // SAFETY: `Request::execute_async` guards guest memory until `complete()`.
        unsafe { self.0.read_vectored(offset as u64, iovecs, user_data) }
            .map_err(AsyncIoError::ReadVectored)
    }

    fn write_vectored(
        &mut self,
        offset: libc::off_t,
        iovecs: Vec<libc::iovec>,
        user_data: u64,
    ) -> AsyncIoResult<()> {
        // SAFETY: `Request::execute_async` guards guest memory until `complete()`.
        unsafe { self.0.write_vectored(offset as u64, iovecs, user_data) }
            .map_err(AsyncIoError::WriteVectored)
    }

    fn fsync(&mut self, user_data: Option<u64>) -> AsyncIoResult<()> {
        self.0.fsync(user_data).map_err(AsyncIoError::Fsync)
    }

    fn complete(&mut self) -> Vec<(u64, i32)> {
        self.0.complete()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn nbd_path_detection_and_direct_rejection() {
        assert_eq!(
            is_nbd_path(Path::new("nbd+unix:///vol0?socket=/tmp/a.sock")),
            Some("nbd+unix:///vol0?socket=/tmp/a.sock")
        );
        assert_eq!(is_nbd_path(Path::new("/var/lib/disk.raw")), None);

        let err = NbdDisk::open_boxed(
            "nbd+unix:///vol0?socket=/tmp/nonexistent.sock",
            false,
            true,
            1,
            false,
        )
        .err()
        .unwrap();
        assert_eq!(err.kind(), io::ErrorKind::InvalidInput);
    }
}

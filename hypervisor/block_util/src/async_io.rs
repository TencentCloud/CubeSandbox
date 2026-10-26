// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause

use libc::{ioctl, S_IFBLK, S_IFMT};
use std::convert::TryInto;
use std::fs::File;
use std::os::unix::io::AsRawFd;
use thiserror::Error;
use vmm_sys_util::eventfd::EventFd;
use vmm_sys_util::{ioctl_io_nr, ioctl_ioc_nr};

#[derive(Error, Debug)]
pub enum DiskFileError {
    /// Failed getting disk file size.
    #[error("Failed getting disk file size: {0}")]
    Size(#[source] std::io::Error),
    /// Failed creating a new AsyncIo.
    #[error("Failed creating a new AsyncIo: {0}")]
    NewAsyncIo(#[source] std::io::Error),
}

#[derive(Debug)]
pub struct DiskTopology {
    pub logical_block_size: u64,
    pub physical_block_size: u64,
    pub minimum_io_size: u64,
    pub optimal_io_size: u64,
}

impl Default for DiskTopology {
    fn default() -> Self {
        Self {
            logical_block_size: 512,
            physical_block_size: 512,
            minimum_io_size: 512,
            optimal_io_size: 0,
        }
    }
}

ioctl_io_nr!(BLKSSZGET, 0x12, 104);
ioctl_io_nr!(BLKPBSZGET, 0x12, 123);
ioctl_io_nr!(BLKIOMIN, 0x12, 120);
ioctl_io_nr!(BLKIOOPT, 0x12, 121);

enum BlockSize {
    LogicalBlock,
    PhysicalBlock,
    MinimumIo,
    OptimalIo,
}

impl DiskTopology {
    fn is_block_device(f: &mut File) -> std::io::Result<bool> {
        let mut stat = std::mem::MaybeUninit::<libc::stat>::uninit();
        let ret = unsafe { libc::fstat(f.as_raw_fd(), stat.as_mut_ptr()) };
        if ret != 0 {
            return Err(std::io::Error::last_os_error());
        }

        let is_block = unsafe { (*stat.as_ptr()).st_mode & S_IFMT == S_IFBLK };
        Ok(is_block)
    }

    // libc::ioctl() takes different types on different architectures
    #[allow(clippy::useless_conversion)]
    fn query_block_size(f: &mut File, block_size_type: BlockSize) -> std::io::Result<u64> {
        let mut block_size = 0;
        let ret = unsafe {
            ioctl(
                f.as_raw_fd(),
                match block_size_type {
                    BlockSize::LogicalBlock => BLKSSZGET(),
                    BlockSize::PhysicalBlock => BLKPBSZGET(),
                    BlockSize::MinimumIo => BLKIOMIN(),
                    BlockSize::OptimalIo => BLKIOOPT(),
                }
                .try_into()
                .unwrap(),
                &mut block_size,
            )
        };
        if ret != 0 {
            return Err(std::io::Error::last_os_error());
        };

        Ok(block_size)
    }

    pub fn probe(f: &mut File) -> std::io::Result<Self> {
        if !Self::is_block_device(f)? {
            return Ok(DiskTopology::default());
        }

        Ok(DiskTopology {
            logical_block_size: Self::query_block_size(f, BlockSize::LogicalBlock)?,
            physical_block_size: Self::query_block_size(f, BlockSize::PhysicalBlock)?,
            minimum_io_size: Self::query_block_size(f, BlockSize::MinimumIo)?,
            optimal_io_size: Self::query_block_size(f, BlockSize::OptimalIo)?,
        })
    }
}

pub type DiskFileResult<T> = std::result::Result<T, DiskFileError>;

pub trait DiskFile: Send {
    fn size(&mut self) -> DiskFileResult<u64>;
    fn new_async_io(&self, ring_depth: u32) -> DiskFileResult<Box<dyn AsyncIo>>;
    fn topology(&mut self) -> DiskTopology {
        DiskTopology::default()
    }
}

#[derive(Error, Debug)]
pub enum AsyncIoError {
    /// Failed vectored reading from file.
    #[error("Failed vectored reading from file: {0}")]
    ReadVectored(#[source] std::io::Error),
    /// Failed vectored writing to file.
    #[error("Failed vectored writing to file: {0}")]
    WriteVectored(#[source] std::io::Error),
    /// Failed synchronizing file.
    #[error("Failed synchronizing file: {0}")]
    Fsync(#[source] std::io::Error),
}

pub type AsyncIoResult<T> = std::result::Result<T, AsyncIoError>;

pub trait AsyncIo: Send {
    fn notifier(&self) -> &EventFd;
    fn read_vectored(
        &mut self,
        offset: libc::off_t,
        iovecs: Vec<libc::iovec>,
        user_data: u64,
    ) -> AsyncIoResult<()>;
    fn write_vectored(
        &mut self,
        offset: libc::off_t,
        iovecs: Vec<libc::iovec>,
        user_data: u64,
    ) -> AsyncIoResult<()>;
    fn fsync(&mut self, user_data: Option<u64>) -> AsyncIoResult<()>;
    fn complete(&mut self) -> Vec<(u64, i32)>;
}

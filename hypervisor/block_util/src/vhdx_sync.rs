// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

use crate::async_io::{AsyncIo, AsyncIoResult, DiskFile, DiskFileError, DiskFileResult};
use crate::AsyncAdaptor;
use std::fs::File;
use std::sync::{Arc, Mutex, MutexGuard};
use vhdx::vhdx::{Result as VhdxResult, Vhdx};
use vmm_sys_util::eventfd::EventFd;

pub struct VhdxDiskSync {
    vhdx_file: Arc<Mutex<Vhdx>>,
}

impl VhdxDiskSync {
    pub fn new(f: File) -> VhdxResult<Self> {
        Ok(VhdxDiskSync {
            vhdx_file: Arc::new(Mutex::new(Vhdx::new(f)?)),
        })
    }
}

impl DiskFile for VhdxDiskSync {
    fn size(&mut self) -> DiskFileResult<u64> {
        Ok(self.vhdx_file.lock().unwrap().virtual_disk_size())
    }

    fn new_async_io(&self, _ring_depth: u32) -> DiskFileResult<Box<dyn AsyncIo>> {
        Ok(
            Box::new(VhdxSync::new(self.vhdx_file.clone()).map_err(DiskFileError::NewAsyncIo)?)
                as Box<dyn AsyncIo>,
        )
    }
}

pub struct VhdxSync {
    vhdx_file: Arc<Mutex<Vhdx>>,
    eventfd: EventFd,
    completion_list: Vec<(u64, i32)>,
}

impl VhdxSync {
    pub fn new(vhdx_file: Arc<Mutex<Vhdx>>) -> std::io::Result<Self> {
        Ok(VhdxSync {
            vhdx_file,
            eventfd: EventFd::new(libc::EFD_NONBLOCK)?,
            completion_list: Vec::new(),
        })
    }
}

impl AsyncAdaptor<Vhdx> for Arc<Mutex<Vhdx>> {
    fn file(&mut self) -> MutexGuard<Vhdx> {
        self.lock().unwrap()
    }
}

impl AsyncIo for VhdxSync {
    fn notifier(&self) -> &EventFd {
        &self.eventfd
    }

    fn read_vectored(
        &mut self,
        offset: libc::off_t,
        iovecs: Vec<libc::iovec>,
        user_data: u64,
    ) -> AsyncIoResult<()> {
        self.vhdx_file.read_vectored_sync(
            offset,
            iovecs,
            user_data,
            &self.eventfd,
            &mut self.completion_list,
        )
    }

    fn write_vectored(
        &mut self,
        offset: libc::off_t,
        iovecs: Vec<libc::iovec>,
        user_data: u64,
    ) -> AsyncIoResult<()> {
        self.vhdx_file.write_vectored_sync(
            offset,
            iovecs,
            user_data,
            &self.eventfd,
            &mut self.completion_list,
        )
    }

    fn fsync(&mut self, user_data: Option<u64>) -> AsyncIoResult<()> {
        self.vhdx_file
            .fsync_sync(user_data, &self.eventfd, &mut self.completion_list)
    }

    fn complete(&mut self) -> Vec<(u64, i32)> {
        self.completion_list.drain(..).collect()
    }
}

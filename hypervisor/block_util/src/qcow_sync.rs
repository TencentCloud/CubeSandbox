// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause

use crate::async_io::{AsyncIo, AsyncIoResult, DiskFile, DiskFileError, DiskFileResult};
use crate::AsyncAdaptor;
use qcow::{QcowFile, RawFile, Result as QcowResult};
use std::fs::File;
use std::io::{Seek, SeekFrom};
use std::sync::{Arc, Mutex, MutexGuard};
use vmm_sys_util::eventfd::EventFd;

pub struct QcowDiskSync {
    qcow_file: Arc<Mutex<QcowFile>>,
}

impl QcowDiskSync {
    pub fn new(file: File, direct_io: bool) -> QcowResult<Self> {
        Ok(QcowDiskSync {
            qcow_file: Arc::new(Mutex::new(QcowFile::from(RawFile::new(file, direct_io))?)),
        })
    }
}

impl DiskFile for QcowDiskSync {
    fn size(&mut self) -> DiskFileResult<u64> {
        let mut file = self.qcow_file.lock().unwrap();

        file.seek(SeekFrom::End(0)).map_err(DiskFileError::Size)
    }

    fn new_async_io(&self, _ring_depth: u32) -> DiskFileResult<Box<dyn AsyncIo>> {
        Ok(Box::new(QcowSync::new(self.qcow_file.clone())) as Box<dyn AsyncIo>)
    }
}

pub struct QcowSync {
    qcow_file: Arc<Mutex<QcowFile>>,
    eventfd: EventFd,
    completion_list: Vec<(u64, i32)>,
}

impl QcowSync {
    pub fn new(qcow_file: Arc<Mutex<QcowFile>>) -> Self {
        QcowSync {
            qcow_file,
            eventfd: EventFd::new(libc::EFD_NONBLOCK)
                .expect("Failed creating EventFd for QcowSync"),
            completion_list: Vec::new(),
        }
    }
}

impl AsyncAdaptor<QcowFile> for Arc<Mutex<QcowFile>> {
    fn file(&mut self) -> MutexGuard<QcowFile> {
        self.lock().unwrap()
    }
}

impl AsyncIo for QcowSync {
    fn notifier(&self) -> &EventFd {
        &self.eventfd
    }

    fn read_vectored(
        &mut self,
        offset: libc::off_t,
        iovecs: Vec<libc::iovec>,
        user_data: u64,
    ) -> AsyncIoResult<()> {
        self.qcow_file.read_vectored_sync(
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
        self.qcow_file.write_vectored_sync(
            offset,
            iovecs,
            user_data,
            &self.eventfd,
            &mut self.completion_list,
        )
    }

    fn fsync(&mut self, user_data: Option<u64>) -> AsyncIoResult<()> {
        self.qcow_file
            .fsync_sync(user_data, &self.eventfd, &mut self.completion_list)
    }

    fn complete(&mut self) -> Vec<(u64, i32)> {
        self.completion_list.drain(..).collect()
    }
}

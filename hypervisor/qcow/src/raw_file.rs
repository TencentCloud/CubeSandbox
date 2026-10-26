// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.
//
// Copyright © 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause

use libc::c_void;
use std::alloc::{alloc_zeroed, dealloc, Layout};
use std::convert::TryInto;
use std::fs::{File, Metadata};
use std::io::{self, Read, Seek, SeekFrom, Write};
use std::os::unix::io::{AsRawFd, RawFd};
use std::slice;
use vmm_sys_util::{seek_hole::SeekHole, write_zeroes::PunchHole, write_zeroes::WriteZeroesAt};

#[derive(Debug)]
pub struct RawFile {
    file: File,
    alignment: usize,
    position: u64,
}

const BLK_ALIGNMENTS: [usize; 2] = [512, 4096];

fn is_valid_alignment(fd: RawFd, alignment: usize) -> bool {
    let layout = Layout::from_size_align(alignment, alignment).unwrap();
    // SAFETY: layout has non-zero size
    let ptr = unsafe { alloc_zeroed(layout) };
    assert!(!ptr.is_null());

    // SAFETY: FFI call
    let ret = unsafe {
        ::libc::pread(
            fd,
            ptr as *mut c_void,
            alignment,
            alignment.try_into().unwrap(),
        )
    };

    // SAFETY: ptr was allocated by alloc_zeroed with layout
    unsafe { dealloc(ptr, layout) };

    ret >= 0
}

impl RawFile {
    pub fn new(file: File, direct_io: bool) -> Self {
        // Assume no alignment restrictions if we aren't using O_DIRECT.
        let mut alignment = 0;
        if direct_io {
            for align in &BLK_ALIGNMENTS {
                if is_valid_alignment(file.as_raw_fd(), *align) {
                    alignment = *align;
                    break;
                }
            }
        }
        RawFile {
            file,
            alignment,
            position: 0,
        }
    }

    fn round_up(&self, offset: u64) -> u64 {
        let align: u64 = self.alignment.try_into().unwrap();
        ((offset + align - 1) / align) * align
    }

    fn round_down(&self, offset: u64) -> u64 {
        let align: u64 = self.alignment.try_into().unwrap();
        (offset / align) * align
    }

    fn is_aligned(&self, buf: &[u8]) -> bool {
        if self.alignment == 0 {
            return true;
        }

        let align64: u64 = self.alignment.try_into().unwrap();

        (self.position % align64 == 0)
            && ((buf.as_ptr() as usize) % self.alignment == 0)
            && (buf.len() % self.alignment == 0)
    }

    pub fn set_len(&self, size: u64) -> std::io::Result<()> {
        self.file.set_len(size)
    }

    pub fn metadata(&self) -> std::io::Result<Metadata> {
        self.file.metadata()
    }

    pub fn try_clone(&self) -> std::io::Result<RawFile> {
        Ok(RawFile {
            file: self.file.try_clone().expect("RawFile cloning failed"),
            alignment: self.alignment,
            position: self.position,
        })
    }

    pub fn sync_all(&self) -> std::io::Result<()> {
        self.file.sync_all()
    }

    pub fn sync_data(&self) -> std::io::Result<()> {
        self.file.sync_data()
    }
}

impl Read for RawFile {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        if self.is_aligned(buf) {
            match self.file.read(buf) {
                Ok(r) => {
                    self.position = self.position.checked_add(r.try_into().unwrap()).unwrap();
                    Ok(r)
                }
                Err(e) => Err(e),
            }
        } else {
            let rounded_pos: u64 = self.round_down(self.position);
            let file_offset: usize = self
                .position
                .checked_sub(rounded_pos)
                .unwrap()
                .try_into()
                .unwrap();
            let buf_len: usize = buf.len();
            let rounded_len: usize = self
                .round_up(
                    file_offset
                        .checked_add(buf_len)
                        .unwrap()
                        .try_into()
                        .unwrap(),
                )
                .try_into()
                .unwrap();

            let layout = Layout::from_size_align(rounded_len, self.alignment).unwrap();
            // SAFETY: layout has non-zero size
            let tmp_ptr = unsafe { alloc_zeroed(layout) };
            if tmp_ptr.is_null() {
                return Err(io::Error::last_os_error());
            }

            // SAFETY: tmp_ptr is valid and at least rounded_len long
            let tmp_buf = unsafe { slice::from_raw_parts_mut(tmp_ptr, rounded_len) };

            // This can eventually replaced with read_at once its interface
            // has been stabilized.
            // SAFETY: FFI call. All parameters are valid.
            let ret = unsafe {
                ::libc::pread64(
                    self.file.as_raw_fd(),
                    tmp_buf.as_mut_ptr() as *mut c_void,
                    tmp_buf.len(),
                    rounded_pos.try_into().unwrap(),
                )
            };
            if ret < 0 {
                // SAFETY: tmp_ptr was allocated by alloc_zeroed with layout
                unsafe { dealloc(tmp_ptr, layout) };
                return Err(io::Error::last_os_error());
            }

            let read: usize = ret.try_into().unwrap();
            if read < file_offset {
                // SAFETY: tmp_ptr was allocated by alloc_zeroed with layout
                unsafe { dealloc(tmp_ptr, layout) };
                return Ok(0);
            }

            let mut to_copy = read - file_offset;
            if to_copy > buf_len {
                to_copy = buf_len;
            }

            buf.copy_from_slice(&tmp_buf[file_offset..(file_offset + buf_len)]);
            // SAFETY: tmp_ptr was allocated by alloc_zeroed with layout
            unsafe { dealloc(tmp_ptr, layout) };

            self.seek(SeekFrom::Current(to_copy.try_into().unwrap()))
                .unwrap();
            Ok(to_copy)
        }
    }
}

impl Write for RawFile {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        if self.is_aligned(buf) {
            match self.file.write(buf) {
                Ok(r) => {
                    self.position = self.position.checked_add(r.try_into().unwrap()).unwrap();
                    Ok(r)
                }
                Err(e) => Err(e),
            }
        } else {
            let rounded_pos: u64 = self.round_down(self.position);
            let file_offset: usize = self
                .position
                .checked_sub(rounded_pos)
                .unwrap()
                .try_into()
                .unwrap();
            let buf_len: usize = buf.len();
            let rounded_len: usize = self
                .round_up(
                    file_offset
                        .checked_add(buf_len)
                        .unwrap()
                        .try_into()
                        .unwrap(),
                )
                .try_into()
                .unwrap();

            let layout = Layout::from_size_align(rounded_len, self.alignment).unwrap();
            // SAFETY: layout has non-zero size
            let tmp_ptr = unsafe { alloc_zeroed(layout) };
            if tmp_ptr.is_null() {
                return Err(io::Error::last_os_error());
            }

            let tmp_buf = unsafe { slice::from_raw_parts_mut(tmp_ptr, rounded_len) };

            // This can eventually replaced with read_at once its interface
            // has been stabilized.
            // SAFETY: FFI call
            let ret = unsafe {
                ::libc::pread64(
                    self.file.as_raw_fd(),
                    tmp_buf.as_mut_ptr() as *mut c_void,
                    tmp_buf.len(),
                    rounded_pos.try_into().unwrap(),
                )
            };
            if ret < 0 {
                // SAFETY: tmp_ptr was allocated by alloc_zeroed with layout
                unsafe { dealloc(tmp_ptr, layout) };
                return Err(io::Error::last_os_error());
            };

            tmp_buf[file_offset..(file_offset + buf_len)].copy_from_slice(buf);

            // This can eventually replaced with write_at once its interface
            // has been stabilized.
            // SAFETY: FFI call
            let ret = unsafe {
                ::libc::pwrite64(
                    self.file.as_raw_fd(),
                    tmp_buf.as_ptr() as *const c_void,
                    tmp_buf.len(),
                    rounded_pos.try_into().unwrap(),
                )
            };

            // SAFETY: tmp_ptr was allocated by alloc_zeroed with layout
            unsafe { dealloc(tmp_ptr, layout) };

            if ret < 0 {
                return Err(io::Error::last_os_error());
            }

            let written: usize = ret.try_into().unwrap();
            if written < file_offset {
                Ok(0)
            } else {
                let mut to_seek = written - file_offset;
                if to_seek > buf_len {
                    to_seek = buf_len;
                }

                self.seek(SeekFrom::Current(to_seek.try_into().unwrap()))
                    .unwrap();
                Ok(to_seek)
            }
        }
    }

    fn flush(&mut self) -> std::io::Result<()> {
        self.file.sync_all()
    }
}

impl Seek for RawFile {
    fn seek(&mut self, newpos: SeekFrom) -> std::io::Result<u64> {
        match self.file.seek(newpos) {
            Ok(pos) => {
                self.position = pos;
                Ok(pos)
            }
            Err(e) => Err(e),
        }
    }
}

impl WriteZeroesAt for RawFile {
    fn write_zeroes_at(&mut self, offset: u64, length: usize) -> std::io::Result<usize> {
        self.file.write_zeroes_at(offset, length)
    }
}

impl PunchHole for RawFile {
    fn punch_hole(&mut self, offset: u64, length: u64) -> std::io::Result<()> {
        self.file.punch_hole(offset, length)
    }
}

impl SeekHole for RawFile {
    fn seek_hole(&mut self, offset: u64) -> std::io::Result<Option<u64>> {
        match self.file.seek_hole(offset) {
            Ok(pos) => {
                if let Some(p) = pos {
                    self.position = p;
                }
                Ok(pos)
            }
            Err(e) => Err(e),
        }
    }

    fn seek_data(&mut self, offset: u64) -> std::io::Result<Option<u64>> {
        match self.file.seek_data(offset) {
            Ok(pos) => {
                if let Some(p) = pos {
                    self.position = p;
                }
                Ok(pos)
            }
            Err(e) => Err(e),
        }
    }
}

impl Clone for RawFile {
    fn clone(&self) -> Self {
        RawFile {
            file: self.file.try_clone().expect("RawFile cloning failed"),
            alignment: self.alignment,
            position: self.position,
        }
    }
}

// Copyright 2018 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

use std::convert::TryInto;
use std::fs::File;
use std::io::{Error, Result};
use std::os::unix::io::{AsFd, AsRawFd};

use vm_memory::bitmap::{AtomicBitmap, Bitmap, RefSlice};
use vm_memory::VolatileSlice;

use crate::oslib;
use libc::{c_int, c_void, off64_t, preadv64, size_t};

/// A trait for setting the size of a file.
/// This is equivalent to File's `set_len` method, but
/// wrapped in a trait so that it can be implemented for
/// other types.
pub trait FileSetLen {
    // Set the size of this file.
    // This is the moral equivalent of `ftruncate()`.
    fn set_len(&self, _len: u64) -> Result<()>;
}

impl FileSetLen for File {
    fn set_len(&self, len: u64) -> Result<()> {
        File::set_len(self, len)
    }
}

/// A trait similar to the unix `ReadExt` and `WriteExt` traits, but for volatile memory.
pub trait FileReadWriteAtVolatile {
    /// Reads bytes from this file at `offset` into the given slice of buffers, returning the number
    /// of bytes read on success. Data is copied to fill each buffer in order, with the final buffer
    /// written to possibly being only partially filled.
    fn read_vectored_at_volatile(
        &self,
        bufs: &[VolatileSlice<RefSlice<AtomicBitmap>>],
        offset: u64,
    ) -> Result<usize>;

    /// Writes bytes to this file at `offset` from the given slice of buffers, returning the number
    /// of bytes written on success. Data is copied from each buffer in order, with the final buffer
    /// read from possibly being only partially consumed.
    fn write_vectored_at_volatile(
        &self,
        bufs: &[VolatileSlice<RefSlice<AtomicBitmap>>],
        offset: u64,
        flags: Option<oslib::WritevFlags>,
    ) -> Result<usize>;
}

impl<T: FileReadWriteAtVolatile + ?Sized> FileReadWriteAtVolatile for &T {
    fn read_vectored_at_volatile(
        &self,
        bufs: &[VolatileSlice<RefSlice<AtomicBitmap>>],
        offset: u64,
    ) -> Result<usize> {
        (**self).read_vectored_at_volatile(bufs, offset)
    }

    fn write_vectored_at_volatile(
        &self,
        bufs: &[VolatileSlice<RefSlice<AtomicBitmap>>],
        offset: u64,
        flags: Option<oslib::WritevFlags>,
    ) -> Result<usize> {
        (**self).write_vectored_at_volatile(bufs, offset, flags)
    }
}

macro_rules! volatile_impl {
    ($ty:ty) => {
        impl FileReadWriteAtVolatile for $ty {
            fn read_vectored_at_volatile(
                &self,
                bufs: &[VolatileSlice<RefSlice<AtomicBitmap>>],
                offset: u64,
            ) -> Result<usize> {
                let slice_guards: Vec<_> = bufs.iter().map(|s| s.ptr_guard_mut()).collect();
                let iovecs: Vec<libc::iovec> = slice_guards
                    .iter()
                    .map(|s| libc::iovec {
                        iov_base: s.as_ptr() as *mut c_void,
                        iov_len: s.len() as size_t,
                    })
                    .collect();

                if iovecs.is_empty() {
                    return Ok(0);
                }

                // SAFETY: Safe because only bytes inside the buffers are
                // accessed and the kernel is expected to handle arbitrary
                // memory for I/O. The pointers into the slice are valid since
                // the slice_guards are still in scope.
                let ret = unsafe {
                    preadv64(
                        self.as_raw_fd(),
                        &iovecs[0],
                        iovecs.len() as c_int,
                        offset as off64_t,
                    )
                };

                if ret >= 0 {
                    let mut total = 0;
                    for vs in bufs {
                        // Each `VolatileSlice` has a "local" bitmap (i.e., the offset 0 in the
                        // bitmap corresponds to the beginning of the `VolatileSlice`)
                        vs.bitmap()
                            .mark_dirty(0, std::cmp::min(ret as usize - total, vs.len()));
                        total += vs.len();
                        if total >= ret as usize {
                            break;
                        }
                    }
                    Ok(ret as usize)
                } else {
                    Err(Error::last_os_error())
                }
            }

            fn write_vectored_at_volatile(
                &self,
                bufs: &[VolatileSlice<RefSlice<AtomicBitmap>>],
                offset: u64,
                flags: Option<oslib::WritevFlags>,
            ) -> Result<usize> {
                let slice_guards: Vec<_> = bufs.iter().map(|s| s.ptr_guard()).collect();
                let iovecs: Vec<libc::iovec> = slice_guards
                    .iter()
                    .map(|s| libc::iovec {
                        iov_base: s.as_ptr() as *mut c_void,
                        iov_len: s.len() as size_t,
                    })
                    .collect();

                if iovecs.is_empty() {
                    return Ok(0);
                }

                // SAFETY: Each `libc::iovec` element is created from a
                // `VolatileSlice` of the guest memory. The pointers are valid
                // because the slice guards are still in scope. We also ensure
                // that we do not read over the slice bounds.
                unsafe {
                    oslib::writev_at(
                        self.as_fd(),
                        iovecs.as_slice(),
                        offset.try_into().unwrap(),
                        flags,
                    )
                }
            }
        }
    };
}

volatile_impl!(File);

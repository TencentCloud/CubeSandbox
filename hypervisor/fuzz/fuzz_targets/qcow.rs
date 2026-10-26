// Copyright 2018 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.

#![no_main]
use libfuzzer_sys::fuzz_target;
use qcow::{QcowFile, RawFile};
use std::ffi;
use std::fs::File;
use std::io::{self, Cursor, Read, Seek, SeekFrom, Write};
use std::mem::size_of;
use std::os::unix::io::{FromRawFd, RawFd};

// Take the first 64 bits of data as an address and the next 64 bits as data to
// store there. The rest of the data is used as a qcow image.
fuzz_target!(|bytes| {
    if bytes.len() < 16 {
        // Need an address and data, each are 8 bytes.
        return;
    }
    let mut disk_image = Cursor::new(bytes);
    let addr = read_u64(&mut disk_image);
    let value = read_u64(&mut disk_image);
    let shm = memfd_create(&ffi::CString::new("fuzz").unwrap(), 0).unwrap();
    let mut disk_file: File = unsafe { File::from_raw_fd(shm) };
    disk_file.write_all(&bytes[16..]).unwrap();
    disk_file.seek(SeekFrom::Start(0)).unwrap();
    if let Ok(mut qcow) = QcowFile::from(RawFile::new(disk_file, false)) {
        if qcow.seek(SeekFrom::Start(addr)).is_ok() {
            let _ = qcow.write_all(&value.to_le_bytes());
        }
    }
});

fn read_u64<T: Read>(readable: &mut T) -> u64 {
    let mut buf = [0u8; size_of::<u64>()];
    readable.read_exact(&mut buf[..]).unwrap();
    u64::from_le_bytes(buf)
}

fn memfd_create(name: &ffi::CStr, flags: u32) -> Result<RawFd, io::Error> {
    let res = unsafe { libc::syscall(libc::SYS_memfd_create, name.as_ptr(), flags) };

    if res < 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(res as RawFd)
    }
}

// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

#![no_main]
use libfuzzer_sys::fuzz_target;
use std::ffi;
use std::fs::File;
use std::io::{self, Read, Seek, SeekFrom, Write};
use std::os::unix::io::{FromRawFd, RawFd};
use vhdx::vhdx::Vhdx;

// Populate the corpus directory with a test file:
// truncate -s 16M /tmp/source
// qemu-img convert -O vhdx /tmp/source fuzz/corpus/vhdx/test.vhdx
// Run with:
// cargo fuzz run vhdx -j 32 -- -max_len=16777216
fuzz_target!(|bytes| {
    let shm = memfd_create(&ffi::CString::new("fuzz").unwrap(), 0).unwrap();
    let mut disk_file: File = unsafe { File::from_raw_fd(shm) };
    disk_file.write_all(&bytes[..]).unwrap();
    disk_file.seek(SeekFrom::Start(0)).unwrap();

    if let Ok(mut vhdx) = Vhdx::new(disk_file) {
        if vhdx.seek(SeekFrom::Start(0)).is_ok() {
            let mut offset = 0;
            while offset < bytes.len() {
                let mut data = vec![0; 8192];
                vhdx.read_exact(&mut data).ok();
                offset += data.len();
            }
        }

        if vhdx.seek(SeekFrom::Start(0)).is_ok() {
            let mut offset = 0;
            while offset < bytes.len() {
                let data = vec![0; 8192];
                vhdx.write_all(&data).ok();
                offset += data.len();
            }
        }
    }
});

fn memfd_create(name: &ffi::CStr, flags: u32) -> Result<RawFd, io::Error> {
    let res = unsafe { libc::syscall(libc::SYS_memfd_create, name.as_ptr(), flags) };

    if res < 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(res as RawFd)
    }
}

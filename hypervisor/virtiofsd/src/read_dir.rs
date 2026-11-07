// Copyright 2020 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.
use crate::filesystem::{DirEntry, DirectoryIterator};

use std::ffi::CStr;
use std::io;
use std::mem::size_of;
use std::ops::{Deref, DerefMut};
use std::os::unix::io::AsRawFd;

use vm_memory::ByteValued;

#[repr(C, packed)]
#[derive(Default, Clone, Copy)]
struct LinuxDirent64 {
    d_ino: libc::ino64_t,
    d_off: libc::off64_t,
    d_reclen: libc::c_ushort,
    d_ty: libc::c_uchar,
}
unsafe impl ByteValued for LinuxDirent64 {}

#[derive(Default)]
pub struct ReadDir<P> {
    buf: P,
    current: usize,
    end: usize,
}

impl<P: DerefMut<Target = [u8]>> ReadDir<P> {
    pub fn new<D: AsRawFd>(dir: &D, offset: u64, buf: P) -> io::Result<Self> {
        // Safe because this doesn't modify any memory and we check the return value.
        let res =
            unsafe { libc::lseek64(dir.as_raw_fd(), offset as libc::off64_t, libc::SEEK_SET) };
        if res < 0 {
            let error = io::Error::last_os_error();
            // NFS cookies are opaque u64 values. The host rejects cookies whose
            // high bit becomes a negative seek offset, even though readdir returns them.
            if error.raw_os_error() == Some(libc::EINVAL) && offset > i64::MAX as u64 {
                return Self::walk_to(dir, offset, buf);
            }
            return Err(error);
        }

        // Safe because we used lseek() to get to the correct position
        unsafe { Self::new_no_seek(dir, buf) }
    }

    fn walk_to<D: AsRawFd>(dir: &D, offset: u64, mut buf: P) -> io::Result<Self> {
        // Safe because seeking doesn't modify memory and the caller holds the directory lock.
        if unsafe { libc::lseek64(dir.as_raw_fd(), 0, libc::SEEK_SET) } < 0 {
            return Err(io::Error::last_os_error());
        }
        loop {
            // Safe because this method rewound the exclusively held directory FD.
            let mut entries = unsafe { Self::new_no_seek(dir, buf)? };
            if entries.remaining() == 0 {
                // EOF before a match means the resume cookie is no longer valid.
                return Err(io::Error::from_raw_os_error(libc::EINVAL));
            }
            if entries.skip_cookie(offset) {
                if entries.remaining() != 0 {
                    return Ok(entries);
                }
                // A cookie points after its entry. At a batch boundary, read the next batch.
                return unsafe { Self::new_no_seek(dir, entries.buf) };
            }
            buf = entries.buf;
        }
    }

    fn skip_cookie(&mut self, offset: u64) -> bool {
        while let Some(entry) = self.next() {
            if entry.offset == offset {
                return true;
            }
        }
        false
    }

    /// Continue reading from the current position in the directory without seeking.
    ///
    /// # Safety
    /// Caller must ensure the current position is valid, for example, by exclusively using this
    /// function on a given FD, potentially repeatedly.
    pub unsafe fn new_no_seek<D: AsRawFd>(dir: &D, mut buf: P) -> io::Result<Self> {
        // Safe because the kernel guarantees that it will only write to `buf` and we check the
        // return value.
        let res = unsafe {
            libc::syscall(
                libc::SYS_getdents64,
                dir.as_raw_fd(),
                buf.as_mut_ptr() as *mut LinuxDirent64,
                buf.len() as libc::c_int,
            )
        };
        if res < 0 {
            return Err(io::Error::last_os_error());
        }

        Ok(ReadDir {
            buf,
            current: 0,
            end: res as usize,
        })
    }
}

impl<P> ReadDir<P> {
    /// Returns the number of bytes from the internal buffer that have not yet been consumed.
    pub fn remaining(&self) -> usize {
        self.end.saturating_sub(self.current)
    }
}

impl<P: Deref<Target = [u8]>> DirectoryIterator for ReadDir<P> {
    fn next(&'_ mut self) -> Option<DirEntry<'_>> {
        let rem = &self.buf[self.current..self.end];
        if rem.is_empty() {
            return None;
        }

        // We only use debug asserts here because these values are coming from the kernel and we
        // trust them implicitly.
        debug_assert!(
            rem.len() >= size_of::<LinuxDirent64>(),
            "not enough space left in `rem`"
        );

        let (front, back) = rem.split_at(size_of::<LinuxDirent64>());

        let dirent64 =
            LinuxDirent64::from_slice(front).expect("unable to get LinuxDirent64 from slice");

        let namelen = dirent64.d_reclen as usize - size_of::<LinuxDirent64>();
        debug_assert!(namelen <= back.len(), "back is smaller than `namelen`");

        // The kernel will pad the name with additional nul bytes until it is 8-byte aligned so
        // we need to strip those off here.
        let name = strip_padding(&back[..namelen]);
        let entry = DirEntry {
            ino: dirent64.d_ino,
            offset: dirent64.d_off as u64,
            type_: dirent64.d_ty as u32,
            name,
        };

        debug_assert!(
            rem.len() >= dirent64.d_reclen as usize,
            "rem is smaller than `d_reclen`"
        );
        self.current += dirent64.d_reclen as usize;

        Some(entry)
    }
}

// Like `CStr::from_bytes_with_nul` but strips any bytes after the first '\0'-byte. Panics if `b`
// doesn't contain any '\0' bytes.
fn strip_padding(b: &[u8]) -> &CStr {
    // It would be nice if we could use memchr here but that's locked behind an unstable gate.
    let pos = b
        .iter()
        .position(|&c| c == 0)
        .expect("`b` doesn't contain any nul bytes");

    // Safe because we are creating this string with the first nul-byte we found so we can
    // guarantee that it is nul-terminated and doesn't contain any interior nuls.
    unsafe { CStr::from_bytes_with_nul_unchecked(&b[..=pos]) }
}

#[cfg(test)]
mod test {
    use super::*;
    use std::fs::{self, File};
    use vmm_sys_util::tempdir::TempDir;

    #[test]
    fn unknown_high_bit_cookie_is_not_reported_as_eof() {
        let directory = TempDir::new_with_prefix("/tmp/virtiofsd-cookie-").unwrap();
        let file = File::open(directory.as_path()).unwrap();
        let error = ReadDir::new(&file, u64::MAX - 1, vec![0; 4096])
            .err()
            .expect("an unknown high-bit cookie must not be reported as EOF");
        assert_eq!(error.raw_os_error(), Some(libc::EINVAL));
    }

    fn cookie_entries(cookies: &[u64]) -> ReadDir<Vec<u8>> {
        let mut buf = Vec::new();
        for &cookie in cookies {
            let entry = LinuxDirent64 {
                d_ino: 1,
                d_off: cookie as libc::off64_t,
                d_reclen: 24,
                d_ty: libc::DT_REG,
            };
            buf.extend_from_slice(entry.as_slice());
            buf.extend_from_slice(b"name\0");
        }
        let end = buf.len();
        ReadDir {
            buf,
            current: 0,
            end,
        }
    }

    #[test]
    fn cookie_scan_preserves_unsigned_cookies_and_resume_position() {
        let cookies = [1, (1 << 63) + 42, u64::MAX - 1];
        for index in 0..cookies.len() {
            let mut entries = cookie_entries(&cookies);
            assert!(entries.skip_cookie(cookies[index]));
            let mut remaining = Vec::new();
            while let Some(entry) = entries.next() {
                remaining.push(entry.offset);
            }
            assert_eq!(remaining, cookies[index + 1..]);
        }
        let mut entries = cookie_entries(&cookies);
        assert!(!entries.skip_cookie(123));
        assert_eq!(entries.remaining(), 0);
        assert!(!cookie_entries(&[]).skip_cookie(123));
    }

    #[test]
    fn cookie_scan_resumes_across_directory_batches_and_at_eof() {
        let directory = TempDir::new_with_prefix("/tmp/virtiofsd-batches-").unwrap();
        for index in 0..24 {
            fs::write(directory.as_path().join(format!("file-{index}")), []).unwrap();
        }
        let file = File::open(directory.as_path()).unwrap();
        let mut all = ReadDir::new(&file, 0, vec![0; 4096]).unwrap();
        let mut expected = Vec::new();
        while let Some(entry) = all.next() {
            expected.push((entry.offset, entry.name.to_owned()));
        }
        assert_eq!(expected.len(), 26);
        for (index, (cookie, _)) in expected.iter().enumerate() {
            let mut resumed = ReadDir::walk_to(&file, *cookie, vec![0; 128]).unwrap();
            let next = resumed
                .next()
                .map(|entry| (entry.offset, entry.name.to_owned()));
            assert_eq!(next.as_ref(), expected.get(index + 1));
        }
    }

    #[test]
    fn cookie_scan_reports_a_cookie_removed_between_reads() {
        let directory = TempDir::new_with_prefix("/tmp/virtiofsd-removed-cookie-").unwrap();
        for index in 0..12 {
            fs::write(directory.as_path().join(format!("file-{index}")), []).unwrap();
        }
        let file = File::open(directory.as_path()).unwrap();
        let mut entries = ReadDir::new(&file, 0, vec![0; 4096]).unwrap();
        let mut before = Vec::new();
        while let Some(entry) = entries.next() {
            before.push(entry.offset);
        }

        for index in 0..8 {
            fs::remove_file(directory.as_path().join(format!("file-{index}"))).unwrap();
        }
        let mut entries = ReadDir::new(&file, 0, vec![0; 4096]).unwrap();
        let mut after = Vec::new();
        while let Some(entry) = entries.next() {
            after.push(entry.offset);
        }
        assert_eq!(after.len(), 6);
        let removed_cookie = before
            .into_iter()
            .find(|cookie| !after.contains(cookie))
            .expect("removing directory entries must remove at least one cookie");
        let error = ReadDir::walk_to(&file, removed_cookie, vec![0; 128])
            .err()
            .expect("a disappeared cookie must not silently skip the remaining entries");
        assert_eq!(error.raw_os_error(), Some(libc::EINVAL));
    }

    #[test]
    fn non_directory_errors_are_not_hidden_by_cookie_fallback() {
        let directory = TempDir::new_with_prefix("/tmp/virtiofsd-errors-").unwrap();
        let path = directory.as_path().join("file");
        fs::write(&path, []).unwrap();
        let file = File::open(path).unwrap();
        let error = ReadDir::new(&file, 0, vec![0; 4096]).err().unwrap();
        assert_eq!(error.raw_os_error(), Some(libc::ENOTDIR));
    }

    #[test]
    #[ignore = "requires VIRTIOFSD_TEST_NFS_DIRECTORY pointing to an existing NFS directory"]
    fn reads_existing_nfs_directory_without_missing_or_duplicate_entries() {
        let path = std::env::var_os("VIRTIOFSD_TEST_NFS_DIRECTORY").unwrap();
        let file = File::open(&path).unwrap();
        let mut expected: Vec<_> = fs::read_dir(&path)
            .unwrap()
            .map(|entry| entry.unwrap().file_name().into_encoded_bytes())
            .collect();
        let mut names = Vec::new();
        let mut offset = 0;
        for _ in 0..expected.len() + 4 {
            let mut entries = ReadDir::new(&file, offset, vec![0; 128]).unwrap();
            if entries.remaining() == 0 {
                names.sort();
                expected.sort();
                assert_eq!(names, expected);
                return;
            }
            while let Some(entry) = entries.next() {
                offset = entry.offset;
                let name = entry.name.to_bytes();
                if name != b"." && name != b".." {
                    names.push(name.to_vec());
                }
            }
        }
        panic!("directory pagination did not reach EOF");
    }

    #[test]
    fn padded_cstrings() {
        assert_eq!(strip_padding(b".\0\0\0\0\0\0\0").to_bytes(), b".");
        assert_eq!(strip_padding(b"..\0\0\0\0\0\0").to_bytes(), b"..");
        assert_eq!(
            strip_padding(b"normal cstring\0").to_bytes(),
            b"normal cstring"
        );
        assert_eq!(strip_padding(b"\0\0\0\0").to_bytes(), b"");
        assert_eq!(
            strip_padding(b"interior\0nul bytes\0\0\0").to_bytes(),
            b"interior"
        );
    }

    #[test]
    #[should_panic(expected = "`b` doesn't contain any nul bytes")]
    fn no_nul_byte() {
        strip_padding(b"no nul bytes in string");
    }
}

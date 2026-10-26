// Copyright 2021 Red Hat, Inc. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

use crate::oslib;
use crate::passthrough::mount_fd::{MPRResult, MountFd, MountFds};
use crate::passthrough::stat::MountId;
use serde::{Deserialize, Serialize};
use std::convert::{TryFrom, TryInto};
use std::ffi::CStr;
use std::fs::File;
use std::io;
use std::os::unix::io::{AsRawFd, RawFd};
use std::sync::Arc;

const EMPTY_CSTR: &[u8] = b"\0";

#[derive(Clone, PartialOrd, Ord, PartialEq, Eq)]
pub struct FileHandle {
    mnt_id: MountId,
    handle: oslib::CFileHandle,
}

pub struct OpenableFileHandle {
    handle: FileHandle,
    mount_fd: Arc<MountFd>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct SerializableFileHandle {
    mnt_id: u64,
    handle_type: i32,
    handle: Vec<u8>,
}

pub enum FileOrHandle {
    File(File),
    Handle(OpenableFileHandle),
    // `io::Error` does not implement `Clone`, so without wrapping it in `Arc`, returning the error
    // anywhere would be impossible without consuming it
    Invalid(Arc<io::Error>),
}

impl FileHandle {
    /// Try to create a file handle for the given file.  In contrast to `from_name_at()`, this will
    /// always return a file handle or an error.
    pub fn from_name_at_fail_hard(dir: &impl AsRawFd, path: &CStr) -> io::Result<Self> {
        let mut mount_id: libc::c_int = 0;
        let mut c_fh = oslib::CFileHandle::default();

        oslib::name_to_handle_at(dir, path, &mut c_fh, &mut mount_id, libc::AT_EMPTY_PATH)?;
        Ok(FileHandle {
            mnt_id: mount_id as MountId,
            handle: c_fh,
        })
    }

    /// Create a file handle for the given file.
    ///
    /// Return `Ok(None)` if no file handle can be generated for this file: Either because the
    /// filesystem does not support it, or because it would require a larger file handle than we
    /// can store.  These are not intermittent failures, i.e. if this function returns `Ok(None)`
    /// for a specific file, it will always return `Ok(None)` for it.  Conversely, if this function
    /// returns `Ok(Some)` at some point, it will never return `Ok(None)` later.
    ///
    /// Return an `io::Error` for all other errors.
    pub fn from_name_at(dir: &impl AsRawFd, path: &CStr) -> io::Result<Option<Self>> {
        match Self::from_name_at_fail_hard(dir, path) {
            Ok(fh) => Ok(Some(fh)),
            Err(err) => match err.raw_os_error() {
                // Filesystem does not support file handles
                Some(libc::EOPNOTSUPP) => Ok(None),
                // Handle would need more bytes than `MAX_HANDLE_SZ`
                Some(libc::EOVERFLOW) => Ok(None),
                // Other error
                _ => Err(err),
            },
        }
    }

    /// Try to create a file handle for `fd`.  In contrast to `from_fd()`, this will always return
    /// a file handle or an error.
    pub fn from_fd_fail_hard(fd: &impl AsRawFd) -> io::Result<Self> {
        // Safe because this is a constant value and a valid C string.
        let empty_path = unsafe { CStr::from_bytes_with_nul_unchecked(EMPTY_CSTR) };
        Self::from_name_at_fail_hard(fd, empty_path)
    }

    /// Create a file handle for `fd`.
    /// This is a wrapper around `from_name_at()` and so has the same interface.
    pub fn from_fd(fd: &impl AsRawFd) -> io::Result<Option<Self>> {
        // Safe because this is a constant value and a valid C string.
        let empty_path = unsafe { CStr::from_bytes_with_nul_unchecked(EMPTY_CSTR) };
        Self::from_name_at(fd, empty_path)
    }

    /**
     * Return an openable copy of the file handle by ensuring that `mount_fds` contains a valid fd
     * for the mount the file handle is for.
     *
     * `reopen_fd` will be invoked to duplicate an `O_PATH` fd with custom `libc::open()` flags.
     */
    pub fn to_openable<F>(
        &self,
        mount_fds: &MountFds,
        reopen_fd: F,
    ) -> MPRResult<OpenableFileHandle>
    where
        F: FnOnce(RawFd, libc::c_int) -> io::Result<File>,
    {
        Ok(OpenableFileHandle {
            handle: self.clone(),
            mount_fd: mount_fds.get(self.mnt_id, reopen_fd)?,
        })
    }
}

impl OpenableFileHandle {
    pub fn inner(&self) -> &FileHandle {
        &self.handle
    }

    /**
     * Open a file handle, using our mount FDs hash map.
     */
    pub fn open(&self, flags: libc::c_int) -> io::Result<File> {
        oslib::open_by_handle_at(self.mount_fd.file(), &self.handle.handle, flags)
    }
}

impl SerializableFileHandle {
    /// Compare `self` against `other`, disregarding the mount ID.  Return a more or less
    /// descriptive error if both handles are not equal.
    pub fn require_equal_without_mount_id(&self, other: &Self) -> Result<(), String> {
        if self.handle_type != other.handle_type {
            Err(format!(
                "File handle type differs: 0x{:x} != 0x{:x}",
                self.handle_type, other.handle_type
            ))
        } else if self.handle != other.handle {
            use std::fmt::Write;
            let mut description = "File handle differs:".to_string();
            for b in self.handle.iter() {
                let _ = write!(&mut description, " {b:02x}");
            }
            description += " !=";
            for b in other.handle.iter() {
                let _ = write!(&mut description, " {b:02x}");
            }
            Err(description)
        } else {
            Ok(())
        }
    }

    /// Compare `self` against `other`.  Return a more or less descriptive error if both handles
    /// are not equal.
    pub fn require_equal(&self, other: &Self) -> Result<(), String> {
        if self.mnt_id != other.mnt_id {
            Err(format!(
                "File handle mount ID differs: {} != {}",
                self.mnt_id, other.mnt_id
            ))
        } else {
            self.require_equal_without_mount_id(other)
        }
    }
}

impl From<&FileHandle> for SerializableFileHandle {
    fn from(fh: &FileHandle) -> SerializableFileHandle {
        SerializableFileHandle {
            mnt_id: fh.mnt_id,
            #[allow(clippy::useless_conversion)]
            handle_type: fh.handle.handle_type().try_into().unwrap(),
            handle: fh.handle.as_bytes().into(),
        }
    }
}

impl From<FileHandle> for SerializableFileHandle {
    fn from(fh: FileHandle) -> SerializableFileHandle {
        (&fh).into()
    }
}

impl TryFrom<&FileOrHandle> for SerializableFileHandle {
    type Error = io::Error;

    fn try_from(file_or_handle: &FileOrHandle) -> io::Result<SerializableFileHandle> {
        match file_or_handle {
            FileOrHandle::Handle(handle) => Ok(handle.inner().into()),
            FileOrHandle::File(file) => {
                FileHandle::from_fd_fail_hard(file).map(SerializableFileHandle::from)
            }
            FileOrHandle::Invalid(err) => Err(io::Error::new(err.kind(), Arc::clone(err))),
        }
    }
}

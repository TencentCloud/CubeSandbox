// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.

use crate::util::other_io_error;
use std::ffi::{CStr, CString};
use std::fs::File;
use std::io;
use std::os::unix::io::{AsRawFd, FromRawFd};

/// Safe wrapper around libc::openat().
pub fn openat(dir_fd: &impl AsRawFd, path: &str, flags: libc::c_int) -> io::Result<File> {
    let path_cstr =
        CString::new(path).map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;

    // Safe because:
    // - CString::new() has returned success and thus guarantees `path_cstr` is a valid
    //   NUL-terminated string
    // - this does not modify any memory
    // - we check the return value
    // We do not check `flags` because if the kernel cannot handle poorly specified flags then we
    // have much bigger problems.
    let fd = unsafe { libc::openat(dir_fd.as_raw_fd(), path_cstr.as_ptr(), flags) };
    if fd >= 0 {
        // Safe because we just opened this fd
        Ok(unsafe { File::from_raw_fd(fd) })
    } else {
        Err(io::Error::last_os_error())
    }
}

/// Open `/proc/self/fd/{fd}` with the given flags to effectively duplicate the given `fd` with new
/// flags (e.g. to turn an `O_PATH` file descriptor into one that can be used for I/O).
pub fn reopen_fd_through_proc(
    fd: &impl AsRawFd,
    flags: libc::c_int,
    proc_self_fd: &File,
) -> io::Result<File> {
    // Clear the `O_NOFOLLOW` flag if it is set since we need to follow the `/proc/self/fd` symlink
    // to get the file.
    openat(
        proc_self_fd,
        format!("{}", fd.as_raw_fd()).as_str(),
        flags & !libc::O_NOFOLLOW,
    )
}

/// Returns true if it's safe to open this inode without O_PATH.
pub fn is_safe_inode(mode: u32) -> bool {
    // Only regular files and directories are considered safe to be opened from the file
    // server without O_PATH.
    matches!(mode & libc::S_IFMT, libc::S_IFREG | libc::S_IFDIR)
}

pub fn ebadf() -> io::Error {
    io::Error::from_raw_os_error(libc::EBADF)
}

pub fn einval() -> io::Error {
    io::Error::from_raw_os_error(libc::EINVAL)
}

pub fn erofs() -> io::Error {
    io::Error::from_raw_os_error(libc::EROFS)
}

/// Looks up an FD's path through /proc/self/fd
pub fn get_path_by_fd(fd: &impl AsRawFd, proc_self_fd: &impl AsRawFd) -> io::Result<CString> {
    let fname = format!("{}\0", fd.as_raw_fd());
    let fname_cstr = CStr::from_bytes_with_nul(fname.as_bytes()).unwrap();

    let max_len = libc::PATH_MAX as usize; // does not include final NUL byte
    let mut link_target = vec![0u8; max_len + 1]; // make space for NUL byte

    let ret = unsafe {
        libc::readlinkat(
            proc_self_fd.as_raw_fd(),
            fname_cstr.as_ptr(),
            link_target.as_mut_ptr().cast::<libc::c_char>(),
            max_len,
        )
    };
    if ret < 0 {
        return Err(io::Error::last_os_error());
    } else if ret as usize == max_len {
        return Err(other_io_error("Path too long".to_string()));
    }

    link_target.truncate(ret as usize + 1);
    let link_target_cstring = CString::from_vec_with_nul(link_target).map_err(other_io_error)?;
    let link_target_str = link_target_cstring.to_string_lossy();

    let pre_slash = link_target_str.split('/').next().unwrap();
    if pre_slash.contains(':') {
        return Err(other_io_error("Not a file".to_string()));
    }

    if link_target_str.ends_with(" (deleted)") {
        return Err(other_io_error("Inode deleted".to_string()));
    }

    Ok(link_target_cstring)
}

/// Debugging helper function: Turn the given file descriptor into a string representation we can
/// show the user.  If `proc_self_fd` is given, try to obtain the actual path through the symlink
/// in /proc/self/fd; otherwise (or on error), just print the integer representation (as
/// "{fd:%i}").
pub fn printable_fd(fd: &impl AsRawFd, proc_self_fd: Option<&impl AsRawFd>) -> String {
    if let Some(Ok(path)) = proc_self_fd.map(|psf| get_path_by_fd(fd, psf)) {
        match path.into_string() {
            Ok(s) => s,
            Err(err) => err.into_cstring().to_string_lossy().into_owned(),
        }
    } else {
        format!("{{fd:{}}}", fd.as_raw_fd())
    }
}

/// Verify that a dentry name coming from a FUSE request is syntactically a
/// single path component, as any legitimate Linux VFS request would already
/// guarantee before dispatching to a FUSE server.
///
/// The VFS resolves the leading portion of the user-supplied pathname inside
/// the guest kernel (see `fs/namei.c::filename_create()` and friends) and only
/// hands FUSE the final path component; therefore the following properties
/// must hold on the wire:
///
///   * name is non-empty
///   * name does not contain `/`
///   * name is neither `.` nor `..`
///   * name does not contain an interior NUL byte
///
/// A compromised guest userspace could nevertheless *forge* a FUSE request
/// where `name` is a multi-component path such as `"../etc/passwd"`. If we
/// then blindly hand that string to `openat(2)` / `symlinkat(2)` /
/// `mkdirat(2)` / `linkat(2)` / `renameat2(2)` etc., the kernel would happily
/// walk it and let the write escape the intended parent directory. This
/// validator closes that gap while remaining zero-cost for legitimate VFS
/// traffic (which never violates any of the above).
pub fn validate_dentry_name(name: &CStr) -> io::Result<()> {
    let bytes = name.to_bytes();
    // An empty name matches the kernel's own errno for e.g. `openat(fd, "")`.
    if bytes.is_empty() {
        return Err(io::Error::from_raw_os_error(libc::ENOENT));
    }
    // Reject `.`, `..` and any embedded `/`. A legitimate VFS request cannot
    // hit any of these: the guest kernel resolves the pathname down to the
    // final component before dispatching to FUSE. A forged FUSE request that
    // does contain them would otherwise let the syscall walk outside the
    // intended parent directory (`../etc/passwd`) or violate the
    // "single new child" contract (`.`, `foo/bar`).
    //
    // Note: interior NUL bytes are already ruled out by the `&CStr` type
    // invariant -- `server.rs` performs `CStr::from_bytes_with_nul` before
    // we ever get here, so we don't need to re-check that.
    if bytes == b"." || bytes == b".." || bytes.contains(&b'/') {
        return Err(einval());
    }
    Ok(())
}

pub fn relative_path<'a>(path: &'a CStr, prefix: &CStr) -> io::Result<&'a CStr> {
    let mut relative_path = path
        .to_bytes_with_nul()
        .strip_prefix(prefix.to_bytes())
        .ok_or_else(|| {
            other_io_error(format!(
                "Path {path:?} is outside the directory ({prefix:?})"
            ))
        })?;

    // Remove leading / if left
    while let Some(prefixless) = relative_path.strip_prefix(b"/") {
        relative_path = prefixless;
    }

    // Must succeed: Was a `CStr` before, converted to `&[u8]` via `to_bytes_with_nul()`, so must
    // still contain exactly one NUL byte at the end of the slice
    Ok(CStr::from_bytes_with_nul(relative_path).unwrap())
}

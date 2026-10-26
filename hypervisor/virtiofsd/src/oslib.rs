// SPDX-License-Identifier: BSD-3-Clause

use bitflags::bitflags;
use std::ffi::{CStr, CString};
use std::fs::File;
use std::io::{self, Error, Result};
use std::os::unix::io::{AsRawFd, BorrowedFd, RawFd};
use std::os::unix::prelude::FromRawFd;

// A helper function that check the return value of a C function call
// and wraps it in a `Result` type, returning the `errno` code as `Err`.
fn check_retval<T: From<i8> + PartialEq>(t: T) -> Result<T> {
    if t == T::from(-1_i8) {
        Err(Error::last_os_error())
    } else {
        Ok(t)
    }
}

/// Simple object to collect basic facts about the OS,
/// such as available syscalls.
pub struct OsFacts {
    pub has_openat2: bool,
}

#[allow(clippy::new_without_default)]
impl OsFacts {
    /// This object should only be constructed using new.
    #[must_use]
    pub fn new() -> Self {
        // Checking for `openat2()` since it first appeared in Linux 5.6.
        // SAFETY: all-zero byte-pattern is a valid `libc::open_how`
        let how: libc::open_how = unsafe { std::mem::zeroed() };
        let cwd = CString::new(".").unwrap();
        // SAFETY: `cwd.as_ptr()` points to a valid NUL-terminated string,
        // and the `how` pointer is a valid pointer to an `open_how` struct.
        let fd = unsafe {
            libc::syscall(
                libc::SYS_openat2,
                libc::AT_FDCWD,
                cwd.as_ptr(),
                std::ptr::addr_of!(how),
                std::mem::size_of::<libc::open_how>(),
            )
        };

        let has_openat2 = fd >= 0;
        if has_openat2 {
            // SAFETY: `fd` is an open file descriptor
            unsafe {
                libc::close(fd as libc::c_int);
            }
        }

        Self { has_openat2 }
    }
}

/// Safe wrapper for `mount(2)`
///
/// # Errors
///
/// Will return `Err(errno)` if `mount(2)` fails.
/// Each filesystem type may have its own special errors and its own special behavior,
/// see `mount(2)` and the linux source kernel for details.
///
/// # Panics
///
/// This function panics if the strings `source`, `target` or `fstype` contain an internal 0 byte.
pub fn mount(source: Option<&str>, target: &str, fstype: Option<&str>, flags: u64) -> Result<()> {
    let source = CString::new(source.unwrap_or("")).unwrap();
    let source = source.as_ptr();

    let target = CString::new(target).unwrap();
    let target = target.as_ptr();

    let fstype = CString::new(fstype.unwrap_or("")).unwrap();
    let fstype = fstype.as_ptr();

    // Safety: `source`, `target` or `fstype` are a valid C string pointers
    check_retval(unsafe { libc::mount(source, target, fstype, flags, std::ptr::null()) })?;
    Ok(())
}

/// Safe wrapper for `umount2(2)`
///
/// # Errors
///
/// Will return `Err(errno)` if `umount2(2)` fails.
/// Each filesystem type may have its own special errors and its own special behavior,
/// see `umount2(2)` and the linux source kernel for details.
///
/// # Panics
///
/// This function panics if the strings `target` contains an internal 0 byte.
pub fn umount2(target: &str, flags: i32) -> Result<()> {
    let target = CString::new(target).unwrap();
    let target = target.as_ptr();

    // Safety: `target` is a valid C string pointer
    check_retval(unsafe { libc::umount2(target, flags) })?;
    Ok(())
}

/// Safe wrapper for `fchdir(2)`
///
/// # Errors
///
/// Will return `Err(errno)` if `fchdir(2)` fails.
/// Each filesystem type may have its own special errors, see `fchdir(2)` for details.
pub fn fchdir(fd: RawFd) -> Result<()> {
    check_retval(unsafe { libc::fchdir(fd) })?;
    Ok(())
}

/// Safe wrapper for `umask(2)`
pub fn umask(mask: u32) -> u32 {
    // SAFETY: this call doesn't modify any memory and there is no need
    // to check the return value because this system call always succeeds.
    unsafe { libc::umask(mask) }
}

/// An RAII implementation of a scoped file mode creation mask (umask), it set the
/// new umask. When this structure is dropped (falls out of scope), it set the previous
/// value of the mask.
pub struct ScopedUmask {
    umask: libc::mode_t,
}

impl ScopedUmask {
    pub fn new(new_umask: u32) -> Self {
        Self {
            umask: umask(new_umask),
        }
    }
}

impl Drop for ScopedUmask {
    fn drop(&mut self) {
        umask(self.umask);
    }
}

/// Safe wrapper around `openat(2)`.
///
/// # Errors
///
/// Will return `Err(errno)` if `openat(2)` fails,
/// see `openat(2)` for details.
pub fn openat(dir: &impl AsRawFd, pathname: &CStr, flags: i32, mode: Option<u32>) -> Result<RawFd> {
    let mode = u64::from(mode.unwrap_or(0));

    // SAFETY: `pathname` points to a valid NUL-terminated string.
    // However, the caller must ensure that `dir` can provide a valid file descriptor.
    check_retval(unsafe {
        libc::openat(
            dir.as_raw_fd(),
            pathname.as_ptr(),
            flags as libc::c_int,
            mode,
        )
    })
}

/// An utility function that uses `openat2(2)` to restrict the how the provided pathname
/// is resolved. It uses the following flags:
/// - `RESOLVE_IN_ROOT`: Treat the directory referred to by dirfd as the root directory while
/// resolving pathname. This has the effect as though virtiofsd had used chroot(2) to modify its
/// root directory to dirfd.
/// - `RESOLVE_NO_MAGICLINKS`: Disallow all magic-link (i.e., proc(2) link-like files) resolution
/// during path resolution.
///
/// Additionally, the flags `O_NOFOLLOW` and `O_CLOEXEC` are added.
///
/// # Error
///
/// Will return `Err(errno)` if `openat2(2)` fails, see the man page for details.
///
/// # Safety
///
/// The caller must ensure that dirfd is a valid file descriptor.
pub fn do_open_relative_to(
    dir: &impl AsRawFd,
    pathname: &CStr,
    flags: i32,
    mode: Option<u32>,
) -> Result<RawFd> {
    // `openat2(2)` returns an error if `how.mode` contains bits other than those in range 07777,
    // let's ignore the extra bits to be compatible with `openat(2)`.
    let mode = u64::from(mode.unwrap_or(0)) & 0o7777;

    // SAFETY: all-zero byte-pattern represents a valid `libc::open_how`
    let mut how: libc::open_how = unsafe { std::mem::zeroed() };
    how.resolve = libc::RESOLVE_IN_ROOT | libc::RESOLVE_NO_MAGICLINKS;
    how.flags = flags as u64;
    how.mode = mode;

    // SAFETY: `pathname` points to a valid NUL-terminated string, and the `how` pointer is a valid
    // pointer to an `open_how` struct. However, the caller must ensure that `dir` can provide a
    // valid file descriptor (this can be changed to BorrowedFd).
    check_retval(unsafe {
        libc::syscall(
            libc::SYS_openat2,
            dir.as_raw_fd(),
            pathname.as_ptr(),
            std::ptr::addr_of!(how),
            std::mem::size_of::<libc::open_how>(),
        )
    } as RawFd)
}

mod filehandle {
    const MAX_HANDLE_SZ: usize = 128;

    #[derive(Clone, PartialOrd, Ord, PartialEq, Eq)]
    #[repr(C)]
    pub struct CFileHandle {
        handle_bytes: libc::c_uint,
        handle_type: libc::c_int,
        f_handle: [u8; MAX_HANDLE_SZ],
    }

    impl Default for CFileHandle {
        fn default() -> Self {
            CFileHandle {
                handle_bytes: MAX_HANDLE_SZ as libc::c_uint,
                handle_type: 0,
                f_handle: [0; MAX_HANDLE_SZ],
            }
        }
    }

    impl CFileHandle {
        pub fn as_bytes(&self) -> &[u8] {
            &self.f_handle[..(self.handle_bytes as usize)]
        }

        pub fn handle_type(&self) -> libc::c_int {
            self.handle_type
        }
    }

    extern "C" {
        pub fn name_to_handle_at(
            dirfd: libc::c_int,
            pathname: *const libc::c_char,
            file_handle: *mut CFileHandle,
            mount_id: *mut libc::c_int,
            flags: libc::c_int,
        ) -> libc::c_int;

        // Technically `file_handle` should be a `mut` pointer, but `open_by_handle_at()` is specified
        // not to change it, so we can declare it `const`.
        pub fn open_by_handle_at(
            mount_fd: libc::c_int,
            file_handle: *const CFileHandle,
            flags: libc::c_int,
        ) -> libc::c_int;
    }
}
pub use filehandle::CFileHandle;

pub fn name_to_handle_at(
    dirfd: &impl AsRawFd,
    pathname: &CStr,
    file_handle: &mut CFileHandle,
    mount_id: &mut libc::c_int,
    flags: libc::c_int,
) -> Result<()> {
    // SAFETY: `dirfd` is a valid file descriptor, `file_handle`
    // is a valid reference to `CFileHandle`, and `mount_id` is
    // valid reference to an `int`
    check_retval(unsafe {
        filehandle::name_to_handle_at(
            dirfd.as_raw_fd(),
            pathname.as_ptr(),
            file_handle,
            mount_id,
            flags,
        )
    })?;
    Ok(())
}

pub fn open_by_handle_at(
    mount_fd: &impl AsRawFd,
    file_handle: &CFileHandle,
    flags: libc::c_int,
) -> Result<File> {
    // SAFETY: `mount_fd` is a valid file descriptor and `file_handle`
    // is a valid reference to `CFileHandle`
    let fd = check_retval(unsafe {
        filehandle::open_by_handle_at(mount_fd.as_raw_fd(), file_handle, flags)
    })?;

    // SAFETY: `open_by_handle_at()` guarantees `fd` is a valid file descriptor
    Ok(unsafe { File::from_raw_fd(fd) })
}

mod writev {
    /// musl does not provide a wrapper for the `pwritev2(2)` system call,
    /// we need to call it using `syscall(2)`.

    #[cfg(target_env = "gnu")]
    pub use libc::pwritev2;

    #[cfg(target_env = "musl")]
    pub unsafe fn pwritev2(
        fd: libc::c_int,
        iov: *const libc::iovec,
        iovcnt: libc::c_int,
        offset: libc::off_t,
        flags: libc::c_int,
    ) -> libc::ssize_t {
        // The `pwritev2(2)` syscall expects to receive the 64-bit offset split in
        // its high and low parts (see `syscall(2)`). On 64-bit architectures we
        // set `lo_off=offset` and `hi_off=0` (glibc does it), since `hi_off` is cleared,
        // so we need to make sure of not clear the higher 32 bits of `lo_off`, otherwise
        // the offset will be 0 on 64-bit architectures.
        let lo_off = offset as libc::c_long; // warn: do not clear the higher 32 bits
        let hi_off = (offset as u64).checked_shr(libc::c_long::BITS).unwrap_or(0) as libc::c_long;
        unsafe {
            libc::syscall(libc::SYS_pwritev2, fd, iov, iovcnt, lo_off, hi_off, flags)
                as libc::ssize_t
        }
    }
}

// We cannot use libc::RWF_HIPRI, etc, because these constants are not defined in musl.
bitflags! {
    /// A bitwise OR of zero or more flags passed in as a parameter to the
    /// write vectored function `writev_at()`.
    pub struct WritevFlags: i32 {
        /// High priority write. Allows block-based filesystems to use polling of the device, which
        /// provides lower latency, but may use additional resources. (Currently, this feature is
        /// usable only on a file descriptor opened using the O_DIRECT flag.)
        const RWF_HIPRI = 0x00000001;

        /// Provide a per-write equivalent of the O_DSYNC open(2) flag. Its effect applies
        /// only to the data range written by the system call.
        const RWF_DSYNC = 0x00000002;

        /// Provide a per-write equivalent of the O_SYNC open(2) flag. Its effect applies only
        /// to the data range written by the system call.
        const RWF_SYNC = 0x00000004;

        /// Provide a per-write equivalent of the O_APPEND open(2) flag. Its effect applies only
        /// to the data range written by the system call. The offset argument does not affect the
        /// write operation; the data is always appended to the end of the file.
        /// However, if the offset argument is -1, the current file offset is updated.
        const RWF_APPEND = 0x00000010;
    }
}

#[cfg(target_env = "gnu")]
mod writev_test {
    // Lets make sure (at compile time) that the WritevFlags don't go out of sync with the libc
    const _: () = assert!(
        super::WritevFlags::RWF_HIPRI.bits() == libc::RWF_HIPRI,
        "invalid RWF_HIPRI value"
    );
    const _: () = assert!(
        super::WritevFlags::RWF_DSYNC.bits() == libc::RWF_DSYNC,
        "invalid RWF_DSYNC value"
    );
    const _: () = assert!(
        super::WritevFlags::RWF_SYNC.bits() == libc::RWF_SYNC,
        "invalid RWF_SYNC value"
    );
    const _: () = assert!(
        super::WritevFlags::RWF_APPEND.bits() == libc::RWF_APPEND,
        "invalid RWF_APPEND value"
    );
}

/// Safe wrapper for `pwritev2(2)`
///
/// This system call is similar `pwritev(2)`, but add a new argument,
/// flags, which modifies the behavior on a per-call basis.
/// Unlike `pwritev(2)`, if the offset argument is -1, then the current file offset
/// is used and updated.
///
/// # Errors
///
/// Will return `Err(errno)` if `pwritev2(2)` fails, see `pwritev2(2)` for details.
///
/// # Safety
///
/// The caller must ensure that each iovec element is valid (i.e., it has a valid `iov_base`
/// pointer and `iov_len`).
pub unsafe fn writev_at(
    fd: BorrowedFd,
    iovecs: &[libc::iovec],
    offset: i64,
    flags: Option<WritevFlags>,
) -> Result<usize> {
    let flags = flags.unwrap_or(WritevFlags::empty());
    // SAFETY: `fd` is a valid filed descriptor, `iov` is a valid pointer
    // to the iovec slice `ìovecs` of `iovcnt` elements. However, the caller
    // must ensure that each iovec element has a valid `iov_base` pointer and `iov_len`.
    let bytes_written = check_retval(unsafe {
        writev::pwritev2(
            fd.as_raw_fd(),
            iovecs.as_ptr(),
            iovecs.len() as libc::c_int,
            offset,
            flags.bits(),
        )
    })?;
    Ok(bytes_written as usize)
}

pub struct PipeReader(File);

impl io::Read for PipeReader {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        self.0.read(buf)
    }
}

pub struct PipeWriter(File);

impl io::Write for PipeWriter {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        self.0.write(buf)
    }

    fn flush(&mut self) -> io::Result<()> {
        self.0.flush()
    }
}

pub fn pipe() -> io::Result<(PipeReader, PipeWriter)> {
    let mut fds: [RawFd; 2] = [-1, -1];
    let ret = unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_CLOEXEC) };
    if ret == -1 {
        Err(io::Error::last_os_error())
    } else {
        Ok((
            PipeReader(unsafe { File::from_raw_fd(fds[0]) }),
            PipeWriter(unsafe { File::from_raw_fd(fds[1]) }),
        ))
    }
}

// We want credential changes to be per-thread because otherwise
// we might interfere with operations being carried out on other
// threads with different uids/gids. However, posix requires that
// all threads in a process share the same credentials. To do this
// libc uses signals to ensure that when one thread changes its
// credentials the other threads do the same thing.
//
// So instead we invoke the syscall directly in order to get around
// this limitation. Another option is to use the setfsuid and
// setfsgid systems calls. However since those calls have no way to
// return an error, it's preferable to do this instead.
/// Set effective user ID
pub fn seteffuid(uid: libc::uid_t) -> io::Result<()> {
    check_retval(unsafe { libc::syscall(libc::SYS_setresuid, -1, uid, -1) })?;
    Ok(())
}

/// Set effective group ID
pub fn seteffgid(gid: libc::gid_t) -> io::Result<()> {
    check_retval(unsafe { libc::syscall(libc::SYS_setresgid, -1, gid, -1) })?;
    Ok(())
}

/// Set supplementary group
pub fn setsupgroup(gid: libc::gid_t) -> io::Result<()> {
    check_retval(unsafe { libc::setgroups(1, &gid) })?;
    Ok(())
}

/// Drop all supplementary groups
pub fn dropsupgroups() -> io::Result<()> {
    check_retval(unsafe { libc::setgroups(0, std::ptr::null()) })?;
    Ok(())
}

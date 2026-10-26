// Copyright (c) 2022 Alibaba Cloud
//
// SPDX-License-Identifier: Apache-2.0
//

use std::ffi::{CString, OsStr};
use std::fs::{self, File, Metadata, OpenOptions};
use std::io::{Error, ErrorKind, Result};
use std::ops::Deref;
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::OpenOptionsExt;
use std::os::unix::io::{AsRawFd, FromRawFd, RawFd};
use std::path::{Component, Path, PathBuf};

use crate::scoped_join;

/// A safe version of [`PathBuf`] pinned to an underlying filesystem object to protect from
/// `TOCTTOU` style of attacks.
///
/// A [`PinnedPathBuf`] is a resolved path buffer pinned to an underlying filesystem object, which
/// guarantees:
/// - the value of [`PinnedPathBuf::as_path()`] never changes.
/// - the path returned by [`PinnedPathBuf::as_path()`] is always a symlink.
/// - the filesystem object referenced by the symlink [`PinnedPathBuf::as_path()`] never changes.
/// - the value of [`PinnedPathBuf::target()`] never changes.
///
/// Note:
/// - Though the filesystem object referenced by the symlink [`PinnedPathBuf::as_path()`] never
///   changes, the value of `fs::read_link(PinnedPathBuf::as_path())` may change due to filesystem
///   operations.
/// - The value of [`PinnedPathBuf::target()`] is a cached version of
///   `fs::read_link(PinnedPathBuf::as_path())` generated when creating the `PinnedPathBuf` object.
/// - It's a sign of possible attacks if `[PinnedPathBuf::target()]` doesn't match
///   `fs::read_link(PinnedPathBuf::as_path())`.
/// - Once the [`PinnedPathBuf`] object gets dropped, the [`Path`] returned by
///   [`PinnedPathBuf::as_path()`] becomes invalid.
///
/// With normal [`PathBuf`], there's a race window for attackers between time to validate a path and
/// time to use the path. An attacker may maliciously change filesystem object referenced by the
/// path by using symlinks to compose an attack.
///
/// The [`PinnedPathBuf`] is introduced to protect from such attacks, by using the
/// `/proc/self/fd/xxx` files on Linux. The `/proc/self/fd/xxx` file on Linux is a symlink to the
/// real target corresponding to the process's file descriptor `xxx`. And the target filesystem
/// object referenced by the symlink will be kept stable until the file descriptor has been closed.
/// Combined with `O_PATH`, a safe version of `PathBuf` could be built by:
/// - Generate a safe path from `root` and `path` by using [`crate::scoped_join()`].
/// - Open the safe path with O_PATH | O_CLOEXEC flags, say the fd number is `fd_num`.
/// - Read the symlink target of `/proc/self/fd/fd_num`.
/// - Compare the symlink target with the safe path, it's safe if these two paths equal.
/// - Use the proc file path as a safe version of [`PathBuf`].
/// - Close the `fd_num` when dropping the [`PinnedPathBuf`] object.
#[derive(Debug)]
pub struct PinnedPathBuf {
    handle: File,
    path: PathBuf,
    target: PathBuf,
}

impl PinnedPathBuf {
    /// Create a [`PinnedPathBuf`] object from `root` and `path`.
    ///
    /// The `path` must be a subdirectory of `root`, otherwise error will be returned.
    pub fn new<R: AsRef<Path>, U: AsRef<Path>>(root: R, path: U) -> Result<Self> {
        let path = scoped_join(root, path)?;
        Self::from_path(path)
    }

    /// Create a `PinnedPathBuf` from `path`.
    ///
    /// If the resolved value of `path` doesn't equal to `path`, an error will be returned.
    pub fn from_path<P: AsRef<Path>>(orig_path: P) -> Result<Self> {
        let orig_path = orig_path.as_ref();
        let handle = Self::open_by_path(orig_path)?;
        Self::new_from_file(handle, orig_path)
    }

    /// Try to clone the [`PinnedPathBuf`] object.
    pub fn try_clone(&self) -> Result<Self> {
        let fd = unsafe { libc::dup(self.path_fd()) };
        if fd < 0 {
            Err(Error::last_os_error())
        } else {
            Ok(Self {
                handle: unsafe { File::from_raw_fd(fd) },
                path: Self::get_proc_path(fd),
                target: self.target.clone(),
            })
        }
    }

    /// Return the underlying file descriptor representing the pinned path.
    ///
    /// Following operations are supported by the returned `RawFd`:
    /// - fchdir
    /// - fstat/fstatfs
    /// - openat/linkat/fchownat/fstatat/readlinkat/mkdirat/*at
    /// - fcntl(F_GETFD, F_SETFD, F_GETFL)
    pub fn path_fd(&self) -> RawFd {
        self.handle.as_raw_fd()
    }

    /// Get the symlink path referring the target filesystem object.
    pub fn as_path(&self) -> &Path {
        self.path.as_path()
    }

    /// Get the cached real path of the target filesystem object.
    ///
    /// The target path is cached version of `fs::read_link(PinnedPathBuf::as_path())` generated
    /// when creating the `PinnedPathBuf` object. On the other hand, the value of
    /// `fs::read_link(PinnedPathBuf::as_path())` may change due to underlying filesystem operations.
    /// So it's a sign of possible attacks if `PinnedPathBuf::target()` does not match
    /// `fs::read_link(PinnedPathBuf::as_path())`.
    pub fn target(&self) -> &Path {
        &self.target
    }

    /// Get [`Metadata`] about the path handle.
    pub fn metadata(&self) -> Result<Metadata> {
        self.handle.metadata()
    }

    /// Open a direct child of the filesystem objected referenced by the `PinnedPathBuf` object.
    pub fn open_child(&self, path_comp: &OsStr) -> Result<Self> {
        let name = Self::prepare_path_component(path_comp)?;
        let oflags = libc::O_PATH | libc::O_CLOEXEC;
        let res = unsafe { libc::openat(self.path_fd(), name.as_ptr(), oflags, 0) };
        if res < 0 {
            Err(Error::last_os_error())
        } else {
            let handle = unsafe { File::from_raw_fd(res) };
            Self::new_from_file(handle, self.target.join(path_comp))
        }
    }

    /// Create or open a child directory if current object is a directory.
    pub fn mkdir(&self, path_comp: &OsStr, mode: libc::mode_t) -> Result<Self> {
        let path_name = Self::prepare_path_component(path_comp)?;
        let res = unsafe { libc::mkdirat(self.handle.as_raw_fd(), path_name.as_ptr(), mode) };
        if res < 0 {
            Err(Error::last_os_error())
        } else {
            self.open_child(path_comp)
        }
    }

    /// Open a directory/file by path.
    ///
    /// Obtain a file descriptor that can be used for two purposes:
    /// - indicate a location in the filesystem tree
    /// - perform operations that act purely at the file descriptor level
    fn open_by_path<P: AsRef<Path>>(path: P) -> Result<File> {
        // When O_PATH is specified in flags, flag bits other than O_CLOEXEC, O_DIRECTORY, and
        // O_NOFOLLOW are ignored.
        let o_flags = libc::O_PATH | libc::O_CLOEXEC;
        OpenOptions::new()
            .read(true)
            .custom_flags(o_flags)
            .open(path.as_ref())
    }

    fn get_proc_path<F: AsRawFd>(file: F) -> PathBuf {
        PathBuf::from(format!("/proc/self/fd/{}", file.as_raw_fd()))
    }

    fn new_from_file<P: AsRef<Path>>(handle: File, orig_path: P) -> Result<Self> {
        let path = Self::get_proc_path(handle.as_raw_fd());
        let link_path = fs::read_link(path.as_path())?;
        if link_path != orig_path.as_ref() {
            Err(Error::new(
                ErrorKind::Other,
                format!(
                    "Path changed from {} to {} on open, possible attack",
                    orig_path.as_ref().display(),
                    link_path.display()
                ),
            ))
        } else {
            Ok(PinnedPathBuf {
                handle,
                path,
                target: link_path,
            })
        }
    }

    #[inline]
    fn prepare_path_component(path_comp: &OsStr) -> Result<CString> {
        let path = Path::new(path_comp);
        let mut comps = path.components();
        let name = comps.next();
        if !matches!(name, Some(Component::Normal(_))) || comps.next().is_some() {
            return Err(Error::new(
                ErrorKind::Other,
                format!("Path component {} is invalid", path_comp.to_string_lossy()),
            ));
        }
        let name = name.unwrap();
        if name.as_os_str() != path_comp {
            return Err(Error::new(
                ErrorKind::Other,
                format!("Path component {} is invalid", path_comp.to_string_lossy()),
            ));
        }

        CString::new(path_comp.as_bytes()).map_err(|_e| {
            Error::new(
                ErrorKind::Other,
                format!("Path component {} is invalid", path_comp.to_string_lossy()),
            )
        })
    }
}

impl Deref for PinnedPathBuf {
    type Target = PathBuf;

    fn deref(&self) -> &Self::Target {
        &self.path
    }
}

impl AsRef<Path> for PinnedPathBuf {
    fn as_ref(&self) -> &Path {
        self.path.as_path()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::ffi::OsString;
    use std::fs::DirBuilder;
    use std::io::Write;
    use std::os::unix::fs::{symlink, MetadataExt};
    use std::sync::{Arc, Barrier};
    use std::thread;

    #[test]
    fn test_pinned_path_buf() {
        // Create a root directory, which itself contains symlinks.
        let rootfs_dir = tempfile::tempdir().expect("failed to create tmpdir");
        DirBuilder::new()
            .create(rootfs_dir.path().join("b"))
            .unwrap();
        symlink(rootfs_dir.path().join("b"), rootfs_dir.path().join("a")).unwrap();
        let rootfs_path = &rootfs_dir.path().join("a");

        // Create a file and a symlink to it.
        fs::create_dir(rootfs_path.join("symlink_dir")).unwrap();
        symlink("/endpoint", rootfs_path.join("symlink_dir/endpoint")).unwrap();
        fs::write(rootfs_path.join("endpoint"), "test").unwrap();

        // Pin the target and validate the path/content.
        let path = PinnedPathBuf::new(rootfs_path, "symlink_dir/endpoint").unwrap();
        assert!(!path.is_dir());
        let path_ref = path.deref();
        let target = fs::read_link(path_ref).unwrap();
        assert_eq!(target, rootfs_path.join("endpoint").canonicalize().unwrap());
        let content = fs::read_to_string(&path).unwrap();
        assert_eq!(&content, "test");

        // Remove the target file and validate that we could still read data from the pinned path.
        fs::remove_file(&target).unwrap();
        fs::read_to_string(&target).unwrap_err();
        let content = fs::read_to_string(&path).unwrap();
        assert_eq!(&content, "test");
    }

    #[test]
    fn test_pinned_path_buf_race() {
        let root_dir = tempfile::tempdir().expect("failed to create tmpdir");
        let root_path = root_dir.path();
        let barrier = Arc::new(Barrier::new(2));

        fs::write(root_path.join("a"), b"a").unwrap();
        fs::write(root_path.join("b"), b"b").unwrap();
        fs::write(root_path.join("c"), b"c").unwrap();
        symlink("a", root_path.join("s")).unwrap();

        let root_path2 = root_path.to_path_buf();
        let barrier2 = barrier.clone();
        let thread = thread::spawn(move || {
            // step 1
            barrier2.wait();
            fs::remove_file(root_path2.join("a")).unwrap();
            symlink("b", root_path2.join("a")).unwrap();
            barrier2.wait();

            // step 2
            barrier2.wait();
            fs::remove_file(root_path2.join("b")).unwrap();
            symlink("c", root_path2.join("b")).unwrap();
            barrier2.wait();
        });

        let path = scoped_join(&root_path, "s").unwrap();
        let data = fs::read_to_string(&path).unwrap();
        assert_eq!(&data, "a");
        assert!(path.is_file());
        barrier.wait();
        barrier.wait();
        // Verify the target has been redirected.
        let data = fs::read_to_string(&path).unwrap();
        assert_eq!(&data, "b");
        PinnedPathBuf::from_path(&path).unwrap_err();

        let pinned_path = PinnedPathBuf::new(&root_path, "s").unwrap();
        let data = fs::read_to_string(&pinned_path).unwrap();
        assert_eq!(&data, "b");

        // step2
        barrier.wait();
        barrier.wait();
        // Verify it still points to the old target.
        let data = fs::read_to_string(&pinned_path).unwrap();
        assert_eq!(&data, "b");

        thread.join().unwrap();
    }

    #[test]
    fn test_new_pinned_path_buf() {
        let rootfs_dir = tempfile::tempdir().expect("failed to create tmpdir");
        let rootfs_path = rootfs_dir.path();
        let path = PinnedPathBuf::from_path(rootfs_path).unwrap();
        let _ = OpenOptions::new().read(true).open(&path).unwrap();
    }

    #[test]
    fn test_pinned_path_try_clone() {
        let rootfs_dir = tempfile::tempdir().expect("failed to create tmpdir");
        let rootfs_path = rootfs_dir.path();
        let path = PinnedPathBuf::from_path(rootfs_path).unwrap();
        let path2 = path.try_clone().unwrap();
        assert_ne!(path.as_path(), path2.as_path());
    }

    #[test]
    fn test_new_pinned_path_buf_from_nonexist_file() {
        let rootfs_dir = tempfile::tempdir().expect("failed to create tmpdir");
        let rootfs_path = rootfs_dir.path();
        PinnedPathBuf::new(rootfs_path, "does_not_exist").unwrap_err();
    }

    #[allow(clippy::zero_prefixed_literal)]
    #[test]
    fn test_new_pinned_path_buf_without_read_perm() {
        let rootfs_dir = tempfile::tempdir().expect("failed to create tmpdir");
        let rootfs_path = rootfs_dir.path();
        let path = rootfs_path.join("write_only_file");

        let mut file = OpenOptions::new()
            .read(false)
            .write(true)
            .create(true)
            .mode(0o200)
            .open(&path)
            .unwrap();
        file.write_all(&[0xa5u8]).unwrap();
        let md = fs::metadata(&path).unwrap();
        let umask = unsafe { libc::umask(0022) };
        unsafe { libc::umask(umask) };
        assert_eq!(md.mode() & 0o700, 0o200 & !umask);
        PinnedPathBuf::from_path(&path).unwrap();
    }

    #[test]
    fn test_pinned_path_buf_path_fd() {
        let rootfs_dir = tempfile::tempdir().expect("failed to create tmpdir");
        let rootfs_path = rootfs_dir.path();
        let path = rootfs_path.join("write_only_file");

        let mut file = OpenOptions::new()
            .read(false)
            .write(true)
            .create(true)
            .mode(0o200)
            .open(&path)
            .unwrap();
        file.write_all(&[0xa5u8]).unwrap();
        let handle = PinnedPathBuf::from_path(&path).unwrap();
        // Check that `fstat()` etc works with the fd returned by `path_fd()`.
        let fd = handle.path_fd();
        let mut stat: libc::stat = unsafe { std::mem::zeroed() };
        let res = unsafe { libc::fstat(fd, &mut stat as *mut _) };
        assert_eq!(res, 0);
    }

    #[test]
    fn test_pinned_path_buf_open_child() {
        let rootfs_dir = tempfile::tempdir().expect("failed to create tmpdir");
        let rootfs_path = rootfs_dir.path();
        let path = PinnedPathBuf::from_path(rootfs_path).unwrap();

        fs::write(path.join("child"), "test").unwrap();
        let path = path.open_child(OsStr::new("child")).unwrap();
        let content = fs::read_to_string(&path).unwrap();
        assert_eq!(&content, "test");

        path.open_child(&OsString::from("__does_not_exist__"))
            .unwrap_err();
        path.open_child(&OsString::from("test/a")).unwrap_err();
    }

    #[test]
    fn test_prepare_path_component() {
        assert!(PinnedPathBuf::prepare_path_component(&OsString::from("")).is_err());
        assert!(PinnedPathBuf::prepare_path_component(&OsString::from(".")).is_err());
        assert!(PinnedPathBuf::prepare_path_component(&OsString::from("..")).is_err());
        assert!(PinnedPathBuf::prepare_path_component(&OsString::from("/")).is_err());
        assert!(PinnedPathBuf::prepare_path_component(&OsString::from("//")).is_err());
        assert!(PinnedPathBuf::prepare_path_component(&OsString::from("a/b")).is_err());
        assert!(PinnedPathBuf::prepare_path_component(&OsString::from("./b")).is_err());
        assert!(PinnedPathBuf::prepare_path_component(&OsString::from("a/.")).is_err());
        assert!(PinnedPathBuf::prepare_path_component(&OsString::from("a/..")).is_err());
        assert!(PinnedPathBuf::prepare_path_component(&OsString::from("a/./")).is_err());
        assert!(PinnedPathBuf::prepare_path_component(&OsString::from("a/../")).is_err());
        assert!(PinnedPathBuf::prepare_path_component(&OsString::from("a/./a")).is_err());
        assert!(PinnedPathBuf::prepare_path_component(&OsString::from("a/../a")).is_err());

        assert!(PinnedPathBuf::prepare_path_component(&OsString::from("a")).is_ok());
        assert!(PinnedPathBuf::prepare_path_component(&OsString::from("a.b")).is_ok());
        assert!(PinnedPathBuf::prepare_path_component(&OsString::from("a..b")).is_ok());
    }

    #[test]
    fn test_target_fs_object_changed() {
        let rootfs_dir = tempfile::tempdir().expect("failed to create tmpdir");
        let rootfs_path = rootfs_dir.path();
        let file = rootfs_path.join("child");
        fs::write(&file, "test").unwrap();

        let path = PinnedPathBuf::from_path(&file).unwrap();
        let path3 = fs::read_link(path.as_path()).unwrap();
        assert_eq!(&path3, path.target());
        fs::rename(file, rootfs_path.join("child2")).unwrap();
        let path4 = fs::read_link(path.as_path()).unwrap();
        assert_ne!(&path4, path.target());
        fs::remove_file(rootfs_path.join("child2")).unwrap();
        let path5 = fs::read_link(path.as_path()).unwrap();
        assert_ne!(&path4, &path5);
    }
}

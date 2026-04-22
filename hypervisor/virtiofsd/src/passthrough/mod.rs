// Copyright 2019 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

pub mod credentials;
pub mod device_state;
pub mod file_handle;
pub mod inode_store;
pub mod mount_fd;
pub mod read_only;
pub mod stat;
pub mod util;
pub mod xattrmap;

use super::fs_cache_req_handler::FsCacheReqHandler;
use crate::filesystem::{
    Context, Entry, Extensions, FileSystem, FsOptions, GetxattrReply, ListxattrReply, OpenOptions,
    SecContext, SetattrValid, SetxattrFlags, ZeroCopyReader, ZeroCopyWriter,
};
use crate::passthrough::credentials::{drop_effective_cap, UnixCredentials};
use crate::passthrough::device_state::preserialization::{HandleMigrationInfo, InodeMigrationInfo};
use crate::passthrough::inode_store::{
    Inode, InodeData, InodeFile, InodeIds, InodeStore, StrongInodeReference,
};
use crate::passthrough::util::{ebadf, is_safe_inode, openat, reopen_fd_through_proc};
use crate::read_dir::ReadDir;
use crate::{fuse, oslib};
use file_handle::{FileHandle, FileOrHandle, OpenableFileHandle};
use mount_fd::{MPRError, MountFds};
use stat::{statx, StatExt};
use std::borrow::Cow;
use std::collections::{btree_map, BTreeMap};
use std::ffi::{CStr, CString};
use std::fs::File;
use std::io;
use std::io::ErrorKind;
use std::mem::MaybeUninit;
use std::os::unix::io::{AsRawFd, FromRawFd, RawFd};
use std::str::FromStr;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, RwLock};
use std::time::Duration;
use xattrmap::{AppliedRule, XattrMap};

const EMPTY_CSTR: &[u8] = b"\0";

type Handle = u64;

enum HandleDataFile {
    File(RwLock<File>),
    // `io::Error` does not implement `Clone`, so without wrapping it in `Arc`, returning the error
    // anywhere would be impossible without consuming it
    Invalid(Arc<io::Error>),
}

struct HandleData {
    inode: Inode,
    file: HandleDataFile,

    // On migration, must be set when we serialize our internal state to send it to the
    // destination.  As long as `HandleMigrationInfo::new()` is cheap, we may as well
    // keep it always set.
    migration_info: HandleMigrationInfo,
}

struct ScopedWorkingDirectory {
    back_to: RawFd,
}

impl ScopedWorkingDirectory {
    fn new(new_wd: RawFd, old_wd: RawFd) -> ScopedWorkingDirectory {
        oslib::fchdir(new_wd).expect("the working directory should be changed");
        ScopedWorkingDirectory { back_to: old_wd }
    }
}

impl Drop for ScopedWorkingDirectory {
    fn drop(&mut self) {
        oslib::fchdir(self.back_to).expect("the working directory should be changed");
    }
}

fn set_working_directory(new_wd: RawFd, old_wd: RawFd) -> ScopedWorkingDirectory {
    ScopedWorkingDirectory::new(new_wd, old_wd)
}

/// The caching policy that the file system should report to the FUSE client. By default the FUSE
/// protocol uses close-to-open consistency. This means that any cached contents of the file are
/// invalidated the next time that file is opened.
#[derive(Default, Debug, Clone)]
pub enum CachePolicy {
    /// The client should never cache file data and all I/O should be directly forwarded to the
    /// server. This policy must be selected when file contents may change without the knowledge of
    /// the FUSE client (i.e., the file system does not have exclusive access to the directory).
    Never,

    /// This is almost same as Never, but it allows page cache of directories, dentries and attr
    /// cache in guest. In other words, it acts like cache=never for normal files, and like
    /// cache=always for directories, besides, metadata like dentries and attrs are kept as well.
    /// This policy can be used if:
    /// 1. the client wants to use Never policy but it's performance in I/O is not good enough
    /// 2. the file system has exclusive access to the directory
    /// 3. cache directory content and other fs metadata can make a difference on performance.
    Metadata,

    /// The client is free to choose when and how to cache file data. This is the default policy and
    /// uses close-to-open consistency as described in the enum documentation.
    #[default]
    Auto,

    /// The client should always cache file data. This means that the FUSE client will not
    /// invalidate any cached data that was returned by the file system the last time the file was
    /// opened. This policy should only be selected when the file system has exclusive access to the
    /// directory.
    Always,

    /// No cache policy. The client should never cache file data or directory data.
    /// All I/O for both files and directories should be directly forwarded to the server.
    /// This is the most conservative policy and should be used when maximum consistency is required
    /// for both files and directories.
    None,
}

impl FromStr for CachePolicy {
    type Err = &'static str;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match &s.to_lowercase()[..] {
            "never" => Ok(CachePolicy::Never),
            "metadata" => Ok(CachePolicy::Metadata),
            "auto" => Ok(CachePolicy::Auto),
            "always" => Ok(CachePolicy::Always),
            "none" => Ok(CachePolicy::None),
            _ => Err("invalid cache policy"),
        }
    }
}

/// When to use file handles to reference inodes instead of `O_PATH` file descriptors.
#[derive(Default, Debug, Copy, Clone, PartialEq, Eq)]
pub enum InodeFileHandlesMode {
    /// Never use file handles, always use `O_PATH` file descriptors.
    #[default]
    Never,

    /// Attempt to generate file handles, but fall back to `O_PATH` file descriptors where the
    /// underlying filesystem does not support file handles.
    Prefer,

    /// Always use file handles, never fall back to `O_PATH` file descriptors.
    Mandatory,
}

/// What to do when an error occurs during migration (checked on the migration destination)
#[derive(Default, Debug, Copy, Clone, PartialEq, Eq)]
pub enum MigrationOnError {
    /// Whenever any failure occurs, return a hard error to the vhost-user front-end (e.g.  QEMU),
    /// aborting migration.
    #[default]
    Abort,

    /// Let migration finish, but the guest will be unable to access any of the files that were
    /// failed to be found/opened, receiving only errors.
    GuestError,
}

impl FromStr for MigrationOnError {
    type Err = &'static str;

    fn from_str(s: &str) -> std::result::Result<Self, Self::Err> {
        match s {
            "abort" => Ok(MigrationOnError::Abort),
            "guest-error" => Ok(MigrationOnError::GuestError),

            _ => Err("invalid migration-on-error value"),
        }
    }
}

/// How to migrate our internal state to the destination instance
#[derive(Default, Debug, Copy, Clone, PartialEq, Eq)]
pub enum MigrationMode {
    /// Iterate through the shared directory to find paths for all inodes indexed and opened by the
    /// guest, and transfer these paths to the destination.
    #[default]
    FindPaths,
}

impl FromStr for MigrationMode {
    type Err = &'static str;

    fn from_str(s: &str) -> std::result::Result<Self, Self::Err> {
        match s {
            "find-paths" => Ok(MigrationMode::FindPaths),

            _ => Err("invalid migration-mode value"),
        }
    }
}

/// Options that configure the behavior of the file system.
#[derive(Debug)]
pub struct Config {
    /// How long the FUSE client should consider directory entries to be valid. If the contents of a
    /// directory can only be modified by the FUSE client (i.e., the file system has exclusive
    /// access), then this should be a large value.
    ///
    /// The default value for this option is 5 seconds.
    pub entry_timeout: Duration,

    /// How long the FUSE client should consider file and directory attributes to be valid. If the
    /// attributes of a file or directory can only be modified by the FUSE client (i.e., the file
    /// system has exclusive access), then this should be set to a large value.
    ///
    /// The default value for this option is 5 seconds.
    pub attr_timeout: Duration,

    /// The caching policy the file system should use. See the documentation of `CachePolicy` for
    /// more details.
    pub cache_policy: CachePolicy,

    /// Whether the file system should enabled writeback caching. This can improve performance as it
    /// allows the FUSE client to cache and coalesce multiple writes before sending them to the file
    /// system. However, enabling this option can increase the risk of data corruption if the file
    /// contents can change without the knowledge of the FUSE client (i.e., the server does **NOT**
    /// have exclusive access). Additionally, the file system should have read access to all files
    /// in the directory it is serving as the FUSE client may send read requests even for files
    /// opened with `O_WRONLY`.
    ///
    /// Therefore callers should only enable this option when they can guarantee that: 1) the file
    /// system has exclusive access to the directory and 2) the file system has read permissions for
    /// all files in that directory.
    ///
    /// The default value for this option is `false`.
    pub writeback: bool,

    /// The path of the root directory.
    ///
    /// The default is `/`.
    pub root_dir: String,

    /// A prefix to strip from the mount points listed in /proc/self/mountinfo.
    ///
    /// The default is `None`.
    pub mountinfo_prefix: Option<String>,

    /// Whether the file system should support Extended Attributes (xattr). Enabling this feature may
    /// have a significant impact on performance, especially on write parallelism. This is the result
    /// of FUSE attempting to remove the special file privileges after each write request.
    ///
    /// The default value for this options is `false`.
    pub xattr: bool,

    /// An optional translation layer for host<->guest Extended Attribute (xattr) names.
    pub xattrmap: Option<XattrMap>,

    /// The xattr name that "security.capability" is remapped to, if the client remapped it at all.
    /// If the client's xattrmap did not remap "security.capability", this will be `None`.
    pub xattr_security_capability: Option<CString>,

    /// Optional `File` object for /proc/self/fd. Callers can open a `File` and pass it here, so
    /// there's no need to open it in PassthroughFs::new(). This is specially useful for
    /// sandboxing.
    ///
    /// The default is `None`.
    pub proc_sfd_rawfd: Option<File>,

    /// Optional `File` object for /proc/self/mountinfo.  Callers can open a `File` and pass it
    /// here, so there is no need to open it in PassthroughFs::new().  This is especially useful
    /// for sandboxing.
    ///
    /// The default is `None`.
    pub proc_mountinfo_rawfd: Option<File>,

    /// Whether the file system should announce submounts to the guest.  Not doing so means that
    /// the FUSE client may see st_ino collisions: This stat field is passed through, so if the
    /// shared directory encompasses multiple mounts, some inodes (in different file systems) may
    /// have the same st_ino value.  If the FUSE client does not know these inodes are in different
    /// file systems, then it will be oblivious to this collision.
    /// By announcing submount points, the FUSE client can create virtual submounts with distinct
    /// st_dev values where necessary, so that the combination of st_dev and st_ino will stay
    /// unique.
    /// On the other hand, it may be undesirable to let the client know the shared directory's
    /// submount structure.  The user needs to decide which drawback weighs heavier for them, which
    /// is why this is a configurable option.
    ///
    /// The default is `false`.
    pub announce_submounts: bool,

    /// Whether to use file handles to reference inodes.  We need to be able to open file
    /// descriptors for arbitrary inodes, and by default that is done by storing an `O_PATH` FD in
    /// `InodeData`.  Not least because there is a maximum number of FDs a process can have open
    /// users may find it preferable to store a file handle instead, which we can use to open an FD
    /// when necessary.
    /// So this switch allows to choose between the alternatives: When set to `Never`, `InodeData`
    /// will store `O_PATH` FDs.  Otherwise, we will attempt to generate and store a file handle
    /// instead.  With `Prefer`, errors that are inherent to file handles (like no support from the
    /// underlying filesystem) lead to falling back to `O_PATH` FDs, and only generic errors (like
    /// `ENOENT` or `ENOMEM`) are passed to the guest.  `Mandatory` enforces the use of file
    /// handles, returning all errors to the guest.
    ///
    /// The default is `Never`.
    pub inode_file_handles: InodeFileHandlesMode,

    /// Whether the file system should support READDIRPLUS (READDIR+LOOKUP) operations.
    ///
    /// The default is `false`.
    pub readdirplus: bool,

    /// Whether the file system should honor the O_DIRECT flag. If this option is disabled (which
    /// is the default value), that flag will be filtered out at `open_inode`.
    ///
    /// The default is `false`.
    pub allow_direct_io: bool,

    /// If `killpriv_v2` is true then it indicates that the file system is expected to clear the
    /// setuid and setgid bits.
    pub killpriv_v2: bool,

    /// Enable support for posix ACLs
    ///
    /// The default is `false`.
    pub posix_acl: bool,

    /// If `security_label` is true, then server will indicate to client
    /// to send any security context associated with file during file
    /// creation and set that security context on newly created file.
    /// This security context is expected to be security.selinux.
    ///
    /// The default is `false`.
    pub security_label: bool,

    /// If `clean_noatime` is true automatically clean up O_NOATIME flag to prevent potential
    /// permission errors.
    pub clean_noatime: bool,

    /// If `allow_mmap` is true, then server will allow shared mmap'ing of files opened/created
    /// with DIRECT_IO.
    ///
    /// The default is `false`.
    pub allow_mmap: bool,

    /// A hit to access share path by read only, so that we could do some optimization on migration.
    /// The correct way to implement a readonly passthroughfs should be like "5eab594b4e2a"("Add
    /// PassthroughFsRo as read-only variant"), let's optimize this in next rebase from upstream.
    ///
    /// The default is 'false'.
    pub read_only: bool,

    /// Whether to disable host page cache by adding O_DIRECT flag when opening files.
    /// When enabled, all file I/O will bypass the host's page cache, which can be useful
    /// for ensuring data consistency and reducing memory usage on the host.
    ///
    /// The default is `false`.
    pub force_direct: bool,

    /// Defines what happens when restoring our internal state on the destination fails.
    ///
    /// The default is `Abort`.
    pub migration_on_error: MigrationOnError,

    /// Whether to store a file handle for each inode in the migration stream, alongside the
    /// information on how to find the inode.  The destination must generate the file handle for
    /// the inode it has opened and verify they match.
    ///
    /// The default is `false`.
    pub migration_verify_handles: bool,

    /// Whether to confirm (for path-based migration) at serialization (during switch-over) whether
    /// the paths still match the inodes they are supposed to represent, and if they do not, try to
    /// correct the path via the respective symlink in /proc/self/fd.
    ///
    /// The default is `false`.
    pub migration_confirm_paths: bool,

    /// Defines how to migrate our internal state to the destination instance.
    ///
    /// The default is `FindPaths`.
    pub migration_mode: MigrationMode,
}

impl Default for Config {
    fn default() -> Self {
        Config {
            entry_timeout: Duration::from_secs(5),
            attr_timeout: Duration::from_secs(5),
            cache_policy: Default::default(),
            writeback: false,
            root_dir: String::from("/"),
            mountinfo_prefix: None,
            xattr: false,
            xattrmap: None,
            xattr_security_capability: None,
            proc_sfd_rawfd: None,
            proc_mountinfo_rawfd: None,
            announce_submounts: false,
            inode_file_handles: Default::default(),
            readdirplus: true,
            allow_direct_io: false,
            killpriv_v2: false,
            posix_acl: false,
            security_label: false,
            clean_noatime: true,
            allow_mmap: false,
            read_only: false,
            force_direct: false,
            migration_on_error: MigrationOnError::Abort,
            migration_verify_handles: false,
            migration_confirm_paths: false,
            migration_mode: MigrationMode::FindPaths,
        }
    }
}

/// A file system that simply "passes through" all requests it receives to the underlying file
/// system. To keep the implementation simple it servers the contents of its root directory. Users
/// that wish to serve only a specific directory should set up the environment so that that
/// directory ends up as the root of the file system process. One way to accomplish this is via a
/// combination of mount namespaces and the pivot_root system call.
pub struct PassthroughFs {
    // File descriptors for various points in the file system tree. These fds are always opened with
    // the `O_PATH` option so they cannot be used for reading or writing any data. See the
    // documentation of the `O_PATH` flag in `open(2)` for more details on what one can and cannot
    // do with an fd opened with this flag.
    inodes: InodeStore,
    next_inode: AtomicU64,

    // File descriptors for open files and directories. Unlike the fds in `inodes`, these _can_ be
    // used for reading and writing data.
    handles: RwLock<BTreeMap<Handle, Arc<HandleData>>>,
    next_handle: AtomicU64,

    // Maps mount IDs to an open FD on the respective ID for the purpose of open_by_handle_at().
    // This is set when inode_file_handles is not never, since in the 'never' case,
    // open_by_handle_at() is not called.
    mount_fds: Option<MountFds>,

    // File descriptor pointing to the `/proc/self/fd` directory. This is used to convert an fd from
    // `inodes` into one that can go into `handles`. This is accomplished by reading the
    // `/proc/self/fd/{}` symlink. We keep an open fd here in case the file system tree that we are
    // meant to be serving doesn't have access to `/proc/self/fd`.
    proc_self_fd: File,

    // File descriptor pointing to the `/` directory.
    root_fd: File,

    // Whether writeback caching is enabled for this directory. This will only be true when
    // `cfg.writeback` is true and `init` was called with `FsOptions::WRITEBACK_CACHE`.
    writeback: AtomicBool,

    // Whether to announce submounts (i.e., whether the guest supports them and whether they are
    // enabled in the configuration)
    announce_submounts: AtomicBool,

    // Whether posix ACLs is enabled.
    posix_acl: AtomicBool,

    // Basic facts about the OS
    os_facts: oslib::OsFacts,

    // Whether the guest kernel supports the supplementary group extension.
    sup_group_extension: AtomicBool,

    // Whether we are preparing for migration and need to track changes to inodes like renames.  We
    // should then also make sure newly created inodes immediately have their migration info set.
    track_migration_info: AtomicBool,

    // White list filter entries, used to record accessed filter entries, so that we could
    // serialization each subtree of filter entries.
    filter: RwLock<BTreeMap<Inode, (String, String)>>,

    cfg: Config,
}

use crate::fuse::ROOT_ID;

impl PassthroughFs {
    pub fn new(mut cfg: Config) -> io::Result<PassthroughFs> {
        let proc_self_fd = if let Some(fd) = cfg.proc_sfd_rawfd.take() {
            fd
        } else {
            openat(
                &libc::AT_FDCWD,
                "/proc/self/fd",
                libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC,
            )?
        };

        let root_fd = openat(
            &libc::AT_FDCWD,
            "/",
            libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC,
        )?;

        let timeout = match cfg.cache_policy {
            // We using mode 'Never' in cube product, to reduce
            // file pagecache inside guest kernel.
            // But we want to keep directory entry cache to speed
            // up getdents/getdents64 in guest kernel, to avoid
            // request to backend virtiofsd, so here we set timeout
            // value as mode 'Always'.
            CachePolicy::Never => Duration::from_secs(86400),
            CachePolicy::Auto => Duration::from_secs(1),
            CachePolicy::Always => Duration::from_secs(86400),
            CachePolicy::Metadata => Duration::from_secs(86400),
            CachePolicy::None => Duration::from_secs(0),
        };
        cfg.entry_timeout = timeout;
        cfg.attr_timeout = timeout;

        let mut fs = PassthroughFs {
            inodes: Default::default(),
            next_inode: AtomicU64::new(fuse::ROOT_ID + 1),
            handles: RwLock::new(BTreeMap::new()),
            next_handle: AtomicU64::new(0),
            mount_fds: None,
            proc_self_fd,
            root_fd,
            writeback: AtomicBool::new(false),
            announce_submounts: AtomicBool::new(false),
            posix_acl: AtomicBool::new(false),
            sup_group_extension: AtomicBool::new(false),
            os_facts: oslib::OsFacts::new(),
            track_migration_info: AtomicBool::new(false),
            filter: RwLock::new(BTreeMap::new()),
            cfg,
        };

        // Check to see if the client remapped "security.capability", if so,
        // stash its mapping since the daemon will have to enforce semantics
        // that the host kernel otherwise would if the xattrname was not mapped.
        let sec_xattr = unsafe { CStr::from_bytes_with_nul_unchecked(b"security.capability\0") };
        fs.cfg.xattr_security_capability = fs
            .map_client_xattrname(sec_xattr)
            .ok()
            .filter(|n| !sec_xattr.eq(n))
            .map(CString::from);

        fs.check_working_file_handles()?;

        // We need to clear the umask here because we want the client to be
        // able to set all the bits in the mode.
        oslib::umask(0o000);

        Ok(fs)
    }

    pub fn keep_fds(&self) -> Vec<RawFd> {
        vec![self.proc_self_fd.as_raw_fd()]
    }

    fn open_relative_to(
        &self,
        dir: &impl AsRawFd,
        pathname: &CStr,
        flags: i32,
        mode: Option<u32>,
    ) -> io::Result<RawFd> {
        let flags = libc::O_NOFOLLOW | libc::O_CLOEXEC | flags;

        if self.os_facts.has_openat2 {
            oslib::do_open_relative_to(dir, pathname, flags, mode)
        } else {
            oslib::openat(dir, pathname, flags, mode)
        }
    }

    fn find_handle(&self, handle: Handle, inode: Inode) -> io::Result<Arc<HandleData>> {
        self.handles
            .read()
            .unwrap()
            .get(&handle)
            .filter(|hd| hd.inode == inode)
            .cloned()
            .ok_or_else(ebadf)
    }

    fn open_inode(&self, inode: Inode, mut flags: i32) -> io::Result<File> {
        let data = self.inodes.get(inode).ok_or_else(ebadf)?;

        // When writeback caching is enabled, the kernel may send read requests even if the
        // userspace program opened the file write-only. So we need to ensure that we have opened
        // the file for reading as well as writing.
        let writeback = self.writeback.load(Ordering::Relaxed);
        if writeback && flags & libc::O_ACCMODE == libc::O_WRONLY {
            flags &= !libc::O_ACCMODE;
            flags |= libc::O_RDWR;
        }

        // When writeback caching is enabled the kernel is responsible for handling `O_APPEND`.
        // However, this breaks atomicity as the file may have changed on disk, invalidating the
        // cached copy of the data in the kernel and the offset that the kernel thinks is the end of
        // the file. Just allow this for now as it is the user's responsibility to enable writeback
        // caching only for directories that are not shared. It also means that we need to clear the
        // `O_APPEND` flag.
        if writeback && flags & libc::O_APPEND != 0 {
            flags &= !libc::O_APPEND;
        }

        if !self.cfg.allow_direct_io && flags & libc::O_DIRECT != 0 {
            flags &= !libc::O_DIRECT;
        }

        // Add O_DIRECT flag to disable host page cache if configured
        if self.cfg.force_direct {
            flags |= libc::O_DIRECT;
        }

        data.open_file(flags | libc::O_CLOEXEC, &self.proc_self_fd)?
            .into_file()
    }

    /// Generate a file handle for `fd` using `FileHandle::from_fd()`.  `st` is `fd`'s stat
    /// information (we may need the mount ID for errors/warnings).
    ///
    /// These are the possible return values:
    /// - `Ok(Some(_))`: Success, caller should use this file handle.
    /// - `Ok(None)`: No error, but no file handle is available.  The caller should fall back to
    ///               using an `O_PATH` FD.
    /// - `Err(_)`: An error occurred, the caller should return this to the guest.
    ///
    /// This function takes the chosen `self.cfg.inode_file_handles` mode into account:
    /// - `Never`: Always return `Ok(None)`.
    /// - `Prefer`: Return `Ok(None)` when file handles are not supported by this filesystem.
    ///             Otherwise, return either `Ok(Some(_))` or `Err(_)`, depending on whether a file
    ///             handle could be generated or not.
    /// - `Mandatory`: Never return `Ok(None)`.  When the filesystem does not support file handles,
    ///                return an `Err(_)`.
    ///
    /// When the filesystem does not support file handles, this is logged (as a warning in
    /// `Prefer` mode, and as an error in `Mandatory` mode) one time per filesystem.
    fn get_file_handle_opt(
        &self,
        fd: &impl AsRawFd,
        st: &StatExt,
    ) -> io::Result<Option<FileHandle>> {
        let handle = match self.cfg.inode_file_handles {
            InodeFileHandlesMode::Never => {
                // Let's make this quick, so we can skip this case below
                return Ok(None);
            }

            InodeFileHandlesMode::Prefer | InodeFileHandlesMode::Mandatory => {
                FileHandle::from_fd(fd)?
            }
        };

        if handle.is_none() {
            // No error, but no handle (because of EOPNOTSUPP/EOVERFLOW)?  Log it.
            let io_err = io::Error::from_raw_os_error(libc::EOPNOTSUPP);

            let desc = match self.cfg.inode_file_handles {
                InodeFileHandlesMode::Never => unreachable!(),
                InodeFileHandlesMode::Prefer => {
                    "Filesystem does not support file handles, falling back to O_PATH FDs"
                }
                InodeFileHandlesMode::Mandatory => "Filesystem does not support file handles",
            };

            // Use the MPRError object, because (with a mount ID obtained through statx())
            // `self.mount_fds.error_for()` will attempt to add a prefix to the error description
            // that describes the offending filesystem by mount point and mount ID, and will also
            // suppress the message if we have already logged any error concerning file handles for
            // the respective filesystem (so we only log errors/warnings once).
            let err: MPRError = if st.mnt_id > 0 {
                // Valid mount ID
                // self.mount_fds won't be None if we enter here.
                self.mount_fds
                    .as_ref()
                    .unwrap()
                    .error_for(st.mnt_id, io_err)
            } else {
                // No valid mount ID, return error object not bound to a filesystem
                io_err.into()
            }
            .set_desc(desc.to_string());

            // In `Prefer` mode, warn; in `Mandatory` mode, log and return an error.
            // (Suppress logging if the error is silenced, which means that we have already logged
            // a warning/error for this filesystem.)
            match self.cfg.inode_file_handles {
                InodeFileHandlesMode::Never => unreachable!(),
                InodeFileHandlesMode::Prefer => {
                    if !err.silent() {
                        warn!("{}", err);
                    }
                }
                InodeFileHandlesMode::Mandatory => {
                    if !err.silent() {
                        error!("{}", err);
                    }
                    return Err(err.into_inner());
                }
            }
        }

        Ok(handle)
    }

    fn make_file_handle_openable(&self, fh: &FileHandle) -> io::Result<OpenableFileHandle> {
        // self.mount_fds won't be None if we enter here.
        fh.to_openable(self.mount_fds.as_ref().unwrap(), |fd, flags| {
            reopen_fd_through_proc(&fd, flags, &self.proc_self_fd)
        })
        .map_err(|e| {
            if !e.silent() {
                error!("{}", e);
            }
            e.into_inner()
        })
    }

    fn check_working_file_handles(&mut self) -> io::Result<()> {
        if self.cfg.inode_file_handles == InodeFileHandlesMode::Never {
            // No need to check anything
            return Ok(());
        }

        // Try to open the root directory, turn it into a file handle, then try to open that file
        // handle to see whether file handles do indeed work
        // (Note that we pass through all I/O errors to the caller, because `PassthroughFs::init()`
        // will do these calls (`openat()`, `stat()`, etc.) anyway, so if they do not work now,
        // they probably are not going to work later either.  Better to report errors early then.)
        let root_dir = openat(
            &libc::AT_FDCWD,
            self.cfg.root_dir.as_str(),
            libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC,
        )?;

        let st = statx(&root_dir, None)?;
        if let Some(h) = self.get_file_handle_opt(&root_dir, &st)? {
            // Got an openable file handle, try opening it
            match self.make_file_handle_openable(&h)?.open(libc::O_PATH) {
                Ok(_) => (),
                Err(e) => match self.cfg.inode_file_handles {
                    InodeFileHandlesMode::Never => unreachable!(),
                    InodeFileHandlesMode::Prefer => {
                        warn!("Failed to open file handle for the root node: {}", e);
                        warn!("File handles do not appear safe to use, disabling file handles altogether");
                        self.cfg.inode_file_handles = InodeFileHandlesMode::Never;
                    }
                    InodeFileHandlesMode::Mandatory => {
                        error!("Failed to open file handle for the root node: {}", e);
                        error!("Refusing to use (mandatory) file handles, as they do not appear safe to use");
                        return Err(e);
                    }
                },
            }
        } else {
            // Did not get an openable file handle (nor an error), so we cannot be in `mandatory`
            // mode.  We also cannot be in `never` mode, because that is sorted out at the very
            // beginning of this function.  Still, use `match` so the compiler could warn us if we
            // were to forget some (future?) variant.
            match self.cfg.inode_file_handles {
                InodeFileHandlesMode::Never => unreachable!(),
                InodeFileHandlesMode::Prefer => {
                    warn!("Failed to generate a file handle for the root node, disabling file handles altogether");
                    self.cfg.inode_file_handles = InodeFileHandlesMode::Never;
                }
                InodeFileHandlesMode::Mandatory => unreachable!(),
            }
        }

        Ok(())
    }

    /// Try to look up an inode by its `name` relative to the given `parent` inode.  If the inode
    /// is registered in our inode store (`self.inodes`), return a strong reference to it.
    /// Otherwise, return `None` instead.
    /// Along with the inode (or `None`), return information gathered along the way: An `O_PATH`
    /// file to the inode, stat information, and optionally a file handle if virtiofsd has been
    /// configured to use file handles.
    /// Return an error if the parent node cannot be opened, the given inode cannot be found on the
    /// filesystem, or generating the stat information or file handle fails.
    fn try_lookup_implementation(
        &self,
        parent_data: &InodeData,
        name: &CStr,
    ) -> io::Result<(
        Option<StrongInodeReference>,
        File,
        StatExt,
        Option<FileHandle>,
    )> {
        let p_file = parent_data.get_file()?;

        let path_fd = {
            let fd = self.open_relative_to(&p_file, name, libc::O_PATH, None)?;
            // Safe because we just opened this fd.
            unsafe { File::from_raw_fd(fd) }
        };

        let st = statx(&path_fd, None)?;

        // Note that this will always be `None` if `cfg.inode_file_handles` is `Never`, but we only
        // really need the handle when we do not have an `O_PATH` fd open for every inode.  So if
        // `cfg.inode_file_handles` is `Never`, we do not need it anyway.
        let handle = self.get_file_handle_opt(&path_fd, &st)?;

        let ids = InodeIds {
            ino: st.st.st_ino,
            dev: st.st.st_dev,
            mnt_id: st.mnt_id,
        };

        Ok((
            self.inodes.claim_inode(handle.as_ref(), &ids).ok(),
            path_fd,
            st,
            handle,
        ))
    }

    /// Try to look up an inode by its `name` relative to the parent inode given by its
    /// `parent_data`.  If the inode is registered in our inode store (`self.inodes`), return a
    /// strong reference to it.  Otherwise, return `None`.
    /// Return an error if the parent node cannot be opened, the given inode cannot be found on the
    /// filesystem, or generating the Stat information or file handle fails.
    fn try_lookup(
        &self,
        parent_data: &InodeData,
        name: &CStr,
    ) -> io::Result<Option<StrongInodeReference>> {
        self.try_lookup_implementation(parent_data, name)
            .map(|result| result.0)
    }

    fn do_lookup(
        &self,
        parent: Inode,
        name: &CStr,
        f_info: Option<(String, StatExt)>,
    ) -> io::Result<Entry> {
        let st;
        let path_fd;
        let p = self.inodes.get(parent).ok_or_else(ebadf)?;
        let mut fullname = String::new();
        let mut is_filter = false;

        debug!(
            "lookup parent {:?} f_info {:?} name {:?}",
            parent,
            f_info.is_some(),
            name
        );
        let (existing_inode, handle) = if parent == ROOT_ID && f_info.is_some() {
            if let Some((_dir, _st)) = f_info {
                st = _st;
                fullname = _dir.clone();
                path_fd = openat(
                    &libc::AT_FDCWD,
                    &_dir,
                    libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC,
                )?;
            } else {
                warn!("Lookup {:?} failed, f_info is None", name);
                return Err(io::Error::from_raw_os_error(libc::EINVAL));
            };

            let ids = InodeIds {
                ino: st.st.st_ino,
                dev: st.st.st_dev,
                mnt_id: st.mnt_id,
            };

            let inode = self.inodes.claim_inode(None, &ids).ok();
            is_filter = true;

            (inode, None)
        } else {
            let (inode, fd, _st, handle) = self.try_lookup_implementation(&p, name)?;

            st = _st;
            path_fd = fd;

            (inode, handle)
        };

        let mut attr_flags: u32 = 0;

        if st.st.st_mode & libc::S_IFMT == libc::S_IFDIR
            && self.announce_submounts.load(Ordering::Relaxed)
            && (st.st.st_dev != p.ids.dev || st.mnt_id != p.ids.mnt_id)
        {
            attr_flags |= fuse::ATTR_SUBMOUNT;
        }

        let inode = if let Some(inode) = existing_inode {
            inode
        } else {
            let file_or_handle = if let Some(h) = handle.as_ref() {
                FileOrHandle::Handle(self.make_file_handle_openable(h)?)
            } else {
                FileOrHandle::File(path_fd)
            };

            let mig_info = if self.track_migration_info.load(Ordering::Relaxed) {
                let parent_strong_ref = StrongInodeReference::new_with_data(p, &self.inodes)?;
                Some(InodeMigrationInfo::new(
                    &self.cfg,
                    parent_strong_ref,
                    name,
                    fullname.clone(),
                    &file_or_handle,
                )?)
            } else {
                None
            };

            let inode_data = InodeData {
                inode: self.next_inode.fetch_add(1, Ordering::Relaxed),
                file_or_handle,
                refcount: AtomicU64::new(1),
                ids: InodeIds {
                    ino: st.st.st_ino,
                    dev: st.st.st_dev,
                    mnt_id: st.mnt_id,
                },
                mode: st.st.st_mode,
                migration_info: Mutex::new(mig_info),
            };

            debug!(
                "lookup inode {:?} name {:?} fullname {:?} is_filter {:?}",
                inode_data.inode,
                name,
                fullname,
                is_filter,
            );

            if is_filter {
                self.filter.write().unwrap().insert(
                    inode_data.inode,
                    (name.to_str().unwrap().to_string(), fullname),
                );
            }

            self.inodes.get_or_insert(inode_data)?
        };

        Ok(Entry {
            // By leaking, we transfer ownership of this refcount to the guest.  That is safe,
            // because the guest is expected to explicitly release its reference and decrement the
            // refcount via `FORGET` later.
            inode: unsafe { inode.leak() },
            generation: 0,
            attr: st.st,
            attr_flags,
            attr_timeout: self.cfg.attr_timeout,
            entry_timeout: self.cfg.entry_timeout,
        })
    }

    fn do_open(
        &self,
        inode: Inode,
        kill_priv: bool,
        flags: u32,
    ) -> io::Result<(Option<Handle>, OpenOptions)> {
        // We need to clean the `O_APPEND` flag in case the file is mem mapped or if the flag
        // is later modified in the guest using `fcntl(F_SETFL)`. We do a per-write `O_APPEND`
        // check setting `RWF_APPEND` for non-mmapped writes, if necessary.
        let mut flags = flags & !(libc::O_APPEND as u32);

        // Clean O_NOATIME (unless specified otherwise with --preserve-noatime) to prevent
        // potential permission errors when running in unprivileged mode.
        if self.cfg.clean_noatime {
            flags &= !(libc::O_NOATIME as u32)
        }

        let file = {
            let _killpriv_guard = if self.cfg.killpriv_v2 && kill_priv {
                drop_effective_cap("FSETID")?
            } else {
                None
            };
            self.open_inode(inode, flags as i32)?
        };

        if flags & (libc::O_TRUNC as u32) != 0 {
            self.clear_file_capabilities(file.as_raw_fd(), false)?;
        }

        let handle = self.next_handle.fetch_add(1, Ordering::Relaxed);
        let data = HandleData {
            inode,
            file: file.into(),
            migration_info: HandleMigrationInfo::new(flags as i32),
        };

        self.handles.write().unwrap().insert(handle, Arc::new(data));

        let mut opts = OpenOptions::empty();
        match self.cfg.cache_policy {
            // We only set the direct I/O option on files.
            CachePolicy::Never => {
                opts.set(
                    OpenOptions::DIRECT_IO,
                    flags & (libc::O_DIRECTORY as u32) == 0,
                );
                // Force enable directory cache, we could speed up
                // getdents/getdents64 in guest without request to
                // backend everytime. Because we are using virtiofs
                // for readonly access in guest.
                if flags & (libc::O_DIRECTORY as u32) != 0 {
                    opts |= OpenOptions::KEEP_CACHE;
                    // Disable cache for root directory. We are going
                    // to support inject docker/POD into VM, which will
                    // add new root entry in backend root directory.
                    if inode != 1 {
                        opts |= OpenOptions::CACHE_DIR;
                    }
                }
            }
            CachePolicy::Metadata => {
                if flags & (libc::O_DIRECTORY as u32) == 0 {
                    opts |= OpenOptions::DIRECT_IO;
                } else {
                    opts |= OpenOptions::CACHE_DIR | OpenOptions::KEEP_CACHE;
                }
            }
            CachePolicy::Always => {
                opts |= OpenOptions::KEEP_CACHE;
                if flags & (libc::O_DIRECTORY as u32) != 0 {
                    opts |= OpenOptions::CACHE_DIR;
                }
            }
            CachePolicy::None => {
                // We only set the direct I/O option on files.
                opts.set(
                    OpenOptions::DIRECT_IO,
                    flags & (libc::O_DIRECTORY as u32) == 0,
                );
            }
            _ => {}
        };

        Ok((Some(handle), opts))
    }

    fn do_release(&self, inode: Inode, handle: Handle) -> io::Result<()> {
        let mut handles = self.handles.write().unwrap();

        if let btree_map::Entry::Occupied(e) = handles.entry(handle) {
            if e.get().inode == inode {
                // We don't need to close the file here because that will happen automatically when
                // the last `Arc` is dropped.
                e.remove();
                return Ok(());
            }
        }

        Err(ebadf())
    }

    fn do_getattr(&self, inode: Inode) -> io::Result<(libc::stat64, Duration)> {
        let data = self.inodes.get(inode).ok_or_else(ebadf)?;
        let inode_file = data.get_file()?;
        let st = statx(&inode_file, None)?.st;

        Ok((st, self.cfg.attr_timeout))
    }

    fn do_unlink(&self, parent: Inode, name: &CStr, flags: libc::c_int) -> io::Result<()> {
        let data = self.inodes.get(parent).ok_or_else(ebadf)?;
        let parent_file = data.get_file()?;

        // Safe because this doesn't modify any memory and we check the return value.
        let res = unsafe { libc::unlinkat(parent_file.as_raw_fd(), name.as_ptr(), flags) };
        if res == 0 {
            Ok(())
        } else {
            Err(io::Error::last_os_error())
        }
    }

    fn block_xattr(&self, name: &[u8]) -> bool {
        // Currently we only filter out posix acl xattrs.
        // If acls are enabled, there is nothing to  filter.
        if self.posix_acl.load(Ordering::Relaxed) {
            return false;
        }

        let acl_access = "system.posix_acl_access".as_bytes();
        let acl_default = "system.posix_acl_default".as_bytes();
        acl_access.starts_with(name) || acl_default.starts_with(name)
    }

    fn map_client_xattrname<'a>(&self, name: &'a CStr) -> std::io::Result<Cow<'a, CStr>> {
        if self.block_xattr(name.to_bytes()) {
            return Err(io::Error::from_raw_os_error(libc::ENOTSUP));
        }

        match &self.cfg.xattrmap {
            Some(map) => match map.map_client_xattr(name).expect("unterminated mapping") {
                AppliedRule::Deny => Err(io::Error::from_raw_os_error(libc::EPERM)),
                AppliedRule::Unsupported => Err(io::Error::from_raw_os_error(libc::ENOTSUP)),
                AppliedRule::Pass(new_name) => Ok(new_name),
            },
            None => Ok(Cow::Borrowed(name)),
        }
    }

    fn map_server_xattrlist(&self, xattr_names: Vec<u8>) -> Vec<u8> {
        let all_xattrs = match &self.cfg.xattrmap {
            Some(map) => map
                .map_server_xattrlist(xattr_names)
                .expect("unterminated mapping"),
            None => xattr_names,
        };

        // filter out the blocked xattrs
        let mut filtered = Vec::with_capacity(all_xattrs.len());
        let all_xattrs = all_xattrs.split(|b| *b == 0).filter(|bs| !bs.is_empty());

        for xattr in all_xattrs {
            if !self.block_xattr(xattr) {
                filtered.extend_from_slice(xattr);
                filtered.push(0);
            }
        }

        filtered.shrink_to_fit();

        filtered
    }

    /// Clears file capabilities
    ///
    /// * `fd` - A file descriptor
    /// * `o_path` - Must be `true` if the file referred to by `fd` was opened with the `O_PATH` flag
    ///
    /// If it is not clear whether `fd` was opened with `O_PATH` it is safe to set `o_path`
    /// to `true`.
    fn clear_file_capabilities(&self, fd: RawFd, o_path: bool) -> io::Result<()> {
        match self.cfg.xattr_security_capability.as_ref() {
            // Unmapped, let the kernel take care of this.
            None => Ok(()),
            // Otherwise we have to uphold the same semantics the kernel
            // would; which is to drop the "security.capability" xattr
            // on write
            Some(xattrname) => {
                let res = if o_path {
                    let proc_file_name = CString::new(format!("{fd}"))
                        .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;
                    let _working_dir_guard = set_working_directory(
                        self.proc_self_fd.as_raw_fd(),
                        self.root_fd.as_raw_fd(),
                    );
                    unsafe { libc::removexattr(proc_file_name.as_ptr(), xattrname.as_ptr()) }
                } else {
                    unsafe { libc::fremovexattr(fd, xattrname.as_ptr()) }
                };

                if res == 0 {
                    Ok(())
                } else {
                    let eno = io::Error::last_os_error();
                    match eno.raw_os_error().unwrap() {
                        libc::ENODATA | libc::ENOTSUP => Ok(()),
                        _ => Err(eno),
                    }
                }
            }
        }
    }

    #[allow(clippy::too_many_arguments)]
    fn do_create(
        &self,
        ctx: &Context,
        parent_file: &InodeFile,
        name: &CStr,
        mode: u32,
        flags: u32,
        umask: u32,
        extensions: Extensions,
    ) -> io::Result<RawFd> {
        let fd = {
            let _credentials_guard = UnixCredentials::new(ctx.uid, ctx.gid)
                .supplementary_gid(
                    self.sup_group_extension.load(Ordering::Relaxed),
                    extensions.sup_gid,
                )
                .set()?;
            let _umask_guard = self
                .posix_acl
                .load(Ordering::Relaxed)
                .then(|| oslib::ScopedUmask::new(umask));

            // Add libc:O_EXCL to ensure we're not accidentally opening a file the guest wouldn't
            // be allowed to access otherwise.
            self.open_relative_to(
                parent_file,
                name,
                flags as i32 | libc::O_CREAT | libc::O_EXCL,
                mode.into(),
            )?
        };

        // Set security context
        if let Some(secctx) = extensions.secctx {
            // Remap security xattr name.
            let xattr_name = match self.map_client_xattrname(&secctx.name) {
                Ok(xattr_name) => xattr_name,
                Err(e) => {
                    unsafe {
                        libc::unlinkat(parent_file.as_raw_fd(), name.as_ptr(), 0);
                    }
                    return Err(e);
                }
            };

            let ret = unsafe {
                libc::fsetxattr(
                    fd,
                    xattr_name.as_ptr(),
                    secctx.secctx.as_ptr() as *const libc::c_void,
                    secctx.secctx.len(),
                    0,
                )
            };

            if ret != 0 {
                unsafe {
                    libc::unlinkat(parent_file.as_raw_fd(), name.as_ptr(), 0);
                }
                return Err(io::Error::last_os_error());
            }
        }
        Ok(fd)
    }

    fn do_mknod_mkdir_symlink_secctx(
        &self,
        parent_file: &InodeFile,
        name: &CStr,
        secctx: &SecContext,
    ) -> io::Result<()> {
        // Remap security xattr name.
        let xattr_name = self.map_client_xattrname(&secctx.name)?;

        // Set security context on newly created node. It could be
        // device node as well, so it is not safe to open the node
        // and call fsetxattr(). Instead, use the fchdir(proc_fd)
        // and call setxattr(o_path_fd). We use this trick while
        // setting xattr as well.

        // Open O_PATH fd for dir/symlink/special node just created.
        let path_fd = self.open_relative_to(parent_file, name, libc::O_PATH, None)?;

        let procname = CString::new(format!("{path_fd}"))
            .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e));

        let procname = match procname {
            Ok(name) => name,
            Err(error) => {
                return Err(error);
            }
        };

        let _working_dir_guard =
            set_working_directory(self.proc_self_fd.as_raw_fd(), self.root_fd.as_raw_fd());

        let res = unsafe {
            libc::setxattr(
                procname.as_ptr(),
                xattr_name.as_ptr(),
                secctx.secctx.as_ptr() as *const libc::c_void,
                secctx.secctx.len(),
                0,
            )
        };

        let res_err = io::Error::last_os_error();

        if res == 0 {
            Ok(())
        } else {
            Err(res_err)
        }
    }

    pub fn open_root_node(&self) -> io::Result<()> {
        // We use `O_PATH` because we just want this for traversing the directory tree
        // and not for actually reading the contents. We don't use `open_relative_to()`
        // here because we are not opening a guest-provided pathname. Also, `self.cfg.root_dir`
        // is an absolute pathname, thus not relative to CWD, so we will not be able to open it
        // if "/" didn't change (e.g., chroot or pivot_root)
        let path_fd = openat(
            &libc::AT_FDCWD,
            self.cfg.root_dir.as_str(),
            libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC,
        )?;

        let st = statx(&path_fd, None)?;
        let handle = self.get_file_handle_opt(&path_fd, &st)?;

        let file_or_handle = if let Some(h) = handle.as_ref() {
            FileOrHandle::Handle(self.make_file_handle_openable(h)?)
        } else {
            FileOrHandle::File(path_fd)
        };

        // Not sure why the root inode gets a refcount of 2 but that's what libfuse does.
        let inode = InodeData {
            inode: fuse::ROOT_ID,
            file_or_handle,
            refcount: AtomicU64::new(2),
            ids: InodeIds {
                ino: st.st.st_ino,
                dev: st.st.st_dev,
                mnt_id: st.mnt_id,
            },
            mode: st.st.st_mode,
            migration_info: Mutex::new(None),
        };
        self.inodes.new_inode(inode)?;
        Ok(())
    }

    /// After renaming an inode while preparing for migration, update its migration info if
    /// necessary.  For example, when representing inodes through their filename and parent
    /// directory node, these must be updated to match the new name and location.
    /// `parent` and `filename` are the inode's new location.
    fn update_inode_migration_info(
        &self,
        parent_data: Arc<InodeData>,
        filename: &CStr,
    ) -> io::Result<()> {
        // We only need to update the node's migration info if we have it in our store
        if let Some(inode) = self.try_lookup(&parent_data, filename)? {
            let inode_data = inode.get();
            let parent_strong_ref = StrongInodeReference::new_with_data(parent_data, &self.inodes)?;
            let mut info_locked = inode_data.migration_info.lock().unwrap();
            // Unconditionally clear any potentially existing path, because it will be outdated
            info_locked.take();
            *info_locked = Some(InodeMigrationInfo::new(
                &self.cfg,
                parent_strong_ref,
                filename,
                String::new(),
                &inode_data.file_or_handle,
            )?);
        }

        Ok(())
    }
}

impl FileSystem for PassthroughFs {
    type Inode = Inode;
    type Handle = Handle;
    type DirIter = ReadDir<Vec<u8>>;

    fn get_root_ino(&self) -> io::Result<u64> {
        let path_fd = openat(
            &libc::AT_FDCWD,
            self.cfg.root_dir.as_str(),
            libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC,
        )?;

        let st = statx(&path_fd, None)?;

        Ok(st.st.st_ino)
    }

    fn init(&self, capable: FsOptions) -> io::Result<FsOptions> {
        self.open_root_node()?;

        // Note: On migration, all options negotiated here with the guest must be sent to the
        // destination in the `device_state::serialized::NegotiatedOpts` structure.  So when adding
        // a new option here, don't forget to add it there, too, and handle it both in
        // `<serialized::NegotiatedOpts as From<&PassthroughFs>>::from()` and
        // `serialized::NegotiatedOpts::apply()`.

        let mut opts = if self.cfg.readdirplus {
            FsOptions::DO_READDIRPLUS | FsOptions::READDIRPLUS_AUTO
        } else {
            FsOptions::empty()
        };
        if self.cfg.writeback && capable.contains(FsOptions::WRITEBACK_CACHE) {
            opts |= FsOptions::WRITEBACK_CACHE;
            self.writeback.store(true, Ordering::Relaxed);
        }
        if self.cfg.announce_submounts {
            if capable.contains(FsOptions::SUBMOUNTS) {
                self.announce_submounts.store(true, Ordering::Relaxed);
            } else {
                eprintln!("Warning: Cannot announce submounts, client does not support it");
            }
        }
        if self.cfg.killpriv_v2 {
            if capable.contains(FsOptions::HANDLE_KILLPRIV_V2) {
                opts |= FsOptions::HANDLE_KILLPRIV_V2;
            } else {
                warn!("Cannot enable KILLPRIV_V2, client does not support it");
            }
        }
        if self.cfg.posix_acl {
            let acl_required_flags =
                FsOptions::POSIX_ACL | FsOptions::DONT_MASK | FsOptions::SETXATTR_EXT;
            if capable.contains(acl_required_flags) {
                opts |= acl_required_flags;
                self.posix_acl.store(true, Ordering::Relaxed);
                debug!("init: enabling posix acl");
            } else {
                error!("Cannot enable posix ACLs, client does not support it");
                return Err(io::Error::from_raw_os_error(libc::EPROTO));
            }
        }

        if self.cfg.security_label {
            if capable.contains(FsOptions::SECURITY_CTX) {
                opts |= FsOptions::SECURITY_CTX;
            } else {
                error!("Cannot enable security label. kernel does not support FUSE_SECURITY_CTX capability");
                return Err(io::Error::from_raw_os_error(libc::EPROTO));
            }
        }

        if self.cfg.allow_mmap {
            opts |= FsOptions::DIRECT_IO_ALLOW_MMAP;
        }

        if capable.contains(FsOptions::CREATE_SUPP_GROUP) {
            self.sup_group_extension.store(true, Ordering::Relaxed);
        }

        // Force enable DIRECT_IO_ALLOW_MMAP when cache is disabled.
        match self.cfg.cache_policy {
            CachePolicy::Never => opts |= FsOptions::DIRECT_IO_ALLOW_MMAP,
            CachePolicy::None => opts |= FsOptions::DIRECT_IO_ALLOW_MMAP,
            _ => {}
        };

        Ok(opts)
    }

    fn destroy(&self) {
        self.handles.write().unwrap().clear();
        self.inodes.clear();
        self.writeback.store(false, Ordering::Relaxed);
        self.announce_submounts.store(false, Ordering::Relaxed);
        self.posix_acl.store(false, Ordering::Relaxed);
        self.sup_group_extension.store(false, Ordering::Relaxed);
    }

    fn statfs(&self, _ctx: Context, inode: Inode) -> io::Result<libc::statvfs64> {
        let data = self.inodes.get(inode).ok_or_else(ebadf)?;
        let inode_file = data.get_file()?;
        let mut out = MaybeUninit::<libc::statvfs64>::zeroed();

        // Safe because this will only modify `out` and we check the return value.
        let res = unsafe { libc::fstatvfs64(inode_file.as_raw_fd(), out.as_mut_ptr()) };
        if res == 0 {
            // Safe because the kernel guarantees that `out` has been initialized.
            Ok(unsafe { out.assume_init() })
        } else {
            Err(io::Error::last_os_error())
        }
    }

    fn lookup(
        &self,
        _ctx: Context,
        parent: Inode,
        name: &CStr,
        f_info: Option<(String, StatExt)>,
    ) -> io::Result<Entry> {
        self.do_lookup(parent, name, f_info)
    }

    fn forget(&self, _ctx: Context, inode: Inode, count: u64) {
        self.inodes.forget_one(inode, count)
    }

    fn batch_forget(&self, _ctx: Context, requests: Vec<(Inode, u64)>) {
        self.inodes.forget_many(requests)
    }

    fn opendir(
        &self,
        _ctx: Context,
        inode: Inode,
        flags: u32,
    ) -> io::Result<(Option<Handle>, OpenOptions)> {
        self.do_open(inode, false, flags | (libc::O_DIRECTORY as u32))
    }

    fn releasedir(
        &self,
        _ctx: Context,
        inode: Inode,
        _flags: u32,
        handle: Handle,
    ) -> io::Result<()> {
        self.do_release(inode, handle)
    }

    fn mkdir(
        &self,
        ctx: Context,
        parent: Inode,
        name: &CStr,
        mode: u32,
        umask: u32,
        extensions: Extensions,
    ) -> io::Result<Entry> {
        let data = self.inodes.get(parent).ok_or_else(ebadf)?;
        let parent_file = data.get_file()?;

        let res = {
            let _credentials_guard = UnixCredentials::new(ctx.uid, ctx.gid)
                .supplementary_gid(
                    self.sup_group_extension.load(Ordering::Relaxed),
                    extensions.sup_gid,
                )
                .set()?;
            let _umask_guard = self
                .posix_acl
                .load(Ordering::Relaxed)
                .then(|| oslib::ScopedUmask::new(umask));

            // Safe because this doesn't modify any memory and we check the return value.
            unsafe { libc::mkdirat(parent_file.as_raw_fd(), name.as_ptr(), mode) }
        };
        if res < 0 {
            return Err(io::Error::last_os_error());
        }

        // Set security context on dir.
        if let Some(secctx) = extensions.secctx {
            if let Err(e) = self.do_mknod_mkdir_symlink_secctx(&parent_file, name, &secctx) {
                unsafe {
                    libc::unlinkat(parent_file.as_raw_fd(), name.as_ptr(), libc::AT_REMOVEDIR);
                };
                return Err(e);
            }
        }

        self.do_lookup(parent, name, None)
    }

    fn rmdir(&self, _ctx: Context, parent: Inode, name: &CStr) -> io::Result<()> {
        self.do_unlink(parent, name, libc::AT_REMOVEDIR)
    }

    fn readdir(
        &self,
        _ctx: Context,
        inode: Inode,
        handle: Handle,
        size: u32,
        offset: u64,
    ) -> io::Result<Self::DirIter> {
        if size == 0 {
            return Ok(ReadDir::default());
        }
        let data = self.find_handle(handle, inode)?;

        let buf = vec![0; size as usize];

        // Since we are going to work with the kernel offset, we have to acquire the file
        // lock for both the `lseek64` and `getdents64` syscalls to ensure that no other
        // thread changes the kernel offset while we are using it.
        #[allow(clippy::readonly_write_lock)]
        let dir = data.file.get()?.write().unwrap();

        ReadDir::new(&*dir, offset as libc::off64_t, buf)
    }

    fn open(
        &self,
        _ctx: Context,
        inode: Inode,
        kill_priv: bool,
        flags: u32,
    ) -> io::Result<(Option<Handle>, OpenOptions)> {
        self.do_open(inode, kill_priv, flags)
    }

    fn release(
        &self,
        _ctx: Context,
        inode: Inode,
        _flags: u32,
        handle: Handle,
        _flush: bool,
        _flock_release: bool,
        _lock_owner: Option<u64>,
    ) -> io::Result<()> {
        self.do_release(inode, handle)
    }

    fn create(
        &self,
        ctx: Context,
        parent: Inode,
        name: &CStr,
        mode: u32,
        kill_priv: bool,
        flags: u32,
        umask: u32,
        extensions: Extensions,
    ) -> io::Result<(Entry, Option<Handle>, OpenOptions)> {
        let data = self.inodes.get(parent).ok_or_else(ebadf)?;
        let parent_file = data.get_file()?;

        // We need to clean the `O_APPEND` flag in case the file is mem mapped or if the flag
        // is later modified in the guest using `fcntl(F_SETFL)`. We do a per-write `O_APPEND`
        // check setting `RWF_APPEND` for non-mmapped writes, if necessary.
        let create_flags = flags & !(libc::O_APPEND as u32);
        let fd = self.do_create(
            &ctx,
            &parent_file,
            name,
            mode,
            create_flags,
            umask,
            extensions,
        );

        let (entry, handle) = match fd {
            Err(last_error) => {
                // Ignore the error if the file exists and O_EXCL is not present in `flags`
                match last_error.kind() {
                    io::ErrorKind::AlreadyExists => {
                        if (flags as i32 & libc::O_EXCL) != 0 {
                            return Err(last_error);
                        }
                    }
                    _ => return Err(last_error),
                }

                let entry = self.do_lookup(parent, name, None)?;
                let (handle, _) = self.do_open(entry.inode, kill_priv, flags)?;
                let handle = handle.ok_or_else(ebadf)?;

                (entry, handle)
            }
            Ok(fd) => {
                // Safe because we just opened this fd.
                let file = unsafe { File::from_raw_fd(fd) };

                let entry = self.do_lookup(parent, name, None)?;

                let handle = self.next_handle.fetch_add(1, Ordering::Relaxed);
                let data = HandleData {
                    inode: entry.inode,
                    file: file.into(),
                    migration_info: HandleMigrationInfo::new(flags as i32),
                };

                self.handles.write().unwrap().insert(handle, Arc::new(data));

                (entry, handle)
            }
        };

        let mut opts = OpenOptions::empty();
        match self.cfg.cache_policy {
            CachePolicy::Never => opts |= OpenOptions::DIRECT_IO,
            CachePolicy::Metadata => opts |= OpenOptions::DIRECT_IO,
            CachePolicy::Always => opts |= OpenOptions::KEEP_CACHE,
            CachePolicy::None => opts |= OpenOptions::DIRECT_IO,
            _ => {}
        };

        Ok((entry, Some(handle), opts))
    }

    fn unlink(&self, _ctx: Context, parent: Inode, name: &CStr) -> io::Result<()> {
        self.do_unlink(parent, name, 0)
    }

    fn setupmapping<T: FsCacheReqHandler>(
        &self,
        _ctx: Context,
        inode: Inode,
        _handle: Handle,
        foffset: u64,
        len: u64,
        flags: u64,
        moffset: u64,
        vu_req: &mut T,
    ) -> io::Result<()> {
        debug!(
            "setupmapping: ino {:?} foffset {} len {} flags {} moffset {}",
            inode, foffset, len, flags, moffset
        );

        let open_flags = if (flags & fuse::SetupmappingFlags::WRITE.bits()) != 0 {
            libc::O_RDWR
        } else {
            libc::O_RDONLY
        };

        let file = self.open_inode(inode, open_flags)?;
        (*vu_req).map(foffset, moffset, len, flags, file.as_raw_fd())
    }

    fn removemapping<T: FsCacheReqHandler>(
        &self,
        _ctx: Context,
        requests: Vec<fuse::RemovemappingOne>,
        vu_req: &mut T,
    ) -> io::Result<()> {
        (*vu_req).unmap(requests)
    }

    fn read<W: ZeroCopyWriter>(
        &self,
        _ctx: Context,
        inode: Inode,
        handle: Handle,
        mut w: W,
        size: u32,
        offset: u64,
        _lock_owner: Option<u64>,
        _flags: u32,
    ) -> io::Result<usize> {
        let data = self.find_handle(handle, inode)?;

        // This is safe because write_from uses preadv64, so the underlying file descriptor
        // offset is not affected by this operation.
        let f = data.file.get()?.read().unwrap();
        w.write_from(&f, size as usize, offset)
    }

    fn write<R: ZeroCopyReader>(
        &self,
        _ctx: Context,
        inode: Inode,
        handle: Handle,
        mut r: R,
        size: u32,
        offset: u64,
        _lock_owner: Option<u64>,
        delayed_write: bool,
        kill_priv: bool,
        flags: u32,
    ) -> io::Result<usize> {
        let data = self.find_handle(handle, inode)?;

        // This is safe because read_to uses `pwritev2(2)`, so the underlying file descriptor
        // offset is not affected by this operation.
        let f = data.file.get()?.read().unwrap();

        {
            let _killpriv_guard = if self.cfg.killpriv_v2 && kill_priv {
                // We need to drop FSETID during a write so that the kernel will remove setuid
                // or setgid bits from the file if it was written to by someone other than the
                // owner.
                drop_effective_cap("FSETID")?
            } else {
                None
            };

            self.clear_file_capabilities(f.as_raw_fd(), false)?;

            // We don't set the `RWF_APPEND` (i.e., equivalent to `O_APPEND`) flag, if it's a
            // delayed write (i.e., using writeback mode or a mem mapped file) even if the file
            // was open in append mode, since the guest kernel sends the correct offset.
            // For non-delayed writes, we set the append mode, if necessary, to correctly handle
            // writes on a file shared among VMs. This case can only be handled correctly if the
            // write on the underlying file is performed in append mode.
            let is_append = flags & libc::O_APPEND as u32 != 0;
            let flags = (!delayed_write && is_append).then_some(oslib::WritevFlags::RWF_APPEND);
            r.read_to(&f, size as usize, offset, flags)
        }
    }

    fn getattr(
        &self,
        _ctx: Context,
        inode: Inode,
        _handle: Option<Handle>,
    ) -> io::Result<(libc::stat64, Duration)> {
        self.do_getattr(inode)
    }

    fn setattr(
        &self,
        _ctx: Context,
        inode: Inode,
        attr: libc::stat64,
        handle: Option<Handle>,
        valid: SetattrValid,
    ) -> io::Result<(libc::stat64, Duration)> {
        let inode_data = self.inodes.get(inode).ok_or_else(ebadf)?;

        // In this case, we need to open a new O_RDWR FD
        let rdwr_inode_file = handle.is_none() && valid.intersects(SetattrValid::SIZE);
        let inode_file = if rdwr_inode_file {
            inode_data.open_file(libc::O_NONBLOCK | libc::O_RDWR, &self.proc_self_fd)?
        } else {
            inode_data.get_file()?
        };

        // `HandleData` is never read, but we need to keep a reference so its file is not dropped
        #[allow(dead_code)]
        enum Data {
            Handle(Arc<HandleData>, RawFd),
            ProcPath(CString),
        }

        // If we have a handle then use it otherwise get a new fd from the inode.
        let data = if let Some(handle) = handle {
            let hd = self.find_handle(handle, inode)?;

            let fd = hd.file.get()?.write().unwrap().as_raw_fd();
            Data::Handle(hd, fd)
        } else {
            let pathname = CString::new(format!("{}", inode_file.as_raw_fd()))
                .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;
            Data::ProcPath(pathname)
        };

        if valid.contains(SetattrValid::MODE) {
            // Safe because this doesn't modify any memory and we check the return value.
            let res = unsafe {
                match data {
                    Data::Handle(_, fd) => libc::fchmod(fd, attr.st_mode),
                    Data::ProcPath(ref p) => {
                        libc::fchmodat(self.proc_self_fd.as_raw_fd(), p.as_ptr(), attr.st_mode, 0)
                    }
                }
            };
            if res < 0 {
                return Err(io::Error::last_os_error());
            }
        }

        if valid.intersects(SetattrValid::UID | SetattrValid::GID) {
            let uid = if valid.contains(SetattrValid::UID) {
                attr.st_uid
            } else {
                // Cannot use -1 here because these are unsigned values.
                u32::MAX
            };
            let gid = if valid.contains(SetattrValid::GID) {
                attr.st_gid
            } else {
                // Cannot use -1 here because these are unsigned values.
                u32::MAX
            };

            self.clear_file_capabilities(inode_file.as_raw_fd(), true)?;

            // Safe because this is a constant value and a valid C string.
            let empty = unsafe { CStr::from_bytes_with_nul_unchecked(EMPTY_CSTR) };

            // Safe because this doesn't modify any memory and we check the return value.
            let res = unsafe {
                libc::fchownat(
                    inode_file.as_raw_fd(),
                    empty.as_ptr(),
                    uid,
                    gid,
                    libc::AT_EMPTY_PATH | libc::AT_SYMLINK_NOFOLLOW,
                )
            };
            if res < 0 {
                return Err(io::Error::last_os_error());
            }
        }

        if valid.contains(SetattrValid::SIZE) {
            let fd = match data {
                Data::Handle(_, fd) => fd,
                _ => {
                    // Should have opened an O_RDWR inode_file above
                    assert!(rdwr_inode_file);
                    inode_file.as_raw_fd()
                }
            };

            let _killpriv_guard =
                if self.cfg.killpriv_v2 && valid.contains(SetattrValid::KILL_SUIDGID) {
                    drop_effective_cap("FSETID")?
                } else {
                    None
                };

            // Safe because this doesn't modify any memory and we check the return value.
            let res = self
                .clear_file_capabilities(fd, false)
                .map(|_| unsafe { libc::ftruncate(fd, attr.st_size) })?;
            if res < 0 {
                return Err(io::Error::last_os_error());
            }
        }

        if valid.intersects(SetattrValid::ATIME | SetattrValid::MTIME) {
            let mut tvs = [
                libc::timespec {
                    tv_sec: 0,
                    tv_nsec: libc::UTIME_OMIT,
                },
                libc::timespec {
                    tv_sec: 0,
                    tv_nsec: libc::UTIME_OMIT,
                },
            ];

            if valid.contains(SetattrValid::ATIME_NOW) {
                tvs[0].tv_nsec = libc::UTIME_NOW;
            } else if valid.contains(SetattrValid::ATIME) {
                tvs[0].tv_sec = attr.st_atime;
                tvs[0].tv_nsec = attr.st_atime_nsec;
            }

            if valid.contains(SetattrValid::MTIME_NOW) {
                tvs[1].tv_nsec = libc::UTIME_NOW;
            } else if valid.contains(SetattrValid::MTIME) {
                tvs[1].tv_sec = attr.st_mtime;
                tvs[1].tv_nsec = attr.st_mtime_nsec;
            }

            // Safe because this doesn't modify any memory and we check the return value.
            let res = match data {
                Data::Handle(_, fd) => unsafe { libc::futimens(fd, tvs.as_ptr()) },
                Data::ProcPath(ref p) => unsafe {
                    libc::utimensat(self.proc_self_fd.as_raw_fd(), p.as_ptr(), tvs.as_ptr(), 0)
                },
            };
            if res < 0 {
                return Err(io::Error::last_os_error());
            }
        }

        self.do_getattr(inode)
    }

    fn rename(
        &self,
        _ctx: Context,
        olddir: Inode,
        oldname: &CStr,
        newdir: Inode,
        newname: &CStr,
        flags: u32,
    ) -> io::Result<()> {
        let old_inode = self.inodes.get(olddir).ok_or_else(ebadf)?;
        let new_inode = self.inodes.get(newdir).ok_or_else(ebadf)?;

        let old_file = old_inode.get_file()?;
        let new_file = new_inode.get_file()?;

        // Safe because this doesn't modify any memory and we check the return value.
        // TODO: Switch to libc::renameat2 once https://github.com/rust-lang/libc/pull/1508 lands
        // and we have glibc 2.28.
        let res = unsafe {
            libc::syscall(
                libc::SYS_renameat2,
                old_file.as_raw_fd(),
                oldname.as_ptr(),
                new_file.as_raw_fd(),
                newname.as_ptr(),
                flags,
            )
        };
        if res != 0 {
            return Err(io::Error::last_os_error());
        }

        if self.track_migration_info.load(Ordering::Relaxed) {
            // When preparing for migration, we need to tell the migration code that this node has
            // been renamed, which might need to be reflected in the migration info
            if let Err(err) = self.update_inode_migration_info(new_inode, newname) {
                warn!(
                    "Failed to update renamed file's ({oldname:?} -> {newname:?}) migration info, \
                    the migration destination may be unable to find it: {err}",
                );
            }
        }

        Ok(())
    }

    fn mknod(
        &self,
        ctx: Context,
        parent: Inode,
        name: &CStr,
        mode: u32,
        rdev: u32,
        umask: u32,
        extensions: Extensions,
    ) -> io::Result<Entry> {
        let data = self.inodes.get(parent).ok_or_else(ebadf)?;
        let parent_file = data.get_file()?;

        let res = {
            let _credentials_guard = UnixCredentials::new(ctx.uid, ctx.gid)
                .supplementary_gid(
                    self.sup_group_extension.load(Ordering::Relaxed),
                    extensions.sup_gid,
                )
                .set()?;
            let _umask_guard = self
                .posix_acl
                .load(Ordering::Relaxed)
                .then(|| oslib::ScopedUmask::new(umask));

            // Safe because this doesn't modify any memory and we check the return value.
            unsafe {
                libc::mknodat(
                    parent_file.as_raw_fd(),
                    name.as_ptr(),
                    mode as libc::mode_t,
                    u64::from(rdev),
                )
            }
        };

        if res < 0 {
            return Err(io::Error::last_os_error());
        }

        // Set security context on node.
        if let Some(secctx) = extensions.secctx {
            if let Err(e) = self.do_mknod_mkdir_symlink_secctx(&parent_file, name, &secctx) {
                unsafe {
                    libc::unlinkat(parent_file.as_raw_fd(), name.as_ptr(), 0);
                };
                return Err(e);
            }
        }
        self.do_lookup(parent, name, None)
    }

    fn link(
        &self,
        _ctx: Context,
        inode: Inode,
        newparent: Inode,
        newname: &CStr,
    ) -> io::Result<Entry> {
        let data = self.inodes.get(inode).ok_or_else(ebadf)?;
        let new_inode = self.inodes.get(newparent).ok_or_else(ebadf)?;

        let inode_file = data.get_file()?;
        let newparent_file = new_inode.get_file()?;

        let procname = CString::new(format!("{}", inode_file.as_raw_fd()))
            .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;

        // Safe because this doesn't modify any memory and we check the return value.
        let res = unsafe {
            libc::linkat(
                self.proc_self_fd.as_raw_fd(),
                procname.as_ptr(),
                newparent_file.as_raw_fd(),
                newname.as_ptr(),
                libc::AT_SYMLINK_FOLLOW,
            )
        };
        if res == 0 {
            self.do_lookup(newparent, newname, None)
        } else {
            Err(io::Error::last_os_error())
        }
    }

    fn symlink(
        &self,
        ctx: Context,
        linkname: &CStr,
        parent: Inode,
        name: &CStr,
        extensions: Extensions,
    ) -> io::Result<Entry> {
        let data = self.inodes.get(parent).ok_or_else(ebadf)?;
        let parent_file = data.get_file()?;

        let res = {
            let _credentials_guard = UnixCredentials::new(ctx.uid, ctx.gid)
                .supplementary_gid(
                    self.sup_group_extension.load(Ordering::Relaxed),
                    extensions.sup_gid,
                )
                .set()?;

            // Safe because this doesn't modify any memory and we check the return value.
            unsafe { libc::symlinkat(linkname.as_ptr(), parent_file.as_raw_fd(), name.as_ptr()) }
        };

        if res < 0 {
            return Err(io::Error::last_os_error());
        }

        // Set security context on symlink.
        if let Some(secctx) = extensions.secctx {
            if let Err(e) = self.do_mknod_mkdir_symlink_secctx(&parent_file, name, &secctx) {
                unsafe {
                    libc::unlinkat(parent_file.as_raw_fd(), name.as_ptr(), 0);
                };
                return Err(e);
            }
        }

        self.do_lookup(parent, name, None)
    }

    fn readlink(&self, _ctx: Context, inode: Inode) -> io::Result<Vec<u8>> {
        let data = self.inodes.get(inode).ok_or_else(ebadf)?;
        let inode_file = data.get_file()?;

        let mut buf = vec![0; libc::PATH_MAX as usize];

        // Safe because this is a constant value and a valid C string.
        let empty = unsafe { CStr::from_bytes_with_nul_unchecked(EMPTY_CSTR) };

        // Safe because this will only modify the contents of `buf` and we check the return value.
        let res = unsafe {
            libc::readlinkat(
                inode_file.as_raw_fd(),
                empty.as_ptr(),
                buf.as_mut_ptr() as *mut libc::c_char,
                buf.len(),
            )
        };
        if res < 0 {
            return Err(io::Error::last_os_error());
        }

        buf.resize(res as usize, 0);
        Ok(buf)
    }

    fn flush(
        &self,
        _ctx: Context,
        inode: Inode,
        handle: Handle,
        _lock_owner: u64,
    ) -> io::Result<()> {
        let data = self.find_handle(handle, inode)?;

        // Since this method is called whenever an fd is closed in the client, we can emulate that
        // behavior by doing the same thing (dup-ing the fd and then immediately closing it). Safe
        // because this doesn't modify any memory and we check the return values.
        unsafe {
            let newfd = libc::dup(data.file.get()?.write().unwrap().as_raw_fd());
            if newfd < 0 {
                return Err(io::Error::last_os_error());
            }

            if libc::close(newfd) < 0 {
                Err(io::Error::last_os_error())
            } else {
                Ok(())
            }
        }
    }

    fn fsync(&self, _ctx: Context, inode: Inode, datasync: bool, handle: Handle) -> io::Result<()> {
        let data = self.find_handle(handle, inode)?;

        let fd = data.file.get()?.write().unwrap().as_raw_fd();

        // Safe because this doesn't modify any memory and we check the return value.
        let res = unsafe {
            if datasync {
                libc::fdatasync(fd)
            } else {
                libc::fsync(fd)
            }
        };

        if res == 0 {
            Ok(())
        } else {
            Err(io::Error::last_os_error())
        }
    }

    fn fsyncdir(
        &self,
        ctx: Context,
        inode: Inode,
        datasync: bool,
        handle: Handle,
    ) -> io::Result<()> {
        self.fsync(ctx, inode, datasync, handle)
    }

    fn access(&self, ctx: Context, inode: Inode, mask: u32) -> io::Result<()> {
        let data = self.inodes.get(inode).ok_or_else(ebadf)?;
        let inode_file = data.get_file()?;
        let st = statx(&inode_file, None)?.st;
        let mode = mask as i32 & (libc::R_OK | libc::W_OK | libc::X_OK);

        if mode == libc::F_OK {
            // The file exists since we were able to call `stat(2)` on it.
            return Ok(());
        }

        if (mode & libc::R_OK) != 0
            && ctx.uid != 0
            && (st.st_uid != ctx.uid || st.st_mode & 0o400 == 0)
            && (st.st_gid != ctx.gid || st.st_mode & 0o040 == 0)
            && st.st_mode & 0o004 == 0
        {
            return Err(io::Error::from_raw_os_error(libc::EACCES));
        }

        if (mode & libc::W_OK) != 0
            && ctx.uid != 0
            && (st.st_uid != ctx.uid || st.st_mode & 0o200 == 0)
            && (st.st_gid != ctx.gid || st.st_mode & 0o020 == 0)
            && st.st_mode & 0o002 == 0
        {
            return Err(io::Error::from_raw_os_error(libc::EACCES));
        }

        // root can only execute something if it is executable by one of the owner, the group, or
        // everyone.
        if (mode & libc::X_OK) != 0
            && (ctx.uid != 0 || st.st_mode & 0o111 == 0)
            && (st.st_uid != ctx.uid || st.st_mode & 0o100 == 0)
            && (st.st_gid != ctx.gid || st.st_mode & 0o010 == 0)
            && st.st_mode & 0o001 == 0
        {
            return Err(io::Error::from_raw_os_error(libc::EACCES));
        }

        Ok(())
    }

    fn setxattr(
        &self,
        ctx: Context,
        inode: Inode,
        name: &CStr,
        value: &[u8],
        flags: u32,
        extra_flags: SetxattrFlags,
    ) -> io::Result<()> {
        if !self.cfg.xattr {
            return Err(io::Error::from_raw_os_error(libc::ENOSYS));
        }

        let data = self.inodes.get(inode).ok_or_else(ebadf)?;
        let name = self.map_client_xattrname(name)?;

        // If we are setting posix access acl and if SGID needs to be
        // cleared, then switch to caller's gid and drop CAP_FSETID
        // and that should make sure host kernel clears SGID.
        //
        // This probably will not work when we support idmapped mounts.
        // In that case we will need to find a non-root gid and switch
        // to it. (Instead of gid in request). Fix it when we support
        // idmapped mounts.
        let xattr_name = name.as_ref().to_str().unwrap();
        let _clear_sgid_guard = if self.posix_acl.load(Ordering::Relaxed)
            && extra_flags.contains(SetxattrFlags::SETXATTR_ACL_KILL_SGID)
            && xattr_name.eq("system.posix_acl_access")
        {
            let cap_guard = drop_effective_cap("FSETID")?;
            let credentials_guard = UnixCredentials::new(ctx.uid, ctx.gid).set()?;

            // If `UnixCredentials::set()` changes the effective user ID to non-zero, then the
            // effective set is cleared from all capabilities. When switching back to root the
            // permitted set is copied to the effective set. We need to keep `DAC_READ_SEARCH`
            // to use file handles.
            if self.cfg.inode_file_handles != InodeFileHandlesMode::Never {
                if let Err(e) = crate::util::add_cap_to_eff("DAC_READ_SEARCH") {
                    warn!(
                        "failed to add 'DAC_READ_SEARCH' to the effective set of capabilities: {}",
                        e
                    );
                }
            }

            (cap_guard, credentials_guard)
        } else {
            (None, None)
        };

        let res = if is_safe_inode(data.mode) {
            // The f{set,get,remove,list}xattr functions don't work on an fd opened with `O_PATH` so we
            // need to get a new fd.
            let file = self.open_inode(inode, libc::O_RDONLY | libc::O_NONBLOCK)?;

            self.clear_file_capabilities(file.as_raw_fd(), false)?;

            // Safe because this doesn't modify any memory and we check the return value.
            unsafe {
                libc::fsetxattr(
                    file.as_raw_fd(),
                    name.as_ptr(),
                    value.as_ptr() as *const libc::c_void,
                    value.len(),
                    flags as libc::c_int,
                )
            }
        } else {
            let file = data.get_file()?;

            self.clear_file_capabilities(file.as_raw_fd(), true)?;

            let procname = CString::new(format!("{}", file.as_raw_fd()))
                .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;

            let _working_dir_guard =
                set_working_directory(self.proc_self_fd.as_raw_fd(), self.root_fd.as_raw_fd());

            // Safe because this doesn't modify any memory and we check the return value.
            unsafe {
                libc::setxattr(
                    procname.as_ptr(),
                    name.as_ptr(),
                    value.as_ptr() as *const libc::c_void,
                    value.len(),
                    flags as libc::c_int,
                )
            }
        };
        if res == 0 {
            Ok(())
        } else {
            Err(io::Error::last_os_error())
        }
    }

    fn getxattr(
        &self,
        _ctx: Context,
        inode: Inode,
        name: &CStr,
        size: u32,
    ) -> io::Result<GetxattrReply> {
        if !self.cfg.xattr {
            return Err(io::Error::from_raw_os_error(libc::ENOSYS));
        }

        let mut buf = vec![0; size as usize];

        let name = self.map_client_xattrname(name).map_err(|e| {
            if e.kind() == ErrorKind::PermissionDenied {
                io::Error::from_raw_os_error(libc::ENODATA)
            } else {
                e
            }
        })?;

        let data = self.inodes.get(inode).ok_or_else(ebadf)?;

        let res = if is_safe_inode(data.mode) {
            // The f{set,get,remove,list}xattr functions don't work on an fd opened with `O_PATH` so we
            // need to get a new fd.
            let file = self.open_inode(inode, libc::O_RDONLY | libc::O_NONBLOCK)?;

            // Safe because this will only modify the contents of `buf`.
            unsafe {
                libc::fgetxattr(
                    file.as_raw_fd(),
                    name.as_ptr(),
                    buf.as_mut_ptr() as *mut libc::c_void,
                    size as libc::size_t,
                )
            }
        } else {
            let file = data.get_file()?;

            let procname = CString::new(format!("{}", file.as_raw_fd()))
                .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;

            let _working_dir_guard =
                set_working_directory(self.proc_self_fd.as_raw_fd(), self.root_fd.as_raw_fd());

            // Safe because this will only modify the contents of `buf`.
            unsafe {
                libc::getxattr(
                    procname.as_ptr(),
                    name.as_ptr(),
                    buf.as_mut_ptr() as *mut libc::c_void,
                    size as libc::size_t,
                )
            }
        };
        if res < 0 {
            return Err(io::Error::last_os_error());
        }

        if size == 0 {
            Ok(GetxattrReply::Count(res as u32))
        } else {
            buf.resize(res as usize, 0);
            Ok(GetxattrReply::Value(buf))
        }
    }

    fn listxattr(&self, _ctx: Context, inode: Inode, size: u32) -> io::Result<ListxattrReply> {
        if !self.cfg.xattr {
            return Err(io::Error::from_raw_os_error(libc::ENOSYS));
        }

        let data = self.inodes.get(inode).ok_or_else(ebadf)?;
        let mut buf = vec![0; size as usize];

        let res = if is_safe_inode(data.mode) {
            // The f{set,get,remove,list}xattr functions don't work on an fd opened with `O_PATH` so we
            // need to get a new fd.
            let file = self.open_inode(inode, libc::O_RDONLY | libc::O_NONBLOCK)?;

            // Safe because this will only modify the contents of `buf`.
            unsafe {
                libc::flistxattr(
                    file.as_raw_fd(),
                    buf.as_mut_ptr() as *mut libc::c_char,
                    size as libc::size_t,
                )
            }
        } else {
            let file = data.get_file()?;

            let procname = CString::new(format!("{}", file.as_raw_fd()))
                .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;

            let _working_dir_guard =
                set_working_directory(self.proc_self_fd.as_raw_fd(), self.root_fd.as_raw_fd());

            // Safe because this will only modify the contents of `buf`.
            unsafe {
                libc::listxattr(
                    procname.as_ptr(),
                    buf.as_mut_ptr() as *mut libc::c_char,
                    size as libc::size_t,
                )
            }
        };
        if res < 0 {
            return Err(io::Error::last_os_error());
        }

        if size == 0 {
            Ok(ListxattrReply::Count(res as u32))
        } else {
            buf.resize(res as usize, 0);
            let buf = self.map_server_xattrlist(buf);
            Ok(ListxattrReply::Names(buf))
        }
    }

    fn removexattr(&self, _ctx: Context, inode: Inode, name: &CStr) -> io::Result<()> {
        if !self.cfg.xattr {
            return Err(io::Error::from_raw_os_error(libc::ENOSYS));
        }

        let data = self.inodes.get(inode).ok_or_else(ebadf)?;
        let name = self.map_client_xattrname(name)?;

        let res = if is_safe_inode(data.mode) {
            // The f{set,get,remove,list}xattr functions don't work on an fd opened with `O_PATH` so we
            // need to get a new fd.
            let file = self.open_inode(inode, libc::O_RDONLY | libc::O_NONBLOCK)?;

            // Safe because this doesn't modify any memory and we check the return value.
            unsafe { libc::fremovexattr(file.as_raw_fd(), name.as_ptr()) }
        } else {
            let file = data.get_file()?;

            let procname = CString::new(format!("{}", file.as_raw_fd()))
                .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;

            let _working_dir_guard =
                set_working_directory(self.proc_self_fd.as_raw_fd(), self.root_fd.as_raw_fd());

            // Safe because this doesn't modify any memory and we check the return value.
            unsafe { libc::removexattr(procname.as_ptr(), name.as_ptr()) }
        };

        if res == 0 {
            Ok(())
        } else {
            Err(io::Error::last_os_error())
        }
    }

    fn fallocate(
        &self,
        _ctx: Context,
        inode: Inode,
        handle: Handle,
        mode: u32,
        offset: u64,
        length: u64,
    ) -> io::Result<()> {
        let data = self.find_handle(handle, inode)?;

        let fd = data.file.get()?.write().unwrap().as_raw_fd();
        // Safe because this doesn't modify any memory and we check the return value.
        let res = unsafe {
            libc::fallocate64(
                fd,
                mode as libc::c_int,
                offset as libc::off64_t,
                length as libc::off64_t,
            )
        };
        if res == 0 {
            Ok(())
        } else {
            Err(io::Error::last_os_error())
        }
    }

    fn lseek(
        &self,
        _ctx: Context,
        inode: Inode,
        handle: Handle,
        offset: u64,
        whence: u32,
    ) -> io::Result<u64> {
        let data = self.find_handle(handle, inode)?;

        let fd = data.file.get()?.write().unwrap().as_raw_fd();

        // Safe because this doesn't modify any memory and we check the return value.
        let res = unsafe { libc::lseek(fd, offset as libc::off64_t, whence as libc::c_int) };
        if res < 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(res as u64)
        }
    }

    fn copyfilerange(
        &self,
        _ctx: Context,
        inode_in: Inode,
        handle_in: Handle,
        offset_in: u64,
        inode_out: Inode,
        handle_out: Handle,
        offset_out: u64,
        len: u64,
        flags: u64,
    ) -> io::Result<usize> {
        let data_in = self.find_handle(handle_in, inode_in)?;

        // Take just a read lock as we're not going to alter the file descriptor offset.
        let fd_in = data_in.file.get()?.read().unwrap().as_raw_fd();

        let data_out = self.find_handle(handle_out, inode_out)?;

        // Take just a read lock as we're not going to alter the file descriptor offset.
        let fd_out = data_out.file.get()?.read().unwrap().as_raw_fd();

        // Safe because this will only modify `offset_in` and `offset_out` and we check
        // the return value.
        let res = unsafe {
            libc::syscall(
                libc::SYS_copy_file_range,
                fd_in,
                &mut (offset_in as i64) as &mut _ as *mut _,
                fd_out,
                &mut (offset_out as i64) as &mut _ as *mut _,
                len,
                flags,
            )
        };
        if res < 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(res as usize)
        }
    }

    fn syncfs(&self, _ctx: Context, inode: Inode) -> io::Result<()> {
        // TODO: Branch here depending on whether virtiofsd announces submounts or not.

        let file = self.open_inode(inode, libc::O_RDONLY | libc::O_NOFOLLOW)?;
        let raw_fd = file.as_raw_fd();
        debug!("syncfs: inode={}, mount_fd={}", inode, raw_fd);
        let ret = unsafe { libc::syncfs(raw_fd) };
        if ret != 0 {
            // Thread-safe, because errno is stored in thread-local storage.
            Err(io::Error::last_os_error())
        } else {
            Ok(())
        }
    }
}

impl HandleDataFile {
    fn get(&self) -> io::Result<&'_ RwLock<File>> {
        match self {
            HandleDataFile::File(file) => Ok(file),
            HandleDataFile::Invalid(err) => Err(io::Error::new(err.kind(), Arc::clone(err))),
        }
    }
}

impl From<File> for HandleDataFile {
    fn from(file: File) -> Self {
        HandleDataFile::File(RwLock::new(file))
    }
}

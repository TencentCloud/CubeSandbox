// Copyright 2024 Red Hat, Inc. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//! Implementation of a read-only variant of [`PassthroughFs`].
//!
//! Implements a wrapper around [`PassthroughFs`] ([`PassthroughFsRo`]) that prohibits all
//! operations that would modify anything within the shared directory.  This wrapper implements the
//! [`FileSystem`] and [`SerializableFileSystem`] traits, so can be used as a virtiofsd filesystem
//! driver.

use super::util::{einval, erofs, openat};
use super::PassthroughFs;
use crate::filesystem::{
    Context, Entry, Extensions, FileSystem, FsOptions, GetxattrReply, ListxattrReply, OpenOptions,
    SerializableFileSystem, SetattrValid, SetxattrFlags, ZeroCopyReader, ZeroCopyWriter,
};
use crate::passthrough::stat::{statx, StatExt};

use std::convert::TryInto;
use std::ffi::CStr;
use std::fs::File;
use std::io;
use std::sync::atomic::AtomicBool;
use std::sync::Arc;
use std::time::Duration;

/// Wrapper around `PassthroughFs`, prohibiting modifications.
///
/// Prevent any operation that would modify the underlying filesystem.
pub struct PassthroughFsRo(PassthroughFs);

impl PassthroughFsRo {
    /// Create a `PassthroughFsRo` filesystem.
    ///
    /// Internally creates a `PassthroughFs` filesystem using the `cfg` configuration, then wraps
    /// it in the `PassthroughFsRo` type.
    pub fn new(cfg: super::Config) -> io::Result<Self> {
        let inner = PassthroughFs::new(cfg)?;
        Ok(PassthroughFsRo(inner))
    }

    /// Internal: Run an `open()`-like function without allowing modifications or write access.
    ///
    /// That means:
    /// - Prevent access modes other than `O_RDONLY` and the following flags:
    ///   - O_EXCL: We filter out `O_CREAT`, and then, its behavior will be undefined (except for
    ///     block devices, which don’t really work with virtio-fs anyway).  In any case, on a
    ///     read-only filesystem, `O_CREAT | O_EXCL` will always give an error.
    ///   - O_TMPFILE: Not allowed with `O_RDONLY`.
    ///   - O_TRUNC: Undefined behavior with `O_RDONLY`, might truncate anyway.
    /// - Filter out `O_CREAT`, and return `EROFS` if the path does not exist yet
    ///
    /// `open_fn` runs the underlying open function, taking the potentially modified flags as an
    /// argument.
    fn rofs_open<R, F: FnOnce(u32) -> io::Result<R>>(flags: u32, open_fn: F) -> io::Result<R> {
        let cflags: libc::c_int = flags
            .try_into()
            .map_err(|e| io::Error::new(io::ErrorKind::InvalidInput, e))?;

        // `O_PATH` ignores all flags but `O_CLOEXEC | O_DIRECTORY | O_NOFOLLOW`, just allow it
        // wholesale
        if cflags & libc::O_PATH != 0 {
            return open_fn(flags);
        }

        if cflags & libc::O_ACCMODE != libc::O_RDONLY {
            return Err(erofs());
        }

        // Problem: We would like to have an allowlist, not a denylist; `O_LARGEFILE` would be in
        // that allowlist (and is indeed set by guests), but `libc::O_LARGEFILE == 0`.  The actual
        // value (in C) can vary between systems (and it seems that it does indeed vary), so we
        // cannot use an allowlist, and have to live with a denylist only.

        // For what it’s worth, here is what the allowlist would be (for comparison against the
        // denylist below):
        // - O_ACCMODE
        //     -- already checked to be O_RDONLY
        // - O_APPEND | O_DSYNC | O_SYNC
        //     -- only affect writes, so useless here, but not harmful
        // - O_CREAT
        //     -- special-case below
        // - O_NOATIME
        //     -- perfectly OK; in fact, we would rather force-set it, but cannot (see
        //     --preserve-noatime)
        // - O_ASYNC | O_CLOEXEC | O_DIRECT | O_DIRECTORY | O_LARGEFILE | O_NOCTTY | O_NOFOLLOW |
        //   O_NONBLOCK
        //     -- Don’t have anything to do with writing in particular.

        // Note that at least `O_TMPFILE` occupies multiple bits, so we need to check exactly.  Do
        // it for the other flags, too, why not.
        if cflags & libc::O_EXCL == libc::O_EXCL {
            // O_EXCL is undefined without O_CREAT (which we filter out below).  Then again, on a
            // read-only filesystem, O_CREAT | O_EXCL will always return an error, so we can just
            // do that here.  Maybe we should check whether to return EROFS or EEXIST, depending on
            // the case, but then again, if someone tries to create a new file on a read-only
            // filesystem, we can just tell them it’s EROFS.
            // (And it’s also weird to use O_CREAT | O_EXCL | O_RDONLY.)
            return Err(erofs());
        }
        if cflags & libc::O_TMPFILE == libc::O_TMPFILE {
            // O_TMPFILE | O_RDONLY should have already resulted in EINVAL in the guest, but better
            // safeguard it explicitly.
            return Err(einval());
        }
        if cflags & libc::O_TRUNC == libc::O_TRUNC {
            // Undefined with O_RDONLY, so we can do what we want.  We need to error out, though,
            // lest passing it to the host will truncate the file even with O_RDONLY.
            return Err(einval());
        }

        if cflags & libc::O_CREAT == 0 {
            open_fn(flags)
        } else {
            // Try to open without CREAT, if that fails, return EROFS
            open_fn(flags & !(libc::O_CREAT as u32)).map_err(|err| {
                if err.kind() == io::ErrorKind::NotFound {
                    erofs()
                } else {
                    err
                }
            })
        }
    }
}

/// Create function definitions that always fall through to the corresponding function on `self.0`
macro_rules! ops_allow {
    {
        $(
            fn $name:ident$(<$($gen_name:ident: $gen_trait:path),*>)?(
                &self
                $(, $($par_name:ident: $par_type:ty),*)?
                $(,)?
            )$( -> $ret:ty)?;
        )*
    } => {
        $(
            fn $name$(<$($gen_name: $gen_trait),*>)?(
                &self
                $(, $($par_name: $par_type),*)?
            )$( -> $ret)? {
                self.0.$name($($($par_name),*)?)
            }
        )*
    }
}

/// Create function definitions that always return `Err(erofs())`
macro_rules! ops_forbid {
    {
        $(
            fn $name:ident$(<$($gen_name:ident: $gen_trait:path),*>)?(
                &self
                $(, $($par_name:ident: $par_type:ty),*)?
                $(,)?
            ) -> io::Result<$ret_ok:ty>;
        )*
    } => {
        $(
            fn $name$(<$($gen_name: $gen_trait),*>)?(
                &self
                $(, $($par_name: $par_type),*)?
            ) -> io::Result<$ret_ok> {
                Err(erofs())
            }
        )*
    }
}

impl FileSystem for PassthroughFsRo {
    type Inode = <PassthroughFs as FileSystem>::Inode;
    type Handle = <PassthroughFs as FileSystem>::Handle;
    type DirIter = <PassthroughFs as FileSystem>::DirIter;

    // Execute these functions without restrictions
    ops_allow! {
        fn init(&self, capable: FsOptions) -> io::Result<FsOptions>;
        fn destroy(&self);
        fn lookup(&self, ctx: Context, parent: Self::Inode, name: &CStr,
            f_info: Option<(String, StatExt)>,
        ) -> io::Result<Entry>;
        fn forget(&self, ctx: Context, inode: Self::Inode, count: u64);
        fn batch_forget(&self, ctx: Context, requests: Vec<(Self::Inode, u64)>);
        fn getattr(&self,
            ctx: Context,
            inode: Self::Inode,
            handle: Option<Self::Handle>,
        ) -> io::Result<(libc::stat64, Duration)>;
        fn readlink(&self, ctx: Context, inode: Self::Inode) -> io::Result<Vec<u8>>;
        fn read<W: ZeroCopyWriter>(
            &self,
            ctx: Context,
            inode: Self::Inode,
            handle: Self::Handle,
            w: W,
            size: u32,
            offset: u64,
            lock_owner: Option<u64>,
            flags: u32,
        ) -> io::Result<usize>;
        fn flush(
            &self,
            ctx: Context,
            inode: Self::Inode,
            handle: Self::Handle,
            lock_owner: u64,
        ) -> io::Result<()>;
        fn fsync(
            &self,
            ctx: Context,
            inode: Self::Inode,
            datasync: bool,
            handle: Self::Handle,
        ) -> io::Result<()>;
        fn release(
            &self,
            ctx: Context,
            inode: Self::Inode,
            flags: u32,
            handle: Self::Handle,
            flush: bool,
            flock_release: bool,
            lock_owner: Option<u64>,
        ) -> io::Result<()>;
        fn statfs(&self, ctx: Context, inode: Self::Inode) -> io::Result<libc::statvfs64>;
        fn getxattr(
            &self,
            ctx: Context,
            inode: Self::Inode,
            name: &CStr,
            size: u32,
        ) -> io::Result<GetxattrReply>;
        fn listxattr(
            &self,
            ctx: Context,
            inode: Self::Inode,
            size: u32,
        ) -> io::Result<ListxattrReply>;
        fn readdir(
            &self,
            ctx: Context,
            inode: Self::Inode,
            handle: Self::Handle,
            size: u32,
            offset: u64,
        ) -> io::Result<Self::DirIter>;
        fn fsyncdir(
            &self,
            ctx: Context,
            inode: Self::Inode,
            datasync: bool,
            handle: Self::Handle,
        ) -> io::Result<()>;
        fn releasedir(
            &self,
            ctx: Context,
            inode: Self::Inode,
            flags: u32,
            handle: Self::Handle,
        ) -> io::Result<()>;
        fn lseek(
            &self,
            ctx: Context,
            inode: Self::Inode,
            handle: Self::Handle,
            offset: u64,
            whence: u32,
        ) -> io::Result<u64>;
        fn syncfs(&self, ctx: Context, inode: Self::Inode) -> io::Result<()>;
    }

    // Refuse to run these functions, always returning EROFS.
    // Note: We assume that these functions must always fail on a read-only filesystem, so failing
    // without further checks should be safe and reasonable.  However, the Linux kernel treats
    // EROFS more like a final barrier, i.e. something that is returned only if the operation would
    // succeed on a writable filesystem.  For example, on an -o ro filesystem, `mkdir()` will not
    // return EROFS immediately, but first check whether the path already exists, and if so, return
    // EEXIST instead.  That would be complicated though (and might introduce TOCTTOU problems), so
    // unconditionally returning EROFS seems like a more viable option for us.
    // (FWIW, the FUSE kernel driver does not seem to special-case EEXIST.)
    ops_forbid! {
        fn setattr(
            &self,
            _ctx: Context,
            _inode: Self::Inode,
            _attr: libc::stat64,
            _handle: Option<Self::Handle>,
            _valid: SetattrValid,
        ) -> io::Result<(libc::stat64, Duration)>;
        fn symlink(
            &self,
            _ctx: Context,
            _linkname: &CStr,
            _parent: Self::Inode,
            _name: &CStr,
            _extensions: Extensions,
        ) -> io::Result<Entry>;
        fn mknod(
            &self,
            _ctx: Context,
            _parent: Self::Inode,
            _name: &CStr,
            _mode: u32,
            _rdev: u32,
            _umask: u32,
            _extensions: Extensions,
        ) -> io::Result<Entry>;
        fn mkdir(
            &self,
            _ctx: Context,
            _parent: Self::Inode,
            _name: &CStr,
            _mode: u32,
            _umask: u32,
            _extensions: Extensions,
        ) -> io::Result<Entry>;
        fn unlink(&self, _ctx: Context, _parent: Self::Inode, _name: &CStr) -> io::Result<()>;
        fn rmdir(&self, _ctx: Context, _parent: Self::Inode, _name: &CStr) -> io::Result<()>;
        fn rename(
            &self,
            _ctx: Context,
            _olddir: Self::Inode,
            _oldname: &CStr,
            _newdir: Self::Inode,
            _newname: &CStr,
            _flags: u32,
        ) -> io::Result<()>;
        fn link(
            &self,
            _ctx: Context,
            _inode: Self::Inode,
            _newparent: Self::Inode,
            _newname: &CStr,
        ) -> io::Result<Entry>;
        fn write<R: ZeroCopyReader>(
            &self,
            _ctx: Context,
            _inode: Self::Inode,
            _handle: Self::Handle,
            _r: R,
            _size: u32,
            _offset: u64,
            _lock_owner: Option<u64>,
            _delayed_write: bool,
            _kill_priv: bool,
            _flags: u32,
        ) -> io::Result<usize>;
        fn fallocate(
            &self,
            _ctx: Context,
            _inode: Self::Inode,
            _handle: Self::Handle,
            _mode: u32,
            _offset: u64,
            _length: u64,
        ) -> io::Result<()>;
        fn setxattr(
            &self,
            _ctx: Context,
            _inode: Self::Inode,
            _name: &CStr,
            _value: &[u8],
            _flags: u32,
            _extra_flags: SetxattrFlags,
        ) -> io::Result<()>;
        fn removexattr(&self, _ctx: Context, _inode: Self::Inode, _name: &CStr) -> io::Result<()>;
        fn copyfilerange(
            &self,
            _ctx: Context,
            _inode_in: Self::Inode,
            _handle_in: Self::Handle,
            _offset_in: u64,
            _inode_out: Self::Inode,
            _handle_out: Self::Handle,
            _offset_out: u64,
            _len: u64,
            _flags: u64,
        ) -> io::Result<usize>;
    }

    fn open(
        &self,
        ctx: Context,
        inode: Self::Inode,
        kill_priv: bool,
        flags: u32,
    ) -> io::Result<(Option<Self::Handle>, OpenOptions)> {
        Self::rofs_open(flags, |flags| self.0.open(ctx, inode, kill_priv, flags))
    }

    fn create(
        &self,
        ctx: Context,
        parent: Self::Inode,
        name: &CStr,
        _mode: u32,
        kill_priv: bool,
        flags: u32,
        _umask: u32,
        _extensions: Extensions,
    ) -> io::Result<(Entry, Option<Self::Handle>, OpenOptions)> {
        // We never want to create, but we should allow opening existing files
        let entry = self.lookup(ctx, parent, name, None).map_err(|err| {
            if err.kind() == io::ErrorKind::NotFound {
                erofs()
            } else {
                err
            }
        })?;
        let (handle, opts) = self.open(ctx, entry.inode, kill_priv, flags)?;
        Ok((entry, handle, opts))
    }

    fn opendir(
        &self,
        ctx: Context,
        inode: Self::Inode,
        flags: u32,
    ) -> io::Result<(Option<Self::Handle>, OpenOptions)> {
        Self::rofs_open(flags, |flags| self.0.opendir(ctx, inode, flags))
    }

    fn access(&self, ctx: Context, inode: Self::Inode, mask: u32) -> io::Result<()> {
        if mask & libc::W_OK as u32 != 0 {
            Err(erofs())
        } else {
            self.0.access(ctx, inode, mask)
        }
    }

    fn get_root_ino(&self) -> io::Result<u64> {
        let path_fd = openat(
            &libc::AT_FDCWD,
            self.0.cfg.root_dir.as_str(),
            libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC,
        )?;

        let st = statx(&path_fd, None)?;

        Ok(st.st.st_ino)
    }
}

impl SerializableFileSystem for PassthroughFsRo {
    fn prepare_serialization(&self, cancel: Arc<AtomicBool>) -> io::Result<()> {
        self.0.prepare_serialization(cancel)
    }

    fn serialize(&self, state_pipe: File) -> io::Result<()> {
        self.0.serialize(state_pipe)
    }

    fn deserialize_and_apply(&self, state_pipe: File) -> io::Result<()> {
        self.0.deserialize_and_apply(state_pipe)
    }

    fn serialize_data(&self) -> io::Result<Vec<u8>> {
        self.0.serialize_data()
    }

    fn deserialize_and_apply_data(&self, serialized: &Vec<u8>) -> io::Result<()> {
        self.0.deserialize_and_apply_data(serialized)
    }
}

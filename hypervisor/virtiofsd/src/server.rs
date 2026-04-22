// Copyright 2019 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

use super::fs_cache_req_handler::FsCacheReqHandler;
use crate::descriptor_utils::{Reader, Writer};
use crate::filesystem::{
    Context, DirEntry, DirEntry2, DirectoryIterator, Entry, Extensions, FileSystem, GetxattrReply,
    ListxattrReply, SecContext, SerializableFileSystem, ZeroCopyReader, ZeroCopyWriter,
};
use crate::fuse::*;
use crate::passthrough::util::einval;
use crate::{oslib, Error, Result};
use anyhow::anyhow;
use rate_limiter::{BucketReduction, RateLimiter, TokenType};
use std::convert::{TryFrom, TryInto};
use std::ffi::{CStr, CString};
use std::fs::File;
use std::io::{self, Read, Write};
use std::mem::{size_of, MaybeUninit};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::sync::Mutex;
use std::time::Duration;
use vm_memory::ByteValued;

const FUSE_BUFFER_HEADER_SIZE: u32 = 0x1000;
const MAX_BUFFER_SIZE: u32 = 1 << 20;
const DIRENT_PADDING: [u8; 8] = [0; 8];

const CURRENT_DIR_CSTR: &[u8] = b".";
const PARENT_DIR_CSTR: &[u8] = b"..";

struct ZcReader<'a>(Reader<'a>);

impl<'a> ZeroCopyReader for ZcReader<'a> {
    fn read_to(
        &mut self,
        f: &File,
        count: usize,
        off: u64,
        flags: Option<oslib::WritevFlags>,
    ) -> io::Result<usize> {
        self.0.read_to_at(f, count, off, flags)
    }
}

struct ZcWriter<'a>(Writer<'a>);

impl<'a> ZeroCopyWriter for ZcWriter<'a> {
    fn write_from(&mut self, f: &File, count: usize, off: u64) -> io::Result<usize> {
        self.0.write_from_at(f, count, off)
    }
}

use std::collections::HashMap;
use std::io::ErrorKind;

use crate::passthrough::stat::{self, statx};
use crate::passthrough::util::openat;
use std::ffi::OsStr;
use std::path::Path;

pub fn get_basename_from_path(path: &str) -> Option<&str> {
    let path_obj = Path::new(path);
    path_obj.file_name().and_then(OsStr::to_str)
}

pub struct FilterList {
    root_ino: u64,
    allowed_dirs: HashMap<String, (String, stat::StatExt)>,
    root_dirents: Vec<DirEntry2>,
}

impl FilterList {
    // Init filter base whitelist.
    fn init(&mut self, whitelist: &Option<Vec<String>>) -> Result<()> {
        let mut dic = HashMap::new();
        let mut dirents = Vec::<DirEntry2>::new();
        let mut index = 2;

        dirents.push(DirEntry2 {
            ino: self.root_ino,
            offset: 1,
            type_: libc::DT_DIR as u32,
            name: ".".to_string(),
        });
        dirents.push(DirEntry2 {
            ino: 1001,
            offset: 2,
            type_: libc::DT_DIR as u32,
            name: "..".to_string(),
        });
        // dir should be absolute path
        match whitelist {
            Some(dirs) => {
                // Sort and Dedup dirs.
                let mut dirs = dirs.clone();
                dirs.sort();
                dirs.dedup();

                for dir in dirs {
                    if dir.is_empty() {
                        continue;
                    }
                    let dir_name = match get_basename_from_path(&dir) {
                        Some(path) => path,
                        _ => {
                            warn!("Invalid path name: {:?}", dir);
                            continue;
                        }
                    };
                    let path_fd = openat(
                        &libc::AT_FDCWD,
                        &dir,
                        libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC,
                    )
                    .map_err(|e| {
                        Error::InvalidFilter(anyhow!("Failed to openat file {:?}: {:?}", dir, e))
                    })?;
                    let st = statx(&path_fd, None).map_err(|e| {
                        Error::InvalidFilter(anyhow!(
                            "Failed to statx file {:?} fd {:?}: {:?}",
                            dir,
                            path_fd,
                            e
                        ))
                    })?;
                    let ino = st.st.st_ino;
                    dic.insert(dir_name.to_string(), (dir.clone(), st));
                    index += 1;
                    dirents.push(DirEntry2 {
                        ino,
                        offset: index,
                        type_: libc::DT_DIR as u32,
                        name: dir_name.to_string(),
                    });
                }
            }
            None => {
                info!("allowed_dirs is None");
            }
        }

        self.allowed_dirs = dic;
        self.root_dirents = dirents;

        Ok(())
    }

    pub fn new(root_ino: u64, whitelist: &Option<Vec<String>>) -> Result<FilterList> {
        let mut filter = FilterList {
            root_ino,
            allowed_dirs: HashMap::new(),
            root_dirents: Vec::<DirEntry2>::new(),
        };

        filter.init(whitelist)?;

        Ok(filter)
    }

    // update filter with new whitelist.
    pub fn update(&mut self, whitelist: &Option<Vec<String>>) -> Result<()> {
        self.init(whitelist)?;

        Ok(())
    }

    // whether allowed_dirs is empty.
    pub fn is_empty(&self) -> bool {
        self.allowed_dirs.is_empty()
    }

    // return allow dire entry.
    pub fn get_allow_dir(&self, key: String) -> Result<(String, stat::StatExt)> {
        if !self.allowed_dirs.contains_key(&key) {
            return Err(Error::DisAllowed);
        }

        Ok(self.allowed_dirs[&key].clone())
    }

    // return root dirent.
    pub fn get_root_dirents(&self) -> &Vec<DirEntry2> {
        &self.root_dirents
    }
}

pub struct Server<F: FileSystem + Sync> {
    fs: F,
    options: AtomicU64,
    root_filter: Arc<Mutex<FilterList>>,
}

impl<F: FileSystem + Sync> Server<F> {
    pub fn new(fs: F, whitelist: &Option<Vec<String>>) -> Result<Server<F>> {
        let root_ino = fs.get_root_ino().map_err(Error::InternalError)?;
        let filter = FilterList::new(root_ino, whitelist)?;

        Ok(Server {
            fs,
            options: AtomicU64::new(FsOptions::empty().bits()),
            root_filter: Arc::new(Mutex::new(filter)),
        })
    }

    pub fn update_filter(&self, whitelist: &Option<Vec<String>>) -> Result<()> {
        self.root_filter
            .as_ref()
            .lock()
            .unwrap()
            .update(whitelist)?;

        Ok(())
    }

    #[allow(clippy::cognitive_complexity)]
    pub fn handle_message<T: FsCacheReqHandler>(
        &self,
        mut r: Reader,
        w: Writer,
        vu_req: Option<&mut T>,
        rl: &mut Option<RateLimiter>,
        rlret: &mut BucketReduction,
        rltyp: &mut TokenType,
    ) -> Result<usize> {
        let in_header: InHeader = r.read_obj().map_err(Error::DecodeMessage)?;

        if in_header.len > (MAX_BUFFER_SIZE + FUSE_BUFFER_HEADER_SIZE) {
            return reply_error(
                io::Error::from_raw_os_error(libc::ENOMEM),
                in_header.unique,
                w,
            );
        }

        if let Ok(opcode) = Opcode::try_from(in_header.opcode) {
            debug!(
                "Received request: opcode={:?} ({}), inode={}, unique={}, pid={}",
                opcode, in_header.opcode, in_header.nodeid, in_header.unique, in_header.pid
            );
            match opcode {
                Opcode::Lookup => self.lookup(in_header, r, w),
                Opcode::Forget => self.forget(in_header, r), // No reply.
                Opcode::Getattr => self.getattr(in_header, r, w),
                Opcode::Setattr => self.setattr(in_header, r, w),
                Opcode::Readlink => self.readlink(in_header, w),
                Opcode::Symlink => self.symlink(in_header, r, w),
                Opcode::Mknod => self.mknod(in_header, r, w),
                Opcode::Mkdir => self.mkdir(in_header, r, w),
                Opcode::Unlink => self.unlink(in_header, r, w),
                Opcode::Rmdir => self.rmdir(in_header, r, w),
                Opcode::Rename => self.rename(in_header, r, w),
                Opcode::Link => self.link(in_header, r, w),
                Opcode::Open => self.open(in_header, r, w),
                Opcode::Read => self.read(in_header, r, w, rl, rlret, rltyp),
                Opcode::Write => self.write(in_header, r, w),
                Opcode::Statfs => self.statfs(in_header, w),
                Opcode::Release => self.release(in_header, r, w),
                Opcode::Fsync => self.fsync(in_header, r, w),
                Opcode::Setxattr => self.setxattr(in_header, r, w),
                Opcode::Getxattr => self.getxattr(in_header, r, w),
                Opcode::Listxattr => self.listxattr(in_header, r, w),
                Opcode::Removexattr => self.removexattr(in_header, r, w),
                Opcode::Flush => self.flush(in_header, r, w),
                Opcode::Init => self.init(in_header, r, w),
                Opcode::Opendir => self.opendir(in_header, r, w),
                Opcode::Readdir => self.readdir(in_header, r, w),
                Opcode::Releasedir => self.releasedir(in_header, r, w),
                Opcode::Fsyncdir => self.fsyncdir(in_header, r, w),
                Opcode::Getlk => self.getlk(in_header, r, w),
                Opcode::Setlk => self.setlk(in_header, r, w),
                Opcode::Setlkw => self.setlkw(in_header, r, w),
                Opcode::Access => self.access(in_header, r, w),
                Opcode::Create => self.create(in_header, r, w),
                Opcode::Interrupt => Ok(self.interrupt(in_header)),
                Opcode::Bmap => self.bmap(in_header, r, w),
                Opcode::Destroy => Ok(self.destroy()),
                Opcode::Ioctl => self.ioctl(in_header, r, w),
                Opcode::Poll => self.poll(in_header, r, w),
                Opcode::NotifyReply => self.notify_reply(in_header, r, w),
                Opcode::BatchForget => self.batch_forget(in_header, r, w),
                Opcode::Fallocate => self.fallocate(in_header, r, w),
                Opcode::Readdirplus => self.readdirplus(in_header, r, w),
                Opcode::Rename2 => self.rename2(in_header, r, w),
                Opcode::Lseek => self.lseek(in_header, r, w),
                Opcode::CopyFileRange => self.copyfilerange(in_header, r, w),
                Opcode::SetupMapping => self.setupmapping(in_header, r, w, vu_req),
                Opcode::RemoveMapping => self.removemapping(in_header, r, w, vu_req),
                Opcode::Syncfs => self.syncfs(in_header, w),
                Opcode::TmpFile => self.tmpfile(in_header, r, w),
            }
        } else {
            debug!(
                "Received unknown request: opcode={}, inode={}",
                in_header.opcode, in_header.nodeid
            );
            reply_error(
                io::Error::from_raw_os_error(libc::ENOSYS),
                in_header.unique,
                w,
            )
        }
    }

    fn setupmapping<T: FsCacheReqHandler>(
        &self,
        in_header: InHeader,
        mut r: Reader,
        w: Writer,
        vu_req: Option<&mut T>,
    ) -> Result<usize> {
        if let Some(req) = vu_req {
            let SetupmappingIn {
                fh,
                foffset,
                len,
                flags,
                moffset,
            } = r.read_obj().map_err(Error::DecodeMessage)?;

            match self.fs.setupmapping(
                Context::from(in_header),
                in_header.nodeid.into(),
                fh.into(),
                foffset,
                len,
                flags,
                moffset,
                req,
            ) {
                Ok(()) => reply_ok(None::<u8>, None, in_header.unique, w),
                Err(e) => reply_error(e, in_header.unique, w),
            }
        } else {
            reply_error(
                io::Error::from_raw_os_error(libc::EINVAL),
                in_header.unique,
                w,
            )
        }
    }

    fn removemapping<T: FsCacheReqHandler>(
        &self,
        in_header: InHeader,
        mut r: Reader,
        w: Writer,
        vu_req: Option<&mut T>,
    ) -> Result<usize> {
        if let Some(req) = vu_req {
            let RemovemappingIn { count } = r.read_obj().map_err(Error::DecodeMessage)?;

            if let Some(size) = (count as usize).checked_mul(size_of::<RemovemappingOne>()) {
                if size > MAX_BUFFER_SIZE as usize {
                    return reply_error(
                        io::Error::from_raw_os_error(libc::ENOMEM),
                        in_header.unique,
                        w,
                    );
                }
            } else {
                return reply_error(
                    io::Error::from_raw_os_error(libc::EOVERFLOW),
                    in_header.unique,
                    w,
                );
            }

            let mut requests = Vec::with_capacity(count as usize);
            for _ in 0..count {
                requests.push(
                    r.read_obj::<RemovemappingOne>()
                        .map_err(Error::DecodeMessage)?,
                );
            }

            match self
                .fs
                .removemapping(Context::from(in_header), requests, req)
            {
                Ok(()) => reply_ok(None::<u8>, None, in_header.unique, w),
                Err(e) => reply_error(e, in_header.unique, w),
            }
        } else {
            reply_error(
                io::Error::from_raw_os_error(libc::EINVAL),
                in_header.unique,
                w,
            )
        }
    }

    fn lookup(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let namelen = (in_header.len as usize)
            .checked_sub(size_of::<InHeader>())
            .ok_or(Error::InvalidHeaderLength)?;

        let mut buf = vec![0u8; namelen];

        r.read_exact(&mut buf).map_err(Error::DecodeMessage)?;

        let name = bytes_to_cstr(buf.as_ref())?;

        let par_id = in_header.nodeid;
        let mut f_info = None;
        if par_id == ROOT_ID {
            let filter = self.root_filter.as_ref().lock().unwrap();
            if !filter.is_empty() {
                match filter.get_allow_dir(name.to_str().unwrap().to_string()) {
                    Ok(entry) => f_info = Some(entry),
                    Err(_) => {
                        return reply_error(ErrorKind::NotFound.into(), in_header.unique, w);
                    }
                }
            }
        }

        match self.fs.lookup(
            Context::from(in_header),
            in_header.nodeid.into(),
            name,
            f_info,
        ) {
            Ok(entry) => {
                let out = EntryOut::from(entry);

                reply_ok(Some(out), None, in_header.unique, w)
            }
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn forget(&self, in_header: InHeader, mut r: Reader) -> Result<usize> {
        let ForgetIn { nlookup } = r.read_obj().map_err(Error::DecodeMessage)?;

        self.fs
            .forget(Context::from(in_header), in_header.nodeid.into(), nlookup);

        // There is no reply for forget messages.
        Ok(0)
    }

    fn getattr(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let GetattrIn { flags, fh, .. } = r.read_obj().map_err(Error::DecodeMessage)?;

        let handle = if (flags & GETATTR_FH) != 0 {
            Some(fh.into())
        } else {
            None
        };

        match self
            .fs
            .getattr(Context::from(in_header), in_header.nodeid.into(), handle)
        {
            Ok((st, timeout)) => {
                let out = AttrOut {
                    attr_valid: timeout.as_secs(),
                    attr_valid_nsec: timeout.subsec_nanos(),
                    dummy: 0,
                    attr: st.into(),
                };
                reply_ok(Some(out), None, in_header.unique, w)
            }
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn setattr(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let setattr_in: SetattrIn = r.read_obj().map_err(Error::DecodeMessage)?;

        let handle = if setattr_in.valid & FATTR_FH != 0 {
            Some(setattr_in.fh.into())
        } else {
            None
        };

        let valid = SetattrValid::from_bits_truncate(setattr_in.valid);

        let st: libc::stat64 = setattr_in.into();

        match self.fs.setattr(
            Context::from(in_header),
            in_header.nodeid.into(),
            st,
            handle,
            valid,
        ) {
            Ok((st, timeout)) => {
                let out = AttrOut {
                    attr_valid: timeout.as_secs(),
                    attr_valid_nsec: timeout.subsec_nanos(),
                    dummy: 0,
                    attr: st.into(),
                };
                reply_ok(Some(out), None, in_header.unique, w)
            }
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn readlink(&self, in_header: InHeader, w: Writer) -> Result<usize> {
        match self
            .fs
            .readlink(Context::from(in_header), in_header.nodeid.into())
        {
            Ok(linkname) => {
                // We need to disambiguate the option type here even though it is `None`.
                reply_ok(None::<u8>, Some(&linkname), in_header.unique, w)
            }
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn symlink(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        // Unfortunately the name and linkname are encoded one after another and
        // separated by a nul character.
        let len = (in_header.len as usize)
            .checked_sub(size_of::<InHeader>())
            .ok_or(Error::InvalidHeaderLength)?;
        let mut buf = vec![0; len];

        r.read_exact(&mut buf).map_err(Error::DecodeMessage)?;

        let mut components = buf.split_inclusive(|c| *c == b'\0');

        let name = components.next().ok_or(Error::MissingParameter)?;
        let linkname = components.next().ok_or(Error::MissingParameter)?;

        let options = FsOptions::from_bits_truncate(self.options.load(Ordering::Relaxed));

        let extensions = get_extensions(options, name.len() + linkname.len(), buf.as_slice())?;

        match self.fs.symlink(
            Context::from(in_header),
            bytes_to_cstr(linkname)?,
            in_header.nodeid.into(),
            bytes_to_cstr(name)?,
            extensions,
        ) {
            Ok(entry) => {
                let out = EntryOut::from(entry);

                reply_ok(Some(out), None, in_header.unique, w)
            }
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn mknod(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let MknodIn {
            mode, rdev, umask, ..
        } = r.read_obj().map_err(Error::DecodeMessage)?;

        let remaining_len = (in_header.len as usize)
            .checked_sub(size_of::<InHeader>())
            .and_then(|l| l.checked_sub(size_of::<MknodIn>()))
            .ok_or(Error::InvalidHeaderLength)?;
        let mut buf = vec![0; remaining_len];

        r.read_exact(&mut buf).map_err(Error::DecodeMessage)?;
        let mut components = buf.split_inclusive(|c| *c == b'\0');
        let name = components.next().ok_or(Error::MissingParameter)?;

        let options = FsOptions::from_bits_truncate(self.options.load(Ordering::Relaxed));

        let extensions = get_extensions(options, name.len(), buf.as_slice())?;

        match self.fs.mknod(
            Context::from(in_header),
            in_header.nodeid.into(),
            bytes_to_cstr(name)?,
            mode,
            rdev,
            umask,
            extensions,
        ) {
            Ok(entry) => {
                let out = EntryOut::from(entry);

                reply_ok(Some(out), None, in_header.unique, w)
            }
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn mkdir(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let MkdirIn { mode, umask } = r.read_obj().map_err(Error::DecodeMessage)?;

        let remaining_len = (in_header.len as usize)
            .checked_sub(size_of::<InHeader>())
            .and_then(|l| l.checked_sub(size_of::<MkdirIn>()))
            .ok_or(Error::InvalidHeaderLength)?;
        let mut buf = vec![0; remaining_len];

        r.read_exact(&mut buf).map_err(Error::DecodeMessage)?;
        let mut components = buf.split_inclusive(|c| *c == b'\0');
        let name = components.next().ok_or(Error::MissingParameter)?;

        let options = FsOptions::from_bits_truncate(self.options.load(Ordering::Relaxed));

        let extensions = get_extensions(options, name.len(), buf.as_slice())?;

        match self.fs.mkdir(
            Context::from(in_header),
            in_header.nodeid.into(),
            bytes_to_cstr(name)?,
            mode,
            umask,
            extensions,
        ) {
            Ok(entry) => {
                let out = EntryOut::from(entry);

                reply_ok(Some(out), None, in_header.unique, w)
            }
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn unlink(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let namelen = (in_header.len as usize)
            .checked_sub(size_of::<InHeader>())
            .ok_or(Error::InvalidHeaderLength)?;
        let mut name = vec![0; namelen];

        r.read_exact(&mut name).map_err(Error::DecodeMessage)?;

        match self.fs.unlink(
            Context::from(in_header),
            in_header.nodeid.into(),
            bytes_to_cstr(&name)?,
        ) {
            Ok(()) => reply_ok(None::<u8>, None, in_header.unique, w),
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn rmdir(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let namelen = (in_header.len as usize)
            .checked_sub(size_of::<InHeader>())
            .ok_or(Error::InvalidHeaderLength)?;
        let mut name = vec![0; namelen];

        r.read_exact(&mut name).map_err(Error::DecodeMessage)?;

        match self.fs.rmdir(
            Context::from(in_header),
            in_header.nodeid.into(),
            bytes_to_cstr(&name)?,
        ) {
            Ok(()) => reply_ok(None::<u8>, None, in_header.unique, w),
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn do_rename(
        &self,
        in_header: InHeader,
        msg_size: usize,
        newdir: u64,
        flags: u32,
        mut r: Reader,
        w: Writer,
    ) -> Result<usize> {
        let buflen = (in_header.len as usize)
            .checked_sub(size_of::<InHeader>())
            .and_then(|l| l.checked_sub(msg_size))
            .ok_or(Error::InvalidHeaderLength)?;
        let mut buf = vec![0; buflen];

        r.read_exact(&mut buf).map_err(Error::DecodeMessage)?;

        // We want to include the '\0' byte in the first slice.
        let split_pos = buf
            .iter()
            .position(|c| *c == b'\0')
            .map(|p| p + 1)
            .ok_or(Error::MissingParameter)?;

        let (oldname, newname) = buf.split_at(split_pos);

        match self.fs.rename(
            Context::from(in_header),
            in_header.nodeid.into(),
            bytes_to_cstr(oldname)?,
            newdir.into(),
            bytes_to_cstr(newname)?,
            flags,
        ) {
            Ok(()) => reply_ok(None::<u8>, None, in_header.unique, w),
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn rename(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let RenameIn { newdir } = r.read_obj().map_err(Error::DecodeMessage)?;

        self.do_rename(in_header, size_of::<RenameIn>(), newdir, 0, r, w)
    }

    fn rename2(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let Rename2In { newdir, flags, .. } = r.read_obj().map_err(Error::DecodeMessage)?;

        let flags =
            flags & (libc::RENAME_EXCHANGE | libc::RENAME_NOREPLACE | libc::RENAME_WHITEOUT);

        self.do_rename(in_header, size_of::<Rename2In>(), newdir, flags, r, w)
    }

    fn link(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let LinkIn { oldnodeid } = r.read_obj().map_err(Error::DecodeMessage)?;

        let namelen = (in_header.len as usize)
            .checked_sub(size_of::<InHeader>())
            .and_then(|l| l.checked_sub(size_of::<LinkIn>()))
            .ok_or(Error::InvalidHeaderLength)?;
        let mut name = vec![0; namelen];

        r.read_exact(&mut name).map_err(Error::DecodeMessage)?;

        match self.fs.link(
            Context::from(in_header),
            oldnodeid.into(),
            in_header.nodeid.into(),
            bytes_to_cstr(&name)?,
        ) {
            Ok(entry) => {
                let out = EntryOut::from(entry);

                reply_ok(Some(out), None, in_header.unique, w)
            }
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn open(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let OpenIn {
            flags, open_flags, ..
        } = r.read_obj().map_err(Error::DecodeMessage)?;

        let kill_priv = open_flags & OPEN_KILL_SUIDGID != 0;

        match self.fs.open(
            Context::from(in_header),
            in_header.nodeid.into(),
            kill_priv,
            flags,
        ) {
            Ok((handle, opts)) => {
                let out = OpenOut {
                    fh: handle.map(Into::into).unwrap_or(0),
                    open_flags: opts.bits(),
                    ..Default::default()
                };

                reply_ok(Some(out), None, in_header.unique, w)
            }
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn read(
        &self,
        in_header: InHeader,
        mut r: Reader,
        mut w: Writer,
        rl: &mut Option<RateLimiter>,
        rlret: &mut BucketReduction,
        rltyp: &mut TokenType,
    ) -> Result<usize> {
        let ReadIn {
            fh,
            offset,
            size,
            read_flags,
            lock_owner,
            flags,
            ..
        } = r.read_obj().map_err(Error::DecodeMessage)?;

        let _max_size = size_of::<OutHeader>() + MAX_BUFFER_SIZE as usize;
        if let Some(rate_limiter) = rl {
            // If limiter.consume() fails it means there is no more TokenType::Ops
            // budget and rate limiting is in effect.
            *rlret = rate_limiter.consume(1, TokenType::Ops);
            match rlret {
                BucketReduction::Success => {}
                _ => {
                    *rltyp = TokenType::Ops;
                    return Ok(0);
                }
            }

            *rlret = rate_limiter.consume(size as u64, TokenType::Bytes);
            match rlret {
                BucketReduction::Failure => {
                    rate_limiter.manual_replenish(1, TokenType::Ops);
                    *rltyp = TokenType::Bytes;
                    return Ok(0);
                }
                _ => {}
            }
        }

        let owner = if read_flags & READ_LOCKOWNER != 0 {
            Some(lock_owner)
        } else {
            None
        };

        // Split the writer into 2 pieces: one for the `OutHeader` and the rest for the data.
        let data_writer = ZcWriter(w.split_at(size_of::<OutHeader>()).unwrap());

        match self.fs.read(
            Context::from(in_header),
            in_header.nodeid.into(),
            fh.into(),
            data_writer,
            size,
            offset,
            owner,
            flags,
        ) {
            Ok(count) => {
                // Don't use `reply_ok` because we need to set a custom size length for the
                // header.
                let out = OutHeader {
                    len: (size_of::<OutHeader>() + count) as u32,
                    error: 0,
                    unique: in_header.unique,
                };

                debug!("Replying OK, header: {:?}", out);
                w.write_all(out.as_slice()).map_err(Error::EncodeMessage)?;
                Ok(out.len as usize)
            }
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn write(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let WriteIn {
            fh,
            offset,
            size,
            write_flags,
            lock_owner,
            flags,
            ..
        } = r.read_obj().map_err(Error::DecodeMessage)?;

        let owner = if write_flags & WRITE_LOCKOWNER != 0 {
            Some(lock_owner)
        } else {
            None
        };

        let delayed_write = write_flags & WRITE_CACHE != 0;
        let kill_priv = write_flags & WRITE_KILL_PRIV != 0;

        let data_reader = ZcReader(r);

        match self.fs.write(
            Context::from(in_header),
            in_header.nodeid.into(),
            fh.into(),
            data_reader,
            size,
            offset,
            owner,
            delayed_write,
            kill_priv,
            flags,
        ) {
            Ok(count) => {
                let out = WriteOut {
                    size: count as u32,
                    ..Default::default()
                };

                reply_ok(Some(out), None, in_header.unique, w)
            }
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn statfs(&self, in_header: InHeader, w: Writer) -> Result<usize> {
        match self
            .fs
            .statfs(Context::from(in_header), in_header.nodeid.into())
        {
            Ok(st) => reply_ok(Some(Kstatfs::from(st)), None, in_header.unique, w),
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn release(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let ReleaseIn {
            fh,
            flags,
            release_flags,
            lock_owner,
        } = r.read_obj().map_err(Error::DecodeMessage)?;

        let flush = release_flags & RELEASE_FLUSH != 0;
        let flock_release = release_flags & RELEASE_FLOCK_UNLOCK != 0;
        let lock_owner = if flush || flock_release {
            Some(lock_owner)
        } else {
            None
        };

        match self.fs.release(
            Context::from(in_header),
            in_header.nodeid.into(),
            flags,
            fh.into(),
            flush,
            flock_release,
            lock_owner,
        ) {
            Ok(()) => reply_ok(None::<u8>, None, in_header.unique, w),
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn fsync(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let FsyncIn {
            fh, fsync_flags, ..
        } = r.read_obj().map_err(Error::DecodeMessage)?;
        let datasync = fsync_flags & 0x1 != 0;

        match self.fs.fsync(
            Context::from(in_header),
            in_header.nodeid.into(),
            datasync,
            fh.into(),
        ) {
            Ok(()) => reply_ok(None::<u8>, None, in_header.unique, w),
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn setxattr(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let options = FsOptions::from_bits_truncate(self.options.load(Ordering::Relaxed));
        let (
            SetxattrIn {
                size,
                flags,
                setxattr_flags,
                ..
            },
            setxattrin_size,
        ) = if options.contains(FsOptions::SETXATTR_EXT) {
            (
                r.read_obj().map_err(Error::DecodeMessage)?,
                size_of::<SetxattrIn>(),
            )
        } else {
            let SetxattrInCompat { size, flags } = r.read_obj().map_err(Error::DecodeMessage)?;
            (
                SetxattrIn {
                    size,
                    flags,
                    setxattr_flags: 0,
                    padding: 0,
                },
                size_of::<SetxattrInCompat>(),
            )
        };

        // The name and value and encoded one after another and separated by a '\0' character.
        let len = (in_header.len as usize)
            .checked_sub(size_of::<InHeader>())
            .and_then(|l| l.checked_sub(setxattrin_size))
            .ok_or(Error::InvalidHeaderLength)?;
        let mut buf = vec![0; len];

        r.read_exact(&mut buf).map_err(Error::DecodeMessage)?;

        // We want to include the '\0' byte in the first slice.
        let split_pos = buf
            .iter()
            .position(|c| *c == b'\0')
            .map(|p| p + 1)
            .ok_or(Error::MissingParameter)?;

        let (name, value) = buf.split_at(split_pos);

        if size != value.len() as u32 {
            return Err(Error::InvalidXattrSize((size, value.len())));
        }

        match self.fs.setxattr(
            Context::from(in_header),
            in_header.nodeid.into(),
            bytes_to_cstr(name)?,
            value,
            flags,
            SetxattrFlags::from_bits_truncate(setxattr_flags),
        ) {
            Ok(()) => reply_ok(None::<u8>, None, in_header.unique, w),
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn getxattr(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let GetxattrIn { size, .. } = r.read_obj().map_err(Error::DecodeMessage)?;

        let namelen = (in_header.len as usize)
            .checked_sub(size_of::<InHeader>())
            .and_then(|l| l.checked_sub(size_of::<GetxattrIn>()))
            .ok_or(Error::InvalidHeaderLength)?;
        let mut name = vec![0; namelen];

        r.read_exact(&mut name).map_err(Error::DecodeMessage)?;

        if size > MAX_BUFFER_SIZE {
            return reply_error(
                io::Error::from_raw_os_error(libc::ENOMEM),
                in_header.unique,
                w,
            );
        }

        match self.fs.getxattr(
            Context::from(in_header),
            in_header.nodeid.into(),
            bytes_to_cstr(&name)?,
            size,
        ) {
            Ok(GetxattrReply::Value(val)) => reply_ok(None::<u8>, Some(&val), in_header.unique, w),
            Ok(GetxattrReply::Count(count)) => {
                let out = GetxattrOut {
                    size: count,
                    ..Default::default()
                };

                reply_ok(Some(out), None, in_header.unique, w)
            }
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn listxattr(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let GetxattrIn { size, .. } = r.read_obj().map_err(Error::DecodeMessage)?;

        if size > MAX_BUFFER_SIZE {
            return reply_error(
                io::Error::from_raw_os_error(libc::ENOMEM),
                in_header.unique,
                w,
            );
        }

        match self
            .fs
            .listxattr(Context::from(in_header), in_header.nodeid.into(), size)
        {
            Ok(ListxattrReply::Names(val)) => reply_ok(None::<u8>, Some(&val), in_header.unique, w),
            Ok(ListxattrReply::Count(count)) => {
                let out = GetxattrOut {
                    size: count,
                    ..Default::default()
                };

                reply_ok(Some(out), None, in_header.unique, w)
            }
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn removexattr(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let namelen = (in_header.len as usize)
            .checked_sub(size_of::<InHeader>())
            .ok_or(Error::InvalidHeaderLength)?;

        let mut buf = vec![0; namelen];

        r.read_exact(&mut buf).map_err(Error::DecodeMessage)?;

        let name = bytes_to_cstr(&buf)?;

        match self
            .fs
            .removexattr(Context::from(in_header), in_header.nodeid.into(), name)
        {
            Ok(()) => reply_ok(None::<u8>, None, in_header.unique, w),
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn flush(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let FlushIn { fh, lock_owner, .. } = r.read_obj().map_err(Error::DecodeMessage)?;

        match self.fs.flush(
            Context::from(in_header),
            in_header.nodeid.into(),
            fh.into(),
            lock_owner,
        ) {
            Ok(()) => reply_ok(None::<u8>, None, in_header.unique, w),
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn init(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let InitInCompat {
            major,
            minor,
            max_readahead,
            flags,
        } = r.read_obj().map_err(Error::DecodeMessage)?;

        let options = FsOptions::from_bits_truncate(flags as u64);

        let InitInExt { flags2, .. } = if options.contains(FsOptions::INIT_EXT) {
            r.read_obj().map_err(Error::DecodeMessage)?
        } else {
            InitInExt::default()
        };

        if major < KERNEL_VERSION {
            error!("Unsupported fuse protocol version: {}.{}", major, minor);
            return reply_error(
                io::Error::from_raw_os_error(libc::EPROTO),
                in_header.unique,
                w,
            );
        }

        if major > KERNEL_VERSION {
            // Wait for the kernel to reply back with a 7.X version.
            let out = InitOut {
                major: KERNEL_VERSION,
                minor: KERNEL_MINOR_VERSION,
                ..Default::default()
            };

            return reply_ok(Some(out), None, in_header.unique, w);
        }

        if minor < MIN_KERNEL_MINOR_VERSION {
            error!(
                "Unsupported fuse protocol minor version: {}.{}",
                major, minor
            );
            return reply_error(
                io::Error::from_raw_os_error(libc::EPROTO),
                in_header.unique,
                w,
            );
        }

        // These fuse features are supported by this server by default.
        let supported = FsOptions::ASYNC_READ
            | FsOptions::PARALLEL_DIROPS
            | FsOptions::BIG_WRITES
            | FsOptions::AUTO_INVAL_DATA
            | FsOptions::ASYNC_DIO
            | FsOptions::HAS_IOCTL_DIR
            | FsOptions::ATOMIC_O_TRUNC
            | FsOptions::MAX_PAGES
            | FsOptions::SUBMOUNTS
            | FsOptions::INIT_EXT
            | FsOptions::CREATE_SUPP_GROUP;

        let flags_64 = ((flags2 as u64) << 32) | (flags as u64);
        let capable = FsOptions::from_bits_truncate(flags_64);

        let page_size: u32 = unsafe { libc::sysconf(libc::_SC_PAGESIZE).try_into().unwrap() };
        let max_pages = ((MAX_BUFFER_SIZE - 1) / page_size) + 1;

        match self.fs.init(capable) {
            Ok(want) => {
                let enabled = (capable & (want | supported)).bits();
                self.options.store(enabled, Ordering::Relaxed);

                let mut out = InitOut {
                    major: KERNEL_VERSION,
                    minor: KERNEL_MINOR_VERSION,
                    max_readahead,
                    flags: enabled as u32,
                    max_background: u16::MAX,
                    congestion_threshold: (u16::MAX / 4) * 3,
                    max_write: MAX_BUFFER_SIZE,
                    time_gran: 1, // nanoseconds
                    max_pages: max_pages.try_into().unwrap(),
                    map_alignment: 0,
                    flags2: (enabled >> 32) as u32,
                    ..Default::default()
                };

                // Enable DIRECT_IO_ALLOW_MMAP when cache is disabled,
                // send then enabled feature with standard FUSE
                // protocol.
                if want.contains(FsOptions::DIRECT_IO_ALLOW_MMAP) {
                    out.flags2 = (FsOptions::DIRECT_IO_ALLOW_MMAP.bits() >> 32) as u32;
                }

                reply_ok(Some(out), None, in_header.unique, w)
            }
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn opendir(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let OpenIn { flags, .. } = r.read_obj().map_err(Error::DecodeMessage)?;

        match self
            .fs
            .opendir(Context::from(in_header), in_header.nodeid.into(), flags)
        {
            Ok((handle, opts)) => {
                let out = OpenOut {
                    fh: handle.map(Into::into).unwrap_or(0),
                    open_flags: opts.bits(),
                    ..Default::default()
                };

                reply_ok(Some(out), None, in_header.unique, w)
            }
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn mock_readdir(&self, size: u32, offset: u64, cursor: &mut Writer) -> io::Result<usize> {
        let mut total_written = 0;
        let mut err = None;
        let filter = self.root_filter.as_ref().lock().unwrap();
        let root_dirents = filter.get_root_dirents();

        for i in (offset as usize)..root_dirents.len() {
            let dirent2 = &root_dirents[i];
            let dirent2_name = CString::new(dirent2.name.clone().into_bytes()).unwrap();
            let dirent = DirEntry {
                ino: dirent2.ino,
                offset: dirent2.offset,
                type_: dirent2.type_,
                name: dirent2_name.as_c_str(),
            };
            //            info!("dirent: {:?}", dirent);
            let remaining = (size as usize).saturating_sub(total_written);
            match add_dirent(cursor, remaining, dirent, None) {
                // No more space left in the buffer.
                Ok(0) => break,
                Ok(bytes_written) => {
                    total_written += bytes_written;
                }
                Err(e) => {
                    err = Some(e);
                    break;
                }
            }
        }

        if let Some(err) = err {
            Err(err)
        } else {
            Ok(total_written)
        }
    }

    fn readdir(&self, in_header: InHeader, mut r: Reader, mut w: Writer) -> Result<usize> {
        let ReadIn {
            fh, offset, size, ..
        } = r.read_obj().map_err(Error::DecodeMessage)?;

        if size > MAX_BUFFER_SIZE {
            return reply_error(
                io::Error::from_raw_os_error(libc::ENOMEM),
                in_header.unique,
                w,
            );
        }

        let available_bytes = w.available_bytes();
        if available_bytes < size as usize {
            return reply_error(
                io::Error::from_raw_os_error(libc::ENOMEM),
                in_header.unique,
                w,
            );
        }

        // Skip over enough bytes for the header.
        let unique = in_header.unique;
        let mut cursor = w.split_at(size_of::<OutHeader>()).unwrap();

        let result = if in_header.nodeid == ROOT_ID
            && !self.root_filter.as_ref().lock().unwrap().is_empty()
        {
            self.mock_readdir(size, offset, &mut cursor)
        } else {
            match self.fs.readdir(
                Context::from(in_header),
                in_header.nodeid.into(),
                fh.into(),
                size,
                offset,
            ) {
                Ok(mut entries) => {
                    let mut total_written = 0;
                    let mut err = None;
                    while let Some(dirent) = entries.next() {
                        let remaining = (size as usize).saturating_sub(total_written);
                        match add_dirent(&mut cursor, remaining, dirent, None) {
                            // No more space left in the buffer.
                            Ok(0) => break,
                            Ok(bytes_written) => {
                                total_written += bytes_written;
                            }
                            Err(e) => {
                                err = Some(e);
                                break;
                            }
                        }
                    }
                    if let Some(err) = err {
                        Err(err)
                    } else {
                        Ok(total_written)
                    }
                }
                Err(e) => Err(e),
            }
        };

        match result {
            Ok(total_written) => reply_readdir(total_written, unique, w),
            Err(e) => reply_error(e, unique, w),
        }
    }

    fn handle_dirent<'d>(
        &self,
        in_header: &InHeader,
        dir_entry: DirEntry<'d>,
    ) -> io::Result<(DirEntry<'d>, Entry)> {
        let parent = in_header.nodeid.into();
        let name = dir_entry.name.to_bytes();
        let entry = if name == CURRENT_DIR_CSTR || name == PARENT_DIR_CSTR {
            // Don't do lookups on the current directory or the parent directory. Safe because
            // this only contains integer fields and any value is valid.
            let mut attr = unsafe { MaybeUninit::<libc::stat64>::zeroed().assume_init() };
            attr.st_ino = dir_entry.ino;
            attr.st_mode = dir_entry.type_ << 12;

            // We use 0 for the inode value to indicate a negative entry.
            Entry {
                inode: 0,
                generation: 0,
                attr,
                attr_flags: 0,
                attr_timeout: Duration::from_secs(0),
                entry_timeout: Duration::from_secs(0),
            }
        } else {
            self.fs
                .lookup(Context::from(*in_header), parent, dir_entry.name, None)?
        };

        Ok((dir_entry, entry))
    }

    fn readdirplus(&self, in_header: InHeader, mut r: Reader, mut w: Writer) -> Result<usize> {
        let ReadIn {
            fh, offset, size, ..
        } = r.read_obj().map_err(Error::DecodeMessage)?;

        if size > MAX_BUFFER_SIZE {
            return reply_error(
                io::Error::from_raw_os_error(libc::ENOMEM),
                in_header.unique,
                w,
            );
        }

        let available_bytes = w.available_bytes();
        if available_bytes < size as usize {
            return reply_error(
                io::Error::from_raw_os_error(libc::ENOMEM),
                in_header.unique,
                w,
            );
        }

        // Skip over enough bytes for the header.
        let unique = in_header.unique;
        let mut cursor = w.split_at(size_of::<OutHeader>()).unwrap();
        let result = match self.fs.readdir(
            Context::from(in_header),
            in_header.nodeid.into(),
            fh.into(),
            size,
            offset,
        ) {
            Ok(mut entries) => {
                let mut total_written = 0;
                let mut err = None;
                while let Some(dirent) = entries.next() {
                    let mut entry_inode = None;
                    let bytes_written =
                        self.handle_dirent(&in_header, dirent).and_then(|(d, e)| {
                            entry_inode = Some(e.inode);
                            let remaining = (size as usize).saturating_sub(total_written);
                            add_dirent(&mut cursor, remaining, d, Some(e))
                        });
                    match bytes_written {
                        Ok(0) => {
                            // No more space left in the buffer but we need to undo the lookup
                            // that created the Entry or we will end up with mismatched lookup
                            // counts.
                            if let Some(inode) = entry_inode {
                                self.fs.forget(Context::from(in_header), inode.into(), 1);
                            }
                            break;
                        }
                        Ok(bytes_written) => {
                            total_written += bytes_written;
                        }
                        Err(e) => {
                            if let Some(inode) = entry_inode {
                                self.fs.forget(Context::from(in_header), inode.into(), 1);
                            }

                            if total_written == 0 {
                                // We haven't filled any entries yet so we can just propagate
                                // the error.
                                err = Some(e);
                            }

                            // We already filled in some entries. Returning an error now will
                            // cause lookup count mismatches for those entries so just return
                            // whatever we already have.
                            break;
                        }
                    }
                }
                if let Some(err) = err {
                    Err(err)
                } else {
                    Ok(total_written)
                }
            }
            Err(e) => Err(e),
        };

        match result {
            Ok(total_written) => reply_readdir(total_written, unique, w),
            Err(e) => reply_error(e, unique, w),
        }
    }

    fn releasedir(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let ReleaseIn { fh, flags, .. } = r.read_obj().map_err(Error::DecodeMessage)?;

        match self.fs.releasedir(
            Context::from(in_header),
            in_header.nodeid.into(),
            flags,
            fh.into(),
        ) {
            Ok(()) => reply_ok(None::<u8>, None, in_header.unique, w),
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn fsyncdir(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let FsyncIn {
            fh, fsync_flags, ..
        } = r.read_obj().map_err(Error::DecodeMessage)?;
        let datasync = fsync_flags & 0x1 != 0;

        match self.fs.fsyncdir(
            Context::from(in_header),
            in_header.nodeid.into(),
            datasync,
            fh.into(),
        ) {
            Ok(()) => reply_ok(None::<u8>, None, in_header.unique, w),
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn getlk(&self, in_header: InHeader, mut _r: Reader, w: Writer) -> Result<usize> {
        if let Err(e) = self.fs.getlk() {
            reply_error(e, in_header.unique, w)
        } else {
            Ok(0)
        }
    }

    fn setlk(&self, in_header: InHeader, mut _r: Reader, w: Writer) -> Result<usize> {
        if let Err(e) = self.fs.setlk() {
            reply_error(e, in_header.unique, w)
        } else {
            Ok(0)
        }
    }

    fn setlkw(&self, in_header: InHeader, mut _r: Reader, w: Writer) -> Result<usize> {
        if let Err(e) = self.fs.setlkw() {
            reply_error(e, in_header.unique, w)
        } else {
            Ok(0)
        }
    }

    fn access(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let AccessIn { mask, .. } = r.read_obj().map_err(Error::DecodeMessage)?;

        match self
            .fs
            .access(Context::from(in_header), in_header.nodeid.into(), mask)
        {
            Ok(()) => reply_ok(None::<u8>, None, in_header.unique, w),
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn create(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let CreateIn {
            flags,
            mode,
            umask,
            open_flags,
            ..
        } = r.read_obj().map_err(Error::DecodeMessage)?;

        let remaining_len = (in_header.len as usize)
            .checked_sub(size_of::<InHeader>())
            .and_then(|l| l.checked_sub(size_of::<CreateIn>()))
            .ok_or(Error::InvalidHeaderLength)?;

        let mut buf = vec![0; remaining_len];

        r.read_exact(&mut buf).map_err(Error::DecodeMessage)?;
        let mut components = buf.split_inclusive(|c| *c == b'\0');
        let name = components.next().ok_or(Error::MissingParameter)?;

        let options = FsOptions::from_bits_truncate(self.options.load(Ordering::Relaxed));

        let extensions = get_extensions(options, name.len(), buf.as_slice())?;

        let kill_priv = open_flags & OPEN_KILL_SUIDGID != 0;

        match self.fs.create(
            Context::from(in_header),
            in_header.nodeid.into(),
            bytes_to_cstr(name)?,
            mode,
            kill_priv,
            flags,
            umask,
            extensions,
        ) {
            Ok((entry, handle, opts)) => {
                let entry_out = EntryOut {
                    nodeid: entry.inode,
                    generation: entry.generation,
                    entry_valid: entry.entry_timeout.as_secs(),
                    attr_valid: entry.attr_timeout.as_secs(),
                    entry_valid_nsec: entry.entry_timeout.subsec_nanos(),
                    attr_valid_nsec: entry.attr_timeout.subsec_nanos(),
                    attr: Attr::with_flags(entry.attr, entry.attr_flags),
                };
                let open_out = OpenOut {
                    fh: handle.map(Into::into).unwrap_or(0),
                    open_flags: opts.bits(),
                    ..Default::default()
                };

                // Kind of a hack to write both structs.
                reply_ok(
                    Some(entry_out),
                    Some(open_out.as_slice()),
                    in_header.unique,
                    w,
                )
            }
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn interrupt(&self, _in_header: InHeader) -> usize {
        0
    }

    fn bmap(&self, in_header: InHeader, mut _r: Reader, w: Writer) -> Result<usize> {
        if let Err(e) = self.fs.bmap() {
            reply_error(e, in_header.unique, w)
        } else {
            Ok(0)
        }
    }

    fn destroy(&self) -> usize {
        // No reply to this function.
        self.fs.destroy();
        self.options
            .store(FsOptions::empty().bits(), Ordering::Relaxed);

        0
    }

    fn ioctl(&self, in_header: InHeader, _r: Reader, w: Writer) -> Result<usize> {
        if let Err(e) = self.fs.ioctl() {
            reply_error(e, in_header.unique, w)
        } else {
            Ok(0)
        }
    }

    fn poll(&self, in_header: InHeader, mut _r: Reader, w: Writer) -> Result<usize> {
        if let Err(e) = self.fs.poll() {
            reply_error(e, in_header.unique, w)
        } else {
            Ok(0)
        }
    }

    fn notify_reply(&self, in_header: InHeader, mut _r: Reader, w: Writer) -> Result<usize> {
        if let Err(e) = self.fs.notify_reply() {
            reply_error(e, in_header.unique, w)
        } else {
            Ok(0)
        }
    }

    fn batch_forget(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let BatchForgetIn { count, .. } = r.read_obj().map_err(Error::DecodeMessage)?;

        if let Some(size) = (count as usize).checked_mul(size_of::<ForgetOne>()) {
            if size > MAX_BUFFER_SIZE as usize {
                return reply_error(
                    io::Error::from_raw_os_error(libc::ENOMEM),
                    in_header.unique,
                    w,
                );
            }
        } else {
            return reply_error(
                io::Error::from_raw_os_error(libc::EOVERFLOW),
                in_header.unique,
                w,
            );
        }

        let mut requests = Vec::with_capacity(count as usize);
        for _ in 0..count {
            requests.push(
                r.read_obj::<ForgetOne>()
                    .map(|f| (f.nodeid.into(), f.nlookup))
                    .map_err(Error::DecodeMessage)?,
            );
        }

        self.fs.batch_forget(Context::from(in_header), requests);

        // No reply for forget messages.
        Ok(0)
    }

    fn fallocate(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let FallocateIn {
            fh,
            offset,
            length,
            mode,
            ..
        } = r.read_obj().map_err(Error::DecodeMessage)?;

        match self.fs.fallocate(
            Context::from(in_header),
            in_header.nodeid.into(),
            fh.into(),
            mode,
            offset,
            length,
        ) {
            Ok(()) => reply_ok(None::<u8>, None, in_header.unique, w),
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn lseek(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let LseekIn {
            fh, offset, whence, ..
        } = r.read_obj().map_err(Error::DecodeMessage)?;

        match self.fs.lseek(
            Context::from(in_header),
            in_header.nodeid.into(),
            fh.into(),
            offset,
            whence,
        ) {
            Ok(offset) => {
                let out = LseekOut { offset };

                reply_ok(Some(out), None, in_header.unique, w)
            }
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn copyfilerange(&self, in_header: InHeader, mut r: Reader, w: Writer) -> Result<usize> {
        let CopyfilerangeIn {
            fh_in,
            off_in,
            nodeid_out,
            fh_out,
            off_out,
            len,
            flags,
            ..
        } = r.read_obj().map_err(Error::DecodeMessage)?;

        match self.fs.copyfilerange(
            Context::from(in_header),
            in_header.nodeid.into(),
            fh_in.into(),
            off_in,
            nodeid_out.into(),
            fh_out.into(),
            off_out,
            len,
            flags,
        ) {
            Ok(count) => {
                let out = WriteOut {
                    size: count as u32,
                    ..Default::default()
                };

                reply_ok(Some(out), None, in_header.unique, w)
            }
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn syncfs(&self, in_header: InHeader, w: Writer) -> Result<usize> {
        match self
            .fs
            .syncfs(Context::from(in_header), in_header.nodeid.into())
        {
            Ok(()) => reply_ok(None::<u8>, None, in_header.unique, w),
            Err(e) => reply_error(e, in_header.unique, w),
        }
    }

    fn tmpfile(&self, in_header: InHeader, _r: Reader, w: Writer) -> Result<usize> {
        let e = self
            .fs
            .tmpfile()
            .err()
            .unwrap_or_else(|| panic!("unsupported operation"));
        reply_error(e, in_header.unique, w)
    }
}

impl<F: FileSystem + SerializableFileSystem + Sync> SerializableFileSystem for Server<F> {
    fn prepare_serialization(&self, cancel: Arc<AtomicBool>) -> io::Result<()> {
        self.fs.prepare_serialization(cancel)
    }

    fn serialize(&self, state_pipe: File) -> io::Result<()> {
        self.fs.serialize(state_pipe)
    }

    fn deserialize_and_apply(&self, state_pipe: File) -> io::Result<()> {
        self.fs.deserialize_and_apply(state_pipe)
    }

    fn serialize_data(&self) -> io::Result<Vec<u8>> {
        self.fs.serialize_data()
    }

    fn deserialize_and_apply_data(&self, serialized: &Vec<u8>) -> io::Result<()> {
        self.fs.deserialize_and_apply_data(serialized)
    }
}

fn reply_readdir(len: usize, unique: u64, mut w: Writer) -> Result<usize> {
    let out = OutHeader {
        len: (size_of::<OutHeader>() + len) as u32,
        error: 0,
        unique,
    };

    debug!("Replying OK, header: {:?}", out);
    w.write_all(out.as_slice()).map_err(Error::EncodeMessage)?;
    w.flush().map_err(Error::FlushMessage)?;
    Ok(out.len as usize)
}

fn reply_ok<T: ByteValued>(
    out: Option<T>,
    data: Option<&[u8]>,
    unique: u64,
    mut w: Writer,
) -> Result<usize> {
    let mut len = size_of::<OutHeader>();

    if out.is_some() {
        len += size_of::<T>();
    }

    if let Some(data) = data {
        len += data.len();
    }

    let header = OutHeader {
        len: len as u32,
        error: 0,
        unique,
    };

    debug!("Replying OK, header: {:?}", header);

    w.write_all(header.as_slice())
        .map_err(Error::EncodeMessage)?;

    if let Some(out) = out {
        w.write_all(out.as_slice()).map_err(Error::EncodeMessage)?;
    }

    if let Some(data) = data {
        w.write_all(data).map_err(Error::EncodeMessage)?;
    }

    debug_assert_eq!(len, w.bytes_written());
    Ok(w.bytes_written())
}

fn strerror(error: i32) -> String {
    let mut err_desc: Vec<u8> = vec![0; 256];
    let buf_ptr = err_desc.as_mut_ptr() as *mut libc::c_char;

    // Safe because libc::strerror_r writes in err_desc at most err_desc.len() bytes
    unsafe {
        // We ignore the returned value since the two possible error values are:
        // EINVAL and ERANGE, in the former err_desc will be "Unknown error #"
        // and in the latter the message will be truncated to fit err_desc
        libc::strerror_r(error, buf_ptr, err_desc.len());
    }
    let err_desc = err_desc.split(|c| *c == b'\0').next().unwrap();
    String::from_utf8(err_desc.to_vec()).unwrap_or_else(|_| "".to_owned())
}

fn reply_error(e: io::Error, unique: u64, mut w: Writer) -> Result<usize> {
    let header = OutHeader {
        len: size_of::<OutHeader>() as u32,
        error: -e.raw_os_error().unwrap_or(libc::EIO),
        unique,
    };

    debug!(
        "Replying ERROR, header: OutHeader {{ error: {} ({}), unique: {}, len: {} }}",
        header.error,
        strerror(-header.error),
        header.unique,
        header.len
    );

    w.write_all(header.as_slice())
        .map_err(Error::EncodeMessage)?;

    debug_assert_eq!(header.len as usize, w.bytes_written());
    Ok(w.bytes_written())
}

fn bytes_to_cstr(buf: &[u8]) -> Result<&CStr> {
    // Convert to a `CStr` first so that we can drop the '\0' byte at the end
    // and make sure there are no interior '\0' bytes.
    CStr::from_bytes_with_nul(buf).map_err(Error::InvalidCString)
}

fn add_dirent(
    cursor: &mut Writer,
    max: usize,
    d: DirEntry,
    entry: Option<Entry>,
) -> io::Result<usize> {
    // Strip the trailing '\0'.
    let name = d.name.to_bytes();
    if name.len() > u32::MAX as usize {
        return Err(io::Error::from_raw_os_error(libc::EOVERFLOW));
    }

    let dirent_len = size_of::<Dirent>()
        .checked_add(name.len())
        .ok_or_else(|| io::Error::from_raw_os_error(libc::EOVERFLOW))?;

    // Directory entries must be padded to 8-byte alignment.  If adding 7 causes
    // an overflow then this dirent cannot be properly padded.
    let padded_dirent_len = dirent_len
        .checked_add(7)
        .map(|l| l & !7)
        .ok_or_else(|| io::Error::from_raw_os_error(libc::EOVERFLOW))?;

    let total_len = if entry.is_some() {
        padded_dirent_len
            .checked_add(size_of::<EntryOut>())
            .ok_or_else(|| io::Error::from_raw_os_error(libc::EOVERFLOW))?
    } else {
        padded_dirent_len
    };

    if max < total_len {
        Ok(0)
    } else {
        if let Some(entry) = entry {
            cursor.write_all(EntryOut::from(entry).as_slice())?;
        }

        let dirent = Dirent {
            ino: d.ino,
            off: d.offset,
            namelen: name.len() as u32,
            type_: d.type_,
        };

        cursor.write_all(dirent.as_slice())?;
        cursor.write_all(name)?;

        // We know that `dirent_len` <= `padded_dirent_len` due to the check above
        // so there's no need for checked arithmetic.
        let padding = padded_dirent_len - dirent_len;
        if padding > 0 {
            cursor.write_all(&DIRENT_PADDING[..padding])?;
        }

        Ok(total_len)
    }
}

fn take_object<T: ByteValued>(data: &[u8]) -> Result<(T, &[u8])> {
    if data.len() < size_of::<T>() {
        return Err(Error::DecodeMessage(einval()));
    }

    let (object_bytes, remaining_bytes) = data.split_at(size_of::<T>());
    // SAFETY: `T` implements `ByteValued` that guarantees that it is safe to instantiate
    // `T` with random data.
    let object: T = unsafe { std::ptr::read_unaligned(object_bytes.as_ptr() as *const T) };
    Ok((object, remaining_bytes))
}

fn parse_security_context(nr_secctx: u32, data: &[u8]) -> Result<Option<SecContext>> {
    // Although the FUSE security context extension allows sending several security contexts,
    // currently the guest kernel only sends one.
    if nr_secctx > 1 {
        return Err(Error::DecodeMessage(einval()));
    } else if nr_secctx == 0 {
        // No security context sent. May be no LSM supports it.
        return Ok(None);
    }

    let (secctx, data) = take_object::<Secctx>(data)?;

    if secctx.size == 0 {
        return Err(Error::DecodeMessage(einval()));
    }

    let mut components = data.split_inclusive(|c| *c == b'\0');
    let secctx_name = components.next().ok_or(Error::MissingParameter)?;
    let (_, data) = data.split_at(secctx_name.len());

    if data.len() < secctx.size as usize {
        return Err(Error::DecodeMessage(einval()));
    }

    // Fuse client aligns the whole security context block to 64 byte
    // boundary. So it is possible that after actual security context
    // of secctx.size, there are some null padding bytes left. If
    // we ever parse more data after secctx, we will have to take those
    // null bytes into account. Total size (including null bytes) is
    // available in SecctxHeader->size.
    let (remaining, _) = data.split_at(secctx.size as usize);

    let fuse_secctx = SecContext {
        name: CString::from_vec_with_nul(secctx_name.to_vec()).map_err(Error::InvalidCString2)?,
        secctx: remaining.to_vec(),
    };

    Ok(Some(fuse_secctx))
}

fn parse_sup_groups(data: &[u8]) -> Result<u32> {
    let (group_header, group_id_bytes) = take_object::<SuppGroups>(data)?;

    // The FUSE extension allows sending several group IDs, but currently the guest
    // kernel only sends one.
    if group_header.nr_groups != 1 {
        return Err(Error::DecodeMessage(einval()));
    }

    let (gid, _) = take_object::<u32>(group_id_bytes)?;
    Ok(gid)
}

fn get_extensions(options: FsOptions, skip: usize, request_bytes: &[u8]) -> Result<Extensions> {
    let mut extensions = Extensions::default();

    if !(options.contains(FsOptions::SECURITY_CTX)
        || options.contains(FsOptions::CREATE_SUPP_GROUP))
    {
        return Ok(extensions);
    }

    // It's not guaranty to receive an extension even if it's supported by the guest kernel
    if request_bytes.len() < skip {
        return Err(Error::DecodeMessage(einval()));
    }

    // We need to track if a SecCtx was received, because it's valid
    // for the guest to send an empty SecCtx (i.e, nr_secctx == 0)
    let mut secctx_received = false;

    let mut buf = &request_bytes[skip..];
    while !buf.is_empty() {
        let (extension_header, remaining_bytes) = take_object::<ExtHeader>(buf)?;

        let extension_size = (extension_header.size as usize)
            .checked_sub(size_of::<ExtHeader>())
            .ok_or(Error::InvalidHeaderLength)?;

        let (current_extension_bytes, next_extension_bytes) =
            remaining_bytes.split_at(extension_size);

        let ext_type = ExtType::try_from(extension_header.ext_type)
            .map_err(|_| Error::DecodeMessage(einval()))?;

        match ext_type {
            ExtType::SecCtx(nr_secctx) => {
                if !options.contains(FsOptions::SECURITY_CTX) || secctx_received {
                    return Err(Error::DecodeMessage(einval()));
                }

                secctx_received = true;
                extensions.secctx = parse_security_context(nr_secctx, current_extension_bytes)?;
                debug!("Extension received: {} SecCtx", nr_secctx);
            }
            ExtType::SupGroups => {
                if !options.contains(FsOptions::CREATE_SUPP_GROUP) || extensions.sup_gid.is_some() {
                    return Err(Error::DecodeMessage(einval()));
                }

                extensions.sup_gid = parse_sup_groups(current_extension_bytes)?.into();
                debug!("Extension received: SupGroups({:?})", extensions.sup_gid);
            }
        }

        // Let's process the next extension
        buf = next_extension_bytes;
    }

    // The SupGroup extension can be missing, since it is only sent if needed.
    // A SecCtx is always sent in create/synlink/mknod/mkdir if supported.
    if options.contains(FsOptions::SECURITY_CTX) && !secctx_received {
        return Err(Error::MissingExtension);
    }

    Ok(extensions)
}

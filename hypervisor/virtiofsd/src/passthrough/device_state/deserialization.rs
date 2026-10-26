// Copyright 2024 Red Hat, Inc. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

/// Deserialization functionality (i.e. what happens in
/// `SerializableFileSystem::deserialize_and_apply()`): Take a plain vector of bytes, deserialize
/// it into our serializable structs ('serialized' module), and then apply the information from
/// there to a `PassthroughFs`, restoring the state from the migration source.
use crate::fuse;
use crate::passthrough::device_state::preserialization::HandleMigrationInfo;
use crate::passthrough::device_state::serialized;
use crate::passthrough::file_handle::SerializableFileHandle;
use crate::passthrough::inode_store::{InodeData, InodeIds, StrongInodeReference};
use crate::passthrough::stat::statx;
use crate::passthrough::util::{openat, printable_fd};
use crate::passthrough::{
    FileOrHandle, HandleData, HandleDataFile, MigrationOnError, PassthroughFs,
};
use crate::util::{other_io_error, ErrorContext};
use std::collections::BTreeMap;
use std::convert::{TryFrom, TryInto};
use std::io;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, RwLock};

impl TryFrom<&Vec<u8>> for serialized::PassthroughFs {
    type Error = io::Error;

    /// Root of deserialization: Turn plain bytes into a structured `serialized::PassthroughFs`
    fn try_from(serialized: &Vec<u8>) -> io::Result<Self> {
        postcard::from_bytes(serialized).map_err(other_io_error)
    }
}

impl serialized::PassthroughFsV1 {
    /// Apply the state represented in `self: PassthroughFsV1` to the given actual filesystem state
    /// `fs: &PassthroughFs` (i.e. restore the inode store, open handles, etc.)
    pub(super) fn apply(mut self, fs: &PassthroughFs) -> io::Result<()> {
        debug!("deserialization apply start");

        // Apply options as negotiated with the guest on the source
        self.negotiated_opts.apply(fs)?;

        fs.inodes.clear();

        // Some inodes may depend on other inodes being deserialized before them, so trying to
        // deserialize them without their dependency being fulfilled will return `false` below,
        // asking to be deferred.  Therefore, it may take multiple iterations until we have
        // successfully deserialized all inodes.
        // (However serialized inodes are represented, it must be ensured that no loops occur in
        // such dependencies.)
        while !self.inodes.is_empty() {
            let mut i = 0;
            let mut processed_any = false;
            while i < self.inodes.len() {
                if self.inodes[i].deserialize_with_fs(fs)? {
                    // All good
                    self.inodes.swap_remove(i);
                    processed_any = true;
                } else {
                    // Process this inode later (e.g. needs to resolve a reference to a parent node
                    // that has not yet been deserialized)
                    i += 1;
                }
            }

            if !processed_any {
                return Err(other_io_error(
                    "Unresolved references between serialized inodes",
                ));
            }
        }

        fs.next_inode.store(self.next_inode, Ordering::Relaxed);

        // Reconstruct handles (i.e., open those files)
        *fs.handles.write().unwrap() = BTreeMap::new();
        for handle in self.handles {
            handle.deserialize_with_fs(fs)?;
        }

        fs.next_handle.store(self.next_handle, Ordering::Relaxed);

        debug!("deserialization apply finish");

        Ok(())
    }
}

impl serialized::NegotiatedOpts {
    /// Apply the options negotiated with the guest on the source side to `fs`'s configuration
    fn apply(self, fs: &PassthroughFs) -> io::Result<()> {
        debug!("deserialization apply NegotiatedOpts");

        if !fs.cfg.writeback && self.writeback {
            return Err(other_io_error(
                "Migration source wants writeback enabled, but it is disabled on the destination",
            ));
        }
        // Note the case of `fs.cfg.writeback && !self.writeback`, i.e. the user asked for it to be
        // enabled, but the migration source had it disabled: From a technical perspective, just
        // disabling it here is fine, because that is what happens (and what we want to happen)
        // when the guest does not support the flag (in which case there will already have been a
        // warning on INIT).  However, it is imaginable that the guest supports the flag, but it
        // was user-disabled on the source (and is user-enabled now): We can't distinguish this
        // case from the no-guest-support one, and disabling the flag is still the right thing to
        // do, because we would need to re-negotiate through INIT first before we can enable it.
        // Given that it would be strange for the user to use different configurations for source
        // and destination, do not print a warning either.
        fs.writeback.store(self.writeback, Ordering::Relaxed);

        if !fs.cfg.announce_submounts && self.announce_submounts {
            return Err(other_io_error(
                "Migration source wants announce-submounts enabled, but it is disabled on the \
                 destination",
            ));
        }
        // The comment from writeback applies here, too
        fs.announce_submounts
            .store(self.announce_submounts, Ordering::Relaxed);

        if !fs.cfg.posix_acl && self.posix_acl {
            return Err(other_io_error(
                "Migration source wants posix ACLs enabled, but it is disabled on the destination",
            ));
        }
        // The comment from writeback applies here, too
        fs.posix_acl.store(self.posix_acl, Ordering::Relaxed);

        fs.sup_group_extension
            .store(self.sup_group_extension, Ordering::Relaxed);

        Ok(())
    }
}

impl serialized::Inode {
    /// Deserialize this inode into `fs`'s inode store.  Return `Ok(true)` on success, `Err(_)` on
    /// error, and `Ok(false)` when there is a dependency to another inode that has not yet been
    /// deserialized, so deserialization should be re-attempted later.
    fn deserialize_with_fs(&self, fs: &PassthroughFs) -> io::Result<bool> {
        debug!(
            "deserialization restore Inode, inode:{:?}, location:{:?}",
            self.id, self.location
        );

        match &self.location {
            serialized::InodeLocation::RootNode => {
                if self.id != fuse::ROOT_ID {
                    return Err(other_io_error(format!(
                        "Node with non-root ID ({}) given as root node",
                        self.id
                    )));
                }

                // We open the root node ourselves (from the configuration the user gave us)...
                fs.open_root_node()?;
                // ...and only take the refcount from the source, ignoring filename and parent
                // information.  Note that we must not call `fs.open_root_node()` before we have
                // the correct refcount, or deserializing child nodes (which drops one reference
                // each) would quickly reduce the refcount below 0.
                let root_data = fs.inodes.get(fuse::ROOT_ID).unwrap();
                root_data.refcount.store(self.refcount, Ordering::Relaxed);

                // For the root node, a non-matching file handle is always a hard error.  We cannot
                // deserialize the root node as an invalid node.
                self.check_file_handle(&root_data)?;

                Ok(true)
            }

            serialized::InodeLocation::Path {
                parent,
                filename,
                fullname,
            } => {
                if self.id == fuse::ROOT_ID {
                    return Err(other_io_error(
                        "Refusing to use path given for root node".to_string(),
                    ));
                }

                let parent_ref = match fs.inodes.get(*parent) {
                    None => {
                        // `parent` not found yet, defer deserialization until it is present
                        return Ok(false);
                    }

                    Some(parent_data) => {
                        // Safe because the migration source guarantees that this reference is
                        // included in the parent node's refcount.  Once we have deserialized this
                        // inode, we must drop that reference, and moving it into
                        // `deserialize_path()` will achieve that.
                        unsafe { StrongInodeReference::new_no_increment(parent_data, &fs.inodes) }
                    }
                };

                // Restore-time FilterList is authoritative for filter roots: same
                // guest basename opens at the current allow_dir path. Children keep
                // walking via parent fd and are not remapped here.
                let (filename, is_filter) =
                    if !fullname.is_empty() && std::path::Path::new(fullname).is_absolute() {
                        (fs.remap_filter_fullname(self.id, fullname), true)
                    } else {
                        (filename.as_str(), false)
                    };

                let inode_data = self
                    .deserialize_path(fs, parent_ref, filename)
                    .or_else(|err| self.deserialize_invalid_inode(fs, err))?;

                let inode_data = match self.check_file_handle(&inode_data) {
                    Ok(()) => inode_data,
                    Err(err) => self.deserialize_invalid_inode(fs, err)?,
                };

                fs.inodes.new_inode(inode_data)?;
                if is_filter {
                    debug!(
                        "deserialization filter inode, id:{:?}, name:{:?}, fullname:{:?}",
                        self.id, filename, fullname
                    );
                    // Write the remapped open path so the data plane follows
                    // the restore-time allow_dir, not the blob's stale fullname.
                    fs.filter
                        .write()
                        .unwrap()
                        .insert(self.id, (filename.to_string(), filename.to_string()));
                }

                Ok(true)
            }

            serialized::InodeLocation::Invalid => {
                let err = io::Error::new(
                    io::ErrorKind::NotFound,
                    format!("Migration source has lost inode {}", self.id),
                );
                let inode_data = self.deserialize_invalid_inode(fs, err)?;
                fs.inodes.new_inode(inode_data)?;
                Ok(true)
            }

            serialized::InodeLocation::FullPath { filename } => {
                if self.id == fuse::ROOT_ID {
                    return Err(other_io_error(
                        "Refusing to use path given for root node".to_string(),
                    ));
                }

                let Ok(shared_dir) = fs.inodes.get_strong(fuse::ROOT_ID) else {
                    // No root node?  Defer until we have it.
                    return Ok(false);
                };

                let inode_data = self
                    .deserialize_path(fs, shared_dir, filename)
                    .or_else(|err| self.deserialize_invalid_inode(fs, err))?;

                fs.inodes.new_inode(inode_data)?;
                Ok(true)
            }
        }
    }

    /// Helper function for `deserialize_with_fs()`: Try to locate an inode based on its parent
    /// directory and its filename.
    /// Takes ownership of the `parent` strong reference and drops it.
    /// On success, returns `InodeData` to add to `fs.inodes`.
    fn deserialize_path(
        &self,
        fs: &PassthroughFs,
        parent: StrongInodeReference,
        filename: &str,
    ) -> io::Result<InodeData> {
        debug!(
            "deserialization restore Inode path, inode:{:?}, filename:{:?}",
            self.id, filename
        );

        let parent_fd = parent.get().get_file()?;
        let fd = openat(
            &parent_fd,
            filename,
            libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC,
        )
        .map_err(|err| {
            let pfd = printable_fd(&parent_fd, Some(&fs.proc_self_fd));
            io::Error::new(
                err.kind(),
                format!(
                    "Opening {}{}{}: {}",
                    pfd,
                    if pfd.ends_with('/') { "" } else { "/" },
                    filename,
                    err
                ),
            )
        })?;

        let st = statx(&fd, None)?;
        let handle = fs.get_file_handle_opt(&fd, &st)?;

        let file_or_handle = if let Some(h) = handle.as_ref() {
            FileOrHandle::Handle(fs.make_file_handle_openable(h)?)
        } else {
            FileOrHandle::File(fd)
        };

        Ok(InodeData {
            inode: self.id,
            file_or_handle,
            refcount: AtomicU64::new(self.refcount),
            ids: InodeIds {
                ino: st.st.st_ino,
                dev: st.st.st_dev,
                mnt_id: st.mnt_id,
            },
            mode: st.st.st_mode,
            migration_info: Mutex::new(None),
        })
    }

    /// Helper function for `deserialize_with_fs()`: Handle invalid inodes, i.e. ones that cannot
    /// be located.
    /// Depending on the configuration, they either cause a hard error, or should be added as
    /// explicitly invalid inodes to `fs.inodes` (in which case their `InodeData` is returned).
    fn deserialize_invalid_inode(
        &self,
        fs: &PassthroughFs,
        err: io::Error,
    ) -> io::Result<InodeData> {
        match fs.cfg.migration_on_error {
            MigrationOnError::Abort => Err(err.context(format!("Inode {}", self.id))),
            MigrationOnError::GuestError => {
                warn!("Invalid inode {} indexed: {}", self.id, err);
                Ok(InodeData {
                    inode: self.id,
                    file_or_handle: FileOrHandle::Invalid(Arc::new(err)),
                    refcount: AtomicU64::new(self.refcount),
                    ids: Default::default(),
                    mode: Default::default(),
                    migration_info: Default::default(),
                })
            }
        }
    }

    /// If the source sent us a reference file handle, check it against `inode_data`'s file handle
    fn check_file_handle(&self, inode_data: &InodeData) -> io::Result<()> {
        let Some(ref_fh) = &self.file_handle else {
            return Ok(());
        };

        let is_fh: SerializableFileHandle = (&inode_data.file_or_handle).try_into()?;
        // Disregard the mount ID, this may be a different host, so the mount ID may differ
        is_fh.require_equal_without_mount_id(ref_fh).map_err(|err| {
            other_io_error(format!(
                "Inode {} is not the same inode as in the migration source: {}",
                self.id, err
            ))
        })
    }
}

impl serialized::Handle {
    /// Deserialize this handle into `fs`'s handle map.
    fn deserialize_with_fs(&self, fs: &PassthroughFs) -> io::Result<()> {
        debug!(
            "deserialization restore Handle, inode:{:?}, source:{:?}",
            self.inode, self.source
        );

        let inode = fs
            .inodes
            .get(self.inode)
            .ok_or_else(|| other_io_error(format!("Inode {} not found", self.inode)))?;

        let (file, migration_info) = match self.source {
            serialized::HandleSource::OpenInode { flags } => {
                let handle_data_file = match inode
                    .open_file(flags, &fs.proc_self_fd)
                    .and_then(|f| f.into_file())
                {
                    Ok(f) => HandleDataFile::File(RwLock::new(f)),
                    Err(err) => {
                        let error_msg = if let Ok(path) = inode.get_path(&fs.proc_self_fd) {
                            let p = path.as_c_str().to_string_lossy();
                            format!(
                                "Opening inode {} ({}) as handle {}: {}",
                                self.inode, p, self.id, err
                            )
                        } else {
                            format!(
                                "Opening inode {} as handle {}: {}",
                                self.inode, self.id, err
                            )
                        };
                        let err = io::Error::new(err.kind(), error_msg);
                        match fs.cfg.migration_on_error {
                            MigrationOnError::Abort => return Err(err),
                            MigrationOnError::GuestError => {
                                warn!("Invalid handle {} is open in guest: {}", self.id, err);
                                HandleDataFile::Invalid(Arc::new(err))
                            }
                        }
                    }
                };
                let migration_info = HandleMigrationInfo::OpenInode { flags };
                (handle_data_file, migration_info)
            }
        };

        let handle_data = HandleData {
            inode: self.inode,
            file,
            migration_info,
        };
        fs.handles
            .write()
            .unwrap()
            .insert(self.id, Arc::new(handle_data));
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::filesystem::SerializableFileSystem;
    use crate::fuse;
    use crate::passthrough::device_state::serialized;
    use crate::passthrough::Config;
    use std::collections::HashMap;
    use std::fs;
    use std::path::{Path, PathBuf};
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::time::{SystemTime, UNIX_EPOCH};

    const FILTER_ID: u64 = 2;
    const CHILD_ID: u64 = 3;

    static FIXTURE_SEQ: AtomicU64 = AtomicU64::new(0);

    struct TestDir {
        path: PathBuf,
    }

    impl TestDir {
        fn new(test_name: &str) -> Self {
            let pid = std::process::id();
            let nanos = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .map(|d| d.as_nanos())
                .unwrap_or(0);
            let seq = FIXTURE_SEQ.fetch_add(1, Ordering::Relaxed);
            let base = std::env::temp_dir()
                .join(format!("virtiofsd-remap-{pid}-{nanos}-{seq}-{test_name}"));
            fs::create_dir_all(&base).unwrap();
            TestDir {
                path: fs::canonicalize(&base).unwrap(),
            }
        }

        fn path(&self) -> &Path {
            &self.path
        }

        fn mkdir(&self, rel: &str) -> PathBuf {
            let full = self.path.join(rel);
            fs::create_dir_all(&full).unwrap();
            fs::canonicalize(&full).unwrap()
        }

        fn touch(&self, rel: &str) {
            let full = self.path.join(rel);
            if let Some(parent) = full.parent() {
                fs::create_dir_all(parent).unwrap();
            }
            fs::File::create(&full).unwrap();
        }
    }

    impl Drop for TestDir {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.path);
        }
    }

    fn mk_fs(root: &Path) -> PassthroughFs {
        let mut cfg = Config::default();
        cfg.root_dir = root.to_str().unwrap().to_string();
        cfg.migration_verify_handles = false;
        cfg.migration_on_error = MigrationOnError::Abort;
        PassthroughFs::new(cfg).unwrap()
    }

    fn apply_mnt_tree(fs: &PassthroughFs, mnt_fullname: &str) {
        serialized::PassthroughFsV1 {
            inodes: vec![
                serialized::Inode {
                    id: fuse::ROOT_ID,
                    // +1 for the filter inode's Path.parent strong ref
                    refcount: 2,
                    location: serialized::InodeLocation::RootNode,
                    file_handle: None,
                },
                serialized::Inode {
                    id: FILTER_ID,
                    // +1 for the child inode's Path.parent strong ref
                    refcount: 2,
                    location: serialized::InodeLocation::Path {
                        parent: fuse::ROOT_ID,
                        filename: "mnt".into(),
                        fullname: mnt_fullname.to_string(),
                    },
                    file_handle: None,
                },
                serialized::Inode {
                    id: CHILD_ID,
                    refcount: 1,
                    location: serialized::InodeLocation::Path {
                        parent: FILTER_ID,
                        filename: "file".into(),
                        fullname: String::new(),
                    },
                    file_handle: None,
                },
            ],
            next_inode: 4,
            handles: Vec::new(),
            next_handle: 0,
            negotiated_opts: serialized::NegotiatedOpts {
                writeback: false,
                announce_submounts: false,
                posix_acl: false,
                sup_group_extension: false,
            },
        }
        .apply(fs)
        .expect("apply failed");
    }

    fn host_path(fs: &PassthroughFs, inode: u64) -> PathBuf {
        let data = fs.inodes.get(inode).expect("inode missing");
        let cpath = data.get_path(&fs.proc_self_fd).expect("get_path failed");
        PathBuf::from(cpath.to_str().unwrap())
    }

    fn filter_original_path(fs: &PassthroughFs, inode: u64) -> String {
        fs.filter
            .read()
            .unwrap()
            .get(&inode)
            .map(|(_, original)| original.clone())
            .expect("filter entry missing")
    }

    #[test]
    fn remap_hit_opens_new_tree() {
        let shared = TestDir::new("hit-shared");
        let old = TestDir::new("hit-old");
        let new = TestDir::new("hit-new");
        old.mkdir("mnt");
        old.touch("mnt/file");
        let new_mnt = new.mkdir("mnt");
        new.touch("mnt/file");

        let fs = mk_fs(shared.path());
        let mut remap = HashMap::new();
        remap.insert("mnt".into(), new_mnt.to_str().unwrap().to_string());
        fs.set_filter_path_remap(remap);

        let old_mnt = old.path().join("mnt");
        apply_mnt_tree(&fs, old_mnt.to_str().unwrap());

        assert_eq!(
            filter_original_path(&fs, FILTER_ID),
            new_mnt.to_str().unwrap()
        );
        assert_eq!(host_path(&fs, FILTER_ID), new_mnt);
        assert_eq!(host_path(&fs, CHILD_ID), new_mnt.join("file"));
    }

    #[test]
    fn remap_uses_fullname_basename_after_restore() {
        let shared = TestDir::new("restored-shared");
        let old = TestDir::new("restored-old");
        let new = TestDir::new("restored-new");
        let old_mnt = old.mkdir("mnt");
        let new_mnt = new.mkdir("mnt");

        let fs = mk_fs(shared.path());
        let mut remap = HashMap::new();
        remap.insert("mnt".into(), new_mnt.to_str().unwrap().to_string());
        fs.set_filter_path_remap(remap);

        assert_eq!(
            fs.remap_filter_fullname(FILTER_ID, old_mnt.to_str().unwrap()),
            new_mnt.to_str().unwrap()
        );
    }

    #[test]
    fn remap_miss_keeps_blob_fullname() {
        let shared = TestDir::new("miss-shared");
        let old = TestDir::new("miss-old");
        old.mkdir("mnt");
        old.touch("mnt/file");
        let old_mnt = old.path().join("mnt");

        let fs = mk_fs(shared.path());
        apply_mnt_tree(&fs, old_mnt.to_str().unwrap());

        let old_mnt = fs::canonicalize(&old_mnt).unwrap();
        assert_eq!(
            filter_original_path(&fs, FILTER_ID),
            old_mnt.to_str().unwrap()
        );
        assert_eq!(host_path(&fs, FILTER_ID), old_mnt);
        assert_eq!(host_path(&fs, CHILD_ID), old_mnt.join("file"));
    }

    #[test]
    fn remap_identity_is_noop() {
        let shared = TestDir::new("id-shared");
        let old = TestDir::new("id-old");
        old.mkdir("mnt");
        old.touch("mnt/file");
        let old_mnt = fs::canonicalize(old.path().join("mnt")).unwrap();

        let fs = mk_fs(shared.path());
        let mut remap = HashMap::new();
        remap.insert("mnt".into(), old_mnt.to_str().unwrap().to_string());
        fs.set_filter_path_remap(remap);
        apply_mnt_tree(&fs, old_mnt.to_str().unwrap());

        assert_eq!(
            filter_original_path(&fs, FILTER_ID),
            old_mnt.to_str().unwrap()
        );
        assert_eq!(host_path(&fs, FILTER_ID), old_mnt);
    }

    #[test]
    fn remap_ignores_non_filter_child() {
        let shared = TestDir::new("child-shared");
        shared.touch("file");
        let decoy = TestDir::new("child-decoy");
        decoy.touch("file");

        let fs = mk_fs(shared.path());
        let mut remap = HashMap::new();
        remap.insert(
            "file".into(),
            decoy.path().join("file").to_str().unwrap().to_string(),
        );
        fs.set_filter_path_remap(remap);

        serialized::PassthroughFsV1 {
            inodes: vec![
                serialized::Inode {
                    id: fuse::ROOT_ID,
                    refcount: 2,
                    location: serialized::InodeLocation::RootNode,
                    file_handle: None,
                },
                serialized::Inode {
                    id: CHILD_ID,
                    refcount: 1,
                    location: serialized::InodeLocation::Path {
                        parent: fuse::ROOT_ID,
                        filename: "file".into(),
                        fullname: String::new(),
                    },
                    file_handle: None,
                },
            ],
            next_inode: 4,
            handles: Vec::new(),
            next_handle: 0,
            negotiated_opts: serialized::NegotiatedOpts {
                writeback: false,
                announce_submounts: false,
                posix_acl: false,
                sup_group_extension: false,
            },
        }
        .apply(&fs)
        .unwrap();

        assert_eq!(host_path(&fs, CHILD_ID), shared.path().join("file"));
        assert!(fs.filter.read().unwrap().is_empty());
    }
}

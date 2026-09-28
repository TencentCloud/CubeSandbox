// Copyright 2024 Red Hat, Inc. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

use crate::filesystem::DirectoryIterator;
use crate::fuse;
use crate::passthrough::file_handle::{FileHandle, SerializableFileHandle};
use crate::passthrough::inode_store::{Inode, InodeData, InodeIds, StrongInodeReference};
use crate::passthrough::stat::statx;
use crate::passthrough::{self, FileOrHandle, PassthroughFs};
use crate::read_dir::ReadDir;
use crate::util::other_io_error;
use std::collections::HashMap;
use std::convert::TryInto;
use std::ffi::CStr;
use std::fs::File;
use std::io;
use std::os::unix::io::{AsRawFd, FromRawFd};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

/// Precursor to `serialized::Inode` that is constructed while serialization is being prepared, and
/// will then be transformed into the latter at the time of serialization.  To be stored in the
/// inode store, alongside each inode (i.e. in its `InodeData`).  Constructing this is costly, so
/// should only be done when necessary, i.e. when actually preparing for migration.
pub(in crate::passthrough) struct InodeMigrationInfo {
    /// Location of the inode (how the destination can find it)
    pub(in crate::passthrough) location: InodeLocation,

    /// The inode's file handle.  The destination is not supposed to open this handle, but instead
    /// compare it against the one from the inode it has opened based on `location`.
    pub(in crate::passthrough) file_handle: Option<SerializableFileHandle>,
}

pub(in crate::passthrough) enum InodeLocation {
    /// The root node: No information is stored, the destination is supposed to find this on its
    /// own (as configured by the user)
    RootNode,

    /// Inode is represented by its parent directory and its filename therein, allowing the
    /// destination to `openat(2)` it
    Path {
        parent: StrongInodeReference,
        filename: String,
        fullname: String,
    },
}

/// Precursor to `SerializableHandleRepresentation` that is constructed while serialization is
/// being prepared, and will then be transformed into the latter at the time of serialization.
/// To be stored in the `handles` map, alongside each handle (i.e. in its `HandleData`).
/// Constructing this is cheap, so can be done whenever any handle is created.
pub(in crate::passthrough) enum HandleMigrationInfo {
    /// Handle can be opened by opening its associated inode with the given `open(2)` flags
    OpenInode { flags: i32 },
}

/// Stores state for constructing serializable data for inodes using the `InodeMigrationInfo::Path`
/// variant, in order to prepare for migration.
pub(super) struct PathReconstructor<'a> {
    /// Reference to the filesystem for which to reconstruct inodes' paths.
    fs: &'a PassthroughFs,
    /// Set to true when we are supposed to cancel
    cancel: Arc<AtomicBool>,
}

/// Constructs `InodeMigrationInfo` data for every inode in the inode store.  This may take a long
/// time, and is the core part of our preserialization phase.
/// Different implementations of this trait can create different variants of the
/// `InodeMigrationInfo` enum.
pub(super) trait InodeMigrationInfoConstructor {
    /// Runs the constructor
    fn execute(self) -> io::Result<()>;
}

impl InodeMigrationInfo {
    /// Create the migration info for an inode that is collected during the `prepare_serialization`
    /// phase
    pub(in crate::passthrough) fn new(
        fs_cfg: &passthrough::Config,
        parent_ref: StrongInodeReference,
        filename: &CStr,
        fullname: String,
        file_or_handle: &FileOrHandle,
    ) -> io::Result<Self> {
        let utf8_name = filename.to_str().map_err(|err| {
            other_io_error(format!(
                "Cannot convert filename into UTF-8: {filename:?}: {err}"
            ))
        })?;

        Self::new_with_utf8_name(fs_cfg, parent_ref, utf8_name, fullname, file_or_handle)
    }

    fn new_with_utf8_name(
        fs_cfg: &passthrough::Config,
        parent_ref: StrongInodeReference,
        filename: &str,
        fullname: String,
        file_or_handle: &FileOrHandle,
    ) -> io::Result<Self> {
        let file_handle: Option<SerializableFileHandle> = if fs_cfg.migration_verify_handles {
            Some(file_or_handle.try_into()?)
        } else {
            None
        };

        Ok(InodeMigrationInfo {
            location: InodeLocation::Path {
                parent: parent_ref,
                fullname,
                filename: filename.to_string(),
            },
            file_handle,
        })
    }
}

impl HandleMigrationInfo {
    /// Create the migration info for a handle that will be required when serializing
    pub(in crate::passthrough) fn new(flags: i32) -> Self {
        HandleMigrationInfo::OpenInode {
            // Remove flags that make sense when the file is first opened by the guest, but which
            // we should not set when continuing to use the file after migration because they would
            // e.g. modify the file
            flags: flags & !(libc::O_CREAT | libc::O_EXCL | libc::O_TRUNC),
        }
    }
}

/// The `PathReconstructor` is an `InodeMigrationInfoConstructor` that creates `InodeMigrationInfo`
/// of the `InodeMigrationInfo::Path` variant: It recurses through the filesystem (i.e. the shared
/// directory), matching up all inodes it finds with our inode store, and thus finds the parent
/// directory node and filename for every such inode.
impl<'a> PathReconstructor<'a> {
    pub(super) fn new(fs: &'a PassthroughFs, cancel: Arc<AtomicBool>) -> Self {
        PathReconstructor { fs, cancel }
    }

    /// Recurse from the given directory inode
    fn recurse_from(&self, root_ref: StrongInodeReference) -> io::Result<()> {
        let mut dir_buf = vec![0u8; 1024];

        // We don't actually use recursion (to not exhaust the stack), but keep a list of
        // directories we still need to visit, and pop from it until it is empty and we're done
        let mut remaining_dirs = vec![root_ref];
        while let Some(inode_ref) = remaining_dirs.pop() {
            let dirfd = inode_ref.get().open_file(
                libc::O_RDONLY | libc::O_NOFOLLOW | libc::O_CLOEXEC,
                &self.fs.proc_self_fd,
            )?;

            // Read all directory entries, check them for matches in our inode store, and add any
            // directory to `remaining_dirs`
            loop {
                // Safe because we use nothing but this function on the FD
                let mut entries = unsafe { ReadDir::new_no_seek(&dirfd, dir_buf.as_mut()) }?;
                if entries.remaining() == 0 {
                    break;
                }

                while let Some(entry) = entries.next() {
                    debug!("rescore discover {:?}", entry.name);
                    if self.cancel.load(Ordering::Relaxed) {
                        return Err(other_io_error("Cancelled serialization preparation"));
                    }

                    debug!(
                        "preserialization discover inode, name:{:?} root:{:?}",
                        entry.name,
                        inode_ref.get().inode
                    );

                    if let Some(entry_inode) = self.discover(&inode_ref, &dirfd, entry.name)? {
                        // Add directories to visit to the list
                        remaining_dirs.push(entry_inode);
                    }
                }
            }
        }

        Ok(())
    }

    /// Check the given directory entry (parent + name) for matches in our inode store.  If we find
    /// any corresponding `InodeData` there, its `.migration_info` is set accordingly.
    /// For all directories (and directories only), return a strong reference to an inode in our
    /// store that can be used to recurse further.
    fn discover<F: AsRawFd>(
        &self,
        parent_reference: &StrongInodeReference,
        parent_fd: &F,
        name: &CStr,
    ) -> io::Result<Option<StrongInodeReference>> {
        let utf8_name = name.to_str().map_err(|err| {
            other_io_error(format!(
                "Cannot convert filename into UTF-8: {name:?}: {err}",
            ))
        })?;

        // Ignore these
        if utf8_name == "." || utf8_name == ".." {
            return Ok(None);
        }

        let path_fd = {
            let fd = self
                .fs
                .open_relative_to(parent_fd, name, libc::O_PATH, None)?;
            unsafe { File::from_raw_fd(fd) }
        };
        let stat = statx(&path_fd, None)?;
        let handle = self.fs.get_file_handle_opt(&path_fd, &stat)?;

        let ids = InodeIds {
            ino: stat.st.st_ino,
            dev: stat.st.st_dev,
            mnt_id: stat.mnt_id,
        };

        let is_directory = stat.st.st_mode & libc::S_IFMT == libc::S_IFDIR;

        if let Ok(inode_ref) = self.fs.inodes.claim_inode(handle.as_ref(), &ids) {
            let file_handle = if self.fs.cfg.migration_verify_handles {
                Some(match &handle {
                    Some(h) => h.into(),
                    None => FileHandle::from_fd_fail_hard(&path_fd)?.into(),
                })
            } else {
                None
            };

            let mig_info = InodeMigrationInfo {
                location: InodeLocation::Path {
                    parent: StrongInodeReference::clone(parent_reference),
                    filename: utf8_name.to_string(),
                    fullname: String::new(),
                },
                file_handle,
            };

            *inode_ref.get().migration_info.lock().unwrap() = Some(mig_info);

            return Ok(is_directory.then_some(inode_ref));
        }

        // For readonly shared mount, we are done. Cause any live dentry will hold a reference to
        // it's parent.
        if self.fs.cfg.read_only {
            debug!(
                "preserialization discover child: name:{:?} is_dir:{:?} is done",
                utf8_name, is_directory
            );
            return Ok(None);
        }

        // We did not find a matching entry in our inode store.  In case of non-directories, we are
        // done.
        if !is_directory {
            return Ok(None);
        }

        // However, in case of directories, we must create an entry, so we can return it.
        // (Our inode store may still have matching entries recursively downwards from this
        // directory.  Because every node is serialized referencing its parent, this directory
        // inode may end up being recursively referenced this way, we don't know yet.
        // In case there is no such entry, the refcount will eventually return to 0 before
        // `Self::execute()` returns, dropping it from the inode store again, so it will not
        // actually end up being serialized.)

        let file_or_handle = if let Some(h) = handle.as_ref() {
            debug!("discover new handle for {:?}", utf8_name);
            FileOrHandle::Handle(self.fs.make_file_handle_openable(h)?)
        } else {
            debug!("discover new file for {:?}", utf8_name);
            FileOrHandle::File(path_fd)
        };

        let mig_info = InodeMigrationInfo::new_with_utf8_name(
            &self.fs.cfg,
            StrongInodeReference::clone(parent_reference),
            utf8_name,
            String::new(),
            &file_or_handle,
        )?;

        let new_inode = InodeData {
            inode: self.fs.next_inode.fetch_add(1, Ordering::Relaxed),
            file_or_handle,
            refcount: AtomicU64::new(1),
            ids,
            mode: stat.st.st_mode,
            migration_info: Mutex::new(Some(mig_info)),
        };

        Ok(Some(self.fs.inodes.get_or_insert(new_inode)?))
    }
}

impl InodeMigrationInfoConstructor for PathReconstructor<'_> {
    /// Recurse from the root directory (the shared directory)
    fn execute(self) -> io::Result<()> {
        if let Ok(root) = self.fs.inodes.get_strong(fuse::ROOT_ID) {
            // Root node is special in that the destination gets no information on how to find it,
            // because that is configured by the user
            *root.get().migration_info.lock().unwrap() = Some(InodeMigrationInfo {
                location: InodeLocation::RootNode,
                file_handle: if self.fs.cfg.migration_verify_handles {
                    Some((&root.get().file_or_handle).try_into()?)
                } else {
                    None
                },
            });

            if !self.fs.filter.read().unwrap().is_empty() {
                for (id, (filename, fullname)) in self.fs.filter.read().unwrap().iter() {
                    debug!(
                        "preserialization filter inode, id:{:?}, name:{:?}, fullname:{:?}",
                        id, filename, fullname
                    );
                    if let Ok(inode) = self.fs.inodes.get_strong(*id) {
                        *inode.get().migration_info.lock().unwrap() = Some(InodeMigrationInfo {
                            location: InodeLocation::Path {
                                parent: StrongInodeReference::clone(&root),
                                filename: filename.clone(),
                                fullname: fullname.clone(),
                            },
                            file_handle: if self.fs.cfg.migration_verify_handles {
                                Some((&inode.get().file_or_handle).try_into()?)
                            } else {
                                None
                            },
                        });
                        if inode.get().mode & libc::S_IFMT == libc::S_IFDIR {
                            debug!(
                                "preserialization filter tree, id: {:?}, name: {:?}, fullname: {:?}",
                                id, filename, fullname
                            );
                            self.recurse_from(inode)?;
                        }
                    }
                }
                Ok(())
            } else {
                self.recurse_from(root)
            }
        } else {
            // No root node?  Then the filesystem is not mounted and we do not need to do anything.
            Ok(())
        }
    }
}

/// The `StoreOnlyReconstructor` is an `InodeMigrationInfoConstructor` that creates
/// `InodeMigrationInfo` of the `InodeMigrationInfo::Path` variant *without* recursing through the
/// shared directory on disk.
///
/// Instead, it:
///   1. Iterates the inode store exactly once, calling `readlinkat("/proc/self/fd/<fd>")` for
///      each inode to obtain its absolute path. The cost of this step is not uniform: for inodes
///      whose `file_or_handle` is `FileOrHandle::File`, the readlink is a pure procfs read; for
///      `FileOrHandle::Handle` inodes, `InodeData::get_path()` first calls `Handle::open(O_PATH)`
///      via `open_by_handle_at(2)` to materialize a temporary FD on the underlying filesystem,
///      which does touch the backing FS (this is what kept it cheap relative to a full DFS on
///      slow backends like NFS: bounded by inode-store size, not on-disk tree size).
///   2. Sorts the collected (inode_id, abs_path) list by path-byte-length so that any parent's
///      entry is processed before its children. This relies on the invariant that within a
///      single mount namespace an ancestor's absolute byte-path is strictly shorter than any
///      of its descendants' (no descendant can collapse to a shorter path under normal POSIX
///      semantics). Equal-length entries cannot have an ancestor/descendant relationship, so
///      any tie-breaking order is acceptable.
///   3. Streams through the sorted list once more, building a `path -> inode_id` lookup table
///      incrementally and using it to resolve each inode's parent inode_id by string-splitting
///      its own absolute path. If a child's parent path is not yet in the lookup table (e.g. the
///      parent directory was forgotten by the guest while the child remained open), the parent
///      chain is materialized on demand via `openat(O_PATH | O_NOFOLLOW)` starting from the
///      nearest known anchor (root or filter inode) and walking strictly downwards. Each
///      synthesized parent is registered into the inode store via `inodes.get_or_insert()` with
///      a real `file_or_handle` and `ids`, so it is semantically identical to a parent inode
///      that would have been (re)discovered by `PathReconstructor`. This keeps the cost bounded
///      by the *depth* of the missing parent chain, not the breadth of the shared directory.
///
/// Compared to `PathReconstructor`, the cost goes from O(size of shared directory tree) down to
/// O(size of inode store) + O(depth of any missing parent chains), which is typically orders of
/// magnitude smaller in production.
///
/// Filter inodes are handled with the legacy semantics (their `fullname` is an absolute path that
/// is *not* under the shared directory, so the generic absolute-path-relative-to-shared-dir
/// reconstruction does not apply to them).
pub(super) struct StoreOnlyReconstructor<'a> {
    fs: &'a PassthroughFs,
    cancel: Arc<AtomicBool>,
}

impl<'a> StoreOnlyReconstructor<'a> {
    pub(super) fn new(fs: &'a PassthroughFs, cancel: Arc<AtomicBool>) -> Self {
        StoreOnlyReconstructor { fs, cancel }
    }

    /// Set the migration info for the root node.
    fn set_root_info(&self, root: &StrongInodeReference) -> io::Result<()> {
        *root.get().migration_info.lock().unwrap() = Some(InodeMigrationInfo {
            location: InodeLocation::RootNode,
            file_handle: if self.fs.cfg.migration_verify_handles {
                Some((&root.get().file_or_handle).try_into()?)
            } else {
                None
            },
        });
        Ok(())
    }

    /// Strip a single trailing '/' (if any) from a path, *unless* the path is exactly "/".
    /// This keeps seed keys aligned with the parent slice computed in step 5
    /// (`abs_path[..rposition('/')]`), which never carries a trailing slash. Without this,
    /// `readlink()`-derived paths like ".../sionli-test/" would fail to match per-inode
    /// procfs-derived parent slices like ".../sionli-test".
    fn normalize_path_bytes(mut bytes: Vec<u8>) -> Vec<u8> {
        while bytes.len() > 1 && *bytes.last().unwrap() == b'/' {
            bytes.pop();
        }
        bytes
    }

    /// Build a *migration-local* `filter_inode_id -> resolved_host_path` map.
    ///
    /// For every filter entry (`filename`, `original_path`) we perform a one-off
    /// `readlink(original_path)` into this local map only. We *never* mutate `fs.filter`.
    /// The resolved path is kept as `Vec<u8>` (raw OS bytes) and only used as a lookup
    /// key in `path_to_inode`. Errors (broken symlink, non-symlink directory, target
    /// missing, etc.) fall back to the original path bytes.
    fn build_filter_resolved_map(&self) -> std::collections::HashMap<Inode, Vec<u8>> {
        use std::os::unix::ffi::OsStringExt;

        let mut out: std::collections::HashMap<Inode, Vec<u8>> = std::collections::HashMap::new();
        let filter_map = self.fs.filter.read().unwrap();
        for (id, (_filename, original_path)) in filter_map.iter() {
            match std::fs::read_link(original_path.as_str()) {
                Ok(path_buf) => {
                    let bytes = path_buf.into_os_string().into_vec();
                    debug!(
                        "store_only preserialization: locally resolved filter symlink, \
                         inode:{:?}, original:{:?}, resolved_bytes_len:{}",
                        id,
                        original_path,
                        bytes.len()
                    );
                    out.insert(*id, bytes);
                }
                Err(err) => {
                    // Match the legacy fallback: if readlink fails (or the entry isn't
                    // actually a symlink), use the original path as-is. The seed pass below
                    // is tolerant of this not matching any inode's procfs path -- it only
                    // means descendants of this filter can't be reverse-resolved, which is
                    // the same outcome as before.
                    debug!(
                        "store_only preserialization: readlink on filter entry failed, \
                         inode:{:?}, original:{:?}, err:{}; falling back to original path",
                        id, original_path, err
                    );
                    out.insert(*id, original_path.as_bytes().to_vec());
                }
            }
        }
        out
    }

    /// On-demand allocate inode store entries for every directory component along
    /// `child_parent_path` that is not yet known in `path_to_inode`, by walking down from the
    /// closest known ancestor with `openat(O_PATH | O_NOFOLLOW)` and registering each
    /// intermediate directory with full `InodeData` + `migration_info`.
    ///
    /// This mirrors what the legacy `PathReconstructor::discover()` does when it walks into a
    /// directory that is not yet in the inode store: it creates a real entry (with a real fd
    /// or file handle) and recurses. The difference is that here we drive the creation
    /// *bottom-up* from a child that already has a known absolute path, instead of recursing
    /// top-down through every directory on disk.
    ///
    /// Returns `Ok(true)` if the chain was fully materialized (the caller's parent is now in
    /// `path_to_inode`); returns `Ok(false)` if no known ancestor was found (cannot happen in
    /// practice as the shared-dir root is always seeded, but we tolerate it defensively); and
    /// returns `Err` on hard IO failures only when `migration_verify_handles` is enabled
    /// (otherwise hard failures degrade gracefully into "parent not found" + skip, matching
    /// the legacy reconstructor's behaviour on transient errors).
    ///
    /// The byte slice `child_parent_path` must be the absolute path of the parent we need to
    /// materialize (i.e. the result of stripping the trailing `/<filename>` off the child's
    /// own absolute path). It must already be normalized via `normalize_path_bytes` (no
    /// trailing slash unless it is exactly `/`).
    ///
    /// `holding_pen` is a caller-owned vector into which this function pushes a strong
    /// reference for every newly synthesized intermediate inode. The caller MUST keep this
    /// vector alive for the entire main reverse-lookup loop in `execute()`. Without this
    /// scaffolding, a freshly-inserted intermediate inode (returned by `claim_inode` or
    /// `get_or_insert` with `refcount = 1`) would be dropped at the end of this function and
    /// the inode store would forget it before the main loop has a chance to take its own
    /// strong reference via `inodes.get_strong(parent_id)`. The destination would then lose
    /// the descendant inode entirely. Once the main loop's `get_strong` succeeds and stores
    /// the parent reference inside the descendant's `migration_info.location.parent`, the
    /// holding-pen reference becomes redundant; dropping the vector after the main loop lets
    /// any intermediate that ended up with no descendants fall back to refcount 0 and be
    /// evicted, matching the legacy `PathReconstructor`'s contract.
    fn ensure_ancestor_chain(
        &self,
        child_parent_path: &[u8],
        path_to_inode: &mut HashMap<Vec<u8>, Inode>,
        holding_pen: &mut Vec<StrongInodeReference>,
    ) -> io::Result<bool> {
        // Find the longest prefix of `child_parent_path` that already exists in
        // `path_to_inode`. We scan from the longest possible prefix (the full path itself,
        // which by definition is *not* there -- that is why we were called) downwards by
        // truncating at successive '/' boundaries.
        //
        // We collect the path components we need to traverse on the way down into
        // `missing_components`, ordered from closest-to-anchor first to deepest last.
        let mut missing_components: Vec<&[u8]> = Vec::new();
        let mut anchor_path: Vec<u8> = Vec::new();
        let mut anchor_inode: Option<Inode> = None;

        // Walk from `child_parent_path` upward, splitting at '/' boundaries.
        let mut cursor: &[u8] = child_parent_path;
        loop {
            if let Some(&id) = path_to_inode.get(cursor) {
                anchor_path = cursor.to_vec();
                anchor_inode = Some(id);
                break;
            }
            // Could not find this prefix; descend to its parent by stripping the last
            // component. If we cannot strip further (cursor is "/" or has no '/'), give up.
            match cursor.iter().rposition(|&b| b == b'/') {
                Some(0) => {
                    // The parent of `cursor` is "/" itself. Check if "/" is seeded.
                    if let Some(&id) = path_to_inode.get(&b"/"[..]) {
                        anchor_path = b"/".to_vec();
                        anchor_inode = Some(id);
                        // The missing component is everything after the leading '/'.
                        missing_components.push(&cursor[1..]);
                    }
                    break;
                }
                Some(p) => {
                    // Component after the '/' is missing; record it and continue upward.
                    missing_components.push(&cursor[p + 1..]);
                    cursor = &cursor[..p];
                }
                None => {
                    // Malformed (no '/' at all and not in the table); cannot anchor.
                    break;
                }
            }
        }

        let anchor_inode = match anchor_inode {
            Some(id) => id,
            None => return Ok(false),
        };

        // Reverse so we walk from anchor downwards.
        missing_components.reverse();

        if missing_components.is_empty() {
            // Already known; nothing to do.
            return Ok(true);
        }

        // Open the anchor's fd once and walk down component-by-component.
        let anchor_data = match self.fs.inodes.get(anchor_inode) {
            Some(d) => d,
            None => {
                debug!(
                    "store_only preserialization: anchor inode {} disappeared from store while \
                     synthesizing parent chain for {:?}; skipping",
                    anchor_inode,
                    std::str::from_utf8(child_parent_path).unwrap_or("<non-utf8>"),
                );
                return Ok(false);
            }
        };
        // Open the anchor's fd once and walk down component-by-component. We keep
        // `current_fd` as a plain `File` (rather than `InodeFile<'static>`) so we can freely
        // move it across iterations and `try_clone()` it when we need a copy for the new
        // inode's `FileOrHandle::File` variant.
        let mut current_fd: File = anchor_data
            .open_file(
                libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC,
                &self.fs.proc_self_fd,
            )?
            .into_file()?;
        let mut current_path = anchor_path;
        let mut current_inode = anchor_inode;

        for component in missing_components {
            if self.cancel.load(Ordering::Relaxed) {
                return Err(other_io_error("Cancelled serialization preparation"));
            }

            // Build the path of the next directory we are about to materialize.
            let mut next_path = current_path.clone();
            if next_path.last() != Some(&b'/') {
                next_path.push(b'/');
            }
            next_path.extend_from_slice(component);
            let next_path = Self::normalize_path_bytes(next_path);

            // Idempotency: if another `ensure_ancestor_chain` call (or the main loop) has
            // already populated this exact path, just rebase onto it and continue.
            if let Some(&existing_id) = path_to_inode.get(&next_path) {
                let existing_data = match self.fs.inodes.get(existing_id) {
                    Some(d) => d,
                    None => return Ok(false),
                };
                current_fd = existing_data
                    .open_file(
                        libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC,
                        &self.fs.proc_self_fd,
                    )?
                    .into_file()?;
                current_path = next_path;
                current_inode = existing_id;
                continue;
            }

            // `open_relative_to` expects a `CStr`. The component cannot contain '/' (we split
            // at '/' boundaries) and cannot contain NUL (paths from `readlink()` on /proc/self/fd
            // never contain NUL since NUL is not allowed in filenames). Map non-UTF-8 conformantly
            // to the legacy reconstructor's behaviour: a hard error under verify_handles, a
            // degrade-to-skip otherwise.
            let component_cstr = match std::ffi::CString::new(component.to_vec()) {
                Ok(c) => c,
                Err(err) => {
                    if self.fs.cfg.migration_verify_handles {
                        return Err(other_io_error(format!(
                            "store_only preserialization: NUL in path component {:?}: {}",
                            std::str::from_utf8(component).unwrap_or("<non-utf8>"),
                            err
                        )));
                    }
                    warn!(
                        "store_only preserialization: NUL in path component {:?}: {}; cannot \
                         synthesize parent chain, skipping",
                        std::str::from_utf8(component).unwrap_or("<non-utf8>"),
                        err
                    );
                    return Ok(false);
                }
            };

            // Open the next component as an O_PATH fd relative to the current dir fd.
            let raw_fd =
                match self
                    .fs
                    .open_relative_to(&current_fd, &component_cstr, libc::O_PATH, None)
                {
                    Ok(fd) => fd,
                    Err(err) => {
                        // Directory disappeared between snapshot and now, or permission issue.
                        // Under verify_handles, propagate; otherwise degrade.
                        if self.fs.cfg.migration_verify_handles {
                            return Err(other_io_error(format!(
                                "store_only preserialization: failed to open {:?} relative to \
                                 inode {}: {}",
                                std::str::from_utf8(component).unwrap_or("<non-utf8>"),
                                current_inode,
                                err
                            )));
                        }
                        warn!(
                            "store_only preserialization: failed to open {:?} relative to inode \
                             {}: {}; skipping descendants under this path",
                            std::str::from_utf8(component).unwrap_or("<non-utf8>"),
                            current_inode,
                            err
                        );
                        return Ok(false);
                    }
                };
            let new_dir_fd = unsafe { File::from_raw_fd(raw_fd) };
            let stat = statx(&new_dir_fd, None)?;

            // Only directories are valid intermediate components on the path to a child. Any
            // non-directory in the middle would mean the path itself is malformed (e.g. the
            // child went through a symlink we did not resolve). Refuse to register
            // non-directories here -- the caller will then mark the child as Invalid.
            if stat.st.st_mode & libc::S_IFMT != libc::S_IFDIR {
                debug!(
                    "store_only preserialization: intermediate component {:?} of synthesized \
                     parent chain is not a directory (mode={:o}); aborting chain",
                    std::str::from_utf8(component).unwrap_or("<non-utf8>"),
                    stat.st.st_mode
                );
                return Ok(false);
            }

            let handle = self.fs.get_file_handle_opt(&new_dir_fd, &stat)?;
            let ids = InodeIds {
                ino: stat.st.st_ino,
                dev: stat.st.st_dev,
                mnt_id: stat.mnt_id,
            };

            // Build the inode's migration_info pointing to the *parent* we just came from.
            let parent_ref = self.fs.inodes.get_strong(current_inode)?;
            let utf8_component = match std::str::from_utf8(component) {
                Ok(s) => s,
                Err(err) => {
                    if self.fs.cfg.migration_verify_handles {
                        return Err(other_io_error(format!(
                            "store_only preserialization: non-UTF-8 path component: {err}"
                        )));
                    }
                    warn!(
                        "store_only preserialization: non-UTF-8 path component: {}; cannot \
                         synthesize parent chain, skipping",
                        err
                    );
                    return Ok(false);
                }
            };

            // First, try to claim an existing inode (with matching handle/ids). This handles
            // the case where the same inode is reachable both through the store (lookup by
            // its own fd) and through this synthesized chain (lookup as a parent of someone
            // else): we must not create a duplicate `InodeData`, or the destination would see
            // two inodes referencing the same on-disk object.
            let new_ref = if let Ok(existing) = self.fs.inodes.claim_inode(handle.as_ref(), &ids) {
                // Already in store. Set migration_info if it does not have one yet.
                {
                    let mut mig_guard = existing.get().migration_info.lock().unwrap();
                    if mig_guard.is_none() {
                        let file_handle = if self.fs.cfg.migration_verify_handles {
                            Some(match &handle {
                                Some(h) => h.into(),
                                None => FileHandle::from_fd_fail_hard(&new_dir_fd)?.into(),
                            })
                        } else {
                            None
                        };
                        *mig_guard = Some(InodeMigrationInfo {
                            location: InodeLocation::Path {
                                parent: parent_ref,
                                filename: utf8_component.to_string(),
                                fullname: String::new(),
                            },
                            file_handle,
                        });
                    }
                }
                existing
            } else {
                // Genuinely new intermediate inode: build a complete `InodeData` and insert.
                let file_or_handle = if let Some(ref h) = handle {
                    FileOrHandle::Handle(self.fs.make_file_handle_openable(h)?)
                } else {
                    FileOrHandle::File(new_dir_fd.try_clone()?)
                };

                let mig_info = InodeMigrationInfo::new_with_utf8_name(
                    &self.fs.cfg,
                    parent_ref,
                    utf8_component,
                    String::new(),
                    &file_or_handle,
                )?;

                let new_inode = InodeData {
                    inode: self.fs.next_inode.fetch_add(1, Ordering::Relaxed),
                    file_or_handle,
                    refcount: AtomicU64::new(1),
                    ids,
                    mode: stat.st.st_mode,
                    migration_info: Mutex::new(Some(mig_info)),
                };

                self.fs.inodes.get_or_insert(new_inode)?
            };

            let new_inode_id = new_ref.get().inode;
            path_to_inode.insert(next_path.clone(), new_inode_id);

            // Move down the chain.
            current_fd = new_dir_fd;
            current_path = next_path;
            current_inode = new_inode_id;
            // Park the strong reference in the caller-owned holding pen so the inode survives
            // until the main reverse-lookup loop has had a chance to take its own strong ref
            // via `get_strong(parent_id)` (and store it in the descendant's
            // `migration_info.location.parent`). See the function-level doc comment for the
            // full rationale; in short, dropping `new_ref` here would let an intermediate
            // inode whose refcount has just been bumped to 1 fall straight back to 0 and be
            // evicted from the store before any descendant can reference it. Intermediate
            // dirs that end up with *no* descendant referencing them will still be dropped
            // when the holding pen is dropped at the end of `execute()`, matching the legacy
            // reconstructor's contract.
            holding_pen.push(new_ref);
        }

        Ok(true)
    }

    /// Set the migration info for every filter inode (mirrors the legacy `PathReconstructor`
    /// filter handling, but without recursing into filter sub-trees -- the per-inode reverse
    /// lookup pass below will populate any descendants that are actually present in the store).
    fn set_filter_infos(
        &self,
        root: &StrongInodeReference,
        filter_inodes: &mut Vec<Inode>,
    ) -> io::Result<()> {
        let filter_map = self.fs.filter.read().unwrap();
        for (id, (filename, fullname)) in filter_map.iter() {
            if let Ok(inode) = self.fs.inodes.get_strong(*id) {
                debug!(
                    "store_only preserialization filter inode, id:{:?}, name:{:?}, fullname:{:?}",
                    id, filename, fullname
                );
                *inode.get().migration_info.lock().unwrap() = Some(InodeMigrationInfo {
                    location: InodeLocation::Path {
                        parent: StrongInodeReference::clone(root),
                        filename: filename.clone(),
                        fullname: fullname.clone(),
                    },
                    file_handle: if self.fs.cfg.migration_verify_handles {
                        Some((&inode.get().file_or_handle).try_into()?)
                    } else {
                        None
                    },
                });
                filter_inodes.push(*id);
            }
        }
        Ok(())
    }
}

impl InodeMigrationInfoConstructor for StoreOnlyReconstructor<'_> {
    fn execute(self) -> io::Result<()> {
        let Ok(root) = self.fs.inodes.get_strong(fuse::ROOT_ID) else {
            // No root node => filesystem not mounted, nothing to do.
            return Ok(());
        };

        // 0. Build a migration-only `filter_inode_id -> resolved_host_path_bytes` map. This
        //    does NOT mutate the runtime `fs.filter` cache, so a cancelled/failed migration
        //    leaves no residue. See `build_filter_resolved_map` doc for the rationale.
        let filter_resolved_map = self.build_filter_resolved_map();

        // 1. Root node
        self.set_root_info(&root)?;

        // 2. Filter nodes (legacy semantics: fullname kept, parent = root).
        let mut filter_inodes: Vec<Inode> = Vec::new();
        self.set_filter_infos(&root, &mut filter_inodes)?;

        // 3. Single-pass collection: snapshot every store inode's absolute path via procfs.
        //    We deliberately keep this pass under one read-lock of the inode store (via
        //    `inodes.map`) so the snapshot is consistent.
        let filter_inode_set: std::collections::HashSet<Inode> =
            filter_inodes.iter().copied().collect();
        let cancel = Arc::clone(&self.cancel);
        let proc_self_fd = &self.fs.proc_self_fd;

        let snapshots: Vec<io::Result<(Inode, Vec<u8>)>> = self.fs.inodes.map(|inode_data| {
            if cancel.load(Ordering::Relaxed) {
                return Err(other_io_error("Cancelled serialization preparation"));
            }
            // Skip nodes we have already populated above (root + filter inodes).
            if inode_data.inode == fuse::ROOT_ID || filter_inode_set.contains(&inode_data.inode) {
                return Ok((inode_data.inode, Vec::new()));
            }
            // Skip invalid inodes (carry-overs from a previous failed in-migration).
            if matches!(inode_data.file_or_handle, FileOrHandle::Invalid(_)) {
                return Ok((inode_data.inode, Vec::new()));
            }
            // For inodes that already have migration_info set (e.g. populated by `do_lookup`
            // while `track_migration_info` was already true), we must still snapshot their
            // absolute path: their descendants in the store need to find them via
            // `path_to_inode` in step 5. We just must not overwrite their migration_info later.
            // The downstream loop checks `migration_info.is_some()` again before mutating it.
            //
            // Resolve the absolute path through /proc/self/fd. The cost of this depends on
            // `file_or_handle`: for `FileOrHandle::File` this is a procfs-only readlink; for
            // `FileOrHandle::Handle`, `InodeData::get_path()` first calls `Handle::open(O_PATH)`
            // via `open_by_handle_at(2)` to materialize a temporary FD, which does touch the
            // backing filesystem. Either way, the cost stays bounded by inode-store size rather
            // than the on-disk tree size.
            match inode_data.get_path(proc_self_fd) {
                Ok(path) => Ok((inode_data.inode, path.into_bytes())),
                Err(err) => {
                    // `get_path_by_fd` returns `"Inode deleted"` when the kernel reports the
                    // procfs link target with a trailing " (deleted)" suffix, i.e. the file
                    // was unlinked on the host while still being open in the guest. Such an
                    // inode cannot be migrated (the destination has nothing to open at the
                    // former path) and will be serialized as `Invalid`, dropping it from the
                    // destination's view. Surface this at warn level so operators can correlate
                    // post-migration "missing inode" reports with on-host unlink activity.
                    let is_deleted = err.to_string().contains("Inode deleted");
                    if is_deleted {
                        warn!(
                            "store_only preserialization: inode {} (st_dev={}, mnt_id={}, \
                             st_ino={}) was unlinked on host while still open; it will be \
                             serialized as Invalid and dropped on the destination",
                            inode_data.inode,
                            inode_data.ids.dev,
                            inode_data.ids.mnt_id,
                            inode_data.ids.ino,
                        );
                    } else {
                        debug!(
                            "store_only preserialization: failed to get path for inode {} \
                             (st_dev={}, mnt_id={}, st_ino={}): {}; skipping (will be marked \
                             Invalid at serialization)",
                            inode_data.inode,
                            inode_data.ids.dev,
                            inode_data.ids.mnt_id,
                            inode_data.ids.ino,
                            err
                        );
                    }
                    Ok((inode_data.inode, Vec::new()))
                }
            }
        });

        // Propagate the (only) hard error: cancellation.
        let mut entries: Vec<(Inode, Vec<u8>)> = Vec::with_capacity(snapshots.len());
        for snap in snapshots {
            let (id, path) = snap?;
            if !path.is_empty() {
                entries.push((id, path));
            }
        }

        // 4. Sort by path byte-length so any parent comes strictly before its children. The
        //    invariant being relied on: in POSIX, every descendant's absolute byte-path is
        //    strictly longer than its ancestor's (a child path is parent + '/' + name, and name
        //    is non-empty). Ties in length therefore cannot represent an ancestor/descendant
        //    relationship, so any order within a tie is fine. If this invariant is ever broken
        //    (e.g. by mount-namespace aliasing surfacing through /proc/self/fd), descendants
        //    may fail to find their parent and fall through to the "parent not found" debug
        //    path below.
        entries.sort_by(|a, b| a.1.len().cmp(&b.1.len()));

        // Pre-seed the lookup table with root and filter inodes so descendants below them can be
        // resolved correctly. Every key we insert here is normalized via `normalize_path_bytes`
        // (i.e. no trailing '/'), because the parent slice computed in step 5 never carries a
        // trailing slash. This is critical for symlinked filters whose `readlink()` target may
        // legitimately end with '/', e.g. ".../sionli-test/".
        let mut path_to_inode: HashMap<Vec<u8>, Inode> = HashMap::with_capacity(entries.len() + 8);
        // Root: its absolute path is the shared_dir path on host.
        if let Ok(root_path) = root.get().get_path(&self.fs.proc_self_fd) {
            path_to_inode.insert(
                Self::normalize_path_bytes(root_path.into_bytes()),
                fuse::ROOT_ID,
            );
        }
        // Filter inodes: their absolute paths on host are typically *outside* the shared_dir
        // (e.g. NFS mount points). Seed them too so any of their store-resident descendants can
        // be reverse-looked up. We prefer the migration-local resolved map (which includes
        // symlink targets we resolved just for this migration without touching `fs.filter`),
        // and additionally fall back to each filter inode's own /proc/self/fd path.
        for (id, resolved_bytes) in filter_resolved_map.iter() {
            let bytes = Self::normalize_path_bytes(resolved_bytes.clone());
            if !bytes.is_empty() {
                path_to_inode.insert(bytes, *id);
            }
            // Also try to seed via the inode's own /proc/self/fd path (covers cases where the
            // resolved path differs from the actual host path, e.g. bind mounts).
            if let Some(inode_data) = self.fs.inodes.get(*id) {
                if let Ok(p) = inode_data.get_path(&self.fs.proc_self_fd) {
                    path_to_inode
                        .entry(Self::normalize_path_bytes(p.into_bytes()))
                        .or_insert(*id);
                }
            }
        }

        // 5. Stream through the sorted list, resolving parent_id from the lookup table.
        //
        // `synthesized_refs` is a holding pen for strong references to inodes that
        // `ensure_ancestor_chain` materializes on demand for missing parents (Blocking 1
        // scenario: a parent directory was forgotten by the guest while a descendant kept a
        // handle alive). Each newly synthesized inode comes back from `claim_inode`/
        // `get_or_insert` with `refcount = 1`, and would otherwise be evicted from the
        // inode store the moment that strong reference is dropped -- before the main loop
        // below can take its own strong reference via `inodes.get_strong(parent_id)` and
        // store it inside the descendant's `migration_info.location.parent`. Keeping these
        // refs alive across the main loop guarantees that `get_strong(parent_id)` always
        // succeeds for synthesized parents. After the main loop, dropping the vector lets
        // intermediates that ended up with no real descendants fall back to refcount 0 and
        // be evicted from the store, matching the legacy `PathReconstructor`'s contract.
        let mut synthesized_refs: Vec<StrongInodeReference> = Vec::new();
        for (inode_id, abs_path) in entries.iter() {
            if self.cancel.load(Ordering::Relaxed) {
                return Err(other_io_error("Cancelled serialization preparation"));
            }

            // Split off the filename and parent path. `abs_path` is a NUL-free byte vec coming
            // from a `CString::into_bytes()`.
            let (parent_bytes, filename_bytes) = match abs_path.iter().rposition(|&b| b == b'/') {
                Some(0) => (&b"/"[..], &abs_path[1..]),
                Some(p) => (&abs_path[..p], &abs_path[p + 1..]),
                None => {
                    debug!(
                        "store_only preserialization: malformed absolute path for inode {}: {:?}; \
                         skipping",
                        inode_id, abs_path
                    );
                    continue;
                }
            };
            if filename_bytes.is_empty() {
                debug!(
                    "store_only preserialization: empty filename for inode {} (path={:?}); \
                     skipping",
                    inode_id, abs_path
                );
                continue;
            }

            // Non-UTF-8 filenames: the legacy `PathReconstructor::discover()` treats this as
            // a hard error (it calls `name.to_str()?` and propagates). We match that semantics
            // here: silently skipping would be a behavioural regression (the inode would be
            // serialized as `Invalid` and the destination would lose it). We always log at
            // warn! level so operators see it; under `migration_verify_handles` we additionally
            // surface it as a hard error to keep parity with the legacy reconstructor's
            // fail-hard guarantees.
            let filename = match std::str::from_utf8(filename_bytes) {
                Ok(s) => s.to_string(),
                Err(err) => {
                    if self.fs.cfg.migration_verify_handles {
                        return Err(other_io_error(format!(
                            "store_only preserialization: non-UTF-8 filename for inode {inode_id}: {err}"
                        )));
                    }
                    warn!(
                        "store_only preserialization: non-UTF-8 filename for inode {}: {}; \
                         skipping (inode will be marked Invalid at serialization)",
                        inode_id, err
                    );
                    continue;
                }
            };

            // Reverse-lookup parent inode_id. If the parent is not yet in the table, try to
            // synthesize the missing ancestor chain on demand: open every intermediate
            // directory between the closest known ancestor and `parent_bytes` with O_PATH,
            // register them as proper inode store entries with `migration_info`, and seed
            // them into `path_to_inode`. This mirrors what the legacy `PathReconstructor`
            // does when it walks into a directory that is not yet in the store -- the
            // difference is that we drive the materialization bottom-up from the child's
            // known absolute path rather than top-down from the shared-dir root, paying
            // O(depth) syscalls per missing chain instead of O(tree size).
            let parent_id = match path_to_inode.get(parent_bytes) {
                Some(id) => *id,
                None => match self.ensure_ancestor_chain(
                    parent_bytes,
                    &mut path_to_inode,
                    &mut synthesized_refs,
                ) {
                    Ok(true) => match path_to_inode.get(parent_bytes) {
                        Some(id) => *id,
                        None => {
                            // `ensure_ancestor_chain` returned `true` but did not actually seed
                            // the path we asked for -- this would be an internal bug.
                            debug!(
                                "store_only preserialization: parent path {:?} still not in \
                                 lookup table after ensure_ancestor_chain succeeded; skipping \
                                 inode {}",
                                std::str::from_utf8(parent_bytes).unwrap_or("<non-utf8>"),
                                inode_id
                            );
                            path_to_inode.insert(abs_path.clone(), *inode_id);
                            continue;
                        }
                    },
                    Ok(false) => {
                        debug!(
                            "store_only preserialization: parent path {:?} not found in lookup \
                             table for inode {} (path={:?}) and could not be synthesized; \
                             skipping (will be marked Invalid at serialization)",
                            std::str::from_utf8(parent_bytes).unwrap_or("<non-utf8>"),
                            inode_id,
                            std::str::from_utf8(abs_path).unwrap_or("<non-utf8>"),
                        );
                        // Still register this inode in the table -- its descendants (if any)
                        // should not be wrongly remapped to a stale ancestor.
                        path_to_inode.insert(abs_path.clone(), *inode_id);
                        continue;
                    }
                    Err(err) => {
                        // Hard error from ensure_ancestor_chain (only possible under
                        // `migration_verify_handles` or on cancellation): propagate.
                        return Err(err);
                    }
                },
            };

            // Take a strong reference to the parent. Failure here is theoretically impossible
            // because the inode store cannot lose entries while guest is paused, but defensively
            // tolerate it.
            let parent_ref = match self.fs.inodes.get_strong(parent_id) {
                Ok(r) => r,
                Err(err) => {
                    debug!(
                        "store_only preserialization: failed to take strong ref to parent inode \
                         {} for child {}: {}; skipping",
                        parent_id, inode_id, err
                    );
                    path_to_inode.insert(abs_path.clone(), *inode_id);
                    continue;
                }
            };

            // Look up the child's InodeData and set its migration_info.
            let inode_data = match self.fs.inodes.get(*inode_id) {
                Some(d) => d,
                None => {
                    debug!(
                        "store_only preserialization: inode {} disappeared from store between \
                         snapshot and processing; skipping",
                        inode_id
                    );
                    continue;
                }
            };

            // When `migration_verify_handles` is enabled, the legacy `PathReconstructor` builds
            // the file handle via `FileHandle::from_fd_fail_hard()` and propagates failure. Match
            // that semantics here: a missing/failing file handle under verify_handles indicates
            // that the destination would not be able to verify this inode, which is exactly the
            // case the operator opted into verifying. Returning a hard error aborts the
            // migration preparation, which is the safe default.
            let file_handle = if self.fs.cfg.migration_verify_handles {
                match (&inode_data.file_or_handle).try_into() {
                    Ok(h) => Some(h),
                    Err(err) => {
                        return Err(other_io_error(format!(
                            "store_only preserialization: failed to build file handle for inode \
                             {inode_id}: {err}"
                        )));
                    }
                }
            } else {
                None
            };

            // Defensive: if migration_info was already populated (e.g. by `do_lookup` after our
            // snapshot pass but before this mutation), do not overwrite it. The snapshot pass
            // intentionally keeps such inodes in `entries` so they can be seeded into
            // `path_to_inode` (done below), but their migration_info must remain authoritative.
            {
                let mut mig_info_guard = inode_data.migration_info.lock().unwrap();
                if mig_info_guard.is_some() {
                    drop(mig_info_guard);
                    path_to_inode.insert(abs_path.clone(), *inode_id);
                    continue;
                }
                *mig_info_guard = Some(InodeMigrationInfo {
                    location: InodeLocation::Path {
                        parent: parent_ref,
                        filename,
                        fullname: String::new(),
                    },
                    file_handle,
                });
            }

            // Insert into table so descendants can resolve us as their parent.
            path_to_inode.insert(abs_path.clone(), *inode_id);
        }

        // Now that the main loop is done, every descendant that genuinely needed one of the
        // synthesized parents has captured its own strong reference inside its
        // `migration_info.location.parent`. Releasing the holding pen here lets unreferenced
        // intermediates fall back to refcount 0 and be evicted from the store, so the
        // destination only sees inodes that are actually reachable from real descendants --
        // matching the legacy `PathReconstructor`'s contract for parent-chain materialization.
        drop(synthesized_refs);

        info!(
            "store_only preserialization finished: processed {} non-root/non-filter inodes",
            entries.len()
        );

        Ok(())
    }
}

#[cfg(test)]
mod tests {
    //! Behavioural tests for `StoreOnlyReconstructor::execute()`.
    //!
    //! These tests drive a real `PassthroughFs` against a private host temporary directory tree
    //! and exercise the full preserialization flow end-to-end. They specifically cover the MR
    //! !23 review integrity-critical scenarios:
    //!
    //! * Basic chain: a multi-level path can be looked up and reconstructed with correct
    //!   filename/parent linkage for every inode.
    //! * Missing-parent recovery (Blocking 1): when a parent directory has been forgotten by
    //!   the guest but a descendant inode is still in the store, `StoreOnlyReconstructor` must
    //!   synthesize the parent chain on demand rather than skipping the descendant.
    //! * Parity with `PathReconstructor`: for the same starting inode-store state, both
    //!   reconstructors must produce equivalent migration info (filename + ancestor path) for
    //!   every guest-looked-up inode.
    //! * Root-only and empty-tree edge cases.
    //!
    //! Test layout:
    //! * Each test creates a unique on-disk fixture under `$TMPDIR/virtiofsd-test-...`.
    //!   `TestDir::drop` recursively removes it.
    //! * Tests are linux-only (the codebase is linux-only) and require a writable temp dir;
    //!   they do not need root.
    //! * The tests run with `migration_verify_handles=false` to remain compatible with file
    //!   systems that do not support `name_to_handle_at()` (e.g. tmpfs).
    //! * `cargo test` runs tests in parallel by default. Each test owns an independent tempdir
    //!   and an independent `PassthroughFs`; no shared state is touched.
    //!
    //! Caveat: the tests link against the production `PassthroughFs` and therefore inherit its
    //! requirement on `/proc/self/fd`. They will fail to construct the fs on platforms where
    //! procfs is unavailable, which is intentional (and matches production behaviour).

    use super::*;
    use crate::passthrough::{Config, PassthroughFs};
    use std::ffi::CString;
    use std::fs;
    use std::path::{Path, PathBuf};
    use std::sync::atomic::{AtomicU64, Ordering as AOrdering};
    use std::sync::Arc;
    use std::time::{SystemTime, UNIX_EPOCH};

    // -- fixture helpers -------------------------------------------------------------------

    /// Globally-unique counter to avoid collisions between tests running in parallel and to
    /// avoid relying on `SystemTime::now()` having sufficient resolution.
    static FIXTURE_SEQ: AtomicU64 = AtomicU64::new(0);

    /// Owns an on-disk temporary directory; recursively removes it on drop.
    struct TestDir {
        path: PathBuf,
    }

    impl TestDir {
        /// Create a fresh empty tempdir scoped to the given test name.
        fn new(test_name: &str) -> Self {
            let pid = std::process::id();
            let nanos = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .map(|d| d.as_nanos())
                .unwrap_or(0);
            let seq = FIXTURE_SEQ.fetch_add(1, AOrdering::Relaxed);
            let base = std::env::temp_dir()
                .join(format!("virtiofsd-test-{pid}-{nanos}-{seq}-{test_name}"));
            fs::create_dir_all(&base).expect("failed to create test tempdir");
            // Canonicalize so that readlink-based path lookups inside the fs (which return
            // host-absolute, symlink-resolved paths) match the path we hand to root_dir.
            let canon = fs::canonicalize(&base).expect("failed to canonicalize tempdir");
            TestDir { path: canon }
        }

        fn path(&self) -> &Path {
            &self.path
        }

        /// `mkdir -p <relative>` inside the tempdir.
        fn mkdir(&self, rel: &str) {
            fs::create_dir_all(self.path.join(rel)).expect("mkdir -p failed");
        }

        /// `touch <relative>` inside the tempdir; parent dirs are created if missing.
        fn touch(&self, rel: &str) {
            let full = self.path.join(rel);
            if let Some(parent) = full.parent() {
                fs::create_dir_all(parent).expect("mkdir for touch failed");
            }
            fs::File::create(&full).expect("touch failed");
        }

        /// Create a symlink at `<rel_link>` pointing to `target` (literal value of the link).
        /// Parent directories of `rel_link` are created if missing. The target is *not*
        /// validated to exist, mirroring `ln -s` behaviour.
        fn symlink(&self, target: &str, rel_link: &str) {
            let full = self.path.join(rel_link);
            if let Some(parent) = full.parent() {
                fs::create_dir_all(parent).expect("mkdir for symlink failed");
            }
            std::os::unix::fs::symlink(target, &full).expect("symlink failed");
        }
    }

    impl Drop for TestDir {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.path);
        }
    }

    /// Construct a `PassthroughFs` rooted at the given path with test-friendly defaults, and
    /// initialize its root inode. Returns the fs along with a never-set cancel flag suitable
    /// for use with both reconstructors.
    fn mk_fs(root: &Path) -> (PassthroughFs, Arc<AtomicBool>) {
        let mut cfg = Config::default();
        cfg.root_dir = root
            .to_str()
            .expect("test tempdir must be UTF-8")
            .to_string();
        // Keep handles disabled so the tests do not depend on the underlying FS supporting
        // name_to_handle_at(); we exercise the FileOrHandle::File branch of get_path().
        cfg.migration_verify_handles = false;
        cfg.migration_dfs_preserialization = false;
        // No filters; tests covering filters seed them explicitly via `fs.filter`.
        let fs = PassthroughFs::new(cfg).expect("PassthroughFs::new failed");
        fs.open_root_node().expect("open_root_node failed");
        (fs, Arc::new(AtomicBool::new(false)))
    }

    /// Look up a chain of names starting from ROOT_ID and return the inode ID of each level
    /// (in order, *not* including ROOT). All path components are resolved via the production
    /// `do_lookup`, so the resulting inodes have real `file_or_handle` and `ids`.
    fn lookup_chain(fs: &PassthroughFs, names: &[&str]) -> Vec<Inode> {
        let mut parent = fuse::ROOT_ID;
        let mut chain = Vec::with_capacity(names.len());
        for name in names {
            let cname = CString::new(*name).expect("name contains NUL");
            let entry = fs
                .do_lookup(parent, &cname, None)
                .unwrap_or_else(|err| panic!("do_lookup({}, {:?}) failed: {}", parent, name, err));
            chain.push(entry.inode);
            parent = entry.inode;
        }
        chain
    }

    /// Helper: clear all migration_info on the fs so a reconstructor can be re-run from a
    /// clean slate. Used by the parity test.
    fn clear_all_migration_info(fs: &PassthroughFs) {
        fs.inodes.clear_migration_info();
    }

    /// Extract `(filename, parent_inode_id)` from an inode's `migration_info`, if it has been
    /// populated with a `Path` location. Returns `None` for unset / `RootNode` / inode not in
    /// store.
    fn path_info(fs: &PassthroughFs, inode: Inode) -> Option<(String, Inode)> {
        let data = fs.inodes.get(inode)?;
        let guard = data.migration_info.lock().unwrap();
        let info = guard.as_ref()?;
        match &info.location {
            InodeLocation::Path {
                filename, parent, ..
            } => {
                // SAFETY: we are not leaking the strong reference; `get_raw` is documented as
                // safe to call while the strong reference exists.
                let parent_id = unsafe { parent.get_raw() };
                Some((filename.clone(), parent_id))
            }
            InodeLocation::RootNode => None,
        }
    }

    /// Returns `true` if the inode's `migration_info` is set to `RootNode`.
    fn is_root_migration_info(fs: &PassthroughFs, inode: Inode) -> bool {
        let Some(data) = fs.inodes.get(inode) else {
            return false;
        };
        let guard = data.migration_info.lock().unwrap();
        matches!(
            guard.as_ref().map(|i| &i.location),
            Some(InodeLocation::RootNode)
        )
    }

    /// Build the full ancestor-filename chain (root-first, including the inode itself) by
    /// walking `migration_info.parent` until a `RootNode` is reached. Returns `None` if any
    /// step is missing migration_info, which signals "lost" in the reconstruction. Note that
    /// the parent inode IDs themselves are *not* part of the chain: only filenames are, so
    /// the chain is invariant under inode-id reallocation between runs (which is essential
    /// for the parity test).
    fn filename_chain_to_root(fs: &PassthroughFs, inode: Inode) -> Option<Vec<String>> {
        let mut out: Vec<String> = Vec::new();
        let mut cur = inode;
        // Safety budget against accidental cycles.
        for _ in 0..256 {
            if cur == fuse::ROOT_ID {
                return Some({
                    out.reverse();
                    out
                });
            }
            let data = fs.inodes.get(cur)?;
            let guard = data.migration_info.lock().unwrap();
            let info = guard.as_ref()?;
            match &info.location {
                InodeLocation::Path {
                    filename, parent, ..
                } => {
                    out.push(filename.clone());
                    cur = unsafe { parent.get_raw() };
                }
                InodeLocation::RootNode => {
                    return Some({
                        out.reverse();
                        out
                    });
                }
            }
        }
        panic!("filename_chain_to_root: walked >256 hops, suspected cycle");
    }

    // -- t1: basic chain ------------------------------------------------------------------

    /// A straight three-level chain `a/b/c.txt` should be fully reconstructed: each non-root
    /// inode gets `InodeLocation::Path` with the correct filename, and walking parent
    /// references upwards yields the original chain.
    #[test]
    fn storeonly_reconstructs_basic_chain() {
        let dir = TestDir::new("basic_chain");
        dir.mkdir("a/b");
        dir.touch("a/b/c.txt");

        let (fs, cancel) = mk_fs(dir.path());
        let chain = lookup_chain(&fs, &["a", "b", "c.txt"]);
        assert_eq!(chain.len(), 3, "expected three inodes in chain");

        StoreOnlyReconstructor::new(&fs, Arc::clone(&cancel))
            .execute()
            .expect("StoreOnlyReconstructor::execute failed");

        // Root must be marked as RootNode.
        assert!(
            is_root_migration_info(&fs, fuse::ROOT_ID),
            "root inode must have RootNode migration_info"
        );

        // Each chain element must have a Path location with the correct filename.
        let (a_name, a_parent) = path_info(&fs, chain[0]).expect("a missing path info");
        assert_eq!(a_name, "a");
        assert_eq!(a_parent, fuse::ROOT_ID);

        let (b_name, b_parent) = path_info(&fs, chain[1]).expect("b missing path info");
        assert_eq!(b_name, "b");
        assert_eq!(b_parent, chain[0]);

        let (c_name, c_parent) = path_info(&fs, chain[2]).expect("c.txt missing path info");
        assert_eq!(c_name, "c.txt");
        assert_eq!(c_parent, chain[1]);

        // And the convenience chain helper agrees end-to-end.
        let names = filename_chain_to_root(&fs, chain[2]).expect("chain reconstruction failed");
        assert_eq!(names, vec!["a", "b", "c.txt"]);
    }

    // -- t2: missing-parent recovery (Blocking 1) ------------------------------------------

    /// Reproduce the central Blocking 1 scenario from MR !23: a child inode survives in the
    /// store while its parent directory has been forgotten by the guest. The old
    /// `StoreOnlyReconstructor` would skip such a child (resulting in `Invalid` at
    /// serialization); the fixed version must synthesize the missing parent chain on demand.
    #[test]
    fn storeonly_recovers_forgotten_parent_directory() {
        let dir = TestDir::new("forgotten_parent");
        dir.mkdir("p");
        dir.touch("p/child.txt");

        let (fs, cancel) = mk_fs(dir.path());
        let chain = lookup_chain(&fs, &["p", "child.txt"]);
        let p_inode = chain[0];
        let child_inode = chain[1];

        // Simulate the guest forgetting `p` while still holding `child.txt`. `do_lookup`
        // incremented `p`'s refcount to 1 (after the implicit +1 from try_lookup), so a
        // single forget brings it to 0 and removes it from the store. `child.txt` does not
        // hold a strong reference to its parent (migration_info is not yet set), so the
        // forget is unobstructed.
        fs.inodes.forget_one(p_inode, 1);
        assert!(
            fs.inodes.get(p_inode).is_none(),
            "parent `p` should have been removed from store after forget"
        );
        assert!(
            fs.inodes.get(child_inode).is_some(),
            "child inode must still be in store"
        );

        StoreOnlyReconstructor::new(&fs, Arc::clone(&cancel))
            .execute()
            .expect("StoreOnlyReconstructor::execute failed on forgotten-parent case");

        // The child must now have valid Path migration_info, and the chain back to root must
        // be exactly ["p", "child.txt"] -- proving that the missing `p` was synthesized.
        let names = filename_chain_to_root(&fs, child_inode).unwrap_or_else(|| {
            panic!(
                "child {} has no migration_info after StoreOnly reconstruction; \
                 Blocking 1 fix did not take effect",
                child_inode
            )
        });
        assert_eq!(
            names,
            vec!["p", "child.txt"],
            "synthesized parent chain has wrong filename(s)"
        );

        // The synthesized parent must have been registered into the inode store with a real
        // InodeData (not just a phantom path table entry), so we can read its file/handle.
        let (child_name, synth_parent_id) =
            path_info(&fs, child_inode).expect("child path_info missing");
        assert_eq!(child_name, "child.txt");
        let synth_parent_data = fs
            .inodes
            .get(synth_parent_id)
            .expect("synthesized parent must be in the inode store with a real InodeData");
        // The synthesized parent's own migration_info must point to root with filename `p`.
        let (synth_name, synth_grandparent) = path_info(&fs, synth_parent_id)
            .expect("synthesized parent has no migration_info -- must point at root");
        assert_eq!(synth_name, "p");
        assert_eq!(synth_grandparent, fuse::ROOT_ID);
        // And its on-disk identity must match the directory we created.
        let stat = crate::passthrough::stat::statx(&synth_parent_data.get_file().unwrap(), None)
            .expect("statx on synthesized parent");
        assert!(
            stat.st.st_mode & libc::S_IFDIR != 0,
            "synthesized parent must be a directory"
        );
    }

    // -- t3: empty / root-only ------------------------------------------------------------

    /// With nothing but the root inode in the store, the reconstructor must run cleanly and
    /// produce `RootNode` migration_info on the root. This pins down the cheapest path
    /// through `execute()`.
    #[test]
    fn storeonly_root_only_is_a_no_op() {
        let dir = TestDir::new("root_only");
        let (fs, cancel) = mk_fs(dir.path());

        StoreOnlyReconstructor::new(&fs, Arc::clone(&cancel))
            .execute()
            .expect("StoreOnlyReconstructor::execute failed on root-only store");

        assert!(is_root_migration_info(&fs, fuse::ROOT_ID));
    }

    // -- t4: parity with PathReconstructor -------------------------------------------------

    /// For the same starting inode-store state, both reconstructors must produce the same
    /// filename-chain back to root for every guest-looked-up inode. This is the
    /// `alexyonghe`-requested parity check.
    ///
    /// We do *not* compare inode IDs directly across runs, because `PathReconstructor` may
    /// auto-create intermediate directory inodes during its DFS (e.g. when their children
    /// are in the store but the directory itself was forgotten), and the new inode IDs are
    /// not stable across runs. Filename chains are inode-id-invariant and are what the
    /// destination actually reconstructs from.
    #[test]
    fn parity_with_path_reconstructor_on_mixed_tree() {
        let dir = TestDir::new("parity");
        dir.mkdir("a/b");
        dir.touch("a/b/leaf.txt");
        dir.mkdir("a/sibling");
        dir.touch("a/sibling/other.txt");
        dir.touch("a/top.txt");

        // Inodes we will assert about (all looked up explicitly so they live in the store
        // independently of the reconstructor's recursion).
        let to_check: &[&[&str]] = &[
            &["a"],
            &["a", "b"],
            &["a", "b", "leaf.txt"],
            &["a", "sibling"],
            &["a", "sibling", "other.txt"],
            &["a", "top.txt"],
        ];

        // ----- run #1: PathReconstructor --------------------------------------------------
        let (fs1, cancel1) = mk_fs(dir.path());
        let chains1: Vec<Inode> = to_check
            .iter()
            .map(|names| *lookup_chain(&fs1, names).last().unwrap())
            .collect();
        PathReconstructor::new(&fs1, Arc::clone(&cancel1))
            .execute()
            .expect("PathReconstructor execute failed");
        let names1: Vec<Vec<String>> = chains1
            .iter()
            .map(|&id| {
                filename_chain_to_root(&fs1, id).unwrap_or_else(|| {
                    panic!("PathReconstructor: inode {} has no migration_info", id)
                })
            })
            .collect();

        // ----- run #2: StoreOnlyReconstructor on a fresh fs -------------------------------
        let (fs2, cancel2) = mk_fs(dir.path());
        let chains2: Vec<Inode> = to_check
            .iter()
            .map(|names| *lookup_chain(&fs2, names).last().unwrap())
            .collect();
        StoreOnlyReconstructor::new(&fs2, Arc::clone(&cancel2))
            .execute()
            .expect("StoreOnlyReconstructor execute failed");
        let names2: Vec<Vec<String>> = chains2
            .iter()
            .map(|&id| {
                filename_chain_to_root(&fs2, id).unwrap_or_else(|| {
                    panic!("StoreOnlyReconstructor: inode {} has no migration_info", id)
                })
            })
            .collect();

        // The filename chains must be identical for every queried inode -- this is the parity
        // contract that the destination relies on for path-based migration to work the same
        // way regardless of which reconstructor produced the data.
        for ((q, p), s) in to_check.iter().zip(names1.iter()).zip(names2.iter()) {
            let expected: Vec<String> = q.iter().map(|s| (*s).to_string()).collect();
            assert_eq!(
                p, &expected,
                "PathReconstructor chain for {q:?} disagrees with the path itself"
            );
            assert_eq!(
                s, &expected,
                "StoreOnlyReconstructor chain for {q:?} disagrees with the path itself"
            );
            assert_eq!(
                p, s,
                "Parity violation: PathReconstructor and StoreOnlyReconstructor produced \
                 different filename chains for {q:?}"
            );
        }

        // And root in both runs must be RootNode.
        assert!(is_root_migration_info(&fs1, fuse::ROOT_ID));
        assert!(is_root_migration_info(&fs2, fuse::ROOT_ID));

        // Defensive: clearing migration_info on the same fs and re-running StoreOnly must
        // produce the same chains again (idempotency-ish under reset). This catches the
        // class of bug where a second run depends on state lingering from the first.
        clear_all_migration_info(&fs2);
        StoreOnlyReconstructor::new(&fs2, Arc::clone(&cancel2))
            .execute()
            .expect("StoreOnlyReconstructor re-execute failed");
        for (q, expected) in to_check.iter().zip(names2.iter()) {
            // Note: chains2 was filled before the clear, so the inode IDs are still valid
            // (clear_migration_info does not remove inodes from the store, only drops the
            // migration_info field).
            let id = *lookup_chain(&fs2, q).last().unwrap();
            let again = filename_chain_to_root(&fs2, id).unwrap_or_else(|| {
                panic!(
                    "StoreOnlyReconstructor re-run: inode {} missing migration_info",
                    id
                )
            });
            assert_eq!(
                &again, expected,
                "Re-run of StoreOnlyReconstructor yielded a different chain for {q:?}"
            );
        }
    }

    /// `StoreOnlyReconstructor` does *not* silently re-resolve symlinks during
    /// preserialization: production `do_lookup` opens entries with
    /// `O_PATH | O_NOFOLLOW`, so a symlink becomes its own inode in the store with an
    /// `O_PATH` fd that *refers to the symlink itself*, not its target. `/proc/self/fd/<fd>`
    /// for such an fd returns the symlink's own path, so the reconstructed migration_info
    /// chain records the symlink filename (`link.txt`), not the target's filename
    /// (`target.txt`).
    ///
    /// This is the property the author claimed in review feedback #1 ("没有实际 inode
    /// 时不会解析"): only inodes that are actually present in the store are walked, and
    /// the procfs path of an `O_PATH | O_NOFOLLOW` symlink fd is the symlink itself.
    #[test]
    fn storeonly_does_not_follow_symlink_file() {
        let dir = TestDir::new("symlink_file");
        dir.mkdir("a");
        dir.touch("a/target.txt");
        // a/link.txt -> target.txt   (relative symlink, sibling of target)
        dir.symlink("target.txt", "a/link.txt");

        let (fs, cancel) = mk_fs(dir.path());

        // Looking up `a/link.txt` returns the symlink inode (because production lookup
        // uses O_NOFOLLOW). Looking up `a/target.txt` returns the target inode. They
        // must be distinct inodes -- this is the contract `StoreOnlyReconstructor`
        // relies on to keep symlink and target separate in the chain.
        let link_inode = *lookup_chain(&fs, &["a", "link.txt"]).last().unwrap();
        let target_inode = *lookup_chain(&fs, &["a", "target.txt"]).last().unwrap();
        assert_ne!(
            link_inode, target_inode,
            "production do_lookup uses O_NOFOLLOW, so the symlink and its target must be \
             two different inodes in the store"
        );

        StoreOnlyReconstructor::new(&fs, Arc::clone(&cancel))
            .execute()
            .expect("StoreOnlyReconstructor execute failed");

        // The symlink's own chain reads the symlink's filename, not the target's.
        let link_chain = filename_chain_to_root(&fs, link_inode)
            .expect("symlink inode must have migration_info");
        assert_eq!(
            link_chain,
            vec!["a".to_string(), "link.txt".to_string()],
            "StoreOnlyReconstructor must record the symlink's own filename, not the \
             target it points at -- /proc/self/fd of an O_PATH|O_NOFOLLOW fd resolves \
             back to the symlink itself"
        );

        // The target's chain is unaffected.
        let target_chain = filename_chain_to_root(&fs, target_inode)
            .expect("target inode must have migration_info");
        assert_eq!(
            target_chain,
            vec!["a".to_string(), "target.txt".to_string()],
        );

        // Parity check: PathReconstructor on a fresh fs over the same fixture yields
        // the same chains for both inodes.
        let (fs_p, cancel_p) = mk_fs(dir.path());
        let link_p = *lookup_chain(&fs_p, &["a", "link.txt"]).last().unwrap();
        let target_p = *lookup_chain(&fs_p, &["a", "target.txt"]).last().unwrap();
        PathReconstructor::new(&fs_p, Arc::clone(&cancel_p))
            .execute()
            .expect("PathReconstructor execute failed");
        let link_chain_p = filename_chain_to_root(&fs_p, link_p)
            .expect("PathReconstructor: symlink inode must have migration_info");
        let target_chain_p = filename_chain_to_root(&fs_p, target_p)
            .expect("PathReconstructor: target inode must have migration_info");
        assert_eq!(
            link_chain_p, link_chain,
            "Parity violation on symlink inode chain"
        );
        assert_eq!(
            target_chain_p, target_chain,
            "Parity violation on target inode chain"
        );
    }

    /// A symlink that points at a directory is an `S_IFLNK` inode in the store with an
    /// `O_PATH | O_NOFOLLOW` fd; it has no children in the inode store (the guest
    /// cannot `lookup` through it because production `do_lookup` would `openat` with
    /// `O_NOFOLLOW` and refuse to traverse). The reconstructor must therefore handle
    /// this isolated symlink inode without crashing and record its own filename in the
    /// migration chain.
    #[test]
    fn storeonly_handles_symlink_to_directory_isolated() {
        let dir = TestDir::new("symlink_dir");
        dir.mkdir("a/realdir");
        dir.touch("a/realdir/leaf.txt");
        // a/symdir -> realdir
        dir.symlink("realdir", "a/symdir");

        let (fs, cancel) = mk_fs(dir.path());
        // Look up the symlink-to-directory itself; production rejects walking *through*
        // it, but the symlink inode itself is a valid lookup target.
        let symdir_inode = *lookup_chain(&fs, &["a", "symdir"]).last().unwrap();
        let real_leaf = *lookup_chain(&fs, &["a", "realdir", "leaf.txt"])
            .last()
            .unwrap();

        StoreOnlyReconstructor::new(&fs, Arc::clone(&cancel))
            .execute()
            .expect("StoreOnlyReconstructor execute failed");

        // Symlink-to-dir gets its own chain ending in `symdir`.
        let symdir_chain = filename_chain_to_root(&fs, symdir_inode)
            .expect("symdir inode must have migration_info");
        assert_eq!(
            symdir_chain,
            vec!["a".to_string(), "symdir".to_string()],
            "Symlink-to-directory must be recorded by its own filename"
        );

        // The real leaf, reached via the real directory, has the real chain.
        let leaf_chain =
            filename_chain_to_root(&fs, real_leaf).expect("leaf inode must have migration_info");
        assert_eq!(
            leaf_chain,
            vec![
                "a".to_string(),
                "realdir".to_string(),
                "leaf.txt".to_string(),
            ],
        );
    }
}

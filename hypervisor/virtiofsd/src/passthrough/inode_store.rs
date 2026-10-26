// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.

use crate::fuse;
use crate::passthrough::device_state::preserialization::{InodeLocation, InodeMigrationInfo};
use crate::passthrough::file_handle::{FileHandle, FileOrHandle};
use crate::passthrough::stat::MountId;
use crate::passthrough::util::{ebadf, get_path_by_fd, is_safe_inode, reopen_fd_through_proc};
use crate::util::other_io_error;
use std::collections::BTreeMap;
use std::ffi::CString;
use std::fs::File;
use std::io;
use std::ops::Deref;
use std::os::unix::io::{AsRawFd, RawFd};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, RwLock};

pub type Inode = u64;

#[derive(Clone, Copy, Default, Eq, Ord, PartialEq, PartialOrd)]
pub struct InodeIds {
    pub ino: libc::ino64_t,
    pub dev: libc::dev_t,
    pub mnt_id: MountId,
}

/// Strong reference to some inode in our inode store, which is counted against the
/// `InodeData.refcount` field.  Dropping this object will thus decrement that refcount, and
/// potentially remove the inode from the store (when the refcount reaches 0).
/// Note that dropping this object locks its inode store, so care must be taken not to drop strong
/// references while the inode store is locked, or to use `StrongInodeReference::drop_unlocked()`.
pub struct StrongInodeReference {
    /// Referenced inode's data.
    /// Is only `None` after the inode has been leaked, which cannot occur outside of `leak()` and
    /// `drop()`, because `leak()` consumes the object.
    inode_data: Option<Arc<InodeData>>,

    /// Inode store that holds the referenced inode.
    inode_store: Arc<RwLock<InodeStoreInner>>,
}

pub struct InodeData {
    pub inode: Inode,
    // Most of these aren't actually files but ¯\_(ツ)_/¯.
    pub file_or_handle: FileOrHandle,
    pub refcount: AtomicU64,

    // Used as key in the `InodeStoreInner::by_ids` map.
    pub ids: InodeIds,

    // File type and mode
    pub mode: u32,

    // Constructed in the `prepare_serialization` phase of migration, and must be set on all inodes
    // when we are actually going to serialize our internal state to send it to the migration
    // destination.
    // Because this may contain a strong inode reference, which must not be dropped while the inode
    // store is locked, this info must in turn not be dropped while the store is locked.
    // To ensure this, locking of the store is only done here in this file, and here we ensure that
    // while the store is locked, `InodeMigrationInfo` (e.g. as part of an `InodeData`) is dropped
    // only by using `drop_unlocked()` for a potentially contained strong reference.
    pub(super) migration_info: Mutex<Option<InodeMigrationInfo>>,
}

/**
 * Represents the file associated with an inode (`InodeData`).
 *
 * When obtaining such a file, it may either be a new file (the `Owned` variant), in which case the
 * object's lifetime is static, or it may reference `InodeData.file` (the `Ref` variant), in which
 * case the object's lifetime is that of the respective `InodeData` object.
 */
pub enum InodeFile<'inode_lifetime> {
    Owned(File),
    Ref(&'inode_lifetime File),
}

#[derive(Default)]
struct InodeStoreInner {
    data: BTreeMap<Inode, Arc<InodeData>>,
    by_ids: BTreeMap<InodeIds, Inode>,
    by_handle: BTreeMap<FileHandle, Inode>,
}

#[derive(Default)]
pub struct InodeStore {
    inner: Arc<RwLock<InodeStoreInner>>,
}

impl<'a> InodeData {
    /// Get an `O_PATH` file for this inode
    pub fn get_file(&'a self) -> io::Result<InodeFile<'a>> {
        match &self.file_or_handle {
            FileOrHandle::File(f) => Ok(InodeFile::Ref(f)),
            FileOrHandle::Handle(h) => {
                let file = h.open(libc::O_PATH)?;
                Ok(InodeFile::Owned(file))
            }
            FileOrHandle::Invalid(err) => Err(io::Error::new(err.kind(), Arc::clone(err))),
        }
    }

    /// Try to obtain this inode's path through /proc/self/fd
    pub fn get_path(&self, proc_self_fd: &File) -> io::Result<CString> {
        let path = get_path_by_fd(&self.get_file()?, proc_self_fd)?;

        // Kernel will report nodes beyond our root as having path / -- but only the root node (the
        // shared directory) can actually have that path, so for others, it must be inaccurate
        if path.as_bytes() == b"/" && self.inode != fuse::ROOT_ID {
            return Err(other_io_error(
                "Got empty path for non-root node, so it is outside the shared directory"
                    .to_string(),
            ));
        }

        Ok(path)
    }

    /// Open this inode with the given flags
    /// (always returns a new (i.e. `Owned`) file, hence the static lifetime)
    pub fn open_file(
        &self,
        flags: libc::c_int,
        proc_self_fd: &File,
    ) -> io::Result<InodeFile<'static>> {
        // Do not move the `is_safe_inode()` check up: It is always false for invalid inodes, so
        // would hide their perfectly good error message
        match &self.file_or_handle {
            FileOrHandle::File(f) => {
                if !is_safe_inode(self.mode) {
                    return Err(ebadf());
                }
                let new_file = reopen_fd_through_proc(f, flags, proc_self_fd)?;
                Ok(InodeFile::Owned(new_file))
            }
            FileOrHandle::Handle(h) => {
                if !is_safe_inode(self.mode) {
                    return Err(ebadf());
                }
                let new_file = h.open(flags)?;
                Ok(InodeFile::Owned(new_file))
            }
            FileOrHandle::Invalid(err) => Err(io::Error::new(err.kind(), Arc::clone(err))),
        }
    }
}

impl InodeFile<'_> {
    /// Create a standalone `File` object
    pub fn into_file(self) -> io::Result<File> {
        match self {
            Self::Owned(file) => Ok(file),
            Self::Ref(file_ref) => file_ref.try_clone(),
        }
    }
}

impl AsRawFd for InodeFile<'_> {
    /// Return a file descriptor for this file
    /// Note: This fd is only valid as long as the `InodeFile` exists.
    fn as_raw_fd(&self) -> RawFd {
        match self {
            Self::Owned(file) => file.as_raw_fd(),
            Self::Ref(file_ref) => file_ref.as_raw_fd(),
        }
    }
}

impl InodeStoreInner {
    /// Insert a new entry into the inode store.  Panics if the entry already existed.
    /// (This guarantees that inserting a value will not drop an existing `InodeMigrationInfo`
    /// object.)
    fn insert_new(&mut self, data: Arc<InodeData>) {
        // Overwriting something in `by_ids` or `by_handle` is not exactly what we want, but having
        // the same physical inode under several different FUSE IDs is not catastrophic, so do not
        // panic about that.
        self.by_ids.insert(data.ids, data.inode);
        if let FileOrHandle::Handle(handle) = &data.file_or_handle {
            self.by_handle.insert(handle.inner().clone(), data.inode);
        }
        let existing = self.data.insert(data.inode, data);
        assert!(existing.is_none());
    }

    /// Remove the given inode, and, if found, take care to drop any associated strong reference in
    /// the migration info via `drop_unlocked()`.
    fn remove(&mut self, inode: Inode) {
        let data = self.data.remove(&inode);
        if let Some(data) = data {
            if let FileOrHandle::Handle(handle) = &data.file_or_handle {
                self.by_handle.remove(handle.inner());
            }
            self.by_ids.remove(&data.ids);
            if let Some(mig_info) = data.migration_info.lock().unwrap().take() {
                match mig_info.location {
                    InodeLocation::RootNode => (),
                    InodeLocation::Path { parent, .. } => parent.drop_unlocked(self),
                }
            }
        }
    }

    fn clear(&mut self) {
        self.clear_migration_info();
        self.data.clear();
        self.by_handle.clear();
        self.by_ids.clear();
    }

    /// Clears all migration info, using `drop_unlocked()` to drop any strong references within.
    fn clear_migration_info(&mut self) {
        let mut strong_references = Vec::<StrongInodeReference>::new();
        for inode in self.data.values() {
            if let Some(mig_info) = inode.migration_info.lock().unwrap().take() {
                match mig_info.location {
                    InodeLocation::RootNode => (),
                    InodeLocation::Path { parent, .. } => strong_references.push(parent),
                }
            }
        }
        for strong_reference in strong_references {
            strong_reference.drop_unlocked(self);
        }
    }

    fn get(&self, inode: Inode) -> Option<&Arc<InodeData>> {
        self.data.get(&inode)
    }

    fn get_by_ids(&self, ids: &InodeIds) -> Option<&Arc<InodeData>> {
        self.inode_by_ids(ids).map(|inode| self.get(inode).unwrap())
    }

    fn get_by_handle(&self, handle: &FileHandle) -> Option<&Arc<InodeData>> {
        self.inode_by_handle(handle)
            .map(|inode| self.get(inode).unwrap())
    }

    fn contains(&self, inode: Inode) -> bool {
        self.data.contains_key(&inode)
    }

    fn inode_by_ids(&self, ids: &InodeIds) -> Option<Inode> {
        self.by_ids.get(ids).copied()
    }

    fn inode_by_handle(&self, handle: &FileHandle) -> Option<Inode> {
        self.by_handle.get(handle).copied()
    }

    fn is_empty(&self) -> bool {
        self.data.is_empty()
    }

    /// Decrement the refcount of the given `inode` ID, and remove it from the store when it
    /// reaches 0
    fn forget_one(&mut self, inode: Inode, count: u64) {
        if let Some(data) = self.get(inode) {
            // Having a mutable reference on `self` prevents concurrent lookups from incrementing
            // the refcount but there is the possibility that a previous lookup already acquired a
            // reference to the inode data and is in the process of updating the refcount so we
            // need to loop here until we can decrement successfully.
            loop {
                let refcount = data.refcount.load(Ordering::Relaxed);

                // Saturating sub because it doesn't make sense for a refcount to go below zero and
                // we don't want misbehaving clients to cause integer overflow.
                let new_count = refcount.saturating_sub(count);

                // We don't need any stronger ordering, because the refcount itself doesn't protect
                // any data.
                if data.refcount.compare_exchange(
                    refcount,
                    new_count,
                    Ordering::Relaxed,
                    Ordering::Relaxed,
                ) == Ok(refcount)
                {
                    if new_count == 0 {
                        // We just removed the last refcount for this inode. There's no need for an
                        // acquire fence here because we have a mutable reference on `self`. So
                        // there's is no other release store for us to synchronize with before
                        // deleting the entry.
                        self.remove(inode);
                    }
                    break;
                }
            }
        }
    }
}

impl InodeStore {
    pub fn get(&self, inode: Inode) -> Option<Arc<InodeData>> {
        self.inner.read().unwrap().get(inode).cloned()
    }

    pub fn get_by_ids(&self, ids: &InodeIds) -> Option<Arc<InodeData>> {
        self.inner.read().unwrap().get_by_ids(ids).cloned()
    }

    pub fn get_by_handle(&self, handle: &FileHandle) -> Option<Arc<InodeData>> {
        self.inner.read().unwrap().get_by_handle(handle).cloned()
    }

    pub fn inode_by_ids(&self, ids: &InodeIds) -> Option<Inode> {
        self.inner.read().unwrap().inode_by_ids(ids)
    }

    pub fn inode_by_handle(&self, handle: &FileHandle) -> Option<Inode> {
        self.inner.read().unwrap().inode_by_handle(handle)
    }

    /// Invoke `func()` on each inode, collect all results, and return them.  Note that the inode
    /// store is read-locked when `func()` is called.
    pub fn map<V, F: Fn(&Arc<InodeData>) -> V>(&self, func: F) -> Vec<V> {
        self.inner.read().unwrap().data.values().map(func).collect()
    }

    /// Turn the weak reference `inode` into a strong one (increments its refcount)
    pub fn get_strong(&self, inode: Inode) -> io::Result<StrongInodeReference> {
        StrongInodeReference::new(inode, self)
    }

    /// Attempt to get an inode from `inodes` and create a strong reference to it, i.e. increment
    /// its refcount.  Return that reference on success, and an error on failure.
    /// Reasons for failure can be that the inode isn't in the map or that the refcount is zero.
    /// This function will never increment a refcount that's already zero.
    /// Note that dropping the returned strong reference will automatically decrement the refcount
    /// again.
    pub fn claim_inode(
        &self,
        handle: Option<&FileHandle>,
        ids: &InodeIds,
    ) -> io::Result<StrongInodeReference> {
        self.do_claim_inode(&self.inner.read().unwrap(), handle, ids)
    }

    fn do_claim_inode<I: Deref<Target = InodeStoreInner>>(
        &self,
        inner: &I,
        handle: Option<&FileHandle>,
        ids: &InodeIds,
    ) -> io::Result<StrongInodeReference> {
        let data = handle
            .and_then(|h| inner.get_by_handle(h))
            .or_else(|| {
                inner.get_by_ids(ids).filter(|data| {
                    // When we have to fall back to looking up an inode by its inode ID, ensure
                    // that we hit an entry that has a valid file descriptor.  Having an FD open
                    // means that the inode cannot really be deleted until the FD is closed, so
                    // that the inode ID remains valid until we evict the `InodeData`.  With no FD
                    // open (and just a file handle), the inode can be deleted while we still have
                    // our `InodeData`, and so the inode ID may be reused by a completely different
                    // new inode.  Such inodes must be looked up by file handle, because this
                    // handle contains a generation ID to differentiate between the old and the new
                    // inode.
                    matches!(data.file_or_handle, FileOrHandle::File(_))
                })
            })
            .ok_or_else(|| {
                io::Error::new(
                    io::ErrorKind::NotFound,
                    "Cannot take strong reference to inode by handle or IDs, not found".to_string(),
                )
            })?;

        StrongInodeReference::new_with_data(Arc::clone(data), self)
    }

    /// Check whether a matching inode is already present (see `claim_inode`), and if so, return
    /// that inode and drop `inode_data`.
    /// Otherwise, insert `inode_data`, and return a strong reference to it.  `inode_data.refcount`
    /// is ignored; the returned strong reference is the only one that can exist, so the refcount
    /// is hard-set to 1.
    pub fn get_or_insert(&self, mut inode_data: InodeData) -> io::Result<StrongInodeReference> {
        let mut inner = self.inner.write().unwrap();
        let handle = match &inode_data.file_or_handle {
            FileOrHandle::File(_) => None,
            FileOrHandle::Handle(handle) => Some(handle.inner()),
            FileOrHandle::Invalid(_) => None,
        };
        if let Ok(inode) = self.do_claim_inode(&inner, handle, &inode_data.ids) {
            // `InodeData`s should not be dropped while the inode store is locked, so drop the lock
            // before `inode_data`
            drop(inner);
            return Ok(inode);
        }
        if inner.contains(inode_data.inode) {
            // `InodeData`s should not be dropped while the inode store is locked, so drop the lock
            // before `inode_data`
            drop(inner);
            return Err(other_io_error(format!(
                "Double-use of FUSE inode ID {}",
                inode_data.inode
            )));
        }

        // Safe because we have the only reference
        inode_data.refcount = AtomicU64::new(1);
        let inode_data = Arc::new(inode_data);
        inner.insert_new(Arc::clone(&inode_data));

        // We just set the reference to 1 to account for this
        Ok(unsafe { StrongInodeReference::new_no_increment(inode_data, self) })
    }

    /// Insert `inode_data` into the inode store regardless of whether a matching inode already
    /// exists.  However, if the given inode ID already exists, return an error and drop
    /// `inode_data.`
    pub fn new_inode(&self, inode_data: InodeData) -> io::Result<()> {
        let mut inner = self.inner.write().unwrap();
        if inner.contains(inode_data.inode) {
            // `InodeData`s should not be dropped while the inode store is locked, so drop the lock
            // before `inode_data`
            drop(inner);
            return Err(other_io_error(format!(
                "Double-use of FUSE inode ID {}",
                inode_data.inode
            )));
        }
        inner.insert_new(Arc::new(inode_data));
        Ok(())
    }

    pub fn remove(&self, inode: Inode) {
        self.inner.write().unwrap().remove(inode);
    }

    pub fn forget_one(&self, inode: Inode, count: u64) {
        self.inner.write().unwrap().forget_one(inode, count);
    }

    pub fn forget_many<I: IntoIterator<Item = (Inode, u64)>>(&self, inodes: I) {
        let mut inner = self.inner.write().unwrap();
        for (inode, count) in inodes {
            inner.forget_one(inode, count);
        }
    }

    pub fn clear(&self) {
        self.inner.write().unwrap().clear();
    }

    pub fn clear_migration_info(&self) {
        self.inner.write().unwrap().clear_migration_info();
    }

    pub fn is_empty(&self) -> bool {
        self.inner.read().unwrap().is_empty()
    }
}

impl StrongInodeReference {
    /// Create a new strong reference to the given inode in the given inode store, incrementing the
    /// refcount appropriately.
    pub fn new(inode: Inode, inode_store: &InodeStore) -> io::Result<Self> {
        let inode_data = inode_store.get(inode).ok_or_else(|| {
            io::Error::new(
                io::ErrorKind::NotFound,
                format!("Cannot take strong reference to inode {inode}: Not found"),
            )
        })?;

        Self::new_with_data(inode_data, inode_store)
    }

    /// Create a new strong reference to an inode with the given data from the given inode store,
    /// incrementing the refcount appropriately.
    pub fn new_with_data(inode_data: Arc<InodeData>, inode_store: &InodeStore) -> io::Result<Self> {
        Self::increment_refcount_for(&inode_data)?;

        // Safe because we have just incremented the refcount
        Ok(unsafe { StrongInodeReference::new_no_increment(inode_data, inode_store) })
    }

    /// Create a new strong reference to an inode with the given data from the given inode store,
    /// but do not increment the inode's refcount, and instead assume that the caller has already
    /// done it.
    ///
    /// # Safety
    /// Caller ensures the inode's refcount is incremented by 1 to account for this strong
    /// reference.
    pub unsafe fn new_no_increment(inode_data: Arc<InodeData>, inode_store: &InodeStore) -> Self {
        StrongInodeReference {
            inode_data: Some(inode_data),
            inode_store: Arc::clone(&inode_store.inner),
        }
    }

    /// Tries to increment the refcount in the given `inode_data`, but will refuse to increment a
    /// refcount that is 0 (because in this case, the inode is already in the process of being
    /// removed from the store, so continuing to use it would not be safe).
    fn increment_refcount_for(inode_data: &InodeData) -> io::Result<()> {
        // Use `.fetch_update()` instead of `.fetch_add()` to ensure we never increment the
        // refcount from zero to one.
        match inode_data
            .refcount
            .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |rc| {
                (rc > 0).then_some(rc + 1)
            }) {
            Ok(_old_rc) => Ok(()),
            Err(_old_rc) => Err(io::Error::new(
                io::ErrorKind::NotFound,
                format!(
                    "Cannot take strong reference to inode {}: Is already deleted",
                    inode_data.inode
                ),
            )),
        }
    }

    /// Consume this strong reference, yield the underlying inode ID, without decrementing the
    /// inode's refcount.
    ///
    /// # Safety
    /// Caller must guarantee that the refcount is tracked somehow still, i.e. that forget_one()
    /// will eventually be called.  Otherwise, this inode will be truly leaked, which generally is
    /// not good.
    pub unsafe fn leak(mut self) -> Inode {
        // Unwrapping is safe: Every initializer sets this to `Some(_)`, and every function that
        // `take()`s the value (`leak()`, `drop_unlocked()`, `drop()`) also consumes `self`, so
        // outside of them, this must always be `None`.
        self.inode_data.take().unwrap().inode
    }

    /// Yield the underlying inode ID.
    ///
    /// # Safety
    /// The inode ID is technically a form of a weak reference.  To ensure safety, the caller may
    /// not assume that it is valid beyond the lifetime of the corresponding strong reference.
    pub unsafe fn get_raw(&self) -> Inode {
        // Unwrapping is safe: Every initializer sets this to `Some(_)`, and every function that
        // `take()`s the value (`leak()`, `drop_unlocked()`, `drop()`) also consumes `self`, so
        // outside of them, this must always be `None`.
        self.inode_data.as_ref().unwrap().inode
    }

    /// Get the associated inode data.
    pub fn get(&self) -> &InodeData {
        // Unwrapping is safe: Every initializer sets this to `Some(_)`, and every function that
        // `take()`s the value (`leak()`, `drop_unlocked()`, `drop()`) also consumes `self`, so
        // outside of them, this must always be `None`.
        self.inode_data.as_ref().unwrap()
    }

    /// This function allows dropping a `StrongInodeReference` while the inode store is locked, but
    /// the caller must have mutable access to the inode store.
    fn drop_unlocked(mut self, inodes: &mut InodeStoreInner) {
        if let Some(inode_data) = self.inode_data.take() {
            inodes.forget_one(inode_data.inode, 1);
        }
    }
}

impl Clone for StrongInodeReference {
    /// Create an additional strong reference.
    fn clone(&self) -> Self {
        // Unwrapping is safe: Every initializer sets this to `Some(_)`, and every function that
        // `take()`s the value (`leak()`, `drop_unlocked()`, `drop()`) also consumes `self`, so
        // outside of them, this must always be `None`.
        let cloned_data = Arc::clone(self.inode_data.as_ref().unwrap());
        let cloned_store = Arc::clone(&self.inode_store);

        // Unwrapping is safe, because this can only fail if the refcount became 0, which is
        // impossible because `self` is a strong reference
        Self::increment_refcount_for(&cloned_data).unwrap();

        StrongInodeReference {
            inode_data: Some(cloned_data),
            inode_store: cloned_store,
        }
    }
}

impl Drop for StrongInodeReference {
    /// Decrement the refcount on the referenced inode, removing it from the store when the
    /// refcount reaches 0.
    /// Note that this function locks `self.inode_store`, so a `StrongInodeReference` must not be
    /// dropped while that inode store is locked.  In such a case,
    /// `StrongInodeReference::drop_unlocked()` must be used.
    fn drop(&mut self) {
        if let Some(inode_data) = self.inode_data.take() {
            self.inode_store
                .write()
                .unwrap()
                .forget_one(inode_data.inode, 1);
        }
    }
}

impl Drop for InodeStore {
    /// Explicitly clear the inner inode store on drop, because there may be circular references
    /// within (in the migration info's strong references) that may otherwise prevent the
    /// `InodeStoreInner` from being dropped.
    fn drop(&mut self) {
        self.inner.write().unwrap().clear();
    }
}

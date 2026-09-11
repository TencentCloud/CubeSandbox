// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use std::ffi::{CString, OsStr};
use std::fs::File;
use std::os::fd::{AsRawFd, FromRawFd};
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::MetadataExt;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

use tokio::sync::oneshot;

#[cfg(test)]
pub(crate) mod snapshot_test;
pub(crate) mod watch;

use crate::error::DomainError;
use crate::proto::filesystem::{EntryInfo, FileType};
use crate::runtime::{ProcessUser, RuntimeState, UserDatabase};

/// Each accepted operation owns a worker so a blocked filesystem cannot occupy
/// Tokio runtime threads or impose an admission limit on independent requests.
pub struct FilesystemService {
    polling: Arc<watch::polling::Registry>,
}

impl Default for FilesystemService {
    fn default() -> Self {
        Self {
            polling: Arc::new(watch::polling::Registry::default()),
        }
    }
}

impl crate::server::ReadinessCheck for FilesystemService {
    fn self_check(&self) -> futures::future::BoxFuture<'_, bool> {
        Box::pin(async move { self.polling.self_check() })
    }
}

struct CancelOnDrop(Arc<AtomicBool>);
impl Drop for CancelOnDrop {
    fn drop(&mut self) {
        self.0.store(true, Ordering::Release);
    }
}

struct FileJob<T> {
    receiver: oneshot::Receiver<Result<T, DomainError>>,
    _cancel: CancelOnDrop,
}

impl<T> FileJob<T> {
    async fn wait(self) -> Result<T, DomainError> {
        self.receiver.await.map_err(|_| DomainError::Internal)?
    }
}

impl<T: Send + 'static> FileJob<T> {
    async fn wait_owned(self) -> Result<T, DomainError> {
        // Accepted filesystem calls finish even when their HTTP caller leaves.
        // The waiter owns cancellation until the disposable worker has returned.
        tokio::spawn(self.wait())
            .await
            .map_err(|_| DomainError::Internal)?
    }
}

impl FilesystemService {
    fn spawn<T: Send + 'static>(
        &self,
        username: Option<String>,
        snapshot: Arc<RuntimeState>,
        users: UserDatabase,
        operation: impl FnOnce(Context) -> Result<T, DomainError> + Send + 'static,
    ) -> Result<FileJob<T>, DomainError> {
        let username = username.unwrap_or_else(|| snapshot.default_user().to_owned());
        let cancel = CancelOnDrop(Arc::new(AtomicBool::new(false)));
        let stopped = cancel.0.clone();
        let (sender, receiver) = oneshot::channel();
        std::thread::Builder::new()
            .name("envd-file".into())
            .spawn(move || {
                let result = (|| {
                    check_cancel(&stopped)?;
                    let user = users.resolve_file_user(&username, &stopped)?;
                    check_cancel(&stopped)?;
                    operation(Context {
                        snapshot,
                        user,
                        cancel: stopped,
                    })
                })();
                let _ = sender.send(result);
            })
            .map_err(|_| DomainError::ResourceExhausted("filesystem worker unavailable".into()))?;
        Ok(FileJob {
            receiver,
            _cancel: cancel,
        })
    }

    async fn run<T: Send + 'static>(
        &self,
        headers: &http::HeaderMap,
        snapshot: Arc<RuntimeState>,
        users: UserDatabase,
        operation: impl FnOnce(Context) -> Result<T, DomainError> + Send + 'static,
    ) -> Result<T, DomainError> {
        self.spawn(
            crate::runtime::explicit_username(headers)?,
            snapshot,
            users,
            operation,
        )?
        .wait_owned()
        .await
    }

    pub(crate) async fn stat(
        &self,
        path: String,
        headers: &http::HeaderMap,
        snapshot: Arc<RuntimeState>,
        users: UserDatabase,
    ) -> Result<EntryInfo, DomainError> {
        self.run(headers, snapshot, users, move |context| {
            let path = context.rpc_path(&path)?;
            let failure = |error: std::io::Error| {
                let detail = format!("lstat {}: {}", path.display(), io_detail(&error));
                if error.raw_os_error() == Some(libc::ENOENT) {
                    DomainError::NotFound(format!("file not found: {detail}"))
                } else {
                    DomainError::InternalMessage(format!("error getting file info: {detail}"))
                }
            };
            validate_os_path(&path).map_err(failure)?;
            let (parent, name) = context.parent_with_error(&path, failure)?;
            context.check()?;
            let object = open_at(&parent, &name, libc::O_NOFOLLOW).map_err(failure)?;
            entry(&object, &parent, &path, &context.cancel)
        })
        .await
    }

    pub(crate) async fn make_dir(
        &self,
        path: String,
        headers: &http::HeaderMap,
        snapshot: Arc<RuntimeState>,
        users: UserDatabase,
    ) -> Result<EntryInfo, DomainError> {
        self.run(headers, snapshot, users, move |context| {
            let path = context.rpc_path(&path)?;
            let failure = |error: std::io::Error| {
                if error.raw_os_error() == Some(libc::ENOENT) {
                    lookup_error(error)
                } else {
                    DomainError::InternalMessage(format!(
                        "error getting file info: stat {}: {}",
                        path.display(),
                        io_detail(&error)
                    ))
                }
            };
            validate_os_path(&path).map_err(failure)?;
            let existing = context
                .parent_with_error(&path, failure)
                .and_then(|(parent, name)| {
                    context.check()?;
                    open_at(&parent, &name, 0).map_err(failure)
                });
            match existing {
                Ok(object) => {
                    context.check()?;
                    return if object.metadata().map_err(lookup_error)?.is_dir() {
                        Err(DomainError::Conflict(format!(
                            "directory already exists: {}",
                            path.display()
                        )))
                    } else {
                        Err(invalid(&format!(
                            "path already exists but it is not a directory: {}",
                            path.display()
                        )))
                    };
                }
                Err(DomainError::NotFound(_)) => {}
                Err(error) => return Err(error),
            }
            let (directory, _) = context.rpc_directories(&path)?;
            entry(&directory, &directory, &path, &context.cancel)
        })
        .await
    }
    pub(crate) async fn move_entry(
        &self,
        source: String,
        destination: String,
        headers: &http::HeaderMap,
        snapshot: Arc<RuntimeState>,
        users: UserDatabase,
    ) -> Result<EntryInfo, DomainError> {
        self.run(headers, snapshot, users, move |context| {
            let source = protect_root(context.rpc_path(&source)?)?;
            let destination = protect_root(context.rpc_path(&destination)?)?;
            let failure = |error| move_error(&source, &destination, error);
            let bind_source = || {
                let (parent, name) = context.parent_with_error(&source, failure)?;
                context.check()?;
                let object = open_at(&parent, &name, libc::O_NOFOLLOW).map_err(failure)?;
                Ok::<_, DomainError>((parent, name, object))
            };
            let source_binding = bind_source();
            let (destination_parent_path, destination_name) = split_path(&destination);
            let (destination_parent, _) = context.rpc_directories(destination_parent_path)?;
            // Go converts both complete rename arguments before the syscall,
            // after EnsureDirs. NUL therefore precedes a missing source, but
            // does not suppress destination parent creation.
            CString::new(source.as_os_str().as_bytes())
                .and_then(|_| CString::new(destination.as_os_str().as_bytes()))
                .map_err(|_| failure(std::io::Error::from_raw_os_error(libc::EINVAL)))?;
            validate_os_path(&source).map_err(failure)?;
            validate_os_path(&destination).map_err(failure)?;
            // Keep a successfully bound source across parent creation, while
            // preserving upstream's destination creation before rename errors.
            // Creating the destination parents can create an initially absent
            // source too. Recheck failed lookups; never replace a successful binding.
            let (source_parent, source_name, object) = source_binding.or_else(|_| bind_source())?;
            let source_name =
                CString::new(source_name.as_bytes()).map_err(|_| invalid("invalid source"))?;
            let destination_name = CString::new(destination_name.as_bytes())
                .map_err(|_| invalid("invalid destination"))?;
            context.check()?;
            let current = open_at(
                &source_parent,
                OsStr::from_bytes(source_name.as_bytes()),
                libc::O_NOFOLLOW,
            )
            .map_err(failure)?;
            let original_metadata = object.metadata().map_err(lookup_error)?;
            let current_metadata = current.metadata().map_err(lookup_error)?;
            if original_metadata.ino() != current_metadata.ino()
                || original_metadata.dev() != current_metadata.dev()
                || mount_id(&object)? != mount_id(&current)?
            {
                return Err(DomainError::FailedPrecondition(
                    "source entry changed".into(),
                ));
            }
            context.check()?;
            // Parent descriptors remain stable; final directory entries use POSIX rename.
            // The comparison only detects an already-visible replacement; it cannot
            // atomically condition renameat on an inode identity.
            // Once in the kernel the rename may complete despite RPC cancellation.
            if unsafe {
                libc::renameat(
                    source_parent.as_raw_fd(),
                    source_name.as_ptr(),
                    destination_parent.as_raw_fd(),
                    destination_name.as_ptr(),
                )
            } != 0
            {
                return Err(failure(std::io::Error::last_os_error()));
            }
            #[cfg(test)]
            crate::filesystem::snapshot_test::file_barrier(
                "Renamed",
                &destination,
                &context.cancel,
            )?;
            context.check()?;
            entry(&object, &destination_parent, &destination, &context.cancel)
        })
        .await
    }

    pub(crate) async fn list_dir(
        &self,
        path: String,
        depth: u32,
        headers: &http::HeaderMap,
        snapshot: Arc<RuntimeState>,
        users: UserDatabase,
    ) -> Result<Vec<EntryInfo>, DomainError> {
        self.run(headers, snapshot, users, move |context| {
            let path = context.rpc_path(&path)?;
            let (directory, resolved) = context.list_root(&path)?;
            if !directory.metadata().map_err(lookup_error)?.is_dir() {
                return Err(invalid(&format!(
                    "path is not a directory: {}",
                    resolved.display()
                )));
            }
            let mut entries = Vec::new();
            context
                .list(&directory, &path, &resolved, depth.max(1), &mut entries)
                .map_err(|error| match error {
                    DomainError::InternalMessage(detail) => DomainError::InternalMessage(format!(
                        "error reading directory {}: {detail}",
                        resolved.display()
                    )),
                    other => other,
                })?;
            Ok(entries)
        })
        .await
    }

    pub(crate) async fn remove(
        &self,
        path: String,
        headers: &http::HeaderMap,
        snapshot: Arc<RuntimeState>,
        users: UserDatabase,
    ) -> Result<(), DomainError> {
        self.run(headers, snapshot, users, move |context| {
            let final_dot = path.starts_with('/') && path.trim_end_matches('/').ends_with("/.");
            let path = protect_root(context.rpc_path(&path)?)?;
            if final_dot {
                return Err(DomainError::Internal);
            }
            // Only Remove ignores final slashes, so a final link is not followed.
            let path = Path::new(
                path.to_str()
                    .ok_or_else(|| invalid("invalid path"))?
                    .trim_end_matches('/'),
            );
            let parent_path = path.parent().unwrap_or(Path::new("/"));
            let parent_error = |error: std::io::Error| {
                if error.raw_os_error() == Some(libc::ENOENT) {
                    lookup_error(error)
                } else {
                    DomainError::InternalMessage(format!(
                        "error removing file or directory: open {}: {}",
                        parent_path.display(),
                        io_detail(&error)
                    ))
                }
            };
            validate_os_path(parent_path).map_err(parent_error)?;
            let (parent, name) = match context.parent_with_error(path, parent_error) {
                Ok(result) => result,
                Err(DomainError::NotFound(_)) => return Ok(()),
                Err(error) => return Err(error),
            };
            context.remove_child(&parent, &name, path)
        })
        .await
    }
}

struct Context {
    snapshot: Arc<RuntimeState>,
    user: ProcessUser,
    cancel: Arc<AtomicBool>,
}

impl Context {
    fn check(&self) -> Result<(), DomainError> {
        check_cancel(&self.cancel)
    }

    fn rpc_path(&self, raw: &str) -> Result<PathBuf, DomainError> {
        // RPC handlers map invalid OS paths at the operation that uses them.
        let raw = if raw.is_empty() {
            self.snapshot.default_workdir().unwrap_or("")
        } else {
            raw
        };
        let path = expand_path(raw, &self.user.home).map_err(|_| {
            invalid(&format!(
                "failed to expand path '' for user '{}': cannot expand user-specific home dir",
                self.user.name
            ))
        })?;
        if raw.starts_with('/') {
            return Ok(path);
        }
        // Go filepath.Join cleans relative and home-expanded paths before any
        // filesystem lookup; absolute request paths retain kernel semantics.
        let mut clean = PathBuf::new();
        for component in path.components() {
            if component == std::path::Component::ParentDir {
                clean.pop();
            } else {
                clean.push(component.as_os_str());
            }
        }
        Ok(clean)
    }

    fn list(
        &self,
        directory: &File,
        path: &Path,
        resolved: &Path,
        depth: u32,
        entries: &mut Vec<EntryInfo>,
    ) -> Result<(), DomainError> {
        struct Directory {
            object: File,
            path: PathBuf,
            resolved: PathBuf,
            depth: u32,
            names: std::vec::IntoIter<std::ffi::OsString>,
        }
        // Keep bound ancestors on the heap instead of consuming one Rust stack
        // frame per directory. The requested depth does not allocate any frames.
        let mut pending = vec![Directory {
            object: directory.try_clone().map_err(lookup_error)?,
            path: path.to_owned(),
            resolved: resolved.to_owned(),
            depth,
            names: self.names(directory, resolved)?.into_iter(),
        }];
        while let Some(directory) = pending.last_mut() {
            self.check()?;
            let Some(name) = directory.names.next() else {
                pending.pop();
                continue;
            };
            let object = match open_at(&directory.object, &name, libc::O_NOFOLLOW) {
                Ok(file) => file,
                Err(error) if error.raw_os_error() == Some(libc::ENOENT) => continue,
                Err(error) => return Err(lookup_error(error)),
            };
            self.check()?;
            let child_path = directory.path.join(&name);
            let info = match entry(&object, &directory.object, &child_path, &self.cancel) {
                Ok(info) => info,
                Err(DomainError::NotFound(_)) => continue,
                Err(error) => return Err(error),
            };
            entries.push(info);
            self.check()?;
            if directory.depth > 1 && object.metadata().map_err(lookup_error)?.is_dir() {
                let resolved = directory.resolved.join(&name);
                let child = Directory {
                    names: self.names(&object, &resolved)?.into_iter(),
                    object,
                    path: child_path,
                    resolved,
                    depth: directory.depth - 1,
                };
                // A completed parent is no longer needed: the child is already
                // bound. Keep only ancestors with siblings still to visit.
                if directory.names.len() == 0 {
                    pending.pop();
                }
                pending.push(child);
            }
        }
        Ok(())
    }

    fn names(&self, directory: &File, path: &Path) -> Result<Vec<std::ffi::OsString>, DomainError> {
        self.check()?;
        let reader = std::fs::read_dir(format!("/proc/self/fd/{}", directory.as_raw_fd()))
            .map_err(|error| {
                DomainError::InternalMessage(format!(
                    "open {}: {}",
                    path.display(),
                    io_detail(&error)
                ))
            })?;
        let mut names = Vec::new();
        for item in reader {
            self.check()?;
            let item = item.map_err(lookup_error)?;
            names.push(item.file_name());
        }
        names.sort();
        Ok(names)
    }

    fn remove_child(&self, parent: &File, name: &OsStr, path: &Path) -> Result<(), DomainError> {
        let Some(entry) = self.bind_remove(parent, name, path)? else {
            return Ok(());
        };
        let mut stack = vec![entry];
        loop {
            let entry = stack.last_mut().unwrap();
            let result = if let Some(name) = entry.children.next() {
                match self.bind_remove(&entry.object, &name, &entry.path.join(&name)) {
                    Ok(Some(child)) => {
                        stack.push(child);
                        continue;
                    }
                    Ok(None) => continue,
                    Err(error) => Err(error),
                }
            } else {
                let entry = stack.pop().unwrap();
                self.finish_remove(entry)
            };
            let Some(parent) = stack.last_mut() else {
                return result;
            };
            if let Err(error) = result {
                match error {
                    DomainError::InternalMessage(_) => {
                        parent.first_error.get_or_insert(error);
                    }
                    // Cancellation and object replacement stop the traversal.
                    _ => return Err(error),
                }
            }
        }
    }

    fn bind_remove(
        &self,
        parent: &File,
        name: &OsStr,
        path: &Path,
    ) -> Result<Option<RemoveEntry>, DomainError> {
        self.check()?;
        let object = match open_at(parent, name, libc::O_NOFOLLOW) {
            Ok(object) => object,
            Err(error) if error.raw_os_error() == Some(libc::ENOENT) => return Ok(None),
            Err(error) if matches!(error.raw_os_error(), Some(libc::EACCES | libc::EPERM)) => {
                return Err(DomainError::InternalMessage(format!(
                    "error removing file or directory: open {}: {}",
                    path.parent().unwrap_or(Path::new("/")).display(),
                    io_detail(&error)
                )));
            }
            Err(error) => return Err(remove_error(path, error)),
        };
        // Bind this object's mount identity independently of its parent: Remove
        // traverses mounted directories, but must still detect replacement.
        let mount = mount_id(&object)?;
        self.check()?;
        let metadata = object.metadata().map_err(lookup_error)?;
        if metadata.is_dir() {
            self.check()?;
            let root = root().map_err(lookup_error)?;
            let root_metadata = root.metadata().map_err(lookup_error)?;
            // Root can be reached through `..` or a bind mount at any depth.
            // Bind aliases have different mount IDs but the same device/inode.
            // Check the bound directory before deleting any of its children.
            if metadata.ino() == root_metadata.ino() && metadata.dev() == root_metadata.dev() {
                return Err(invalid("root mutation is forbidden"));
            }
        }
        let children = if metadata.is_dir() {
            self.names(&object, path).map_err(|error| match error {
                DomainError::InternalMessage(detail) => DomainError::InternalMessage(format!(
                    "error removing file or directory: {detail}"
                )),
                other => other,
            })?
        } else {
            Vec::new()
        };
        Ok(Some(RemoveEntry {
            parent: parent.try_clone().map_err(lookup_error)?,
            name: name.to_owned(),
            path: path.to_owned(),
            object,
            metadata,
            mount,
            children: children.into_iter(),
            first_error: None,
        }))
    }

    fn finish_remove(&self, entry: RemoveEntry) -> Result<(), DomainError> {
        let RemoveEntry {
            parent,
            name,
            path,
            object: _object,
            metadata,
            mount,
            first_error,
            ..
        } = entry;
        self.check()?;
        // Detect replacement already visible here. This is not an atomic inode
        // condition for unlinkat: the final name retains normal POSIX semantics.
        // Recursion above only ever uses the bound directory, never a replacement.
        let current = match open_at(&parent, &name, libc::O_NOFOLLOW) {
            Ok(current) => current,
            Err(error) if error.raw_os_error() == Some(libc::ENOENT) => return Ok(()),
            Err(error) => return Err(lookup_error(error)),
        };
        let current_metadata = current.metadata().map_err(lookup_error)?;
        if current_metadata.ino() != metadata.ino()
            || current_metadata.dev() != metadata.dev()
            || mount_id(&current)? != mount
        {
            return Err(DomainError::FailedPrecondition(
                "directory entry changed".into(),
            ));
        }
        self.check()?;
        let name = CString::new(name.as_bytes()).map_err(|_| invalid("invalid path"))?;
        let flags = if metadata.is_dir() {
            libc::AT_REMOVEDIR
        } else {
            0
        };
        if unsafe { libc::unlinkat(parent.as_raw_fd(), name.as_ptr(), flags) } != 0 {
            let error = std::io::Error::last_os_error();
            if error.raw_os_error() != Some(libc::ENOENT) {
                return Err(first_error.unwrap_or_else(|| remove_error(&path, error)));
            }
        }
        #[cfg(test)]
        crate::filesystem::snapshot_test::file_barrier(
            "Removed",
            Path::new(OsStr::from_bytes(name.as_bytes())),
            &self.cancel,
        )?;
        first_error.map_or(Ok(()), Err)
    }

    fn list_root(&self, requested: &Path) -> Result<(File, PathBuf), DomainError> {
        use std::collections::VecDeque;
        use std::path::Component;

        let components = |path: &Path| {
            let mut parts: VecDeque<_> = path
                .components()
                .map(|part| part.as_os_str().to_owned())
                .collect();
            // Path::components normalizes these suffixes, but they require
            // the preceding object to be a directory during Go resolution.
            let bytes = path.as_os_str().as_bytes();
            if bytes.ends_with(b"/") || bytes.ends_with(b"/.") {
                parts.push_back(".".into());
            }
            parts
        };
        let mut pending = components(requested);
        let mut directory = root().map_err(lookup_error)?;
        let mut resolved = PathBuf::from("/");
        let mut links = 0;
        while let Some(name) = pending.pop_front() {
            self.check()?;
            match Path::new(&name).components().next() {
                Some(Component::RootDir) => {
                    directory = root().map_err(lookup_error)?;
                    resolved = PathBuf::from("/");
                    continue;
                }
                Some(Component::CurDir) => continue,
                _ => {}
            }
            let path = resolved.join(&name);
            let object = validate_os_path(&path)
                .and_then(|_| open_at(&directory, &name, libc::O_NOFOLLOW))
                .map_err(|error| {
                    let detail = format!("lstat {}: {}", path.display(), io_detail(&error));
                    if error.raw_os_error() == Some(libc::ENOENT) {
                        DomainError::NotFound(format!("path not found: {detail}"))
                    } else {
                        DomainError::InternalMessage(format!("error resolving symlink: {detail}"))
                    }
                })?;
            let metadata = object.metadata().map_err(lookup_error)?;
            if metadata.file_type().is_symlink() {
                links += 1;
                if links > 255 {
                    return Err(DomainError::FailedPrecondition(format!(
                        "cyclic symlink or chain >255 links at {:?}",
                        requested.to_string_lossy()
                    )));
                }
                // Resolve one bound link at a time: the kernel's per-lookup
                // limit is lower than the upstream EvalSymlinks chain limit.
                let target = bound_link(&object, &self.cancel)?;
                for part in components(Path::new(OsStr::from_bytes(&target)))
                    .into_iter()
                    .rev()
                {
                    pending.push_front(part);
                }
                continue;
            }
            if !metadata.is_dir() && !pending.is_empty() {
                return Err(DomainError::InternalMessage(
                    "error resolving symlink: not a directory".into(),
                ));
            }
            if name == ".." {
                resolved.pop();
            } else {
                resolved.push(name);
            }
            directory = object;
        }
        Ok((directory, resolved))
    }

    fn parent(&self, path: &Path) -> Result<(File, std::ffi::OsString), DomainError> {
        self.parent_with_error(path, lookup_error)
    }

    fn parent_with_error(
        &self,
        path: &Path,
        map_error: impl Fn(std::io::Error) -> DomainError,
    ) -> Result<(File, std::ffi::OsString), DomainError> {
        let (parent_path, name) = split_path(path);
        self.check()?;
        let mut directory = root().map_err(&map_error)?;
        for component in parent_path
            .components()
            .filter(|component| !matches!(component, std::path::Component::RootDir))
        {
            self.check()?;
            directory = open_at(&directory, component.as_os_str(), libc::O_DIRECTORY)
                .map_err(&map_error)?;
        }
        self.check()?;
        Ok((directory, name))
    }

    fn prepare_creation(&self) -> Result<(), DomainError> {
        self.check()?;
        // Each job owns a disposable thread. Isolate its umask, then use raw
        // per-thread filesystem credentials; no other daemon thread is changed.
        // The kernel applies ownership/mode during mkdir itself, so a subsequent
        // name replacement never receives chown/chmod from this operation.
        if unsafe { libc::unshare(libc::CLONE_FS) } != 0 {
            return Err(mkdir_error(std::io::Error::last_os_error()));
        }
        unsafe { libc::umask(0) };
        // Linux capability v3: header(version, pid), two data words containing
        // effective/permitted/inheritable masks. Changing fsuid can clear effective
        // filesystem capabilities; retain the daemon's original access semantics.
        let mut header = [0x2008_0522u32, 0];
        let mut capabilities = [[0u32; 3]; 2];
        if unsafe {
            libc::syscall(
                libc::SYS_capget,
                header.as_mut_ptr(),
                capabilities.as_mut_ptr(),
            )
        } != 0
        {
            return Err(mkdir_error(std::io::Error::last_os_error()));
        }
        unsafe {
            libc::setfsgid(self.user.gid);
            libc::setfsuid(self.user.uid);
        }
        if unsafe { libc::setfsuid(u32::MAX) } as u32 != self.user.uid
            || unsafe { libc::setfsgid(u32::MAX) } as u32 != self.user.gid
        {
            return Err(DomainError::PermissionDenied);
        }
        if unsafe { libc::syscall(libc::SYS_capset, header.as_ptr(), capabilities.as_ptr()) } != 0 {
            return Err(mkdir_error(std::io::Error::last_os_error()));
        }
        // This worker is never reused; its private credentials/umask die on return.
        self.check()
    }

    fn rpc_directories(&self, path: &Path) -> Result<(File, bool), DomainError> {
        self.directories_with_errors(path, true, 0o755)
    }

    fn directories_with_errors(
        &self,
        path: &Path,
        rpc_errors: bool,
        mode: u32,
    ) -> Result<(File, bool), DomainError> {
        let mut current_path = PathBuf::from("/");
        let mut directory = root().map_err(lookup_error)?;
        let mut created = false;
        let mut creation_ready = false;
        for component in path
            .components()
            .filter(|part| !matches!(part, std::path::Component::RootDir))
        {
            self.check()?;
            let name = component.as_os_str();
            current_path.push(name);
            created = false;
            let opened = if rpc_errors {
                validate_os_path(&current_path)
            } else {
                Ok(())
            }
            .and_then(|_| {
                open_at(
                    &directory,
                    name,
                    if rpc_errors { 0 } else { libc::O_DIRECTORY },
                )
            });
            match opened {
                Ok(next) => {
                    if rpc_errors && !next.metadata().map_err(lookup_error)?.is_dir() {
                        return Err(DomainError::InternalMessage(format!(
                            "path is a file: {}",
                            current_path.display()
                        )));
                    }
                    directory = next;
                }
                Err(error) if error.raw_os_error() == Some(libc::ENOENT) => {
                    if !creation_ready {
                        self.prepare_creation()?;
                        creation_ready = true;
                    }
                    let name_c =
                        CString::new(name.as_bytes()).map_err(|_| invalid("invalid path"))?;
                    self.check()?;
                    let result =
                        unsafe { libc::mkdirat(directory.as_raw_fd(), name_c.as_ptr(), mode) };
                    if result != 0 {
                        let error = std::io::Error::last_os_error();
                        if error.raw_os_error() != Some(libc::EEXIST) {
                            return Err(if rpc_errors {
                                DomainError::InternalMessage(format!(
                                    "failed to create directory: mkdir {}: {}",
                                    current_path.display(),
                                    io_detail(&error)
                                ))
                            } else {
                                mkdir_error(error)
                            });
                        }
                    } else {
                        created = true;
                        #[cfg(test)]
                        crate::filesystem::snapshot_test::file_barrier(
                            "ParentCreated",
                            Path::new(name),
                            &self.cancel,
                        )?;
                    }
                    self.check()?;
                    let next = open_at(&directory, name, libc::O_DIRECTORY | libc::O_NOFOLLOW)
                        .map_err(mkdir_error)?;
                    if created {
                        self.check()?;
                        let metadata = next.metadata().map_err(mkdir_error)?;
                        if metadata.mode() & 0o7777 != mode
                            || metadata.uid() != self.user.uid
                            || metadata.gid() != self.user.gid
                        {
                            // Inheritance or replacement can change the observed attributes.
                            // Never repair them: this descriptor might bind an external inode.
                            return Err(DomainError::FailedPrecondition(
                                "directory creation attributes changed".into(),
                            ));
                        }
                    }
                    directory = next;
                }
                Err(error) => {
                    return Err(if rpc_errors {
                        DomainError::InternalMessage(format!(
                            "failed to stat directory: stat {}: {}",
                            current_path.display(),
                            io_detail(&error)
                        ))
                    } else {
                        mkdir_error(error)
                    })
                }
            }
        }
        Ok((directory, created))
    }
}

struct RemoveEntry {
    parent: File,
    name: std::ffi::OsString,
    path: PathBuf,
    object: File,
    metadata: std::fs::Metadata,
    mount: u64,
    children: std::vec::IntoIter<std::ffi::OsString>,
    first_error: Option<DomainError>,
}

fn mount_id(object: &File) -> Result<u64, DomainError> {
    // Linux statx UAPI has a 256-byte result, mask at byte 0 and mount ID at
    // byte 144. Use the syscall directly: libc does not expose statx on musl.
    const STATX_MNT_ID: u32 = 0x1000;
    let mut stat = [0u8; 256];
    if unsafe {
        libc::syscall(
            libc::SYS_statx,
            object.as_raw_fd(),
            c"".as_ptr(),
            libc::AT_EMPTY_PATH | libc::AT_SYMLINK_NOFOLLOW,
            STATX_MNT_ID,
            stat.as_mut_ptr(),
        )
    } != 0
    {
        return Err(lookup_error(std::io::Error::last_os_error()));
    }
    if u32::from_ne_bytes(stat[..4].try_into().unwrap()) & STATX_MNT_ID == 0 {
        return Err(DomainError::FailedPrecondition(
            "mount identity unavailable".into(),
        ));
    }
    Ok(u64::from_ne_bytes(stat[144..152].try_into().unwrap()))
}

fn remove_error(path: &Path, error: std::io::Error) -> DomainError {
    DomainError::InternalMessage(format!(
        "error removing file or directory: unlinkat {}: {}",
        path.display(),
        io_detail(&error)
    ))
}

fn move_error(source: &Path, destination: &Path, error: std::io::Error) -> DomainError {
    let detail = format!(
        "rename {} {}: {}",
        source.display(),
        destination.display(),
        io_detail(&error)
    );
    match error.raw_os_error() {
        Some(libc::ENOENT) => DomainError::NotFound(format!("source file not found: {detail}")),
        _ => DomainError::InternalMessage(format!("error renaming: {detail}")),
    }
}

fn mkdir_error(error: std::io::Error) -> DomainError {
    match error.raw_os_error() {
        Some(libc::ENOTDIR | libc::EEXIST) => invalid("path is not a directory"),
        Some(libc::ENOSPC | libc::EDQUOT) => {
            DomainError::ResourceExhausted("filesystem space exhausted".into())
        }
        _ => lookup_error(error),
    }
}

fn check_cancel(cancel: &AtomicBool) -> Result<(), DomainError> {
    if cancel.load(Ordering::Acquire) {
        Err(DomainError::Cancelled)
    } else {
        Ok(())
    }
}

fn expand_path(raw: &str, home: &Path) -> Result<PathBuf, DomainError> {
    let path = if raw == "~" {
        home.to_owned()
    } else if raw.starts_with("~/") || raw.starts_with("~\\") {
        home.join(&raw[2..])
    } else if raw.starts_with('~') {
        return Err(invalid("user-specific home expansion is unsupported"));
    } else {
        home.join(raw)
    };
    Ok(path)
}

fn protect_root(path: PathBuf) -> Result<PathBuf, DomainError> {
    let mut depth = 0usize;
    for component in path.components() {
        match component {
            std::path::Component::Normal(_) => depth += 1,
            std::path::Component::ParentDir => depth = depth.saturating_sub(1),
            _ => {}
        }
    }
    if depth == 0 {
        return Err(invalid("root mutation is forbidden"));
    }
    Ok(path)
}

fn validate_os_path(path: &Path) -> std::io::Result<()> {
    // Bound openat traversal must retain the errors a full Linux syscall path
    // would produce. NUL conversion precedes the kernel's PATH_MAX check.
    let bytes = path.as_os_str().as_bytes();
    if bytes.contains(&0) {
        Err(std::io::Error::from_raw_os_error(libc::EINVAL))
    } else if bytes.len() >= libc::PATH_MAX as usize {
        Err(std::io::Error::from_raw_os_error(libc::ENAMETOOLONG))
    } else {
        Ok(())
    }
}

fn invalid(message: &str) -> DomainError {
    DomainError::InvalidArgument(message.into())
}

fn split_path(path: &Path) -> (&Path, std::ffi::OsString) {
    let raw = path.as_os_str().as_bytes();
    let end = raw
        .iter()
        .rposition(|byte| *byte != b'/')
        .map_or(0, |index| index + 1);
    if end == 0 {
        return (Path::new("/"), ".".into());
    }
    let separator = raw[..end]
        .iter()
        .rposition(|byte| *byte == b'/')
        .unwrap_or(0);
    let parent = Path::new(OsStr::from_bytes(&raw[..=separator]));
    let mut name = OsStr::from_bytes(&raw[separator + 1..end]).to_owned();
    if end < raw.len() {
        name.push("/");
    }
    (parent, name)
}

fn root() -> std::io::Result<File> {
    let fd = unsafe {
        libc::open(
            c"/".as_ptr(),
            libc::O_PATH | libc::O_CLOEXEC | libc::O_DIRECTORY,
        )
    };
    owned_fd(fd)
}

fn owned_fd(fd: i32) -> std::io::Result<File> {
    if fd < 0 {
        Err(std::io::Error::last_os_error())
    } else {
        Ok(unsafe { File::from_raw_fd(fd) })
    }
}

fn open_at(parent: &File, name: &OsStr, flags: i32) -> std::io::Result<File> {
    let name = CString::new(name.as_bytes())
        .map_err(|_| std::io::Error::from_raw_os_error(libc::EINVAL))?;
    // O_PATH never opens FIFO/device data streams; each lookup binds its result.
    owned_fd(unsafe {
        libc::openat(
            parent.as_raw_fd(),
            name.as_ptr(),
            libc::O_PATH | libc::O_CLOEXEC | flags,
        )
    })
}

fn bound_link(object: &File, cancel: &AtomicBool) -> Result<Vec<u8>, DomainError> {
    // Empty-path readlinkat reads the bound symlink, not a replacement name.
    let mut bytes = vec![0u8; 4097];
    check_cancel(cancel)?;
    let length = unsafe {
        libc::readlinkat(
            object.as_raw_fd(),
            c"".as_ptr(),
            bytes.as_mut_ptr().cast(),
            bytes.len(),
        )
    };
    if length < 0 {
        return Err(lookup_error(std::io::Error::last_os_error()));
    }
    if length as usize == bytes.len() {
        return Err(invalid("symlink target exceeds limit"));
    }
    bytes.truncate(length as usize);
    check_cancel(cancel)?;
    Ok(bytes)
}

fn entry(
    object: &File,
    parent: &File,
    path: &Path,
    cancel: &AtomicBool,
) -> Result<EntryInfo, DomainError> {
    check_cancel(cancel)?;
    let metadata = object.metadata().map_err(lookup_error)?;
    let mut target = None;
    let mut symlink_target = None;
    if metadata.file_type().is_symlink() {
        let bytes = bound_link(object, cancel)?;
        match open_at(parent, OsStr::from_bytes(&bytes), 0) {
            Ok(file) => {
                check_cancel(cancel)?;
                symlink_target = Some(
                    std::fs::read_link(format!("/proc/self/fd/{}", file.as_raw_fd()))
                        .map_err(lookup_error)?
                        .to_string_lossy()
                        .into_owned(),
                );
                check_cancel(cancel)?;
                target = Some(file.metadata().map_err(lookup_error)?);
            }
            // Upstream still reports the bound link's own metadata when its
            // target cannot be resolved, including cyclic or inaccessible links.
            Err(_) => {
                symlink_target = Some(path.to_string_lossy().into_owned());
            }
        }
    }
    let effective = if metadata.file_type().is_symlink() {
        target.as_ref()
    } else {
        Some(&metadata)
    };
    let kind = match effective {
        Some(meta) if meta.is_dir() => FileType::Directory,
        Some(meta) if meta.is_file() => FileType::File,
        _ => FileType::Unspecified,
    };
    check_cancel(cancel)?;
    let owner = identity_name("/etc/passwd", metadata.uid());
    check_cancel(cancel)?;
    let group = identity_name("/etc/group", metadata.gid());
    check_cancel(cancel)?;
    Ok(EntryInfo {
        name: {
            let raw = path.as_os_str().as_bytes();
            let end = raw
                .iter()
                .rposition(|byte| *byte != b'/')
                .map_or(0, |index| index + 1);
            if end == 0 {
                "/".into()
            } else {
                OsStr::from_bytes(
                    raw[..end]
                        .rsplit(|byte| *byte == b'/')
                        .next()
                        .unwrap_or(b"/"),
                )
                .to_string_lossy()
                .into_owned()
            }
        },
        path: path.to_string_lossy().into_owned(),
        r#type: kind.into(),
        size: metadata.len() as i64,
        mode: effective.map_or(0, |meta| meta.mode() & 0o777),
        permissions: permissions(metadata.mode()),
        owner,
        group,
        modified_time: buffa_types::google::protobuf::Timestamp {
            seconds: metadata.mtime(),
            nanos: metadata.mtime_nsec() as i32,
            ..Default::default()
        }
        .into(),
        symlink_target,
        ..Default::default()
    })
}

fn identity_name(database: &str, id: u32) -> String {
    std::fs::read_to_string(database)
        .ok()
        .and_then(|text| {
            text.lines().find_map(|line| {
                let fields: Vec<_> = line.split(':').collect();
                (fields.len() > 2 && fields[2].parse::<u32>().ok() == Some(id))
                    .then(|| fields[0].to_owned())
            })
        })
        .unwrap_or_else(|| id.to_string())
}

fn permissions(mode: u32) -> String {
    let mut text = String::new();
    match mode & libc::S_IFMT {
        libc::S_IFDIR => text.push('d'),
        libc::S_IFLNK => text.push('L'),
        libc::S_IFIFO => text.push('p'),
        libc::S_IFSOCK => text.push('S'),
        libc::S_IFBLK => text.push('D'),
        libc::S_IFCHR => text.push_str("Dc"),
        _ => {}
    }
    if mode & libc::S_ISUID != 0 {
        text.push('u');
    }
    if mode & libc::S_ISGID != 0 {
        text.push('g');
    }
    if mode & libc::S_ISVTX != 0 {
        text.push('t');
    }
    if text.is_empty() {
        text.push('-');
    }
    for (bit, character) in [
        (0o400, 'r'),
        (0o200, 'w'),
        (0o100, 'x'),
        (0o040, 'r'),
        (0o020, 'w'),
        (0o010, 'x'),
        (0o004, 'r'),
        (0o002, 'w'),
        (0o001, 'x'),
    ] {
        text.push(if mode & bit != 0 { character } else { '-' });
    }
    text
}

fn io_detail(error: &std::io::Error) -> String {
    // Go's Linux errno text is independent of libc. In particular, musl uses
    // different wording for these errors from real mount/watcher operations.
    match error.raw_os_error() {
        Some(libc::EBUSY) => return "device or resource busy".into(),
        Some(libc::EDQUOT) => return "disk quota exceeded".into(),
        Some(libc::EMFILE) => return "too many open files".into(),
        Some(libc::EIO) => return "input/output error".into(),
        Some(libc::ENAMETOOLONG) => return "file name too long".into(),
        Some(libc::ELOOP) => return "too many levels of symbolic links".into(),
        Some(libc::ENOMEM) => return "cannot allocate memory".into(),
        _ => {}
    }
    let detail = error.to_string().to_lowercase();
    detail
        .split(" (os error ")
        .next()
        .unwrap_or(&detail)
        .to_owned()
}

fn lookup_error(error: std::io::Error) -> DomainError {
    match error.raw_os_error() {
        Some(libc::ENOENT | libc::ENOTDIR) => DomainError::NotFound("path not found".into()),
        Some(libc::EACCES | libc::EPERM | libc::EROFS) => DomainError::PermissionDenied,
        Some(libc::ELOOP) => DomainError::FailedPrecondition("symlink loop".into()),
        Some(libc::EMFILE | libc::ENFILE | libc::ENOMEM) => {
            DomainError::ResourceExhausted("filesystem resources exhausted".into())
        }
        _ => DomainError::Internal,
    }
}

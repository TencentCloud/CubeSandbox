// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use std::fs::File;
use std::io::{Read, Write};
use std::os::fd::AsRawFd;
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::{MetadataExt, PermissionsExt};
use std::sync::Arc;

use super::upload::{status, UploadError};
use super::{Context, FilesystemService};
use crate::runtime::{RuntimeState, UserDatabase};
use http::StatusCode;

type Result<T> = std::result::Result<T, UploadError>;

#[derive(Default, serde::Deserialize)]
pub(crate) struct Compose {
    #[serde(default)]
    pub destination: Option<String>,
    #[serde(default)]
    pub source_paths: Option<Vec<String>>,
    pub username: Option<String>,
}

impl FilesystemService {
    pub(crate) async fn compose(
        &self,
        request: Compose,
        snapshot: Arc<RuntimeState>,
        users: UserDatabase,
    ) -> Result<serde_json::Value> {
        let sources = request.source_paths.unwrap_or_default();
        if sources.is_empty() {
            return Err(UploadError::new(
                StatusCode::BAD_REQUEST,
                "source_paths must not be empty",
            ));
        }
        let destination = request.destination.unwrap_or_default();
        if destination.is_empty() {
            return Err(UploadError::new(
                StatusCode::BAD_REQUEST,
                "destination is required",
            ));
        }
        let job = self
            .spawn(request.username, snapshot, users, move |context| {
                Ok(context.compose(&destination, sources))
            })
            .map_err(status)?;
        job.wait_owned().await.map_err(status)?
    }
}

impl Context {
    fn compose(&self, destination: &str, sources: Vec<String>) -> Result<serde_json::Value> {
        let destination = self.destructive_path(destination).map_err(status)?;
        let mut resolved = Vec::with_capacity(sources.len());
        for source in sources {
            self.check().map_err(status)?;
            let path = self.path(&source, false).map_err(status)?;
            if path == destination {
                return Err(UploadError::new(
                    StatusCode::BAD_REQUEST,
                    format!("source path {source:?} cannot be the same as destination"),
                ));
            }
            let metadata = std::fs::metadata(&path).map_err(|_| {
                UploadError::new(
                    StatusCode::NOT_FOUND,
                    format!("source file not found: {source}"),
                )
            })?;
            if !metadata.is_file() {
                return Err(UploadError::new(
                    StatusCode::BAD_REQUEST,
                    format!("source path is not a regular file: {source}"),
                ));
            }
            let entry = std::fs::symlink_metadata(&path)
                .map_err(|e| io_error("statting source entry", e))?;
            resolved.push((path, metadata, entry, None));
        }
        // Read the inherited mask only after isolating this disposable worker's
        // filesystem state. Parent creation may clear its mask for atomic modes.
        if unsafe { libc::unshare(libc::CLONE_FS) } != 0 {
            return Err(io_error(
                "preparing destination file",
                std::io::Error::last_os_error(),
            ));
        }
        let mask = unsafe { libc::umask(0) };
        let (parent_path, name) = super::split_path(&destination);
        let (parent, _) = self
            .directories_with_errors(parent_path, false, 0o755 & !mask)
            .map_err(status)?;
        let bound_parent = format!("/proc/self/fd/{}", parent.as_raw_fd());
        let mut temporary = tempfile::Builder::new()
            .prefix(&format!("{}.e2b-compose.", name.to_string_lossy()))
            .suffix(".tmp")
            .tempfile_in(&bound_parent)
            .map_err(|e| io_error("creating destination file", e))?;
        let temporary_path = parent_path.join(temporary.path().file_name().unwrap());
        if unsafe {
            libc::fchown(
                temporary.as_file().as_raw_fd(),
                self.user.uid,
                self.user.gid,
            )
        } != 0
        {
            return Err(io_error(
                "changing file ownership",
                std::io::Error::last_os_error(),
            ));
        }
        temporary
            .as_file()
            .set_permissions(std::fs::Permissions::from_mode(0o666 & !mask))
            .map_err(|e| io_error("changing file mode", e))?;
        let mut buffer = [0; super::upload::CHUNK];
        for (path, metadata, _, parent_identity) in &mut resolved {
            self.check().map_err(status)?;
            // Check and reopen an O_PATH-bound inode, so a substituted device or
            // FIFO is rejected before any data open and cannot redirect the read.
            let (source_parent, source_name) = self.parent(path).map_err(status)?;
            let parent_metadata = source_parent
                .metadata()
                .map_err(|e| io_error("statting source parent", e))?;
            *parent_identity = Some((parent_metadata.dev(), parent_metadata.ino()));
            let object = super::open_at(&source_parent, &source_name, 0).map_err(|e| {
                io_error(
                    &format!(
                        "opening source file {}: open {}",
                        path.display(),
                        path.display()
                    ),
                    e,
                )
            })?;
            let opened = object
                .metadata()
                .map_err(|e| io_error("statting source file", e))?;
            if !opened.is_file() || opened.dev() != metadata.dev() || opened.ino() != metadata.ino()
            {
                return Err(UploadError::new(
                    StatusCode::CONFLICT,
                    "source file changed during compose",
                ));
            }
            let mut source =
                File::open(format!("/proc/self/fd/{}", object.as_raw_fd())).map_err(|e| {
                    io_error(
                        &format!(
                            "opening source file {}: open {}",
                            path.display(),
                            path.display()
                        ),
                        e,
                    )
                })?;
            loop {
                self.check().map_err(status)?;
                let count = source.read(&mut buffer).map_err(|e| {
                    io_error(
                        &format!(
                            "composing source {}: read {}",
                            path.display(),
                            path.display()
                        ),
                        e,
                    )
                })?;
                if count == 0 {
                    break;
                }
                self.check().map_err(status)?;
                temporary
                    .as_file_mut()
                    .write_all(&buffer[..count])
                    .map_err(|e| {
                        io_error(
                            &format!(
                                "composing source {}: write {}",
                                path.display(),
                                temporary_path.display()
                            ),
                            e,
                        )
                    })?;
            }
        }
        self.check().map_err(status)?;
        // Close errors must prevent publication. TempPath retains cancellation/error cleanup.
        let (file, temp_path) = temporary.into_parts();
        close(file).map_err(|e| {
            io_error(
                &format!(
                    "closing destination file: close {}",
                    temporary_path.display()
                ),
                e,
            )
        })?;
        self.check().map_err(status)?;
        temp_path
            .persist(std::path::Path::new(&bound_parent).join(name))
            .map_err(|e| {
                // Go's os.Rename reports EEXIST for a destination directory.
                let error = if e.error.raw_os_error() == Some(libc::EISDIR) {
                    std::io::Error::from_raw_os_error(libc::EEXIST)
                } else {
                    e.error
                };
                io_error(
                    &format!(
                        "finalizing compose: rename {} {}",
                        temporary_path.display(),
                        destination.display()
                    ),
                    error,
                )
            })?;
        // As with Remove, final directory entries have POSIX unlink semantics.
        // Never delete a source already observed to have been replaced.
        for (path, _, original, parent_identity) in resolved {
            // Reopen one parent at a time rather than retaining an unbounded
            // number of descriptors. Refuse a replacement ancestor, then keep
            // this parent bound through both the entry check and unlink.
            let Ok((parent, name)) = self.parent(&path) else {
                continue;
            };
            let Ok(parent_metadata) = parent.metadata() else {
                continue;
            };
            if Some((parent_metadata.dev(), parent_metadata.ino())) != parent_identity {
                continue;
            }
            let Ok(current) = super::open_at(&parent, &name, libc::O_NOFOLLOW) else {
                continue;
            };
            let Ok(current) = current.metadata() else {
                continue;
            };
            if current.dev() == original.dev() && current.ino() == original.ino() {
                if let Ok(name) = std::ffi::CString::new(name.as_bytes()) {
                    let _ = unsafe { libc::unlinkat(parent.as_raw_fd(), name.as_ptr(), 0) };
                }
            }
        }
        Ok(super::upload::write_info(&destination))
    }
}

fn close(file: File) -> std::io::Result<()> {
    use std::os::fd::IntoRawFd;
    if unsafe { libc::close(file.into_raw_fd()) } == 0 {
        Ok(())
    } else {
        Err(std::io::Error::last_os_error())
    }
}

fn io_error(operation: &str, error: std::io::Error) -> UploadError {
    if error.raw_os_error() == Some(libc::ENOSPC) {
        UploadError::new(
            StatusCode::INSUFFICIENT_STORAGE,
            "not enough disk space available",
        )
    } else {
        UploadError::new(
            StatusCode::INTERNAL_SERVER_ERROR,
            format!("error {operation}: {}", super::io_detail(&error)),
        )
    }
}

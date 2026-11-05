// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use std::ffi::{CString, OsStr};
use std::fs::{File, OpenOptions};
use std::io::{BufRead, BufReader, Read, Write};
use std::os::fd::AsRawFd;
use std::os::unix::ffi::OsStrExt;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use axum::body::{Body, Bytes};
use futures::StreamExt;
use http::StatusCode;
use tokio::sync::mpsc;

use super::{lookup_error, open_at, owned_fd, Context, FilesystemService};
use crate::error::DomainError;
use crate::runtime::{RuntimeState, UserDatabase};

pub(super) const CHUNK: usize = 32 * 1024;

pub(super) type Result<T> = std::result::Result<T, UploadError>;

pub(crate) struct UploadError {
    pub status: StatusCode,
    pub message: String,
}
impl UploadError {
    pub(crate) fn new(status: StatusCode, message: impl Into<String>) -> Self {
        Self {
            status,
            message: message.into(),
        }
    }
}
impl From<StatusCode> for UploadError {
    fn from(status: StatusCode) -> Self {
        Self::new(status, "upload failed")
    }
}

pub(crate) fn status(error: DomainError) -> StatusCode {
    match error {
        DomainError::ResourceExhausted(ref message) if message == "filesystem jobs are full" => {
            StatusCode::TOO_MANY_REQUESTS
        }
        DomainError::ResourceExhausted(ref message) if message == "filesystem space exhausted" => {
            StatusCode::INSUFFICIENT_STORAGE
        }
        DomainError::ResourceExhausted(_) => StatusCode::SERVICE_UNAVAILABLE,
        _ => error.http_status(),
    }
}

pub(super) fn io_status(error: std::io::Error) -> StatusCode {
    if error.raw_os_error() == Some(libc::EISDIR) {
        return StatusCode::BAD_REQUEST;
    }
    if matches!(error.raw_os_error(), Some(libc::ENOSPC | libc::EDQUOT)) {
        StatusCode::INSUFFICIENT_STORAGE
    } else {
        status(lookup_error(error))
    }
}

pub(crate) struct Upload {
    pub path: Option<String>,
    pub boundary: Result<Option<String>>,
    pub gzip: bool,
    pub body: Body,
}

impl FilesystemService {
    pub(crate) async fn upload(
        &self,
        request: Upload,
        username: Option<String>,
        snapshot: Arc<RuntimeState>,
        users: UserDatabase,
    ) -> Result<Vec<serde_json::Value>> {
        let Upload {
            path,
            boundary,
            gzip,
            body,
        } = request;
        let (sender, receiver) = mpsc::channel(1);
        let job = self
            .spawn(username, snapshot, users, move |context| {
                Ok((|| {
                    let wire = BodyReader {
                        receiver,
                        current: Bytes::new(),
                        ended: false,
                        cancel: context.cancel.clone(),
                    };
                    let mut input: Box<dyn Read> = if gzip {
                        Box::new(Gzip::new(BufReader::with_capacity(CHUNK, wire))?)
                    } else {
                        Box::new(wire)
                    };
                    if let Some(boundary) = boundary? {
                        return super::multipart::upload(
                            &context,
                            &mut input,
                            &boundary,
                            path.as_deref(),
                        );
                    }
                    let raw_path = path.as_deref().ok_or_else(|| {
                        UploadError::new(
                            StatusCode::BAD_REQUEST,
                            "path query parameter is required for raw body upload",
                        )
                    })?;
                    let path = context.destructive_path(raw_path).map_err(status)?;
                    let mut file = context.upload_file(&path)?;
                    let mut buffer = [0; CHUNK];
                    loop {
                        context.check().map_err(status)?;
                        let count = input.read(&mut buffer).map_err(body_status)?;
                        if count == 0 {
                            break;
                        }
                        context.check().map_err(status)?;
                        let mut bytes = &buffer[..count];
                        while !bytes.is_empty() {
                            context.check().map_err(status)?;
                            match file.write(bytes) {
                                Ok(0) => return Err(StatusCode::INTERNAL_SERVER_ERROR.into()),
                                Ok(count) => bytes = &bytes[count..],
                                Err(error) if error.kind() == std::io::ErrorKind::Interrupted => {
                                    continue
                                }
                                Err(error) => return Err(io_status(error).into()),
                            }
                        }
                    }
                    context.check().map_err(status)?;
                    Ok(vec![write_info(&path)])
                })())
            })
            .map_err(status)?;
        let mut input = body.into_data_stream();
        let wait = job.wait();
        tokio::pin!(wait);
        let pump = async move {
            while let Some(chunk) = input.next().await {
                let chunk = chunk.map_err(|_| StatusCode::BAD_REQUEST)?;
                for part in chunk.chunks(CHUNK) {
                    if sender.send(Bytes::copy_from_slice(part)).await.is_err() {
                        return Ok(());
                    }
                }
            }
            let _ = sender.send(Bytes::new()).await;
            Ok::<_, UploadError>(())
        };
        tokio::pin!(pump);
        tokio::select! {
            result = &mut wait => result.map_err(status)?,
            result = &mut pump => { result?; wait.await.map_err(status)? }
        }
    }
}

pub(super) fn write_info(path: &Path) -> serde_json::Value {
    serde_json::json!({"name": path.file_name().unwrap_or_default().to_string_lossy(), "path": path.to_string_lossy(), "type": "file"})
}

impl Context {
    pub(super) fn upload_file(&self, path: &Path) -> Result<File> {
        let (parent_path, name) = super::split_path(path);
        let (mut parent, _) = self.directories(parent_path).map_err(status)?;
        let mut name = name;
        for _ in 0..40 {
            self.check().map_err(status)?;
            // Let the kernel follow existing targets, including procfs magic
            // links whose readlink text is not a usable path (e.g. deleted FDs).
            // Only a missing target needs a bound dangling-link creation walk.
            let object = match open_at(&parent, &name, 0) {
                Err(error) if error.raw_os_error() == Some(libc::ENOENT) => {
                    open_at(&parent, &name, libc::O_NOFOLLOW)
                }
                result => result,
            };
            match object {
                Ok(object) => {
                    let metadata = object.metadata().map_err(io_status)?;
                    if metadata.file_type().is_symlink() {
                        self.check().map_err(status)?;
                        let mut bytes = [0; 4097];
                        let count = unsafe {
                            libc::readlinkat(
                                object.as_raw_fd(),
                                c"".as_ptr(),
                                bytes.as_mut_ptr().cast(),
                                bytes.len(),
                            )
                        };
                        if count < 0 {
                            return Err(io_status(std::io::Error::last_os_error()).into());
                        }
                        if count as usize == bytes.len() {
                            return Err(StatusCode::BAD_REQUEST.into());
                        }
                        let link = Path::new(OsStr::from_bytes(&bytes[..count as usize]));
                        // Relative link targets walk from the bound parent, never its old name.
                        let target = PathBuf::from(format!("/proc/self/fd/{}", parent.as_raw_fd()))
                            .join(link);
                        let (parent_path, next_name) = super::split_path(&target);
                        let (next_parent, _) = self.directories(parent_path).map_err(status)?;
                        parent = next_parent;
                        name = next_name;
                        continue;
                    }
                    if metadata.is_dir() {
                        return Err(StatusCode::BAD_REQUEST.into());
                    }
                    self.check().map_err(status)?;
                    let file = OpenOptions::new()
                        .write(true)
                        .open(format!("/proc/self/fd/{}", object.as_raw_fd()))
                        .map_err(|error| {
                            if metadata.is_file() {
                                return UploadError::from(io_status(error));
                            }
                            let detail = error.to_string().to_lowercase();
                            let detail = detail.split(" (os error ").next().unwrap_or(&detail);
                            UploadError::new(
                                StatusCode::INTERNAL_SERVER_ERROR,
                                format!("error opening file: open {}: {detail}", path.display()),
                            )
                        })?;
                    self.check().map_err(status)?;
                    if unsafe { libc::fchown(file.as_raw_fd(), self.user.uid, self.user.gid) } != 0
                    {
                        return Err(io_status(std::io::Error::last_os_error()).into());
                    }
                    self.check().map_err(status)?;
                    // O_TRUNC is ignored for devices and FIFOs by the upstream
                    // open. Only regular files require explicit truncation here.
                    if metadata.is_file() {
                        file.set_len(0).map_err(io_status)?;
                    }
                    #[cfg(test)]
                    crate::filesystem::snapshot_test::file_barrier("Truncated", path, &self.cancel)
                        .map_err(status)?;
                    return Ok(file);
                }
                Err(error) if error.raw_os_error() == Some(libc::ENOENT) => {
                    self.prepare_creation().map_err(status)?;
                    let name_c =
                        CString::new(name.as_bytes()).map_err(|_| StatusCode::BAD_REQUEST)?;
                    self.check().map_err(status)?;
                    let file = owned_fd(unsafe {
                        libc::openat(
                            parent.as_raw_fd(),
                            name_c.as_ptr(),
                            libc::O_WRONLY
                                | libc::O_CREAT
                                | libc::O_EXCL
                                | libc::O_NOFOLLOW
                                | libc::O_CLOEXEC,
                            0o644,
                        )
                    });
                    let file = match file {
                        Ok(file) => file,
                        Err(error) if error.raw_os_error() == Some(libc::EEXIST) => continue,
                        Err(error) => return Err(io_status(error).into()),
                    };
                    self.check().map_err(status)?;
                    if unsafe { libc::fchown(file.as_raw_fd(), self.user.uid, self.user.gid) } != 0
                    {
                        return Err(io_status(std::io::Error::last_os_error()).into());
                    }
                    self.check().map_err(status)?;
                    if unsafe { libc::fchmod(file.as_raw_fd(), 0o644) } != 0 {
                        return Err(io_status(std::io::Error::last_os_error()).into());
                    }
                    return Ok(file);
                }
                Err(error) => return Err(io_status(error).into()),
            }
        }
        Err(StatusCode::BAD_REQUEST.into())
    }
}

struct BodyReader {
    receiver: mpsc::Receiver<Bytes>,
    current: Bytes,
    ended: bool,
    cancel: Arc<std::sync::atomic::AtomicBool>,
}
impl Read for BodyReader {
    fn read(&mut self, output: &mut [u8]) -> std::io::Result<usize> {
        if output.is_empty() {
            return Ok(0);
        }
        if self.cancel.load(std::sync::atomic::Ordering::Acquire) {
            return Err(std::io::ErrorKind::Interrupted.into());
        }
        if self.ended {
            return Ok(0);
        }
        while self.current.is_empty() {
            match self.receiver.blocking_recv() {
                Some(bytes) if bytes.is_empty() => {
                    self.ended = true;
                    return Ok(0);
                }
                Some(bytes) => self.current = bytes,
                None => return Err(std::io::ErrorKind::UnexpectedEof.into()),
            }
        }
        let count = output.len().min(self.current.len());
        output[..count].copy_from_slice(&self.current.split_to(count));
        Ok(count)
    }
}

#[derive(Debug, thiserror::Error)]
#[error("upload exceeds limit")]
struct LimitExceeded;

pub(super) fn body_status(error: std::io::Error) -> UploadError {
    if error
        .get_ref()
        .is_some_and(|error| error.is::<LimitExceeded>())
    {
        StatusCode::PAYLOAD_TOO_LARGE.into()
    } else {
        let detail = if error.kind() == std::io::ErrorKind::UnexpectedEof {
            "unexpected EOF"
        } else if error.to_string().contains("checksum") {
            "gzip: invalid checksum"
        } else {
            "gzip: invalid header"
        };
        UploadError::new(
            StatusCode::INTERNAL_SERVER_ERROR,
            format!("error writing file: {detail}"),
        )
    }
}

// Each gzip member has bounded optional metadata. Payload bytes remain streaming
// without a daemon-specific file-size limit.
struct Gzip<R> {
    decoder: Option<flate2::bufread::GzDecoder<std::io::Take<R>>>,
}
impl<R: BufRead> Gzip<R> {
    fn new(mut input: R) -> Result<Self> {
        let empty = input.fill_buf().map_err(body_status)?.is_empty();
        let decoder = Self::member(input).map_err(|error| {
            if error
                .get_ref()
                .is_some_and(|error| error.is::<LimitExceeded>())
            {
                return StatusCode::PAYLOAD_TOO_LARGE.into();
            }
            let detail = if empty {
                "EOF"
            } else if error.kind() == std::io::ErrorKind::UnexpectedEof {
                "unexpected EOF"
            } else {
                "gzip: invalid header"
            };
            UploadError::new(
                StatusCode::BAD_REQUEST,
                format!("error decompressing request body: failed to create gzip reader: {detail}"),
            )
        })?;
        Ok(Self {
            decoder: Some(decoder),
        })
    }
    fn member(input: R) -> std::io::Result<flate2::bufread::GzDecoder<std::io::Take<R>>> {
        let mut decoder = flate2::bufread::GzDecoder::new(input.take(16 * 1024));
        if decoder.header().is_none() {
            if decoder.get_ref().limit() == 0 {
                return Err(std::io::Error::other(LimitExceeded));
            }
            return match decoder.read(&mut [0]) {
                Err(error) => Err(error),
                Ok(_) => Err(std::io::ErrorKind::InvalidData.into()),
            };
        }
        decoder.get_mut().set_limit(u64::MAX);
        Ok(decoder)
    }
}
impl<R: BufRead> Read for Gzip<R> {
    fn read(&mut self, output: &mut [u8]) -> std::io::Result<usize> {
        if output.is_empty() {
            return Ok(0);
        }
        while let Some(decoder) = self.decoder.as_mut() {
            let count = decoder.read(output)?;
            if count != 0 {
                return Ok(count);
            }
            let mut input = self.decoder.take().unwrap().into_inner().into_inner();
            if input.fill_buf()?.is_empty() {
                return Ok(0);
            }
            self.decoder = Some(Self::member(input)?);
        }
        Ok(0)
    }
}

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
const RAW_WRITE_MAX: usize = 128 * 1024;

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
                    let mut input = if gzip {
                        Input::Gzip(Box::new(Gzip::new(BufReader::with_capacity(CHUNK, wire))?))
                    } else {
                        Input::Raw(wire)
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
                    match input {
                        Input::Raw(mut wire) => {
                            while let Some(bytes) = wire.next_chunk().map_err(body_status)? {
                                write_bytes(&context, &mut file, &bytes)?;
                            }
                        }
                        Input::Gzip(mut input) => {
                            let mut buffer = [0; CHUNK];
                            loop {
                                context.check().map_err(status)?;
                                let count = input.read(&mut buffer).map_err(body_status)?;
                                if count == 0 {
                                    break;
                                }
                                write_bytes(&context, &mut file, &buffer[..count])?;
                            }
                        }
                    }
                    context.check().map_err(status)?;
                    Ok(vec![write_info(&path)])
                })())
            })
            .map_err(status)?;
        let wait = job.wait();
        tokio::pin!(wait);
        let pump = pump_body(body, sender);
        tokio::pin!(pump);
        tokio::select! {
            result = &mut wait => result.map_err(status)?,
            result = &mut pump => { result?; wait.await.map_err(status)? }
        }
    }
}

async fn pump_body(body: Body, sender: mpsc::Sender<Bytes>) -> Result<()> {
    let mut input = body.into_data_stream();
    while let Some(chunk) = input.next().await {
        let chunk = chunk.map_err(|_| StatusCode::BAD_REQUEST)?;
        // Only the final marker is EOF. Forward whole nonempty frames without
        // copying or imposing extra cross-thread handoffs. A frame (or slice)
        // can retain a larger backing allocation; queue length is not a byte cap.
        if !chunk.is_empty() && sender.send(chunk).await.is_err() {
            return Ok(());
        }
    }
    // Channel closure without this marker is an interrupted body. The caller
    // still waits for the file job even if the receiver has already failed.
    let _ = sender.send(Bytes::new()).await;
    Ok(())
}

pub(super) fn write_bytes(context: &Context, file: &mut impl Write, bytes: &[u8]) -> Result<()> {
    for mut pending in bytes.chunks(RAW_WRITE_MAX) {
        while !pending.is_empty() {
            context.check().map_err(status)?;
            match file.write(pending) {
                Ok(0) => return Err(StatusCode::INTERNAL_SERVER_ERROR.into()),
                Ok(count) => pending = &pending[count..],
                Err(error) if error.kind() == std::io::ErrorKind::Interrupted => continue,
                Err(error) => return Err(io_status(error).into()),
            }
            context.check().map_err(status)?;
        }
    }
    Ok(())
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

impl BodyReader {
    fn next_chunk(&mut self) -> std::io::Result<Option<Bytes>> {
        if self.cancel.load(std::sync::atomic::Ordering::Acquire) {
            return Err(std::io::ErrorKind::Interrupted.into());
        }
        if self.ended {
            return Ok(None);
        }
        match self.receiver.blocking_recv() {
            Some(bytes) if bytes.is_empty() => {
                self.ended = true;
                Ok(None)
            }
            Some(bytes) => Ok(Some(bytes)),
            None => Err(std::io::ErrorKind::UnexpectedEof.into()),
        }
    }
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
            match self.next_chunk()? {
                None => return Ok(0),
                Some(bytes) => self.current = bytes,
            }
        }
        let count = output.len().min(self.current.len());
        output[..count].copy_from_slice(&self.current.split_to(count));
        Ok(count)
    }
}

enum Input {
    Raw(BodyReader),
    Gzip(Box<Gzip<BufReader<BodyReader>>>),
}

impl Read for Input {
    fn read(&mut self, output: &mut [u8]) -> std::io::Result<usize> {
        match self {
            Self::Raw(input) => input.read(output),
            Self::Gzip(input) => input.read(output),
        }
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

    fn reader(receiver: mpsc::Receiver<Bytes>) -> BodyReader {
        BodyReader {
            receiver,
            current: Bytes::new(),
            ended: false,
            cancel: Arc::new(AtomicBool::new(false)),
        }
    }

    #[tokio::test]
    async fn frames_empty_frames_and_tails_preserve_bytes() {
        for size in [
            0,
            1,
            CHUNK - 1,
            CHUNK,
            CHUNK + 1,
            RAW_WRITE_MAX - 1,
            RAW_WRITE_MAX,
            RAW_WRITE_MAX + 1,
            4 * 1024 * 1024,
        ] {
            let expected: Vec<_> = (0..size).map(|i| i as u8).collect();
            for frame_size in [1, CHUNK - 1, CHUNK + 1, 4 * 1024 * 1024] {
                // Exercise one-byte frames at the boundary without producing
                // millions of test-only frame allocations for the 4 MiB case.
                if size > CHUNK + 1 && frame_size == 1 {
                    continue;
                }
                let mut frames = vec![Ok::<_, std::io::Error>(Bytes::new())];
                for bytes in expected.chunks(frame_size) {
                    frames.push(Ok(Bytes::copy_from_slice(bytes)));
                    frames.push(Ok(Bytes::new()));
                }
                let (sender, receiver) = mpsc::channel(1);
                let consumer = tokio::task::spawn_blocking(move || {
                    let mut input = reader(receiver);
                    let mut actual = Vec::new();
                    input.read_to_end(&mut actual).unwrap();
                    assert_eq!(input.read(&mut [0]).unwrap(), 0);
                    actual
                });
                assert!(
                    pump_body(Body::from_stream(futures::stream::iter(frames)), sender)
                        .await
                        .is_ok()
                );
                assert_eq!(consumer.await.unwrap(), expected);
            }
        }
    }

    #[tokio::test]
    async fn pump_forwards_original_frame_and_tail_then_one_eof() {
        let frame = Bytes::from(vec![5; 4 * 1024 * 1024]);
        let pointer = frame.as_ptr();
        let (sender, mut receiver) = mpsc::channel(1);
        let body = Body::from_stream(futures::stream::iter([
            Ok::<_, std::io::Error>(Bytes::new()),
            Ok(frame),
            Ok(Bytes::new()),
            Ok(Bytes::from_static(b"tail")),
        ]));
        let mut pump = Box::pin(pump_body(body, sender));
        assert!(futures::poll!(&mut pump).is_pending());
        let frame = receiver.try_recv().unwrap();
        assert_eq!(frame.len(), 4 * 1024 * 1024);
        assert_eq!(frame.as_ptr(), pointer);
        assert!(futures::poll!(&mut pump).is_pending());
        assert_eq!(receiver.try_recv().unwrap(), b"tail".as_slice());
        assert!(pump.await.is_ok());
        assert!(receiver.recv().await.unwrap().is_empty());
        assert!(receiver.recv().await.is_none());
    }

    struct Writer {
        steps: std::collections::VecDeque<std::result::Result<usize, i32>>,
        attempts: Vec<usize>,
        written: Vec<u8>,
        cancel: Option<Arc<AtomicBool>>,
    }

    impl Writer {
        fn new(steps: impl IntoIterator<Item = std::result::Result<usize, i32>>) -> Self {
            Self {
                steps: steps.into_iter().collect(),
                attempts: Vec::new(),
                written: Vec::new(),
                cancel: None,
            }
        }
    }

    impl Write for Writer {
        fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
            self.attempts.push(bytes.len());
            let count = self
                .steps
                .pop_front()
                .unwrap_or(Ok(bytes.len()))
                .map_err(std::io::Error::from_raw_os_error)?;
            assert!(count <= bytes.len());
            self.written.extend_from_slice(&bytes[..count]);
            if let Some(cancel) = self.cancel.take() {
                cancel.store(true, Ordering::Release);
            }
            Ok(count)
        }
        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }

    async fn context() -> Context {
        Context {
            snapshot: crate::runtime::RuntimeStateStore::new().snapshot().await,
            user: crate::runtime::ProcessUser {
                name: "root".into(),
                uid: 0,
                gid: 0,
                groups: vec![0],
                home: "/".into(),
            },
            cancel: Arc::new(AtomicBool::new(false)),
        }
    }

    #[tokio::test]
    async fn writes_bound_syscalls_and_retry_short_writes_and_eintr() {
        let context = context().await;
        let bytes: Vec<_> = (0..RAW_WRITE_MAX + 9).map(|i| i as u8).collect();
        let mut writer = Writer::new([Err(libc::EINTR), Ok(7), Ok(RAW_WRITE_MAX - 7), Ok(9)]);
        assert!(write_bytes(&context, &mut writer, &bytes).is_ok());
        assert_eq!(writer.written, bytes);
        assert_eq!(
            writer.attempts,
            [RAW_WRITE_MAX, RAW_WRITE_MAX, RAW_WRITE_MAX - 7, 9]
        );
        let mut writer = Writer::new([]);
        assert!(write_bytes(&context, &mut writer, &[]).is_ok());
        assert!(writer.attempts.is_empty());
    }

    #[tokio::test]
    async fn writes_keep_prefix_and_error_and_stop_after_zero_or_cancel() {
        for (failure, code) in [
            (Ok(0), StatusCode::INTERNAL_SERVER_ERROR),
            (Err(libc::EIO), StatusCode::INTERNAL_SERVER_ERROR),
            (Err(libc::ENOSPC), StatusCode::INSUFFICIENT_STORAGE),
            (Err(libc::EDQUOT), StatusCode::INSUFFICIENT_STORAGE),
        ] {
            let context = context().await;
            let mut writer = Writer::new([Ok(7), failure]);
            let error = write_bytes(&context, &mut writer, b"prefix-and-tail")
                .err()
                .unwrap();
            assert_eq!(error.status, code);
            assert_eq!(writer.written, b"prefix-");
            assert_eq!(writer.attempts.len(), 2);
        }
        let context = context().await;
        let mut writer = Writer::new([Ok(7)]);
        writer.cancel = Some(context.cancel.clone());
        let error = write_bytes(&context, &mut writer, b"prefix-and-tail")
            .err()
            .unwrap();
        assert_eq!(error.status, status(DomainError::Cancelled));
        assert_eq!(writer.written, b"prefix-");
        assert_eq!(writer.attempts.len(), 1);
    }

    #[tokio::test]
    async fn multipart_search_preserves_false_candidates_and_split_delimiters() {
        struct Split<'a> {
            remaining: &'a [u8],
            max: usize,
        }
        impl Read for Split<'_> {
            fn read(&mut self, output: &mut [u8]) -> std::io::Result<usize> {
                let count = output.len().min(self.max).min(self.remaining.len());
                output[..count].copy_from_slice(&self.remaining[..count]);
                self.remaining = &self.remaining[count..];
                Ok(count)
            }
        }
        let context = context().await;
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("multipart-search");
        let payload = [
            b"\r\n--boundaryX\r\n--bound\r\n--boundary \tX".repeat(1000),
            (0..=255).collect::<Vec<u8>>().repeat(100),
        ]
        .concat();
        for newline in ["\r\n", "\n"] {
            let body = [
                format!("--boundary{newline}Content-Disposition: form-data; name=\"file\"{newline}{newline}").into_bytes(),
                payload.clone(),
                format!("{newline}--boundary \t{newline}Content-Disposition: form-data; name=\"ignored\"{newline}{newline}ignored{newline}--boundary--{newline}").into_bytes(),
            ]
            .concat();
            for max in [1, 7, 31, CHUNK - 1, CHUNK, CHUNK + 1] {
                let mut input = Split {
                    remaining: &body,
                    max,
                };
                let entries = super::super::multipart::upload(
                    &context,
                    &mut input,
                    "boundary",
                    path.to_str(),
                )
                .ok()
                .unwrap();
                assert_eq!(entries.len(), 1, "newline={newline:?}, max={max}");
                assert_eq!(
                    std::fs::read(&path).unwrap(),
                    payload,
                    "newline={newline:?}, max={max}"
                );
                assert!(input.remaining.is_empty());
            }
        }
    }

    #[tokio::test]
    async fn short_prefix_arrives_before_next_frame_or_eof() {
        let (frames, input) = mpsc::channel::<std::io::Result<Bytes>>(1);
        let (sender, receiver) = mpsc::channel(1);
        let pump = tokio::spawn(pump_body(
            Body::from_stream(tokio_stream::wrappers::ReceiverStream::new(input)),
            sender,
        ));
        frames
            .send(Ok(Bytes::from_static(b"prefix")))
            .await
            .unwrap();
        let consumer = tokio::task::spawn_blocking(move || {
            let mut input = reader(receiver);
            let mut buffer = [0; CHUNK];
            assert_eq!(input.read(&mut buffer).unwrap(), 6);
            assert_eq!(&buffer[..6], b"prefix");
            input
        });
        let mut input = tokio::time::timeout(std::time::Duration::from_secs(5), consumer)
            .await
            .unwrap()
            .unwrap();
        drop(frames);
        assert!(pump.await.unwrap().is_ok());
        tokio::task::spawn_blocking(move || assert_eq!(input.read(&mut [0]).unwrap(), 0))
            .await
            .unwrap();
    }

    #[tokio::test]
    async fn body_error_closes_without_normal_eof() {
        let (sender, receiver) = mpsc::channel(1);
        let body = Body::from_stream(futures::stream::iter([
            Ok(Bytes::from_static(b"prefix")),
            Err(std::io::Error::other("body failed")),
        ]));
        let consumer = tokio::task::spawn_blocking(move || {
            let mut actual = Vec::new();
            let error = reader(receiver).read_to_end(&mut actual).unwrap_err();
            assert_eq!(actual, b"prefix");
            assert_eq!(error.kind(), std::io::ErrorKind::UnexpectedEof);
        });
        assert_eq!(
            pump_body(body, sender).await.err().unwrap().status,
            StatusCode::BAD_REQUEST
        );
        consumer.await.unwrap();
    }

    #[test]
    fn channel_close_is_not_eof_and_cancel_stops_current_bytes() {
        let (sender, receiver) = mpsc::channel(1);
        drop(sender);
        let mut input = reader(receiver);
        assert_eq!(
            input.read(&mut [0]).unwrap_err().kind(),
            std::io::ErrorKind::UnexpectedEof
        );
        input.current = Bytes::from_static(b"unconsumed");
        input.cancel.store(true, Ordering::Release);
        assert_eq!(
            input.read(&mut [0]).unwrap_err().kind(),
            std::io::ErrorKind::Interrupted
        );
    }

    #[tokio::test]
    async fn dropping_request_wakes_file_reader_and_cancels_job() {
        let service = FilesystemService::default();
        let snapshot = crate::runtime::RuntimeStateStore::new().snapshot().await;
        let (sender, receiver) = mpsc::channel(1);
        let (entered, started) = tokio::sync::oneshot::channel();
        let (finished, stopped) = tokio::sync::oneshot::channel();
        let job = service
            .spawn(None, snapshot, UserDatabase::system(), move |context| {
                let mut input = reader(receiver);
                input.cancel = context.cancel.clone();
                let _ = entered.send(());
                let error = input.read(&mut [0]).unwrap_err();
                let _ = finished.send((error.kind(), context.cancel.load(Ordering::Acquire)));
                Ok(())
            })
            .unwrap_or_else(|_| panic!("spawn file reader"));
        let mut wait = Box::pin(job.wait());
        let mut pump = Box::pin(pump_body(
            Body::from_stream(futures::stream::pending::<std::io::Result<Bytes>>()),
            sender,
        ));
        assert!(futures::poll!(&mut wait).is_pending());
        assert!(futures::poll!(&mut pump).is_pending());
        started.await.unwrap();
        // Same owned futures as upload: cancellation flag then sender closure
        // must release a reader even if it already entered blocking_recv.
        drop(wait);
        drop(pump);
        let (kind, cancelled) = tokio::time::timeout(std::time::Duration::from_secs(5), stopped)
            .await
            .unwrap()
            .unwrap();
        assert!(cancelled);
        assert!(matches!(
            kind,
            std::io::ErrorKind::Interrupted | std::io::ErrorKind::UnexpectedEof
        ));
    }

    #[tokio::test]
    async fn early_file_error_ends_pending_body_and_preserves_error() {
        let service = FilesystemService::default();
        let snapshot = crate::runtime::RuntimeStateStore::new().snapshot().await;
        let request = Upload {
            path: Some("/".into()),
            boundary: Ok(None),
            gzip: false,
            body: Body::from_stream(futures::stream::pending::<std::io::Result<Bytes>>()),
        };
        let result = tokio::time::timeout(
            std::time::Duration::from_secs(5),
            service.upload(request, None, snapshot, UserDatabase::system()),
        )
        .await
        .unwrap();
        assert_eq!(result.err().unwrap().status, StatusCode::BAD_REQUEST);
    }

    struct Owner {
        data: Vec<u8>,
        live: Arc<AtomicUsize>,
    }
    impl AsRef<[u8]> for Owner {
        fn as_ref(&self) -> &[u8] {
            &self.data
        }
    }
    impl Drop for Owner {
        fn drop(&mut self) {
            self.live.fetch_sub(self.data.len(), Ordering::SeqCst);
        }
    }
    fn owned(size: usize, live: &Arc<AtomicUsize>) -> Bytes {
        live.fetch_add(size, Ordering::SeqCst);
        Bytes::from_owner(Owner {
            data: vec![7; size],
            live: live.clone(),
        })
    }

    #[tokio::test]
    async fn backpressure_consumer_failure_and_cancel_release_backing() {
        for cancel in [false, true] {
            let live = Arc::new(AtomicUsize::new(0));
            let pulls = Arc::new(AtomicUsize::new(0));
            let frames = futures::stream::repeat_with({
                let live = live.clone();
                let pulls = pulls.clone();
                move || {
                    pulls.fetch_add(1, Ordering::SeqCst);
                    Ok::<_, std::io::Error>(owned(4 * 1024 * 1024, &live))
                }
            });
            let (sender, mut receiver) = mpsc::channel(1);
            let mut pump = Box::pin(pump_body(Body::from_stream(frames), sender));
            assert!(futures::poll!(&mut pump).is_pending());
            assert_eq!(pulls.load(Ordering::SeqCst), 2);
            assert_eq!(live.load(Ordering::SeqCst), 8 * 1024 * 1024);
            let current = receiver.try_recv().unwrap();
            assert_eq!(current.len(), 4 * 1024 * 1024);
            assert!(futures::poll!(&mut pump).is_pending());
            assert_eq!(receiver.len(), 1);
            assert_eq!(pulls.load(Ordering::SeqCst), 3);
            assert_eq!(live.load(Ordering::SeqCst), 12 * 1024 * 1024);
            if cancel {
                drop(pump); // Request future cancellation while send is pending.
                assert_eq!(live.load(Ordering::SeqCst), 8 * 1024 * 1024);
                drop(receiver);
            } else {
                drop(receiver); // File worker failed before consuming the body.
                assert!(pump.await.is_ok());
            }
            // The consumer still owns its entire 4 MiB frame.
            assert_eq!(live.load(Ordering::SeqCst), 4 * 1024 * 1024);
            drop(current);
            assert_eq!(live.load(Ordering::SeqCst), 0);
        }
    }

    #[tokio::test]
    async fn distinct_backings_live_in_current_queue_and_producer_until_completion() {
        let live = Arc::new(AtomicUsize::new(0));
        let pulls = Arc::new(AtomicUsize::new(0));
        let frames = futures::stream::iter([4 * 1024 * 1024; 3]).map({
            let live = live.clone();
            let pulls = pulls.clone();
            move |size| {
                pulls.fetch_add(1, Ordering::SeqCst);
                // A short source view can itself retain a much larger owner.
                Ok::<_, std::io::Error>(owned(size, &live).slice(..CHUNK))
            }
        });
        let (sender, mut receiver) = mpsc::channel(1);
        let mut pump = Box::pin(pump_body(Body::from_stream(frames), sender));
        assert!(futures::poll!(&mut pump).is_pending());
        let current = receiver.try_recv().unwrap();
        assert!(futures::poll!(&mut pump).is_pending());
        assert_eq!(pulls.load(Ordering::SeqCst), 3);
        assert_eq!(live.load(Ordering::SeqCst), 12 * 1024 * 1024);
        drop(current);
        assert_eq!(live.load(Ordering::SeqCst), 8 * 1024 * 1024);
        drop(receiver.try_recv().unwrap());
        assert!(futures::poll!(&mut pump).is_pending()); // Waiting to send EOF.
        assert_eq!(live.load(Ordering::SeqCst), 4 * 1024 * 1024);
        drop(receiver.try_recv().unwrap());
        assert!(pump.await.is_ok());
        assert!(receiver.recv().await.unwrap().is_empty());
        assert_eq!(live.load(Ordering::SeqCst), 0);
    }
}

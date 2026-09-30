// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use std::io::{Read, Seek, SeekFrom, Write};
use std::os::fd::AsRawFd;
use std::os::unix::fs::MetadataExt;
use std::sync::Arc;

use axum::body::{Body, Bytes};
use axum::response::Response;
use tokio::sync::{mpsc, oneshot};

use super::{invalid, lookup_error, open_at, FilesystemService};
use crate::error::DomainError;
use crate::runtime::{RuntimeState, UserDatabase};

// One queued chunk and one in-flight chunk per admitted file job.
const CHUNK: usize = 32 * 1024;

impl FilesystemService {
    pub(crate) async fn download(
        &self,
        path: String,
        headers: http::HeaderMap,
        username: Option<String>,
        snapshot: Arc<RuntimeState>,
        users: UserDatabase,
    ) -> Result<Response, DomainError> {
        let (metadata_tx, metadata_rx) = oneshot::channel();
        let (sender, receiver) = mpsc::channel(1);
        let job = self.spawn(username, snapshot, users, move |context| {
            let path = context.path(&path, false)?;
            let not_found = |error| match error {
                DomainError::NotFound(_) => DomainError::NotFound(format!("path '{}' does not exist",path.display())),
                other => other,
            };
            let object = match context.parent(&path).and_then(|(parent,name)| {
                context.check()?;
                open_at(&parent,&name,0).map_err(lookup_error)
            }) {
                Ok(object) => object,
                Err(DomainError::FailedPrecondition(message)) if message == "symlink loop" => {
                    let message = format!("error checking if path exists '{}': stat {}: too many levels of symbolic links",path.display(),path.display());
                    metadata_tx.send(Err(crate::transport::rest::file_error(http::StatusCode::INTERNAL_SERVER_ERROR,message))).map_err(|_| DomainError::Cancelled)?;
                    return Ok(());
                }
                Err(error) => return Err(not_found(error)),
            };
            let metadata = object.metadata().map_err(lookup_error)?;
            if metadata.is_dir() { return Err(invalid(&format!("path '{}' is a directory",path.display()))); }
            let gzip = match crate::transport::encoding::negotiate(&headers) {
                Ok(gzip) => gzip,
                Err(message) => {
                    let mut response = crate::transport::rest::file_error(http::StatusCode::NOT_ACCEPTABLE,message);
                    if message.starts_with("identity") { response.headers_mut().insert("vary",http::HeaderValue::from_static("Accept-Encoding")); }
                    metadata_tx.send(Err(response)).map_err(|_| DomainError::Cancelled)?;
                    return Ok(());
                }
            };
            context.check()?;
            // This procfs magic link reopens the bound object, even after unlink.
            // Never reopen the caller's path after binding the object.
            let file = match std::fs::File::open(format!("/proc/self/fd/{}", object.as_raw_fd())) {
                Ok(file) => file,
                Err(error) => {
                    let detail = error.to_string().to_lowercase();
                    let detail = detail.split(" (os error ").next().unwrap_or(&detail);
                    let message = format!("error opening file '{}': open {}: {}", path.display(), path.display(), detail);
                    metadata_tx.send(Err(crate::transport::rest::file_error(http::StatusCode::INTERNAL_SERVER_ERROR,message))).map_err(|_| DomainError::Cancelled)?;
                    return Ok(());
                }
            };
            context.check()?;
            let seconds = metadata.mtime();
            let modified = (seconds != 0 || metadata.mtime_nsec() != 0).then(||
                jiff::Timestamp::from_second(seconds).ok().map(|t| t.strftime("%a, %d %b %Y %H:%M:%S GMT").to_string())
            ).flatten();
            let disposition = super::download_metadata::disposition(&path);
            let mut response = Response::builder()
                .header("content-disposition", disposition)
                .header("vary", "Accept-Encoding");
            if !gzip {
                if let Some(status) = precondition(&headers, modified.as_ref().map(|_| seconds)) {
                    response = response.status(status);
                    if let Some(modified) = modified { response = response.header("last-modified", modified); }
                    if status == 412 { response = response.header("content-length",0); }
                    metadata_tx.send(Ok(response)).map_err(|_| DomainError::Cancelled)?;
                    return Ok(());
                }
            }
            let mut file = file;
            let extension = super::download_metadata::extension(&path);
            let mut prefix = Vec::with_capacity(512);
            if !gzip && extension.is_none() {
                while prefix.len() < 512 {
                    context.check()?;
                    let mut buffer = [0; 512];
                    match file.read(&mut buffer[..512 - prefix.len()]) {
                        Ok(0) => break,
                        Ok(count) => prefix.extend_from_slice(&buffer[..count]),
                        Err(error) => {
                            tracing::error!(%error, "file download sniff failed");
                            break;
                        }
                    }
                }
            }
            let length = if gzip { None } else {
                match file.seek(SeekFrom::Start(0)).and_then(|_| file.seek(SeekFrom::End(0))).and_then(|size| { file.seek(SeekFrom::Start(0))?; Ok(size) }) {
                    Ok(size) => Some(size),
                    Err(_) => {
                        response = response.status(500).header("content-type","text/plain; charset=utf-8")
                            .header("x-content-type-options","nosniff").header("content-length",18);
                        metadata_tx.send(Ok(response)).map_err(|_| DomainError::Cancelled)?;
                        sender.blocking_send(Bytes::from_static(b"seeker can't seek\n")).map_err(|_| DomainError::Cancelled)?;
                        return Ok(());
                    }
                }
            };
            let mut file = file.take(if gzip {
                u64::MAX
            } else {
                length.unwrap_or(u64::MAX)
            });
            context.check()?;
            let mime = if gzip { extension.unwrap_or_else(|| "application/octet-stream".into()) } else { super::download_metadata::mime(&path, &prefix) };
            let range_header = header(&headers,"range");
            let range_header = if !header(&headers,"if-range").is_empty()
                && http_time(header(&headers,"if-range")) != Some(seconds) { "" } else { range_header };
            let ranges = if !gzip {
                match parse_ranges(range_header, length.unwrap_or(0)) {
                    Ok(ranges) => ranges,
                    Err(overlap) => {
                        let message = if overlap { "invalid range: failed to overlap\n" } else { "invalid range\n" };
                        response = response.status(416).header("x-content-type-options", "nosniff")
                            .header("content-type", "text/plain; charset=utf-8").header("content-length", message.len());
                        if overlap { response = response.header("content-range", format!("bytes */{}", length.unwrap_or(0))); }
                        metadata_tx.send(Ok(response)).map_err(|_| DomainError::Cancelled)?;
                        sender.blocking_send(Bytes::from_static(message.as_bytes())).map_err(|_| DomainError::Cancelled)?;
                        return Ok(());
                    }
                }
            } else { Vec::new() };
            let boundary = if ranges.len() > 1 {
                let mut bytes = [0; 30];
                std::fs::File::open("/dev/urandom").and_then(|mut f| f.read_exact(&mut bytes)).map_err(lookup_error)?;
                bytes.iter().map(|b| format!("{b:02x}")).collect::<String>()
            } else { String::new() };
            let part_header = |index: usize, start: u64, count: u64| {
                format!("{}--{boundary}\r\nContent-Range: {}\r\nContent-Type: {mime}\r\n\r\n",
                    if index == 0 { "" } else { "\r\n" }, content_range(start, count, length.unwrap_or(0)))
            };
            let mut send_length = length;
            if !gzip {
                response = response.header("accept-ranges", "bytes");
                if let Some(modified) = modified { response = response.header("last-modified", modified); }
                if let [(start, count)] = ranges.as_slice() {
                    response = response.status(206).header("content-range", content_range(*start,*count,length.unwrap_or(0)));
                    send_length = Some(*count);
                } else if ranges.len() > 1 {
                    response = response.status(206).header("content-type",format!("multipart/byteranges; boundary={boundary}"));
                    send_length = Some(ranges.iter().enumerate().map(|(i,(start,count))| part_header(i,*start,*count).len() as u64 + count).sum::<u64>() + boundary.len() as u64 + 8);
                }
            }
            if ranges.len() <= 1 { response = response.header("content-type", &mime); }
            if gzip { response = response.header("content-encoding", "gzip"); }
            else if let Some(length) = send_length { response = response.header("content-length",length); }
            let writer = ChunkWriter {
                sender,
                cancel: context.cancel.clone(),
            };
            if gzip {
                let writer = GzipResponse { pending: Some((metadata_tx,response)), prefix: Vec::new(), writer };
                let mut output = flate2::write::GzEncoder::new(writer, flate2::Compression::default());
                copy(&context, &mut file, &mut output)?;
                context.check()?;
                output.finish().map_err(lookup_error)?.finish().map_err(lookup_error)?;
                return Ok(());
            }
            metadata_tx.send(Ok(response)).map_err(|_| DomainError::Cancelled)?;
            if !ranges.is_empty() {
                let mut writer = writer;
                for (i, (start,count)) in ranges.iter().enumerate() {
                    context.check()?;
                    file.get_mut().seek(SeekFrom::Start(*start)).map_err(lookup_error)?;
                    file.set_limit(*count);
                    if ranges.len() > 1 { writer.write_all(part_header(i,*start,*count).as_bytes()).map_err(lookup_error)?; }
                    copy(&context, &mut file, &mut writer)?;
                    if file.limit() != 0 { return Err(DomainError::Internal); }
                }
                if ranges.len() > 1 { writer.write_all(format!("\r\n--{boundary}--\r\n").as_bytes()).map_err(lookup_error)?; }
            } else {
                let mut writer = writer;
                copy(&context, &mut file, &mut writer)?;
            }
            Ok(())
        })?;
        let metadata = match metadata_rx.await {
            Ok(Ok(metadata)) => metadata,
            Ok(Err(response)) => {
                job.wait().await?;
                return Ok(response);
            }
            Err(_) => return Err(job.wait().await.err().unwrap_or(DomainError::Internal)),
        };
        let stream = futures::stream::unfold(
            (receiver, Some(job), true),
            |(mut receiver, mut job, first_poll)| async move {
                if first_poll {
                    // Let HTTP flush the declared response before discovering a
                    // short body, including an I/O fault before the first byte.
                    tokio::task::yield_now().await;
                }
                if let Some(bytes) = receiver.recv().await {
                    return Some((Ok::<_, DomainError>(bytes), (receiver, job, false)));
                }
                if let Some(job_result) = job.take() {
                    if let Err(error) = job_result.wait().await {
                        return Some((Err(error), (receiver, job, false)));
                    }
                }
                None
            },
        );
        metadata
            .body(Body::from_stream(stream))
            .map_err(|_| DomainError::Internal)
    }
}

struct ChunkWriter {
    sender: mpsc::Sender<Bytes>,
    cancel: Arc<std::sync::atomic::AtomicBool>,
}
impl Write for ChunkWriter {
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        if self.cancel.load(std::sync::atomic::Ordering::Acquire) {
            return Err(std::io::ErrorKind::BrokenPipe.into());
        }
        let count = bytes.len().min(CHUNK);
        if count > 0 {
            self.sender
                .blocking_send(Bytes::copy_from_slice(&bytes[..count]))
                .map_err(|_| std::io::Error::from(std::io::ErrorKind::BrokenPipe))?;
        }
        Ok(count)
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

fn copy(
    context: &super::Context,
    input: &mut dyn Read,
    output: &mut dyn Write,
) -> Result<(), DomainError> {
    loop {
        context.check()?;
        let mut buffer = [0; CHUNK];
        let count = match input.read(&mut buffer) {
            Ok(count) => count,
            Err(error) => {
                // Match the upstream HTTP representation; report I/O failure
                // through the existing daemon log instead of a new wire signal.
                tracing::error!(%error, "file download read failed");
                return Ok(());
            }
        };
        if count == 0 {
            return Ok(());
        }
        context.check()?;
        output.write_all(&buffer[..count]).map_err(lookup_error)?;
    }
}

fn content_range(start: u64, count: u64, size: u64) -> String {
    format!(
        "bytes {start}-{}/{size}",
        i128::from(start) + i128::from(count) - 1
    )
}

// The fixed oracle uses Go ServeContent: invalid syntax differs from no overlap,
// and overlapping ranges whose combined size exceeds the file are ignored.
fn parse_ranges(value: &str, size: u64) -> Result<Vec<(u64, u64)>, bool> {
    if value.is_empty() {
        return Ok(Vec::new());
    }
    let value = value.strip_prefix("bytes=").ok_or(false)?;
    let mut ranges = Vec::new();
    let mut no_overlap = false;
    let number = |v: &str| {
        v.parse::<i64>()
            .ok()
            .filter(|v| *v >= 0)
            .map(|v| v as u64)
            .ok_or(false)
    };
    for part in value
        .split(',')
        .map(|v| v.trim_matches([' ', '\t']))
        .filter(|v| !v.is_empty())
    {
        let (start, end) = part.split_once('-').ok_or(false)?;
        let (start, end) = (
            start.trim_matches([' ', '\t']),
            end.trim_matches([' ', '\t']),
        );
        if start.is_empty() {
            let count = number(end)?.min(size);
            ranges.push((size - count, count));
        } else {
            let start = number(start)?;
            if start >= size {
                no_overlap = true;
                continue;
            }
            let end = if end.is_empty() {
                size - 1
            } else {
                number(end)?
            };
            if start > end {
                return Err(false);
            }
            ranges.push((start, end.min(size - 1) - start + 1));
        }
    }
    if ranges.is_empty() && no_overlap && size != 0 {
        return Err(true);
    }
    if ranges
        .iter()
        .map(|(_, count)| u128::from(*count))
        .sum::<u128>()
        > u128::from(size)
    {
        ranges.clear();
    }
    Ok(ranges)
}

fn header<'a>(headers: &'a http::HeaderMap, name: &str) -> &'a str {
    headers
        .get(name)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
}

fn http_time(value: &str) -> Option<i64> {
    let short = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];
    let long = [
        "Monday",
        "Tuesday",
        "Wednesday",
        "Thursday",
        "Friday",
        "Saturday",
        "Sunday",
    ];
    let (format, rest) = if let Some((day, rest)) = value.split_once(", ") {
        if short.contains(&day) {
            ("%d %b %Y %H:%M:%S GMT", rest)
        } else if long.contains(&day) {
            ("%d-%b-%y %H:%M:%S GMT", rest)
        } else {
            return None;
        }
    } else {
        let (day, rest) = value.split_once(' ')?;
        if !short.contains(&day) {
            return None;
        }
        ("%b %e %H:%M:%S %Y", rest)
    };
    // Go parses but does not cross-check the weekday against the calendar date.
    let date = jiff::civil::DateTime::strptime(format, rest).ok()?;
    Some(
        date.to_zoned(jiff::tz::TimeZone::UTC)
            .ok()?
            .timestamp()
            .as_second(),
    )
}

// The oracle never sets an ETag. Only a syntactically reachable wildcard can
// match, but malformed tags must still stop scanning before a later wildcard.
fn wildcard(mut value: &str) -> bool {
    loop {
        value = value.trim_matches([' ', '\t']);
        if let Some(rest) = value.strip_prefix(',') {
            value = rest;
            continue;
        }
        if value.starts_with('*') {
            return true;
        }
        value = value.strip_prefix("W/").unwrap_or(value);
        let Some(rest) = value.strip_prefix('"') else {
            return false;
        };
        let Some(end) = rest.find('"') else {
            return false;
        };
        if !rest[..end]
            .bytes()
            .all(|b| b == 0x21 || (0x23..=0x7e).contains(&b) || b >= 0x80)
        {
            return false;
        }
        value = &rest[end + 1..];
    }
}

fn precondition(headers: &http::HeaderMap, modified: Option<i64>) -> Option<u16> {
    let im = header(headers, "if-match");
    if !im.is_empty() {
        if !wildcard(im) {
            return Some(412);
        }
    } else if modified
        .zip(http_time(header(headers, "if-unmodified-since")))
        .is_some_and(|(m, t)| m > t)
    {
        return Some(412);
    }
    let inm = header(headers, "if-none-match");
    if !inm.is_empty() {
        if wildcard(inm) {
            return Some(304);
        }
    } else if modified
        .zip(http_time(header(headers, "if-modified-since")))
        .is_some_and(|(m, t)| m <= t)
    {
        return Some(304);
    }
    None
}

// Go's HTTP writer supplies Content-Length when the finished compressed body
// fits its 2 KiB response buffer. Larger output immediately becomes streaming.
struct GzipResponse {
    pending: Option<(
        oneshot::Sender<Result<http::response::Builder, Response>>,
        http::response::Builder,
    )>,
    prefix: Vec<u8>,
    writer: ChunkWriter,
}
impl GzipResponse {
    fn start(&mut self, finished: bool) -> std::io::Result<()> {
        if let Some((sender, mut response)) = self.pending.take() {
            if finished {
                response = response.header("content-length", self.prefix.len());
            }
            sender
                .send(Ok(response))
                .map_err(|_| std::io::Error::from(std::io::ErrorKind::BrokenPipe))?;
            self.writer.write_all(&self.prefix)?;
            self.prefix.clear();
        }
        Ok(())
    }
    fn finish(mut self) -> std::io::Result<()> {
        self.start(true)
    }
}
impl Write for GzipResponse {
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        if self.pending.is_some() && self.prefix.len() + bytes.len() <= 2048 {
            self.prefix.extend_from_slice(bytes);
            return Ok(bytes.len());
        }
        self.start(false)?;
        self.writer.write(bytes)
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

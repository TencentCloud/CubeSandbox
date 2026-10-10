// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use std::collections::{HashMap, HashSet};
use std::io::{Read, Write};
use std::path::PathBuf;

use http::StatusCode;

use super::upload::{body_status, io_status, status, write_info, Result, UploadError, CHUNK};
use super::Context;

const HEADER_LIMIT: usize = 16 * 1024;
const PART_LIMIT: usize = 1000;

/// MIME token/quoted-string parameters, shared by Content-Type and disposition.
/// Duplicate parameters and malformed quoting are rejected, never guessed.
pub(crate) fn parameters(value: &str) -> Result<(String, HashMap<String, String>)> {
    let (kind, mut rest) = value.split_once(';').unwrap_or((value, ""));
    let mut parameters = HashMap::new();
    while !rest.trim().is_empty() {
        rest = rest.trim_start();
        let (key, tail) = rest.split_once('=').ok_or(StatusCode::BAD_REQUEST)?;
        let key = key.trim().to_ascii_lowercase();
        if key.is_empty() || !key.bytes().all(token) {
            return Err(StatusCode::BAD_REQUEST.into());
        }
        rest = tail.trim_start();
        let value = if let Some(quoted) = rest.strip_prefix('"') {
            let mut value = String::new();
            let mut chars = quoted.char_indices();
            let end = loop {
                let (index, character) = chars.next().ok_or(StatusCode::BAD_REQUEST)?;
                match character {
                    '"' => break index + 1,
                    '\\' => value.push(chars.next().ok_or(StatusCode::BAD_REQUEST)?.1),
                    '\r' | '\n' | '\0' => return Err(StatusCode::BAD_REQUEST.into()),
                    other => value.push(other),
                }
            };
            rest = quoted[end..].trim_start();
            if !rest.is_empty() {
                rest = rest.strip_prefix(';').ok_or(StatusCode::BAD_REQUEST)?;
            }
            value
        } else {
            let (value, tail) = rest.split_once(';').unwrap_or((rest, ""));
            let value = value.trim();
            if value.is_empty() || !value.bytes().all(token) {
                return Err(StatusCode::BAD_REQUEST.into());
            }
            rest = tail;
            value.to_owned()
        };
        if parameters
            .get(&key)
            .is_some_and(|previous| previous != &value)
        {
            return Err(StatusCode::BAD_REQUEST.into());
        }
        parameters.insert(key, value);
    }
    let extended: HashSet<_> = parameters
        .keys()
        .filter_map(|key| key.split_once('*').map(|(base, _)| base.to_owned()))
        .collect();
    for key in extended {
        if let Some(value) = parameters.get(&format!("{key}*")) {
            if let Some(value) =
                extended_value(value).and_then(|bytes| String::from_utf8(bytes).ok())
            {
                parameters.insert(key, value);
            }
            continue;
        }
        let mut joined = Vec::new();
        let mut present = false;
        for index in 0..parameters.len() {
            if let Some(value) = parameters.get(&format!("{key}*{index}")) {
                joined.extend_from_slice(value.as_bytes());
            } else if let Some(value) = parameters.get(&format!("{key}*{index}*")) {
                let value = if index == 0 {
                    extended_value(value)
                } else {
                    percent_decode(value)
                };
                joined.extend_from_slice(&value.unwrap_or_default());
            } else {
                break;
            }
            present = true;
        }
        if present {
            if let Ok(joined) = String::from_utf8(joined) {
                parameters.insert(key, joined);
            }
        }
    }
    Ok((kind.trim().to_ascii_lowercase(), parameters))
}

fn extended_value(value: &str) -> Option<Vec<u8>> {
    let mut pieces = value.splitn(3, '\'');
    let charset = pieces.next()?;
    if !charset.eq_ignore_ascii_case("utf-8") && !charset.eq_ignore_ascii_case("us-ascii") {
        return None;
    }
    pieces.next()?;
    percent_decode(pieces.next()?)
}

fn percent_decode(value: &str) -> Option<Vec<u8>> {
    let mut bytes = value.bytes();
    let mut decoded = Vec::new();
    while let Some(byte) = bytes.next() {
        decoded.push(if byte == b'%' {
            let high = (bytes.next()? as char).to_digit(16)?;
            let low = (bytes.next()? as char).to_digit(16)?;
            (high * 16 + low) as u8
        } else {
            byte
        });
    }
    Some(decoded)
}

fn token(byte: u8) -> bool {
    byte.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(&byte)
}

pub(super) fn upload(
    context: &Context,
    input: &mut dyn Read,
    boundary: &str,
    path: Option<&str>,
) -> Result<Vec<serde_json::Value>> {
    let mut parser = Parser {
        input,
        buffer: Vec::new(),
        newline: b"\r\n",
        last_crlf: true,
    };
    let delimiter = format!("--{boundary}");
    let mut budget = HEADER_LIMIT;
    loop {
        let line = parser.line(&mut budget)?;
        let line = line.trim_ascii_end();
        if line == delimiter.as_bytes() {
            parser.newline = if parser.last_crlf { b"\r\n" } else { b"\n" };
            break;
        }
        if line == format!("{delimiter}--").as_bytes() {
            parser.finish()?;
            return Ok(Vec::new());
        }
    }
    let delimiter = [parser.newline, delimiter.as_bytes()].concat();
    let mut seen = HashSet::<PathBuf>::new();
    let mut entries: Vec<serde_json::Value> = Vec::new();
    for _ in 0..PART_LIMIT {
        context.check().map_err(status)?;
        let mut budget = HEADER_LIMIT;
        let mut disposition = None;
        let mut quoted_printable = false;
        loop {
            let line = parser.line(&mut budget)?;
            if line.is_empty() {
                break;
            }
            let text = std::str::from_utf8(&line).map_err(|_| StatusCode::BAD_REQUEST)?;
            let (key, value) = text.split_once(':').ok_or(StatusCode::BAD_REQUEST)?;
            if !key.bytes().all(token) || key.is_empty() {
                return Err(StatusCode::BAD_REQUEST.into());
            }
            if key.eq_ignore_ascii_case("content-transfer-encoding") {
                quoted_printable = value.trim().eq_ignore_ascii_case("quoted-printable");
            }
            if key.eq_ignore_ascii_case("content-disposition") {
                if disposition.is_some() {
                    return Err(StatusCode::BAD_REQUEST.into());
                }
                disposition = Some(parameters(value.trim())?);
            }
        }
        let filename = disposition.and_then(|(kind, mut params)| {
            if kind != "form-data" || params.get("name").map(String::as_str) != Some("file") {
                return None;
            }
            Some(params.remove("filename").unwrap_or_default())
        });
        let target = if let Some(filename) = filename {
            let path = context
                .destructive_path(path.unwrap_or(&filename))
                .map_err(status)?;
            // Components removes repeated separators and `.` but retains every `..`.
            // No realpath or inode identity: symlinks/hardlinks remain independent keys.
            let key = path.components().collect();
            if !seen.insert(key) {
                let mut message = format!("you cannot upload multiple files to the same path '{}' in one upload request, only the first specified file was uploaded", path.display());
                let others: Vec<_> = entries
                    .iter()
                    .filter_map(|entry| entry["path"].as_str())
                    .filter(|other| *other != path.to_string_lossy())
                    .collect();
                if others.len() > 1 {
                    message.push_str(&format!(
                        ", also the following files were uploaded: {}",
                        others.join(", ")
                    ));
                }
                return Err(UploadError::new(StatusCode::BAD_REQUEST, message));
            }
            Some(path)
        } else {
            None
        };
        let mut file = target
            .as_ref()
            .map(|path| context.upload_file(path))
            .transpose()?;
        let last = parser.part(context, &delimiter, file.as_mut(), quoted_printable)?;
        if let Some(path) = target {
            entries.push(write_info(&path));
        }
        if last {
            parser.closing()?;
            parser.finish()?;
            return Ok(entries);
        }
    }
    Err(StatusCode::PAYLOAD_TOO_LARGE.into())
}

struct Parser<'a> {
    input: &'a mut dyn Read,
    buffer: Vec<u8>,
    newline: &'static [u8],
    last_crlf: bool,
}
impl Parser<'_> {
    fn more(&mut self) -> Result<()> {
        let mut chunk = [0; CHUNK];
        let count = self.input.read(&mut chunk).map_err(body_status)?;
        if count == 0 {
            return Err(StatusCode::BAD_REQUEST.into());
        }
        self.buffer.extend_from_slice(&chunk[..count]);
        Ok(())
    }
    fn line(&mut self, budget: &mut usize) -> Result<Vec<u8>> {
        loop {
            if let Some(end) = self.buffer.iter().position(|byte| *byte == b'\n') {
                *budget = budget
                    .checked_sub(end + 1)
                    .ok_or(StatusCode::PAYLOAD_TOO_LARGE)?;
                self.last_crlf = end > 0 && self.buffer[end - 1] == b'\r';
                let line = self.buffer[..end - usize::from(self.last_crlf)].to_vec();
                self.buffer.drain(..end + 1);
                return Ok(line);
            }
            if self.buffer.len() >= *budget {
                return Err(StatusCode::PAYLOAD_TOO_LARGE.into());
            }
            self.more()?;
        }
    }
    fn part(
        &mut self,
        context: &Context,
        delimiter: &[u8],
        file: Option<&mut std::fs::File>,
        quoted_printable: bool,
    ) -> Result<bool> {
        let mut writer = PartWriter {
            file,
            quoted_printable,
            line: Vec::new(),
        };
        loop {
            context.check().map_err(status)?;
            let mut found = None;
            let mut retain = None;
            for start in 0..self.buffer.len().saturating_sub(delimiter.len() - 1) {
                if !self.buffer[start..].starts_with(delimiter) {
                    continue;
                }
                let suffix = &self.buffer[start + delimiter.len()..];
                if suffix.starts_with(b"--") {
                    found = Some((start, start + delimiter.len() + 2, true));
                    break;
                }
                let padding = suffix
                    .iter()
                    .take_while(|byte| matches!(byte, b' ' | b'\t'))
                    .count();
                if padding > HEADER_LIMIT {
                    return Err(StatusCode::PAYLOAD_TOO_LARGE.into());
                }
                let suffix = &suffix[padding..];
                if suffix.len() < self.newline.len() {
                    retain = Some(start);
                    break;
                }
                if suffix.starts_with(self.newline) {
                    found = Some((
                        start,
                        start + delimiter.len() + padding + self.newline.len(),
                        false,
                    ));
                    break;
                }
            }
            let count = found
                .map(|(start, _, _)| start)
                .or(retain)
                .unwrap_or_else(|| self.buffer.len().saturating_sub(delimiter.len() + 2));
            writer.push(context, &self.buffer[..count], found.is_some())?;
            if let Some((_, end, last)) = found {
                self.buffer.drain(..end);
                return Ok(last);
            }
            self.buffer.drain(..count);
            self.more()?;
        }
    }
    fn closing(&mut self) -> Result<()> {
        // A closing delimiter ends at EOF or a CRLF, not at an arbitrary `--`
        // prefix inside file content. Optional transport padding is bounded.
        let mut padding = HEADER_LIMIT;
        loop {
            if self.buffer.is_empty() {
                let mut chunk = [0; CHUNK];
                let count = self.input.read(&mut chunk).map_err(body_status)?;
                if count == 0 {
                    return Ok(());
                }
                self.buffer.extend_from_slice(&chunk[..count]);
            }
            if matches!(self.buffer[0], b' ' | b'\t') {
                padding = padding
                    .checked_sub(1)
                    .ok_or(StatusCode::PAYLOAD_TOO_LARGE)?;
                self.buffer.remove(0);
                continue;
            }
            if self.buffer.len() == 1 && self.buffer[0] == b'\r' {
                self.more()?;
            }
            if !self.buffer.starts_with(self.newline) {
                return Err(StatusCode::BAD_REQUEST.into());
            }
            self.buffer.drain(..self.newline.len());
            return Ok(());
        }
    }
    fn finish(&mut self) -> Result<()> {
        // Consume the epilogue too: gzip trailers and malformed trailing encodings still apply.
        let mut chunk = [0; CHUNK];
        while self.input.read(&mut chunk).map_err(body_status)? != 0 {}
        Ok(())
    }
}

// Go's quoted-printable reader processes bounded (4096-byte) lines. Keep that
// bound while streaming decoded bytes directly to the bound file.
struct PartWriter<'a> {
    file: Option<&'a mut std::fs::File>,
    quoted_printable: bool,
    line: Vec<u8>,
}

impl PartWriter<'_> {
    fn write(&mut self, context: &Context, bytes: &[u8]) -> Result<()> {
        let Some(file) = self.file.as_mut() else {
            return Ok(());
        };
        let mut pending = bytes;
        while !pending.is_empty() {
            context.check().map_err(status)?;
            match file.write(pending) {
                Ok(0) => return Err(StatusCode::INTERNAL_SERVER_ERROR.into()),
                Ok(count) => pending = &pending[count..],
                Err(error) if error.kind() == std::io::ErrorKind::Interrupted => continue,
                Err(error) => return Err(io_status(error).into()),
            }
        }
        Ok(())
    }

    fn push(&mut self, context: &Context, bytes: &[u8], end: bool) -> Result<()> {
        if self.file.is_none() {
            return Ok(());
        }
        if !self.quoted_printable {
            return self.write(context, bytes);
        }
        for &byte in bytes {
            self.line.push(byte);
            if byte == b'\n' || self.line.len() == 4096 {
                self.flush_line(context, false)?;
            }
        }
        if end && !self.line.is_empty() {
            self.flush_line(context, true)?;
        }
        Ok(())
    }

    fn flush_line(&mut self, context: &Context, eof: bool) -> Result<()> {
        let whole = std::mem::take(&mut self.line);
        let has_lf = whole.ends_with(b"\n");
        let mut line = whole.as_slice();
        while line.last().is_some_and(|byte| b"\r\n \t".contains(byte)) {
            line = &line[..line.len() - 1];
        }
        let mut error = if !has_lf && !eof {
            Some("bufio: buffer full".to_owned())
        } else {
            None
        };
        let mut encoded = line.to_vec();
        if line.ends_with(b"=") {
            encoded.pop();
            let mut stripped = &whole[line.len()..];
            while stripped.first().is_some_and(|byte| b" \t".contains(byte)) {
                stripped = &stripped[1..];
            }
            if !(stripped.starts_with(b"\n")
                || stripped.starts_with(b"\r\n")
                || stripped.is_empty() && !encoded.is_empty() && eof)
            {
                error = Some(format!(
                    "quotedprintable: invalid bytes after =: {:?}",
                    String::from_utf8_lossy(stripped)
                ));
            }
        } else if has_lf {
            encoded.extend_from_slice(if whole.ends_with(b"\r\n") {
                b"\r\n"
            } else {
                b"\n"
            });
        }
        let mut decoded = Vec::with_capacity(encoded.len());
        let mut index = 0;
        while index < encoded.len() {
            let byte = encoded[index];
            if byte == b'=' {
                let hex = encoded.get(index + 1..index + 3).and_then(|pair| {
                    Some(
                        ((pair[0] as char).to_digit(16)? * 16 + (pair[1] as char).to_digit(16)?)
                            as u8,
                    )
                });
                if let Some(byte) = hex {
                    decoded.push(byte);
                    index += 3;
                    continue;
                }
                if encoded
                    .get(index + 1)
                    .is_none_or(|byte| matches!(byte, b'\r' | b'\n'))
                {
                    error = Some("unexpected EOF".to_owned());
                    break;
                }
            } else if (byte < b' ' && !b"\t\r\n".contains(&byte)) || byte == 0x7f {
                error = Some(format!(
                    "quotedprintable: invalid unescaped byte 0x{byte:02x} in body"
                ));
                break;
            }
            decoded.push(byte);
            index += 1;
        }
        self.write(context, &decoded)?;
        if let Some(error) = error {
            return Err(UploadError::new(
                StatusCode::INTERNAL_SERVER_ERROR,
                format!("error writing file: {error}"),
            ));
        }
        Ok(())
    }
}

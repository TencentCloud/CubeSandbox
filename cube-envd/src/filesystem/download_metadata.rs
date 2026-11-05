// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use std::collections::HashMap;
use std::io::Read;
use std::path::Path;
use std::sync::OnceLock;

pub(super) fn mime(path: &Path, prefix: &[u8]) -> String {
    extension(path).unwrap_or_else(|| sniff(prefix).into())
}

pub(super) fn extension(path: &Path) -> Option<String> {
    static TYPES: OnceLock<HashMap<String, String>> = OnceLock::new();
    let types = TYPES.get_or_init(|| {
        // Like the oracle, supplement the built-in types with the guest MIME DB.
        let mut types: HashMap<String, String> = [
            ("avif", "image/avif"),
            ("css", "text/css"),
            ("gif", "image/gif"),
            ("htm", "text/html"),
            ("html", "text/html"),
            ("jpeg", "image/jpeg"),
            ("jpg", "image/jpeg"),
            ("js", "text/javascript"),
            ("json", "application/json"),
            ("mjs", "text/javascript"),
            ("pdf", "application/pdf"),
            ("png", "image/png"),
            ("svg", "image/svg+xml"),
            ("wasm", "application/wasm"),
            ("webp", "image/webp"),
            ("xml", "text/xml"),
        ]
        .into_iter()
        .map(|(key, value)| (key.into(), value.into()))
        .collect();
        for name in [
            "/etc/mime.types",
            "/etc/apache2/mime.types",
            "/etc/apache/mime.types",
        ] {
            if let Ok(file) = std::fs::File::open(name) {
                let mut text = String::new();
                // This is a guest configuration lookup, never whole-file download buffering.
                if file.take(1024 * 1024).read_to_string(&mut text).is_ok() {
                    for line in text.lines() {
                        let mut fields = line
                            .split('#')
                            .next()
                            .unwrap_or_default()
                            .split_whitespace();
                        if let Some(mime) = fields.next().filter(|mime| mime.contains('/')) {
                            for extension in fields {
                                types.insert(extension.into(), mime.into());
                            }
                        }
                    }
                }
            }
        }
        types
    });
    // filepath.Ext in the oracle includes the leading dot of `.json` too.
    if let Some(mime) = path
        .file_name()
        .and_then(|name| name.to_str())
        .and_then(|name| name.rsplit_once('.').map(|(_, extension)| extension))
        .and_then(|ext| {
            types
                .get(ext)
                .or_else(|| types.get(&ext.to_ascii_lowercase()))
        })
    {
        return Some(if mime.starts_with("text/") && !mime.contains("charset=") {
            format!("{mime}; charset=utf-8")
        } else {
            mime.clone()
        });
    }
    None
}

pub(super) fn disposition(path: &Path) -> String {
    let name = path.file_name().unwrap_or_default().to_string_lossy();
    let parameter = if name.bytes().any(|byte| !(32..127).contains(&byte)) {
        let mut encoded = String::from("filename*=utf-8''");
        for byte in name.bytes() {
            if byte.is_ascii_alphanumeric() || b"!#$&+-.^_`|~".contains(&byte) {
                encoded.push(byte as char);
            } else {
                use std::fmt::Write;
                write!(encoded, "%{byte:02X}").expect("string write");
            }
        }
        encoded
    } else if !name.is_empty()
        && name
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(&byte))
    {
        format!("filename={name}")
    } else {
        format!(
            "filename=\"{}\"",
            name.replace('\\', "\\\\").replace('"', "\\\"")
        )
    };
    format!("inline; {parameter}")
}

fn sniff(data: &[u8]) -> &'static str {
    let trimmed = data.trim_ascii_start();
    for tag in [
        "<!DOCTYPE HTML",
        "<HTML",
        "<HEAD",
        "<SCRIPT",
        "<IFRAME",
        "<H1",
        "<DIV",
        "<FONT",
        "<TABLE",
        "<A",
        "<STYLE",
        "<TITLE",
        "<B",
        "<BODY",
        "<BR",
        "<P",
        "<!--",
    ] {
        if trimmed
            .get(..tag.len())
            .is_some_and(|prefix| prefix.eq_ignore_ascii_case(tag.as_bytes()))
            && trimmed
                .get(tag.len())
                .is_some_and(|byte| matches!(byte, b' ' | b'>'))
        {
            return "text/html; charset=utf-8";
        }
    }
    if trimmed.starts_with(b"<?xml") {
        return "text/xml; charset=utf-8";
    }
    for (signature, mime) in [
        (b"%PDF-".as_slice(), "application/pdf"),
        (b"%!PS-Adobe-", "application/postscript"),
        (b"\xfe\xff", "text/plain; charset=utf-16be"),
        (b"\xff\xfe", "text/plain; charset=utf-16le"),
        (b"\xef\xbb\xbf", "text/plain; charset=utf-8"),
        (b"\x00\x00\x01\x00", "image/x-icon"),
        (b"\x00\x00\x02\x00", "image/x-icon"),
        (b"BM", "image/bmp"),
        (b"GIF87a", "image/gif"),
        (b"GIF89a", "image/gif"),
        (b"\x89PNG\r\n\x1a\n", "image/png"),
        (b"\xff\xd8\xff", "image/jpeg"),
        (b".snd", "audio/basic"),
        (b"ID3", "audio/mpeg"),
        (b"OggS\x00", "application/ogg"),
        (b"MThd\x00\x00\x00\x06", "audio/midi"),
        (b"\x1a\x45\xdf\xa3", "video/webm"),
        (b"\x00\x00\x01\xba", "video/mpeg"),
        (b"\x00\x00\x01\xb3", "video/mpeg"),
        (b"wOFF", "font/woff"),
        (b"wOF2", "font/woff2"),
        (b"\x00\x01\x00\x00", "font/ttf"),
        (b"OTTO", "font/otf"),
        (b"ttcf", "font/collection"),
        (b"\x1f\x8b\x08", "application/x-gzip"),
        (b"PK\x03\x04", "application/zip"),
        (b"Rar!\x1a\x07\x00", "application/x-rar-compressed"),
        (b"Rar!\x1a\x07\x01\x00", "application/x-rar-compressed"),
        (b"\x00asm", "application/wasm"),
    ] {
        if data.starts_with(signature) {
            return mime;
        }
    }
    if data.len() >= 12 {
        match (&data[..4], &data[8..12]) {
            (b"RIFF", b"WEBP") if data.get(12..14) == Some(b"VP") => return "image/webp",
            (b"RIFF", b"WAVE") => return "audio/wave",
            (b"RIFF", b"AVI ") => return "video/avi",
            (b"FORM", b"AIFF") => return "audio/aiff",
            _ => {}
        }
        let size = u32::from_be_bytes(data[..4].try_into().unwrap()) as usize;
        if &data[4..8] == b"ftyp"
            && size >= 12
            && size <= data.len()
            && size % 4 == 0
            && (8..size)
                .step_by(4)
                .filter(|index| *index != 12)
                .any(|index| &data[index..index + 3] == b"mp4")
        {
            return "video/mp4";
        }
    }
    if data.len() >= 36 && data[..34].iter().all(|byte| *byte == 0) && &data[34..36] == b"LP" {
        return "application/vnd.ms-fontobject";
    }
    if data
        .iter()
        .any(|byte| matches!(byte, 0..=8 | 11 | 14..=26 | 28..=31))
    {
        "application/octet-stream"
    } else {
        "text/plain; charset=utf-8"
    }
}

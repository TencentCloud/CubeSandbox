// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! `Content-Disposition` header values for file downloads.
//!
//! Mirror of Go stdlib `mime.FormatMediaType("inline", {"filename": base})`
//! — the exact call upstream envd's download.go makes. Pure function; the
//! media type and attribute are fixed because upstream only ever sends
//! `inline; filename=…`.

/// RFC 7230 token characters (`isTokenChar`).
fn is_token_char(c: u8) -> bool {
    c.is_ascii_alphanumeric()
        || matches!(
            c,
            b'!' | b'#'
                | b'$'
                | b'%'
                | b'&'
                | b'\''
                | b'*'
                | b'+'
                | b'-'
                | b'.'
                | b'^'
                | b'_'
                | b'`'
                | b'{'
                | b'|'
                | b'}'
                | b'~'
        )
}

fn is_token(s: &str) -> bool {
    !s.is_empty() && s.bytes().all(is_token_char)
}

/// RFC 2045 tspecials (`isTSpecial`): `()<>@,;:\"/[]?=`.
fn is_tspecial(c: u8) -> bool {
    matches!(
        c,
        b'(' | b')'
            | b'<'
            | b'>'
            | b'@'
            | b','
            | b';'
            | b':'
            | b'\\'
            | b'"'
            | b'/'
            | b'['
            | b']'
            | b'?'
            | b'='
    )
}

/// `mime.FormatMediaType("inline", {"filename": base})` — value only. Mirrors
/// upstream's download_test.go cases byte-for-byte:
/// - pure token → `inline; filename=name`;
/// - printable ASCII with punctuation → quoted-string (`"` and `\` escaped);
/// - any byte <0x20 (except tab) or >0x7E → RFC 2231 `filename*=utf-8''…`
///   with `%XX` (upper hex) for bytes outside the RFC 2231 attribute charset.
pub fn format_content_disposition(base: &str) -> String {
    let need_encoding = base
        .bytes()
        .any(|b| !(b' '..=b'~').contains(&b) && b != b'\t');
    if need_encoding {
        let mut out = String::from("inline; filename*=utf-8''");
        for b in base.bytes() {
            if b <= b' ' || b >= 0x7F || matches!(b, b'*' | b'\'' | b'%') || is_tspecial(b) {
                use std::fmt::Write as _;
                let _ = write!(out, "%{b:02X}");
            } else {
                out.push(b as char);
            }
        }
        return out;
    }
    if is_token(base) {
        return format!("inline; filename={base}");
    }
    let mut out = String::from("inline; filename=\"");
    for ch in base.chars() {
        if ch == '"' || ch == '\\' {
            out.push('\\');
        }
        out.push(ch);
    }
    out.push('"');
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cd_simple_filename() {
        // download_test.go: pure token filenames stay bare.
        assert_eq!(
            format_content_disposition("test.txt"),
            "inline; filename=test.txt"
        );
        assert_eq!(
            format_content_disposition("presentation.pptx"),
            "inline; filename=presentation.pptx"
        );
        assert_eq!(
            format_content_disposition("archive.tar.gz"),
            "inline; filename=archive.tar.gz"
        );
        assert_eq!(format_content_disposition(".env"), "inline; filename=.env");
        assert_eq!(
            format_content_disposition(".gitignore"),
            "inline; filename=.gitignore"
        );
    }

    #[test]
    fn cd_space_is_quoted() {
        // Non-token printable ASCII → quoted-string.
        assert_eq!(
            format_content_disposition("my document.pdf"),
            "inline; filename=\"my document.pdf\""
        );
    }

    #[test]
    fn cd_quote_and_backslash_escaped() {
        assert_eq!(
            format_content_disposition("file\"name.txt"),
            "inline; filename=\"file\\\"name.txt\""
        );
        assert_eq!(
            format_content_disposition("file\\name.txt"),
            "inline; filename=\"file\\\\name.txt\""
        );
    }

    #[test]
    fn cd_unicode_is_rfc2231() {
        // 文档.pdf — non-ASCII → RFC 2231 percent-encoding (upper hex).
        assert_eq!(
            format_content_disposition("\u{6587}\u{6863}.pdf"),
            "inline; filename*=utf-8''%E6%96%87%E6%A1%A3.pdf"
        );
    }

    #[test]
    fn cd_tspecial_percent_escaped_in_rfc2231() {
        // '/' is a tspecial → percent-encoded even though ASCII.
        assert_eq!(
            format_content_disposition("a/b\u{6587}.pdf"),
            "inline; filename*=utf-8''a%2Fb%E6%96%87.pdf"
        );
    }
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! `Range` request-header parsing and `Content-Range` response values.
//!
//! Translated 1:1 from Go stdlib `net/http/fs.go` `parseRange` /
//! `httpRange.contentRange` (go1.26.5 — the toolchain envd-reference builds
//! with). Pure functions; the handler owns the 206/416/200 decision that
//! consumes them.

use super::preconditions::trim_string;

/// One satisfiable byte range (`httpRange`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ByteRange {
    pub start: i64,
    pub length: i64,
}

impl ByteRange {
    /// `httpRange.contentRange`: `bytes <start>-<end>/<size>`.
    pub fn content_range(self, size: i64) -> String {
        format!(
            "bytes {}-{}/{}",
            self.start,
            self.start + self.length - 1,
            size
        )
    }
}

/// Which 416 shape an unsatisfiable `Range` maps to (`errNoOverlap` vs
/// `errInvalidRange`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RangeError {
    /// `errors.New("invalid range")` — bad syntax / unit / numbers.
    Invalid,
    /// `errNoOverlap` — `"invalid range: failed to overlap"`.
    NoOverlap,
}

impl RangeError {
    pub fn message(self) -> &'static str {
        match self {
            RangeError::Invalid => "invalid range",
            RangeError::NoOverlap => "invalid range: failed to overlap",
        }
    }
}

/// `parseRange` — `None` when the header is absent; `Err(Invalid)` on syntax
/// errors; `Err(NoOverlap)` when no listed range overlaps (callers must still
/// special-case `size == 0` → ignore, mirroring serveContent). Multiple ranges
/// come back as a vec; the download handler answers any request whose vec has
/// more than one entry with the full file (no multipart writer).
pub fn parse_range(header: Option<&str>, size: i64) -> Result<Option<Vec<ByteRange>>, RangeError> {
    let Some(s) = header else {
        return Ok(None);
    };
    if s.is_empty() {
        return Ok(None);
    }
    const PREFIX: &str = "bytes=";
    if !s.starts_with(PREFIX) {
        return Err(RangeError::Invalid);
    }
    let mut ranges: Vec<ByteRange> = Vec::new();
    let mut no_overlap = false;
    for piece in s[PREFIX.len()..].split(',') {
        // Go parseRange trims OWS around pieces with textproto.TrimString
        // semantics (ASCII space/tab; see preconditions::trim_string).
        let piece = trim_string(piece);
        if piece.is_empty() {
            continue;
        }
        let Some((start_s, end_s)) = piece.split_once('-') else {
            return Err(RangeError::Invalid);
        };
        let start_s = trim_string(start_s);
        let end_s = trim_string(end_s);
        if start_s.is_empty() {
            // Suffix range: `-N` = last N bytes; N must be a non-negative
            // integer (RFC 7233 §2.1), clamped to size.
            if end_s.is_empty() || end_s.starts_with('-') {
                return Err(RangeError::Invalid);
            }
            let i: i64 = end_s.parse().map_err(|_| RangeError::Invalid)?;
            if i < 0 {
                return Err(RangeError::Invalid);
            }
            let i = i.min(size);
            ranges.push(ByteRange {
                start: size - i,
                length: size - (size - i),
            });
        } else {
            let i: i64 = start_s.parse().map_err(|_| RangeError::Invalid)?;
            if i < 0 {
                return Err(RangeError::Invalid);
            }
            if i >= size {
                // Range starts past the end: does not overlap.
                no_overlap = true;
                continue;
            }
            let start = i;
            let length = if end_s.is_empty() {
                size - start
            } else {
                let i: i64 = end_s.parse().map_err(|_| RangeError::Invalid)?;
                if start > i {
                    return Err(RangeError::Invalid);
                }
                let end = i.min(size - 1);
                end - start + 1
            };
            ranges.push(ByteRange { start, length });
        }
    }
    if no_overlap && ranges.is_empty() {
        return Err(RangeError::NoOverlap);
    }
    Ok(Some(ranges))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn range_absent() {
        assert_eq!(parse_range(None, 100).unwrap(), None);
        assert_eq!(parse_range(Some(""), 100).unwrap(), None);
    }

    #[test]
    fn range_single() {
        let r = parse_range(Some("bytes=0-4"), 100).unwrap().unwrap();
        assert_eq!(
            r,
            vec![ByteRange {
                start: 0,
                length: 5
            }]
        );
    }

    #[test]
    fn range_open_ended() {
        let r = parse_range(Some("bytes=5-"), 100).unwrap().unwrap();
        assert_eq!(
            r,
            vec![ByteRange {
                start: 5,
                length: 95
            }]
        );
    }

    #[test]
    fn range_open_end_with_start_is_valid() {
        // `bytes=0-` is an open-ended range (fs.go treats a trailing dash as
        // "to end of file"), not a syntax error.
        let r = parse_range(Some("bytes=0-"), 100).unwrap().unwrap();
        assert_eq!(
            r,
            vec![ByteRange {
                start: 0,
                length: 100
            }]
        );
    }

    #[test]
    fn range_suffix() {
        let r = parse_range(Some("bytes=-5"), 100).unwrap().unwrap();
        assert_eq!(
            r,
            vec![ByteRange {
                start: 95,
                length: 5
            }]
        );
        // Suffix longer than the file clamps to the whole file.
        let r = parse_range(Some("bytes=-500"), 100).unwrap().unwrap();
        assert_eq!(
            r,
            vec![ByteRange {
                start: 0,
                length: 100
            }]
        );
    }

    #[test]
    fn range_end_clamped() {
        let r = parse_range(Some("bytes=0-999"), 100).unwrap().unwrap();
        assert_eq!(
            r,
            vec![ByteRange {
                start: 0,
                length: 100
            }]
        );
    }

    #[test]
    fn range_no_overlap() {
        assert_eq!(
            parse_range(Some("bytes=999-"), 100).unwrap_err(),
            RangeError::NoOverlap
        );
    }

    #[test]
    fn range_bad_syntax() {
        for bad in ["abc", "chunks=0-1", "bytes=0--"] {
            assert_eq!(
                parse_range(Some(bad), 100).unwrap_err(),
                RangeError::Invalid,
                "accepted {bad:?}"
            );
        }
    }

    #[test]
    fn range_negative_or_garbage_numbers() {
        for bad in ["bytes=-1-5", "bytes=abc-5", "bytes=5-abc", "bytes=-"] {
            assert_eq!(
                parse_range(Some(bad), 100).unwrap_err(),
                RangeError::Invalid,
                "accepted {bad:?}"
            );
        }
    }

    #[test]
    fn range_multiple() {
        let r = parse_range(Some("bytes=0-1, 5-6"), 100).unwrap().unwrap();
        assert_eq!(
            r,
            vec![
                ByteRange {
                    start: 0,
                    length: 2
                },
                ByteRange {
                    start: 5,
                    length: 2
                }
            ]
        );
        // Second range starts past EOF; first is kept.
        let r = parse_range(Some("bytes=0-1, 999-"), 100).unwrap().unwrap();
        assert_eq!(
            r,
            vec![ByteRange {
                start: 0,
                length: 2
            }]
        );
    }

    #[test]
    fn range_multiple_ranges_stay_separate() {
        // Overlapping ranges parse individually; the handler ignores any
        // multi-range result (full 200), so they need not be merged.
        let r = parse_range(Some("bytes=0-5, 3-8"), 100).unwrap().unwrap();
        assert_eq!(r.len(), 2);
    }

    #[test]
    fn range_empty_file() {
        // size == 0: `bytes=-1` clamps to 0 → an empty-but-present range.
        let r = parse_range(Some("bytes=-1"), 0).unwrap().unwrap();
        assert_eq!(
            r,
            vec![ByteRange {
                start: 0,
                length: 0
            }]
        );
        // Start beyond EOF on an empty file → NoOverlap (handler ignores it
        // when size == 0, mirroring serveContent).
        assert_eq!(
            parse_range(Some("bytes=0-"), 0).unwrap_err(),
            RangeError::NoOverlap
        );
    }

    #[test]
    fn range_content_range_string() {
        let r = ByteRange {
            start: 5,
            length: 95,
        };
        assert_eq!(r.content_range(100), "bytes 5-99/100");
    }
}

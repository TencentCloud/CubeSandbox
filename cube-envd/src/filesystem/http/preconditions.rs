// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! HTTP conditional-request evaluation (RFC 7232), translated 1:1 from Go
//! stdlib `net/http/fs.go` `checkPreconditions` / `checkIfMatch` /
//! `checkIfUnmodifiedSince` / `checkIfNoneMatch` / `checkIfModifiedSince` /
//! `checkIfRange` (go1.26.5).
//!
//! cube-envd never sets an ETag (upstream envd doesn't either — it serves
//! through `http.ServeContent` without one), so the etag comparisons below
//! are against an always-absent current representation. The handler owns the
//! actual 304/412 header assembly; this module only decides *which* outcome
//! applies (mirroring fs.go's `done` short-circuit).

/// Result of evaluating all preconditions, in RFC 7232 §6 evaluation order.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CondOutcome {
    /// No precondition failed: serve the requested representation.
    Serve,
    /// `If-None-Match` matched (`*` or an etag — never here, no ETag) or
    /// `If-Modified-Since` says unchanged → 304 (GET/HEAD only).
    NotModified,
    /// `If-Match` / `If-Unmodified-Since` failed → 412.
    PreconditionFailed,
}

/// `checkPreconditions` core, for GET/HEAD only (the only methods /files
/// serves). `modtime_secs: None` = zero/epoch mtime (`isZeroTime`): the
/// date-based conditions then do not apply, exactly like Go.
///
/// Order mirrors fs.go: If-Match → (if none) If-Unmodified-Since → 412 on
/// failure; then If-None-Match (304 on GET/HEAD when matched, otherwise the
/// request proceeds *without* consulting If-Modified-Since — a concrete
/// non-matching etag means the client knows the representation changed);
/// If-Modified-Since only when If-None-Match is absent.
pub fn check_preconditions(
    if_match: Option<&str>,
    if_unmodified_since: Option<&str>,
    if_none_match: Option<&str>,
    if_modified_since: Option<&str>,
    modtime_secs: Option<i64>,
) -> CondOutcome {
    // Go's checkPreconditions reads the headers with Header.Get, whose empty
    // value for a present-but-empty header is indistinguishable from an
    // absent one (`if im := r.Header.Get("If-Match"); im != ""`). An empty
    // header must therefore behave exactly like a missing one: If-Match: ""
    // must NOT 412, If-None-Match: "" must NOT short-circuit
    // If-Modified-Since.
    let if_match = if_match.filter(|v| !v.is_empty());
    let if_unmodified_since = if_unmodified_since.filter(|v| !v.is_empty());
    let if_none_match = if_none_match.filter(|v| !v.is_empty());
    let if_modified_since = if_modified_since.filter(|v| !v.is_empty());
    // Stage 1: If-Match, else If-Unmodified-Since → 412 on failure.
    if let Some(im) = if_match {
        match if_match_result(im) {
            CondOutcome::PreconditionFailed => return CondOutcome::PreconditionFailed,
            // condTrue (`*` matched) → skip IUS, fall through to stage 2.
            CondOutcome::Serve => {}
            CondOutcome::NotModified => unreachable!("checkIfMatch never yields 304"),
        }
    } else if let Some(ius) = if_unmodified_since {
        if if_unmodified_since_result(ius, modtime_secs) == CondOutcome::PreconditionFailed {
            return CondOutcome::PreconditionFailed;
        }
    }
    // Stage 2: If-None-Match → 304 on `*`; a concrete non-matching etag
    // (condTrue) short-circuits If-Modified-Since entirely.
    if let Some(inm) = if_none_match {
        return match if_none_match_result(inm) {
            CondOutcome::NotModified => CondOutcome::NotModified,
            _ => CondOutcome::Serve,
        };
    }
    // Stage 3: no If-None-Match → If-Modified-Since decides.
    if let Some(ims) = if_modified_since {
        if if_modified_since_result(ims, modtime_secs) == CondOutcome::NotModified {
            return CondOutcome::NotModified;
        }
    }
    CondOutcome::Serve
}

/// `checkIfMatch`, 1:1 with go1.26.5 fs.go: skip spaces, skip a leading
/// comma, `*` → condTrue, scan an ETag — a scan failure breaks the loop
/// (→ 412) and otherwise scanning continues *after the consumed ETag*, so
/// garbage riding on a valid ETag (`"a"garbage, *`) breaks the next scan and
/// fails the precondition. No current ETag exists, so every concrete ETag is
/// a non-match and the scan keeps looking for `*`.
fn if_match_result(if_match: &str) -> CondOutcome {
    let mut im = if_match;
    loop {
        im = trim_string(im);
        if im.is_empty() {
            break;
        }
        if im.starts_with(',') {
            im = &im[1..];
            continue;
        }
        if im.starts_with('*') {
            return CondOutcome::Serve; // condTrue
        }
        let Some((_etag, remain)) = scan_etag(im) else {
            break; // scan break → condFalse
        };
        // A valid etag never strong-matches the absent current etag.
        im = remain;
    }
    CondOutcome::PreconditionFailed
}

/// `checkIfUnmodifiedSince`: applies only when If-Match was absent. A
/// non-parseable date or a zero mtime → condNone (serve); mtime ≤ IUS →
/// condTrue (serve); mtime > IUS → condFalse → 412.
fn if_unmodified_since_result(ius: &str, modtime_secs: Option<i64>) -> CondOutcome {
    let (Some(modtime), Some(t)) = (
        modtime_secs,
        crate::filesystem::http::httpdate::parse_http_date(ius),
    ) else {
        return CondOutcome::Serve; // condNone
    };
    if modtime <= t {
        CondOutcome::Serve // condTrue
    } else {
        CondOutcome::PreconditionFailed // condFalse → 412
    }
}

/// `checkIfNoneMatch` against the absent current etag, 1:1 with go1.26.5
/// fs.go: `*` (→ condFalse → 304 on GET) short-circuits; a scan failure
/// breaks the loop → condTrue; scanning resumes after each consumed ETag.
fn if_none_match_result(if_none_match: &str) -> CondOutcome {
    let mut inm = if_none_match;
    loop {
        inm = trim_string(inm);
        if inm.is_empty() {
            break;
        }
        if inm.starts_with(',') {
            inm = &inm[1..];
            continue;
        }
        if inm.starts_with('*') {
            return CondOutcome::NotModified; // condFalse → 304 (GET)
        }
        let Some((_etag, remain)) = scan_etag(inm) else {
            break; // scan break → condTrue
        };
        // A valid etag never weak-matches the absent current etag.
        inm = remain;
    }
    CondOutcome::Serve // condTrue
}

/// `checkIfModifiedSince`: only reached with no If-None-Match. Non-parseable
/// date or zero mtime → condNone (serve); mtime ≤ IMS → condFalse → 304;
/// mtime > IMS → condTrue (serve).
fn if_modified_since_result(ims: &str, modtime_secs: Option<i64>) -> CondOutcome {
    let (Some(modtime), Some(t)) = (
        modtime_secs,
        crate::filesystem::http::httpdate::parse_http_date(ims),
    ) else {
        return CondOutcome::Serve; // condNone
    };
    if modtime <= t {
        CondOutcome::NotModified // condFalse → 304
    } else {
        CondOutcome::Serve // condTrue
    }
}

/// `checkIfRange` gate — decides whether a `Range` header survives an
/// `If-Range` precondition (fs.go checkIfRange). Returns `true` when the
/// range request may proceed (keep the Range header).
///
/// The current representation has no ETag, so:
/// - a syntactically valid etag in If-Range never strong-matches → drop Range;
/// - a date value keeps Range only when `t.Unix() == modtime.Unix()` — both
///   sides are floor seconds, and fs.go has *no* zero-time gate here (only
///   the year-1 `IsZero()`, which a filesystem mtime never is), so an
///   exactly-epoch file still keeps its Range on a matching epoch date;
/// - `modtime_secs: None` means the mtime is entirely unknown → drop;
/// - anything unparseable → drop Range (RFC 7233 §3.2: proceed with a full
///   200 when If-Range fails).
pub fn if_range_keeps_range(if_range: Option<&str>, modtime_secs: Option<i64>) -> bool {
    let Some(ir) = if_range else {
        return true; // no If-Range: nothing drops the Range header
    };
    if ir.is_empty() {
        return true; // Header.Get: empty == absent
    }
    if scan_etag(ir).is_some() {
        // Valid etag syntax, no current etag → strong match fails → drop.
        return false;
    }
    let Some(modtime) = modtime_secs else {
        return false;
    };
    match crate::filesystem::http::httpdate::parse_http_date(ir) {
        Some(t) => t == modtime,
        None => false,
    }
}

/// `scanETag` (go1.26.5): determines whether a syntactically valid ETag is
/// present at `s` (spaces and tabs trimmed first); returns the ETag and the
/// remaining unconsumed text, or `None` when no valid ETag starts there.
/// Both slices borrow from the input.
fn scan_etag(s: &str) -> Option<(&str, &str)> {
    let s = trim_string(s);
    let start = if s.starts_with("W/") { 2 } else { 0 };
    let b = s.as_bytes();
    if b.len() < start + 2 || b[start] != b'"' {
        return None;
    }
    // ETag is either W/"text" or "text" (RFC 7232 §2.3).
    let mut i = start + 1;
    while i < b.len() {
        let c = b[i];
        if c == 0x21 || (0x23..=0x7E).contains(&c) || c >= 0x80 {
            i += 1;
            continue;
        }
        if c == b'"' {
            return Some((&s[..i + 1], &s[i + 1..]));
        }
        return None; // not an ETag character → no match at all
    }
    None // unterminated
}

/// Go `textproto.TrimString`: strips leading/trailing ASCII space, tab, CR
/// and LF (deliberately not the wider Unicode set that Rust's `str::trim`
/// takes). Shared with `ranges.rs`, which must trim `bytes=...` pieces with
/// the same semantics.
pub(super) fn trim_string(s: &str) -> &str {
    s.trim_matches(|c| matches!(c, ' ' | '\t' | '\n' | '\r'))
}

#[cfg(test)]
mod tests {
    use super::*;

    const MT: Option<i64> = Some(1_788_678_000); // Sun, 06 Sep 2026 07:00:00 GMT

    // ---- If-Match / If-Unmodified-Since → 412 ------------------------------

    #[test]
    fn no_preconditions_serves() {
        assert_eq!(
            check_preconditions(None, None, None, None, MT),
            CondOutcome::Serve
        );
    }

    #[test]
    fn if_match_star_serves() {
        assert_eq!(
            check_preconditions(Some("*"), None, None, None, MT),
            CondOutcome::Serve
        );
    }

    #[test]
    fn empty_header_values_are_cond_none() {
        // fs.go reads conditional headers via Header.Get, so a
        // present-but-empty header is indistinguishable from an absent one
        // (`if im := r.Header.Get("If-Match"); im != ""`). An empty If-Match
        // must NOT 412 (concrete etag semantics)…
        assert_eq!(
            check_preconditions(Some(""), None, None, None, MT),
            CondOutcome::Serve
        );
        // …and an empty If-None-Match must NOT short-circuit
        // If-Modified-Since: with a fresh IMS the revalidation still 304s.
        assert_eq!(
            check_preconditions(
                None,
                None,
                Some(""),
                Some("Sun, 06 Sep 2099 07:00:00 GMT"),
                MT
            ),
            CondOutcome::NotModified
        );
        // Whitespace-only is *not* empty to Header.Get (it is a real value);
        // the fs.go parse of "   " in If-Modified-Since fails → condNone.
        assert_eq!(
            check_preconditions(None, None, None, Some("   "), MT),
            CondOutcome::Serve
        );
    }

    #[test]
    fn if_match_concrete_etag_fails_412() {
        // No current etag → any concrete If-Match value fails.
        assert_eq!(
            check_preconditions(Some("\"abc\""), None, None, None, MT),
            CondOutcome::PreconditionFailed
        );
        // Whitespace/commas around segments are tolerated.
        assert_eq!(
            check_preconditions(Some(" \"x\", \"y\" "), None, None, None, MT),
            CondOutcome::PreconditionFailed
        );
    }

    #[test]
    fn if_match_garbage_fails_412() {
        assert_eq!(
            check_preconditions(Some("garbage"), None, None, None, MT),
            CondOutcome::PreconditionFailed
        );
    }

    /// Review finding #4 (PR #13): malformed ETag lists must follow Go's
    /// scan-and-resume loop, not a naive `split(',')`.
    #[test]
    fn etag_list_parsing_follows_go_scan_loop() {
        // `"a"garbage, *`: Go scans `"a"`, resumes at `garbage, *`; that scan
        // fails → If-Match 412 (Go-verified)…
        assert_eq!(
            check_preconditions(Some("\"a\"garbage, *"), None, None, None, MT),
            CondOutcome::PreconditionFailed
        );
        // …and condTrue (full 200) for If-None-Match.
        assert_eq!(
            check_preconditions(None, None, Some("\"a\"garbage, *"), None, MT),
            CondOutcome::Serve
        );
        // Without the garbage the remainder starts with a comma, scanning
        // continues, and the trailing `*` decides.
        assert_eq!(
            check_preconditions(Some("\"a\" , *"), None, None, None, MT),
            CondOutcome::Serve
        );
        assert_eq!(
            check_preconditions(None, None, Some("\"a\" , *"), None, MT),
            CondOutcome::NotModified
        );
        // Leading commas are skipped by the go1.26 loop.
        assert_eq!(
            check_preconditions(Some(", , *"), None, None, None, MT),
            CondOutcome::Serve
        );
        // Garbage alone breaks the first scan.
        assert_eq!(
            check_preconditions(Some("garbage, *"), None, None, None, MT),
            CondOutcome::PreconditionFailed
        );
        assert_eq!(
            check_preconditions(None, None, Some("garbage, *"), None, MT),
            CondOutcome::Serve
        );
        // Trimming is ASCII space/tab/CR/LF (textproto.TrimString): a tab is
        // trimmed so `\t*` still matches, but a vertical tab is not and
        // breaks the scan.
        assert_eq!(
            check_preconditions(Some("\t*"), None, None, None, MT),
            CondOutcome::Serve
        );
        assert_eq!(
            check_preconditions(Some("\u{b}*"), None, None, None, MT),
            CondOutcome::PreconditionFailed
        );
    }

    #[test]
    fn if_unmodified_since_rules() {
        // mtime ≤ IUS date → serve.
        assert_eq!(
            check_preconditions(None, Some("Sun, 06 Sep 2026 07:00:00 GMT"), None, None, MT),
            CondOutcome::Serve
        );
        assert_eq!(
            check_preconditions(None, Some("Mon, 07 Sep 2026 07:00:00 GMT"), None, None, MT),
            CondOutcome::Serve
        );
        // mtime later than IUS → 412.
        assert_eq!(
            check_preconditions(None, Some("Sat, 05 Sep 2026 07:00:00 GMT"), None, None, MT),
            CondOutcome::PreconditionFailed
        );
    }

    #[test]
    fn if_unmodified_since_unparseable_or_no_mtime_is_condnone() {
        assert_eq!(
            check_preconditions(None, Some("not a date"), None, None, MT),
            CondOutcome::Serve
        );
        assert_eq!(
            check_preconditions(
                None,
                Some("Sun, 06 Sep 2026 07:00:00 GMT"),
                None,
                None,
                None
            ),
            CondOutcome::Serve
        );
    }

    #[test]
    fn if_match_defeats_ius() {
        // If-Match present (even failing) → IUS not consulted.
        assert_eq!(
            check_preconditions(
                Some("\"x\""),
                Some("Mon, 01 Jan 2000 00:00:00 GMT"),
                None,
                None,
                MT
            ),
            CondOutcome::PreconditionFailed // If-Match fails first
        );
        assert_eq!(
            check_preconditions(
                Some("*"),
                Some("Mon, 01 Jan 2000 00:00:00 GMT"),
                None,
                None,
                MT
            ),
            CondOutcome::Serve // If-Match `*` → condTrue; IUS skipped
        );
    }

    // ---- If-None-Match → 304 / short-circuit --------------------------------

    #[test]
    fn inm_star_is_304() {
        assert_eq!(
            check_preconditions(None, None, Some("*"), None, MT),
            CondOutcome::NotModified
        );
        assert_eq!(
            check_preconditions(None, None, Some("\"a\", *"), None, MT),
            CondOutcome::NotModified
        );
    }

    #[test]
    fn inm_concrete_etag_serves_and_skips_ims() {
        // No current etag → concrete INM never matches (condTrue → serve).
        assert_eq!(
            check_preconditions(None, None, Some("\"abc\""), None, MT),
            CondOutcome::Serve
        );
        assert_eq!(
            check_preconditions(None, None, Some("W/\"abc\""), None, MT),
            CondOutcome::Serve
        );
        // Crucial: a concrete (non-matching) etag must NOT fall through to
        // If-Modified-Since — the file "changed", so a stale IMS would
        // otherwise wrongly produce a 304.
        assert_eq!(
            check_preconditions(
                None,
                None,
                Some("\"abc\""),
                Some("Sun, 06 Sep 2099 07:00:00 GMT"),
                MT
            ),
            CondOutcome::Serve
        );
    }

    #[test]
    fn inm_garbage_serves() {
        // scanETag fails on every segment → condTrue.
        assert_eq!(
            check_preconditions(None, None, Some("garbage"), None, MT),
            CondOutcome::Serve
        );
    }

    // ---- If-Modified-Since → 304 (only without If-None-Match) ---------------

    #[test]
    fn ims_stale_date_is_304() {
        // IMS equal to mtime (nothing changed) or later → 304.
        assert_eq!(
            check_preconditions(None, None, None, Some("Sun, 06 Sep 2026 07:00:00 GMT"), MT),
            CondOutcome::NotModified
        );
        assert_eq!(
            check_preconditions(None, None, None, Some("Mon, 07 Sep 2026 07:00:00 GMT"), MT),
            CondOutcome::NotModified
        );
    }

    #[test]
    fn ims_older_than_mtime_serves() {
        // IMS before the mtime → the file changed since the cached copy.
        assert_eq!(
            check_preconditions(None, None, None, Some("Wed, 01 Jan 2025 00:00:00 GMT"), MT),
            CondOutcome::Serve
        );
        assert_eq!(
            check_preconditions(None, None, None, Some("Tue, 01 Sep 2026 07:00:00 GMT"), MT),
            CondOutcome::Serve
        );
    }

    #[test]
    fn ims_unparseable_or_no_mtime_serves() {
        assert_eq!(
            check_preconditions(None, None, None, Some("garbage"), MT),
            CondOutcome::Serve
        );
        assert_eq!(
            check_preconditions(
                None,
                None,
                None,
                Some("Sun, 06 Sep 2026 07:00:00 GMT"),
                None
            ),
            CondOutcome::Serve
        );
    }

    #[test]
    fn inm_present_blocks_ims() {
        // INM absent semantics: IMS is only consulted when INM is absent.
        assert_eq!(
            check_preconditions(
                None,
                None,
                Some("*"),
                Some("Wed, 01 Jan 2025 00:00:00 GMT"),
                MT
            ),
            CondOutcome::NotModified
        );
        assert_eq!(
            check_preconditions(
                None,
                None,
                Some("\"x\""),
                Some("Sun, 06 Sep 2026 07:00:00 GMT"),
                MT
            ),
            CondOutcome::Serve // concrete etag wins; stale IMS ignored
        );
    }

    // ---- If-Range -----------------------------------------------------------

    #[test]
    fn if_range_absent_keeps_range() {
        assert!(if_range_keeps_range(None, MT));
        assert!(if_range_keeps_range(Some(""), MT));
    }

    #[test]
    fn if_range_etag_never_matches_absent_etag() {
        assert!(!if_range_keeps_range(Some("\"abc\""), MT));
        assert!(!if_range_keeps_range(Some("W/\"abc\""), MT));
    }

    #[test]
    fn if_range_date_must_equal_mtime_second_precision() {
        assert!(if_range_keeps_range(
            Some("Sun, 06 Sep 2026 07:00:00 GMT"),
            MT
        ));
        assert!(!if_range_keeps_range(
            Some("Sat, 05 Sep 2026 07:00:00 GMT"),
            MT
        ));
        // Not an etag, not a parseable date → drop.
        assert!(!if_range_keeps_range(Some("garbage"), MT));
        // No mtime at all → drop (fs.go: modtime.IsZero() → condFalse — the
        // year-1 zero Time, i.e. "no mtime", not the epoch).
        assert!(!if_range_keeps_range(
            Some("Sun, 06 Sep 2026 07:00:00 GMT"),
            None
        ));
        // fs.go checkIfRange has no zero-time gate on the *epoch*: the date
        // compare is t.Unix() == modtime.Unix(), so an exactly-epoch mtime
        // keeps the Range on a matching epoch date.
        assert!(if_range_keeps_range(
            Some("Thu, 01 Jan 1970 00:00:00 GMT"),
            Some(0)
        ));
    }

    // ---- scan_etag ----------------------------------------------------------

    #[test]
    fn scan_etag_accepts_valid_and_rejects_garbage() {
        assert_eq!(scan_etag("\"abc\"").unwrap().0, "\"abc\"");
        assert_eq!(scan_etag("W/\"abc\"").unwrap().0, "W/\"abc\"");
        // The remainder after the consumed ETag is returned (Go scanETag).
        assert_eq!(scan_etag(" \"a\"rest").unwrap(), ("\"a\"", "rest"));
        assert_eq!(scan_etag("\"a b\""), None); // space not allowed in etag
        assert_eq!(scan_etag("abc"), None); // no opening quote
        assert_eq!(scan_etag("\"ab"), None); // unterminated
    }
}

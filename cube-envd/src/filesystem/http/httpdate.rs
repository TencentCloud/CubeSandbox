// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! HTTP-date formatting and parsing, behaviour-compatible with Go's
//! `net/http` (go1.26): `http.ParseTime` tries the three HTTP/1.1 layouts in
//! order — IMF-fixdate `Mon, 02 Jan 2006 15:04:05 GMT`, RFC 850
//! `Monday, 02-Jan-06 15:04:05 MST`, asctime `Mon Jan _2 15:04:05 2006` —
//! and `Format(TimeFormat)` renders IMF-fixdate. Implemented by hand (not on
//! the `time` crate's parser) so the quirks match Go exactly, verified
//! against `time.Parse` on go1.26.5 (TZ=UTC, the environment both binaries
//! run in):
//!
//! - weekday/month names match case-insensitively (`time.match`); literal
//!   text (`GMT`, `-`, `:`, `,`) is byte-exact; layout spaces match a run of
//!   one or more spaces; any leading or trailing text fails;
//! - weekday names are validated but never cross-checked against the date
//!   (go1.26 dropped the consistency check: "Ignore weekday except for error
//!   checking");
//! - IMF-fixdate and RFC 850 days are exactly two digits, asctime days one
//!   or two; hours accept one or two digits everywhere; minutes and seconds
//!   exactly two; a fractional second directly after the seconds is
//!   consumed and dropped (Go's stdSecond special case);
//! - ranges: day within the month (leap years included), hour < 24,
//!   minute/second < 60; RFC 850 two-digit years map 69-99 → 19xx, else
//!   20xx; asctime/IMF years are exactly four digits (0000-9999);
//! - the RFC 850 zone token accepts everything Go's `parseTimeZone` does:
//!   `UTC`, `GMT` with an optional ±offset (magnitude ≤ 23), three
//!   upper-case letters, four ending in `T` (or `WITA`), five ending in
//!   `T`, and the mixed-case `ChST`/`MeST`. Unknown names resolve to a zero
//!   offset — Go's "fabricated location" behaviour, which on a UTC host is
//!   also what it does for every known abbreviation it can't find locally.
//!   asctime has no zone (parses as UTC) and IMF-fixdate's `GMT` is a
//!   literal, so `+0000`, a trailing zone or trailing spaces all fail.

/// `Mon, 02 Jan 2006 15:04:05 GMT` — Go `time.Time.Format(TimeFormat)`, for
/// a Unix timestamp already truncated to whole seconds. Works for the full
/// i64 range (year may exceed four digits or turn negative, like Go's
/// `appendInt`).
pub fn format_http_date(unix_secs: i64) -> String {
    let days = unix_secs.div_euclid(86_400);
    let (year, month, day) = civil_from_days(days);
    // 1970-01-01 (days == 0) was a Thursday.
    let weekday = (days + 4).rem_euclid(7) as usize;
    const DAY_NAMES: [&str; 7] = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
    const MONTH_NAMES: [&str; 12] = [
        "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
    ];
    let secs_of_day = unix_secs.rem_euclid(86_400);
    format!(
        "{}, {:02} {} {} {:02}:{:02}:{:02} GMT",
        DAY_NAMES[weekday],
        day,
        MONTH_NAMES[(month - 1) as usize],
        format_year(year),
        secs_of_day / 3600,
        (secs_of_day % 3600) / 60,
        secs_of_day % 60,
    )
}

/// Go `appendInt(x, 4)`: sign, then the absolute value zero-padded to four
/// digits (more digits are printed as-is).
fn format_year(year: i64) -> String {
    if year < 0 {
        format!("-{:04}", year.unsigned_abs())
    } else {
        format!("{:04}", year)
    }
}

/// Go `http.ParseTime`: try the three HTTP/1.1 layouts in order. `None`
/// means "condition does not apply" to the caller.
pub fn parse_http_date(s: &str) -> Option<i64> {
    parse_imf_fixdate(s)
        .or_else(|| parse_rfc850(s))
        .or_else(|| parse_asctime(s))
}

const DAY_NAMES_SHORT: [&str; 7] = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
const DAY_NAMES_LONG: [&str; 7] = [
    "Sunday",
    "Monday",
    "Tuesday",
    "Wednesday",
    "Thursday",
    "Friday",
    "Saturday",
];
const MONTH_NAMES: [&str; 12] = [
    "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
];

/// `Mon, 02 Jan 2006 15:04:05 GMT` — `GMT` is a literal, so `+0000` or any
/// other zone spelling fails here and falls through to the other layouts.
fn parse_imf_fixdate(s: &str) -> Option<i64> {
    let rest = lookup_name(&DAY_NAMES_SHORT, s)?.1;
    let rest = skip_space_run(rest.strip_prefix(',')?)?;
    let (day, rest) = getnum(rest, true)?;
    let rest = skip_space_run(rest)?;
    let (month, rest) = lookup_name(&MONTH_NAMES, rest)?;
    let rest = skip_space_run(rest)?;
    let (year, rest) = getnum4(rest)?;
    let rest = skip_space_run(rest)?;
    let (hour, rest) = getnum(rest, false)?;
    let (min, sec, rest) = parse_hms(rest)?;
    let rest = skip_frac_second(rest);
    let rest = skip_space_run(rest)?;
    let rest = rest.strip_prefix("GMT")?;
    if !rest.is_empty() {
        return None; // Go: "extra text"
    }
    validate_to_unix(year, month, day, hour, min, sec)
}

/// `Monday, 02-Jan-06 15:04:05 MST`.
fn parse_rfc850(s: &str) -> Option<i64> {
    let rest = lookup_name(&DAY_NAMES_LONG, s)?.1;
    let rest = skip_space_run(rest.strip_prefix(',')?)?;
    let (day, rest) = getnum(rest, true)?;
    let rest = rest.strip_prefix('-')?;
    let (month, rest) = lookup_name(&MONTH_NAMES, rest)?;
    let rest = rest.strip_prefix('-')?;
    let (yy, rest) = getnum(rest, true)?;
    let year = if yy >= 69 { yy + 1900 } else { yy + 2000 };
    let rest = skip_space_run(rest)?;
    let (hour, rest) = getnum(rest, false)?;
    let (min, sec, rest) = parse_hms(rest)?;
    let rest = skip_frac_second(rest);
    let rest = skip_space_run(rest)?;
    let rest = parse_zone_token(rest)?;
    if !rest.is_empty() {
        return None; // Go: "extra text"
    }
    validate_to_unix(year, month, day, hour, min, sec)
}

/// `Mon Jan _2 15:04:05 2006` — no zone; parses as UTC.
fn parse_asctime(s: &str) -> Option<i64> {
    let rest = lookup_name(&DAY_NAMES_SHORT, s)?.1;
    let rest = skip_space_run(rest)?;
    let (month, rest) = lookup_name(&MONTH_NAMES, rest)?;
    let rest = skip_space_run(rest)?;
    // stdUnderDay skips one more space on top of the run above; getnum then
    // accepts one or two digits.
    let rest = rest.strip_prefix(' ').unwrap_or(rest);
    let (day, rest) = getnum(rest, false)?;
    let rest = skip_space_run(rest)?;
    let (hour, rest) = getnum(rest, false)?;
    let (min, sec, rest) = parse_hms(rest)?;
    let rest = skip_frac_second(rest);
    let rest = skip_space_run(rest)?;
    let (year, rest) = getnum4(rest)?;
    if !rest.is_empty() {
        return None; // Go: "extra text" (a trailing zone fails here)
    }
    validate_to_unix(year, month, day, hour, min, sec)
}

/// Shared `HH:MM:SS` tail (minutes and seconds fixed two-digit).
fn parse_hms(rest: &str) -> Option<(i64, i64, &str)> {
    let rest = rest.strip_prefix(':')?;
    let (min, rest) = getnum(rest, true)?;
    let rest = rest.strip_prefix(':')?;
    let (sec, rest) = getnum(rest, true)?;
    Some((min, sec, rest))
}

/// Go `time.match`/`lookup`: first case-insensitive prefix match.
fn lookup_name<'a>(names: &[&'a str], value: &'a str) -> Option<(usize, &'a str)> {
    names
        .iter()
        .position(|n| {
            let b = value.as_bytes();
            b.len() >= n.len() && b[..n.len()].eq_ignore_ascii_case(n.as_bytes())
        })
        .map(|i| (i, &value[names[i].len()..]))
}

/// Go `getnum`: one digit, or exactly two when `fixed`.
fn getnum(value: &str, fixed: bool) -> Option<(i64, &str)> {
    let b = value.as_bytes();
    if b.is_empty() || !b[0].is_ascii_digit() {
        return None;
    }
    if b.len() < 2 || !b[1].is_ascii_digit() {
        if fixed {
            return None;
        }
        return Some(((b[0] - b'0') as i64, &value[1..]));
    }
    Some((((b[0] - b'0') * 10 + (b[1] - b'0')) as i64, &value[2..]))
}

/// Go `stdLongYear`: exactly four ASCII digits (0000-9999).
fn getnum4(value: &str) -> Option<(i64, &str)> {
    let b = value.as_bytes();
    if b.len() < 4 || !b[..4].iter().all(|c| c.is_ascii_digit()) {
        return None;
    }
    Some((
        (b[0] - b'0') as i64 * 1000
            + (b[1] - b'0') as i64 * 100
            + (b[2] - b'0') as i64 * 10
            + (b[3] - b'0') as i64,
        &value[4..],
    ))
}

/// A layout space matches a run of one or more spaces (Go `skip` requires at
/// least one, then `cutspace` consumes the rest).
fn skip_space_run(value: &str) -> Option<&str> {
    let rest = value.strip_prefix(' ')?;
    Some(rest.trim_start_matches(' '))
}

/// Go's stdSecond special case: a `,`/`.` followed by a digit in the input is
/// consumed as a fractional second even though the layout has none. The
/// value is dropped — it never affects `Unix()` or the truncated mtime
/// comparisons.
fn skip_frac_second(rest: &str) -> &str {
    let b = rest.as_bytes();
    if b.len() >= 2 && (b[0] == b'.' || b[0] == b',') && b[1].is_ascii_digit() {
        // Go scans the digit run with `for ; n < len(value) &&
        // isDigit(value, n); n++` — only the digits immediately after the
        // separator. Counting every digit in the remainder would swallow
        // the following field (the asctime year, a numeric zone) and turn
        // e.g. "07:00:00.5 2026" into "2026" leftover → parse failure.
        let n = 2 + b[2..].iter().take_while(|c| c.is_ascii_digit()).count();
        // n counts ASCII bytes only, so it always lands on a char boundary;
        // `get` keeps that invariant explicit (a mid-character index used to
        // panic on obs-text bytes surviving as U+FFFD).
        return rest.get(n..).unwrap_or(rest);
    }
    rest
}

/// Go `parseTimeZone` (via the stdTZ chunk): accepts a zone token and returns
/// the unconsumed text. On a UTC host every accepted token resolves to a
/// zero offset, so the offset itself is not modelled.
fn parse_zone_token(value: &str) -> Option<&str> {
    if let Some(rest) = value.strip_prefix("UTC") {
        return Some(rest);
    }
    let b = value.as_bytes();
    if b.len() < 3 {
        return None;
    }
    // The only zones with a lower-case letter.
    if value.starts_with("ChST") || value.starts_with("MeST") {
        return Some(&value[4..]);
    }
    // GMT may carry an hour offset; a bad one just consumes less (Go
    // parseGMT), leaving the text to fail as "extra text".
    if let Some(rest) = value.strip_prefix("GMT") {
        for sign in ['+', '-'] {
            if let Some(after) = rest.strip_prefix(sign) {
                let nd = after.bytes().take_while(|c| c.is_ascii_digit()).count();
                if nd > 0 {
                    // leadingInt: overflow or magnitude > 23 → offset rejected
                    return Some(match after[..nd].parse::<u64>() {
                        Ok(x) if x <= 23 => &after[nd..],
                        _ => rest,
                    });
                }
                break;
            }
        }
        return Some(rest);
    }
    // Special case: a signed numeric offset like `+0000` (magnitude ≤ 23).
    if b[0] == b'+' || b[0] == b'-' {
        let nd = b[1..].iter().take_while(|c| c.is_ascii_digit()).count();
        if nd == 0 {
            return None;
        }
        return match value[1..1 + nd].parse::<u64>() {
            Ok(x) if x <= 23 => Some(&value[1 + nd..]),
            _ => None,
        };
    }
    // A run of upper-case letters: 3 always; 4 must end in `T` (or WITA);
    // 5 must end in `T`; 6 or more is rejected.
    let n_upper = b
        .iter()
        .take(6)
        .take_while(|c| c.is_ascii_uppercase())
        .count();
    match n_upper {
        3 => Some(&value[3..]),
        4 if b[3] == b'T' || &value[..4] == "WITA" => Some(&value[4..]),
        5 if b[4] == b'T' => Some(&value[5..]),
        _ => None,
    }
}

/// Range-validate like Go's parse epilogue (`day out of range`,
/// `hour/minute/second out of range`), then convert to Unix seconds. `month`
/// is the 0-based lookup position.
fn validate_to_unix(
    year: i64,
    month: usize,
    day: i64,
    hour: i64,
    min: i64,
    sec: i64,
) -> Option<i64> {
    if day < 1 || day > days_in_month(year, month + 1) {
        return None;
    }
    if hour >= 24 || min >= 60 || sec >= 60 {
        return None;
    }
    let days = days_from_civil(year, month as u32 + 1, day as u32);
    Some(days * 86_400 + hour * 3_600 + min * 60 + sec)
}

fn is_leap(year: i64) -> bool {
    (year % 4 == 0 && year % 100 != 0) || year % 400 == 0
}

fn days_in_month(year: i64, month: usize) -> i64 {
    const BEFORE: [i64; 13] = [0, 31, 59, 90, 120, 151, 181, 212, 243, 273, 304, 334, 365];
    if month == 2 && is_leap(year) {
        return 29;
    }
    BEFORE[month] - BEFORE[month - 1]
}

/// Howard Hinnant's `civil_from_days` (day 0 = 1970-01-01), valid for
/// negative day counts.
fn civil_from_days(z: i64) -> (i64, u32, u32) {
    let z = z + 719_468;
    let era = (if z >= 0 { z } else { z - 146_096 }) / 146_097;
    let doe = (z - era * 146_097) as u64; // [0, 146096]
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365; // [0, 399]
    let y = yoe as i64 + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100); // [0, 365]
    let mp = (5 * doy + 2) / 153; // [0, 11]
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32; // [1, 31]
    let m = if mp < 10 { mp + 3 } else { mp - 9 } as u32; // [1, 12]
    (if m <= 2 { y + 1 } else { y }, m, d)
}

/// Inverse of `civil_from_days` (Howard Hinnant's `days_from_civil`).
fn days_from_civil(y: i64, m: u32, d: u32) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = (if y >= 0 { y } else { y - 399 }) / 400;
    let yoe = (y - era * 400) as u64; // [0, 399]
    let mp = ((m + 9) % 12) as u64; // [0, 11]
    let doy = (153 * mp + 2) / 5 + d as u64 - 1; // [0, 365]
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy; // [0, 146096]
    era * 146_097 + doe as i64 - 719_468
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn http_date_epoch_and_known() {
        // 1970-01-01T00:00:00Z was a Thursday.
        assert_eq!(format_http_date(0), "Thu, 01 Jan 1970 00:00:00 GMT");
        // Verified civil dates (python datetime).
        assert_eq!(
            format_http_date(1_788_678_000),
            "Sun, 06 Sep 2026 07:00:00 GMT"
        );
        // Leap day (2024 was a leap year); Thu 2024-02-29 12:34:56 UTC.
        assert_eq!(
            format_http_date(1_709_210_096),
            "Thu, 29 Feb 2024 12:34:56 GMT"
        );
    }

    #[test]
    fn http_date_roundtrip() {
        for secs in [0, 1_788_678_000, 1_709_210_096] {
            let s = format_http_date(secs);
            assert_eq!(parse_http_date(&s), Some(secs), "{s}");
        }
    }

    /// The time crate rejects these; Go formats them fine and a review probe
    /// hit the panic on a real tmpfs.
    #[test]
    fn http_date_extremes_match_go_appendint() {
        // time.Unix(-1, 0).UTC().Format(http.TimeFormat)
        assert_eq!(format_http_date(-1), "Wed, 31 Dec 1969 23:59:59 GMT");
        // Ground truth: go run — time.Unix(253402300800, 0) (year 10000).
        assert_eq!(
            format_http_date(253_402_300_800),
            "Sat, 01 Jan 10000 00:00:00 GMT"
        );
        // Year 0000, cross-checked against http.ParseTime on go1.26.5.
        assert_eq!(
            format_http_date(-62_145_680_400),
            "Wed, 06 Sep 0000 07:00:00 GMT"
        );
    }

    /// Ground truth from `http.ParseTime` on go1.26.5 (TZ=UTC).
    #[test]
    fn parse_accepts_rfc850_and_asctime_like_go() {
        let ts = 1_788_678_000; // Sun, 06 Sep 2026 07:00:00 GMT
        for (input, want) in [
            ("Sunday, 06-Sep-26 07:00:00 GMT", ts),
            ("Sun Sep  6 07:00:00 2026", ts),
            ("Sun Sep 6 07:00:00 2026", ts),
            ("Sun Sep 6 7:04:05 2026", 1_788_678_245),
            ("Sun, 06 Sep 2026 7:04:05 GMT", 1_788_678_245),
            // Names are case-insensitive…
            ("SUNDAY, 06-SEP-26 07:00:00 GMT", ts),
            // …the weekday is never cross-checked against the date…
            ("Fri, 06 Sep 2026 07:00:00 GMT", ts),
            // …and a fractional second is consumed and dropped.
            ("Sun, 06 Sep 2026 07:00:00.5 GMT", ts),
            // Zone tokens: anything parseTimeZone accepts → zero offset.
            ("Sunday, 06-Sep-26 07:00:00 UTC", ts),
            ("Sunday, 06-Sep-26 07:00:00 EST", ts),
            ("Sunday, 06-Sep-26 07:00:00 ABC", ts),
            ("Sunday, 06-Sep-26 07:00:00 WITA", ts),
            ("Sunday, 06-Sep-26 07:00:00 ChST", ts),
            ("Sunday, 06-Sep-26 07:00:00 CHST", ts),
            ("Sunday, 06-Sep-26 07:00:00 +0000", ts),
            ("Sunday, 06-Sep-26 07:00:00 GMT-8", ts),
            ("Sunday, 06-Sep-26 07:00:00 GMT+0", ts),
            ("Sunday, 06-Sep-26 07:00:00 GMT+05", ts),
            // Two-digit year mapping: 69-99 → 19xx, else 20xx.
            ("Sunday, 06-Sep-69 07:00:00 GMT", -10_083_600),
            ("Sunday, 06-Sep-68 07:00:00 GMT", 3_114_140_400),
            // Four-digit year 0000.
            ("Sun, 06 Sep 0000 07:00:00 GMT", -62_145_680_400),
        ] {
            assert_eq!(parse_http_date(input), Some(want), "input {input:?}");
        }
    }

    /// Round-5 review: the fractional-second scan must stop at the first
    /// non-digit, not consume every digit in the remainder — otherwise the
    /// asctime year or a numeric zone gets swallowed.
    #[test]
    fn fractional_second_consumes_only_the_adjacent_digit_run() {
        let ts = 1_788_678_000; // Sun, 06 Sep 2026 07:00:00 GMT
        for input in [
            "Sun, 06 Sep 2026 07:00:00.5 GMT",    // IMF-fixdate
            "Sunday, 06-Sep-26 07:00:00.5 GMT",   // RFC 850
            "Sunday, 06-Sep-26 07:00:00.5 GMT+0", // …followed by a zone
            "Sun Sep  6 07:00:00.5 2026",         // asctime, year follows
            "Sun Sep 6 07:00:00,5 2026",          // comma separator
            "Sunday, 06-Sep-26 07:00:00.123456789 GMT",
        ] {
            assert_eq!(parse_http_date(input), Some(ts), "input {input:?}");
        }
        // A non-digit right after the run ends it; the rest must still parse.
        assert_eq!(parse_http_date("Sun, 06 Sep 2026 07:00:00.5x GMT"), None);
        // Separator without a following digit is not a fraction.
        assert_eq!(parse_http_date("Sun, 06 Sep 2026 07:00:00. GMT"), None);
    }

    /// Obs-text bytes survive lossy header conversion as U+FFFD; the old
    /// byte-count slicing landed inside that character and panicked (HTTP
    /// 500 through CatchPanicLayer).
    #[test]
    fn fractional_second_with_obs_text_is_boundary_safe() {
        for bad in [
            "Sun, 06 Sep 2026 07:00:00.5\u{FFFD}1 GMT",
            "Sun, 06 Sep 2026 07:00:00.5\u{FFFD} GMT",
            "Sun, 06 Sep 2026 07:00:00\u{FFFD}.5 GMT",
            "Sun, 06 Sep 2026 07:00:00.\u{FFFD}5 GMT",
        ] {
            assert_eq!(parse_http_date(bad), None, "accepted {bad:?}");
        }
    }

    /// Panic regression: every byte-index slice in the parsers must land on a
    /// char boundary, for all three layouts and arbitrary obs-text tails.
    #[test]
    fn parse_never_panics_on_arbitrary_bytes() {
        let pieces: [&str; 7] = [".", ",", "5", "x", "\u{FFFD}", " GMT", "2026"];
        let heads = [
            "Sun, 06 Sep 2026 07:00:00",
            "Sunday, 06-Sep-26 07:00:00",
            "Sun Sep  6 07:00:00",
        ];
        let mut buf = String::new();
        for head in heads {
            for a in pieces {
                for b in pieces {
                    for c in pieces {
                        buf.clear();
                        buf.push_str(head);
                        buf.push_str(a);
                        buf.push_str(b);
                        buf.push_str(c);
                        let _ = parse_http_date(&buf);
                    }
                }
            }
        }
    }

    /// Ground truth from `http.ParseTime` on go1.26.5 (TZ=UTC).
    #[test]
    fn parse_rejects_like_go() {
        for bad in [
            "",
            "   ",
            "not a date",
            "garbage",
            // IMF-fixdate is strict: literal GMT, fixed two-digit day.
            "Sun, 06 Sep 2026 07:00:00 +0000",
            "Sun, 06 Sep 2026 07:00:00",     // no zone
            "Sun,  6 Sep 2026 07:00:00 GMT", // day must be two digits
            // Literals are case-sensitive even though names are not.
            "sun, 06 sep 2026 07:00:00 gmt",
            // No leading/trailing text anywhere.
            "Sun, 06 Sep 2026 07:00:00 GMT ",
            "Sun Sep  6 07:00:00 2026 GMT", // asctime has no zone
            // Range validation.
            "Sun, 32 Sep 2026 07:00:00 GMT",
            "Sun, 31 Feb 2026 07:00:00 GMT",
            "Sun, 00 Sep 2026 07:00:00 GMT",
            "Sun, 06 Sep 2026 24:00:00 GMT",
            "Sun, 06 Sep 2026 07:00:60 GMT",
            // Zone token shapes parseTimeZone rejects.
            "Sunday, 06-Sep-26 07:00:00 ABCD",
            "Sunday, 06-Sep-26 07:00:00 GMT+24",
            "Sunday, 06-Sep-26 07:00:00 -0500", // 500 > 23
            "Sunday, 06-Sep-26 07:00:00 Z",
        ] {
            assert_eq!(parse_http_date(bad), None, "accepted {bad:?}");
        }
    }
}

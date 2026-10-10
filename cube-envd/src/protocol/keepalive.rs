// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Keepalive cadence for streaming responses: the `Keepalive-Ping-Interval`
//! request header and its default.
//!
//! Mirrors upstream `permissions.GetKeepAliveTicker`. cube-envd keeps 30s
//! rather than the filesystem watch's 90s (the LB in front of CubeProxy has an
//! unknown idle timeout, so 30s stays under any LB ≥ 30s).
//!
//! Source: `connect.rs` (keepalive half).

/// Default keepalive ping cadence for a quiet Start stream. cube-envd keeps
/// 30s rather than upstream's 90s: the LB in front of CubeProxy has an unknown
/// idle timeout (typically 60s), so 30s stays safely under any LB that is
/// >= 30s.
pub const DEFAULT_KEEPALIVE_INTERVAL: std::time::Duration = std::time::Duration::from_secs(30);

/// Parse the `Keepalive-Ping-Interval` request header (integer seconds).
///
/// Mirrors upstream `permissions.GetKeepAliveTicker`: the header tunes only the
/// cadence; keepalive is always on for a quiet Start stream and there is no
/// request field that gates it. An absent, non-numeric, non-positive, or
/// oversized value falls back to [`DEFAULT_KEEPALIVE_INTERVAL`].
///
/// The value is parsed as `u32` deliberately: an interval above `u32::MAX`
/// seconds (~136 years) is rejected rather than letting `Duration::from_secs`
/// overflow. Upstream feeds the header straight into `time.NewTicker`, which
/// panics for 0/negative/duration-overflowing values; cube-envd degrades to
/// the default instead of crashing.
pub fn keepalive_interval_from_headers(headers: &axum::http::HeaderMap) -> std::time::Duration {
    let secs = headers
        .get("keepalive-ping-interval")
        .and_then(|v| v.to_str().ok())
        .and_then(|s| s.trim().parse::<u32>().ok())
        .filter(|&s| s > 0);
    match secs {
        Some(s) => std::time::Duration::from_secs(u64::from(s)),
        None => DEFAULT_KEEPALIVE_INTERVAL,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn keepalive_interval_header_parsing() {
        let mut headers = axum::http::HeaderMap::new();
        // Absent -> default.
        assert_eq!(
            keepalive_interval_from_headers(&headers),
            DEFAULT_KEEPALIVE_INTERVAL
        );
        // Integer seconds override (case-insensitive header name).
        headers.insert("Keepalive-Ping-Interval", "90".parse().unwrap());
        assert_eq!(
            keepalive_interval_from_headers(&headers),
            std::time::Duration::from_secs(90)
        );
        // Non-numeric / zero / negative / oversized -> default.
        for bad in ["abc", "0", "-5", "", "4294967296", "18446744073709551616"] {
            headers.insert("keepalive-ping-interval", bad.parse().unwrap());
            assert_eq!(
                keepalive_interval_from_headers(&headers),
                DEFAULT_KEEPALIVE_INTERVAL,
                "value {bad:?} should fall back to default"
            );
        }
    }
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! `Connect-Timeout-Ms` request-header parsing.
//!
//! Source: `connect.rs` (timeout half).

/// Parse the `Connect-Timeout-Ms` request header.
pub fn timeout_from_headers(headers: &axum::http::HeaderMap) -> Option<std::time::Duration> {
    let raw = headers.get("connect-timeout-ms")?.to_str().ok()?;
    let ms: u64 = raw.trim().parse().ok()?;
    Some(std::time::Duration::from_millis(ms))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn timeout_header_parsing() {
        let mut headers = axum::http::HeaderMap::new();
        assert!(timeout_from_headers(&headers).is_none());
        headers.insert("connect-timeout-ms", "1500".parse().unwrap());
        assert_eq!(
            timeout_from_headers(&headers),
            Some(std::time::Duration::from_millis(1500))
        );
    }
}

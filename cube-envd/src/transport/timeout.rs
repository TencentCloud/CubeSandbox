// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use std::time::Duration;

use connectrpc::RequestContext;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ConnectTimeoutPolicy {
    NoDeadline,
    ActiveVmDeadline(Duration),
    Invalid,
}

pub fn parse_connect_timeout_ms(raw: Option<&str>) -> ConnectTimeoutPolicy {
    let Some(raw) = raw else {
        return ConnectTimeoutPolicy::NoDeadline;
    };
    if raw.is_empty() {
        return ConnectTimeoutPolicy::NoDeadline;
    }
    let Ok(value) = raw.parse::<i64>() else {
        return ConnectTimeoutPolicy::Invalid;
    };
    // Go's handler multiplies a signed machine integer into time.Duration.
    // Preserve its wrapping conversion; only positive durations set a lifetime.
    let nanos = value.wrapping_mul(1_000_000);
    if nanos <= 0 {
        ConnectTimeoutPolicy::NoDeadline
    } else {
        ConnectTimeoutPolicy::ActiveVmDeadline(Duration::from_nanos(nanos as u64))
    }
}

pub fn extract_connect_timeout(ctx: &RequestContext) -> ConnectTimeoutPolicy {
    parse_connect_timeout_ms(
        ctx.headers()
            .get("connect-timeout-ms")
            .and_then(|value| value.to_str().ok()),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn timeout_matrix() {
        assert_eq!(
            parse_connect_timeout_ms(None),
            ConnectTimeoutPolicy::NoDeadline
        );
        assert_eq!(
            parse_connect_timeout_ms(Some("0")),
            ConnectTimeoutPolicy::NoDeadline
        );
        assert_eq!(
            parse_connect_timeout_ms(Some("-1")),
            ConnectTimeoutPolicy::NoDeadline
        );
        assert!(matches!(
            parse_connect_timeout_ms(Some("1000")),
            ConnectTimeoutPolicy::ActiveVmDeadline(_)
        ));
        assert_eq!(
            parse_connect_timeout_ms(Some("")),
            ConnectTimeoutPolicy::NoDeadline
        );
        assert_eq!(
            parse_connect_timeout_ms(Some("nope")),
            ConnectTimeoutPolicy::Invalid
        );
    }

    #[test]
    fn timeout_duration_conversion_matches_go_signed_wrapping() {
        assert_eq!(
            parse_connect_timeout_ms(Some("9223372036855")),
            ConnectTimeoutPolicy::NoDeadline
        );
        assert!(matches!(
            parse_connect_timeout_ms(Some("9223372036854")),
            ConnectTimeoutPolicy::ActiveVmDeadline(_)
        ));
    }
}

// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use std::time::Duration;

use connectrpc::RequestContext;

/// Preserve the Start lifetime header without the transport applying its own
/// request deadline before Process validation (notably zero/negative values).
#[derive(Clone)]
pub(crate) struct ProcessTimeout(pub http::HeaderValue);

#[derive(Clone, Copy)]
struct RequestStarted(tokio::time::Instant);

pub(crate) fn process_timeout(request: &mut http::Request<axum::body::Body>) {
    // Upstream's stdin/PTY writes and CloseStdin wait for the pipe, not the
    // request context. Do not let the transport abort these handler futures.
    if matches!(
        request.uri().path(),
        "/process.Process/SendInput"
            | "/process.Process/StreamInput"
            | "/process.Process/CloseStdin"
    ) && request
        .headers()
        .get("connect-timeout-ms")
        .and_then(|value| value.to_str().ok())
        .is_some_and(|value| {
            value.is_empty() || (value.len() <= 10 && value.parse::<i64>().is_ok())
        })
    {
        request.headers_mut().remove("connect-timeout-ms");
    }
    let start = request.uri().path() == "/process.Process/Start";
    if start || request.uri().path() == "/process.Process/Connect" {
        request
            .extensions_mut()
            .insert(RequestStarted(tokio::time::Instant::now()));
        if let Some(value) = request.headers_mut().remove("connect-timeout-ms") {
            request.extensions_mut().insert(ProcessTimeout(value));
        }
    }
}

/// A response subscription has its own deadline; it never owns process cleanup.
pub(crate) fn subscription_deadline(
    ctx: &RequestContext,
) -> Result<Option<tokio::time::Instant>, connectrpc::ConnectError> {
    let started = ctx
        .extensions()
        .get::<RequestStarted>()
        .map(|value| value.0)
        .unwrap_or_else(tokio::time::Instant::now);
    if ctx
        .headers()
        .get("content-type")
        .is_some_and(|value| value.as_bytes().starts_with(b"application/grpc"))
    {
        return ctx
            .headers()
            .get("grpc-timeout")
            .and_then(|value| value.to_str().ok())
            .map(super::grpc::timeout)
            .transpose()
            .map(|duration| duration.flatten().map(|duration| started + duration));
    }
    let Some(timeout) = ctx.extensions().get::<ProcessTimeout>() else {
        return Ok(None);
    };
    let raw = timeout.0.to_str().unwrap_or("");
    if raw.is_empty() {
        return Ok(None);
    }
    if raw.len() > 10 {
        return Err(connectrpc::ConnectError::invalid_argument(format!(
            "parse timeout: {raw:?} has >10 digits"
        )));
    }
    let millis = raw.parse::<i64>().map_err(|_| {
        connectrpc::ConnectError::invalid_argument(format!(
            "parse timeout: strconv.ParseInt: parsing {raw:?}: invalid syntax"
        ))
    })?;
    Ok(Some(started + Duration::from_millis(millis.max(0) as u64)))
}

pub(crate) fn with_subscription_deadline<T: Send + 'static>(
    stream: connectrpc::ServiceStream<T>,
    deadline: Option<tokio::time::Instant>,
) -> connectrpc::ServiceStream<T> {
    let Some(deadline) = deadline else {
        return stream;
    };
    use futures::StreamExt;
    Box::pin(futures::stream::unfold(
        Some((stream, true)),
        move |state| async move {
            let (mut stream, first) = state?;
            // Upstream sends the already-created process identity before waiting on
            // the request context, including an immediately expired Connect.
            if first {
                return stream
                    .next()
                    .await
                    .map(|event| (event, Some((stream, false))));
            }
            tokio::select! {
                biased;
                _ = tokio::time::sleep_until(deadline) => Some((Err(connectrpc::ConnectError::deadline_exceeded("context deadline exceeded")), None)),
                event = stream.next() => event.map(|event| (event, Some((stream, false)))),
            }
        },
    ))
}

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

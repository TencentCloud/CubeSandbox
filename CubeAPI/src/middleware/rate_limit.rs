// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::error::AppError;
use crate::state::AppState;
use axum::{
    extract::{Request, State},
    middleware::Next,
    response::Response,
};

/// Per-API-key token bucket rate limiter middleware.
/// Derives the bucket key from the request credential and checks the shared
/// governor limiter. Returns 429 if the key has exceeded its quota.
pub async fn rate_limit(
    State(state): State<AppState>,
    request: Request,
    next: Next,
) -> Result<Response, AppError> {
    let key = rate_limit_key(&request);

    match state.rate_limiter.check_key(&key) {
        Ok(_) => Ok(next.run(request).await),
        Err(_) => Err(AppError::TooManyRequests(
            "Rate limit exceeded. Slow down.".to_string(),
        )),
    }
}

/// Derive the rate-limit bucket key for this request.
///
/// Terminal WebSocket handshakes (`/sandboxes/.../terminal/ws`) are keyed by
/// client IP, never by the presented credential: at this layer the credential
/// is unverified, so keying on it would let an attacker mint unbounded
/// buckets (limiter map growth) and rotate keys to dodge the quota. The
/// per-IP coarse limit plus the terminal session caps bound the blast
/// radius. A post-auth per-identity check in the handler was considered and
/// deliberately skipped: `AppState::rate_limiter` is a single keyed limiter
/// shared with the HTTP routes, so terminal identity keys would share quota
/// semantics (and bucket namespace) with HTTP credential keys — a muddy
/// tradeoff for marginal benefit once IP limiting and session caps apply.
///
/// All other routes keep the existing credential keying, mirroring the
/// credential precedence of `unified_auth`: the `token` query param wins
/// over `X-API-Key`; with no credential at all the key is "anonymous".
/// `Sec-WebSocket-Protocol` is meaningful only on the terminal route and is
/// deliberately ignored elsewhere. The credential is used only as the
/// in-memory limiter key; it is never logged.
fn rate_limit_key(request: &Request) -> String {
    let headers = request.headers();

    if is_terminal_path(request.uri().path()) {
        return client_ip_key(headers);
    }

    // The raw (still percent-encoded) query value is good enough as a bucket
    // key — the same client encodes it the same way on every request.
    if let Some(token) = query_param(request.uri().query().unwrap_or(""), "token") {
        return token.to_string();
    }

    if let Some(key) = headers
        .get("X-API-Key")
        .and_then(|v| v.to_str().ok())
        .filter(|k| !k.is_empty())
    {
        return key.to_string();
    }

    "anonymous".to_string()
}

/// Whether the request path is the terminal WebSocket route
/// (`.../sandboxes/<id>/terminal/ws`).
fn is_terminal_path(path: &str) -> bool {
    path.contains("/sandboxes/") && path.ends_with("/terminal/ws")
}

/// Client-IP bucket key for terminal handshakes: the first
/// `X-Forwarded-For` value (the deployment proxy overwrites it — direct
/// clients could spoof it, but spoofing only moves them between equally
/// sized buckets), then `X-Real-IP`, then "unknown".
fn client_ip_key(headers: &axum::http::HeaderMap) -> String {
    headers
        .get("x-forwarded-for")
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.split(',').next())
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .or_else(|| {
            headers
                .get("x-real-ip")
                .and_then(|v| v.to_str().ok())
                .map(str::trim)
                .filter(|v| !v.is_empty())
        })
        .unwrap_or("unknown")
        .to_string()
}

/// Extract one raw query parameter value (`name=value`), without
/// percent-decoding. Empty values count as absent.
fn query_param<'a>(query: &'a str, name: &str) -> Option<&'a str> {
    query.split('&').find_map(|pair| {
        let (k, v) = pair.split_once('=')?;
        if k == name && !v.is_empty() {
            Some(v)
        } else {
            None
        }
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::body::Body;

    fn request(uri: &str, headers: &[(&str, &str)]) -> Request {
        let mut builder = Request::builder().method("GET").uri(uri);
        for (name, value) in headers {
            builder = builder.header(*name, *value);
        }
        builder.body(Body::empty()).expect("request should build")
    }

    #[test]
    fn terminal_path_is_keyed_by_client_ip() {
        // The terminal route ignores the (unverified) credential and buckets
        // by the first X-Forwarded-For value.
        let req = request(
            "/cubeapi/v1/sandboxes/sb-1/terminal/ws",
            &[
                (
                    "sec-websocket-protocol",
                    "cube-terminal, cube-terminal.tok-1",
                ),
                ("x-forwarded-for", "203.0.113.7, 10.0.0.1"),
            ],
        );
        assert_eq!(rate_limit_key(&req), "203.0.113.7");
    }

    #[test]
    fn terminal_same_ip_different_tokens_share_one_bucket() {
        // Forged or rotated tokens must not mint fresh buckets: same IP →
        // same key, whatever token the handshake presents.
        let key_of = |token: &str| {
            let req = request(
                "/cubeapi/v1/sandboxes/sb-1/terminal/ws",
                &[
                    ("sec-websocket-protocol", token),
                    ("x-forwarded-for", "203.0.113.7"),
                ],
            );
            rate_limit_key(&req)
        };
        assert_eq!(
            key_of("cube-terminal.forged-1"),
            key_of("cube-terminal.forged-2")
        );
        // …and the same holds for query-param tokens.
        let req = request(
            "/cubeapi/v1/sandboxes/sb-1/terminal/ws?token=forged-3",
            &[("x-forwarded-for", "203.0.113.7")],
        );
        assert_eq!(rate_limit_key(&req), "203.0.113.7");
    }

    #[test]
    fn terminal_key_falls_back_to_x_real_ip_then_unknown() {
        let req = request(
            "/cubeapi/v1/sandboxes/sb-1/terminal/ws",
            &[("x-real-ip", "198.51.100.9")],
        );
        assert_eq!(rate_limit_key(&req), "198.51.100.9");

        let req = request("/cubeapi/v1/sandboxes/sb-1/terminal/ws", &[]);
        assert_eq!(rate_limit_key(&req), "unknown");
    }

    #[test]
    fn non_terminal_path_ignores_terminal_subprotocol() {
        // A `/sandboxes/` route that is NOT the terminal WebSocket still
        // keys on its HTTP credential even if an unrelated client supplies
        // a terminal-style WebSocket subprotocol header.
        let req = request(
            "/cubeapi/v1/sandboxes/sb-1",
            &[
                ("sec-websocket-protocol", "cube-terminal.tok-1"),
                ("x-api-key", "api-key-1"),
            ],
        );
        assert_eq!(rate_limit_key(&req), "api-key-1");
    }

    #[test]
    fn key_falls_back_to_x_api_key() {
        let req = request("/sandboxes/sb-1", &[("x-api-key", "api-key-1")]);
        assert_eq!(rate_limit_key(&req), "api-key-1");
    }

    #[test]
    fn key_defaults_to_anonymous() {
        let req = request("/sandboxes/sb-1", &[]);
        assert_eq!(rate_limit_key(&req), "anonymous");
    }
}

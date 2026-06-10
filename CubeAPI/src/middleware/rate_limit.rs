// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::error::AppError;
use crate::state::AppState;
use axum::{
    extract::{ConnectInfo, Request, State},
    http::header,
    middleware::Next,
    response::Response,
};
use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use std::net::SocketAddr;

/// Per-API-key token bucket rate limiter middleware.
/// Reads the X-API-Key header and checks the shared governor limiter.
/// Returns 429 if the key has exceeded its quota.
pub async fn rate_limit(
    State(state): State<AppState>,
    request: Request,
    next: Next,
) -> Result<Response, AppError> {
    // Extract key; fall back to IP or "anonymous"
    let key = request
        .headers()
        .get("X-API-Key")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("anonymous")
        .to_string();

    match state.rate_limiter.check_key(&key) {
        Ok(_) => Ok(next.run(request).await),
        Err(_) => Err(AppError::TooManyRequests(
            "Rate limit exceeded. Slow down.".to_string(),
        )),
    }
}

/// Rate-limit middleware specialised for the OCI registry reverse-proxy.
///
/// Docker / oci-distribution clients do **not** send `X-API-Key`; they
/// authenticate with `Authorization: Basic <b64(user:pass)>` instead. The
/// generic `rate_limit` middleware would therefore collapse every docker
/// client onto the single "anonymous" bucket, which is unusable: a
/// runaway client could lock every other operator out of pushing layers.
///
/// We pick a key in this priority order:
///
///   1. `Authorization: Basic` username (i.e. the per-build `bld_<…>`
///      token we minted in `mint_registry_credential`). One bucket per
///      build is the natural granularity — a misbehaving build
///      doesn't impact others.
///   2. Peer socket address (`ConnectInfo`). Catches the unauthenticated
///      `GET /v2/` ping flood and any other anonymous traffic.
///   3. The literal string `\"reg:anonymous\"` as the absolute fallback,
///      should `ConnectInfo` somehow be missing.
///
/// All keys are prefixed with `reg:` so they live in a disjoint key space
/// from the sandbox API's `X-API-Key` buckets — a sandbox abuser cannot
/// starve the registry path and vice versa, even though both share the
/// same governor instance and quota.
pub async fn registry_rate_limit(
    State(state): State<AppState>,
    request: Request,
    next: Next,
) -> Result<Response, AppError> {
    let key = registry_key_for(&request);

    match state.rate_limiter.check_key(&key) {
        Ok(_) => Ok(next.run(request).await),
        Err(_) => Err(AppError::TooManyRequests(
            "Registry rate limit exceeded for this credential. Slow down.".to_string(),
        )),
    }
}

fn registry_key_for(request: &Request) -> String {
    if let Some(user) = basic_auth_username(request) {
        return format!("reg:user:{}", user);
    }
    if let Some(ConnectInfo(addr)) = request.extensions().get::<ConnectInfo<SocketAddr>>() {
        return format!("reg:ip:{}", addr.ip());
    }
    "reg:anonymous".to_string()
}

fn basic_auth_username(request: &Request) -> Option<String> {
    let raw = request.headers().get(header::AUTHORIZATION)?.to_str().ok()?;
    let b64 = raw
        .strip_prefix("Basic ")
        .or_else(|| raw.strip_prefix("basic "))?;
    let decoded = BASE64.decode(b64.trim()).ok()?;
    let s = std::str::from_utf8(&decoded).ok()?;
    let (user, _pass) = s.split_once(':')?;
    if user.is_empty() {
        return None;
    }
    Some(user.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::body::Body;
    use axum::http::{HeaderValue, Request as HttpRequest};

    fn req_with_auth(value: Option<&str>) -> Request {
        let mut builder = HttpRequest::builder().uri("/v2/foo/blobs/sha256:abc");
        if let Some(v) = value {
            builder = builder.header(header::AUTHORIZATION, HeaderValue::from_str(v).unwrap());
        }
        builder.body(Body::empty()).unwrap()
    }

    #[test]
    fn registry_key_uses_basic_username_when_present() {
        let r = req_with_auth(Some("Basic YmxkX3VzZXI6c2VjcmV0"));
        assert_eq!(registry_key_for(&r), "reg:user:bld_user");
    }

    #[test]
    fn registry_key_falls_back_to_anonymous_without_connect_info() {
        let r = req_with_auth(None);
        assert_eq!(registry_key_for(&r), "reg:anonymous");
    }

    #[test]
    fn registry_key_ignores_malformed_authorization() {
        let r = req_with_auth(Some("Bearer some-token"));
        assert_eq!(registry_key_for(&r), "reg:anonymous");

        let r = req_with_auth(Some("Basic !!!not-base64!!!"));
        assert_eq!(registry_key_for(&r), "reg:anonymous");

        let r = req_with_auth(Some("Basic bm9jb2xvbg=="));
        assert_eq!(registry_key_for(&r), "reg:anonymous");

        let r = req_with_auth(Some("Basic OnB3"));
        assert_eq!(registry_key_for(&r), "reg:anonymous");
    }
}

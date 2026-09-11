// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Shared authentication and REST error helpers for `/files`.

use std::collections::HashMap;

use axum::http::{HeaderMap, StatusCode};

use crate::platform::config::Config;
use crate::platform::identity::{self, User};
use crate::protocol::RestError;

/// Upload size cap for both upload paths (raw octet-stream and multipart).
///
/// Upstream envd's upload handler is unbounded (it streams to disk); in
/// production the cap is enforced by the proxy layer, which rejects bodies
/// over 256 MiB. Mirroring that external cap keeps the 64 MiB - 256 MiB
/// range functional (a proxy-passed 100 MiB upload must succeed here too)
/// while still bounding memory: unlike upstream, the body is buffered
/// before the atomic temp-file write, so the cap is also the memory
/// ceiling. (Streaming to disk would remove that trade-off — future work.)
/// Deliberately NOT `protocol::MAX_ENVELOPE_SIZE` (64 MiB): that constant
/// bounds Connect envelopes, not file uploads.
pub(crate) const MAX_UPLOAD_SIZE: usize = 256 * 1024 * 1024;

pub(crate) fn resolve_request_user(
    config: &Config,
    params: &HashMap<String, String>,
    headers: &HeaderMap,
) -> Result<User, RestError> {
    let name = params
        .get("username")
        .cloned()
        .or_else(|| {
            identity::user_from_basic_auth(
                headers
                    .get(axum::http::header::AUTHORIZATION)
                    .and_then(|v| v.to_str().ok()),
            )
        })
        .unwrap_or_else(|| config.default_user());
    identity::lookup_user(&name).map_err(|msg| RestError::new(StatusCode::UNAUTHORIZED, msg))
}

pub(crate) fn check_token_rest(config: &Config, headers: &HeaderMap) -> Result<(), RestError> {
    let token = headers.get("x-access-token").and_then(|v| v.to_str().ok());
    config
        .check_access_token(token)
        .map_err(|_| RestError::new(StatusCode::UNAUTHORIZED, "invalid access token".to_string()))
}

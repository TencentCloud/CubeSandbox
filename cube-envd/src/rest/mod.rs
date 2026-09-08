// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! REST/HTTP API 面：/health /init /envs /metrics /files 端点 + 下载协商原语
//! （镜像上游 `internal/api/`：handler 与协议函数同包）。协商原语是纯函数、
//! 无 I/O：encoding.rs（Accept-Encoding ↔ encoding.go）、ranges.rs（Range /
//! Content-Range ↔ net/http fs.go parseRange）、httpdate.rs（RFC 1123 ↔
//! TimeFormat）、content_disposition.rs（↔ mime.FormatMediaType）、
//! preconditions.rs（条件请求决策 ↔ fs.go checkPreconditions）。

pub mod content_disposition;
pub mod encoding;
pub mod files;
pub mod httpdate;
pub mod metrics;
pub mod preconditions;
pub mod ranges;

use std::collections::HashMap;
use std::sync::Arc;

use axum::extract::State;
use axum::http::{HeaderMap, StatusCode};
use axum::response::IntoResponse;
use serde::Deserialize;

use crate::state::{constant_time_eq, AppState};

/// `/init` token rejection messages, byte-for-byte the upstream errors
/// (`internal/api/init.go`: ErrAccessTokenMismatch /
/// ErrAccessTokenResetNotAuthorized), which upstream writes as the 401 body.
const ACCESS_TOKEN_MISMATCH: &str = "access token validation failed";
const ACCESS_TOKEN_RESET_NOT_AUTHORIZED: &str = "access token reset not authorized";

/// GET /health — baseline: 204 with `Cache-Control: no-store`.
pub async fn health() -> impl IntoResponse {
    (
        StatusCode::NO_CONTENT,
        [(axum::http::header::CACHE_CONTROL, "no-store")],
    )
}

/// /init request body. CubeSandbox's Cubelet only ever sends `envVars`;
/// the remaining upstream fields are accepted and logged so foreign callers
/// are not broken. Of those, `volumeMounts` / `hyperloopIP` / `caBundle`
/// carry no behavior (declared MVP differences), while `defaultUser`,
/// `defaultWorkdir`, `timestamp` and `accessToken` do — see `init`.
#[derive(Debug, Default, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct InitRequest {
    #[serde(default)]
    pub env_vars: Option<HashMap<String, String>>,
    #[serde(default, deserialize_with = "reject_empty_token")]
    pub access_token: Option<String>,
    #[serde(default)]
    pub default_user: Option<String>,
    #[serde(default)]
    pub default_workdir: Option<String>,
    #[serde(default)]
    pub volume_mounts: Option<serde_json::Value>,
    #[serde(default)]
    pub hyperloop_ip: Option<String>,
    #[serde(default)]
    pub timestamp: Option<String>,
    #[serde(default)]
    pub ca_bundle: Option<String>,
}

/// Upstream types this field as `*SecureToken`, whose `UnmarshalJSON` rejects
/// an empty string (`secure_token.go:66-75`), so the whole body fails to
/// decode and /init answers a bare 400. Accepting it here would store an empty
/// token instead — one that no real caller (it sends either a real token or
/// none at all) could ever match again, and that a later /init could not
/// replace either, because every body token would then mismatch it.
fn reject_empty_token<'de, D>(deserializer: D) -> Result<Option<String>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    match Option::<String>::deserialize(deserializer)? {
        Some(token) if token.is_empty() => {
            Err(serde::de::Error::custom("accessToken must not be empty"))
        }
        other => Ok(other),
    }
}

/// POST /init — the /init endpoint is on upstream's authorization whitelist
/// (see `auth.go`), so it is *not* gated by the `X-Access-Token` header:
/// the token lifecycle is decided by the body's `accessToken` field alone.
/// Order follows `SetData`: token validation, then env vars, then the
/// default user/workdir. `timestamp` gates the whole update — it is an
/// idempotency mark only, cube-envd never calls clock_settime (declared
/// difference).
pub async fn init(
    State(state): State<Arc<AppState>>,
    body: axum::body::Body,
) -> axum::response::Response {
    // Read with the same explicit cap the RPC unary surface uses instead of
    // relying on axum's default 2 MiB extractor limit: an /init carrying a
    // large caBundle (accepted-and-ignored fields) must not bounce with a
    // framework-shaped 413.
    let body = match axum::body::to_bytes(body, crate::server::MAX_UNARY_BODY).await {
        Ok(b) => b,
        Err(e) => {
            return (StatusCode::BAD_REQUEST, format!("read body: {e}")).into_response();
        }
    };
    let req: InitRequest = if body.is_empty() {
        InitRequest::default()
    } else {
        match serde_json::from_slice(&body) {
            Ok(r) => r,
            // Upstream decodes into a typed body and answers a decode failure
            // with a bare 400 (no body), so a malformed /init must not leak
            // parser wording into the response either.
            Err(_) => return StatusCode::BAD_REQUEST.into_response(),
        }
    };
    // Timestamp gate first: an outdated /init is dropped before it can
    // validate (or fail to validate) the token — the 0.5.13 baseline order.
    // e2b envd's main thaws cgroups on ANY authorized /init (its unfreeze
    // defer runs before the timestamp guard), but rejects unauthorized
    // requests before that defer is registered — hence token first there.
    // cube-envd has no freeze/thaw; if it ever grows one, follow e2b main.
    let timestamp_nanos = match req.timestamp.as_deref() {
        None => None,
        Some(raw) => match parse_rfc3339_nanos(raw) {
            Some(nanos) => Some(nanos),
            // Not RFC3339, or outside the i64-nanosecond range: both are a
            // caller bug, answered with the same bare 400 upstream gives a
            // body that fails to decode.
            None => return StatusCode::BAD_REQUEST.into_response(),
        },
    };
    if !state.claim_timestamp(timestamp_nanos) {
        tracing::info!("init: dropping request older than the last applied timestamp");
        return no_store(StatusCode::NO_CONTENT).into_response();
    }
    let action = {
        let stored = state.access_token();
        init_token_action(stored.as_deref(), req.access_token.as_deref())
    };
    match action {
        Err(reason) => {
            tracing::warn!("init: rejected: {reason}");
            return (StatusCode::UNAUTHORIZED, reason).into_response();
        }
        Ok(InitTokenAction::Set) => {
            if let Some(token) = req.access_token.as_deref() {
                tracing::info!("init: access token configured");
                state.set_access_token(token.to_string());
            }
        }
        Ok(InitTokenAction::Keep) => {}
    }
    if let Some(vars) = req.env_vars {
        tracing::info!("init: merging {} env vars", vars.len());
        state.merge_env_vars(vars);
    } else {
        state
            .initialized
            .store(true, std::sync::atomic::Ordering::Relaxed);
    }
    state.apply_init_defaults(req.default_user.as_deref(), req.default_workdir.as_deref());
    for (field, present) in [
        ("volumeMounts", req.volume_mounts.is_some()),
        ("hyperloopIP", req.hyperloop_ip.is_some()),
        ("caBundle", req.ca_bundle.is_some()),
    ] {
        if present {
            tracing::warn!("init: field '{field}' is accepted but ignored by cube-envd");
        }
    }
    no_store(StatusCode::NO_CONTENT).into_response()
}

/// Outcome of validating the token in an `/init` body (upstream
/// `validateInitAccessToken`).
#[derive(Debug, PartialEq)]
pub(crate) enum InitTokenAction {
    /// The body carries no token and none is configured: nothing to store.
    Keep,
    /// Store the body token — first-time setup, or a token equal to the
    /// configured one (upstream re-takes it, which is value-preserving).
    Set,
}

/// Validate an `/init` body token against the configured one. The MMDS reset
/// path upstream consults here is unreachable under `-isnotfc`
/// (`checkMMDSHash` returns `(false, false)`), which is the only mode
/// CubeSandbox runs, so a configured token is neither replaceable nor
/// clearable: a body without a token is refused instead of resetting it.
pub(crate) fn init_token_action(
    stored: Option<&str>,
    requested: Option<&str>,
) -> Result<InitTokenAction, &'static str> {
    match (stored, requested) {
        (None, None) => Ok(InitTokenAction::Keep),
        (None, Some(_)) => Ok(InitTokenAction::Set),
        (Some(_), None) => Err(ACCESS_TOKEN_RESET_NOT_AUTHORIZED),
        (Some(stored), Some(requested)) => {
            if constant_time_eq(stored.as_bytes(), requested.as_bytes()) {
                Ok(InitTokenAction::Set)
            } else {
                Err(ACCESS_TOKEN_MISMATCH)
            }
        }
    }
}

/// Parse an RFC3339 timestamp into Unix nanoseconds.
///
/// Returns None for anything that is not a well-formed RFC3339 timestamp, and
/// for one that is well-formed but outside the i64-nanosecond range (before
/// 1677-09-21 or after 2262-04-11). `/init`'s `timestamp` is a `time.Time`
/// upstream, and upstream answers a body it cannot decode with a bare 400 —
/// so the format boundary (month/day range, leap years, mandatory zone) is
/// the same one `time` enforces here. Out-of-range is a declared difference:
/// upstream's `UnixNano()` wraps and drops the request as stale (204); an
/// out-of-range timestamp is a caller bug, so we 400 instead.
pub(crate) fn parse_rfc3339_nanos(raw: &str) -> Option<i64> {
    let dt =
        time::OffsetDateTime::parse(raw, &time::format_description::well_known::Rfc3339).ok()?;
    let nanos = dt.unix_timestamp_nanos(); // i128, can exceed i64 outside 1677..2262
    if (i64::MIN as i128) <= nanos && nanos <= (i64::MAX as i128) {
        Some(nanos as i64)
    } else {
        None
    }
}

/// GET /envs — the accumulated env-var store (includes E2B_SANDBOX).
pub async fn envs(State(state): State<Arc<AppState>>, headers: HeaderMap) -> impl IntoResponse {
    if check_token(&state, &headers).is_err() {
        return StatusCode::UNAUTHORIZED.into_response();
    }
    (
        [(axum::http::header::CACHE_CONTROL, "no-store")],
        axum::Json(state.env_vars()),
    )
        .into_response()
}

/// Baseline: health/init/envs/metrics all answer with Cache-Control: no-store.
pub(crate) fn no_store(status: StatusCode) -> impl IntoResponse {
    (status, [(axum::http::header::CACHE_CONTROL, "no-store")])
}

pub(crate) fn check_token(state: &AppState, headers: &HeaderMap) -> Result<(), ()> {
    let token = headers.get("x-access-token").and_then(|v| v.to_str().ok());
    state.check_access_token(token)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_rfc3339_nanos_basics() {
        assert_eq!(parse_rfc3339_nanos("1970-01-01T00:00:00Z"), Some(0));
        assert_eq!(
            parse_rfc3339_nanos("2001-01-01T00:00:00Z"),
            Some(978_307_200_000_000_000)
        );
        assert_eq!(
            parse_rfc3339_nanos("2030-01-01T00:00:00Z"),
            Some(1_893_456_000_000_000_000)
        );
        // Fractional seconds: kept at nanosecond precision, truncated beyond.
        assert_eq!(
            parse_rfc3339_nanos("1970-01-01T00:00:00.5Z"),
            Some(500_000_000)
        );
        assert_eq!(
            parse_rfc3339_nanos("1970-01-01T00:00:00.123456789Z"),
            Some(123_456_789)
        );
        assert_eq!(
            parse_rfc3339_nanos("1970-01-01T00:00:00.1234567891Z"),
            Some(123_456_789)
        );
        // Offsets shift the instant; lowercase t / z are accepted like Go's
        // time.Parse.
        assert_eq!(parse_rfc3339_nanos("1970-01-01T01:00:00+01:00"), Some(0));
        assert_eq!(
            parse_rfc3339_nanos("1970-01-01T00:30:00-00:30"),
            Some(3600 * 1_000_000_000)
        );
        assert_eq!(parse_rfc3339_nanos("1970-01-01t00:00:00z"), Some(0));
    }

    #[test]
    fn parse_rfc3339_nanos_rejects_malformed() {
        // Mandatory zone, zero-padded colon offset, real calendar dates:
        for bad in [
            "not-a-timestamp",
            "1970-01-01",
            "1970-01-01T00:00:00",      // no zone: RFC3339 requires one
            "1970-01-01T00:00:00.5",    // ...including after a fraction
            "1970-01-01T00:00:00+0100", // offset without a colon
            "1970-01-01T00:00:00+1:00", // offset not zero-padded
            "1970-13-01T00:00:00Z",     // month out of range
            "1970-01-01T24:00:00Z",     // hour out of range
            "1970-01-01T00:60:00Z",     // minute out of range
            "1970-01-01T00:00:60Z",     // leap second: upstream rejects it too
            "1970-01-01T00:00:00.Z",    // fraction without digits
            "2023-02-31T00:00:00Z",     // February 31st
            "2023-04-31T00:00:00Z",     // April 31st
            "2023-02-29T00:00:00Z",     // not a leap year
            "1970-01-01T00:00:00ZZ",    // trailing junk
            "",
        ] {
            assert_eq!(parse_rfc3339_nanos(bad), None, "accepted {bad:?}");
        }
    }

    #[test]
    fn parse_rfc3339_nanos_accepts_leap_days() {
        // 2024 is a leap year, 2000 is a leap century, 1900 is not.
        assert!(parse_rfc3339_nanos("2024-02-29T00:00:00Z").is_some());
        assert!(parse_rfc3339_nanos("2000-02-29T00:00:00Z").is_some());
        assert_eq!(parse_rfc3339_nanos("1900-02-29T00:00:00Z"), None);
    }

    #[test]
    fn parse_rfc3339_nanos_rejects_out_of_range() {
        // i64 nanoseconds span 1677-09-21 .. 2262-04-11. Outside that range
        // upstream's UnixNano() wraps and drops the request as stale (204);
        // an out-of-range timestamp is a caller bug, so we 400 instead
        // (declared difference).
        for out_of_range in [
            "2263-01-01T00:00:00Z",
            "9999-01-01T00:00:00Z",
            "0001-01-01T00:00:00Z",
            "1677-09-21T00:00:00Z",
        ] {
            assert_eq!(parse_rfc3339_nanos(out_of_range), None, "{out_of_range:?}");
        }
        // The last representable instant still parses.
        assert!(parse_rfc3339_nanos("2262-04-11T00:00:00Z").is_some());
    }

    #[test]
    fn an_empty_body_token_fails_to_decode_like_upstream() {
        // Go types the field as *SecureToken, whose UnmarshalJSON rejects ""
        // (secure_token.go:66-75) — the whole body then fails to decode, which
        // /init turns into a bare 400. Deserializing into Option<String>
        // without this check would store an empty token that no caller could
        // match and no later /init could replace.
        assert!(serde_json::from_str::<InitRequest>(r#"{"accessToken":""}"#).is_err());
        // null and absence both mean "not carried", like Go's nil pointer.
        let null: InitRequest = serde_json::from_str(r#"{"accessToken":null}"#).unwrap();
        assert_eq!(null.access_token, None);
        let absent: InitRequest = serde_json::from_str("{}").unwrap();
        assert_eq!(absent.access_token, None);
        let set: InitRequest = serde_json::from_str(r#"{"accessToken":"tok"}"#).unwrap();
        assert_eq!(set.access_token.as_deref(), Some("tok"));
    }

    #[test]
    fn init_token_action_lifecycle() {
        // First-time setup: nothing configured, the body may set a token.
        assert_eq!(init_token_action(None, None), Ok(InitTokenAction::Keep));
        assert_eq!(init_token_action(None, Some("t")), Ok(InitTokenAction::Set));
        // Configured token: a matching body token re-takes it...
        assert_eq!(
            init_token_action(Some("t"), Some("t")),
            Ok(InitTokenAction::Set)
        );
        // ...a different one is rejected...
        assert_eq!(
            init_token_action(Some("t"), Some("other")),
            Err(ACCESS_TOKEN_MISMATCH)
        );
        assert_eq!(ACCESS_TOKEN_MISMATCH, "access token validation failed");
        // ...and a body that drops the token may not reset it.
        assert_eq!(
            init_token_action(Some("t"), None),
            Err(ACCESS_TOKEN_RESET_NOT_AUTHORIZED)
        );
        assert_eq!(
            ACCESS_TOKEN_RESET_NOT_AUTHORIZED,
            "access token reset not authorized"
        );
        // Prefix/length mismatches are rejected like any other mismatch.
        assert_eq!(
            init_token_action(Some("token"), Some("toke")),
            Err(ACCESS_TOKEN_MISMATCH)
        );
        assert_eq!(
            init_token_action(Some("token"), Some("tokenx")),
            Err(ACCESS_TOKEN_MISMATCH)
        );
    }
}

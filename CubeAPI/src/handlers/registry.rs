// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use axum::{
    body::Body,
    extract::{Path, Request, State},
    http::{header, HeaderMap, HeaderName, HeaderValue, Method, StatusCode},
    response::Response,
};
use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use futures::TryStreamExt;
use std::str::FromStr;

use crate::{
    error::{AppError, AppResult},
    models::ApiError,
    services::builds::BuildContext,
    state::AppState,
};

/// Headers that must NOT be propagated end-to-end.
const HOP_BY_HOP: &[&str] = &[
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
    "host",
];

/// Realm string echoed back in `WWW-Authenticate` challenges so docker /
/// oci-distribution clients know to retry with `Authorization: Basic`.
const REALM: &str = "cubeapi-registry";

/// `GET /v2/` — registry ping. Always returns `200 OK` with the version header
/// when an upstream is configured.
///
/// Note we deliberately do **not** require `Authorization` on the ping. The
/// docker / oci-distribution v2 protocol uses an unauthenticated GET /v2/ as
/// the discovery handshake — it's how the client learns the realm of the
/// auth challenge in the first place. Requiring auth here would break every
/// CLI client at the very first round-trip. The actual blob/manifest paths
/// in `proxy()` *do* require credentials, so this is not a bypass.
pub async fn ping(State(state): State<AppState>) -> AppResult<Response> {
    let upstream = state
        .config
        .registry_upstream
        .as_deref()
        .filter(|s| !s.is_empty())
        .ok_or_else(registry_disabled)?;

    forward(
        &state,
        Method::GET,
        upstream,
        "/v2/",
        "",
        &HeaderMap::new(),
        None,
    )
    .await
}

/// `ANY /v2/*path` — generic reverse-proxy.
///
/// Both the request body (Docker/OCI blob PATCH/PUT can be GiB-sized) and the
/// upstream response body (blob GET) are forwarded as streams; nothing is ever
/// fully buffered in CubeAPI's heap. This keeps memory pressure bounded
/// regardless of layer size or upload concurrency.
///
/// ## Defence in depth
///
/// Before any upstream forwarding happens, we enforce **two CubeAPI-layer
/// access controls** that do *not* rely on the upstream registry having its
/// own auth configured:
///
///   1. **Per-build credential validation** — the inbound `Authorization:
///      Basic` header must decode to a `(username, password)` pair that we
///      ourselves issued via `mint_registry_credential` and is still
///      attached to a *live* build. Missing / malformed / unknown / wrong
///      password → `401 Unauthorized` with a `WWW-Authenticate: Basic`
///      challenge so the docker client retries the standard way.
///   2. **Repo scoping** — once the credential resolves to a `BuildContext`,
///      we require the request's `<repo>` segment (everything between
///      `/v2/` and the next protocol verb) to match the repo embedded in
///      that build's `image_ref`. So even a holder of a valid build A
///      credential cannot push, pull or fingerprint blobs/manifests under
///      build B's repository — the request is rejected with `403 Forbidden`.
pub async fn proxy(
    State(state): State<AppState>,
    Path(path): Path<String>,
    request: Request,
) -> AppResult<Response> {
    let upstream = state
        .config
        .registry_upstream
        .as_deref()
        .filter(|s| !s.is_empty())
        .ok_or_else(registry_disabled)?
        .to_string();

    let method = request.method().clone();
    let query = request.uri().query().unwrap_or("").to_string();
    let headers = request.headers().clone();
    let normalized = normalize_subpath(&path);

    let ctx = match resolve_build_credential(&state, &headers) {
        CredentialOutcome::Authenticated(ctx) => ctx,
        CredentialOutcome::Missing => {
            tracing::debug!(path = %normalized, "registry request without Authorization");
            return Ok(challenge_response(
                StatusCode::UNAUTHORIZED,
                "authentication required",
            ));
        }
        CredentialOutcome::Malformed => {
            tracing::debug!(path = %normalized, "registry request with malformed Authorization");
            return Ok(challenge_response(
                StatusCode::UNAUTHORIZED,
                "malformed Authorization header",
            ));
        }
        CredentialOutcome::Rejected => {
            tracing::warn!(
                path = %normalized,
                "registry request with unknown or invalid build credential"
            );
            return Ok(challenge_response(
                StatusCode::UNAUTHORIZED,
                "invalid build credential",
            ));
        }
    };

    if let Some(repo) = parse_repo(&normalized) {
        if !repo_allowed(&ctx, repo) {
            tracing::warn!(
                build_id = %ctx.build_id,
                requested_repo = %repo,
                expected_image_ref = %ctx.image_ref,
                "registry credential used against unauthorised repository"
            );
            return Ok(forbidden_response(
                "credential is scoped to a different repository",
            ));
        }
    }
    else if normalized != "/v2/" {
        tracing::warn!(
            build_id = %ctx.build_id,
            path = %normalized,
            "registry credential used against non-repository endpoint"
        );
        return Ok(forbidden_response(
            "credential is not authorised for this endpoint",
        ));
    }

    let body_stream = request
        .into_body()
        .into_data_stream()
        .map_err(|e| std::io::Error::new(std::io::ErrorKind::Other, e));
    let upstream_body = reqwest::Body::wrap_stream(body_stream);

    let response = forward(
        &state,
        method.clone(),
        &upstream,
        &normalized,
        &query,
        &headers,
        Some(upstream_body),
    )
    .await?;

    // After a successful manifest PUT we mark the build as image-pushed so
    // that the orchestrator stage proceeds. We only need the status — the
    // manifest body itself is being streamed back to the client untouched.
    if method == Method::PUT && response.status().is_success() {
        if let Some(parsed) = parse_manifest_path(&normalized) {
            // tag carries either the buildID (preferred) or a digest. Pull the
            // build context by tag first, then fall back to no-op.
            if !parsed.tag.starts_with("sha256:") {
                tracing::info!(
                    build_id = %ctx.build_id,
                    repo = %parsed.repo,
                    tag = %parsed.tag,
                    "manifest pushed; marking build as image-pushed"
                );
                state
                    .services
                    .templates
                    .mark_image_pushed(&parsed.tag, &parsed.repo);
            }
        }
    }

    Ok(response)
}

async fn forward(
    state: &AppState,
    method: Method,
    upstream: &str,
    path: &str,
    query: &str,
    in_headers: &HeaderMap,
    body: Option<reqwest::Body>,
) -> AppResult<Response> {
    let upstream = upstream.trim_end_matches('/');
    let path = if path.starts_with('/') {
        path.to_string()
    } else {
        format!("/{}", path)
    };
    let url = if query.is_empty() {
        format!("{}{}", upstream, path)
    } else {
        format!("{}{}?{}", upstream, path, query)
    };

    let mut req = state.http_client.request(method, &url);

    for (name, value) in in_headers {
        let key = name.as_str().to_ascii_lowercase();
        if HOP_BY_HOP.contains(&key.as_str()) {
            continue;
        }
        req = req.header(name.clone(), value.clone());
    }

    if let Some(body) = body {
        req = req.body(body);
    }

    let upstream_resp = req.send().await.map_err(|e| {
        tracing::error!(error = %e, url = %url, "registry upstream request failed");
        AppError::Internal(anyhow::anyhow!("registry upstream unreachable: {}", e))
    })?;

    let status = upstream_resp.status();
    let mut headers = HeaderMap::new();
    for (name, value) in upstream_resp.headers() {
        let key = name.as_str().to_ascii_lowercase();
        if HOP_BY_HOP.contains(&key.as_str()) {
            continue;
        }
        if let (Ok(name), Ok(value)) = (
            HeaderName::from_str(name.as_str()),
            HeaderValue::from_bytes(value.as_bytes()),
        ) {
            headers.insert(name, value);
        }
    }

    let resp_stream = upstream_resp
        .bytes_stream()
        .map_err(|e| std::io::Error::new(std::io::ErrorKind::Other, e));
    let resp_body = Body::from_stream(resp_stream);

    let mut response = Response::builder()
        .status(StatusCode::from_u16(status.as_u16()).unwrap_or(StatusCode::BAD_GATEWAY))
        .body(resp_body)
        .map_err(|e| AppError::Internal(anyhow::anyhow!("response build failed: {}", e)))?;

    *response.headers_mut() = headers;
    response
        .headers_mut()
        .entry(header::HeaderName::from_static("docker-distribution-api-version"))
        .or_insert(HeaderValue::from_static("registry/2.0"));

    Ok(response)
}

fn registry_disabled() -> AppError {
    AppError::NotImplemented(
        "registry upstream is not configured: set CUBE_API_REGISTRY_UPSTREAM \
         to enable the bundled OCI registry"
            .to_string(),
    )
}

fn normalize_subpath(path: &str) -> String {
    if path.starts_with("/v2") {
        path.to_string()
    } else if path.starts_with("v2/") {
        format!("/{}", path)
    } else {
        format!("/v2/{}", path.trim_start_matches('/'))
    }
}

#[derive(Debug)]
struct ManifestPath {
    repo: String,
    tag: String,
}

/// Parse `/v2/<repo>/manifests/<tag>` (where `<repo>` may itself contain
/// slashes). Returns `None` for blob / upload / catalog endpoints.
fn parse_manifest_path(path: &str) -> Option<ManifestPath> {
    let stripped = path.strip_prefix("/v2/")?;
    let idx = stripped.rfind("/manifests/")?;
    let repo = &stripped[..idx];
    let tag = &stripped[idx + "/manifests/".len()..];
    if repo.is_empty() || tag.is_empty() {
        return None;
    }
    Some(ManifestPath {
        repo: repo.to_string(),
        tag: tag.to_string(),
    })
}

impl ManifestPath {
    #[allow(dead_code)]
    fn rebuild(&self) -> String {
        format!("/v2/{}/manifests/{}", self.repo, self.tag)
    }
}

enum CredentialOutcome {
    /// Header present, base64-decoded `user:pass` matches a live build
    /// whose stored password equals the presented one.
    Authenticated(BuildContext),
    /// No `Authorization` header at all. Triggers the standard
    /// `WWW-Authenticate: Basic` challenge.
    Missing,
    /// Header present but not a valid `Basic <b64(user:pass)>` envelope
    /// (wrong scheme, bad base64, no colon, …).
    Malformed,
    /// Header is well-formed but the username is unknown, the build has
    /// already been evicted, or the password does not match.
    ///
    /// Note: we deliberately do not distinguish "unknown user" from "bad
    /// password" in the response, to avoid an enumeration oracle. The
    /// internal log lines do record the difference for ops debugging.
    Rejected,
}

fn resolve_build_credential(state: &AppState, headers: &HeaderMap) -> CredentialOutcome {
    let Some(raw) = headers.get(header::AUTHORIZATION) else {
        return CredentialOutcome::Missing;
    };
    let Ok(value) = raw.to_str() else {
        return CredentialOutcome::Malformed;
    };
    let Some(b64) = value
        .strip_prefix("Basic ")
        .or_else(|| value.strip_prefix("basic "))
    else {
        return CredentialOutcome::Malformed;
    };
    let Ok(decoded) = BASE64.decode(b64.trim()) else {
        return CredentialOutcome::Malformed;
    };
    let Ok(decoded_str) = std::str::from_utf8(&decoded) else {
        return CredentialOutcome::Malformed;
    };
    let Some((user, pass)) = decoded_str.split_once(':') else {
        return CredentialOutcome::Malformed;
    };

    let Some(ctx) = state.services.builds.find_by_registry_username(user) else {
        return CredentialOutcome::Rejected;
    };

    if !constant_time_eq_strings(pass, &ctx.credential.password) {
        return CredentialOutcome::Rejected;
    }
    CredentialOutcome::Authenticated(ctx)
}

fn constant_time_eq_strings(a: &str, b: &str) -> bool {
    if a.is_empty() || b.is_empty() {
        return false;
    }
    if a.len() != b.len() {
        // Still walk the longer slice to keep the timing roughly stable.
        let longer = if a.len() > b.len() { a } else { b };
        let mut diff = 0u8;
        for byte in longer.as_bytes() {
            diff |= byte ^ 0;
        }
        let _ = diff;
        return false;
    }
    let mut diff = 0u8;
    for (x, y) in a.as_bytes().iter().zip(b.as_bytes()) {
        diff |= x ^ y;
    }
    diff == 0
}

/// Extract the `<repo>` segment from any well-formed v2 distribution path
/// (`/v2/<repo>/{blobs,manifests,tags,referrers}/...`). Returns `None` for
/// the bare ping (`/v2/`), for catalog endpoints, and for paths that don't
/// match the v2 layout at all.
fn parse_repo(path: &str) -> Option<&str> {
    let stripped = path.strip_prefix("/v2/")?;
    if stripped.is_empty() {
        return None;
    }
    if stripped.starts_with('_') {
        return None;
    }
    for verb in ["/manifests/", "/blobs/", "/tags/", "/referrers/"] {
        if let Some(idx) = stripped.rfind(verb) {
            if idx == 0 {
                return None;
            }
            return Some(&stripped[..idx]);
        }
    }
    None
}

fn repo_allowed(ctx: &BuildContext, repo: &str) -> bool {
    let Some(expected) = image_ref_repo(&ctx.image_ref) else {
        return false;
    };
    expected == repo
}

fn image_ref_repo(image_ref: &str) -> Option<String> {
    let without_tag = image_ref.rsplit_once(':').map(|(l, _)| l).unwrap_or(image_ref);
    // Drop everything up to and including the first `/`, which is the host.
    let (_, repo) = without_tag.split_once('/')?;
    if repo.is_empty() {
        return None;
    }
    Some(repo.to_string())
}

fn challenge_response(status: StatusCode, message: &str) -> Response {
    let body = serde_json::to_vec(&ApiError::new(status.as_u16() as i32, message.to_string()))
        .unwrap_or_default();
    let mut resp = Response::builder()
        .status(status)
        .body(Body::from(body))
        .expect("static challenge response is always well-formed");
    resp.headers_mut().insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("application/json"),
    );
    resp.headers_mut().insert(
        header::WWW_AUTHENTICATE,
        HeaderValue::from_str(&format!("Basic realm=\"{}\"", REALM))
            .expect("REALM is ASCII"),
    );
    resp.headers_mut().insert(
        HeaderName::from_static("docker-distribution-api-version"),
        HeaderValue::from_static("registry/2.0"),
    );
    resp
}

fn forbidden_response(message: &str) -> Response {
    let body = serde_json::to_vec(&ApiError::new(403, message.to_string())).unwrap_or_default();
    let mut resp = Response::builder()
        .status(StatusCode::FORBIDDEN)
        .body(Body::from(body))
        .expect("static forbidden response is always well-formed");
    resp.headers_mut().insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("application/json"),
    );
    resp.headers_mut().insert(
        HeaderName::from_static("docker-distribution-api-version"),
        HeaderValue::from_static("registry/2.0"),
    );
    resp
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::models::CreateTemplateRequest;
    use crate::services::builds::{BuildRegistry, EvictionPolicy};

    #[test]
    fn parse_manifest_path_accepts_namespaced_repo() {
        let p = parse_manifest_path("/v2/e2b/tpl-abc/manifests/bld-001").unwrap();
        assert_eq!(p.repo, "e2b/tpl-abc");
        assert_eq!(p.tag, "bld-001");
    }

    #[test]
    fn parse_manifest_path_rejects_blob_paths() {
        assert!(parse_manifest_path("/v2/e2b/tpl-abc/blobs/sha256:abc").is_none());
        assert!(parse_manifest_path("/v2/").is_none());
    }

    #[test]
    fn normalize_subpath_handles_axum_capture_variants() {
        assert_eq!(normalize_subpath("v2/foo/bar"), "/v2/foo/bar");
        assert_eq!(normalize_subpath("/foo/bar"), "/v2/foo/bar");
        assert_eq!(normalize_subpath("/v2/foo/bar"), "/v2/foo/bar");
    }

    // ── repo / image_ref helpers ─────────────────────────────────────

    #[test]
    fn parse_repo_extracts_namespaced_repo_from_each_verb() {
        for path in [
            "/v2/e2b/tpl-abc/manifests/bld-001",
            "/v2/e2b/tpl-abc/blobs/sha256:abc",
            "/v2/e2b/tpl-abc/blobs/uploads/uuid-123",
            "/v2/e2b/tpl-abc/tags/list",
            "/v2/e2b/tpl-abc/referrers/sha256:abc",
        ] {
            assert_eq!(
                parse_repo(path),
                Some("e2b/tpl-abc"),
                "parse_repo failed for {}", path
            );
        }
    }

    #[test]
    fn parse_repo_rejects_non_repo_endpoints() {
        assert_eq!(parse_repo("/v2/"), None);
        assert_eq!(parse_repo("/v2/_catalog"), None);
        assert_eq!(parse_repo("/v2/manifests/foo"), None);
        assert_eq!(parse_repo("foo/bar"), None);
    }

    #[test]
    fn image_ref_repo_strips_host_and_tag() {
        assert_eq!(
            image_ref_repo("127.0.0.1:5000/e2b/tpl-abc:bld-deadbeef").as_deref(),
            Some("e2b/tpl-abc")
        );
        assert_eq!(
            image_ref_repo("registry.example.com/e2b/tpl-abc").as_deref(),
            Some("e2b/tpl-abc")
        );
    }

    #[test]
    fn repo_allowed_rejects_prefix_collisions() {
        let mut ctx = sample_context();
        ctx.image_ref = "127.0.0.1:5000/e2b/tpl-abc:bld-001".to_string();
        assert!(repo_allowed(&ctx, "e2b/tpl-abc"));
        assert!(!repo_allowed(&ctx, "e2b/tpl-abc-evil"));
        assert!(!repo_allowed(&ctx, "evil/tpl-abc"));
    }

    #[test]
    fn constant_time_eq_strings_basic_correctness() {
        assert!(constant_time_eq_strings("abc", "abc"));
        assert!(!constant_time_eq_strings("abc", "abd"));
        assert!(!constant_time_eq_strings("abc", "abcd"));
        assert!(!constant_time_eq_strings("", ""));
        assert!(!constant_time_eq_strings("", "abc"));
        assert!(!constant_time_eq_strings("abc", ""));
    }

    // ── credential resolution against an in-memory BuildRegistry ─────

    fn sample_request() -> CreateTemplateRequest {
        CreateTemplateRequest {
            template_id: String::new(),
            instance_type: None,
            alias: None,
            team_id: None,
            image: None,
            dockerfile: None,
            writable_layer_size: None,
            exposed_ports: None,
            probe_port: None,
            probe_path: None,
            cpu: None,
            memory: None,
            cpu_count: None,
            memory_mb: None,
            env: None,
            env_vars: None,
            allow_internet_access: None,
            network_type: None,
            nodes: None,
            registry_username: None,
            registry_password: None,
            command: None,
            args: None,
            dns: None,
            allow_out: None,
            deny_out: None,
            start_cmd: None,
            ready_cmd: None,
        }
    }

    fn sample_context() -> BuildContext {
        let reg = BuildRegistry::with_policy(EvictionPolicy::unbounded());
        let cred = crate::models::RegistryCredential {
            url: "http://127.0.0.1:5000".to_string(),
            repository: "e2b/tpl-abc".to_string(),
            username: "bld_test_user".to_string(),
            password: "bld_test_pass_secret".to_string(),
        };
        reg.create(
            "tpl-abc".to_string(),
            sample_request(),
            cred,
            "127.0.0.1:5000/e2b/tpl-abc:bld".to_string(),
        )
    }

    #[test]
    fn build_registry_indexes_credential_username() {
        let reg = BuildRegistry::with_policy(EvictionPolicy::unbounded());
        let cred = crate::models::RegistryCredential {
            url: "http://127.0.0.1:5000".to_string(),
            repository: "e2b/tpl-x".to_string(),
            username: "bld_unique_user".to_string(),
            password: "secret".to_string(),
        };
        let ctx = reg.create(
            "tpl-x".to_string(),
            sample_request(),
            cred,
            "127.0.0.1:5000/e2b/tpl-x:bld".to_string(),
        );
        let resolved = reg.find_by_registry_username("bld_unique_user").unwrap();
        assert_eq!(resolved.build_id, ctx.build_id);
        assert!(reg.find_by_registry_username("bld_other_user").is_none());
    }

    #[test]
    fn parse_manifest_tag_uses_build_id_after_credential_check() {
        let m = parse_manifest_path("/v2/e2b/tpl-abc/manifests/bld-deadbeef").unwrap();
        assert_eq!(m.tag, "bld-deadbeef");
    }
}

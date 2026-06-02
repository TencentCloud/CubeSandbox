// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//


use axum::{
    body::{Body, Bytes},
    extract::{Path, Request, State},
    http::{header, HeaderMap, HeaderName, HeaderValue, Method, StatusCode},
    response::Response,
};
use std::str::FromStr;

use crate::{
    error::{AppError, AppResult},
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

/// `GET /v2/` — registry ping. Always returns `200 OK` with the version header
/// when an upstream is configured.
pub async fn ping(State(state): State<AppState>) -> AppResult<Response> {
    let upstream = state
        .config
        .registry_upstream
        .as_deref()
        .filter(|s| !s.is_empty())
        .ok_or_else(registry_disabled)?;

    forward(&state, Method::GET, upstream, "/v2/", "", &HeaderMap::new(), Bytes::new()).await
}

/// `ANY /v2/*path` — generic reverse-proxy.
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
    let body = match axum::body::to_bytes(request.into_body(), 512 * 1024 * 1024).await {
        Ok(b) => b,
        Err(e) => {
            return Err(AppError::BadRequest(format!(
                "failed to read /v2/* request body: {}",
                e
            )))
        }
    };

    let normalized = normalize_subpath(&path);
    let response = forward(&state, method.clone(), &upstream, &normalized, &query, &headers, body)
        .await?;

    // After a successful manifest PUT we mark the build as image-pushed so
    // that the orchestrator stage proceeds.
    if method == Method::PUT && response.status().is_success() {
        if let Some(parsed) = parse_manifest_path(&normalized) {
            // tag carries either the buildID (preferred) or a digest. Pull the
            // build context by tag first, then fall back to no-op.
            if !parsed.tag.starts_with("sha256:") {
                tracing::info!(
                    repo = %parsed.repo,
                    tag = %parsed.tag,
                    "manifest pushed; marking build as image-pushed"
                );
                state.services.templates.mark_image_pushed(&parsed.tag);
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
    body: Bytes,
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

    if !body.is_empty() {
        req = req.body(body.to_vec());
    }

    let upstream_resp = req.send().await.map_err(|e| {
        tracing::error!(error = %e, url = %url, "registry upstream request failed");
        AppError::Internal(anyhow::anyhow!("registry upstream unreachable: {}", e))
    })?;

    let status = upstream_resp.status();
    let mut headers = HeaderMap::new();
    for (name, value) in upstream_resp.headers() {
        let key = name.as_str().to_ascii_lowercase();
        if HOP_BY_HOP.contains(&key.as_str()) || key == "content-length" {
            continue;
        }
        if let (Ok(name), Ok(value)) = (
            HeaderName::from_str(name.as_str()),
            HeaderValue::from_bytes(value.as_bytes()),
        ) {
            headers.insert(name, value);
        }
    }

    let body_bytes = upstream_resp
        .bytes()
        .await
        .map_err(|e| AppError::Internal(anyhow::anyhow!("registry response read failed: {}", e)))?;

    let mut response = Response::builder()
        .status(StatusCode::from_u16(status.as_u16()).unwrap_or(StatusCode::BAD_GATEWAY))
        .body(Body::from(body_bytes))
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

#[cfg(test)]
mod tests {
    use super::*;

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
}

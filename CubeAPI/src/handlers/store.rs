// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! GET /cubeapi/v1/store/meta — returns live digest + size for each store
//! template image, sourced from the local Docker daemon (docker inspect).
//!
//! If an image is not present locally the entry is omitted from the response
//! (caller should treat missing entry as "unknown").  No network pull is
//! performed here; freshness is maintained by a background task.

use axum::{http::StatusCode, response::IntoResponse, Json};
use serde::{Deserialize, Serialize};
use std::process::Command;
use utoipa::ToSchema;

// ── canonical store image list ────────────────────────────────────────────

/// Images whose metadata we expose.  Keep in sync with
/// `web/src/data/templateStore.ts`.
const STORE_IMAGES: &[&str] = &[
    "cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest",
    "cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest",
    "ghcr.io/tencentcloud/cubesandbox-base:latest",
];

// ── docker inspect structs ────────────────────────────────────────────────

#[derive(Deserialize)]
struct InspectResult {
    #[serde(rename = "Size")]
    size: u64,
    #[serde(rename = "RepoDigests")]
    repo_digests: Vec<String>,
}

// ── response types ────────────────────────────────────────────────────────

/// Per-image metadata entry.
#[derive(Debug, Serialize, ToSchema)]
pub struct ImageMeta {
    /// Full image reference that was queried (e.g. `registry/repo:tag`).
    pub image: String,
    /// Uncompressed on-disk size in bytes.
    pub size_bytes: u64,
    /// Uncompressed size in MiB, rounded to one decimal place.
    pub size_mb: f64,
    /// Full repo digest, e.g. `registry/repo@sha256:abc…`.
    /// `None` when the image exists locally but has no remote digest yet.
    pub digest: Option<String>,
    /// The bare `sha256:…` portion extracted from `digest` for easy comparison
    /// with the value stored in `templateStore.ts`.
    pub digest_short: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct StoreMeta {
    pub images: Vec<ImageMeta>,
}

// ── helper ────────────────────────────────────────────────────────────────

fn inspect_image(image: &str) -> Option<ImageMeta> {
    let output = Command::new("docker")
        .args(["image", "inspect", "--format", "{{json .}}", image])
        .output()
        .ok()?;

    if !output.status.success() {
        return None; // image not present locally
    }

    // docker inspect returns a JSON array; unwrap the single element
    let raw = String::from_utf8_lossy(&output.stdout);
    let raw = raw.trim();
    let json_str = if raw.starts_with('[') {
        // strip outer brackets
        let inner = raw.trim_start_matches('[').trim_end_matches(']').trim();
        inner.to_string()
    } else {
        raw.to_string()
    };

    let info: InspectResult = serde_json::from_str(&json_str).ok()?;

    let size_mb = (info.size as f64) / (1024.0 * 1024.0);
    let size_mb = (size_mb * 10.0).round() / 10.0;

    // Pick the digest that matches the queried registry (first match wins)
    let digest = info
        .repo_digests
        .iter()
        .find(|d| {
            let registry = image.split('/').next().unwrap_or("");
            d.starts_with(registry)
        })
        .or_else(|| info.repo_digests.first())
        .cloned();

    let digest_short = digest
        .as_deref()
        .and_then(|d| d.split('@').nth(1).map(|s| s.to_string()));

    Some(ImageMeta {
        image: image.to_string(),
        size_bytes: info.size,
        size_mb,
        digest,
        digest_short,
    })
}

// ── handler ───────────────────────────────────────────────────────────────

/// GET /cubeapi/v1/store/meta
///
/// Returns live image metadata (digest + size) for all store templates.
/// Images not present locally are omitted.  Response is ~instant because
/// it only reads the local Docker daemon cache.
pub async fn get_store_meta() -> impl IntoResponse {
    // Run inspections in a blocking thread-pool to avoid stalling the async
    // runtime (docker inspect is a short-lived subprocess but still blocking).
    let meta = tokio::task::spawn_blocking(|| {
        STORE_IMAGES
            .iter()
            .filter_map(|img| inspect_image(img))
            .collect::<Vec<_>>()
    })
    .await
    .unwrap_or_default();

    (StatusCode::OK, Json(StoreMeta { images: meta }))
}

// ── POST /store/refresh ───────────────────────────────────────────────────────

/// POST /cubeapi/v1/store/refresh
///
/// Pulls all store images from the registry (may take a while for large images)
/// then returns the updated metadata.  Call this when the user explicitly asks
/// to check for updates.
pub async fn refresh_store_meta() -> impl IntoResponse {
    // Pull all images in parallel, then inspect each.
    let handles: Vec<_> = STORE_IMAGES
        .iter()
        .map(|img| {
            let img = *img;
            tokio::task::spawn_blocking(move || {
                // pull (ignore errors — image might not be accessible)
                let _ = Command::new("docker")
                    .args(["pull", "--quiet", img])
                    .output();
                // inspect regardless (use cached if pull failed)
                inspect_image(img)
            })
        })
        .collect();

    let mut results = Vec::new();
    for handle in handles {
        if let Ok(Some(m)) = handle.await {
            results.push(m);
        }
    }

    (StatusCode::OK, Json(StoreMeta { images: results }))
}

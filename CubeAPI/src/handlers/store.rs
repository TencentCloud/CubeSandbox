// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Store template catalog + live image metadata endpoints.
//!
//! - `GET  /store/catalog`  — list all store template catalog entries (from DB)
//! - `POST /store/catalog`  — create / upsert a catalog entry
//! - `PATCH /store/catalog/:itemID` — update a catalog entry
//! - `DELETE /store/catalog/:itemID` — soft-delete a catalog entry
//! - `GET  /store/meta`     — live digest + size from local Docker daemon
//! - `POST /store/refresh`  — pull images then return updated metadata

use axum::extract::{Path, State};
use axum::{http::StatusCode, response::IntoResponse, Json};
use serde::{Deserialize, Serialize};
use std::process::Command;
use std::sync::Arc;
use tokio::sync::Semaphore;
use utoipa::ToSchema;

use crate::db::StoreTemplateRecord;
use crate::state::AppState;

// ── catalog response types ─────────────────────────────────────────────────

/// A single store template catalog entry.
#[derive(Debug, Serialize, Deserialize, ToSchema)]
pub struct StoreCatalogItem {
    /// Unique catalog item identifier, e.g. `sandbox-code`.
    pub id: String,
    /// i18n key for the display name.
    pub name_key: String,
    /// i18n key for the description.
    pub description_key: String,
    /// Container image for China region (also used as the default image).
    pub image_cn: String,
    /// Container image for international region.
    pub image_intl: String,
    /// Default image reference (same as `image_cn`).  Kept for backward
    /// compatibility with the old `templateStore.ts` frontend data shape.
    pub image: String,
    /// Expected sha256 digest for update detection.
    pub digest: Option<String>,
    /// Tags for filtering.
    pub tags: Vec<String>,
    /// Category: `code`, `browser`, `ai`, or `base`.
    pub category: String,
    /// Approximate uncompressed image size in MB.
    pub size_mb: i32,
    /// Ports to expose on the sandbox.
    pub expose_ports: Vec<i32>,
    /// Health-check probe port.
    pub probe_port: i32,
    /// Health-check probe path.
    pub probe_path: String,
    /// Writable layer size, e.g. `1G`.
    pub writable_layer_size: String,
    /// Whether this is an officially maintained template.
    pub official: bool,
    /// DNS servers to configure in the sandbox (ordered by priority).
    #[serde(default)]
    pub dns: Vec<String>,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct StoreCatalogResponse {
    pub items: Vec<StoreCatalogItem>,
}

/// Body for `POST /store/catalog` and `PATCH /store/catalog/:itemID`.
#[derive(Debug, Deserialize, ToSchema)]
pub struct UpsertStoreCatalogRequest {
    /// Unique catalog item identifier.
    pub id: String,
    /// i18n key for the display name.
    pub name_key: String,
    /// i18n key for the description.
    pub description_key: String,
    /// Container image for China region.
    pub image_cn: String,
    /// Container image for international region.
    pub image_intl: String,
    /// Expected sha256 digest.
    pub digest: Option<String>,
    /// Tags for filtering.
    #[serde(default)]
    pub tags: Vec<String>,
    /// Category: `code`, `browser`, `ai`, or `base`.
    pub category: String,
    /// Approximate uncompressed image size in MB.
    #[serde(default)]
    pub size_mb: i32,
    /// Ports to expose.
    #[serde(default)]
    pub expose_ports: Vec<i32>,
    /// Health-check probe port.
    #[serde(default)]
    pub probe_port: i32,
    /// Health-check probe path.
    #[serde(default = "default_probe_path")]
    pub probe_path: String,
    /// Writable layer size.
    #[serde(default = "default_writable_layer_size")]
    pub writable_layer_size: String,
    /// Official flag.
    #[serde(default)]
    pub official: bool,
    /// DNS servers to configure in the sandbox (ordered by priority).
    #[serde(default)]
    pub dns: Vec<String>,
    /// Display order (lower first).
    #[serde(default)]
    pub sort_order: i32,
}

fn default_probe_path() -> String {
    "/".into()
}
fn default_writable_layer_size() -> String {
    "1G".into()
}

impl From<StoreTemplateRecord> for StoreCatalogItem {
    fn from(r: StoreTemplateRecord) -> Self {
        Self {
            id: r.item_id,
            name_key: r.name_key,
            description_key: r.description_key,
            image_cn: r.image_cn.clone(),
            image_intl: r.image_intl,
            image: r.image_cn, // default to CN image for backward compat
            digest: r.digest,
            tags: r.tags,
            category: r.category,
            size_mb: r.size_mb,
            expose_ports: r.expose_ports,
            probe_port: r.probe_port,
            probe_path: r.probe_path,
            writable_layer_size: r.writable_layer_size,
            official: r.official,
            dns: r.dns,
        }
    }
}

impl From<&UpsertStoreCatalogRequest> for StoreTemplateRecord {
    fn from(r: &UpsertStoreCatalogRequest) -> Self {
        Self {
            item_id: r.id.clone(),
            name_key: r.name_key.clone(),
            description_key: r.description_key.clone(),
            image_cn: r.image_cn.clone(),
            image_intl: r.image_intl.clone(),
            digest: r.digest.clone(),
            tags: r.tags.clone(),
            category: r.category.clone(),
            size_mb: r.size_mb,
            expose_ports: r.expose_ports.clone(),
            probe_port: r.probe_port,
            probe_path: r.probe_path.clone(),
            writable_layer_size: r.writable_layer_size.clone(),
            official: r.official,
            dns: r.dns.clone(),
            sort_order: r.sort_order,
        }
    }
}

// ── docker inspect structs ─────────────────────────────────────────────────

#[derive(Deserialize)]
struct InspectResult {
    #[serde(rename = "Id")]
    #[allow(dead_code)]
    id: String,
    #[serde(rename = "Size")]
    size: u64,
    #[serde(rename = "RepoDigests")]
    repo_digests: Vec<String>,
}

// ── image metadata response types ──────────────────────────────────────────

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
    /// The bare `sha256:…` portion extracted from `digest` for easy comparison.
    pub digest_short: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct StoreMeta {
    pub images: Vec<ImageMeta>,
}

// ── helper ─────────────────────────────────────────────────────────────────

fn inspect_image(image: &str) -> Option<ImageMeta> {
    let output = Command::new("docker")
        .args(["image", "inspect", "--format", "{{json .}}", image])
        .output()
        .ok()?;

    if !output.status.success() {
        return None;
    }

    let raw = String::from_utf8_lossy(&output.stdout);
    let raw = raw.trim();
    let json_str = if raw.starts_with('[') {
        let inner = raw.trim_start_matches('[').trim_end_matches(']').trim();
        inner.to_string()
    } else {
        raw.to_string()
    };

    let info: InspectResult = serde_json::from_str(&json_str).ok()?;

    let size_mb = (info.size as f64) / (1024.0 * 1024.0);
    let size_mb = (size_mb * 10.0).round() / 10.0;

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

/// Collect all image references that need to be inspected.
/// When the DB is available we use the catalog from the database;
/// otherwise returns an empty list (no images to inspect).
async fn collect_store_images(state: &AppState) -> Vec<String> {
    if let Some(store) = &state.agenthub_store {
        if let Ok(catalog) = store.list_store_templates().await {
            return catalog.iter().map(|t| t.image_cn.clone()).collect();
        }
    }
    Vec::new()
}

// ── catalog handlers ───────────────────────────────────────────────────────

/// GET /cubeapi/v1/store/catalog
///
/// List all store template catalog entries.  Requires a configured database;
/// returns an empty list when no database is available.
pub async fn list_store_catalog(State(state): State<AppState>) -> impl IntoResponse {
    let items: Vec<StoreCatalogItem> = match &state.agenthub_store {
        Some(store) => match store.list_store_templates().await {
            Ok(rows) => rows.into_iter().map(StoreCatalogItem::from).collect(),
            Err(err) => {
                tracing::warn!(error = %err, "failed to read store catalog from DB");
                Vec::new()
            }
        },
        None => Vec::new(),
    };
    (StatusCode::OK, Json(StoreCatalogResponse { items })).into_response()
}

/// POST /cubeapi/v1/store/catalog
///
/// Create or upsert a store template catalog entry.
pub async fn create_store_catalog_item(
    State(state): State<AppState>,
    Json(body): Json<UpsertStoreCatalogRequest>,
) -> impl IntoResponse {
    let store = match &state.agenthub_store {
        Some(s) => s,
        None => {
            return (
                StatusCode::SERVICE_UNAVAILABLE,
                Json(serde_json::json!({"error": "Database not configured"})),
            )
                .into_response()
        }
    };

    let record = StoreTemplateRecord::from(&body);
    match store.create_store_template(&record).await {
        Ok(()) => (StatusCode::CREATED, Json(StoreCatalogItem::from(record))).into_response(),
        Err(err) => {
            tracing::error!(error = %err, "failed to create store catalog item");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(serde_json::json!({"error": err.to_string()})),
            )
                .into_response()
        }
    }
}

/// PATCH /cubeapi/v1/store/catalog/:itemID
///
/// Update an existing store template catalog entry.
pub async fn update_store_catalog_item(
    State(state): State<AppState>,
    Path(item_id): Path<String>,
    Json(body): Json<UpsertStoreCatalogRequest>,
) -> impl IntoResponse {
    let store = match &state.agenthub_store {
        Some(s) => s,
        None => {
            return (
                StatusCode::SERVICE_UNAVAILABLE,
                Json(serde_json::json!({"error": "Database not configured"})),
            )
                .into_response()
        }
    };

    // Ensure the path itemID matches the body id
    let mut record = StoreTemplateRecord::from(&body);
    record.item_id = item_id;

    match store.update_store_template(&record).await {
        Ok(()) => (StatusCode::OK, Json(StoreCatalogItem::from(record))).into_response(),
        Err(err) => {
            tracing::error!(error = %err, "failed to update store catalog item");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(serde_json::json!({"error": err.to_string()})),
            )
                .into_response()
        }
    }
}

/// DELETE /cubeapi/v1/store/catalog/:itemID
///
/// Soft-delete a store template catalog entry.
pub async fn delete_store_catalog_item(
    State(state): State<AppState>,
    Path(item_id): Path<String>,
) -> impl IntoResponse {
    let store = match &state.agenthub_store {
        Some(s) => s,
        None => {
            return (
                StatusCode::SERVICE_UNAVAILABLE,
                Json(serde_json::json!({"error": "Database not configured"})),
            )
                .into_response()
        }
    };

    match store.soft_delete_store_template(&item_id).await {
        Ok(()) => StatusCode::NO_CONTENT.into_response(),
        Err(err) => {
            tracing::error!(error = %err, "failed to delete store catalog item");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(serde_json::json!({"error": err.to_string()})),
            )
                .into_response()
        }
    }
}

// ── image metadata handlers ────────────────────────────────────────────────

/// GET /cubeapi/v1/store/meta
///
/// Returns live image metadata (digest + size) for all store templates.
/// Images not present locally are omitted.
pub async fn get_store_meta(State(state): State<AppState>) -> impl IntoResponse {
    let images = collect_store_images(&state).await;

    let meta = tokio::task::spawn_blocking(move || {
        images
            .iter()
            .filter_map(|img| inspect_image(img))
            .collect::<Vec<_>>()
    })
    .await
    .unwrap_or_default();

    (StatusCode::OK, Json(StoreMeta { images: meta }))
}

/// POST /cubeapi/v1/store/refresh
///
/// Pulls all store images from the registry then returns the updated metadata.
pub async fn refresh_store_meta(State(state): State<AppState>) -> impl IntoResponse {
    let images = collect_store_images(&state).await;

    // Limit concurrent docker pulls to avoid saturating the Docker daemon and
    // network bandwidth when the store catalog is large.
    const MAX_CONCURRENT_PULLS: usize = 3;
    let sem = Arc::new(Semaphore::new(MAX_CONCURRENT_PULLS));

    let handles: Vec<_> = images
        .iter()
        .map(|img| {
            let img = img.clone();
            let sem = Arc::clone(&sem);
            tokio::task::spawn(async move {
                let _permit = sem.acquire_owned().await;
                tokio::task::spawn_blocking(move || {
                    let _ = Command::new("docker")
                        .args(["pull", "--quiet", &img])
                        .output();
                    inspect_image(&img)
                })
                .await
                .ok()
                .flatten()
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

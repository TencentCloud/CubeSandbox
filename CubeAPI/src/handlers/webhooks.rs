// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Webhook management API.
//!
//! Runtime CRUD over the webhook endpoints CubeAPI notifies: list, register,
//! and remove endpoints without a restart. Secrets are never returned — only
//! whether one is set. Changes are in-memory and do not survive a restart;
//! the config file provides the startup set.

use crate::config::WebhookConfig;
use crate::error::{AppError, AppResult};
use crate::logging::http::WebhookEntry;
use crate::models::ApiError;
use crate::state::AppState;
use axum::{
    extract::{Path, State},
    http::StatusCode,
    response::IntoResponse,
    Json,
};
use serde::Serialize;
use utoipa::ToSchema;

/// A configured webhook endpoint, as exposed by the management API.
///
/// Note: the HMAC `secret` is deliberately **not** included — we only report
/// whether one is configured, never its value.
#[derive(Serialize, ToSchema)]
pub struct WebhookView {
    /// Stable id of this endpoint (assigned when registered).
    pub id: String,
    /// Destination URL events are POSTed to.
    pub url: String,
    /// Event types this endpoint is subscribed to.
    pub events: Vec<String>,
    /// Whether an HMAC signing secret is configured (value never exposed).
    pub has_secret: bool,
    /// Per-request delivery timeout in milliseconds.
    pub timeout_ms: u64,
    /// Number of retries after the first delivery attempt.
    pub max_retries: u32,
}

impl From<WebhookEntry> for WebhookView {
    fn from(e: WebhookEntry) -> Self {
        WebhookView {
            id: e.id,
            url: e.config.url,
            events: e.config.events,
            has_secret: e.config.secret.is_some(),
            timeout_ms: e.config.timeout_ms,
            max_retries: e.config.max_retries,
        }
    }
}

/// GET /webhooks — list configured webhook endpoints (secrets masked).
#[utoipa::path(
    get,
    path = "/webhooks",
    responses(
        (status = 200, description = "Configured webhook endpoints", body = [WebhookView])
    )
)]
pub async fn list_webhooks(State(state): State<AppState>) -> Json<Vec<WebhookView>> {
    let views = state
        .webhooks
        .list()
        .into_iter()
        .map(WebhookView::from)
        .collect();
    Json(views)
}

/// POST /webhooks — register a new webhook endpoint. The server assigns the id.
#[utoipa::path(
    post,
    path = "/webhooks",
    request_body = WebhookConfig,
    responses(
        (status = 201, description = "Webhook registered", body = WebhookView),
        (status = 400, description = "Invalid request", body = ApiError)
    )
)]
pub async fn create_webhook(
    State(state): State<AppState>,
    Json(config): Json<WebhookConfig>,
) -> AppResult<impl IntoResponse> {
    let url = config.url.trim();
    if url.is_empty() {
        return Err(AppError::BadRequest("url must not be empty".into()));
    }
    // Only http(s) endpoints are deliverable. Rejecting other schemes at
    // registration gives a clear 400 instead of a delivery that fails silently
    // through the retry path, and keeps this SSRF-adjacent surface to plain
    // HTTP callbacks. (Deeper SSRF hardening — blocking internal/metadata
    // addresses — is out of scope here.)
    if !(url.starts_with("http://") || url.starts_with("https://")) {
        return Err(AppError::BadRequest(
            "url must start with http:// or https://".into(),
        ));
    }
    if config.events.is_empty() {
        return Err(AppError::BadRequest("events must not be empty".into()));
    }
    let entry = state.webhooks.add(config);
    Ok((StatusCode::CREATED, Json(WebhookView::from(entry))))
}

/// DELETE /webhooks/{id} — remove a webhook endpoint by id.
#[utoipa::path(
    delete,
    path = "/webhooks/{id}",
    params(("id" = String, Path, description = "Webhook id")),
    responses(
        (status = 204, description = "Webhook removed"),
        (status = 404, description = "No webhook with that id", body = ApiError)
    )
)]
pub async fn delete_webhook(
    State(state): State<AppState>,
    Path(id): Path<String>,
) -> AppResult<impl IntoResponse> {
    if state.webhooks.remove(&id) {
        Ok(StatusCode::NO_CONTENT)
    } else {
        Err(AppError::NotFound(format!("webhook {id}")))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::{ServerConfig, WebhookConfig};
    use crate::logging::http::WebhookRegistry;
    use crate::logging::{arc, noop::NoopLogger};
    use crate::routes::build_router;
    use axum::http::StatusCode;
    use axum_test::TestServer;

    #[tokio::test]
    async fn lists_configured_webhooks_and_masks_secrets() {
        let config = ServerConfig {
            webhooks: vec![
                WebhookConfig {
                    url: "http://a/hook".into(),
                    events: vec!["sandbox.created".into()],
                    secret: Some("super-secret".into()),
                    timeout_ms: 5000,
                    max_retries: 3,
                },
                WebhookConfig {
                    url: "http://b/hook".into(),
                    events: vec!["sandbox.deleted".into()],
                    secret: None,
                    timeout_ms: 1000,
                    max_retries: 0,
                },
            ],
            ..ServerConfig::default()
        };
        let registry = WebhookRegistry::from_configs(config.webhooks.clone());
        let state = AppState::new(config, arc(NoopLogger), registry).await;
        let server = TestServer::new(build_router(state)).expect("router should build");

        let resp = server.get("/webhooks").await;
        assert_eq!(resp.status_code(), StatusCode::OK);

        // The secret VALUE must never appear anywhere in the response.
        let text = resp.text();
        assert!(!text.contains("super-secret"), "secret leaked: {text}");

        let body: serde_json::Value = resp.json();
        let list = body.as_array().expect("response is a JSON array");
        assert_eq!(list.len(), 2);

        assert!(list[0]["id"].is_string()); // id is now a registry-assigned string
        assert_eq!(list[0]["url"], "http://a/hook");
        assert_eq!(list[0]["events"][0], "sandbox.created");
        assert_eq!(list[0]["has_secret"], true); // secret set → true, value hidden
        assert_eq!(list[1]["url"], "http://b/hook");
        assert_eq!(list[1]["has_secret"], false);
    }

    #[tokio::test]
    async fn create_then_delete_webhook() {
        // Start with an empty registry (no config-file endpoints).
        let state = AppState::new(
            ServerConfig::default(),
            arc(NoopLogger),
            WebhookRegistry::default(),
        )
        .await;
        let server = TestServer::new(build_router(state)).expect("router should build");

        let count = |body: serde_json::Value| body.as_array().unwrap().len();

        // Initially empty.
        assert_eq!(count(server.get("/webhooks").await.json()), 0);

        // Create one.
        let resp = server
            .post("/webhooks")
            .json(&serde_json::json!({
                "url": "http://x/hook",
                "events": ["sandbox.created"],
                "secret": "shh"
            }))
            .await;
        assert_eq!(resp.status_code(), StatusCode::CREATED);
        assert!(!resp.text().contains("shh"), "secret must not be echoed");
        let created: serde_json::Value = resp.json();
        let id = created["id"]
            .as_str()
            .expect("server-assigned id")
            .to_string();
        assert_eq!(created["url"], "http://x/hook");
        assert_eq!(created["has_secret"], true);

        // It now shows up in the list.
        assert_eq!(count(server.get("/webhooks").await.json()), 1);

        // Delete it.
        let del = server.delete(&format!("/webhooks/{id}")).await;
        assert_eq!(del.status_code(), StatusCode::NO_CONTENT);
        assert_eq!(count(server.get("/webhooks").await.json()), 0);

        // Deleting a non-existent id is a 404.
        let missing = server.delete("/webhooks/does-not-exist").await;
        assert_eq!(missing.status_code(), StatusCode::NOT_FOUND);
    }

    #[tokio::test]
    async fn create_webhook_rejects_empty_events() {
        let state = AppState::new(
            ServerConfig::default(),
            arc(NoopLogger),
            WebhookRegistry::default(),
        )
        .await;
        let server = TestServer::new(build_router(state)).expect("router should build");

        let resp = server
            .post("/webhooks")
            .json(&serde_json::json!({ "url": "http://x/hook", "events": [] }))
            .await;
        assert_eq!(resp.status_code(), StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn create_webhook_rejects_non_http_url() {
        let state = AppState::new(
            ServerConfig::default(),
            arc(NoopLogger),
            WebhookRegistry::default(),
        )
        .await;
        let server = TestServer::new(build_router(state)).expect("router should build");

        for bad in ["ftp://host/hook", "not-a-url", "javascript:alert(1)"] {
            let resp = server
                .post("/webhooks")
                .json(&serde_json::json!({ "url": bad, "events": ["sandbox.created"] }))
                .await;
            assert_eq!(
                resp.status_code(),
                StatusCode::BAD_REQUEST,
                "non-http url {bad:?} must be rejected"
            );
        }
    }
}

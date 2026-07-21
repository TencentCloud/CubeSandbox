// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Webhook management API.
//!
//! Read-only for now: lists the webhook endpoints CubeAPI is configured to
//! notify. Secrets are never returned — only whether one is set.

use crate::state::AppState;
use axum::{extract::State, Json};
use serde::Serialize;
use utoipa::ToSchema;

/// A configured webhook endpoint, as exposed by the management API.
///
/// Note: the HMAC `secret` is deliberately **not** included — we only report
/// whether one is configured, never its value.
#[derive(Serialize, ToSchema)]
pub struct WebhookView {
    /// Stable index of this endpoint within the configured list.
    pub id: usize,
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
        .config
        .webhooks
        .iter()
        .enumerate()
        .map(|(id, w)| WebhookView {
            id,
            url: w.url.clone(),
            events: w.events.clone(),
            has_secret: w.secret.is_some(),
            timeout_ms: w.timeout_ms,
            max_retries: w.max_retries,
        })
        .collect();
    Json(views)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::{ServerConfig, WebhookConfig};
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
        let state = AppState::new(config, arc(NoopLogger)).await;
        let server = TestServer::new(build_router(state)).expect("router should build");

        let resp = server.get("/webhooks").await;
        assert_eq!(resp.status_code(), StatusCode::OK);

        // The secret VALUE must never appear anywhere in the response.
        let text = resp.text();
        assert!(!text.contains("super-secret"), "secret leaked: {text}");

        let body: serde_json::Value = resp.json();
        let list = body.as_array().expect("response is a JSON array");
        assert_eq!(list.len(), 2);

        assert_eq!(list[0]["id"], 0);
        assert_eq!(list[0]["url"], "http://a/hook");
        assert_eq!(list[0]["events"][0], "sandbox.created");
        assert_eq!(list[0]["has_secret"], true); // secret set → true, value hidden
        assert_eq!(list[1]["url"], "http://b/hook");
        assert_eq!(list[1]["has_secret"], false);
    }
}

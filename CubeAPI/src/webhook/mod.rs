// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Webhook event notification module.
//!
//! Provides asynchronous, non-blocking delivery of sandbox lifecycle events
//! to user-configured HTTP endpoints.  Delivery is fire-and-forget from the
//! perspective of the API handler — each dispatch spawns an independent Tokio
//! task so that sandbox create / delete / pause / resume paths are never
//! blocked by a slow or unreachable receiver.
//!
//! # Configuration
//!
//! Webhook endpoints are configured via environment variables:
//!
//! | Variable                | Description                              | Default          |
//! |-------------------------|------------------------------------------|------------------|
//! | `WEBHOOK_ENDPOINTS`     | Comma-separated endpoint URLs            | (none)           |
//! | `WEBHOOK_EVENTS`        | Comma-separated event types to subscribe | all 4 lifecycle  |
//! | `WEBHOOK_SECRET`        | Shared secret for HMAC-SHA256 signing    | (none / off)     |
//! | `WEBHOOK_RETRY_MAX`     | Maximum delivery retries                 | 3                |
//! | `WEBHOOK_RETRY_BASE_MS` | Base backoff in milliseconds             | 1000             |
//!
//! # Payload Format
//!
//! Each webhook is an HTTP POST with `Content-Type: application/json`:
//!
//! ```json
//! {
//!   "event": "sandbox.created",
//!   "timestamp": "2026-07-27T00:00:00Z",
//!   "sandbox_id": "sb-abc123",
//!   "template_id": "tpl-xyz"
//! }
//! ```
//!
//! When signing is enabled, the `X-Cube-Webhook-Signature` header contains
//! a hex-encoded HMAC-SHA256 of the request body.

use chrono::{DateTime, Utc};
use hmac::{Hmac, Mac};
use serde::Serialize;
use sha2::Sha256;
use std::sync::Arc;
use std::time::Duration;

// ─── Configuration ──────────────────────────────────────────────────────────

/// Webhook endpoint configuration parsed from environment variables.
#[derive(Debug, Clone)]
pub struct WebhookConfig {
    /// One or more HTTP(S) endpoint URLs to deliver events to.
    pub endpoints: Vec<String>,

    /// Which event types to deliver.  When empty all known types are delivered.
    pub events: Vec<String>,

    /// Optional shared secret for HMAC-SHA256 request signing.
    pub secret: Option<String>,

    /// Maximum number of delivery retries before giving up.
    pub retry_max: u32,

    /// Base backoff in milliseconds (doubles each retry).
    pub retry_base_ms: u64,
}

impl Default for WebhookConfig {
    fn default() -> Self {
        Self {
            endpoints: Vec::new(),
            events: vec![
                "sandbox.created".to_string(),
                "sandbox.deleted".to_string(),
                "sandbox.paused".to_string(),
                "sandbox.resumed".to_string(),
            ],
            secret: None,
            retry_max: 3,
            retry_base_ms: 1000,
        }
    }
}

impl WebhookConfig {
    /// Build configuration from environment variables.
    ///
    /// - `WEBHOOK_ENDPOINTS`: comma-separated URLs
    /// - `WEBHOOK_EVENTS`: comma-separated event type names (optional)
    /// - `WEBHOOK_SECRET`: HMAC shared secret (optional)
    /// - `WEBHOOK_RETRY_MAX`: max attempts per delivery (default 3)
    /// - `WEBHOOK_RETRY_BASE_MS`: initial backoff in ms (default 1000)
    pub fn from_env() -> Self {
        let mut cfg = Self::default();

        if let Ok(raw) = std::env::var("WEBHOOK_ENDPOINTS") {
            cfg.endpoints = raw
                .split(',')
                .map(|s| s.trim().to_string())
                .filter(|s| !s.is_empty())
                .collect();
        }

        if let Ok(raw) = std::env::var("WEBHOOK_EVENTS") {
            cfg.events = raw
                .split(',')
                .map(|s| s.trim().to_string())
                .filter(|s| !s.is_empty())
                .collect();
        }

        if let Ok(secret) = std::env::var("WEBHOOK_SECRET") {
            let secret = secret.trim().to_string();
            if !secret.is_empty() {
                cfg.secret = Some(secret);
            }
        }

        if let Ok(val) = std::env::var("WEBHOOK_RETRY_MAX") {
            match val.trim().parse::<u32>() {
                Ok(n) => cfg.retry_max = n,
                Err(_) => tracing::warn!(
                    "WEBHOOK_RETRY_MAX: invalid value \"{}\", using default {}",
                    val, cfg.retry_max
                ),
            }
        }

        if let Ok(val) = std::env::var("WEBHOOK_RETRY_BASE_MS") {
            match val.trim().parse::<u64>() {
                Ok(n) => cfg.retry_base_ms = n,
                Err(_) => tracing::warn!(
                    "WEBHOOK_RETRY_BASE_MS: invalid value \"{}\", using default {}",
                    val, cfg.retry_base_ms
                ),
            }
        }

        cfg
    }

    /// Whether any endpoints are configured.
    pub fn is_enabled(&self) -> bool {
        !self.endpoints.is_empty()
    }

    /// Whether the given event type should be delivered.
    fn should_deliver(&self, event_type: &str) -> bool {
        self.events.iter().any(|e| e == event_type)
    }
}

// ─── Webhook Payload ────────────────────────────────────────────────────────

/// JSON body sent to each webhook endpoint.
#[derive(Debug, Clone, Serialize)]
pub struct WebhookPayload {
    /// Event type, e.g. "sandbox.created"
    pub event: String,
    /// ISO-8601 UTC timestamp of the event
    pub timestamp: DateTime<Utc>,
    /// ID of the sandbox that triggered the event
    pub sandbox_id: String,
    /// Template ID associated with the sandbox (when available)
    #[serde(skip_serializing_if = "Option::is_none")]
    pub template_id: Option<String>,
}

// ─── Dispatcher ─────────────────────────────────────────────────────────────

type HmacSha256 = Hmac<Sha256>;

/// Asynchronous webhook dispatcher.
///
/// Holds a shared configuration and HTTP client.  `dispatch()` spawns a
/// Tokio task so delivery never blocks the calling handler.
#[derive(Clone)]
pub struct WebhookDispatcher {
    config: Arc<WebhookConfig>,
    client: reqwest::Client,
}

impl WebhookDispatcher {
    /// Create a new dispatcher.  Returns `None` when no endpoints are configured.
    pub fn new(config: WebhookConfig, client: reqwest::Client) -> Option<Self> {
        if !config.is_enabled() {
            tracing::info!("webhook: no endpoints configured, webhook delivery disabled");
            return None;
        }
        tracing::info!(
            endpoints = ?config.endpoints,
            events = ?config.events,
            signing = config.secret.is_some(),
            "webhook: dispatcher initialized"
        );
        Some(Self {
            config: Arc::new(config),
            client,
        })
    }

    /// Dispatch a webhook event asynchronously.
    ///
    /// Returns immediately after spawning a background delivery task.
    /// The caller (typically an API handler) is never blocked.
    pub fn dispatch(
        &self,
        event_type: impl Into<String>,
        sandbox_id: impl Into<String>,
        template_id: Option<String>,
    ) {
        let event_type = event_type.into();

        if !self.config.should_deliver(&event_type) {
            return;
        }

        let payload = WebhookPayload {
            event: event_type,
            timestamp: Utc::now(),
            sandbox_id: sandbox_id.into(),
            template_id,
        };

        let config = Arc::clone(&self.config);
        let client = self.client.clone();

        let handle = tokio::spawn(async move {
            deliver_to_all(&config, &client, &payload).await;
        });
        tokio::spawn(async move {
            if let Err(e) = handle.await {
                tracing::error!(
                    ?e,
                    event = %payload.event,
                    sandbox_id = %payload.sandbox_id,
                    "webhook: dispatch task panicked"
                );
            }
        });
    }
}

/// Deliver a payload to every configured endpoint with retries.
async fn deliver_to_all(
    config: &WebhookConfig,
    client: &reqwest::Client,
    payload: &WebhookPayload,
) {
    let body = match serde_json::to_vec(payload) {
        Ok(b) => b,
        Err(e) => {
            tracing::error!(error = %e, "webhook: failed to serialize payload");
            return;
        }
    };

    let signature = config
        .secret
        .as_ref()
        .map(|secret| sign_payload(secret, &body));

    for endpoint in &config.endpoints {
        let client = client.clone();
        let body = body.clone();
        let signature = signature.clone();
        let endpoint = endpoint.clone();
        let retry_max = config.retry_max;
        let retry_base_ms = config.retry_base_ms;

        let handle = tokio::spawn(async move {
            deliver_with_retry(
                &client,
                &endpoint,
                &body,
                signature.as_deref(),
                retry_max,
                retry_base_ms,
            )
            .await;
        });
        tokio::spawn(async move {
            if let Err(e) = handle.await {
                tracing::error!(
                    ?e,
                    endpoint = %endpoint,
                    "webhook: per-endpoint delivery task panicked"
                );
            }
        });
    }
}

/// Attempt delivery to a single endpoint with exponential backoff retry.
async fn deliver_with_retry(
    client: &reqwest::Client,
    endpoint: &str,
    body: &[u8],
    signature: Option<&str>,
    retry_max: u32,
    retry_base_ms: u64,
) {
    let event_info: String =
        if let Ok(payload) = serde_json::from_slice::<serde_json::Value>(body) {
            format!(
                "event={} sandbox_id={}",
                payload
                    .get("event")
                    .and_then(|v| v.as_str())
                    .unwrap_or("?"),
                payload
                    .get("sandbox_id")
                    .and_then(|v| v.as_str())
                    .unwrap_or("?")
            )
        } else {
            "unknown".to_string()
        };

    for attempt in 0..=retry_max {
        if attempt > 0 {
            let backoff_ms = retry_base_ms * (1u64 << (attempt - 1));
            tracing::warn!(
                endpoint = %endpoint,
                attempt = attempt,
                backoff_ms = backoff_ms,
                "webhook: retrying delivery ({})",
                event_info
            );
            tokio::time::sleep(Duration::from_millis(backoff_ms)).await;
        }

        let mut req = client
            .post(endpoint)
            .header("Content-Type", "application/json")
            .header("User-Agent", "CubeSandbox-Webhook/1.0")
            .body(body.to_vec());

        if let Some(sig) = signature {
            req = req.header("X-Cube-Webhook-Signature", sig);
        }

        match req.send().await {
            Ok(resp) if resp.status().is_success() => {
                tracing::debug!(
                    endpoint = %endpoint,
                    status = %resp.status(),
                    "webhook: delivered ({})",
                    event_info
                );
                return;
            }
            Ok(resp) => {
                tracing::warn!(
                    endpoint = %endpoint,
                    status = %resp.status(),
                    attempt = attempt + 1,
                    "webhook: delivery failed with non-2xx ({})",
                    event_info
                );
            }
            Err(e) => {
                tracing::error!(
                    endpoint = %endpoint,
                    error = %e,
                    attempt = attempt + 1,
                    "webhook: delivery error ({})",
                    event_info
                );
            }
        }
    }

    tracing::error!(
        endpoint = %endpoint,
        max_retries = retry_max,
        "webhook: exhausted retries, giving up ({})",
        event_info
    );
}

/// Compute HMAC-SHA256 signature of the request body.
fn sign_payload(secret: &str, body: &[u8]) -> String {
    let mut mac =
        HmacSha256::new_from_slice(secret.as_bytes()).expect("HMAC can take a key of any size");
    mac.update(body);
    let result = mac.finalize();
    hex::encode(result.into_bytes())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn config_defaults() {
        let cfg = WebhookConfig::default();
        assert!(cfg.endpoints.is_empty());
        assert!(!cfg.is_enabled());
        assert_eq!(cfg.events.len(), 4);
        assert!(cfg.events.contains(&"sandbox.created".to_string()));
        assert!(cfg.events.contains(&"sandbox.deleted".to_string()));
        assert!(cfg.events.contains(&"sandbox.paused".to_string()));
        assert!(cfg.events.contains(&"sandbox.resumed".to_string()));
        assert!(cfg.secret.is_none());
        assert_eq!(cfg.retry_max, 3);
        assert_eq!(cfg.retry_base_ms, 1000);
    }

    #[test]
    fn should_deliver_subscribed_event() {
        let cfg = WebhookConfig {
            events: vec!["sandbox.created".to_string()],
            ..Default::default()
        };
        assert!(cfg.should_deliver("sandbox.created"));
        assert!(!cfg.should_deliver("sandbox.deleted"));
    }

    #[test]
    fn payload_serialization() {
        let payload = WebhookPayload {
            event: "sandbox.created".to_string(),
            timestamp: Utc::now(),
            sandbox_id: "sb-test".to_string(),
            template_id: Some("tpl-test".to_string()),
        };
        let json = serde_json::to_value(&payload).unwrap();
        assert_eq!(json["event"], "sandbox.created");
        assert_eq!(json["sandbox_id"], "sb-test");
        assert_eq!(json["template_id"], "tpl-test");
        assert!(json["timestamp"].is_string());
    }

    #[test]
    fn payload_serialization_without_template_id() {
        let payload = WebhookPayload {
            event: "sandbox.deleted".to_string(),
            timestamp: Utc::now(),
            sandbox_id: "sb-test".to_string(),
            template_id: None,
        };
        let json = serde_json::to_value(&payload).unwrap();
        assert!(
            json.get("template_id").is_none(),
            "template_id=None should be omitted"
        );
    }

    #[test]
    fn sign_payload_produces_valid_hmac() {
        let secret = "test-secret";
        let body = b"{\"event\":\"sandbox.created\"}";
        let sig = sign_payload(secret, body);

        assert_eq!(sig.len(), 64);
        assert!(sig.chars().all(|c| c.is_ascii_hexdigit()));

        let sig2 = sign_payload(secret, body);
        assert_eq!(sig, sig2);

        let sig3 = sign_payload("other-secret", body);
        assert_ne!(sig, sig3);

        let sig4 = sign_payload(secret, b"different body");
        assert_ne!(sig, sig4);
    }
}

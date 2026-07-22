// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use serde::Deserialize;
use std::fmt;

#[derive(Debug, Deserialize, Clone)]
pub struct ServerConfig {
    /// Bind address, e.g. "0.0.0.0:3000". Env var: CUBE_API_BIND (default "0.0.0.0:3000")
    #[serde(default = "default_bind")]
    pub bind: String,

    /// Log level: trace | debug | info | warn | error
    #[serde(default = "default_log_level")]
    pub log_level: String,

    /// Tokio worker thread count (0 = number of CPU cores)
    #[serde(default = "default_worker_threads")]
    pub worker_threads: usize,

    /// Rate limit: max requests per second per API key
    #[serde(default = "default_rate_limit")]
    pub rate_limit_per_sec: u32,

    /// CubeMaster base URL, e.g. "http://10.0.0.1:8080". Env var: CUBE_MASTER_ADDR (default "http://127.0.0.1:8089")
    #[serde(default = "default_cubemaster_url")]
    pub cubemaster_url: String,

    /// Default instance_type sent to CubeMaster ("cubebox")
    #[serde(default = "default_instance_type")]
    pub instance_type: String,

    /// Domain returned in sandbox API responses (`domain` JSON field). Env: CUBE_API_SANDBOX_DOMAIN (default "cube.app")
    #[serde(default = "default_sandbox_domain")]
    pub sandbox_domain: String,

    /// Directory for rolling log files (default: <binary_dir>/log)
    #[serde(default = "default_log_dir")]
    pub log_dir: String,

    /// File log prefix, e.g. "cube-api" → "cube-api-2026-03-16.log"
    #[serde(default = "default_log_prefix")]
    pub log_prefix: String,

    /// Auth callback URL for HTTP authentication.
    ///
    /// When set, every request (except /health) must carry either:
    ///   - `Authorization: Bearer <token>`, or
    ///   - `X-API-Key: <key>`
    ///
    /// The middleware will POST to this URL with the credential headers plus:
    ///   - `X-Request-Path: <original request path>`
    ///   - `X-Request-Method: <HTTP method>` (e.g. GET, POST, DELETE, PATCH)
    ///
    /// An HTTP 200 response grants access; any other status code returns 401 to the client.
    ///
    /// When unset (default), all requests are allowed through without authentication.
    ///
    /// CLI flag: --auth-callback-url  |  Env var: AUTH_CALLBACK_URL
    #[serde(default)]
    pub auth_callback_url: Option<String>,

    /// Built-in simple API key for lightweight authentication.
    ///
    /// When `auth_callback_url` is unset and this field is set, every request
    /// (except /health) must carry either:
    ///   - `Authorization: Bearer <token>`, or
    ///   - `X-API-Key: <key>`
    ///
    /// The extracted credential is compared as a string against this value.
    /// A match grants access; a mismatch or missing credential returns 401.
    ///
    /// This is mutually exclusive with `auth_callback_url`: when both are set,
    /// `auth_callback_url` (callback mode) takes priority.
    ///
    /// Env var: CUBE_API_KEY
    #[serde(default)]
    pub cube_api_key: Option<String>,

    /// Webhook endpoints for structured lifecycle event delivery.
    ///
    /// Env var: CUBE_API_WEBHOOK_ENDPOINTS
    /// Value: JSON array of WebhookEndpointConfig objects.
    #[serde(default)]
    pub webhook_endpoints: Vec<WebhookEndpointConfig>,
}

#[derive(Deserialize, Clone)]
pub struct WebhookEndpointConfig {
    pub url: String,

    #[serde(default = "default_webhook_events")]
    pub events: Vec<String>,

    #[serde(default)]
    pub secret: Option<String>,

    #[serde(default = "default_webhook_queue_capacity")]
    pub queue_capacity: usize,

    #[serde(default = "default_webhook_max_retries")]
    pub max_retries: usize,

    #[serde(default = "default_webhook_retry_base_ms")]
    pub retry_base_ms: u64,

    #[serde(default = "default_webhook_retry_max_ms")]
    pub retry_max_ms: u64,

    #[serde(default = "default_webhook_timeout_secs")]
    pub timeout_secs: u64,
}

impl fmt::Debug for WebhookEndpointConfig {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("WebhookEndpointConfig")
            .field("url", &self.url)
            .field("events", &self.events)
            .field("secret", &self.secret.as_ref().map(|_| "<redacted>"))
            .field("queue_capacity", &self.queue_capacity)
            .field("max_retries", &self.max_retries)
            .field("retry_base_ms", &self.retry_base_ms)
            .field("retry_max_ms", &self.retry_max_ms)
            .field("timeout_secs", &self.timeout_secs)
            .finish()
    }
}

fn default_bind() -> String {
    std::env::var("CUBE_API_BIND").unwrap_or_else(|_| "0.0.0.0:3000".to_string())
}
fn default_log_level() -> String {
    "info".to_string()
}
fn default_worker_threads() -> usize {
    16
}
fn default_rate_limit() -> u32 {
    100
}
fn default_cubemaster_url() -> String {
    std::env::var("CUBE_MASTER_ADDR").unwrap_or_else(|_| "http://127.0.0.1:8089".to_string())
}
fn default_instance_type() -> String {
    "cubebox".to_string()
}
fn default_sandbox_domain() -> String {
    std::env::var("CUBE_API_SANDBOX_DOMAIN").unwrap_or_else(|_| "cube.app".to_string())
}
fn default_log_dir() -> String {
    std::env::current_exe()
        .ok()
        .and_then(|p| p.parent().map(|d| d.join("log")))
        .map(|p| p.display().to_string())
        .unwrap_or_else(|| "./log".to_string())
}
fn default_log_prefix() -> String {
    "cube-api".to_string()
}
fn default_webhook_events() -> Vec<String> {
    [
        "sandbox.created",
        "sandbox.deleted",
        "sandbox.paused",
        "sandbox.resumed",
    ]
    .into_iter()
    .map(str::to_string)
    .collect()
}
fn default_webhook_queue_capacity() -> usize {
    1024
}
fn default_webhook_max_retries() -> usize {
    3
}
fn default_webhook_retry_base_ms() -> u64 {
    500
}
fn default_webhook_retry_max_ms() -> u64 {
    30_000
}
fn default_webhook_timeout_secs() -> u64 {
    5
}

fn webhook_endpoints_from_env() -> anyhow::Result<Option<Vec<WebhookEndpointConfig>>> {
    let value = match std::env::var("CUBE_API_WEBHOOK_ENDPOINTS") {
        Ok(value) if !value.trim().is_empty() => value,
        _ => return Ok(None),
    };

    let endpoints = serde_json::from_str::<Vec<WebhookEndpointConfig>>(&value)
        .map_err(|e| anyhow::anyhow!("invalid CUBE_API_WEBHOOK_ENDPOINTS JSON: {e}"))?;
    Ok(Some(endpoints))
}

impl ServerConfig {
    pub fn from_env() -> anyhow::Result<Self> {
        let _ = dotenvy::dotenv();
        let mut cfg: Self = config::Config::builder()
            .add_source(config::Environment::default().separator("__"))
            .build()?
            .try_deserialize()?;
        if let Some(endpoints) = webhook_endpoints_from_env()? {
            cfg.webhook_endpoints = endpoints;
        }
        Ok(cfg)
    }
}

impl Default for ServerConfig {
    fn default() -> Self {
        Self {
            bind: default_bind(),
            log_level: default_log_level(),
            worker_threads: default_worker_threads(),
            rate_limit_per_sec: default_rate_limit(),
            cubemaster_url: default_cubemaster_url(),
            instance_type: default_instance_type(),
            sandbox_domain: default_sandbox_domain(),
            log_dir: default_log_dir(),
            log_prefix: default_log_prefix(),
            auth_callback_url: None,
            cube_api_key: std::env::var("CUBE_API_KEY").ok().filter(|s| !s.is_empty()),
            webhook_endpoints: webhook_endpoints_from_env()
                .ok()
                .flatten()
                .unwrap_or_default(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn webhook_endpoint_defaults_to_sandbox_lifecycle_events() {
        let endpoint: WebhookEndpointConfig =
            serde_json::from_str(r#"{"url":"http://127.0.0.1:9000/webhook"}"#)
                .expect("endpoint config");

        assert_eq!(
            endpoint.events,
            vec![
                "sandbox.created",
                "sandbox.deleted",
                "sandbox.paused",
                "sandbox.resumed",
            ]
        );
    }

    #[test]
    fn webhook_endpoint_debug_redacts_secret() {
        let endpoint: WebhookEndpointConfig = serde_json::from_str(
            r#"{"url":"http://127.0.0.1:9000/webhook","secret":"super-secret"}"#,
        )
        .expect("endpoint config");
        let debug = format!("{endpoint:?}");

        assert!(debug.contains("<redacted>"));
        assert!(!debug.contains("super-secret"));
    }
}

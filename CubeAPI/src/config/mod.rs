// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use anyhow::Context;
use serde::Deserialize;
use std::{fmt, str::FromStr};

#[derive(Clone, Deserialize)]
pub struct WebhookEndpointConfig {
    /// Stable diagnostic label. The URL is deliberately not logged because it
    /// may contain credentials in its query string.
    #[serde(default)]
    pub name: Option<String>,
    pub url: String,
    /// Empty means all supported sandbox lifecycle events.
    #[serde(default)]
    pub events: Vec<String>,
    /// Optional HMAC-SHA256 signing secret.
    #[serde(default)]
    pub secret: Option<String>,
}

impl fmt::Debug for WebhookEndpointConfig {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("WebhookEndpointConfig")
            .field("name", &self.name)
            .field("url", &"<redacted>")
            .field("events", &self.events)
            .field("secret", &self.secret.as_ref().map(|_| "<redacted>"))
            .finish()
    }
}

#[derive(Debug, Clone, Deserialize)]
pub struct WebhookConfig {
    #[serde(default)]
    pub endpoints: Vec<WebhookEndpointConfig>,
    #[serde(default = "default_webhook_queue_capacity")]
    pub queue_capacity: usize,
    #[serde(default = "default_webhook_max_in_flight")]
    pub max_in_flight: usize,
    #[serde(default = "default_webhook_timeout_ms")]
    pub timeout_ms: u64,
    #[serde(default = "default_webhook_max_attempts")]
    pub max_attempts: u32,
    #[serde(default = "default_webhook_retry_base_ms")]
    pub retry_base_ms: u64,
    #[serde(default = "default_webhook_retry_max_ms")]
    pub retry_max_ms: u64,
}

impl Default for WebhookConfig {
    fn default() -> Self {
        Self {
            endpoints: Vec::new(),
            queue_capacity: default_webhook_queue_capacity(),
            max_in_flight: default_webhook_max_in_flight(),
            timeout_ms: default_webhook_timeout_ms(),
            max_attempts: default_webhook_max_attempts(),
            retry_base_ms: default_webhook_retry_base_ms(),
            retry_max_ms: default_webhook_retry_max_ms(),
        }
    }
}

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

    /// Asynchronous sandbox lifecycle webhook delivery.
    #[serde(default)]
    pub webhooks: WebhookConfig,
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

fn default_webhook_queue_capacity() -> usize {
    1024
}

fn default_webhook_max_in_flight() -> usize {
    16
}

fn default_webhook_timeout_ms() -> u64 {
    5_000
}

fn default_webhook_max_attempts() -> u32 {
    3
}

fn default_webhook_retry_base_ms() -> u64 {
    500
}

fn default_webhook_retry_max_ms() -> u64 {
    30_000
}

impl ServerConfig {
    pub fn from_env() -> anyhow::Result<Self> {
        let _ = dotenvy::dotenv();
        // Preserve the existing fallback for generic configuration. Explicit
        // webhook overrides below still fail fast with actionable errors.
        let mut cfg: Self = config::Config::builder()
            .add_source(config::Environment::default().separator("__"))
            .build()
            .and_then(|config| config.try_deserialize())
            .unwrap_or_default();

        if let Ok(raw) = std::env::var("CUBE_API_WEBHOOKS") {
            if !raw.trim().is_empty() {
                cfg.webhooks.endpoints = parse_webhook_endpoints(&raw)?;
            }
        }
        apply_env_override(
            "CUBE_API_WEBHOOK_QUEUE_CAPACITY",
            &mut cfg.webhooks.queue_capacity,
        )?;
        apply_env_override(
            "CUBE_API_WEBHOOK_MAX_IN_FLIGHT",
            &mut cfg.webhooks.max_in_flight,
        )?;
        apply_env_override("CUBE_API_WEBHOOK_TIMEOUT_MS", &mut cfg.webhooks.timeout_ms)?;
        apply_env_override(
            "CUBE_API_WEBHOOK_MAX_ATTEMPTS",
            &mut cfg.webhooks.max_attempts,
        )?;
        apply_env_override(
            "CUBE_API_WEBHOOK_RETRY_BASE_MS",
            &mut cfg.webhooks.retry_base_ms,
        )?;
        apply_env_override(
            "CUBE_API_WEBHOOK_RETRY_MAX_MS",
            &mut cfg.webhooks.retry_max_ms,
        )?;

        Ok(cfg)
    }
}

fn parse_webhook_endpoints(raw: &str) -> anyhow::Result<Vec<WebhookEndpointConfig>> {
    serde_json::from_str(raw).context("CUBE_API_WEBHOOKS must be a JSON array of webhook endpoints")
}

fn apply_env_override<T>(name: &str, target: &mut T) -> anyhow::Result<()>
where
    T: FromStr,
    T::Err: fmt::Display,
{
    let Ok(raw) = std::env::var(name) else {
        return Ok(());
    };
    if raw.trim().is_empty() {
        return Ok(());
    }
    *target = raw
        .parse()
        .map_err(|err| anyhow::anyhow!("{name} must be a valid number: {err}"))?;
    Ok(())
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
            webhooks: WebhookConfig::default(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::{parse_webhook_endpoints, WebhookEndpointConfig};

    #[test]
    fn parses_multiple_webhook_endpoints_from_json() {
        let endpoints = parse_webhook_endpoints(
            r#"[
                {"url":"https://one.example/hook","events":["sandbox.created"]},
                {"name":"two","url":"http://two.example/hook","secret":"secret"}
            ]"#,
        )
        .unwrap();

        assert_eq!(endpoints.len(), 2);
        assert_eq!(endpoints[0].events, ["sandbox.created"]);
        assert_eq!(endpoints[1].name.as_deref(), Some("two"));
        assert_eq!(endpoints[1].secret.as_deref(), Some("secret"));
    }

    #[test]
    fn rejects_non_array_webhook_configuration() {
        assert!(parse_webhook_endpoints(r#"{"url":"https://example.com"}"#)
            .unwrap_err()
            .to_string()
            .contains("must be a JSON array"));
    }

    #[test]
    fn webhook_endpoint_debug_redacts_url_and_secret() {
        let endpoint = WebhookEndpointConfig {
            name: Some("alerts".to_string()),
            url: "https://example.com/hook?token=sensitive".to_string(),
            events: vec!["sandbox.created".to_string()],
            secret: Some("signing-secret".to_string()),
        };

        let debug = format!("{endpoint:?}");
        assert!(!debug.contains("sensitive"));
        assert!(!debug.contains("signing-secret"));
        assert!(debug.contains("<redacted>"));
    }
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use serde::Deserialize;

#[derive(Debug, Deserialize, Clone)]
pub struct WebhookEndpointConfig {
    pub url: String,
    #[serde(default = "default_webhook_events")]
    pub events: Vec<String>,
    #[serde(default)]
    pub secret: Option<String>,
}

pub fn default_webhook_events() -> Vec<String> {
    [
        "sandbox.created",
        "sandbox.deleted",
        "sandbox.paused",
        "sandbox.resumed",
    ]
    .into_iter()
    .map(str::to_owned)
    .collect()
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
    /// **Security note**: Multiple HTTP methods (e.g. GET/POST/DELETE/PATCH) are mounted
    /// on the same path (e.g. `/templates/:id`). Callbacks that only whitelist by path
    /// cannot distinguish read from write/delete operations. Always validate both
    /// `X-Request-Path` **and** `X-Request-Method` in your callback implementation.
    ///
    /// When unset (default), all requests are allowed through without authentication.
    ///
    /// CLI flag: --auth-callback-url  |  Env var: AUTH_CALLBACK_URL
    #[serde(default)]
    pub auth_callback_url: Option<String>,

    /// Optional MySQL database URL used by AgentHub persistence.
    ///
    /// Env var: `DATABASE_URL`. When unset, built from `CUBE_SANDBOX_MYSQL_*`.
    /// Example: mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp
    #[serde(default = "default_database_url")]
    pub database_url: Option<String>,

    /// JSON array of Webhook endpoints. Each endpoint has its own event filter
    /// and optional HMAC secret. Env var: WEBHOOK_ENDPOINTS_JSON.
    #[serde(default)]
    pub webhook_endpoints_json: Option<String>,

    #[serde(default = "default_webhook_queue_capacity")]
    pub webhook_queue_capacity: usize,
    #[serde(default = "default_webhook_max_retries")]
    pub webhook_max_retries: usize,
    #[serde(default = "default_webhook_retry_base_ms")]
    pub webhook_retry_base_ms: u64,
    #[serde(default = "default_webhook_request_timeout_secs")]
    pub webhook_request_timeout_secs: u64,
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
fn default_database_url() -> Option<String> {
    std::env::var("DATABASE_URL")
        .ok()
        .or_else(default_cube_sandbox_mysql_url)
}

fn default_webhook_queue_capacity() -> usize {
    1024
}
fn default_webhook_max_retries() -> usize {
    3
}
fn default_webhook_retry_base_ms() -> u64 {
    250
}
fn default_webhook_request_timeout_secs() -> u64 {
    10
}

fn default_cube_sandbox_mysql_url() -> Option<String> {
    let host = std::env::var("CUBE_SANDBOX_MYSQL_HOST").ok()?;
    let port = std::env::var("CUBE_SANDBOX_MYSQL_PORT").unwrap_or_else(|_| "3306".to_string());
    let user = std::env::var("CUBE_SANDBOX_MYSQL_USER").ok()?;
    let password = std::env::var("CUBE_SANDBOX_MYSQL_PASSWORD").ok()?;
    let database = std::env::var("CUBE_SANDBOX_MYSQL_DB").ok()?;

    Some(format!(
        "mysql://{}:{}@{}:{}/{}",
        user, password, host, port, database
    ))
}

impl ServerConfig {
    pub fn from_env() -> anyhow::Result<Self> {
        let _ = dotenvy::dotenv();
        let cfg = config::Config::builder()
            .add_source(config::Environment::default().separator("__"))
            .build()?
            .try_deserialize()?;
        Ok(cfg)
    }
}

#[cfg(test)]
mod webhook_tests {
    use super::*;

    fn config_with(raw: &str) -> ServerConfig {
        ServerConfig {
            webhook_endpoints_json: Some(raw.to_owned()),
            ..ServerConfig::default()
        }
    }

    #[test]
    fn rejects_invalid_webhook_json() {
        assert!(config_with("not-json").webhook_endpoints().is_err());
    }

    #[test]
    fn rejects_non_http_webhook_url() {
        let raw = r#"[{"url":"file:///tmp/hook","events":["sandbox.created"]}]"#;
        assert!(config_with(raw).webhook_endpoints().is_err());
    }

    #[test]
    fn rejects_empty_webhook_events() {
        let raw = r#"[{"url":"https://hooks.example.test","events":[]}]"#;
        assert!(config_with(raw).webhook_endpoints().is_err());
    }

    #[test]
    fn rejects_mixed_valid_and_invalid_webhooks() {
        let raw = r#"[
            {"url":"https://hooks.example.test","events":["sandbox.created"]},
            {"url":"ftp://invalid.example.test","events":["sandbox.deleted"]}
        ]"#;
        assert!(config_with(raw).webhook_endpoints().is_err());
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
            database_url: default_database_url(),
            webhook_endpoints_json: None,
            webhook_queue_capacity: default_webhook_queue_capacity(),
            webhook_max_retries: default_webhook_max_retries(),
            webhook_retry_base_ms: default_webhook_retry_base_ms(),
            webhook_request_timeout_secs: default_webhook_request_timeout_secs(),
        }
    }
}

impl ServerConfig {
    pub fn webhook_endpoints(&self) -> anyhow::Result<Vec<WebhookEndpointConfig>> {
        let Some(raw) = self.webhook_endpoints_json.as_deref() else {
            return Ok(Vec::new());
        };
        let endpoints: Vec<WebhookEndpointConfig> = serde_json::from_str(raw)
            .map_err(|err| anyhow::anyhow!("invalid WEBHOOK_ENDPOINTS_JSON: {err}"))?;
        for (index, endpoint) in endpoints.iter().enumerate() {
            let url = reqwest::Url::parse(&endpoint.url)
                .map_err(|err| anyhow::anyhow!("invalid webhook endpoint #{index}: {err}"))?;
            if !matches!(url.scheme(), "http" | "https") {
                anyhow::bail!("webhook endpoint #{index} must use http or https");
            }
            if endpoint.events.is_empty() {
                anyhow::bail!("webhook endpoint #{index} must subscribe to at least one event");
            }
        }
        Ok(endpoints)
    }
}

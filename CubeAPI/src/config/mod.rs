// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use serde::Deserialize;
use utoipa::ToSchema;

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

    /// Registered webhook endpoints for sandbox lifecycle event notifications.
    ///
    /// Populated from a separate config file (YAML/JSON/TOML) whose path is
    /// given by `--webhook-config` or the `CUBE_API_WEBHOOK_CONFIG` env var;
    /// see [`load_webhooks`]. Defaults to empty (webhooks disabled).
    #[serde(default)]
    pub webhooks: Vec<WebhookConfig>,
}

/// A single webhook subscription: where to POST and which events to send.
///
/// Also used as the request body of `POST /webhooks`.
#[derive(Debug, Deserialize, Clone, ToSchema)]
pub struct WebhookConfig {
    /// Full URL to POST events to, e.g. `"http://127.0.0.1:9100/webhook"`.
    pub url: String,

    /// Event types this endpoint subscribes to, e.g.
    /// `["sandbox.created", "sandbox.deleted"]`.
    pub events: Vec<String>,

    /// Optional shared secret. When set, each POST carries an
    /// `X-Cube-Signature: sha256=<hex>` header (HMAC-SHA256 of the body).
    #[serde(default)]
    pub secret: Option<String>,

    /// Per-request timeout in milliseconds (default: 5000).
    #[serde(default = "default_webhook_timeout_ms")]
    pub timeout_ms: u64,

    /// Max delivery retries after the first attempt (default: 3).
    /// Backoff is exponential: 1s, 2s, 4s, ...
    #[serde(default = "default_webhook_max_retries")]
    pub max_retries: u32,
}

fn default_webhook_timeout_ms() -> u64 {
    5000
}
fn default_webhook_max_retries() -> u32 {
    3
}

/// Wrapper matching the top-level `webhooks:` key of the webhook config file.
#[derive(Debug, Deserialize)]
struct WebhookFile {
    #[serde(default)]
    webhooks: Vec<WebhookConfig>,
}

/// Load webhook endpoints from a config file (YAML / JSON / TOML — format is
/// auto-detected from the extension).
///
/// The file must contain a top-level `webhooks` list, e.g. (YAML):
///
/// ```yaml
/// webhooks:
///   - url: "http://127.0.0.1:9100/webhook"
///     events: ["sandbox.created", "sandbox.deleted"]
///     secret: "my-shared-secret"
///     timeout_ms: 5000
///     max_retries: 3
/// ```
pub fn load_webhooks(path: &str) -> anyhow::Result<Vec<WebhookConfig>> {
    let parsed: WebhookFile = config::Config::builder()
        .add_source(config::File::with_name(path))
        .build()?
        .try_deserialize()?;
    Ok(parsed.webhooks)
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
mod tests {
    use super::*;
    use std::io::Write;

    #[test]
    fn load_webhooks_parses_yaml_and_fills_defaults() {
        let path = std::env::temp_dir().join("cube_api_webhooks_test.yaml");
        let yaml = r#"
webhooks:
  - url: "http://127.0.0.1:9100/webhook"
    events: ["sandbox.created", "sandbox.deleted"]
    secret: "s3cret"
  - url: "http://127.0.0.1:9200/hook"
    events: ["sandbox.paused"]
"#;
        std::fs::File::create(&path)
            .unwrap()
            .write_all(yaml.as_bytes())
            .unwrap();

        let hooks = load_webhooks(path.to_str().unwrap()).expect("should parse the YAML file");
        assert_eq!(hooks.len(), 2);

        // Explicit fields.
        assert_eq!(hooks[0].url, "http://127.0.0.1:9100/webhook");
        assert_eq!(hooks[0].events, vec!["sandbox.created", "sandbox.deleted"]);
        assert_eq!(hooks[0].secret.as_deref(), Some("s3cret"));
        // Omitted fields fall back to defaults.
        assert_eq!(hooks[0].timeout_ms, 5000);
        assert_eq!(hooks[0].max_retries, 3);

        assert_eq!(hooks[1].url, "http://127.0.0.1:9200/hook");
        assert_eq!(hooks[1].events, vec!["sandbox.paused"]);
        assert!(hooks[1].secret.is_none());

        let _ = std::fs::remove_file(&path);
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
            webhooks: Vec::new(),
        }
    }
}

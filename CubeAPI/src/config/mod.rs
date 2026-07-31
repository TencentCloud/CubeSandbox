// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use serde::Deserialize;

#[derive(Debug, Deserialize, Clone)]
pub struct WebhookEndpointConfig {
    /// Diagnostic name used in delivery logs.
    pub name: String,
    /// HTTP(S) endpoint that receives one JSON event per POST.
    pub url: String,
    /// Subscribed event names. `*` subscribes to all supported webhook events.
    pub events: Vec<String>,
    /// Optional HMAC-SHA256 signing secret.
    #[serde(default)]
    pub secret: Option<String>,
    /// Per-attempt request timeout.
    #[serde(default = "default_webhook_timeout_secs")]
    pub timeout_secs: u64,
    /// Number of retries after the initial delivery attempt.
    #[serde(default = "default_webhook_max_retries")]
    pub max_retries: u32,
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

    /// JSON array of webhook endpoint configurations.
    /// Env var: CUBE_API_WEBHOOK_ENDPOINTS
    #[serde(default)]
    pub webhook_endpoints: Vec<WebhookEndpointConfig>,

    /// Maximum number of pending webhook deliveries.
    /// Env var: CUBE_API_WEBHOOK_QUEUE_CAPACITY
    #[serde(default = "default_webhook_queue_capacity")]
    pub webhook_queue_capacity: usize,

    /// Maximum number of webhook HTTP requests in flight.
    /// Env var: CUBE_API_WEBHOOK_MAX_CONCURRENCY
    #[serde(default = "default_webhook_max_concurrency")]
    pub webhook_max_concurrency: usize,
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
fn default_webhook_timeout_secs() -> u64 {
    5
}
fn default_webhook_max_retries() -> u32 {
    4
}
fn default_webhook_queue_capacity() -> usize {
    1024
}
fn default_webhook_max_concurrency() -> usize {
    16
}

impl ServerConfig {
    pub fn from_env() -> anyhow::Result<Self> {
        let _ = dotenvy::dotenv();
        let mut cfg: Self = config::Config::builder()
            .add_source(config::Environment::default().separator("__"))
            .build()?
            .try_deserialize()
            .unwrap_or_default();

        if let Ok(value) = std::env::var("CUBE_API_WEBHOOK_ENDPOINTS") {
            if !value.trim().is_empty() {
                cfg.webhook_endpoints = serde_json::from_str(&value).map_err(|error| {
                    anyhow::anyhow!("invalid CUBE_API_WEBHOOK_ENDPOINTS JSON: {}", error)
                })?;
            }
        }
        if let Some(value) = parse_env("CUBE_API_WEBHOOK_QUEUE_CAPACITY")? {
            cfg.webhook_queue_capacity = value;
        }
        if let Some(value) = parse_env("CUBE_API_WEBHOOK_MAX_CONCURRENCY")? {
            cfg.webhook_max_concurrency = value;
        }
        Ok(cfg)
    }
}

fn parse_env<T>(name: &str) -> anyhow::Result<Option<T>>
where
    T: std::str::FromStr,
    T::Err: std::fmt::Display,
{
    match std::env::var(name) {
        Ok(value) if !value.trim().is_empty() => value
            .parse()
            .map(Some)
            .map_err(|error| anyhow::anyhow!("invalid {}: {}", name, error)),
        _ => Ok(None),
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
            webhook_endpoints: Vec::new(),
            webhook_queue_capacity: default_webhook_queue_capacity(),
            webhook_max_concurrency: default_webhook_max_concurrency(),
        }
    }
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use serde::Deserialize;

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

    /// Comma-separated webhook endpoint URLs. Env var: WEBHOOK_URLS
    /// (or CUBE_API_WEBHOOK_URLS).
    #[serde(default = "default_webhook_urls")]
    pub webhook_urls: String,

    /// Comma-separated event names delivered to every configured endpoint.
    /// Env var: WEBHOOK_EVENTS (or CUBE_API_WEBHOOK_EVENTS).
    #[serde(default = "default_webhook_events")]
    pub webhook_events: String,

    /// Optional HMAC-SHA256 secret for webhook signatures. Env var:
    /// WEBHOOK_SECRET (or CUBE_API_WEBHOOK_SECRET).
    #[serde(default = "default_webhook_secret")]
    pub webhook_secret: Option<String>,

    /// Maximum number of queued events per webhook worker. Env var:
    /// WEBHOOK_QUEUE_SIZE (or CUBE_API_WEBHOOK_QUEUE_SIZE).
    #[serde(default = "default_webhook_queue_size")]
    pub webhook_queue_size: usize,

    /// Number of retries after the initial webhook attempt. Env var:
    /// WEBHOOK_MAX_RETRIES (or CUBE_API_WEBHOOK_MAX_RETRIES).
    #[serde(default = "default_webhook_max_retries")]
    pub webhook_max_retries: u32,

    /// Base delay for exponential retry backoff, in milliseconds. Env var:
    /// WEBHOOK_RETRY_BASE_MS (or CUBE_API_WEBHOOK_RETRY_BASE_MS).
    #[serde(default = "default_webhook_retry_base_ms")]
    pub webhook_retry_base_ms: u64,

    /// Per-request webhook timeout, in seconds. Env var:
    /// WEBHOOK_REQUEST_TIMEOUT_SECS (or CUBE_API_WEBHOOK_REQUEST_TIMEOUT_SECS).
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

fn env_string(primary: &str, alias: &str, default: &str) -> String {
    std::env::var(primary)
        .or_else(|_| std::env::var(alias))
        .unwrap_or_else(|_| default.to_string())
}

fn default_webhook_urls() -> String {
    env_string("WEBHOOK_URLS", "CUBE_API_WEBHOOK_URLS", "")
}

fn default_webhook_events() -> String {
    env_string(
        "WEBHOOK_EVENTS",
        "CUBE_API_WEBHOOK_EVENTS",
        "sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed",
    )
}

fn default_webhook_secret() -> Option<String> {
    std::env::var("WEBHOOK_SECRET")
        .or_else(|_| std::env::var("CUBE_API_WEBHOOK_SECRET"))
        .ok()
        .filter(|value| !value.is_empty())
}

fn parse_env_usize(primary: &str, alias: &str, default: usize) -> usize {
    std::env::var(primary)
        .or_else(|_| std::env::var(alias))
        .ok()
        .and_then(|value| value.parse().ok())
        .unwrap_or(default)
}

fn parse_env_u32(primary: &str, alias: &str, default: u32) -> u32 {
    std::env::var(primary)
        .or_else(|_| std::env::var(alias))
        .ok()
        .and_then(|value| value.parse().ok())
        .unwrap_or(default)
}

fn parse_env_u64(primary: &str, alias: &str, default: u64) -> u64 {
    std::env::var(primary)
        .or_else(|_| std::env::var(alias))
        .ok()
        .and_then(|value| value.parse().ok())
        .unwrap_or(default)
}

fn default_webhook_queue_size() -> usize {
    parse_env_usize("WEBHOOK_QUEUE_SIZE", "CUBE_API_WEBHOOK_QUEUE_SIZE", 1024)
}

fn default_webhook_max_retries() -> u32 {
    parse_env_u32("WEBHOOK_MAX_RETRIES", "CUBE_API_WEBHOOK_MAX_RETRIES", 3)
}

fn default_webhook_retry_base_ms() -> u64 {
    parse_env_u64(
        "WEBHOOK_RETRY_BASE_MS",
        "CUBE_API_WEBHOOK_RETRY_BASE_MS",
        250,
    )
}

fn default_webhook_request_timeout_secs() -> u64 {
    parse_env_u64(
        "WEBHOOK_REQUEST_TIMEOUT_SECS",
        "CUBE_API_WEBHOOK_REQUEST_TIMEOUT_SECS",
        5,
    )
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
            webhook_urls: default_webhook_urls(),
            webhook_events: default_webhook_events(),
            webhook_secret: default_webhook_secret(),
            webhook_queue_size: default_webhook_queue_size(),
            webhook_max_retries: default_webhook_max_retries(),
            webhook_retry_base_ms: default_webhook_retry_base_ms(),
            webhook_request_timeout_secs: default_webhook_request_timeout_secs(),
        }
    }
}

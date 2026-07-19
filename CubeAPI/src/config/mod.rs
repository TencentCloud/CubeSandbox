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

    /// Env var: `DATABASE_URL`. When unset, built from `CUBE_SANDBOX_MYSQL_*`.
    /// Example: mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp
    #[serde(default = "default_database_url")]
    pub database_url: Option<String>,

    /// Web Terminal idle timeout in seconds: a terminal session that receives
    /// no user input for this long is closed (and its PTY killed). Values
    /// below 30 are clamped up to 30 at session start.
    ///
    /// Env var: `CUBE_API_TERMINAL_IDLE_TIMEOUT_SECS` (default 900 = 15 min)
    #[serde(default = "default_terminal_idle_timeout_secs")]
    pub terminal_idle_timeout_secs: u64,

    /// Maximum number of concurrent Web Terminal sessions across the whole
    /// server.
    ///
    /// Env var: `CUBE_API_TERMINAL_MAX_SESSIONS` (default 64)
    #[serde(default = "default_terminal_max_sessions")]
    pub terminal_max_sessions: usize,

    /// Time-to-live in seconds for a one-time Web Terminal ticket.
    ///
    /// Env var: `CUBE_API_TERMINAL_TICKET_TTL_SECS` (default 30)
    #[serde(default = "default_terminal_ticket_ttl_secs")]
    pub terminal_ticket_ttl_secs: u64,
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
fn default_terminal_idle_timeout_secs() -> u64 {
    std::env::var("CUBE_API_TERMINAL_IDLE_TIMEOUT_SECS")
        .ok()
        .and_then(|v| v.trim().parse().ok())
        .unwrap_or(900)
}
fn default_terminal_max_sessions() -> usize {
    std::env::var("CUBE_API_TERMINAL_MAX_SESSIONS")
        .ok()
        .and_then(|v| v.trim().parse().ok())
        .filter(|&n| n >= 1)
        .unwrap_or(64)
}
fn default_terminal_ticket_ttl_secs() -> u64 {
    std::env::var("CUBE_API_TERMINAL_TICKET_TTL_SECS")
        .ok()
        .and_then(|v| v.trim().parse().ok())
        .filter(|&n| n >= 1)
        .unwrap_or(30)
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
            database_url: default_database_url(),
            terminal_idle_timeout_secs: default_terminal_idle_timeout_secs(),
            terminal_max_sessions: default_terminal_max_sessions(),
            terminal_ticket_ttl_secs: default_terminal_ticket_ttl_secs(),
        }
    }
}

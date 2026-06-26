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

    /// Default template ID used by the Examples runner.
    /// Env var: CUBE_TEMPLATE_ID
    #[serde(default = "default_template_id")]
    pub default_template_id: Option<String>,

    /// CubeAPI URL used by the Examples runner (passed as CUBE_API_URL to scripts).
    /// Env var: CUBE_API_URL (default "http://127.0.0.1:3000")
    #[serde(default = "default_cube_api_url")]
    pub cube_api_url: Option<String>,

    /// CubeProxy node IP for bypassing DNS resolution (passed as CUBE_PROXY_NODE_IP).
    /// Env var: CUBE_PROXY_NODE_IP
    #[serde(default)]
    pub cube_proxy_node_ip: Option<String>,

    /// CubeProxy HTTP port (passed as CUBE_PROXY_PORT_HTTP).
    /// Env var: CUBE_PROXY_PORT_HTTP (no default; omitted when unset)
    #[serde(default = "default_cube_proxy_port_http")]
    pub cube_proxy_port_http: Option<u16>,

    /// Base URL of the sandbox proxy used to reach in-sandbox services
    /// (envd / Jupyter). Env var: AGENTHUB_SANDBOX_PROXY_URL (default
    /// "http://127.0.0.1").
    #[serde(default = "default_sandbox_proxy_url")]
    pub sandbox_proxy_url: String,

    /// `Authorization` header value used for internal service-to-service auth
    /// with the in-sandbox envd / Jupyter endpoints.
    ///
    /// **Security**: this is a credential and must never be hardcoded in
    /// business logic. It is sourced from the environment so deployments can
    /// rotate it without code changes. Env var: CUBE_API_ENVD_AUTH (default
    /// `Basic cm9vdDo=`, i.e. the envd built-in `root:` with an empty
    /// password — override it in any non-local environment).
    #[serde(default = "default_envd_auth")]
    pub envd_auth: String,

    /// Fallback CUBE_API_KEY injected into example subprocesses when the
    /// parent process does not export CUBE_API_KEY.
    ///
    /// Intended for sandbox/demo deployments to provide an out-of-the-box
    /// experience. In production, leave this unset and export CUBE_API_KEY
    /// directly. Env var: CUBE_API_DEFAULT_KEY
    #[serde(default = "default_api_key")]
    pub default_api_key: Option<String>,
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
fn default_template_id() -> Option<String> {
    std::env::var("CUBE_TEMPLATE_ID")
        .ok()
        .filter(|s| !s.is_empty())
}
fn default_cube_api_url() -> Option<String> {
    std::env::var("CUBE_API_URL")
        .ok()
        .filter(|s| !s.is_empty())
        .or_else(|| Some("http://127.0.0.1:3000".to_string()))
}

fn default_cube_proxy_port_http() -> Option<u16> {
    std::env::var("CUBE_PROXY_PORT_HTTP")
        .ok()
        .and_then(|s| s.parse().ok())
}
fn default_sandbox_proxy_url() -> String {
    std::env::var("AGENTHUB_SANDBOX_PROXY_URL").unwrap_or_else(|_| "http://127.0.0.1".to_string())
}
fn default_envd_auth() -> String {
    std::env::var("CUBE_API_ENVD_AUTH").unwrap_or_else(|_| "Basic cm9vdDo=".to_string())
}
fn default_api_key() -> Option<String> {
    std::env::var("CUBE_API_DEFAULT_KEY")
        .ok()
        .filter(|s| !s.is_empty())
        .or_else(|| Some("cube_0000000000000000000000000000000000000000".to_string()))
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
            database_url: default_database_url(),
            default_template_id: default_template_id(),
            cube_api_url: default_cube_api_url(),
            cube_proxy_node_ip: None,
            cube_proxy_port_http: default_cube_proxy_port_http(),
            sandbox_proxy_url: default_sandbox_proxy_url(),
            envd_auth: default_envd_auth(),
            default_api_key: default_api_key(),
        }
    }
}

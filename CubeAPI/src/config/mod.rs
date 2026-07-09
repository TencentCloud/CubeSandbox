// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use serde::{Deserialize, Deserializer, Serialize};

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

    /// Webhook endpoints that receive selected structured lifecycle events.
    ///
    /// Preferred env var: `CUBE_API_WEBHOOKS_JSON`, for example:
    /// `[{"url":"http://127.0.0.1:9000/webhook","events":["sandbox.created"],"secret":"..."}]`
    ///
    /// Simple env vars:
    ///   - `CUBE_API_WEBHOOK_URLS=http://127.0.0.1:9000/webhook,http://127.0.0.1:9001/webhook`
    ///   - `CUBE_API_WEBHOOK_EVENTS=sandbox.created,sandbox.deleted`
    ///   - `CUBE_API_WEBHOOK_SECRET=shared-secret`
    #[serde(default = "default_webhooks")]
    pub webhooks: Vec<WebhookEndpointConfig>,
}

#[derive(Debug, Deserialize, Serialize, Clone, PartialEq, Eq)]
pub struct WebhookEndpointConfig {
    /// Full URL that receives JSON POST requests.
    pub url: String,

    /// Event names this endpoint subscribes to. Use `*` to receive every event.
    #[serde(
        default = "default_webhook_events",
        deserialize_with = "deserialize_webhook_events"
    )]
    pub events: Vec<String>,

    /// Optional HMAC-SHA256 secret. When set, delivery includes
    /// `X-Cube-Signature-256: sha256=<hex>`.
    #[serde(default)]
    pub secret: Option<String>,

    /// Number of retries after the first failed attempt.
    #[serde(default = "default_webhook_max_retries")]
    pub max_retries: u32,

    /// Per-request timeout.
    #[serde(default = "default_webhook_timeout_secs")]
    pub timeout_secs: u64,

    /// Initial retry delay. Each retry doubles this delay.
    #[serde(default = "default_webhook_retry_initial_delay_ms")]
    pub retry_initial_delay_ms: u64,
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

fn default_webhooks() -> Vec<WebhookEndpointConfig> {
    if let Ok(raw) = std::env::var("CUBE_API_WEBHOOKS_JSON") {
        return match serde_json::from_str::<Vec<WebhookEndpointConfig>>(&raw) {
            Ok(endpoints) => normalize_webhook_endpoints(endpoints),
            Err(err) => {
                eprintln!("invalid CUBE_API_WEBHOOKS_JSON: {err}");
                Vec::new()
            }
        };
    }

    let urls = std::env::var("CUBE_API_WEBHOOK_URLS")
        .ok()
        .map(|value| parse_csv(&value))
        .unwrap_or_default();
    if urls.is_empty() {
        return Vec::new();
    }

    let events = std::env::var("CUBE_API_WEBHOOK_EVENTS")
        .ok()
        .map(|value| parse_csv(&value))
        .filter(|events| !events.is_empty())
        .unwrap_or_else(default_webhook_events);
    let secret = std::env::var("CUBE_API_WEBHOOK_SECRET")
        .ok()
        .filter(|value| !value.trim().is_empty());
    let max_retries = env_parse_or(
        "CUBE_API_WEBHOOK_MAX_RETRIES",
        default_webhook_max_retries(),
    );
    let timeout_secs = env_parse_or(
        "CUBE_API_WEBHOOK_TIMEOUT_SECS",
        default_webhook_timeout_secs(),
    );
    let retry_initial_delay_ms = env_parse_or(
        "CUBE_API_WEBHOOK_RETRY_INITIAL_DELAY_MS",
        default_webhook_retry_initial_delay_ms(),
    );

    urls.into_iter()
        .map(|url| WebhookEndpointConfig {
            url,
            events: events.clone(),
            secret: secret.clone(),
            max_retries,
            timeout_secs,
            retry_initial_delay_ms,
        })
        .collect()
}

fn normalize_webhook_endpoints(
    endpoints: Vec<WebhookEndpointConfig>,
) -> Vec<WebhookEndpointConfig> {
    endpoints
        .into_iter()
        .filter_map(|mut endpoint| {
            endpoint.url = endpoint.url.trim().to_string();
            endpoint.events = normalize_webhook_events(endpoint.events);
            if endpoint.url.is_empty() {
                None
            } else {
                Some(endpoint)
            }
        })
        .collect()
}

fn normalize_webhook_events(events: Vec<String>) -> Vec<String> {
    let events: Vec<String> = events
        .into_iter()
        .map(|event| event.trim().to_string())
        .filter(|event| !event.is_empty())
        .collect();
    if events.is_empty() {
        default_webhook_events()
    } else {
        events
    }
}

fn default_webhook_events() -> Vec<String> {
    vec![
        "sandbox.created".to_string(),
        "sandbox.deleted".to_string(),
        "sandbox.paused".to_string(),
        "sandbox.resumed".to_string(),
    ]
}

fn default_webhook_max_retries() -> u32 {
    3
}

fn default_webhook_timeout_secs() -> u64 {
    5
}

fn default_webhook_retry_initial_delay_ms() -> u64 {
    200
}

fn parse_csv(value: &str) -> Vec<String> {
    value
        .split(',')
        .map(|part| part.trim().to_string())
        .filter(|part| !part.is_empty())
        .collect()
}

fn env_parse_or<T>(name: &str, fallback: T) -> T
where
    T: std::str::FromStr,
{
    std::env::var(name)
        .ok()
        .and_then(|value| value.parse::<T>().ok())
        .unwrap_or(fallback)
}

fn deserialize_webhook_events<'de, D>(deserializer: D) -> Result<Vec<String>, D::Error>
where
    D: Deserializer<'de>,
{
    #[derive(Deserialize)]
    #[serde(untagged)]
    enum RawEvents {
        List(Vec<String>),
        Csv(String),
    }

    let events = match RawEvents::deserialize(deserializer)? {
        RawEvents::List(events) => events,
        RawEvents::Csv(events) => parse_csv(&events),
    };
    Ok(normalize_webhook_events(events))
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
            webhooks: default_webhooks(),
        }
    }
}

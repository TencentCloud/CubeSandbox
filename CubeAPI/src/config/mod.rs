// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::logging::http::{HttpLoggerConfig, WebhookEndpointConfig};
use anyhow::{bail, Context};
use serde::Deserialize;
use std::path::Path;

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

    /// Static webhook registration loaded at startup.
    #[serde(default)]
    pub webhook: WebhookConfig,

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
}

#[derive(Debug, Deserialize, Clone, PartialEq, Eq)]
pub struct WebhookConfig {
    /// Enable HTTP webhook delivery.
    #[serde(default)]
    pub enabled: bool,
    /// Registered webhook endpoints. Multiple endpoints are supported via config file.
    #[serde(default)]
    pub endpoints: Vec<WebhookEndpointConfig>,
    /// Max events per batch.
    #[serde(default = "default_webhook_batch_size")]
    pub batch_size: usize,
    /// Flush interval in seconds even if batch is not full.
    #[serde(default = "default_webhook_flush_interval_secs")]
    pub flush_interval_secs: u64,
    /// Number of retries after the initial delivery attempt.
    #[serde(default = "default_webhook_max_retries")]
    pub max_retries: usize,
    /// Delay between retry attempts in milliseconds.
    #[serde(default = "default_webhook_retry_backoff_millis")]
    pub retry_backoff_millis: u64,
    /// HTTP request timeout in seconds.
    #[serde(default = "default_webhook_request_timeout_secs")]
    pub request_timeout_secs: u64,
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct WebhookConfigPatch {
    enabled: Option<bool>,
    endpoint: Option<WebhookEndpointConfig>,
    batch_size: Option<usize>,
    flush_interval_secs: Option<u64>,
    max_retries: Option<usize>,
    retry_backoff_millis: Option<u64>,
    request_timeout_secs: Option<u64>,
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

fn default_webhook_batch_size() -> usize {
    100
}
fn default_webhook_flush_interval_secs() -> u64 {
    5
}
fn default_webhook_max_retries() -> usize {
    3
}
fn default_webhook_retry_backoff_millis() -> u64 {
    200
}
fn default_webhook_request_timeout_secs() -> u64 {
    5
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
        let cfg = ::config::Config::builder()
            .add_source(::config::Environment::default().separator("__"))
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
            webhook: WebhookConfig::default(),
            auth_callback_url: None,
            database_url: default_database_url(),
        }
    }
}

impl Default for WebhookConfig {
    fn default() -> Self {
        Self {
            enabled: false,
            endpoints: Vec::new(),
            batch_size: default_webhook_batch_size(),
            flush_interval_secs: default_webhook_flush_interval_secs(),
            max_retries: default_webhook_max_retries(),
            retry_backoff_millis: default_webhook_retry_backoff_millis(),
            request_timeout_secs: default_webhook_request_timeout_secs(),
        }
    }
}

impl WebhookConfig {
    pub fn from_file(path: impl AsRef<Path>) -> anyhow::Result<Self> {
        let path = path.as_ref();
        let mut cfg: Self = ::config::Config::builder()
            .add_source(::config::File::from(path))
            .build()
            .with_context(|| format!("failed to read webhook config file {}", path.display()))?
            .try_deserialize()
            .with_context(|| format!("failed to parse webhook config file {}", path.display()))?;
        cfg.normalize();
        cfg.validate()?;
        Ok(cfg)
    }

    #[cfg(test)]
    pub fn from_toml_str(input: &str) -> anyhow::Result<Self> {
        let mut cfg: Self = ::config::Config::builder()
            .add_source(::config::File::from_str(input, ::config::FileFormat::Toml))
            .build()?
            .try_deserialize()?;
        cfg.normalize();
        cfg.validate()?;
        Ok(cfg)
    }

    pub fn apply_patch(&mut self, patch: WebhookConfigPatch) {
        let endpoint_was_provided = patch.endpoint.is_some();
        let explicit_enabled = patch.enabled.is_some();

        if let Some(v) = patch.enabled {
            self.enabled = v;
        }
        if let Some(v) = patch.batch_size {
            self.batch_size = v;
        }
        if let Some(v) = patch.flush_interval_secs {
            self.flush_interval_secs = v;
        }
        if let Some(v) = patch.max_retries {
            self.max_retries = v;
        }
        if let Some(v) = patch.retry_backoff_millis {
            self.retry_backoff_millis = v;
        }
        if let Some(v) = patch.request_timeout_secs {
            self.request_timeout_secs = v;
        }
        if let Some(endpoint) = patch.endpoint {
            self.endpoints = vec![endpoint];
        }
        if endpoint_was_provided && !explicit_enabled {
            self.enabled = true;
        }
    }

    pub fn normalize(&mut self) {
        for endpoint in &mut self.endpoints {
            endpoint.url = endpoint.url.trim().to_string();
            endpoint.events = normalize_events(endpoint.events.iter().map(String::as_str));
            endpoint.hmac_secret = endpoint
                .hmac_secret
                .as_deref()
                .map(str::trim)
                .filter(|v| !v.is_empty())
                .map(ToOwned::to_owned);
        }
    }

    pub fn validate(&self) -> anyhow::Result<()> {
        for endpoint in &self.endpoints {
            if endpoint.url.is_empty() {
                bail!("webhook endpoint url must not be empty");
            }
        }
        Ok(())
    }
}

impl From<WebhookConfig> for HttpLoggerConfig {
    fn from(config: WebhookConfig) -> Self {
        Self {
            endpoints: config.endpoints,
            batch_size: config.batch_size,
            flush_interval_secs: config.flush_interval_secs,
            max_retries: config.max_retries,
            retry_backoff_millis: config.retry_backoff_millis,
            request_timeout_secs: config.request_timeout_secs,
        }
    }
}

impl WebhookConfigPatch {
    pub fn from_env() -> anyhow::Result<Self> {
        Self::from_lookup(|key| std::env::var(key).ok())
    }

    pub fn from_lookup<F>(lookup: F) -> anyhow::Result<Self>
    where
        F: Fn(&str) -> Option<String>,
    {
        let url = lookup("CUBE_API_WEBHOOK_URL")
            .map(|v| v.trim().to_string())
            .filter(|v| !v.is_empty());
        let events = lookup("CUBE_API_WEBHOOK_EVENTS")
            .map(|v| normalize_events(v.split(',')))
            .unwrap_or_default();
        let hmac_secret = lookup("CUBE_API_WEBHOOK_SECRET")
            .map(|v| v.trim().to_string())
            .filter(|v| !v.is_empty());

        Ok(Self {
            enabled: parse_bool_var(
                "CUBE_API_WEBHOOK_ENABLED",
                lookup("CUBE_API_WEBHOOK_ENABLED"),
            )?,
            endpoint: url.map(|url| WebhookEndpointConfig {
                url,
                events,
                hmac_secret,
            }),
            batch_size: parse_usize_var(
                "CUBE_API_WEBHOOK_BATCH_SIZE",
                lookup("CUBE_API_WEBHOOK_BATCH_SIZE"),
            )?,
            flush_interval_secs: parse_u64_var(
                "CUBE_API_WEBHOOK_FLUSH_INTERVAL_SECS",
                lookup("CUBE_API_WEBHOOK_FLUSH_INTERVAL_SECS"),
            )?,
            max_retries: parse_usize_var(
                "CUBE_API_WEBHOOK_MAX_RETRIES",
                lookup("CUBE_API_WEBHOOK_MAX_RETRIES"),
            )?,
            retry_backoff_millis: parse_u64_var(
                "CUBE_API_WEBHOOK_RETRY_BACKOFF_MILLIS",
                lookup("CUBE_API_WEBHOOK_RETRY_BACKOFF_MILLIS"),
            )?,
            request_timeout_secs: parse_u64_var(
                "CUBE_API_WEBHOOK_REQUEST_TIMEOUT_SECS",
                lookup("CUBE_API_WEBHOOK_REQUEST_TIMEOUT_SECS"),
            )?,
        })
    }

    pub fn from_single_endpoint(
        url: Option<String>,
        events: Option<String>,
        hmac_secret: Option<String>,
    ) -> Self {
        let endpoint = url
            .map(|v| v.trim().to_string())
            .filter(|v| !v.is_empty())
            .map(|url| WebhookEndpointConfig {
                url,
                events: events
                    .as_deref()
                    .map(|v| normalize_events(v.split(',')))
                    .unwrap_or_default(),
                hmac_secret: hmac_secret
                    .as_deref()
                    .map(str::trim)
                    .filter(|v| !v.is_empty())
                    .map(ToOwned::to_owned),
            });

        Self {
            endpoint,
            ..Self::default()
        }
    }

    pub fn from_explicit_single_endpoint(
        url: Option<String>,
        events: Option<String>,
        hmac_secret: Option<String>,
    ) -> anyhow::Result<Self> {
        if matches!(url.as_deref().map(str::trim), Some("")) {
            bail!("webhook endpoint url must not be empty");
        }
        Ok(Self::from_single_endpoint(url, events, hmac_secret))
    }
}

fn normalize_events<'a>(events: impl IntoIterator<Item = &'a str>) -> Vec<String> {
    events
        .into_iter()
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .map(ToOwned::to_owned)
        .collect()
}

fn parse_bool_var(name: &str, value: Option<String>) -> anyhow::Result<Option<bool>> {
    let Some(value) = value
        .map(|v| v.trim().to_ascii_lowercase())
        .filter(|v| !v.is_empty())
    else {
        return Ok(None);
    };

    match value.as_str() {
        "1" | "true" | "yes" | "on" => Ok(Some(true)),
        "0" | "false" | "no" | "off" => Ok(Some(false)),
        _ => bail!("invalid boolean value for {}: {}", name, value),
    }
}

fn parse_usize_var(name: &str, value: Option<String>) -> anyhow::Result<Option<usize>> {
    let Some(value) = value
        .map(|v| v.trim().to_string())
        .filter(|v| !v.is_empty())
    else {
        return Ok(None);
    };
    value
        .parse::<usize>()
        .map(Some)
        .with_context(|| format!("invalid integer value for {}: {}", name, value))
}

fn parse_u64_var(name: &str, value: Option<String>) -> anyhow::Result<Option<u64>> {
    let Some(value) = value
        .map(|v| v.trim().to_string())
        .filter(|v| !v.is_empty())
    else {
        return Ok(None);
    };
    value
        .parse::<u64>()
        .map(Some)
        .with_context(|| format!("invalid integer value for {}: {}", name, value))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    #[test]
    fn webhook_default_is_disabled() {
        let cfg = WebhookConfig::default();
        assert!(!cfg.enabled);
        assert!(cfg.endpoints.is_empty());
        assert_eq!(cfg.batch_size, 100);
        assert_eq!(cfg.flush_interval_secs, 5);
        assert_eq!(cfg.max_retries, 3);
        assert_eq!(cfg.retry_backoff_millis, 200);
        assert_eq!(cfg.request_timeout_secs, 5);
        assert!(ServerConfig::default().webhook.endpoints.is_empty());
    }

    #[test]
    fn webhook_config_file_supports_multiple_endpoints() {
        let cfg = WebhookConfig::from_toml_str(
            r#"
            enabled = true
            batch_size = 20

            [[endpoints]]
            url = "http://127.0.0.1:8080/webhook"
            events = ["sandbox.created", " sandbox.deleted ", ""]
            hmac_secret = " secret-a "

            [[endpoints]]
            url = "http://127.0.0.1:8081/webhook"
            events = ["sandbox.*"]
            "#,
        )
        .expect("parse webhook config");

        assert!(cfg.enabled);
        assert_eq!(cfg.batch_size, 20);
        assert_eq!(cfg.flush_interval_secs, 5);
        assert_eq!(cfg.endpoints.len(), 2);
        assert_eq!(
            cfg.endpoints[0].events,
            vec!["sandbox.created", "sandbox.deleted"]
        );
        assert_eq!(cfg.endpoints[0].hmac_secret.as_deref(), Some("secret-a"));
        assert_eq!(cfg.endpoints[1].events, vec!["sandbox.*"]);
    }

    #[test]
    fn webhook_config_rejects_blank_endpoint_url() {
        let err = WebhookConfig::from_toml_str(
            r#"
            enabled = true

            [[endpoints]]
            url = "   "
            "#,
        )
        .expect_err("blank endpoint url must fail");

        assert!(err.to_string().contains("webhook endpoint url"));
    }

    #[test]
    fn webhook_config_file_missing_path_fails() {
        let path = unique_temp_path("missing-webhook.toml");
        let _ = std::fs::remove_file(&path);

        let err = WebhookConfig::from_file(&path).expect_err("missing file must fail");
        assert!(err
            .to_string()
            .contains("failed to read webhook config file"));
    }

    #[test]
    fn webhook_config_file_malformed_toml_fails() {
        let path = unique_temp_path("malformed-webhook.toml");
        std::fs::write(&path, "enabled = true\n[[endpoints]\nurl = ")
            .expect("write malformed config");

        let err = WebhookConfig::from_file(&path).expect_err("malformed file must fail");
        let _ = std::fs::remove_file(&path);
        assert!(err
            .to_string()
            .contains("failed to read webhook config file"));
    }

    #[test]
    fn webhook_config_converts_to_http_logger_config() {
        let cfg = WebhookConfig {
            enabled: true,
            endpoints: vec![WebhookEndpointConfig {
                url: "http://receiver/webhook".to_string(),
                events: vec!["sandbox.*".to_string()],
                hmac_secret: Some("secret".to_string()),
            }],
            batch_size: 7,
            flush_interval_secs: 8,
            max_retries: 9,
            retry_backoff_millis: 10,
            request_timeout_secs: 11,
        };

        let http: HttpLoggerConfig = cfg.clone().into();
        assert_eq!(http.endpoints, cfg.endpoints);
        assert_eq!(http.batch_size, 7);
        assert_eq!(http.flush_interval_secs, 8);
        assert_eq!(http.max_retries, 9);
        assert_eq!(http.retry_backoff_millis, 10);
        assert_eq!(http.request_timeout_secs, 11);
    }

    #[test]
    fn webhook_env_url_auto_enables_single_endpoint() {
        let patch = WebhookConfigPatch::from_lookup(map_lookup(&[
            ("CUBE_API_WEBHOOK_URL", "http://receiver/webhook"),
            (
                "CUBE_API_WEBHOOK_EVENTS",
                "sandbox.created, sandbox.deleted,,",
            ),
            ("CUBE_API_WEBHOOK_SECRET", " secret "),
        ]))
        .expect("parse env patch");
        let mut cfg = WebhookConfig::default();
        cfg.apply_patch(patch);

        assert!(cfg.enabled);
        assert_eq!(cfg.endpoints.len(), 1);
        assert_eq!(cfg.endpoints[0].url, "http://receiver/webhook");
        assert_eq!(
            cfg.endpoints[0].events,
            vec!["sandbox.created", "sandbox.deleted"]
        );
        assert_eq!(cfg.endpoints[0].hmac_secret.as_deref(), Some("secret"));
    }

    #[test]
    fn webhook_env_explicit_false_overrides_url_auto_enable() {
        let patch = WebhookConfigPatch::from_lookup(map_lookup(&[
            ("CUBE_API_WEBHOOK_ENABLED", "false"),
            ("CUBE_API_WEBHOOK_URL", "http://receiver/webhook"),
        ]))
        .expect("parse env patch");
        let mut cfg = WebhookConfig::default();
        cfg.apply_patch(patch);

        assert!(!cfg.enabled);
        assert_eq!(cfg.endpoints.len(), 1);
    }

    #[test]
    fn webhook_env_tuning_fields_override_config() {
        let patch = WebhookConfigPatch::from_lookup(map_lookup(&[
            ("CUBE_API_WEBHOOK_ENABLED", "yes"),
            ("CUBE_API_WEBHOOK_BATCH_SIZE", "12"),
            ("CUBE_API_WEBHOOK_FLUSH_INTERVAL_SECS", "13"),
            ("CUBE_API_WEBHOOK_MAX_RETRIES", "14"),
            ("CUBE_API_WEBHOOK_RETRY_BACKOFF_MILLIS", "15"),
            ("CUBE_API_WEBHOOK_REQUEST_TIMEOUT_SECS", "16"),
        ]))
        .expect("parse env patch");
        let mut cfg = WebhookConfig::default();
        cfg.apply_patch(patch);

        assert!(cfg.enabled);
        assert_eq!(cfg.batch_size, 12);
        assert_eq!(cfg.flush_interval_secs, 13);
        assert_eq!(cfg.max_retries, 14);
        assert_eq!(cfg.retry_backoff_millis, 15);
        assert_eq!(cfg.request_timeout_secs, 16);
    }

    #[test]
    fn webhook_env_invalid_integer_fails() {
        let err = WebhookConfigPatch::from_lookup(map_lookup(&[(
            "CUBE_API_WEBHOOK_BATCH_SIZE",
            "not-a-number",
        )]))
        .expect_err("invalid integer must fail");

        assert!(err.to_string().contains("CUBE_API_WEBHOOK_BATCH_SIZE"));
    }

    fn unique_temp_path(name: &str) -> std::path::PathBuf {
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .expect("system time before epoch")
            .as_nanos();
        std::env::temp_dir().join(format!(
            "cube-api-webhook-{}-{}-{}",
            std::process::id(),
            nanos,
            name
        ))
    }

    fn map_lookup(entries: &[(&str, &str)]) -> impl Fn(&str) -> Option<String> {
        let map: HashMap<String, String> = entries
            .iter()
            .map(|(k, v)| ((*k).to_string(), (*v).to_string()))
            .collect();
        move |key| map.get(key).cloned()
    }
}

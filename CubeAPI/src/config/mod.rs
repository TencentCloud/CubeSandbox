// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::{collections::BTreeMap, env, str::FromStr};

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

    /// Webhook endpoints for structured lifecycle events.
    #[serde(default)]
    pub webhook: WebhookConfig,
}

#[derive(Debug, Deserialize, Clone)]
pub struct WebhookConfig {
    #[serde(default)]
    pub enabled: bool,
    #[serde(default = "default_webhook_queue_size")]
    pub queue_size: usize,
    #[serde(default = "default_webhook_delivery_concurrency")]
    pub delivery_concurrency: usize,
    #[serde(default = "default_webhook_timeout_ms")]
    pub default_timeout_ms: u64,
    #[serde(default = "default_webhook_max_retries")]
    pub default_max_retries: usize,
    #[serde(default = "default_webhook_initial_backoff_ms")]
    pub default_initial_backoff_ms: u64,
    #[serde(default = "default_webhook_max_backoff_ms")]
    pub default_max_backoff_ms: u64,
    #[serde(default)]
    pub endpoints: Vec<WebhookEndpointConfig>,
}

#[derive(Debug, Deserialize, Clone, Default)]
pub struct WebhookEndpointConfig {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub url: String,
    #[serde(default)]
    pub events: Vec<String>,
    #[serde(default)]
    pub secret: Option<String>,
    #[serde(default)]
    pub timeout_ms: Option<u64>,
    #[serde(default)]
    pub max_retries: Option<usize>,
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

fn default_webhook_queue_size() -> usize {
    1024
}
fn default_webhook_delivery_concurrency() -> usize {
    64
}
fn default_webhook_timeout_ms() -> u64 {
    3000
}
fn default_webhook_max_retries() -> usize {
    3
}
fn default_webhook_initial_backoff_ms() -> u64 {
    200
}
fn default_webhook_max_backoff_ms() -> u64 {
    2000
}

impl ServerConfig {
    pub fn from_env() -> anyhow::Result<Self> {
        let _ = dotenvy::dotenv();

        // `config::Environment` deserializes `ENDPOINTS__0` as a map, not a
        // Vec. Parse the documented indexed webhook variables explicitly so
        // startup cannot silently discard a valid webhook configuration.
        let mut cfg = Self::default();
        override_string("LOG_LEVEL", &mut cfg.log_level);
        override_parsed("WORKER_THREADS", &mut cfg.worker_threads)?;
        override_parsed("RATE_LIMIT_PER_SEC", &mut cfg.rate_limit_per_sec)?;
        override_string("INSTANCE_TYPE", &mut cfg.instance_type);
        override_string("LOG_DIR", &mut cfg.log_dir);
        override_string("LOG_PREFIX", &mut cfg.log_prefix);
        cfg.auth_callback_url = optional_env("AUTH_CALLBACK_URL");
        cfg.database_url = optional_env("DATABASE_URL").or_else(default_cube_sandbox_mysql_url);
        cfg.webhook = webhook_from_env()?;
        Ok(cfg)
    }
}

fn optional_env(key: &str) -> Option<String> {
    env::var(key)
        .ok()
        .map(|value| value.trim().to_string())
        .filter(|value| !value.is_empty())
}

fn override_string(key: &str, target: &mut String) {
    if let Some(value) = optional_env(key) {
        *target = value;
    }
}

fn override_parsed<T>(key: &str, target: &mut T) -> anyhow::Result<()>
where
    T: FromStr,
    T::Err: std::fmt::Display,
{
    let Some(value) = optional_env(key) else {
        return Ok(());
    };
    *target = value
        .parse()
        .map_err(|err| anyhow::anyhow!("invalid {key}={value:?}: {err}"))?;
    Ok(())
}

fn webhook_from_env() -> anyhow::Result<WebhookConfig> {
    let mut webhook = WebhookConfig::default();
    if let Some(value) = optional_env("WEBHOOK__ENABLED") {
        webhook.enabled = value
            .parse()
            .map_err(|err| anyhow::anyhow!("invalid WEBHOOK__ENABLED={value:?}: {err}"))?;
    }
    override_parsed("WEBHOOK__QUEUE_SIZE", &mut webhook.queue_size)?;
    override_parsed(
        "WEBHOOK__DELIVERY_CONCURRENCY",
        &mut webhook.delivery_concurrency,
    )?;
    override_parsed(
        "WEBHOOK__DEFAULT_TIMEOUT_MS",
        &mut webhook.default_timeout_ms,
    )?;
    override_parsed(
        "WEBHOOK__DEFAULT_MAX_RETRIES",
        &mut webhook.default_max_retries,
    )?;
    override_parsed(
        "WEBHOOK__DEFAULT_INITIAL_BACKOFF_MS",
        &mut webhook.default_initial_backoff_ms,
    )?;
    override_parsed(
        "WEBHOOK__DEFAULT_MAX_BACKOFF_MS",
        &mut webhook.default_max_backoff_ms,
    )?;

    webhook.endpoints = parse_indexed_webhook_endpoints(env::vars())?;
    Ok(webhook)
}

fn parse_indexed_webhook_endpoints(
    vars: impl IntoIterator<Item = (String, String)>,
) -> anyhow::Result<Vec<WebhookEndpointConfig>> {
    let mut endpoints: BTreeMap<usize, BTreeMap<String, String>> = BTreeMap::new();
    let mut events: BTreeMap<(usize, usize), String> = BTreeMap::new();
    for (key, value) in vars {
        let Some(rest) = key.strip_prefix("WEBHOOK__ENDPOINTS__") else {
            continue;
        };
        let mut parts = rest.split("__");
        let index = parts
            .next()
            .ok_or_else(|| anyhow::anyhow!("invalid webhook endpoint variable {key}"))?
            .parse::<usize>()
            .map_err(|err| anyhow::anyhow!("invalid webhook endpoint index in {key}: {err}"))?;
        let field = parts
            .next()
            .ok_or_else(|| anyhow::anyhow!("invalid webhook endpoint variable {key}"))?;

        if field == "EVENTS" {
            let event_index = parts
                .next()
                .ok_or_else(|| anyhow::anyhow!("invalid webhook event variable {key}"))?
                .parse::<usize>()
                .map_err(|err| anyhow::anyhow!("invalid webhook event index in {key}: {err}"))?;
            if parts.next().is_some() {
                anyhow::bail!("invalid webhook event variable {key}");
            }
            events.insert((index, event_index), value);
        } else {
            if parts.next().is_some() {
                anyhow::bail!("invalid webhook endpoint variable {key}");
            }
            endpoints
                .entry(index)
                .or_default()
                .insert(field.to_string(), value);
        }
    }

    let mut result = Vec::new();
    for (index, _) in &events {
        anyhow::ensure!(
            endpoints.contains_key(&index.0),
            "WEBHOOK__ENDPOINTS__{} has events but no endpoint configuration",
            index.0
        );
    }
    for (index, fields) in endpoints {
        let endpoint_events = events
            .iter()
            .filter(|((endpoint_index, _), _)| *endpoint_index == index)
            .map(|((_, event_index), value)| (*event_index, value.trim().to_string()))
            .collect::<BTreeMap<_, _>>();
        let events = endpoint_events
            .into_iter()
            .enumerate()
            .map(|(expected, (actual, value))| {
                anyhow::ensure!(
                    actual == expected,
                    "missing WEBHOOK__ENDPOINTS__{index}__EVENTS__{expected}"
                );
                Ok(value)
            })
            .collect::<anyhow::Result<Vec<_>>>()?;
        result.push(WebhookEndpointConfig {
            name: fields.get("NAME").map_or_else(String::new, Clone::clone),
            url: fields.get("URL").map_or_else(String::new, Clone::clone),
            events,
            secret: fields
                .get("SECRET")
                .map(|value| value.trim().to_string())
                .filter(|value| !value.is_empty()),
            timeout_ms: fields
                .get("TIMEOUT_MS")
                .map(|value| value.parse())
                .transpose()
                .map_err(|err| {
                    anyhow::anyhow!("invalid WEBHOOK__ENDPOINTS__{index}__TIMEOUT_MS: {err}")
                })?,
            max_retries: fields
                .get("MAX_RETRIES")
                .map(|value| value.parse())
                .transpose()
                .map_err(|err| {
                    anyhow::anyhow!("invalid WEBHOOK__ENDPOINTS__{index}__MAX_RETRIES: {err}")
                })?,
        });
    }
    Ok(result)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_indexed_webhook_endpoints_in_order() {
        let endpoints = parse_indexed_webhook_endpoints(vec![
            ("WEBHOOK__ENDPOINTS__1__NAME".into(), "secondary".into()),
            ("WEBHOOK__ENDPOINTS__0__NAME".into(), "primary".into()),
            (
                "WEBHOOK__ENDPOINTS__0__URL".into(),
                "http://one.example/webhook".into(),
            ),
            (
                "WEBHOOK__ENDPOINTS__1__URL".into(),
                "http://two.example/webhook".into(),
            ),
            (
                "WEBHOOK__ENDPOINTS__0__EVENTS__1".into(),
                "sandbox.deleted".into(),
            ),
            (
                "WEBHOOK__ENDPOINTS__0__EVENTS__0".into(),
                "sandbox.created".into(),
            ),
            (
                "WEBHOOK__ENDPOINTS__1__EVENTS__0".into(),
                "sandbox.paused".into(),
            ),
            ("WEBHOOK__ENDPOINTS__1__SECRET".into(), "secret-two".into()),
        ])
        .expect("indexed webhook endpoints should parse");

        assert_eq!(endpoints.len(), 2);
        assert_eq!(endpoints[0].name, "primary");
        assert_eq!(
            endpoints[0].events,
            vec!["sandbox.created", "sandbox.deleted"]
        );
        assert_eq!(endpoints[1].name, "secondary");
        assert_eq!(endpoints[1].events, vec!["sandbox.paused"]);
        assert_eq!(endpoints[1].secret.as_deref(), Some("secret-two"));
    }

    #[test]
    fn parses_sparse_indexed_webhook_endpoints() {
        let endpoints = parse_indexed_webhook_endpoints(vec![
            ("WEBHOOK__ENDPOINTS__2__NAME".into(), "secondary".into()),
            (
                "WEBHOOK__ENDPOINTS__2__URL".into(),
                "http://two.example/webhook".into(),
            ),
            (
                "WEBHOOK__ENDPOINTS__2__EVENTS__0".into(),
                "sandbox.paused".into(),
            ),
        ])
        .expect("sparse endpoint indexes should parse");

        assert_eq!(endpoints.len(), 1);
        assert_eq!(endpoints[0].name, "secondary");
        assert_eq!(endpoints[0].events, vec!["sandbox.paused"]);
    }
}

impl Default for WebhookConfig {
    fn default() -> Self {
        Self {
            enabled: false,
            queue_size: default_webhook_queue_size(),
            delivery_concurrency: default_webhook_delivery_concurrency(),
            default_timeout_ms: default_webhook_timeout_ms(),
            default_max_retries: default_webhook_max_retries(),
            default_initial_backoff_ms: default_webhook_initial_backoff_ms(),
            default_max_backoff_ms: default_webhook_max_backoff_ms(),
            endpoints: Vec::new(),
        }
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
            webhook: WebhookConfig::default(),
        }
    }
}

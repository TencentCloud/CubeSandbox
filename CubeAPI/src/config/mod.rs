// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use serde::Deserialize;
use std::{
    collections::HashSet,
    fmt,
    net::ToSocketAddrs,
    net::{IpAddr, Ipv4Addr, Ipv6Addr},
};

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

    /// Webhook endpoint subscriptions for sandbox lifecycle events.
    ///
    /// Env var: `CUBE_API_WEBHOOKS` as a JSON array, for example:
    /// `[{"url":"http://127.0.0.1:9000/webhook","events":["sandbox.created"],"secret":"..."}]`
    ///
    /// For simple local setups, `CUBE_API_WEBHOOK_URLS` may be a comma-separated
    /// list and uses `CUBE_API_WEBHOOK_EVENTS` plus `CUBE_API_WEBHOOK_SECRET`.
    #[serde(default = "default_webhooks")]
    pub webhooks: Vec<WebhookEndpointConfig>,

    /// Buffered event capacity before webhook events are dropped.
    #[serde(default = "default_webhook_queue_capacity")]
    pub webhook_queue_capacity: usize,

    /// Per-request timeout for webhook deliveries.
    #[serde(default = "default_webhook_request_timeout_secs")]
    pub webhook_request_timeout_secs: u64,

    /// Maximum delivery attempts per endpoint.
    #[serde(default = "default_webhook_max_attempts")]
    pub webhook_max_attempts: usize,

    /// First retry delay; later retries use exponential backoff.
    #[serde(default = "default_webhook_initial_backoff_millis")]
    pub webhook_initial_backoff_millis: u64,
}

#[derive(Deserialize, Clone, PartialEq, Eq)]
pub struct WebhookEndpointConfig {
    pub url: String,

    /// Subscribed event names. Empty or `["*"]` subscribes to all events.
    #[serde(default)]
    pub events: Vec<String>,

    /// Optional HMAC-SHA256 secret.
    #[serde(default)]
    pub secret: Option<String>,
}

impl fmt::Debug for WebhookEndpointConfig {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("WebhookEndpointConfig")
            .field("url", &self.url)
            .field("events", &self.events)
            .field("secret", &self.secret.as_ref().map(|_| "***"))
            .finish()
    }
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
    if let Ok(raw) = std::env::var("CUBE_API_WEBHOOKS") {
        match serde_json::from_str::<Vec<WebhookEndpointConfig>>(&raw) {
            Ok(endpoints) => return clean_webhook_endpoints(endpoints),
            Err(err) => {
                eprintln!("ignoring invalid CUBE_API_WEBHOOKS: {err}");
                tracing::error!(error = %err, "ignoring invalid CUBE_API_WEBHOOKS");
            }
        }
    }

    let urls = match std::env::var("CUBE_API_WEBHOOK_URLS") {
        Ok(value) => value,
        Err(_) => return Vec::new(),
    };
    let events = std::env::var("CUBE_API_WEBHOOK_EVENTS")
        .ok()
        .map(|value| split_csv(&value))
        .unwrap_or_else(|| {
            vec![
                "sandbox.created".to_string(),
                "sandbox.deleted".to_string(),
                "sandbox.paused".to_string(),
                "sandbox.resumed".to_string(),
            ]
        });
    let secret = std::env::var("CUBE_API_WEBHOOK_SECRET")
        .ok()
        .filter(|value| !value.trim().is_empty());

    clean_webhook_endpoints(
        split_csv(&urls)
            .into_iter()
            .map(|url| WebhookEndpointConfig {
                url,
                events: events.clone(),
                secret: secret.clone(),
            })
            .collect(),
    )
}

fn default_webhook_queue_capacity() -> usize {
    env_parse("CUBE_API_WEBHOOK_QUEUE_CAPACITY", 1024).max(1)
}

fn default_webhook_request_timeout_secs() -> u64 {
    env_parse("CUBE_API_WEBHOOK_REQUEST_TIMEOUT_SECS", 5)
}

fn default_webhook_max_attempts() -> usize {
    env_parse("CUBE_API_WEBHOOK_MAX_ATTEMPTS", 3).max(1)
}

fn default_webhook_initial_backoff_millis() -> u64 {
    env_parse("CUBE_API_WEBHOOK_INITIAL_BACKOFF_MILLIS", 200)
}

fn env_parse<T>(key: &str, default: T) -> T
where
    T: std::str::FromStr,
{
    std::env::var(key)
        .ok()
        .and_then(|value| match value.parse::<T>() {
            Ok(parsed) => Some(parsed),
            Err(_) => {
                eprintln!("ignoring invalid {key}={value:?}; using default");
                tracing::warn!(key, value, "invalid environment value, using default");
                None
            }
        })
        .unwrap_or(default)
}

fn split_csv(value: &str) -> Vec<String> {
    value
        .split(',')
        .map(str::trim)
        .filter(|item| !item.is_empty())
        .map(ToOwned::to_owned)
        .collect()
}

fn clean_webhook_endpoints(endpoints: Vec<WebhookEndpointConfig>) -> Vec<WebhookEndpointConfig> {
    let mut seen_urls = HashSet::new();

    endpoints
        .into_iter()
        .filter(|endpoint| !endpoint.url.trim().is_empty())
        .filter_map(|mut endpoint| {
            endpoint.url = match validate_webhook_url(endpoint.url.trim(), endpoint.secret.as_ref())
            {
                Ok(url) => url,
                Err(err) => {
                    eprintln!("ignoring invalid webhook endpoint: {err}");
                    tracing::warn!(error = %err, "ignoring invalid webhook endpoint");
                    return None;
                }
            };
            if !seen_urls.insert(endpoint.url.clone()) {
                eprintln!("ignoring duplicate webhook endpoint URL: {}", endpoint.url);
                tracing::warn!(url = %endpoint.url, "ignoring duplicate webhook endpoint URL");
                return None;
            }
            let had_explicit_events = !endpoint.events.is_empty();
            endpoint.events = endpoint
                .events
                .into_iter()
                .map(|event| event.trim().to_string())
                .filter(|event| !event.is_empty())
                .collect();
            if had_explicit_events && endpoint.events.is_empty() {
                eprintln!(
                    "webhook endpoint {} has no valid subscribed events after cleanup; subscribing to all events",
                    endpoint.url
                );
                tracing::warn!(
                    url = %endpoint.url,
                    "webhook endpoint has no valid subscribed events after cleanup; subscribing to all events"
                );
            }
            Some(endpoint)
        })
        .collect()
}

fn validate_webhook_url(raw_url: &str, secret: Option<&String>) -> anyhow::Result<String> {
    let url = reqwest::Url::parse(raw_url)?;
    match url.scheme() {
        "http" | "https" => {}
        scheme => anyhow::bail!("unsupported webhook URL scheme {scheme:?}: {raw_url}"),
    }
    if !url.username().is_empty() || url.password().is_some() {
        anyhow::bail!("webhook URL must not include credentials: {raw_url}");
    }
    let host = url
        .host_str()
        .ok_or_else(|| anyhow::anyhow!("webhook URL must include a host: {raw_url}"))?;
    if let Ok(ip) = host.parse::<IpAddr>() {
        validate_webhook_ip_literal(ip, raw_url)?;
    } else {
        validate_webhook_hostname(host, url.port_or_known_default(), raw_url)?;
    }
    if url.scheme() == "http" && secret.is_some() {
        eprintln!("webhook endpoint with HMAC secret is using HTTP, not HTTPS: {raw_url}");
        tracing::warn!(
            url = raw_url,
            "webhook endpoint with HMAC secret is using HTTP"
        );
    }

    Ok(url.to_string())
}

fn validate_webhook_ip_literal(ip: IpAddr, raw_url: &str) -> anyhow::Result<()> {
    let allowed = match ip {
        IpAddr::V4(ip) => is_allowed_webhook_ipv4(ip),
        IpAddr::V6(ip) => ip
            .to_ipv4_mapped()
            .map(is_allowed_webhook_ipv4)
            .unwrap_or_else(|| is_allowed_webhook_ipv6(ip)),
    };
    if allowed {
        return Ok(());
    }
    anyhow::bail!("webhook URL uses a disallowed IP literal: {raw_url}");
}

fn validate_webhook_hostname(host: &str, port: Option<u16>, raw_url: &str) -> anyhow::Result<()> {
    let port = port.ok_or_else(|| anyhow::anyhow!("webhook URL must include a port: {raw_url}"))?;
    let mut resolved = 0usize;
    for addr in (host, port).to_socket_addrs()? {
        validate_webhook_ip_literal(addr.ip(), raw_url)?;
        resolved += 1;
    }
    if resolved == 0 {
        anyhow::bail!("webhook URL host did not resolve to any address: {raw_url}");
    }
    Ok(())
}

fn is_allowed_webhook_ipv4(ip: Ipv4Addr) -> bool {
    ip.is_loopback()
        || !(ip.is_private()
            || ip.is_link_local()
            || ip.is_multicast()
            || ip.is_broadcast()
            || ip.is_unspecified())
}

fn is_allowed_webhook_ipv6(ip: Ipv6Addr) -> bool {
    ip.is_loopback()
        || !(ip.is_unique_local()
            || ip.is_unicast_link_local()
            || ip.is_multicast()
            || ip.is_unspecified())
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
            webhook_queue_capacity: default_webhook_queue_capacity(),
            webhook_request_timeout_secs: default_webhook_request_timeout_secs(),
            webhook_max_attempts: default_webhook_max_attempts(),
            webhook_initial_backoff_millis: default_webhook_initial_backoff_millis(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::{
        clean_webhook_endpoints, default_webhook_queue_capacity, split_csv, WebhookEndpointConfig,
    };

    #[test]
    fn webhook_endpoint_cleanup_trims_empty_values() {
        let endpoints = clean_webhook_endpoints(vec![
            WebhookEndpointConfig {
                url: "  http://localhost:9000/webhook  ".to_string(),
                events: vec![" sandbox.created ".to_string(), "".to_string()],
                secret: None,
            },
            WebhookEndpointConfig {
                url: " ".to_string(),
                events: vec![],
                secret: None,
            },
        ]);

        assert_eq!(endpoints.len(), 1);
        assert_eq!(endpoints[0].url, "http://localhost:9000/webhook");
        assert_eq!(endpoints[0].events, vec!["sandbox.created"]);
    }

    #[test]
    fn webhook_endpoint_cleanup_rejects_invalid_url_schemes() {
        let endpoints = clean_webhook_endpoints(vec![
            WebhookEndpointConfig {
                url: "file:///tmp/webhook".to_string(),
                events: vec![],
                secret: None,
            },
            WebhookEndpointConfig {
                url: "http://127.0.0.1:9000/webhook".to_string(),
                events: vec![],
                secret: None,
            },
        ]);

        assert_eq!(endpoints.len(), 1);
        assert_eq!(endpoints[0].url, "http://127.0.0.1:9000/webhook");
    }

    #[test]
    fn webhook_endpoint_cleanup_rejects_private_ip_literals() {
        let endpoints = clean_webhook_endpoints(vec![WebhookEndpointConfig {
            url: "https://10.0.0.10/webhook".to_string(),
            events: vec![],
            secret: None,
        }]);

        assert!(endpoints.is_empty());
    }

    #[test]
    fn webhook_endpoint_cleanup_rejects_ipv4_mapped_ipv6_private_literals() {
        let endpoints = clean_webhook_endpoints(vec![WebhookEndpointConfig {
            url: "https://[::ffff:10.0.0.1]/webhook".to_string(),
            events: vec![],
            secret: None,
        }]);

        assert!(endpoints.is_empty());
    }

    #[test]
    fn webhook_endpoint_cleanup_accepts_resolvable_loopback_hostnames() {
        let endpoints = clean_webhook_endpoints(vec![WebhookEndpointConfig {
            url: "http://localhost:9000/webhook".to_string(),
            events: vec![],
            secret: None,
        }]);

        assert_eq!(endpoints.len(), 1);
    }

    #[test]
    fn webhook_endpoint_debug_redacts_secret() {
        let endpoint = WebhookEndpointConfig {
            url: "http://example.test/webhook".to_string(),
            events: vec!["sandbox.created".to_string()],
            secret: Some("top-secret".to_string()),
        };

        let debug = format!("{endpoint:?}");
        assert!(!debug.contains("top-secret"));
        assert!(debug.contains("***"));
    }

    #[test]
    fn csv_parser_trims_empty_items() {
        assert_eq!(
            split_csv(" sandbox.created, ,sandbox.deleted "),
            vec!["sandbox.created", "sandbox.deleted"]
        );
    }

    #[test]
    fn env_parse_clamps_queue_capacity_to_effective_minimum() {
        std::env::set_var("CUBE_API_WEBHOOK_QUEUE_CAPACITY", "0");
        assert_eq!(default_webhook_queue_capacity(), 1usize);
        std::env::remove_var("CUBE_API_WEBHOOK_QUEUE_CAPACITY");
    }
}

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
    /// The middleware will POST to this URL with the credential headers plus
    /// `X-Request-Path: <original request path>`. An HTTP 200 response grants
    /// access; any other status code returns 401 to the client.
    ///
    /// When unset (default), all requests are allowed through without authentication.
    ///
    /// CLI flag: --auth-callback-url  |  Env var: AUTH_CALLBACK_URL
    #[serde(default)]
    pub auth_callback_url: Option<String>,

    /// Optional MySQL database URL used by AgentHub persistence.
    ///
    /// Env vars checked by default: DATABASE_URL, then CUBE_API_DATABASE_URL.
    /// Example: mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp
    #[serde(default = "default_database_url")]
    pub database_url: Option<String>,

    /// E2B-compatible OCI registry upstream URL. When set, /v2/* requests are
    /// reverse-proxied to this address so that `e2b template build` (which uses
    /// `docker push`) can upload images that CubeMaster will later consume.
    ///
    /// Recommended deployment: run `distribution/distribution` (CNCF Registry)
    /// as a sidecar listening on 127.0.0.1:5000 and set
    /// CUBE_API_REGISTRY_UPSTREAM=http://127.0.0.1:5000.
    ///
    /// When unset, /v2/* returns 503 and `dockerfile`-based template requests
    /// are rejected with 501.
    ///
    /// ## Security contract — read this before exposing CubeAPI publicly
    ///
    /// CubeAPI itself enforces **per-build, short-lived push credentials**
    /// on every `/v2/*` path other than the unauthenticated `GET /v2/` ping
    /// (which is required by the docker / oci-distribution handshake). The
    /// credential is minted at build-creation time, returned to the SDK in
    /// the `registry` field of the build response, indexed inside the
    /// in-memory `BuildRegistry`, and is repo-scoped: it can only push /
    /// pull blobs and manifests under `<repo_prefix>/<templateID>`. It is
    /// dropped when the build reaches its terminal stage (TTL- or
    /// size-cap-evicted by `BuildRegistry`).
    ///
    /// **Strongly recommended** in addition: run an authenticated upstream
    /// (e.g. `distribution/distribution` with htpasswd) and bind CubeAPI
    /// itself behind TLS + an HTTP authenticator. Both layers together
    /// match the depth of access control most operators expect from a
    /// public OCI registry.
    ///
    /// **Not safe**: setting `registry_upstream` to an unauthenticated
    /// upstream *and* binding CubeAPI on a public interface without TLS.
    /// CubeAPI's own credential gate covers the bulk of the attack
    /// surface, but it cannot stop a network attacker from observing the
    /// per-build password in transit. CubeAPI logs a `WARN` at startup
    /// when this combination is detected (see
    /// `AppState::log_registry_security_posture`).
    #[serde(default)]
    pub registry_upstream: Option<String>,

    /// Public host (no scheme) advertised to E2B clients as the docker-push
    /// target, e.g. "cube.example.com". Defaults to the Host header of the
    /// originating /templates request when unset.
    #[serde(default)]
    pub registry_public_host: Option<String>,

    /// Repository namespace prefix for uploaded build images. The full image
    /// reference returned to CubeMaster will be:
    ///   <registry_pull_host>/<repo_prefix>/<templateID>:<buildID>
    /// Default: "e2b".
    #[serde(default = "default_registry_repo_prefix")]
    pub registry_repo_prefix: String,

    /// Internal registry host CubeMaster nodes should pull from (e.g.
    /// "10.0.0.1:5000"). Defaults to `registry_upstream` host:port when unset.
    #[serde(default)]
    pub registry_pull_host: Option<String>,

    /// Optional shared secret printed back as `registry.password` in
    /// POST /templates responses. Empty → "_anon".
    #[serde(default)]
    pub registry_token: Option<String>,

    /// Default `writable_layer_size` to send to CubeMaster when the client
    /// (e.g. the E2B Python SDK) does not specify one. CubeMaster validates
    /// this field as required, so a non-empty default is needed for the V3
    /// flow to work out of the box.
    ///
    /// Env var: CUBE_API_DEFAULT_WRITABLE_LAYER_SIZE  |  Default: "1G".
    #[serde(default = "default_writable_layer_size")]
    pub default_writable_layer_size: String,

    /// How long (seconds) a *terminal* build (Ready / Error) is kept in the
    /// in-memory `BuildRegistry` after reaching its terminal stage. Past this
    /// TTL the build context (create request, credentials, logs, …) is
    /// evicted by the background GC.
    ///
    /// 0 disables TTL-based eviction (only the size cap will fire).
    /// Default: 3600 (1 hour) — comfortably covers slow log pollers without
    /// retaining old builds for the lifetime of the process.
    #[serde(default = "default_build_registry_terminal_ttl_secs")]
    pub build_registry_terminal_ttl_secs: u64,

    /// Hard upper bound on the number of *logical* builds tracked in the
    /// `BuildRegistry`. When exceeded, the oldest terminal builds are
    /// evicted FIFO regardless of TTL. In-flight builds are never evicted by
    /// this cap (a warning is logged if the cap can't be honoured because
    /// every entry is still in-flight).
    ///
    /// 0 disables the cap (only TTL applies). Default: 5000.
    #[serde(default = "default_build_registry_max_entries")]
    pub build_registry_max_entries: usize,

    /// Interval (seconds) at which the background GC task scans the
    /// `BuildRegistry` for TTL-expired terminal builds. Default: 300 (5 min).
    /// 0 disables the background task entirely (size-cap eviction at
    /// `create()` time still applies).
    #[serde(default = "default_build_registry_gc_interval_secs")]
    pub build_registry_gc_interval_secs: u64,
}

fn default_registry_repo_prefix() -> String {
    "e2b".to_string()
}

fn default_build_registry_terminal_ttl_secs() -> u64 {
    3600
}
fn default_build_registry_max_entries() -> usize {
    5000
}
fn default_build_registry_gc_interval_secs() -> u64 {
    300
}

fn default_writable_layer_size() -> String {
    std::env::var("CUBE_API_DEFAULT_WRITABLE_LAYER_SIZE").unwrap_or_else(|_| "1G".to_string())
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
        .or_else(|| std::env::var("CUBE_API_DATABASE_URL").ok())
        .or_else(default_cube_sandbox_mysql_url)
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
            registry_upstream: None,
            registry_public_host: None,
            registry_repo_prefix: default_registry_repo_prefix(),
            registry_pull_host: None,
            registry_token: None,
            default_writable_layer_size: default_writable_layer_size(),
            build_registry_terminal_ttl_secs: default_build_registry_terminal_ttl_secs(),
            build_registry_max_entries: default_build_registry_max_entries(),
            build_registry_gc_interval_secs: default_build_registry_gc_interval_secs(),
        }
    }
}

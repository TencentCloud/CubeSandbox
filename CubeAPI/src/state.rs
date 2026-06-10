// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::cubemaster::CubeMasterClient;
use crate::db::AgentHubStore;
use crate::logging::ArcLogger;
use crate::services::AppServices;
use governor::{DefaultKeyedRateLimiter, Quota, RateLimiter};
use std::num::NonZeroU32;
use std::sync::Arc;

/// Shared application state passed to every handler via Axum's `State` extractor.
/// All fields must be cheap to clone (Arc / DashMap / etc.) — Axum clones State
/// on every request, so real data must live behind Arc.
#[derive(Clone)]
pub struct AppState {
    /// Per-API-key rate limiter (token bucket).
    pub rate_limiter: Arc<DefaultKeyedRateLimiter<String>>,

    /// Shared reqwest connection pool.
    pub http_client: reqwest::Client,

    /// Shared business services built on top of CubeMaster.
    pub services: AppServices,

    /// Structured event logger (fan-out to all configured backends).
    pub logger: ArcLogger,

    /// Server config snapshot.
    pub config: Arc<crate::config::ServerConfig>,

    /// Optional database-backed AgentHub instance store.
    pub agenthub_store: Option<AgentHubStore>,
}

impl AppState {
    /// Construct AppState with all backends initialised.
    ///
    /// The `logger` is built externally (in `main.rs`) because `FileLogger::new`
    /// is async and requires the Tokio runtime to be running.
    pub async fn new(config: crate::config::ServerConfig, logger: ArcLogger) -> Self {
        let quota = Quota::per_second(NonZeroU32::new(config.rate_limit_per_sec.max(1)).unwrap());
        let rate_limiter = Arc::new(RateLimiter::keyed(quota));

        let http_client = reqwest::Client::builder()
            .pool_max_idle_per_host(100)
            .connection_verbose(false)
            .build()
            .expect("failed to build HTTP client");

        let cubemaster = CubeMasterClient::new(config.cubemaster_url.clone(), http_client.clone());
        let services = AppServices::new(&config, cubemaster.clone());
        let agenthub_store = match config
            .database_url
            .as_deref()
            .filter(|v| !v.trim().is_empty())
        {
            Some(url) => match AgentHubStore::connect(url).await {
                Ok(store) => Some(store),
                Err(err) => {
                    tracing::warn!(error = %err, "agenthub database disabled");
                    None
                }
            },
            None => None,
        };

        let s = Self {
            rate_limiter,
            http_client,
            services,
            logger,
            config: Arc::new(config),
            agenthub_store,
        };
        s.log_registry_security_posture();
        s
    }

    /// Emit a single startup line summarising whether the bundled OCI
    /// registry reverse-proxy is on, and — if so — whether the operator
    /// has obviously misconfigured the deployment such that the per-build
    /// credentials are exposed in the clear.
    ///
    /// We don't refuse to start: this is a one-click developer-experience
    /// product, and a hard failure on `bind=0.0.0.0` would surprise users
    /// running on a single VM with a firewall in front of them. But we do
    /// log loudly so that production operators see the warning during the
    /// first deploy.
    fn log_registry_security_posture(&self) {
        let upstream = self
            .config
            .registry_upstream
            .as_deref()
            .map(str::trim)
            .filter(|s| !s.is_empty());
        let Some(upstream) = upstream else {
            tracing::info!("registry reverse-proxy disabled (CUBE_API_REGISTRY_UPSTREAM unset)");
            return;
        };

        let upstream_is_loopback = upstream.contains("127.0.0.1")
            || upstream.contains("localhost")
            || upstream.contains("[::1]");
        let bind = self.config.bind.as_str();
        let bind_is_loopback = bind.starts_with("127.0.0.1") || bind.starts_with("[::1]");
        let bind_is_public = bind.starts_with("0.0.0.0") || bind.starts_with("[::]");

        if bind_is_public && upstream_is_loopback {
            tracing::warn!(
                bind = %bind,
                upstream = %upstream,
                "registry reverse-proxy is enabled with an unauthenticated loopback \
                 upstream while CubeAPI binds on a public interface. CubeAPI's own \
                 per-build credential gate is in force, but build push tokens will \
                 cross the network in clear text unless this listener is fronted \
                 by TLS. Either: (a) terminate TLS in a reverse proxy in front of \
                 CubeAPI, or (b) run distribution/distribution with htpasswd auth \
                 and rely on the upstream's own TLS+auth. See ServerConfig::registry_upstream."
            );
        } else if bind_is_loopback {
            tracing::info!(
                bind = %bind,
                upstream = %upstream,
                "registry reverse-proxy enabled on a loopback bind; safe for development"
            );
        } else {
            tracing::info!(
                bind = %bind,
                upstream = %upstream,
                "registry reverse-proxy enabled; per-build credential gate is in force"
            );
        }
    }
}

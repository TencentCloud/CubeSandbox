// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::cubemaster::CubeMasterClient;
use crate::handlers::terminal::TerminalTickets;
use crate::logging::ArcLogger;
use crate::services::AppServices;
use governor::{DefaultKeyedRateLimiter, Quota, RateLimiter};
use std::num::NonZeroU32;
use std::sync::Arc;

/// Optional WebUI session store for validating `X-Session-Token` headers.
/// Implementors provide a database-backed session lookup.
#[async_trait::async_trait]
pub trait SessionStore: Send + Sync {
    async fn validate_session(&self, token: &str) -> anyhow::Result<Option<String>>;
}

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

    /// Process-wide store of outstanding terminal tickets.
    pub terminal_tickets: TerminalTickets,

    /// Bounds the number of concurrent terminal sessions server-wide.
    pub terminal_sessions: Arc<tokio::sync::Semaphore>,

    /// Optional WebUI session store (database-backed). When set, terminal
    /// handlers require a valid `X-Session-Token` header.
    pub agenthub_store: Option<Arc<dyn SessionStore>>,
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
        let services = AppServices::new(&config, cubemaster);

        let terminal_sessions =
            Arc::new(tokio::sync::Semaphore::new(config.terminal_max_sessions.max(1)));

        Self {
            rate_limiter,
            http_client,
            services,
            logger,
            config: Arc::new(config),
            terminal_tickets: TerminalTickets::default(),
            terminal_sessions,
            agenthub_store: None,
        }
    }
}

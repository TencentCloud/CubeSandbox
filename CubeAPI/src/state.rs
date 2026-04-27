// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::cubemaster::CubeMasterClient;
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
}

impl AppState {
    /// Construct AppState with all backends initialised.
    ///
    /// The `logger` is built externally (in `main.rs`) because `FileLogger::new`
    /// is async and requires the Tokio runtime to be running.
    pub fn new(config: crate::config::ServerConfig, logger: ArcLogger) -> Self {
        let quota = Quota::per_second(NonZeroU32::new(config.rate_limit_per_sec.max(1)).unwrap());
        let rate_limiter = Arc::new(RateLimiter::keyed(quota));

        let http_client = reqwest::Client::builder()
            .pool_max_idle_per_host(100)
            .connection_verbose(false)
            .build()
            .expect("failed to build HTTP client");

        let cubemaster = CubeMasterClient::new(config.cubemaster_url.clone(), http_client.clone());
        let services = AppServices::new(&config, cubemaster.clone());

        Self {
            rate_limiter,
            http_client,
            services,
            logger,
            config: Arc::new(config),
        }
    }
}

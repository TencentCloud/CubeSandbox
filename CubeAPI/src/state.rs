// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::cubemaster::CubeMasterClient;
use crate::logging::ArcLogger;
use crate::services::AppServices;
use governor::{DefaultKeyedRateLimiter, Quota, RateLimiter};
use std::num::NonZeroU32;
use std::sync::Arc;
use std::time::Duration;

/// Shared application state passed to every handler via Axum's `State` extractor.
/// All fields must be cheap to clone (Arc / DashMap / etc.) — Axum clones State
/// on every request, so real data must live behind Arc.
#[derive(Clone)]
pub struct AppState {
    /// Per-credential rate limiter (token bucket) shared by all request classes.
    /// Used as fallback for classes without a dedicated limiter.
    pub rate_limiter: Arc<DefaultKeyedRateLimiter<String>>,

    /// Optional per-credential rate limiter for read requests (GET/HEAD).
    /// When `None`, read requests fall back to `rate_limiter`.
    pub read_rate_limiter: Option<Arc<DefaultKeyedRateLimiter<String>>>,

    /// Optional per-credential rate limiter for write requests
    /// (POST/PUT/PATCH/DELETE). When `None`, write requests fall back to
    /// `rate_limiter`.
    pub write_rate_limiter: Option<Arc<DefaultKeyedRateLimiter<String>>>,

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
    pub async fn new(config: crate::config::ServerConfig, logger: ArcLogger) -> Self {
        let rate_limiter = keyed_rate_limiter(config.rate_limit_per_sec);
        let read_rate_limiter = config.rate_limit_read_per_sec.map(keyed_rate_limiter);
        let write_rate_limiter = config.rate_limit_write_per_sec.map(keyed_rate_limiter);
        spawn_rate_limiter_housekeeping(
            rate_limiter.clone(),
            read_rate_limiter.clone(),
            write_rate_limiter.clone(),
        );

        let http_client = reqwest::Client::builder()
            .pool_max_idle_per_host(100)
            .connection_verbose(false)
            .build()
            .expect("failed to build HTTP client");

        let cubemaster = CubeMasterClient::new(config.cubemaster_url.clone(), http_client.clone());
        let services = AppServices::new(&config, cubemaster);

        Self {
            rate_limiter,
            read_rate_limiter,
            write_rate_limiter,
            http_client,
            services,
            logger,
            config: Arc::new(config),
        }
    }
}

/// Build a per-second keyed token bucket; values below 1 clamp to 1 so the
/// quota is always valid.
fn keyed_rate_limiter(per_sec: u32) -> Arc<DefaultKeyedRateLimiter<String>> {
    let quota = Quota::per_second(NonZeroU32::new(per_sec.max(1)).unwrap());
    Arc::new(RateLimiter::keyed(quota))
}

/// Periodically evict credential buckets whose token state is fresh again.
/// Without this, rotated callback credentials remain in governor's keyed maps
/// for the lifetime of the process.
fn spawn_rate_limiter_housekeeping(
    shared: Arc<DefaultKeyedRateLimiter<String>>,
    read: Option<Arc<DefaultKeyedRateLimiter<String>>>,
    write: Option<Arc<DefaultKeyedRateLimiter<String>>>,
) {
    tokio::spawn(async move {
        let mut interval = tokio::time::interval(Duration::from_secs(60));
        interval.tick().await;
        loop {
            interval.tick().await;
            shared.retain_recent();
            if let Some(limiter) = read.as_ref() {
                limiter.retain_recent();
            }
            if let Some(limiter) = write.as_ref() {
                limiter.retain_recent();
            }
        }
    });
}

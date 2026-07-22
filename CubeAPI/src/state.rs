// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::cubemaster::CubeMasterClient;
use crate::logging::ArcLogger;
use crate::security::outbound_url::{build_secure_client, OutboundUrlPolicy};
use crate::services::AppServices;
use anyhow::Context;
use governor::{DefaultKeyedRateLimiter, Quota, RateLimiter};
use std::num::NonZeroU32;
use std::sync::Arc;
use std::time::Duration;

/// Shared application state passed to every handler via Axum's `State` extractor.
/// All fields must be cheap to clone (Arc / DashMap / etc.) — Axum clones State
/// on every request, so real data must live behind Arc.
#[derive(Clone)]
pub struct AppState {
    /// Per-API-key rate limiter (token bucket).
    pub rate_limiter: Arc<DefaultKeyedRateLimiter<String>>,

    /// Hardened auth callback configuration.
    ///
    /// Only present when `auth_callback_url` is configured. The bundled client
    /// uses a pinned DNS address, disables redirects/proxies, and enforces
    /// timeouts.
    pub auth_callback: Option<AuthCallbackConfig>,

    /// Shared business services built on top of CubeMaster.
    pub services: AppServices,

    /// Structured event logger (fan-out to all configured backends).
    pub logger: ArcLogger,

    /// Server config snapshot.
    pub config: Arc<crate::config::ServerConfig>,
}

/// Auth callback URL together with its dedicated, hardened HTTP client.
///
/// Bundling the URL and client guarantees that a configured callback always has
/// a matching secure client, so callers do not need to assert this invariant.
#[derive(Clone)]
pub struct AuthCallbackConfig {
    pub url: String,
    pub client: reqwest::Client,
}

impl AppState {
    /// Construct AppState with all backends initialised.
    ///
    /// The `logger` is built externally (in `main.rs`) because `FileLogger::new`
    /// is async and requires the Tokio runtime to be running.
    pub async fn new(
        config: crate::config::ServerConfig,
        logger: ArcLogger,
    ) -> anyhow::Result<Self> {
        let quota = Quota::per_second(NonZeroU32::new(config.rate_limit_per_sec.max(1)).unwrap());
        let rate_limiter = Arc::new(RateLimiter::keyed(quota));

        let http_client = reqwest::Client::builder()
            .pool_max_idle_per_host(100)
            .connection_verbose(false)
            .build()
            .expect("failed to build HTTP client");

        // Validate and build a hardened client for the auth callback URL.
        let auth_callback = if let Some(ref url) = config.auth_callback_url {
            let policy = OutboundUrlPolicy::from_config(&config.outbound_url_security);
            let validated = policy
                .validate(url)
                .await
                .with_context(|| format!("invalid auth_callback_url: {}", url))?;
            let client =
                build_secure_client(&validated, Duration::from_secs(5), Duration::from_secs(10))
                    .context("failed to build auth callback HTTP client")?;
            Some(AuthCallbackConfig {
                url: url.clone(),
                client,
            })
        } else {
            None
        };

        let cubemaster = CubeMasterClient::new(config.cubemaster_url.clone(), http_client.clone());
        let services = AppServices::new(&config, cubemaster);

        Ok(Self {
            rate_limiter,
            auth_callback,
            services,
            logger,
            config: Arc::new(config),
        })
    }
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::cubemaster::CubeMasterClient;
use crate::db::AgentHubStore;
use crate::logging::ArcLogger;
use crate::services::AppServices;
use dashmap::{mapref::entry::Entry, DashMap};
use governor::{DefaultKeyedRateLimiter, Quota, RateLimiter};
use std::num::NonZeroU32;
use std::sync::Arc;

/// Process-local limiter scoped to one `AppState` and one CubeAPI lifetime.
/// A restart discards all counts; permits must not be carried across reloaded
/// application state.
#[derive(Clone)]
pub struct TerminalSessionLimiter {
    counts: Arc<DashMap<String, usize>>,
    max_per_sandbox: usize,
}

impl TerminalSessionLimiter {
    fn new(max_per_sandbox: usize) -> Self {
        Self {
            counts: Arc::new(DashMap::new()),
            max_per_sandbox: max_per_sandbox.max(1),
        }
    }

    pub fn try_acquire(&self, sandbox_id: &str) -> Option<TerminalSessionPermit> {
        let mut count = self.counts.entry(sandbox_id.to_string()).or_default();
        if *count >= self.max_per_sandbox {
            return None;
        }
        *count += 1;
        drop(count);
        Some(TerminalSessionPermit {
            limiter: self.clone(),
            sandbox_id: sandbox_id.to_string(),
        })
    }
}

pub struct TerminalSessionPermit {
    limiter: TerminalSessionLimiter,
    sandbox_id: String,
}

impl Drop for TerminalSessionPermit {
    fn drop(&mut self) {
        if let Entry::Occupied(mut entry) = self.limiter.counts.entry(self.sandbox_id.clone()) {
            if *entry.get() <= 1 {
                entry.remove();
            } else {
                *entry.get_mut() -= 1;
            }
        }
    }
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

    /// Optional database-backed AgentHub instance store.
    pub agenthub_store: Option<AgentHubStore>,

    /// Process-local active terminal session limit keyed by sandbox ID.
    pub terminal_sessions: TerminalSessionLimiter,
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
        let terminal_sessions =
            TerminalSessionLimiter::new(config.terminal_max_sessions_per_sandbox);

        Self {
            rate_limiter,
            http_client,
            services,
            logger,
            config: Arc::new(config),
            agenthub_store,
            terminal_sessions,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn terminal_session_limit_is_per_sandbox_and_releases_capacity() {
        let limiter = TerminalSessionLimiter::new(2);
        let first = limiter.try_acquire("sandbox-1").unwrap();
        let second = limiter.try_acquire("sandbox-1").unwrap();
        assert!(limiter.try_acquire("sandbox-1").is_none());
        assert!(limiter.try_acquire("sandbox-2").is_some());

        drop(first);
        assert!(limiter.try_acquire("sandbox-1").is_some());
        drop(second);
    }
}

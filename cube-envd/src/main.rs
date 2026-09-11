// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! cube-envd — CubeSandbox-maintained in-guest data-plane daemon.
//!
//! Speaks the E2B envd protocol (REST + ConnectRPC over JSON) on a single
//! port so existing SDKs and the CubeSandbox control plane keep working
//! unchanged. See README.md and issue #1227.
//!
//! "Upstream" / "baseline" throughout the code means the e2b Go envd
//! (`e2b-dev/infra`, pinned 0.5.13 / base image 2026.16) that this crate is
//! compatibility-tested against — see tests/e2e/envd_conformance.

mod app;
mod compat;
mod filesystem;
mod platform;
mod process;
mod protocol;

use std::sync::Arc;

use app::cli::{parse_cli, COMMIT, VERSION};
use app::routes;

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let cli = match parse_cli(&args) {
        Ok(cli) => cli,
        Err(code) => std::process::exit(code),
    };

    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_env("ENVD_LOG_LEVEL")
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .with_target(false)
        .init();

    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        // Blocking pool (see app/pool.rs): the default 512 threads at
        // ~13KiB touched RSS each is a ~6.6MiB worst case — larger than the
        // whole #1311 memory budget. 64 leaves ample headroom for the
        // sandbox's dozens-of-ops workload. Deliberate divergence from the
        // unbounded-goroutine baseline: over the cap, requests queue
        // (never error). `thread_keep_alive` is tokio's 10s default, written
        // out so the burst-reuse behavior is explicit.
        .max_blocking_threads(64)
        .thread_keep_alive(std::time::Duration::from_secs(10))
        .enable_all()
        .build()
        .expect("build tokio runtime");

    runtime.block_on(async move {
        let state = Arc::new(app::state::AppState::new().with_cgroup(process::cgroup::init()));
        let app = routes::router(state);
        let addr = std::net::SocketAddr::from(([0, 0, 0, 0], cli.port));
        let listener = match tokio::net::TcpListener::bind(addr).await {
            Ok(l) => l,
            Err(e) => {
                tracing::error!("failed to bind {addr}: {e}");
                std::process::exit(1);
            }
        };
        tracing::info!("cube-envd {VERSION} ({COMMIT}) listening on {addr}");
        if let Err(e) = axum::serve(listener, app).await {
            tracing::error!("server error: {e}");
            std::process::exit(1);
        }
    });
}

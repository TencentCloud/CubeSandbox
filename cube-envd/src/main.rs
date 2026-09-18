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

use app::cli::{parse_cli, COMMIT, IMPL_VERSION, VERSION};
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

    // Deployment knobs first (see platform/limits.rs): the flags from
    // ENVD_EXTRA_ARGS win, the CUBE_ENVD_* variables are the fallback.
    platform::limits::configure(cli.blocking_threads, cli.download_max_bodies);

    // Deployment knob (see platform/limits.rs): 64 is the default, chosen so
    // the pool's worst-case touched RSS (~13KiB/thread) stays inside this
    // in-guest daemon's budget while covering the sandbox's dozens-of-ops
    // workload. Deliberate divergence from the unbounded-goroutine baseline:
    // over the cap, requests queue (never error).
    let blocking_threads = platform::limits::blocking_threads();
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .max_blocking_threads(blocking_threads)
        .thread_keep_alive(std::time::Duration::from_secs(10))
        .enable_all()
        .build()
        .expect("build tokio runtime");
    // Effective limits, once, so an operator can see what the deployment
    // actually got instead of inferring it from the environment.
    tracing::info!(
        blocking_threads,
        download_blocking_producers = platform::limits::download_blocking_producers(),
        download_buffered_bodies = platform::limits::download_buffered_bodies(),
        download_max_bodies = platform::limits::download_max_bodies(),
        download_pool_bytes = filesystem::download::pool_budget_bytes(),
        "runtime limits"
    );

    runtime.block_on(async move {
        let state = Arc::new(
            app::state::AppState::new().with_cgroup(process::cgroup::init(cli.cgroup_memory_max)),
        );
        let app = routes::router(state);
        let addr = std::net::SocketAddr::from(([0, 0, 0, 0], cli.port));
        let listener = match tokio::net::TcpListener::bind(addr).await {
            Ok(l) => l,
            Err(e) => {
                tracing::error!("failed to bind {addr}: {e}");
                std::process::exit(1);
            }
        };
        // `app::serve::serve` is this branch's accept loop: it asks for
        // `half_close(true)` (a Connect client may end its request stream before
        // the response is written) and sets TCP_NODELAY per connection. Nagle is
        // on by default and costs a full delayed-ACK round trip on any response
        // whose head and body leave as separate small writes -- a 4 KiB `/files`
        // download measured 44 ms against 1.6 ms for 1 MiB -- while the Go
        // baseline's `net/http` sets it itself, so the accept loop is where it
        // belongs rather than on a server builder.
        tracing::info!(
            "cube-envd {VERSION} (compat; impl {IMPL_VERSION}, commit {COMMIT}) listening on {addr}"
        );
        if let Err(e) = app::serve::serve(listener, app).await {
            tracing::error!("server error: {e}");
            std::process::exit(1);
        }
    });
}

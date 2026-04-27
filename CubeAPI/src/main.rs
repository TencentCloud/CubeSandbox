// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

mod config;
mod constants;
mod cubemaster;
mod error;
mod handlers;
mod logging;
mod middleware;
mod models;
mod openapi;
mod routes;
mod services;
mod state;

use clap::Parser;
use tracing_subscriber::{fmt, prelude::*, EnvFilter};

/// cube-api — high-performance E2B-compatible sandbox API server.
///
/// Configuration is layered in this priority order (highest → lowest):
///
///   1. CLI flags (e.g. --bind, --auth-callback-url)
///   2. Environment variables (e.g. CUBE_API_BIND, CUBE_API_SANDBOX_DOMAIN, AUTH_CALLBACK_URL)
///   3. Built-in defaults
///
/// Most settings are controlled via environment variables; only the most
/// commonly overridden ones are exposed as CLI flags.
#[derive(Parser, Debug)]
#[command(name = "cube-api", version, about, long_about = None)]
struct Cli {
    /// Enable debug log level (overrides LOG_LEVEL env var and config).
    ///
    /// When set, both the stdout tracing subscriber and the structured file
    /// logger will emit DEBUG-level messages. Equivalent to setting
    /// LOG_LEVEL=debug.
    #[arg(long, default_value_t = false)]
    debug: bool,

    /// Bind address for the HTTP server (default: "0.0.0.0:3000").
    ///
    /// Overrides the CUBE_API_BIND environment variable.
    /// Examples: "0.0.0.0:3000", "127.0.0.1:8080", "[::]:3000"
    #[arg(long, value_name = "HOST:PORT")]
    bind: Option<String>,

    /// CubeMaster base URL (default: "http://127.0.0.1:8089").
    ///
    /// Overrides the CUBE_MASTER_ADDR environment variable.
    /// Example: "http://10.0.0.1:8089"
    #[arg(long, value_name = "URL")]
    cubemaster_url: Option<String>,

    /// Auth callback URL for HTTP authentication.
    ///
    /// When set, every API request (except GET /health) must carry either:
    ///   - "Authorization: Bearer <token>", or
    ///   - "X-API-Key: <key>"
    ///
    /// The server will POST to this URL forwarding the credential header plus:
    ///   - "X-Request-Path: <original request path>"
    ///
    /// The callback must return HTTP 200 to allow the request; any other
    /// status code causes the server to reject the request with 401.
    ///
    /// When omitted, all requests are allowed without authentication.
    /// Overrides the AUTH_CALLBACK_URL environment variable.
    #[arg(long, value_name = "URL")]
    auth_callback_url: Option<String>,

    /// Maximum number of Tokio worker threads (default: number of CPU cores).
    ///
    /// Set to 0 to use the default (all available cores).
    /// Overrides the WORKER_THREADS environment variable.
    #[arg(long, value_name = "N")]
    worker_threads: Option<usize>,

    /// Log level for stdout tracing (default: "info").
    ///
    /// Valid values: trace | debug | info | warn | error
    /// The RUST_LOG environment variable takes precedence over this flag.
    /// Overrides the LOG_LEVEL environment variable.
    #[arg(long, value_name = "LEVEL")]
    log_level: Option<String>,

    /// Directory for rolling log files (default: <binary_dir>/log).
    ///
    /// Overrides the LOG_DIR environment variable.
    #[arg(long, value_name = "PATH")]
    log_dir: Option<String>,

    /// Prefix for log file names (default: "cube-api").
    ///
    /// Log files are named "<prefix>-YYYY-MM-DD.log".
    /// Overrides the LOG_PREFIX environment variable.
    #[arg(long, value_name = "PREFIX")]
    log_prefix: Option<String>,

    /// Rate limit: max requests per second per API key (default: 100).
    ///
    /// Overrides the RATE_LIMIT_PER_SEC environment variable.
    #[arg(long, value_name = "N")]
    rate_limit_per_sec: Option<u32>,

    /// Default sandbox instance type sent to CubeMaster (default: "cubebox").
    ///
    /// Valid values: "cubebox"
    /// Overrides the INSTANCE_TYPE environment variable.
    #[arg(long, value_name = "TYPE")]
    instance_type: Option<String>,

    /// Domain string returned in sandbox API responses (default: "cube.app").
    #[arg(long, value_name = "DOMAIN")]
    sandbox_domain: Option<String>,

    /// Export the current OpenAPI spec to a YAML file and exit.
    #[arg(long, value_name = "PATH")]
    export_openapi: Option<String>,
}

fn main() -> anyhow::Result<()> {
    // ── CLI ────────────────────────────────────────────────────────────────
    let cli = Cli::parse();

    if let Some(path) = cli.export_openapi.as_deref() {
        openapi::export_to_file(path)?;
        println!("exported OpenAPI to {}", path);
        return Ok(());
    }

    // ── Config ─────────────────────────────────────────────────────────────
    let mut cfg = config::ServerConfig::from_env().unwrap_or_default();

    // CLI flags override env vars / config file (highest priority)
    if cli.debug {
        cfg.log_level = "debug".to_string();
    }
    if let Some(v) = cli.log_level {
        cfg.log_level = v;
    }
    if let Some(v) = cli.bind {
        cfg.bind = v;
    }
    if let Some(v) = cli.cubemaster_url {
        cfg.cubemaster_url = v;
    }
    if let Some(v) = cli.auth_callback_url {
        cfg.auth_callback_url = Some(v);
    }
    if let Some(v) = cli.worker_threads {
        cfg.worker_threads = v;
    }
    if let Some(v) = cli.log_dir {
        cfg.log_dir = v;
    }
    if let Some(v) = cli.log_prefix {
        cfg.log_prefix = v;
    }
    if let Some(v) = cli.rate_limit_per_sec {
        cfg.rate_limit_per_sec = v;
    }
    if let Some(v) = cli.instance_type {
        cfg.instance_type = v;
    }
    if let Some(v) = cli.sandbox_domain {
        cfg.sandbox_domain = v;
    }

    // ── Tracing (stdout) ───────────────────────────────────────────────────
    // RUST_LOG env var takes precedence; --debug / --log-level / config is fallback.
    tracing_subscriber::registry()
        .with(fmt::layer())
        .with(EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new(&cfg.log_level)))
        .init();

    tracing::info!(
        debug_mode = cli.debug,
        log_level = %cfg.log_level,
        bind = %cfg.bind,
        auth_enabled = cfg.auth_callback_url.is_some(),
        "cube-api starting"
    );

    // ── Tokio runtime ──────────────────────────────────────────────────────
    let mut builder = tokio::runtime::Builder::new_multi_thread();
    if cfg.worker_threads > 0 {
        builder.worker_threads(cfg.worker_threads);
    }
    let rt = builder
        .enable_all()
        .thread_name("cube-api-worker")
        .build()?;

    rt.block_on(async_main(cfg, cli.debug))
}

async fn async_main(cfg: config::ServerConfig, debug: bool) -> anyhow::Result<()> {
    use logging::{arc, file::FileLogger, filtered::FilteredLogger, multi::MultiLogger, LogLevel};

    // ── Logger ────────────────────────────────────────────────────────────
    let min_level = if debug {
        LogLevel::Debug
    } else {
        LogLevel::Info
    };

    let file_logger = FileLogger::new(cfg.log_dir.clone(), cfg.log_prefix.clone()).await?;

    // FilteredLogger gates by level → MultiLogger fans out to file (+ future backends)
    let logger: logging::ArcLogger = arc(FilteredLogger::new(
        arc(
            MultiLogger::new().add(arc(file_logger)), // Uncomment to add more backends:
                                                      // .add(arc(logging::http::HttpLogger::new(Default::default())))
                                                      // .add(arc(logging::otlp::OtlpLogger::new()))
        ),
        min_level,
    ));

    tracing::info!(
        log_dir = %cfg.log_dir,
        log_prefix = %cfg.log_prefix,
        min_level = %min_level,
        "structured event logger started"
    );

    // ── App state ─────────────────────────────────────────────────────────
    let state = state::AppState::new(cfg.clone(), logger.clone());

    // ── Router ────────────────────────────────────────────────────────────
    let app = routes::build_router(state);

    // ── Bind ──────────────────────────────────────────────────────────────
    let listener = tokio::net::TcpListener::bind(&cfg.bind).await?;
    tracing::info!("cube-api listening on {}", cfg.bind);

    // ── Graceful shutdown ──────────────────────────────────────────────────
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal())
        .await?;

    logging::Logger::flush(&*logger).await;
    tracing::info!("cube-api shut down gracefully");
    Ok(())
}

async fn shutdown_signal() {
    use tokio::signal;

    let ctrl_c = async {
        signal::ctrl_c()
            .await
            .expect("failed to install Ctrl+C handler");
    };

    #[cfg(unix)]
    let terminate = async {
        signal::unix::signal(signal::unix::SignalKind::terminate())
            .expect("failed to install SIGTERM handler")
            .recv()
            .await;
    };

    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();

    tokio::select! {
        _ = ctrl_c => {},
        _ = terminate => {},
    }

    tracing::info!("shutdown signal received");
}

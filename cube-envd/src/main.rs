// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! cube-envd — CubeSandbox-maintained in-guest data-plane daemon.
//!
//! Speaks the E2B envd protocol (REST + ConnectRPC over JSON) on a single
//! port so existing SDKs and the CubeSandbox control plane keep working
//! unchanged. See README.md and issue #1227.

mod auth;
mod connect;
mod error;
mod exec;
mod msg;
mod rest;
mod server;
mod services;
mod state;

use std::sync::Arc;

const VERSION: &str = env!("CARGO_PKG_VERSION");
const COMMIT: &str = match option_env!("CUBE_ENVD_COMMIT") {
    Some(c) => c,
    None => "unknown",
};
const DEFAULT_PORT: u16 = 49983;

struct Cli {
    port: u16,
}

/// Go-style single-dash flag parsing, kept lenient on purpose: unknown flags
/// are warned about and skipped so `ENVD_EXTRA_ARGS` written for the upstream
/// Go envd (e.g. `-cgroup-root`) do not prevent cube-envd from starting.
fn parse_cli(args: &[String]) -> Result<Cli, i32> {
    let mut port = DEFAULT_PORT;
    let mut i = 0;
    while i < args.len() {
        let arg = args[i].as_str();
        let flag = arg.trim_start_matches('-');
        match flag {
            "version" => {
                println!("{VERSION}");
                return Err(0);
            }
            "commit" => {
                println!("{COMMIT}");
                return Err(0);
            }
            "help" | "h" => {
                eprintln!("Usage of cube-envd:\n  -port uint\n        port on which the daemon should run (default {DEFAULT_PORT})\n  -isnotfc\n        accepted and ignored (compatibility with upstream envd)\n  -version\n        print the version\n  -commit\n        print the build commit");
                return Err(0);
            }
            "isnotfc" => {}
            "port" => {
                i += 1;
                let val = args.get(i).ok_or_else(|| {
                    eprintln!("flag needs an argument: -port");
                    2
                })?;
                port = val.parse().map_err(|_| {
                    eprintln!("invalid value {val:?} for flag -port");
                    2
                })?;
            }
            _ if flag.starts_with("port=") => {
                let val = &flag["port=".len()..];
                port = val.parse().map_err(|_| {
                    eprintln!("invalid value {val:?} for flag -port");
                    2
                })?;
            }
            other => {
                eprintln!("cube-envd: ignoring unknown flag: -{other}");
                // Skip a possible value belonging to the unknown flag.
                if let Some(next) = args.get(i + 1) {
                    if !next.starts_with('-') {
                        i += 1;
                    }
                }
            }
        }
        i += 1;
    }
    Ok(Cli { port })
}

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
        .enable_all()
        .build()
        .expect("build tokio runtime");

    runtime.block_on(async move {
        let state = Arc::new(state::AppState::new());
        let app = server::router(state);
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cli_defaults_and_port() {
        assert_eq!(parse_cli(&[]).map(|c| c.port).unwrap(), DEFAULT_PORT);
        let args: Vec<String> = ["-port", "8080", "-isnotfc"]
            .iter()
            .map(|s| s.to_string())
            .collect();
        assert_eq!(parse_cli(&args).map(|c| c.port).unwrap(), 8080);
        let args: Vec<String> = ["--port", "9000"].iter().map(|s| s.to_string()).collect();
        assert_eq!(parse_cli(&args).map(|c| c.port).unwrap(), 9000);
    }

    #[test]
    fn cli_unknown_flags_ignored() {
        let args: Vec<String> = ["-cgroup-root", "/sys/fs/cgroup", "-port", "7000"]
            .iter()
            .map(|s| s.to_string())
            .collect();
        assert_eq!(parse_cli(&args).map(|c| c.port).unwrap(), 7000);
    }
}

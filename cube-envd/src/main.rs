// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use cube_envd::cli::{Cli, Command};
use cube_envd::server::{run_server_with_runtime, ServerConfig, ServerRuntime};
use cube_envd::telemetry;
use tracing::info;

#[tokio::main]
async fn main() {
    let cli = Cli::parse();

    if let Err(err) = run(cli).await {
        eprintln!("{err}");
        std::process::exit(1);
    }
}

async fn run(cli: Cli) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    match cli.command {
        Command::Version => {
            println!("{}", cube_envd::COMPATIBILITY_VERSION);
            Ok(())
        }
        Command::CubeVersion => {
            println!("{}", cube_envd::PRODUCT_VERSION);
            Ok(())
        }
        Command::Commit => {
            println!("{}", cube_envd::REVISION);
            Ok(())
        }
        Command::Serve(config) => {
            telemetry::init(config.log_format);
            if let Err(error) = cube_envd::init::write_sandbox_marker(!config.is_not_fc) {
                tracing::warn!(%error, "could not write sandbox marker");
            }
            info!(
                product_version = cube_envd::PRODUCT_VERSION,
                compatibility_version = cube_envd::COMPATIBILITY_VERSION,
                revision = cube_envd::REVISION,
                provider = cube_envd::PROVIDER,
                port = config.port,
                "starting cube-envd"
            );
            run_server_with_runtime(
                ServerConfig {
                    port: u16::try_from(config.port)?,
                    is_not_fc: config.is_not_fc,
                },
                ServerRuntime::default()
                    .with_start_command(config.start_cmd)
                    .with_cgroup_root(config.cgroup_root),
            )
            .await
            .map_err(Into::into)
        }
    }
}

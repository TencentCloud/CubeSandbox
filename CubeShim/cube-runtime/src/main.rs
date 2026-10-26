// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use anyhow::Result;

use clap::Parser;
use containerd_shim_cube_rs::snapshot;
use cube_runtime::{
    completions, login,
    parser::{CliArgs, SubCommands},
};
use tokio::runtime::Builder;

const TOKIO_THREAD_NUM: usize = 1;

fn main() {
    let args = CliArgs::parse();
    let runtime = Builder::new_multi_thread()
        .worker_threads(TOKIO_THREAD_NUM)
        .enable_all()
        .build()
        .unwrap();

    if let Err(e) = runtime.block_on(execute(args)) {
        println!("error: {}", e);
        std::process::exit(-1);
    }
    drop(runtime);
}

async fn execute(args: CliArgs) -> Result<()> {
    match args.command {
        SubCommands::Snapshot(snapshot_args) => {
            snapshot::cmd::execute(snapshot_args).await?;
        }

        SubCommands::Login(login_args) => {
            login::execute(login_args).await?;
        }

        SubCommands::Completions(completions_args) => {
            completions::generate_completions(completions_args)?;
        }
    }
    Ok(())
}

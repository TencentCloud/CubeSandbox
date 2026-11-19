// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use clap::{Args, Parser, Subcommand};
use containerd_shim_cube_rs::snapshot::cmd::SnapshotArgs;
use containerd_shim_cube_rs::snapshot::verify::VerifyRestoreArgs;

#[derive(Parser, Debug)]
#[command(version = env!("CUBE_VERSION_FULL"), about, long_about = None)]
pub struct CliArgs {
    #[command(subcommand)]
    pub command: SubCommands,
}
#[derive(Subcommand, Debug)]
pub enum SubCommands {
    /// snapshot command
    #[clap(name = "snapshot", about = "snapshot command")]
    Snapshot(SnapshotArgs),
    /// Restore a pause package into an isolated throwaway VM and check its agent
    #[clap(
        name = "verify-restore",
        about = "Restore a pause package into an isolated throwaway VM and check its agent"
    )]
    VerifyRestore(VerifyRestoreArgs),
    /// login command
    #[clap(name = "login", about = "Enter the guest by debug console")]
    Login(LoginArgs),
    /// Generate shell completions
    #[clap(name = "completions", about = "Generate shell completions")]
    Completions(CompletionsArgs),
}

#[derive(Args, Debug)]
pub struct CompletionsArgs {}

#[derive(Args, Debug)]
pub struct LoginArgs {
    /// Sandbox ID (required)
    #[clap(required = true)]
    pub sandbox_id: String,

    /// Port that debug console is listening on.
    #[clap(short, long, default_value_t = 1026)]
    pub port: u32,

    /// Timeout for the connection to the debug console (in seconds)
    #[clap(short, long, default_value_t = 10)]
    pub timeout: u32,
}

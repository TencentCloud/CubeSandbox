// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use clap::{Args, Parser, Subcommand};
use containerd_shim_cube_rs::snapshot::cmd::SnapshotArgs;

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
    /// Resume a VM left paused after an app snapshot
    #[clap(name = "snapshot-resume", about = "resume a VM after an app snapshot")]
    SnapshotResume(SnapshotResumeArgs),
    /// login command
    #[clap(name = "login", about = "Enter the guest by debug console")]
    Login(LoginArgs),
    /// Generate shell completions
    #[clap(name = "completions", about = "Generate shell completions")]
    Completions(CompletionsArgs),
}

#[derive(Args, Debug)]
pub struct SnapshotResumeArgs {
    /// VM id
    #[arg(long = "vm-id", value_name = "vm id", required = true)]
    pub vm_id: String,
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_snapshot_resume() {
        let args =
            CliArgs::try_parse_from(["cube-runtime", "snapshot-resume", "--vm-id", "sandbox-1"])
                .unwrap();

        match args.command {
            SubCommands::SnapshotResume(args) => assert_eq!(args.vm_id, "sandbox-1"),
            command => panic!("unexpected command: {command:?}"),
        }
    }

    #[test]
    fn snapshot_keep_paused_is_opt_in() {
        let args = CliArgs::try_parse_from([
            "cube-runtime",
            "snapshot",
            "--path",
            "/tmp/snapshot",
            "--disk",
            "[]",
            "--resource",
            "{}",
            "--pmem",
            "[]",
            "--kernel",
            "/kernel",
            "--app-snapshot",
            "--vm-id",
            "sandbox-1",
            "--keep-paused",
        ])
        .unwrap();

        match args.command {
            SubCommands::Snapshot(args) => assert!(args.keep_paused),
            command => panic!("unexpected command: {command:?}"),
        }
    }
}

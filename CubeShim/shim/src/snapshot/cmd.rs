// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use anyhow::{anyhow, Result};
use clap::{ArgAction, Args};
use cube_hypervisor::SnapshotType;
use uuid::Uuid;

use crate::{
    common::utils::Utils,
    sandbox::{config::VmResource, disk::Disk, pmem::Pmem},
};

use super::Snapshot;

#[derive(Args, Debug)]
pub struct SnapshotArgs {
    /// Target path
    #[arg(
        long = "path",
        value_name = "target path",
        help = "target path",
        required = true
    )]
    pub path: String,

    /// Disk info
    #[arg(
        long = "disk",
        value_name = "disk info",
        help = "disk info",
        required = true
    )]
    pub disk: String,

    /// Resource info
    #[arg(
        long = "resource",
        value_name = "resource {}",
        help = "resource info",
        required = true
    )]
    pub resource: String,

    /// PMEM path
    #[arg(
        long = "pmem",
        value_name = "pmem path",
        help = "pmem path",
        required = true
    )]
    pub pmem: String,

    /// Kernel path
    #[arg(
        long = "kernel",
        value_name = "kernel path",
        help = "kernel path",
        required = true
    )]
    pub kernel: String,

    /// Don't create tap
    #[arg(long = "notap", help = "don't create tap", action = ArgAction::SetTrue, required = false)]
    pub notap: bool,

    /// Force
    #[arg(long = "force", help = "force", action = ArgAction::SetTrue, required = false)]
    pub force: bool,

    /// App snapshot
    #[arg(long = "app-snapshot", help = "app-snapshot", action = ArgAction::SetTrue, required = false)]
    pub app_snapshot: bool,

    /// Vm id
    #[arg(
        long = "vm-id",
        value_name = "vm id",
        help = "vm id",
        required_if_eq("app_snapshot", "true")
    )]
    pub vm_id: Option<String>,

    /// Snapshot type: 'full', 'incremental' (saves only CoW anonymous pages,
    /// cumulative since restore) or 'soft-dirty' (true delta of pages
    /// dirtied since the previous soft-dirty snapshot; falls back to
    /// 'incremental' on kernels without CONFIG_MEM_SOFT_DIRTY).
    #[arg(
        long = "snapshot-type",
        value_name = "snapshot type",
        help = "snapshot type: 'full', 'incremental' or 'soft-dirty'",
        default_value = "full"
    )]
    pub snapshot_type: String,

    /// Optional existing path or file URL for storing memory range data on a
    /// separate volume.
    /// When set, memory snapshot data is written to this location instead of
    /// the default <path>/snapshot/memory-ranges.
    #[arg(
        long = "memory-vol",
        value_name = "memory vol path",
        help = "optional existing path or file URL for storing memory data on a separate volume (e.g. /dev/vdb or file:///dev/vdb)",
        required = false
    )]
    pub memory_vol: Option<String>,

    #[arg(
        long = "container-id",
        value_name = "container id",
        help = "logical container id persisted in app snapshot metadata",
        required = false
    )]
    pub container_id: Option<String>,
}

pub async fn execute(args: SnapshotArgs) -> Result<()> {
    let mut snapshot =
        Snapshot::try_from(args).map_err(|e| anyhow!("failed to create snapshot: {}", e))?;
    println!("debuginfo force:{}, tap:{}", snapshot.force, snapshot.tap);
    snapshot
        .handle()
        .await
        .map_err(|e| anyhow!("failed to handle snapshot: {}", e))?;
    println!("snapshot success");
    Ok(())
}

impl TryFrom<SnapshotArgs> for Snapshot {
    type Error = String;

    fn try_from(args: SnapshotArgs) -> std::result::Result<Self, Self::Error> {
        let mut snapshot = Snapshot::new();
        snapshot.id = Uuid::new_v4().to_string();
        println!("InstanceId: {}", snapshot.id);
        snapshot.res = Utils::anno_to_obj::<VmResource>(&args.resource)?;
        snapshot.disk = Utils::anno_to_obj::<Vec<Disk>>(&args.disk)?;
        snapshot.pmem = Utils::anno_to_obj::<Vec<Pmem>>(&args.pmem)?;
        snapshot.path = args.path;
        snapshot.kernel = args.kernel;
        snapshot.tap = !args.notap;
        snapshot.force = args.force;
        snapshot.app_snapshot = args.app_snapshot;
        snapshot.snapshot_type = args
            .snapshot_type
            .parse::<SnapshotType>()
            .map_err(|e| format!("invalid snapshot type: {}", e))?;
        snapshot.memory_vol_url = args.memory_vol;
        snapshot.container_id = args
            .container_id
            .map(|value| value.trim().to_string())
            .filter(|value| !value.is_empty());
        if args.app_snapshot {
            if args.vm_id.is_none() {
                return Err("not specify the vmid in app snapshot mode".to_string());
            }
            snapshot.id = args.vm_id.unwrap();
        }
        println!("InstanceId: {}", snapshot.id);
        Ok(snapshot)
    }
}

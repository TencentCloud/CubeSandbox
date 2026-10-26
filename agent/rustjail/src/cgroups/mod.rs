// Copyright (c) 2019,2020 Ant Financial
//
// SPDX-License-Identifier: Apache-2.0
//

use anyhow::{anyhow, Result};
use oci::LinuxResources;
use protocols::agent::CgroupStats;

use cgroups::freezer::FreezerState;

pub mod fs;
pub mod mock;
pub mod notifier;
pub mod systemd;

pub const RESOURCE_METRICS_VERSION_V1: u32 = 1;

pub trait Manager {
    fn apply(&self, _pid: i32) -> Result<()> {
        Err(anyhow!("not supported!".to_string()))
    }

    fn get_pids(&self) -> Result<Vec<i32>> {
        Err(anyhow!("not supported!"))
    }

    fn get_stats(&self) -> Result<CgroupStats> {
        Err(anyhow!("not supported!"))
    }

    fn resource_metrics_version(&self) -> u32 {
        0
    }

    fn freeze(&self, _state: FreezerState) -> Result<()> {
        Err(anyhow!("not supported!"))
    }

    fn destroy(&mut self) -> Result<()> {
        Err(anyhow!("not supported!"))
    }

    fn set(&self, _container: &LinuxResources, _update: bool) -> Result<()> {
        Err(anyhow!("not supported!"))
    }
}

#[cfg(test)]
mod tests {
    use super::Manager;

    struct UnsupportedManager;

    impl Manager for UnsupportedManager {}

    #[test]
    fn managers_must_explicitly_claim_resource_metrics_support() {
        assert_eq!(UnsupportedManager.resource_metrics_version(), 0);
    }
}

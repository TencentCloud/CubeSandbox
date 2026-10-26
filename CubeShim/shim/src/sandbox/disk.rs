// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::common::utils::CPath;
use cube_hypervisor::config::RateLimiterConfig;
use serde::{Deserialize, Serialize};

pub const ANNO_DISK: &str = "cube.disk";
const GUEST_MOUNT_DIR_PREFIX: &str = "/run/blk-cube/vd";
pub const MNT_OPT_SRC: &str = "blk-cube-source";
const FS_TYPE_EXT4: &str = "ext4";

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub struct Disk {
    pub path: String,
    #[serde(default)]
    pub source_dir: String,
    pub fs_type: String,
    pub size: u64,
    #[serde(default)]
    pub fs_quota: u64,
    pub rate_limiter_config: Option<RateLimiterConfig>,
}

impl Disk {
    // pmem0 is image
    pub fn guest_device_name(i: u32) -> String {
        format!("vd{}", ('a' as u32 + i) as u8 as char)
    }

    pub fn guest_device_path(i: u32) -> String {
        format!("/dev/vd{}", ('a' as u32 + i) as u8 as char)
    }

    pub fn guest_mount_point(i: u32) -> String {
        format!(
            "{}{}",
            GUEST_MOUNT_DIR_PREFIX,
            ('a' as u32 + i) as u8 as char
        )
    }

    pub fn guest_bind_source(&self, i: u32, options: &Option<Vec<String>>) -> String {
        self.guest_bind_source_with_subdir(i, options, self.source_dir.clone())
    }

    pub fn guest_bind_source_with_subdir(
        &self,
        i: u32,
        options: &Option<Vec<String>>,
        mut subdir: String,
    ) -> String {
        let base = format!(
            "{}{}",
            GUEST_MOUNT_DIR_PREFIX,
            ('a' as u32 + i) as u8 as char
        );
        let mut src = CPath::new(base.as_str());

        if let Some(opts) = options {
            let target = format!("{}=", MNT_OPT_SRC);
            for opt in opts.iter() {
                if opt.starts_with(target.as_str()) {
                    subdir = opt[target.len()..].to_string();
                    break;
                }
            }
        }
        src.join(subdir.as_str());
        src.to_str()
            .unwrap_or_else(|| panic!("Invalid path string:{:?}", src))
            .to_string()
    }

    pub fn driver() -> String {
        "blk-cube".to_string()
    }

    pub fn driver_opt(&self) -> Vec<String> {
        let mut opt = Vec::new();
        if self.fs_quota != 0 {
            opt.push(format!("ext4-quota={}", self.fs_quota / 1024 / 1024));
        }
        opt
    }

    pub fn opt(&self) -> Vec<String> {
        if self.fs_type == FS_TYPE_EXT4 {
            return vec!["barrier=0".to_string()];
        }
        vec![]
    }
}

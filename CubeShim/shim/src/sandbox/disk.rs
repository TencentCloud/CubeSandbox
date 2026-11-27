// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::common::utils::CPath;
use crate::common::CResult;
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
    #[serde(default)]
    pub discard: bool,
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
        if self.fs_type != FS_TYPE_EXT4 {
            return vec![];
        }
        let mut opt = vec!["barrier=0".to_string()];
        if self.discard {
            opt.push("discard".to_string());
        }
        opt
    }

    pub fn validate(&self) -> CResult<()> {
        if self.discard && self.fs_type != FS_TYPE_EXT4 {
            return Err(format!(
                "discard is only supported for ext4 disks, got fs_type:{} path:{}",
                self.fs_type, self.path
            ));
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn disk_from_json(discard: Option<bool>, fs_type: &str) -> Disk {
        let mut value = serde_json::json!({
            "path": "/dev/vdb",
            "fs_type": fs_type,
            "size": 1024u64,
            "fs_quota": 128 * 1024 * 1024u64,
        });
        if let Some(discard) = discard {
            value["discard"] = serde_json::Value::Bool(discard);
        }
        serde_json::from_value(value).expect("disk json should deserialize")
    }

    #[test]
    fn disk_opt_without_discard_is_barrier_only() {
        let missing = disk_from_json(None, FS_TYPE_EXT4);
        assert_eq!(missing.opt(), vec!["barrier=0".to_string()]);

        let disabled = disk_from_json(Some(false), FS_TYPE_EXT4);
        assert_eq!(disabled.opt(), vec!["barrier=0".to_string()]);
    }

    #[test]
    fn disk_opt_with_discard_appends_once_and_keeps_fsquota() {
        let plain = disk_from_json(Some(false), FS_TYPE_EXT4);
        let enabled = disk_from_json(Some(true), FS_TYPE_EXT4);

        assert_eq!(
            enabled.opt(),
            vec!["barrier=0".to_string(), "discard".to_string()]
        );
        assert_eq!(
            enabled
                .opt()
                .iter()
                .filter(|option| option.as_str() == "discard")
                .count(),
            1
        );
        assert_eq!(enabled.driver_opt(), plain.driver_opt());
        assert_eq!(enabled.driver_opt(), vec!["ext4-quota=128".to_string()]);
    }
}

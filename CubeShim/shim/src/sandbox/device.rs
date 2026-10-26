// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use serde::Deserialize;
use serde::Serialize;

use crate::common::utils::CPath;

use super::disk::MNT_OPT_SRC;
pub const ANNO_VFIO_DISK: &str = "cube.vfio.disk";
pub const ANNO_VFIO_DISK_RM: &str = "cube.vfio.disk.rm";

pub const ANNO_VFIO_NET: &str = "cube.vfio.net";

const GUEST_PCI_MOUNT_DIR_PREFIX: &str = "/run/cube-containers/sandbox/blk-cube/";
#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub struct Device {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub bdf: String,
    #[serde(default)]
    pub sysfs_dev: String,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub struct DeviceDisk {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub bdf: String,
    #[serde(default)]
    pub sysfs_dev: String,
    #[serde(default)]
    pub guest_pci: String,
    #[serde(default)]
    pub platform: bool,
    #[serde(default)]
    pub fs_type: String,
    #[serde(default)]
    pub source_dir: String,
    /// 磁盘是否需要被格式化
    #[serde(default)]
    pub need_format: bool,

    /// 磁盘的配额大小，单位为字节
    #[serde(default)]
    pub fs_quota: u64,

    #[serde(default)]
    pub need_resize: bool,
}

impl DeviceDisk {
    pub fn get_mount_point(&self) -> String {
        format!("{}{}", GUEST_PCI_MOUNT_DIR_PREFIX, self.guest_pci.clone())
    }

    pub fn guest_pci_source(&self, options: &Option<Vec<String>>) -> String {
        self.guest_pci_source_with_subdir(options, self.source_dir.clone())
    }

    pub fn guest_pci_source_with_subdir(
        &self,
        options: &Option<Vec<String>>,
        mut subdir: String,
    ) -> String {
        let mut src = CPath::new(self.get_mount_point().as_str());

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
        src.join(self.source_dir.as_str());
        src.to_str()
            .unwrap_or_else(|| panic!("Invalid path string:{:?}", src))
            .to_string()
    }

    pub fn driver_opt(&self) -> Vec<String> {
        let mut opt = Vec::new();
        if self.fs_quota != 0 {
            opt.push(format!("ext4-quota={}", self.fs_quota / 1024 / 1024));
        }
        opt
    }
}

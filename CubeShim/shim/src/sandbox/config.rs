// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::common::utils::Utils;
use crate::common::CResult;
use crate::common::PRODUCT_CUBEBOX;
use crate::sandbox::disk::{Disk, ANNO_DISK};
use crate::sandbox::net::{Net, ANNO_NET};
use crate::sandbox::pmem::{Pmem, ANNO_PMEM};
use cube_hypervisor::config::{BackendFsConfig, RateLimiterConfig};

use serde::{Deserialize, Serialize};
use std::collections::HashMap;

use super::device::{self, Device, DeviceDisk};

pub const ANNO_VM_RES: &str = "cube.vmmres";
pub const ANNO_VMM_FS: &str = "cube.fs";
pub const ANNO_VIRTIOFS: &str = "cube.virtiofs";
pub const ANNO_CUBE_VIPS: &str = "cube.net.vips";
pub const ANNO_SNAPSHOT_DISABLE: &str = "cube.snapshot.disable";
pub const ANNO_SNAPSHOT_NOTIFY: &str = "cube.snapshot.healthcheck";
pub const ANNO_VM_KERNEL: &str = "cube.vm.kernel.path";
/// Annotation key used to append extra kernel cmdline parameters.
pub const ANNO_VM_KERNEL_CMDLINE_APPEND: &str = "cube.vm.kernel.cmdline.append";
pub const ANNO_SNAPSHOT_BASE: &str = "cube.vm.snapshot.base.path";
pub const ANNO_SNAPSHOT_MEMORY_VOL_URL: &str = "cube.vm.snapshot.memory_vol_url";
pub const ANNO_APP_SNAPSHOT_CREATE: &str = "cube.appsnapshot.create";
pub const ANNO_APP_SNAPSHOT_RESTORE: &str = "cube.appsnapshot.restore";

pub const SHARE_CACHE_ALWAYS: u8 = 1;
pub const SHARE_CACHE_NEVER: u8 = 2;

pub use crate::hypervisor::config::{VIRTIO_FS_ID, VIRTIO_FS_TAG};

const KERNEL_SCF: &str = "/usr/local/services/cubetoolbox/cube-kernel-scf/vmlinux";

#[derive(Clone, Default)]
pub struct Config {
    pub net: Net,
    pub disk: Vec<Disk>,
    pub disk_path_map: HashMap<String, u32>,
    pub pmem: Vec<Pmem>,
    pub pmem_path_map: HashMap<String, u32>,
    pub vm_res: VmResource,
    pub kernel: String,
    pub snapshot_base: String,
    pub snapshot_memory_vol_url: Option<String>,
    pub fs: Option<Fs>,
    pub virtiofs: Vec<VirtioFs>,
    pub vips: String,
    pub product: String,
    pub snapshot: bool,
    pub vfio_nets: Vec<Device>,
    pub vfio_disks: Vec<DeviceDisk>,
    pub vfio_disk_path_map: HashMap<String, u32>,
    pub notify_snapshot_ret: bool,
    pub app_snapshot_create: bool,
    pub app_snapshot_restore: bool,
    /// Extra kernel cmdline parameters injected through annotations.
    pub extra_kernel_params: Vec<String>,
}

impl Config {
    pub fn new(annotation: &Option<HashMap<String, String>>) -> CResult<Self> {
        if annotation.is_none() {
            return Err("Spec.annotation is None".to_string());
        }
        let anno = annotation.as_ref().unwrap();

        let net = {
            if let Some(opt_net) = anno.get(ANNO_NET) {
                Utils::anno_to_obj::<Net>(opt_net)?
            } else {
                Net::default()
            }
        };
        let mut disk_path_map: HashMap<String, u32> = HashMap::new();
        let mut vfio_disk_path_map: HashMap<String, u32> = HashMap::new();
        let mut disk = Vec::new();
        if let Some(opt_disk) = anno.get(ANNO_DISK) {
            disk = Utils::anno_to_obj::<Vec<Disk>>(opt_disk)?;
            for (i, d) in disk.iter().enumerate() {
                disk_path_map.insert(d.path.clone(), i as u32);
            }
        }

        let mut pmem_path_map: HashMap<String, u32> = HashMap::new();
        let mut pmem = Vec::new();
        if let Some(opt_pmem) = anno.get(ANNO_PMEM) {
            pmem = Utils::anno_to_obj::<Vec<Pmem>>(opt_pmem)?;

            for (i, p) in pmem.iter().enumerate() {
                pmem_path_map.insert(p.file.clone(), i as u32);
            }
        }

        let vm_res = {
            if let Some(opt_vm_res) = anno.get(ANNO_VM_RES) {
                Utils::anno_to_obj::<VmResource>(opt_vm_res)?
            } else {
                return Err("Not found annotation:cube.vmmres".to_string());
            }
        };

        let prod = PRODUCT_CUBEBOX.to_string();
        let mut kernel = KERNEL_SCF.to_string();

        if let Some(kernel_path) = anno.get(ANNO_VM_KERNEL) {
            kernel = kernel_path.clone();
        }

        let snapshot_base = Utils::get_snapshot_base_dir(
            anno.get(ANNO_SNAPSHOT_BASE).map(|x| x.as_str()),
            prod.as_str(),
        );
        let snapshot_memory_vol_url = anno
            .get(ANNO_SNAPSHOT_MEMORY_VOL_URL)
            .map(|x| x.trim().to_string())
            .filter(|x| !x.is_empty());

        let fs = {
            if let Some(opt_fs) = anno.get(ANNO_VMM_FS) {
                let fs = Utils::anno_to_obj::<Fs>(opt_fs)?;
                Some(fs)
            } else {
                None
            }
        };

        let mut cube_vips = String::new();
        if let Some(v) = anno.get(ANNO_CUBE_VIPS) {
            cube_vips = v.clone();
        }

        let mut vfio_nets = Vec::new();
        if let Some(net) = anno.get(device::ANNO_VFIO_NET) {
            vfio_nets = Utils::anno_to_obj::<Vec<Device>>(net)?;
        }

        let mut vfio_disks = Vec::new();
        if let Some(disk) = anno.get(device::ANNO_VFIO_DISK) {
            vfio_disks = Utils::anno_to_obj::<Vec<DeviceDisk>>(disk)?;
            for (i, d) in vfio_disks.iter().enumerate() {
                vfio_disk_path_map.insert(d.sysfs_dev.clone(), i as u32);
            }
        }

        let mut notify_snapshot_ret = false;
        if anno.get(ANNO_SNAPSHOT_NOTIFY).is_some() {
            notify_snapshot_ret = true;
        }

        let mut app_snapshot_create = false;
        if anno.get(ANNO_APP_SNAPSHOT_CREATE).is_some() {
            app_snapshot_create = true;
        }

        let mut app_snapshot_restore = false;
        if anno.get(ANNO_APP_SNAPSHOT_RESTORE).is_some() {
            app_snapshot_restore = true;
        }

        if app_snapshot_restore && app_snapshot_create {
            return Err(format!(
                "{} conflicts with {}",
                ANNO_APP_SNAPSHOT_RESTORE, ANNO_APP_SNAPSHOT_CREATE
            ));
        }

        let mut virtiofs = Vec::new();
        if let Some(anno) = anno.get(ANNO_VIRTIOFS) {
            virtiofs = Utils::anno_to_obj::<Vec<VirtioFs>>(anno)?;
        }
        let extra_kernel_params = if let Some(params) = anno.get(ANNO_VM_KERNEL_CMDLINE_APPEND) {
            let params_vec = Utils::anno_to_obj::<Vec<String>>(params)?;
            params_vec
                .into_iter()
                .filter_map(|param| {
                    let trimmed = param.trim();
                    if trimmed.is_empty() {
                        None
                    } else {
                        Some(trimmed.to_string())
                    }
                })
                .collect()
        } else {
            Vec::new()
        };

        let c = Config {
            net,
            disk,
            disk_path_map,
            pmem,
            pmem_path_map,
            vm_res,
            kernel,
            snapshot_base,
            snapshot_memory_vol_url,
            fs,
            virtiofs,
            vips: cube_vips,
            product: prod,
            snapshot: false,
            vfio_nets,
            vfio_disks,
            vfio_disk_path_map,
            notify_snapshot_ret,
            app_snapshot_create,
            app_snapshot_restore,
            extra_kernel_params,
        };
        Ok(c)
    }
}

#[derive(Clone, Debug, Serialize, Deserialize, Default)]
pub struct VmResource {
    #[serde(default)]
    pub cpu: u32,
    #[serde(default)]
    pub memory: u64,
    #[serde(default)]
    pub preserve_memory: u64,
    #[serde(default)]
    pub snap_memory: u64,
}

#[derive(Clone, Debug, Serialize, Deserialize, Default)]
pub struct Fs {
    pub backendfs_config: Option<BackendFsConfig>,
    pub rate_limiter_config: Option<RateLimiterConfig>,
}

#[derive(Clone, Debug, Serialize, Deserialize, Default)]
pub struct VirtioFs {
    pub id: String,
    #[serde(default)]
    pub propagation_mount_name: String,
    pub backendfs_config: Option<BackendFsConfig>,
    pub rate_limiter_config: Option<RateLimiterConfig>,
}

#[cfg(test)]
mod tests {
    use std::collections::HashMap;

    use crate::common::utils::Utils;
    use crate::common::PRODUCT_CUBEBOX;
    use crate::sandbox::config::Config;
    use crate::sandbox::config::ANNO_SNAPSHOT_BASE;
    use crate::sandbox::config::ANNO_SNAPSHOT_MEMORY_VOL_URL;
    use crate::sandbox::config::ANNO_VM_KERNEL;
    use crate::sandbox::config::ANNO_VM_KERNEL_CMDLINE_APPEND;
    use crate::sandbox::config::ANNO_VM_RES;
    use crate::sandbox::config::KERNEL_SCF;

    #[test]
    fn utils_config_new() {
        let ret = Config::new(&None);
        assert!(ret.is_err());

        let mut annotations = HashMap::<String, String>::new();

        // res
        let res = r#"{"cpu": 1, "memory": 2048, "preserve_memory": 2048, "snap_memory": 2048}"#;
        annotations.insert(ANNO_VM_RES.to_string(), res.to_string());
        let ret = Config::new(&Some(annotations.clone()));
        assert!(ret.is_ok());
        let config = ret.unwrap();

        assert_eq!(config.vm_res.cpu, 1);
        assert_eq!(config.vm_res.memory, 2048);
        assert_eq!(config.vm_res.preserve_memory, 2048);
        assert_eq!(config.vm_res.snap_memory, 2048);

        // product,kernel,snapshot base dir (always CUBEBOX)
        assert_eq!(config.product, PRODUCT_CUBEBOX);
        assert_eq!(config.kernel, KERNEL_SCF);
        assert_eq!(
            config.snapshot_base,
            Utils::get_snapshot_base_dir(None, PRODUCT_CUBEBOX)
        );
        assert_eq!(config.snapshot_memory_vol_url, None);

        annotations.insert(ANNO_SNAPSHOT_BASE.to_string(), "/1/2/3".to_string());
        let ret = Config::new(&Some(annotations.clone()));
        assert!(ret.is_ok());
        let config = ret.unwrap();
        assert_eq!(config.product, PRODUCT_CUBEBOX);
        assert_eq!(
            config.snapshot_base,
            Utils::get_snapshot_base_dir(Some("/1/2/3"), PRODUCT_CUBEBOX)
        );

        annotations.insert(
            ANNO_SNAPSHOT_MEMORY_VOL_URL.to_string(),
            "file:///dev/dm-29".to_string(),
        );
        let ret = Config::new(&Some(annotations.clone()));
        assert!(ret.is_ok());
        let config = ret.unwrap();
        assert_eq!(
            config.snapshot_memory_vol_url,
            Some("file:///dev/dm-29".to_string())
        );

        //vm kernel path
        annotations.insert(ANNO_VM_KERNEL.to_string(), "/1/2/3".to_string());
        let ret = Config::new(&Some(annotations.clone()));
        assert!(ret.is_ok());
        let config = ret.unwrap();
        assert_eq!(config.kernel, "/1/2/3".to_string());

        let extra_cmdlines = r#"["foo=bar","  second=2  ",""]"#;
        annotations.insert(
            ANNO_VM_KERNEL_CMDLINE_APPEND.to_string(),
            extra_cmdlines.to_string(),
        );
        let ret = Config::new(&Some(annotations.clone()));
        assert!(ret.is_ok());
        let config = ret.unwrap();
        assert_eq!(
            config.extra_kernel_params,
            vec!["foo=bar".to_string(), "second=2".to_string()]
        );
    }

    #[test]
    fn test_extra_kernel_params() {
        let mut annotations = HashMap::<String, String>::new();
        let res = r#"{"cpu": 1, "memory": 2048, "preserve_memory": 2048, "snap_memory": 2048}"#;
        annotations.insert(ANNO_VM_RES.to_string(), res.to_string());

        // Test 1: No annotation - should default to empty vec
        let ret = Config::new(&Some(annotations.clone()));
        assert!(ret.is_ok());
        let config = ret.unwrap();
        assert_eq!(config.extra_kernel_params, Vec::<String>::new());

        // Test 2: Empty array
        annotations.insert(
            ANNO_VM_KERNEL_CMDLINE_APPEND.to_string(),
            r#"[]"#.to_string(),
        );
        let ret = Config::new(&Some(annotations.clone()));
        assert!(ret.is_ok());
        let config = ret.unwrap();
        assert_eq!(config.extra_kernel_params, Vec::<String>::new());

        // Test 3: Array with only empty strings
        annotations.insert(
            ANNO_VM_KERNEL_CMDLINE_APPEND.to_string(),
            r#"["", "  ", "\t"]"#.to_string(),
        );
        let ret = Config::new(&Some(annotations.clone()));
        assert!(ret.is_ok());
        let config = ret.unwrap();
        assert_eq!(config.extra_kernel_params, Vec::<String>::new());

        // Test 4: Array with normal params
        annotations.insert(
            ANNO_VM_KERNEL_CMDLINE_APPEND.to_string(),
            r#"["param1=value1", "param2=value2"]"#.to_string(),
        );
        let ret = Config::new(&Some(annotations.clone()));
        assert!(ret.is_ok());
        let config = ret.unwrap();
        assert_eq!(
            config.extra_kernel_params,
            vec!["param1=value1".to_string(), "param2=value2".to_string()]
        );

        // Test 5: Array with params that have leading/trailing whitespace
        annotations.insert(
            ANNO_VM_KERNEL_CMDLINE_APPEND.to_string(),
            r#"["  param1=value1  ", "  param2=value2", "param3=value3  "]"#.to_string(),
        );
        let ret = Config::new(&Some(annotations.clone()));
        assert!(ret.is_ok());
        let config = ret.unwrap();
        assert_eq!(
            config.extra_kernel_params,
            vec![
                "param1=value1".to_string(),
                "param2=value2".to_string(),
                "param3=value3".to_string()
            ]
        );

        // Test 6: Mixed valid and empty/whitespace params
        annotations.insert(
            ANNO_VM_KERNEL_CMDLINE_APPEND.to_string(),
            r#"["valid1=value1", "", "  valid2=value2  ", "   ", "valid3=value3"]"#.to_string(),
        );
        let ret = Config::new(&Some(annotations.clone()));
        assert!(ret.is_ok());
        let config = ret.unwrap();
        assert_eq!(
            config.extra_kernel_params,
            vec![
                "valid1=value1".to_string(),
                "valid2=value2".to_string(),
                "valid3=value3".to_string()
            ]
        );

        // Test 7: Invalid JSON - should error
        annotations.insert(
            ANNO_VM_KERNEL_CMDLINE_APPEND.to_string(),
            r#"["invalid json"#.to_string(),
        );
        let ret = Config::new(&Some(annotations.clone()));
        assert!(ret.is_err());

        // Test 8: Single param with whitespace
        annotations.insert(
            ANNO_VM_KERNEL_CMDLINE_APPEND.to_string(),
            r#"["  single=param  "]"#.to_string(),
        );
        let ret = Config::new(&Some(annotations.clone()));
        assert!(ret.is_ok());
        let config = ret.unwrap();
        assert_eq!(config.extra_kernel_params, vec!["single=param".to_string()]);
    }
}

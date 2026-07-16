// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::collections::HashSet;
use std::fmt;
use std::num::ParseIntError;
use std::path::PathBuf;
use std::str::FromStr;

use crate::common::utils::{self, Utils};
use crate::common::CResult;
use crate::sandbox::config::{Fs, VirtioFs};
use crate::sandbox::disk::Disk;
use crate::sandbox::net::Net;
use crate::sandbox::pmem::Pmem;

use cube_hypervisor::config::{RateLimiterConfig, TokenBucketConfig};
use cube_hypervisor::vm_config::{
    ConsoleConfig, ConsoleOutputMode, CpuTopology, DiskConfig, FsConfig, IvshmemConfig, MacAddr,
    NetConfig, PayloadConfig, PmemConfig, RngConfig, VmConfig as VC, VsockConfig,
};
use cube_hypervisor::vmm_config::VmmConfig;

use log::LevelFilter;
use serde::de::Visitor;
use serde::{Deserialize, Serialize};
const IMAGE_PATH: &str = "/usr/local/services/cubetoolbox/cube-image/cube-guest-image-cpu.img";

pub const VIRTIO_FS_TAG: &str = "cubeShared";
pub const VIRTIO_FS_ID: &str = "cube-fs";
const KI_B: u64 = 1 << 10;
const MI_B: u64 = KI_B << 10;
#[derive(Clone)]
pub struct HypConfig {
    pub debug: bool,
    pub log_level: LevelFilter,
    pub sandbox_id: String,
    pub ch_http_api: Option<String>,
}

impl HypConfig {
    pub fn to_vmm_config(&self) -> VmmConfig {
        VmmConfig {
            sandbox_id: self.sandbox_id.clone(),
            log_level: self.log_level,
            http_path: self.ch_http_api.clone(),
            ..Default::default()
        }
    }
}

#[derive(Clone, Debug)]
pub struct VmConfig {
    pub ivshmem: Option<IvshmemConfig>,
    pub vcpus: u32,
    pub memory_size: u64,
    pub dirty_log: bool,
    pub cmdlines: Vec<String>,
    pub kernel: String,
    pub disks: Option<Vec<DiskConfig>>,
    pub nets: Option<Vec<NetConfig>>,
    pub fss: Option<Vec<FsConfig>>,
    pub pmems: Option<Vec<PmemConfig>>,
    pub serial: ConsoleConfig,
    pub console: ConsoleConfig,
    pub vsock: Option<VsockConfig>,
    pub sys_ctrl: bool,
    pub rng: RngConfig,
}

impl Default for VmConfig {
    fn default() -> Self {
        let mut params = vec![
            "root=/dev/pmem0".to_string(),
            "rootflags=dax,errors=remount-ro ro".to_string(),
            "rootfstype=ext4".to_string(),
            "panic=1".to_string(),
        ];
        #[cfg(target_arch = "x86_64")]
        params.extend(["no_timer_check".to_string(), "noreplace-smp".to_string()]);
        params.extend(["printk.devkmsg=on".to_string()]);
        #[cfg(target_arch = "aarch64")]
        params.push("console=ttyAMA0,115200".to_string());
        #[cfg(not(target_arch = "aarch64"))]
        params.push("console=hvc0".to_string());
        params.extend([
            "net.ifnames=0".to_string(),
            "audit=0".to_string(),
            "LANG=C".to_string(),
            "raid=noautodetect".to_string(),
            "earlyprintk=ttyS0".to_string(),
            "agent.debug_console".to_string(),
            "agent.debug_console_vport=1026".to_string(),
            "mitigations=off".to_string(),
        ]);

        let pmems = vec![PmemConfig {
            file: PathBuf::from(IMAGE_PATH),
            discard_writes: true,
            ..Default::default()
        }];

        //console/serial
        let console = ConsoleConfig {
            mode: ConsoleOutputMode::Tty,
            ..Default::default()
        };
        VmConfig {
            ivshmem: None,
            vcpus: 0,
            memory_size: 0,
            dirty_log: false,
            cmdlines: params,
            kernel: String::default(),
            disks: Some(Vec::new()),
            nets: Some(Vec::new()),
            fss: Some(Vec::new()),
            pmems: Some(pmems),
            serial: console.clone(),
            console,
            vsock: None,
            sys_ctrl: false,
            rng: RngConfig {
                src: PathBuf::from("/dev/urandom"),
                iommu: false,
            },
        }
    }
}

impl VmConfig {
    pub fn to_vm_config(&self) -> VC {
        let mut vc = VC::default();

        vc.cpus.max_vcpus = self.vcpus as u8;
        vc.cpus.boot_vcpus = self.vcpus as u8;
        let topology = CpuTopology {
            threads_per_core: 1,
            cores_per_die: self.vcpus as u8,
            dies_per_package: 1,
            packages: 1,
        };
        vc.cpus.topology = Some(topology);

        vc.memory.size = self.memory_size * MI_B;
        vc.memory.dirty_log = self.dirty_log;

        let cmds = self.cmdlines.join(" ").to_string();
        let payload = PayloadConfig {
            cmdline: Some(cmds),
            kernel: Some(self.kernel.clone().into()),
            ..Default::default()
        };
        vc.payload = Some(payload);

        vc.disks = self.disks.clone();
        vc.net = self.nets.clone();
        vc.fs = self.fss.clone();
        vc.pmem = self.pmems.clone();

        let console = ConsoleConfig {
            mode: ConsoleOutputMode::Tty,
            ..Default::default()
        };

        //fixd in cube
        vc.serial = console.clone();
        vc.console = console;
        vc.sys_ctrl = true;
        if let Some(vs) = self.vsock.clone() {
            vc.vsock = Some(vs)
        }
        if let Some(ivshmem) = self.ivshmem.clone() {
            vc.ivshmem = Some(ivshmem)
        }
        vc
    }

    /// Enable ivshmem shared memory channel with a caller-supplied backend
    /// file path. The caller is responsible for creating and sizing the
    /// file before calling this. This keeps the shim agnostic of the
    /// backend naming convention.
    pub fn enable_ivshmem(&mut self, path: PathBuf, size: usize) {
        self.ivshmem = Some(IvshmemConfig { path, size });
    }

    pub fn add_cmdline(&mut self, cmd: String) -> &mut Self {
        self.cmdlines.push(cmd);
        self
    }

    pub fn check_cmdline_conflicts(&self, extra_params: &[String]) -> Vec<String> {
        let mut existing_keys = HashSet::new();
        let mut existing_params = HashSet::new();

        for param in &self.cmdlines {
            existing_params.insert(param.as_str());

            if let Some(equal_pos) = param.find('=') {
                let key = &param[..equal_pos];
                existing_keys.insert(key);
            } else {
                existing_keys.insert(param.as_str());
            }
        }

        let mut conflicts = Vec::new();
        for param in extra_params {
            let trimmed = param.trim();
            if trimmed.is_empty() {
                continue;
            }

            if existing_params.contains(trimmed) {
                conflicts.push(format!("kernel parameter '{}' already exists", trimmed));
                continue;
            }

            if let Some(equal_pos) = trimmed.find('=') {
                let key = &trimmed[..equal_pos];
                if existing_keys.contains(key) {
                    conflicts.push(format!(
                        "kernel parameter '{}' conflicts with existing parameter (key '{}' already exists)",
                        trimmed, key
                    ));
                }
            } else {
                if existing_keys.contains(trimmed) {
                    conflicts.push(format!("kernel parameter '{}' already exists", trimmed));
                }
            }
        }

        conflicts
    }

    pub fn set_vcpus(&mut self, vcpu: u32) -> &mut Self {
        self.vcpus = vcpu;
        self
    }

    pub fn set_memory(&mut self, size: u64, dirty_log: bool) -> &mut Self {
        self.memory_size = size;
        self.dirty_log = dirty_log;
        self
    }

    pub fn set_kernel(&mut self, kernel: String) -> &mut Self {
        self.kernel = kernel;
        self
    }

    pub fn add_nets(&mut self, net: &Net) -> CResult<&mut Self> {
        let nets = self.nets.as_mut().unwrap();
        for n in net.interfaces.iter() {
            let mut nc: NetConfig = NetConfig {
                id: Some(format!("{}-{}", utils::NET_DEVICE_ID_PRE, nets.len())),
                tap: n.name.clone(),
                ..Default::default()
            };

            //todo:handle err
            nc.mac =
                MacAddr::from_str(&n.mac).map_err(|_| format!("New mac addr failed:{}", &n.mac))?;
            if let Some(q) = &n.qos {
                let rate_limit = RateLimiterConfig {
                    bandwidth: Some(TokenBucketConfig {
                        size: q.bw_size,
                        one_time_burst: Some(q.bw_one_time_burst),
                        refill_time: q.bw_refill_time,
                    }),
                    ops: Some(TokenBucketConfig {
                        size: q.ops_size,
                        one_time_burst: Some(q.ops_one_time_burst),
                        refill_time: q.ops_refill_time,
                    }),
                };
                nc.rate_limiter_config = Some(rate_limit);
            }
            nets.push(nc);
        }
        Ok(self)
    }

    pub fn add_disks(&mut self, disks: &[Disk]) -> &mut Self {
        let ds = self.disks.as_mut().unwrap();
        for d in disks.iter() {
            let disk_conf = DiskConfig {
                id: Some(format!("{}-{}", utils::DISK_DEVICE_ID_PRE, ds.len())),
                path: Some(d.path.clone().into()),
                rate_limiter_config: d.rate_limiter_config,
                ..Default::default()
            };
            ds.push(disk_conf);
        }
        self
    }
    pub fn add_pmem(&mut self, p: &Pmem) -> &mut Self {
        let ps = self.pmems.as_mut().unwrap();
        let pmem_config = PmemConfig {
            file: p.file.clone().into(),
            size: p.size,
            discard_writes: p.discard_writes,
            id: Some(p.id.clone()),
            ..Default::default()
        };
        ps.push(pmem_config);
        self
    }

    pub fn add_pmems(&mut self, pmems: &[Pmem]) -> &mut Self {
        for p in pmems.iter() {
            self.add_pmem(p);
        }
        self
    }

    pub fn add_fs(&mut self, fs: &Fs) -> &mut Self {
        let fss = self.fss.as_mut().unwrap();
        let fc = FsConfig {
            id: Some(VIRTIO_FS_ID.to_string()),
            tag: VIRTIO_FS_TAG.to_string(),
            backendfs_config: fs.backendfs_config.clone(),
            rate_limiter_config: fs.rate_limiter_config,
            num_queues: 1,
            queue_size: 1024,
            ..Default::default()
        };
        fss.push(fc);

        self
    }

    pub fn add_virtiofs(&mut self, fs: &Vec<VirtioFs>) -> &mut Self {
        let fss = self.fss.as_mut().unwrap();

        let fs_configs = Utils::restore_virtiofs_configs(fs);
        fss.extend(fs_configs);
        self
    }

    pub fn add_vsock(&mut self, id: String) -> &mut Self {
        self.vsock = Some(Utils::gen_vsock_config(&id));
        self
    }
}

#[derive(Clone, Copy, PartialEq, Eq, PartialOrd)]
pub struct PciBdf(u32);
impl PciBdf {
    pub fn new(segment: u16, bus: u8, device: u8, function: u8) -> Self {
        Self(
            (segment as u32) << 16
                | (bus as u32) << 8
                | ((device & 0x1f) as u32) << 3
                | (function & 0x7) as u32,
        )
    }
    pub fn segment(&self) -> u16 {
        ((self.0 >> 16) & 0xffff) as u16
    }

    pub fn bus(&self) -> u8 {
        ((self.0 >> 8) & 0xff) as u8
    }

    pub fn device(&self) -> u8 {
        ((self.0 >> 3) & 0x1f) as u8
    }

    pub fn function(&self) -> u8 {
        (self.0 & 0x7) as u8
    }

    pub fn to_string(&self) -> String {
        format!(
            "{:04x}:{:02x}:{:02x}.{:01x}",
            self.segment(),
            self.bus(),
            self.device(),
            self.function()
        )
    }
}

impl From<&str> for PciBdf {
    fn from(bdf: &str) -> Self {
        Self::from_str(bdf).unwrap()
    }
}

impl FromStr for PciBdf {
    type Err = ParseIntError;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        let items: Vec<&str> = s.split('.').collect();
        assert_eq!(items.len(), 2);
        let function = u8::from_str_radix(items[1], 16)?;
        let items: Vec<&str> = items[0].split(':').collect();
        assert_eq!(items.len(), 3);
        let segment = u16::from_str_radix(items[0], 16)?;
        let bus = u8::from_str_radix(items[1], 16)?;
        let device = u8::from_str_radix(items[2], 16)?;
        Ok(PciBdf::new(segment, bus, device, function))
    }
}

impl<'de> serde::Deserialize<'de> for PciBdf {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        deserializer.deserialize_str(PciBdfVisitor)
    }
}

impl serde::Serialize for PciBdf {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        serializer.collect_str(&self.to_string())
    }
}

struct PciBdfVisitor;

impl<'de> Visitor<'de> for PciBdfVisitor {
    type Value = PciBdf;

    fn expecting(&self, formatter: &mut fmt::Formatter) -> fmt::Result {
        formatter.write_str("struct PciBdf")
    }

    fn visit_str<E>(self, v: &str) -> Result<Self::Value, E>
    where
        E: serde::de::Error,
    {
        Ok(v.into())
    }
}

#[derive(Clone, PartialEq, Eq, PartialOrd, Deserialize, Serialize)]
pub struct PciDeviceInfo {
    pub id: String,
    pub bdf: PciBdf,
}

#[cfg(test)]
mod tests {
    use cube_hypervisor::vm_config::ConsoleOutputMode;
    use std::path::PathBuf;

    use crate::common::utils::Utils;
    use crate::{
        common::utils::DISK_DEVICE_ID_PRE,
        hypervisor::config::{VmConfig, IMAGE_PATH},
        sandbox::disk::Disk,
    };
    #[test]
    fn utils_config_dft() {
        let config = VmConfig::default();

        //default
        assert!(config.pmems.is_some());
        let pmem = config.pmems.unwrap();
        assert_eq!(pmem[0].file, PathBuf::from(IMAGE_PATH));
        assert_eq!(config.console.mode, ConsoleOutputMode::Tty);
        assert_eq!(config.rng.src, PathBuf::from("/dev/urandom"));

        let mut params = vec![
            "root=/dev/pmem0".to_string(),
            "rootflags=dax,errors=remount-ro ro".to_string(),
            "rootfstype=ext4".to_string(),
            "panic=1".to_string(),
        ];
        #[cfg(target_arch = "x86_64")]
        params.extend(["no_timer_check".to_string(), "noreplace-smp".to_string()]);
        params.extend(["printk.devkmsg=on".to_string()]);
        #[cfg(target_arch = "aarch64")]
        params.push("console=ttyAMA0,115200".to_string());
        #[cfg(not(target_arch = "aarch64"))]
        params.push("console=hvc0".to_string());
        params.extend([
            "net.ifnames=0".to_string(),
            "audit=0".to_string(),
            "LANG=C".to_string(),
            "raid=noautodetect".to_string(),
            "earlyprintk=ttyS0".to_string(),
            "agent.debug_console".to_string(),
            "agent.debug_console_vport=1026".to_string(),
            "mitigations=off".to_string(),
        ]);
        assert_eq!(config.cmdlines, params);
    }

    #[test]
    fn utils_config_func() {
        let mut config = VmConfig::default();

        config.add_cmdline("ut_test".to_string());
        assert_eq!(
            config.cmdlines[config.cmdlines.len() - 1],
            "ut_test".to_string()
        );

        config.set_vcpus(999);
        assert_eq!(config.vcpus, 999);

        config.set_memory(999, true);
        assert_eq!(config.memory_size, 999);
        assert!(config.dirty_log);

        //add_disk
        assert!(config.disks.is_some());
        let disk = Disk {
            path: "ut_test".to_string(),
            source_dir: "/".to_string(),
            fs_type: "ext4".to_string(),
            size: 88,
            fs_quota: 88,
            rate_limiter_config: None,
        };

        config.add_disks(&[disk]);

        let disks = config.disks.clone().unwrap();
        let disk = disks[disks.len() - 1].clone();
        assert_eq!(disk.path, Some("ut_test".to_string().into()));
        assert_eq!(
            disk.id,
            Some(format!("{}-{}", DISK_DEVICE_ID_PRE, disks.len() - 1))
        );

        //add_fs
        assert!(config.vsock.is_none());
        config.add_vsock("ut_test".to_string());
        assert!(config.vsock.is_some());
        let vsock = config.vsock.unwrap();
        let vs = Utils::gen_vsock_config("ut_test");
        assert_eq!(vs, vsock)
    }

    #[test]
    fn test_check_cmdline_conflicts() {
        let mut config = VmConfig::default();

        // 测试无冲突的情况
        let extra_params = vec!["new_param=value".to_string(), "another_flag".to_string()];
        let conflicts = config.check_cmdline_conflicts(&extra_params);
        assert!(conflicts.is_empty(), "应该没有冲突");

        // 测试与默认参数冲突 - 完整参数冲突
        let extra_params = vec!["root=/dev/pmem0".to_string()];
        let conflicts = config.check_cmdline_conflicts(&extra_params);
        assert!(!conflicts.is_empty(), "应该检测到冲突");
        assert!(conflicts[0].contains("root=/dev/pmem0"));

        // 测试与默认参数冲突 - key 冲突
        let extra_params = vec!["root=/dev/sda1".to_string()];
        let conflicts = config.check_cmdline_conflicts(&extra_params);
        assert!(!conflicts.is_empty(), "应该检测到 key 冲突");
        assert!(conflicts[0].contains("root"));

        // 测试与后续添加的参数冲突
        config.add_cmdline("test_param=value1".to_string());
        let extra_params = vec!["test_param=value2".to_string()];
        let conflicts = config.check_cmdline_conflicts(&extra_params);
        assert!(!conflicts.is_empty(), "应该检测到冲突");
        assert!(conflicts[0].contains("test_param"));

        // 测试单独标志参数冲突
        config.add_cmdline("some_flag".to_string());
        let extra_params = vec!["some_flag".to_string()];
        let conflicts = config.check_cmdline_conflicts(&extra_params);
        assert!(!conflicts.is_empty(), "应该检测到标志参数冲突");

        // 测试多个冲突
        let extra_params = vec![
            "root=/dev/sda1".to_string(),
            "console=ttyS0".to_string(),
            "panic=0".to_string(),
        ];
        let conflicts = config.check_cmdline_conflicts(&extra_params);
        assert_eq!(conflicts.len(), 3, "应该检测到3个冲突");
    }
}

// Copyright (c) 2019, 2020 Ant Group
//
// SPDX-License-Identifier: Apache-2.0
//

use crate::cgroups::{Manager as CgroupManager, RESOURCE_METRICS_VERSION_V1};
use crate::container::DEFAULT_DEVICES;
use anyhow::{anyhow, Context, Result};
use cgroups::blkio::{BlkIoController, BlkIoData, IoService};
use cgroups::cpu::CpuController;
use cgroups::cpuacct::CpuAcctController;
//use cgroups::cpuset::CpuSetController;
use cgroups::devices::DevicePermissions;
use cgroups::devices::DeviceType;
use cgroups::freezer::{FreezerController, FreezerState};
use cgroups::hugetlb::HugeTlbController;
use cgroups::memory::MemController;
use cgroups::pid::PidController;
use cgroups::{
    BlkIoDeviceResource, BlkIoDeviceThrottleResource, Cgroup, CgroupPid, Controller,
    DeviceResource, HugePageResource, MaxValue, NetworkPriority,
};
use libc::{self, pid_t};
use oci::{
    LinuxBlockIo, LinuxCpu, LinuxDevice, LinuxDeviceCgroup, LinuxHugepageLimit, LinuxMemory,
    LinuxNetwork, LinuxPids, LinuxResources,
};
use slog::info;

use protobuf::MessageField;
use protocols::agent::{
    BlkioStats, BlkioStatsEntry, CgroupStats, CpuStats, CpuUsage, HugetlbStats, MemoryData,
    MemoryStats, PidsStats, ThrottlingData,
};
use std::collections::{BTreeSet, HashMap};
use std::fs;
use std::path::{Path, PathBuf};
use std::thread;
use std::time::Duration;
//use std::path::Path;
use lazy_static::lazy_static;

lazy_static! {
    static ref CUBE_CONTROLLER: Vec<String> = {
        let mut vec = Vec::new();
        vec.push("cpu".to_string());
        vec.push("cpuacct".to_string());
        vec.push("memory".to_string());
        vec.push("freezer".to_string());
        vec.push("net_cls".to_string());
        vec.push("net_prio".to_string());
        vec.push("oom".to_string());
        vec
    };
}

const GUEST_CPUS_PATH: &str = "/sys/devices/system/cpu/online";

fn resource_metrics_version_for_layout(cgroup_v2: bool, has_process_cgroup: bool) -> u32 {
    if cgroup_v2 && has_process_cgroup {
        RESOURCE_METRICS_VERSION_V1
    } else {
        0
    }
}

// Convenience macro to obtain the scope logger
macro_rules! sl {
    () => {
        slog_scope::logger().new(o!("subsystem" => "cgroups"))
    };
}

macro_rules! get_controller_or_return_singular_none {
    ($cg:ident) => {
        match $cg.controller_of() {
            Some(c) => c,
            None => return MessageField::none(),
        }
    };
}

#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct Manager {
    pub paths: HashMap<String, String>,
    pub mounts: HashMap<String, String>,
    pub cpath: String,
    #[serde(skip)]
    cgroup: cgroups::Cgroup,
    // On cgroup v2 the accounting cgroup must remain process-free so its
    // controllers can be delegated to envd's child cgroups. Runtime-created
    // OCI processes are placed in this leaf instead.
    #[serde(skip)]
    process_cgroup: Option<cgroups::Cgroup>,
}

// set_resource is used to set reources by cgroup controller.
macro_rules! set_resource {
    ($cont:ident, $func:ident, $res:ident, $field:ident) => {
        let resource_value = $res.$field.unwrap_or(0);
        if resource_value != 0 {
            $cont.$func(resource_value)?;
        }
    };
}

impl CgroupManager for Manager {
    fn apply(&self, pid: pid_t) -> Result<()> {
        let cgroup_pid = CgroupPid::from(pid as u64);
        let target = self.process_cgroup.as_ref().unwrap_or(&self.cgroup);
        if target.v2() {
            target.add_task_by_tgid(cgroup_pid)?;
        } else {
            target.add_task(cgroup_pid)?;
        }
        Ok(())
    }

    fn set(&self, r: &LinuxResources, update: bool) -> Result<()> {
        let res = &mut cgroups::Resources::default();

        // set cpuset and cpu reources
        if let Some(cpu) = &r.cpu {
            set_cpu_resources(&self.cgroup, cpu)?;
        }

        // set memory resources
        if let Some(memory) = &r.memory {
            set_memory_resources(&self.cgroup, memory, update)?;
        }

        // set pids resources
        if let Some(pids_resources) = &r.pids {
            set_pids_resources(&self.cgroup, pids_resources)?;
        }

        // set block_io resources
        if let Some(blkio) = &r.block_io {
            set_block_io_resources(&self.cgroup, blkio, res);
        }

        // set hugepages resources
        if !r.hugepage_limits.is_empty() {
            set_hugepages_resources(&self.cgroup, &r.hugepage_limits, res);
        }

        // set network resources
        if let Some(network) = &r.network {
            set_network_resources(&self.cgroup, network, res);
        }

        // set devices resources
        set_devices_resources(&self.cgroup, &r.devices, res);

        // apply resources
        self.cgroup.apply(res)?;

        Ok(())
    }

    fn get_stats(&self) -> Result<CgroupStats> {
        // CpuStats
        let cpu_usage = get_cpuacct_stats_strict(&self.cgroup)?;

        let throttling_data = get_cpu_stats_strict(&self.cgroup)?;

        let cpu_stats = MessageField::some(CpuStats {
            cpu_usage,
            throttling_data,
            ..Default::default()
        });

        // Memorystats
        let memory_stats = get_memory_stats_strict(&self.cgroup)?;

        // PidsStats
        let pids_stats = get_pids_stats(&self.cgroup);

        // BlkioStats
        // note that virtiofs has no blkio stats
        let blkio_stats = get_blkio_stats(&self.cgroup);

        // HugetlbStats
        let hugetlb_stats = get_hugetlb_stats(&self.cgroup);

        Ok(CgroupStats {
            cpu_stats,
            memory_stats,
            pids_stats,
            blkio_stats,
            hugetlb_stats,
            ..Default::default()
        })
    }

    fn resource_metrics_version(&self) -> u32 {
        resource_metrics_version_for_layout(self.cgroup.v2(), self.process_cgroup.is_some())
    }

    fn freeze(&self, state: FreezerState) -> Result<()> {
        let freezer_controller: &FreezerController = self.cgroup.controller_of().unwrap();
        match state {
            FreezerState::Thawed => {
                freezer_controller.thaw()?;
            }
            FreezerState::Frozen => {
                freezer_controller.freeze()?;
            }
            _ => {
                return Err(anyhow!(nix::Error::EINVAL));
            }
        }

        Ok(())
    }

    fn destroy(&mut self) -> Result<()> {
        if self.cgroup.v2() {
            // cgroup.kill is recursive on supported cgroup v2 kernels. It is
            // best-effort because the caller already kills tracked PIDs.
            let _ = self.cgroup.kill();
            return remove_cgroup_tree(&self.cpath);
        }

        let _ = self.cgroup.delete();
        Ok(())
    }

    fn get_pids(&self) -> Result<Vec<pid_t>> {
        let pids: Vec<pid_t> = if self.cgroup.v2() {
            let mut pids = BTreeSet::new();
            collect_cgroup_pids(&cgroup_filesystem_path(&self.cpath)?, &mut pids)?;
            pids.into_iter().collect::<Vec<_>>()
        } else {
            let mem_controller: &MemController = self.cgroup.controller_of().unwrap();
            mem_controller
                .tasks()
                .into_iter()
                .map(|pid| pid.pid as pid_t)
                .collect()
        };
        let result = pids.into_iter().map(|pid| pid as i32).collect::<Vec<i32>>();

        Ok(result)
    }
}

const CGROUP2_MOUNTPOINT: &str = "/sys/fs/cgroup";
const PROCESS_CGROUP_NAME: &str = "runtime";
const REQUIRED_DELEGATED_CONTROLLERS: [&str; 3] = ["cpu", "memory", "pids"];

pub fn process_cgroup_path(cpath: &str) -> String {
    format!("{}/{}", cpath.trim_end_matches('/'), PROCESS_CGROUP_NAME)
}

pub fn cgroup_filesystem_path(cpath: &str) -> Result<PathBuf> {
    let relative = cpath.trim_matches('/');
    let mut path = PathBuf::from(CGROUP2_MOUNTPOINT);
    if relative.is_empty() {
        return Ok(path);
    }

    for component in relative.split('/') {
        if component.is_empty() || component == "." || component == ".." {
            return Err(anyhow!("invalid cgroup path component in {cpath:?}"));
        }
        path.push(component);
    }
    Ok(path)
}

fn read_cgroup_words(path: &Path) -> Result<BTreeSet<String>> {
    Ok(fs::read_to_string(path)
        .with_context(|| format!("read cgroup controller file {}", path.display()))?
        .split_whitespace()
        .map(str::to_string)
        .collect())
}

fn verify_delegated_controllers_at(path: &Path) -> Result<()> {
    let available = read_cgroup_words(&path.join("cgroup.controllers"))?;
    let delegated = read_cgroup_words(&path.join("cgroup.subtree_control"))?;

    for controller in REQUIRED_DELEGATED_CONTROLLERS {
        if !available.contains(controller) {
            return Err(anyhow!(
                "required cgroup v2 controller {controller} is unavailable at {}",
                path.display()
            ));
        }
        if !delegated.contains(controller) {
            return Err(anyhow!(
                "required cgroup v2 controller {controller} is not delegated at {}",
                path.display()
            ));
        }
    }
    Ok(())
}

fn collect_cgroup_pids(path: &Path, pids: &mut BTreeSet<pid_t>) -> Result<()> {
    let procs = path.join("cgroup.procs");
    match fs::read_to_string(&procs) {
        Ok(content) => {
            for line in content.lines() {
                let pid = line
                    .trim()
                    .parse::<pid_t>()
                    .with_context(|| format!("parse cgroup pid {line:?}"))?;
                pids.insert(pid);
            }
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(error) => {
            return Err(error)
                .with_context(|| format!("read cgroup process list {}", procs.display()))
        }
    }

    let entries = match fs::read_dir(path) {
        Ok(entries) => entries,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(error) => {
            return Err(error).with_context(|| format!("read cgroup directory {}", path.display()))
        }
    };
    for entry in entries {
        let entry = match entry {
            Ok(entry) => entry,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
            Err(error) => return Err(error.into()),
        };
        let file_type = match entry.file_type() {
            Ok(file_type) => file_type,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
            Err(error) => return Err(error.into()),
        };
        if file_type.is_dir() {
            collect_cgroup_pids(&entry.path(), pids)?;
        }
    }
    Ok(())
}

fn collect_cgroup_dirs(path: &Path, dirs: &mut Vec<PathBuf>) -> Result<()> {
    let entries = match fs::read_dir(path) {
        Ok(entries) => entries,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(error) => {
            return Err(error).with_context(|| format!("read cgroup directory {}", path.display()))
        }
    };
    for entry in entries {
        let entry = match entry {
            Ok(entry) => entry,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
            Err(error) => return Err(error.into()),
        };
        let file_type = match entry.file_type() {
            Ok(file_type) => file_type,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
            Err(error) => return Err(error.into()),
        };
        if file_type.is_dir() {
            let child = entry.path();
            collect_cgroup_dirs(&child, dirs)?;
            dirs.push(child);
        }
    }
    Ok(())
}

fn remove_cgroup_tree(cpath: &str) -> Result<()> {
    remove_cgroup_tree_at(&cgroup_filesystem_path(cpath)?)
}

fn remove_cgroup_tree_at(root: &Path) -> Result<()> {
    for _ in 0..20 {
        if !root.exists() {
            return Ok(());
        }

        let mut dirs = Vec::new();
        collect_cgroup_dirs(root, &mut dirs)?;
        dirs.push(root.to_path_buf());
        let mut retry = false;

        for dir in dirs {
            match fs::remove_dir(&dir) {
                Ok(()) => {}
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
                Err(error)
                    if error.raw_os_error() == Some(libc::EBUSY)
                        || error.raw_os_error() == Some(libc::ENOTEMPTY) =>
                {
                    retry = true;
                }
                Err(error) => {
                    return Err(error).with_context(|| format!("remove cgroup {}", dir.display()));
                }
            }
        }

        if !retry && !root.exists() {
            return Ok(());
        }
        thread::sleep(Duration::from_millis(10));
    }

    Err(anyhow!("cgroup {} remained busy", root.display()))
}

fn set_network_resources(
    _cg: &cgroups::Cgroup,
    network: &LinuxNetwork,
    res: &mut cgroups::Resources,
) {
    // set classid
    // description can be found at https://www.kernel.org/doc/html/latest/admin-guide/cgroup-v1/net_cls.html
    let class_id = network.class_id.unwrap_or(0) as u64;
    if class_id != 0 {
        res.network.class_id = Some(class_id);
    }

    // set network priorities
    // description can be found at https://www.kernel.org/doc/html/latest/admin-guide/cgroup-v1/net_prio.html
    let mut priorities = vec![];
    for p in network.priorities.iter() {
        priorities.push(NetworkPriority {
            name: p.name.clone(),
            priority: p.priority as u64,
        });
    }

    res.network.priorities = priorities;
}

fn set_devices_resources(
    _cg: &cgroups::Cgroup,
    device_resources: &[LinuxDeviceCgroup],
    res: &mut cgroups::Resources,
) {
    let mut devices = vec![];

    for d in device_resources.iter() {
        if let Some(dev) = linux_device_group_to_cgroup_device(d) {
            devices.push(dev);
        }
    }

    for d in DEFAULT_DEVICES.iter() {
        if let Some(dev) = linux_device_to_cgroup_device(d) {
            devices.push(dev);
        }
    }

    for d in DEFAULT_ALLOWED_DEVICES.iter() {
        if let Some(dev) = linux_device_group_to_cgroup_device(d) {
            devices.push(dev);
        }
    }

    res.devices.devices = devices;
}

fn set_hugepages_resources(
    _cg: &cgroups::Cgroup,
    hugepage_limits: &[LinuxHugepageLimit],
    res: &mut cgroups::Resources,
) {
    let mut limits = vec![];

    for l in hugepage_limits.iter() {
        let hr = HugePageResource {
            size: l.page_size.clone(),
            limit: l.limit,
        };
        limits.push(hr);
    }
    res.hugepages.limits = limits;
}

fn set_block_io_resources(
    _cg: &cgroups::Cgroup,
    blkio: &LinuxBlockIo,
    res: &mut cgroups::Resources,
) {
    res.blkio.weight = blkio.weight;
    res.blkio.leaf_weight = blkio.leaf_weight;

    let mut blk_device_resources = vec![];
    for d in blkio.weight_device.iter() {
        let dr = BlkIoDeviceResource {
            major: d.blk.major as u64,
            minor: d.blk.minor as u64,
            weight: blkio.weight,
            leaf_weight: blkio.leaf_weight,
        };
        blk_device_resources.push(dr);
    }
    res.blkio.weight_device = blk_device_resources;

    res.blkio.throttle_read_bps_device =
        build_blk_io_device_throttle_resource(&blkio.throttle_read_bps_device);
    res.blkio.throttle_write_bps_device =
        build_blk_io_device_throttle_resource(&blkio.throttle_write_bps_device);
    res.blkio.throttle_read_iops_device =
        build_blk_io_device_throttle_resource(&blkio.throttle_read_iops_device);
    res.blkio.throttle_write_iops_device =
        build_blk_io_device_throttle_resource(&blkio.throttle_write_iops_device);
}

fn set_cpu_resources(cg: &cgroups::Cgroup, cpu: &LinuxCpu) -> Result<()> {
    /*
    let cpuset_controller: &CpuSetController = cg.controller_of().unwrap();

    if !cpu.cpus.is_empty() {
        if let Err(e) = cpuset_controller.set_cpus(&cpu.cpus) {
            warn!(sl!(), "write cpuset failed: {:?}", e);
        }
    }

    if !cpu.mems.is_empty() {
        cpuset_controller.set_mems(&cpu.mems)?;
    }*/

    let cpu_controller: &CpuController = cg.controller_of().unwrap();

    if let Some(shares) = cpu.shares {
        let shares = if cg.v2() {
            convert_shares_to_v2_value(shares)
        } else {
            shares
        };
        if shares != 0 {
            cpu_controller.set_shares(shares)?;
        }
    }

    set_resource!(cpu_controller, set_cfs_quota, cpu, quota);
    set_resource!(cpu_controller, set_cfs_period, cpu, period);

    set_resource!(cpu_controller, set_rt_runtime, cpu, realtime_runtime);
    set_resource!(cpu_controller, set_rt_period_us, cpu, realtime_period);

    Ok(())
}

fn set_memory_resources(cg: &cgroups::Cgroup, memory: &LinuxMemory, update: bool) -> Result<()> {
    let mem_controller: &MemController = cg.controller_of().unwrap();

    if !update {
        // initialize kmem limits for accounting
        mem_controller.set_kmem_limit(1)?;
        mem_controller.set_kmem_limit(-1)?;
    }

    // If the memory update is set to -1 we should also
    // set swap to -1, it means unlimited memory.
    let mut swap = memory.swap.unwrap_or(0);
    if memory.limit == Some(-1) {
        swap = -1;
    }

    if memory.limit.is_some() && swap != 0 {
        let memstat = get_memory_stats(cg)
            .into_option()
            .ok_or_else(|| anyhow!("failed to get the cgroup memory stats"))?;
        let memusage = memstat.usage();

        // When update memory limit, the kernel would check the current memory limit
        // set against the new swap setting, if the current memory limit is large than
        // the new swap, then set limit first, otherwise the kernel would complain and
        // refused to set; on the other hand, if the current memory limit is smaller than
        // the new swap, then we should set the swap first and then set the memor limit.
        if swap == -1 || memusage.limit() < swap as u64 {
            mem_controller.set_memswap_limit(swap)?;
            set_resource!(mem_controller, set_limit, memory, limit);
        } else {
            set_resource!(mem_controller, set_limit, memory, limit);
            mem_controller.set_memswap_limit(swap)?;
        }
    } else {
        set_resource!(mem_controller, set_limit, memory, limit);
        swap = if cg.v2() {
            convert_memory_swap_to_v2_value(swap, memory.limit.unwrap_or(0))?
        } else {
            swap
        };
        if swap != 0 {
            mem_controller.set_memswap_limit(swap)?;
        }
    }

    set_resource!(mem_controller, set_soft_limit, memory, reservation);
    set_resource!(mem_controller, set_kmem_limit, memory, kernel);
    set_resource!(mem_controller, set_tcp_limit, memory, kernel_tcp);

    if let Some(swappiness) = memory.swappiness {
        if (0..=100).contains(&swappiness) {
            mem_controller.set_swappiness(swappiness)?;
        } else {
            return Err(anyhow!(
                "invalid value:{}. valid memory swappiness range is 0-100",
                swappiness
            ));
        }
    }

    if memory.disable_oom_killer.unwrap_or(false) {
        mem_controller.disable_oom_killer()?;
    }

    Ok(())
}

fn set_pids_resources(cg: &cgroups::Cgroup, pids: &LinuxPids) -> Result<()> {
    let pid_controller: &PidController = cg.controller_of().unwrap();
    let v = if pids.limit > 0 {
        MaxValue::Value(pids.limit)
    } else {
        MaxValue::Max
    };
    pid_controller
        .set_pid_max(v)
        .context("failed to set pids resources")
}

fn build_blk_io_device_throttle_resource(
    input: &[oci::LinuxThrottleDevice],
) -> Vec<BlkIoDeviceThrottleResource> {
    let mut blk_io_device_throttle_resources = vec![];
    for d in input.iter() {
        let tr = BlkIoDeviceThrottleResource {
            major: d.blk.major as u64,
            minor: d.blk.minor as u64,
            rate: d.rate,
        };
        blk_io_device_throttle_resources.push(tr);
    }

    blk_io_device_throttle_resources
}

fn linux_device_to_cgroup_device(d: &LinuxDevice) -> Option<DeviceResource> {
    let dev_type = match DeviceType::from_char(d.r#type.chars().next()) {
        Some(t) => t,
        None => return None,
    };

    let permissions = vec![
        DevicePermissions::Read,
        DevicePermissions::Write,
        DevicePermissions::MkNod,
    ];

    Some(DeviceResource {
        allow: true,
        devtype: dev_type,
        major: d.major,
        minor: d.minor,
        access: permissions,
    })
}

fn linux_device_group_to_cgroup_device(d: &LinuxDeviceCgroup) -> Option<DeviceResource> {
    let dev_type = match DeviceType::from_char(d.r#type.chars().next()) {
        Some(t) => t,
        None => return None,
    };

    let mut permissions: Vec<DevicePermissions> = vec![];
    for p in d.access.chars().collect::<Vec<char>>() {
        match p {
            'r' => permissions.push(DevicePermissions::Read),
            'w' => permissions.push(DevicePermissions::Write),
            'm' => permissions.push(DevicePermissions::MkNod),
            _ => {}
        }
    }

    Some(DeviceResource {
        allow: d.allow,
        devtype: dev_type,
        major: d.major.unwrap_or(0),
        minor: d.minor.unwrap_or(0),
        access: permissions,
    })
}

pub const NANO_PER_SECOND: u64 = 1000000000;
pub const WILDCARD: i64 = -1;

lazy_static! {
    pub static ref CLOCK_TICKS: f64 = {
        let n = unsafe { libc::sysconf(libc::_SC_CLK_TCK) };

        n as f64
    };

    pub static ref DEFAULT_ALLOWED_DEVICES: Vec<LinuxDeviceCgroup> = {
        vec![
            // all mknod to all char devices
            LinuxDeviceCgroup {
                allow: true,
                r#type: "c".to_string(),
                major: Some(WILDCARD),
                minor: Some(WILDCARD),
                access: "m".to_string(),
            },

            // all mknod to all block devices
            LinuxDeviceCgroup {
                allow: true,
                r#type: "b".to_string(),
                major: Some(WILDCARD),
                minor: Some(WILDCARD),
                access: "m".to_string(),
            },

            // all read/write/mknod to char device /dev/console
            LinuxDeviceCgroup {
                allow: true,
                r#type: "c".to_string(),
                major: Some(5),
                minor: Some(1),
                access: "rwm".to_string(),
            },

            // all read/write/mknod to char device /dev/pts/<N>
            LinuxDeviceCgroup {
                allow: true,
                r#type: "c".to_string(),
                major: Some(136),
                minor: Some(WILDCARD),
                access: "rwm".to_string(),
            },

            // all read/write/mknod to char device /dev/ptmx
            LinuxDeviceCgroup {
                allow: true,
                r#type: "c".to_string(),
                major: Some(5),
                minor: Some(2),
                access: "rwm".to_string(),
            },

            // all read/write/mknod to char device /dev/net/tun
            LinuxDeviceCgroup {
                allow: true,
                r#type: "c".to_string(),
                major: Some(10),
                minor: Some(200),
                access: "rwm".to_string(),
            },
        ]
    };
}

#[derive(Debug, PartialEq, Eq)]
struct V2CpuStats {
    usage_ns: u64,
    user_ns: u64,
    system_ns: u64,
    periods: u64,
    throttled_periods: u64,
    throttled_time_ns: u64,
}

#[derive(Debug, PartialEq, Eq)]
struct V1CpuAcctStats {
    usage_ns: u64,
    user_ns: u64,
    system_ns: u64,
    per_cpu_ns: Vec<u64>,
}

fn required_counter(content: &str, key: &str, source: &str) -> Result<u64> {
    let value = content
        .lines()
        .find_map(|line| {
            let mut fields = line.split_whitespace();
            match (fields.next(), fields.next(), fields.next()) {
                (Some(name), Some(value), None) if name == key => Some(value),
                _ => None,
            }
        })
        .ok_or_else(|| anyhow!("missing required cgroup key {} in {}", key, source))?;
    value
        .parse::<u64>()
        .with_context(|| format!("invalid cgroup value for {} in {}: {}", key, source, value))
}

fn micros_to_nanos(value: u64, key: &str, source: &str) -> Result<u64> {
    value.checked_mul(1_000).ok_or_else(|| {
        anyhow!(
            "cgroup value for {} in {} overflows nanoseconds",
            key,
            source
        )
    })
}

fn ticks_to_nanos(value: u64, ticks_per_second: u64, key: &str) -> Result<u64> {
    if ticks_per_second == 0 {
        return Err(anyhow!("system clock ticks per second is zero"));
    }
    let nanos = u128::from(value) * u128::from(NANO_PER_SECOND) / u128::from(ticks_per_second);
    if nanos > u128::from(u64::MAX) {
        return Err(anyhow!("cgroup value for {} overflows nanoseconds", key));
    }
    Ok(nanos as u64)
}

fn parse_v1_cpuacct_stats(
    usage: &str,
    stat: &str,
    per_cpu: &str,
    ticks_per_second: u64,
) -> Result<V1CpuAcctStats> {
    Ok(V1CpuAcctStats {
        usage_ns: usage
            .trim()
            .parse::<u64>()
            .context("invalid cgroup value for cpuacct.usage")?,
        user_ns: ticks_to_nanos(
            required_counter(stat, "user", "cpuacct.stat")?,
            ticks_per_second,
            "cpuacct.stat user",
        )?,
        system_ns: ticks_to_nanos(
            required_counter(stat, "system", "cpuacct.stat")?,
            ticks_per_second,
            "cpuacct.stat system",
        )?,
        per_cpu_ns: per_cpu
            .split_whitespace()
            .map(|value| {
                value
                    .parse::<u64>()
                    .with_context(|| format!("invalid cpuacct.usage_percpu value {}", value))
            })
            .collect::<Result<Vec<_>>>()?,
    })
}

fn parse_v2_cpu_stats(content: &str) -> Result<V2CpuStats> {
    Ok(V2CpuStats {
        usage_ns: micros_to_nanos(
            required_counter(content, "usage_usec", "cpu.stat")?,
            "usage_usec",
            "cpu.stat",
        )?,
        user_ns: micros_to_nanos(
            required_counter(content, "user_usec", "cpu.stat")?,
            "user_usec",
            "cpu.stat",
        )?,
        system_ns: micros_to_nanos(
            required_counter(content, "system_usec", "cpu.stat")?,
            "system_usec",
            "cpu.stat",
        )?,
        periods: required_counter(content, "nr_periods", "cpu.stat")?,
        throttled_periods: required_counter(content, "nr_throttled", "cpu.stat")?,
        throttled_time_ns: micros_to_nanos(
            required_counter(content, "throttled_usec", "cpu.stat")?,
            "throttled_usec",
            "cpu.stat",
        )?,
    })
}

#[derive(Debug, PartialEq, Eq)]
struct V2MemoryStats {
    current: u64,
    limit: u64,
    peak: u64,
    failures: u64,
}

#[derive(Debug, PartialEq, Eq)]
struct V1MemoryUsage {
    current: u64,
    limit: u64,
    peak: u64,
    failures: u64,
}

fn parse_v1_memory_usage(
    current: &str,
    limit: &str,
    peak: &str,
    failures: &str,
) -> Result<V1MemoryUsage> {
    let parse = |value: &str, key: &str| {
        value
            .trim()
            .parse::<u64>()
            .with_context(|| format!("invalid cgroup value for {}: {}", key, value.trim()))
    };
    Ok(V1MemoryUsage {
        current: parse(current, "memory.usage_in_bytes")?,
        limit: parse(limit, "memory.limit_in_bytes")?,
        peak: parse(peak, "memory.max_usage_in_bytes")?,
        failures: parse(failures, "memory.failcnt")?,
    })
}

fn parse_v2_limit(value: &str, key: &str) -> Result<u64> {
    if value.trim() == "max" {
        return Ok(u64::MAX);
    }
    value
        .trim()
        .parse::<u64>()
        .with_context(|| format!("invalid cgroup value for {}: {}", key, value.trim()))
}

fn parse_v2_memory_stats(
    current: &str,
    limit: &str,
    peak: &str,
    events: &str,
) -> Result<V2MemoryStats> {
    Ok(V2MemoryStats {
        current: current
            .trim()
            .parse::<u64>()
            .context("invalid cgroup value for memory.current")?,
        limit: parse_v2_limit(limit, "memory.max")?,
        peak: peak
            .trim()
            .parse::<u64>()
            .context("invalid cgroup value for memory.peak")?,
        failures: required_counter(events, "max", "memory.events")?,
    })
}

fn read_required(path: &Path) -> Result<String> {
    fs::read_to_string(path).with_context(|| format!("read cgroup stat {}", path.display()))
}

fn get_cpuacct_stats_strict(cg: &cgroups::Cgroup) -> Result<MessageField<CpuUsage>> {
    if !cg.v2() {
        let controller: &CpuAcctController = cg
            .controller_of()
            .ok_or_else(|| anyhow!("cpu accounting controller is unavailable"))?;
        let usage = read_required(&controller.path().join("cpuacct.usage"))?;
        let stat = read_required(&controller.path().join("cpuacct.stat"))?;
        let per_cpu = read_required(&controller.path().join("cpuacct.usage_percpu"))?;
        if *CLOCK_TICKS <= 0.0 {
            return Err(anyhow!("failed to determine system clock ticks per second"));
        }
        let stats = parse_v1_cpuacct_stats(&usage, &stat, &per_cpu, *CLOCK_TICKS as u64)?;
        return Ok(MessageField::some(CpuUsage {
            total_usage: stats.usage_ns,
            percpu_usage: stats.per_cpu_ns,
            usage_in_kernelmode: stats.system_ns,
            usage_in_usermode: stats.user_ns,
            ..Default::default()
        }));
    }
    let controller: &CpuController = cg
        .controller_of()
        .ok_or_else(|| anyhow!("CPU controller is unavailable"))?;
    let stats = parse_v2_cpu_stats(&read_required(&controller.path().join("cpu.stat"))?)?;
    Ok(MessageField::some(CpuUsage {
        total_usage: stats.usage_ns,
        usage_in_kernelmode: stats.system_ns,
        usage_in_usermode: stats.user_ns,
        ..Default::default()
    }))
}

fn get_cpu_stats_strict(cg: &cgroups::Cgroup) -> Result<MessageField<ThrottlingData>> {
    if !cg.v2() {
        let controller: &CpuController = cg
            .controller_of()
            .ok_or_else(|| anyhow!("CPU controller is unavailable"))?;
        let stat = read_required(&controller.path().join("cpu.stat"))?;
        return Ok(MessageField::some(ThrottlingData {
            periods: required_counter(&stat, "nr_periods", "cpu.stat")?,
            throttled_periods: required_counter(&stat, "nr_throttled", "cpu.stat")?,
            throttled_time: required_counter(&stat, "throttled_time", "cpu.stat")?,
            ..Default::default()
        }));
    }
    let controller: &CpuController = cg
        .controller_of()
        .ok_or_else(|| anyhow!("CPU controller is unavailable"))?;
    let stats = parse_v2_cpu_stats(&read_required(&controller.path().join("cpu.stat"))?)?;
    Ok(MessageField::some(ThrottlingData {
        periods: stats.periods,
        throttled_periods: stats.throttled_periods,
        throttled_time: stats.throttled_time_ns,
        ..Default::default()
    }))
}

fn get_memory_stats_strict(cg: &cgroups::Cgroup) -> Result<MessageField<MemoryStats>> {
    if !cg.v2() {
        let controller: &MemController = cg
            .controller_of()
            .ok_or_else(|| anyhow!("memory controller is unavailable"))?;
        let path = controller.path();
        let usage = parse_v1_memory_usage(
            &read_required(&path.join("memory.usage_in_bytes"))?,
            &read_required(&path.join("memory.limit_in_bytes"))?,
            &read_required(&path.join("memory.max_usage_in_bytes"))?,
            &read_required(&path.join("memory.failcnt"))?,
        )?;
        let mut value = get_memory_stats(cg)
            .into_option()
            .ok_or_else(|| anyhow!("memory stats are unavailable"))?;
        value.usage = MessageField::some(MemoryData {
            usage: usage.current,
            max_usage: usage.peak,
            failcnt: usage.failures,
            limit: usage.limit,
            ..Default::default()
        });
        return Ok(MessageField::some(value));
    }
    let controller: &MemController = cg
        .controller_of()
        .ok_or_else(|| anyhow!("memory controller is unavailable"))?;
    let path = controller.path();
    let stats = parse_v2_memory_stats(
        &read_required(&path.join("memory.current"))?,
        &read_required(&path.join("memory.max"))?,
        &read_required(&path.join("memory.peak"))?,
        &read_required(&path.join("memory.events"))?,
    )?;
    // Preserve the cache and raw memory.stat fields exposed by the existing
    // StatsContainer contract while replacing its usage entry with values
    // read from the accounting cgroup using strict cgroup v2 semantics.
    let mut value = get_memory_stats(cg)
        .into_option()
        .ok_or_else(|| anyhow!("memory stats are unavailable"))?;
    value.usage = MessageField::some(MemoryData {
        usage: stats.current,
        max_usage: stats.peak,
        failcnt: stats.failures,
        limit: stats.limit,
        ..Default::default()
    });
    Ok(MessageField::some(value))
}

fn get_memory_stats(cg: &cgroups::Cgroup) -> MessageField<MemoryStats> {
    let memory_controller: &MemController = get_controller_or_return_singular_none!(cg);

    // cache from memory stat
    let memory = memory_controller.memory_stat();
    let cache = memory.stat.cache;

    // use_hierarchy
    let value = memory.use_hierarchy;
    let use_hierarchy = value == 1;

    // gte memory datas
    let usage = MessageField::some(MemoryData {
        usage: memory.usage_in_bytes,
        max_usage: memory.max_usage_in_bytes,
        failcnt: memory.fail_cnt,
        limit: memory.limit_in_bytes as u64,
        ..Default::default()
    });

    // get swap usage
    let memswap = memory_controller.memswap();

    let swap_usage = MessageField::some(MemoryData {
        usage: memswap.usage_in_bytes,
        max_usage: memswap.max_usage_in_bytes,
        failcnt: memswap.fail_cnt,
        limit: memswap.limit_in_bytes as u64,
        ..Default::default()
    });

    // get kernel usage
    let kmem_stat = memory_controller.kmem_stat();

    let kernel_usage = MessageField::some(MemoryData {
        usage: kmem_stat.usage_in_bytes,
        max_usage: kmem_stat.max_usage_in_bytes,
        failcnt: kmem_stat.fail_cnt,
        limit: kmem_stat.limit_in_bytes as u64,
        ..Default::default()
    });

    MessageField::some(MemoryStats {
        cache,
        usage,
        swap_usage,
        kernel_usage,
        use_hierarchy,
        stats: memory.stat.raw,
        ..Default::default()
    })
}

fn get_pids_stats(cg: &cgroups::Cgroup) -> MessageField<PidsStats> {
    let pid_controller: &PidController = get_controller_or_return_singular_none!(cg);

    let current = pid_controller.get_pid_current().unwrap_or(0);
    let max = pid_controller.get_pid_max();

    let limit = match max {
        Err(_) => 0,
        Ok(max) => match max {
            MaxValue::Value(v) => v,
            MaxValue::Max => 0,
        },
    } as u64;

    MessageField::some(PidsStats {
        current,
        limit,
        ..Default::default()
    })
}

/*
examples(from runc, cgroup v1):
https://github.com/opencontainers/runc/blob/a5847db387ae28c0ca4ebe4beee1a76900c86414/libcontainer/cgroups/fs/blkio.go

    blkio.sectors
    8:0 6792

    blkio.io_service_bytes
    8:0 Read 1282048
    8:0 Write 2195456
    8:0 Sync 2195456
    8:0 Async 1282048
    8:0 Total 3477504
    Total 3477504

    blkio.io_serviced
    8:0 Read 124
    8:0 Write 104
    8:0 Sync 104
    8:0 Async 124
    8:0 Total 228
    Total 228

    blkio.io_queued
    8:0 Read 0
    8:0 Write 0
    8:0 Sync 0
    8:0 Async 0
    8:0 Total 0
    Total 0
*/

fn get_blkio_stat_blkiodata(blkiodata: &[BlkIoData]) -> Vec<BlkioStatsEntry> {
    let mut m = Vec::new();
    if blkiodata.is_empty() {
        return m;
    }

    // blkio.time_recursive and blkio.sectors_recursive have no op field.
    let op = "".to_string();
    for d in blkiodata {
        m.push(BlkioStatsEntry {
            major: d.major as u64,
            minor: d.minor as u64,
            op: op.clone(),
            value: d.data,
            ..Default::default()
        });
    }

    m
}

fn get_blkio_stat_ioservice(services: &[IoService]) -> Vec<BlkioStatsEntry> {
    let mut m = Vec::new();

    if services.is_empty() {
        return m;
    }

    for s in services {
        m.push(build_blkio_stats_entry(s.major, s.minor, "read", s.read));
        m.push(build_blkio_stats_entry(s.major, s.minor, "write", s.write));
        m.push(build_blkio_stats_entry(s.major, s.minor, "sync", s.sync));
        m.push(build_blkio_stats_entry(
            s.major, s.minor, "async", s.r#async,
        ));
        m.push(build_blkio_stats_entry(s.major, s.minor, "total", s.total));
    }
    m
}

fn build_blkio_stats_entry(major: i16, minor: i16, op: &str, value: u64) -> BlkioStatsEntry {
    BlkioStatsEntry {
        major: major as u64,
        minor: minor as u64,
        op: op.to_string(),
        value,
        ..Default::default()
    }
}

fn get_blkio_stats_v2(cg: &cgroups::Cgroup) -> MessageField<BlkioStats> {
    let blkio_controller: &BlkIoController = get_controller_or_return_singular_none!(cg);
    let blkio = blkio_controller.blkio();

    let mut resp = BlkioStats::new();
    let mut blkio_stats = Vec::new();

    let stat = blkio.io_stat;
    for s in stat {
        blkio_stats.push(build_blkio_stats_entry(s.major, s.minor, "read", s.rbytes));
        blkio_stats.push(build_blkio_stats_entry(s.major, s.minor, "write", s.wbytes));
        blkio_stats.push(build_blkio_stats_entry(s.major, s.minor, "rios", s.rios));
        blkio_stats.push(build_blkio_stats_entry(s.major, s.minor, "wios", s.wios));
        blkio_stats.push(build_blkio_stats_entry(
            s.major, s.minor, "dbytes", s.dbytes,
        ));
        blkio_stats.push(build_blkio_stats_entry(s.major, s.minor, "dios", s.dios));
    }

    resp.io_service_bytes_recursive = blkio_stats;

    MessageField::some(resp)
}

fn get_blkio_stats(cg: &cgroups::Cgroup) -> MessageField<BlkioStats> {
    if cg.v2() {
        return get_blkio_stats_v2(cg);
    }

    let blkio_controller: &BlkIoController = get_controller_or_return_singular_none!(cg);
    let blkio = blkio_controller.blkio();

    let mut m = BlkioStats::new();
    let io_serviced_recursive = blkio.io_serviced_recursive;

    if io_serviced_recursive.is_empty() {
        // fall back to generic stats
        // blkio.throttle.io_service_bytes,
        // maybe io_service_bytes_recursive?
        // stick to runc for now
        m.io_service_bytes_recursive = get_blkio_stat_ioservice(&blkio.throttle.io_service_bytes);
        m.io_serviced_recursive = get_blkio_stat_ioservice(&blkio.throttle.io_serviced);
    } else {
        // Try to read CFQ stats available on all CFQ enabled kernels first
        // IoService type data
        m.io_service_bytes_recursive = get_blkio_stat_ioservice(&blkio.io_service_bytes_recursive);
        m.io_serviced_recursive = get_blkio_stat_ioservice(&io_serviced_recursive);
        m.io_queued_recursive = get_blkio_stat_ioservice(&blkio.io_queued_recursive);
        m.io_service_time_recursive = get_blkio_stat_ioservice(&blkio.io_service_time_recursive);
        m.io_wait_time_recursive = get_blkio_stat_ioservice(&blkio.io_wait_time_recursive);
        m.io_merged_recursive = get_blkio_stat_ioservice(&blkio.io_merged_recursive);

        // BlkIoData type data
        m.io_time_recursive = get_blkio_stat_blkiodata(&blkio.time_recursive);
        m.sectors_recursive = get_blkio_stat_blkiodata(&blkio.sectors_recursive);
    }

    MessageField::some(m)
}

fn get_hugetlb_stats(cg: &cgroups::Cgroup) -> HashMap<String, HugetlbStats> {
    let mut h = HashMap::new();

    let hugetlb_controller: Option<&HugeTlbController> = cg.controller_of();
    if hugetlb_controller.is_none() {
        return h;
    }
    let hugetlb_controller = hugetlb_controller.unwrap();

    let sizes = hugetlb_controller.get_sizes();
    for size in sizes {
        let usage = hugetlb_controller.usage_in_bytes(&size).unwrap_or(0);
        let max_usage = hugetlb_controller.max_usage_in_bytes(&size).unwrap_or(0);
        let failcnt = hugetlb_controller.failcnt(&size).unwrap_or(0);

        h.insert(
            size.to_string(),
            HugetlbStats {
                usage,
                max_usage,
                failcnt,
                ..Default::default()
            },
        );
    }

    h
}

pub const PATHS: &str = "/proc/self/cgroup";
pub const MOUNTS: &str = "/proc/self/mountinfo";

pub fn get_paths() -> Result<HashMap<String, String>> {
    let mut m = HashMap::new();
    let mut real_m = HashMap::new();
    for l in fs::read_to_string(PATHS)?.lines() {
        let fl: Vec<&str> = l.split(':').collect();
        if fl.len() != 3 {
            info!(sl!(), "Corrupted cgroup data!");
            continue;
        }

        let keys: Vec<&str> = fl[1].split(',').collect();
        for key in &keys {
            m.insert(key.to_string(), fl[2].to_string());
        }
    }
    for ctl in CUBE_CONTROLLER.to_vec() {
        if m.contains_key(&ctl) {
            real_m.insert(ctl.clone(), m.get(&ctl).unwrap().clone());
        }
    }
    Ok(real_m)
}

pub fn get_mounts() -> Result<HashMap<String, String>> {
    let mut m = HashMap::new();
    let paths = get_paths()?;

    for l in fs::read_to_string(MOUNTS)?.lines() {
        let p: Vec<&str> = l.splitn(2, " - ").collect();
        let pre: Vec<&str> = p[0].split(' ').collect();
        let post: Vec<&str> = p[1].split(' ').collect();

        if post.len() != 3 {
            warn!(sl!(), "can't parse {} line {:?}", MOUNTS, l);
            continue;
        }

        if post[0] != "cgroup" && post[0] != "cgroup2" {
            continue;
        }

        let names: Vec<&str> = post[2].split(',').collect();

        for name in &names {
            if paths.contains_key(*name) {
                m.insert(name.to_string(), pre[4].to_string());
            }
        }
    }

    Ok(m)
}

fn new_cgroup(h: Box<dyn cgroups::Hierarchy>, path: &str) -> Result<Cgroup> {
    let valid_path = path.trim_start_matches('/').to_string();

    if h.v2() {
        // Let cgroups-rs map cgroup v2 kernel controller names to its own subsystem model.
        return cgroups::Cgroup::new(h, valid_path.as_str()).context("create cgroup v2");
    }

    cgroups::Cgroup::new_with_specified_controllers(
        h,
        valid_path.as_str(),
        Some(CUBE_CONTROLLER.to_vec()),
    )
    .context("create cgroup v1")
}

impl Manager {
    pub fn new(cpath: &str) -> Result<Self> {
        let cgroup_path = cgroup_filesystem_path(cpath)?;
        let mut m = HashMap::new();
        let paths = get_paths()?;
        let mounts = get_mounts()?;

        for key in paths.keys() {
            let mnt = mounts.get(key);

            if mnt.is_none() {
                continue;
            }

            let p = format!("{}/{}", mnt.unwrap(), cpath);

            m.insert(key.to_string(), p);
        }
        let cgroup = new_cgroup(cgroups::hierarchies::auto(), cpath)?;
        let process_cgroup = if cgroup.v2() {
            let process_path = process_cgroup_path(cpath);
            match new_cgroup(cgroups::hierarchies::auto(), &process_path) {
                Ok(process_cgroup) => {
                    if let Err(error) = verify_delegated_controllers_at(&cgroup_path) {
                        let _ = process_cgroup.delete();
                        let _ = cgroup.delete();
                        return Err(error).context("verify runtime cgroup delegation");
                    }
                    Some(process_cgroup)
                }
                Err(error) => {
                    let _ = cgroup.delete();
                    return Err(error)
                        .with_context(|| format!("create runtime process cgroup {process_path}"));
                }
            }
        } else {
            None
        };

        Ok(Self {
            paths: m,
            mounts,
            // rels: paths,
            cpath: cpath.to_string(),
            cgroup,
            process_cgroup,
        })
    }

    pub fn update_cpuset_path(&self, _guest_cpuset: &str, _container_cpuset: &str) -> Result<()> {
        Ok(())
        /*
        if guest_cpuset.is_empty() {
            return Ok(());
        }
        info!(sl!(), "update_cpuset_path to: {}", guest_cpuset);

        let h = cgroups::hierarchies::auto();
        let root_cg = h.root_control_group();

        let root_cpuset_controller: &CpuSetController = root_cg.controller_of().unwrap();
        let path = root_cpuset_controller.path();
        let root_path = Path::new(path);
        info!(sl!(), "root cpuset path: {:?}", &path);

        let container_cpuset_controller: &CpuSetController = self.cgroup.controller_of().unwrap();
        let path = container_cpuset_controller.path();
        let container_path = Path::new(path);
        info!(sl!(), "container cpuset path: {:?}", &path);

        let mut paths = vec![];
        for ancestor in container_path.ancestors() {
            if ancestor == root_path {
                break;
            }
            paths.push(ancestor);
        }
        info!(sl!(), "parent paths to update cpuset: {:?}", &paths);

        let mut i = paths.len();
        loop {
            if i == 0 {
                break;
            }
            i -= 1;

            // remove cgroup root from path
            let r_path = &paths[i]
                .to_str()
                .unwrap()
                .trim_start_matches(root_path.to_str().unwrap());
            info!(sl!(), "updating cpuset for parent path {:?}", &r_path);
            let cg = new_cgroup(cgroups::hierarchies::auto(), r_path);
            let cpuset_controller: &CpuSetController = cg.controller_of().unwrap();
            cpuset_controller.set_cpus(guest_cpuset)?;
        }

        if !container_cpuset.is_empty() {
            info!(
                sl!(),
                "updating cpuset for container path: {:?} cpuset: {}",
                &container_path,
                container_cpuset
            );
            container_cpuset_controller.set_cpus(container_cpuset)?;
        }

        Ok(())
        */
    }

    pub fn get_cg_path(&self, cg: &str) -> Option<String> {
        if cgroups::hierarchies::is_cgroup2_unified_mode() {
            let cg_path = format!("/sys/fs/cgroup/{}", self.cpath);
            return Some(cg_path);
        }

        // for cgroup v1
        self.paths.get(cg).map(|s| s.to_string())
    }
}

// get the guest's online cpus.
pub fn get_guest_cpuset() -> Result<String> {
    let c = fs::read_to_string(GUEST_CPUS_PATH)?;
    Ok(c.trim().to_string())
}

// Since the OCI spec is designed for cgroup v1, in some cases
// there is need to convert from the cgroup v1 configuration to cgroup v2
// the formula for cpuShares is y = (1 + ((x - 2) * 9999) / 262142)
// convert from [2-262144] to [1-10000]
// 262144 comes from Linux kernel definition "#define MAX_SHARES (1UL << 18)"
// from https://github.com/opencontainers/runc/blob/a5847db387ae28c0ca4ebe4beee1a76900c86414/libcontainer/cgroups/utils.go#L394
pub fn convert_shares_to_v2_value(shares: u64) -> u64 {
    if shares == 0 {
        return 0;
    }
    1 + ((shares - 2) * 9999) / 262142
}

// ConvertMemorySwapToCgroupV2Value converts MemorySwap value from OCI spec
// for use by cgroup v2 drivers. A conversion is needed since Resources.MemorySwap
// is defined as memory+swap combined, while in cgroup v2 swap is a separate value.
fn convert_memory_swap_to_v2_value(memory_swap: i64, memory: i64) -> Result<i64> {
    // for compatibility with cgroup1 controller, set swap to unlimited in
    // case the memory is set to unlimited, and swap is not explicitly set,
    // treating the request as "set both memory and swap to unlimited".
    if memory == -1 && memory_swap == 0 {
        return Ok(-1);
    }
    if memory_swap == -1 || memory_swap == 0 {
        // -1 is "max", 0 is "unset", so treat as is
        return Ok(memory_swap);
    }
    // sanity checks
    if memory == 0 || memory == -1 {
        return Err(anyhow!("unable to set swap limit without memory limit"));
    }
    if memory < 0 {
        return Err(anyhow!("invalid memory value: {}", memory));
    }
    if memory_swap < memory {
        return Err(anyhow!("memory+swap limit should be >= memory limit"));
    }
    Ok(memory_swap - memory)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn resource_metrics_capability_requires_complete_v2_accounting_layout() {
        assert_eq!(resource_metrics_version_for_layout(false, false), 0);
        assert_eq!(resource_metrics_version_for_layout(false, true), 0);
        assert_eq!(resource_metrics_version_for_layout(true, false), 0);
        assert_eq!(
            resource_metrics_version_for_layout(true, true),
            RESOURCE_METRICS_VERSION_V1
        );
    }

    #[test]
    fn process_cgroup_path_uses_a_runtime_leaf() {
        assert_eq!(
            process_cgroup_path("/default/container"),
            "/default/container/runtime"
        );
        assert_eq!(
            process_cgroup_path("default/container/"),
            "default/container/runtime"
        );
    }

    #[test]
    fn cgroup_filesystem_path_stays_below_the_unified_mount() {
        assert_eq!(
            cgroup_filesystem_path("/default/container").unwrap(),
            PathBuf::from("/sys/fs/cgroup/default/container")
        );
        assert!(cgroup_filesystem_path("/default/../escape").is_err());
        assert!(cgroup_filesystem_path("/default//escape").is_err());
    }

    #[test]
    fn delegated_controller_check_requires_cpu_memory_and_pids() {
        let root = tempfile::tempdir().unwrap();
        fs::write(
            root.path().join("cgroup.controllers"),
            "cpu io memory pids\n",
        )
        .unwrap();
        fs::write(
            root.path().join("cgroup.subtree_control"),
            "cpu memory pids\n",
        )
        .unwrap();
        verify_delegated_controllers_at(root.path()).unwrap();

        fs::write(root.path().join("cgroup.subtree_control"), "cpu memory\n").unwrap();
        assert!(verify_delegated_controllers_at(root.path()).is_err());
    }

    #[test]
    fn collect_cgroup_pids_includes_nested_envd_groups() {
        let root = tempfile::tempdir().unwrap();
        let user = root.path().join("user");
        let ptys = user.join("ptys");
        fs::create_dir_all(&ptys).unwrap();
        fs::write(root.path().join("cgroup.procs"), "10\n").unwrap();
        fs::write(user.join("cgroup.procs"), "11\n").unwrap();
        fs::write(ptys.join("cgroup.procs"), "12\n11\n").unwrap();

        let mut pids = BTreeSet::new();
        collect_cgroup_pids(root.path(), &mut pids).unwrap();
        assert_eq!(pids.into_iter().collect::<Vec<_>>(), vec![10, 11, 12]);
    }

    #[test]
    fn remove_cgroup_tree_deletes_descendants_before_the_parent() {
        let parent = tempfile::tempdir().unwrap();
        let root = parent.path().join("container");
        fs::create_dir_all(root.join("user/ptys")).unwrap();
        fs::create_dir_all(root.join("runtime")).unwrap();

        remove_cgroup_tree_at(&root).unwrap();

        assert!(!root.exists());
    }

    #[test]
    fn remove_cgroup_tree_retries_after_enotempty() {
        let parent = tempfile::tempdir().unwrap();
        let root = parent.path().join("container");
        let user = root.join("user");
        fs::create_dir_all(&user).unwrap();
        let hold = user.join("hold");
        fs::write(&hold, "busy").unwrap();

        let cleanup = thread::spawn(move || {
            thread::sleep(Duration::from_millis(25));
            fs::remove_file(hold).unwrap();
        });
        remove_cgroup_tree_at(&root).unwrap();
        cleanup.join().unwrap();

        assert!(!root.exists());
    }

    #[test]
    fn parse_v2_cpu_stats_converts_microseconds_to_nanoseconds() {
        let got = parse_v2_cpu_stats(
            "usage_usec 12\nuser_usec 7\nsystem_usec 5\nnr_periods 9\nnr_throttled 2\nthrottled_usec 3\n",
        )
        .unwrap();
        assert_eq!(
            got,
            V2CpuStats {
                usage_ns: 12_000,
                user_ns: 7_000,
                system_ns: 5_000,
                periods: 9,
                throttled_periods: 2,
                throttled_time_ns: 3_000,
            }
        );
    }

    #[test]
    fn parse_v1_cpuacct_stats_preserves_nanosecond_and_tick_semantics() {
        let got =
            parse_v1_cpuacct_stats("12000\n", "user 7\nsystem 5\n", "4000 8000\n", 100).unwrap();
        assert_eq!(
            got,
            V1CpuAcctStats {
                usage_ns: 12_000,
                user_ns: 70_000_000,
                system_ns: 50_000_000,
                per_cpu_ns: vec![4_000, 8_000],
            }
        );
    }

    #[test]
    fn parse_v1_cpuacct_stats_accepts_large_ticks_when_final_nanoseconds_fit() {
        let got =
            parse_v1_cpuacct_stats("1\n", "user 20000000000\nsystem 1\n", "1\n", 100).unwrap();
        assert_eq!(got.user_ns, 200_000_000_000_000_000);
    }

    #[test]
    fn parse_v1_cpuacct_stats_rejects_bad_values_and_tick_overflow() {
        assert!(parse_v1_cpuacct_stats("bad\n", "user 1\nsystem 1\n", "1\n", 100).is_err());
        assert!(parse_v1_cpuacct_stats("1\n", "user 1\nsystem 1\n", "bad\n", 100).is_err());
        assert!(parse_v1_cpuacct_stats("1\n", "user 1\nsystem 1\n", "1\n", 0).is_err());
        assert!(
            parse_v1_cpuacct_stats("1\n", "user 18446744073709551615\nsystem 1\n", "1\n", 100,)
                .is_err()
        );
    }

    #[test]
    fn parse_v2_cpu_stats_rejects_missing_malformed_and_overflow_values() {
        let error = parse_v2_cpu_stats("usage_usec 1\n").unwrap_err();
        assert_eq!(
            error.to_string(),
            "missing required cgroup key user_usec in cpu.stat"
        );
        assert!(parse_v2_cpu_stats("usage_usec nope\nuser_usec 1\nsystem_usec 1\nnr_periods 1\nnr_throttled 1\nthrottled_usec 1\n").is_err());
        let error = parse_v2_cpu_stats("usage_usec 18446744073709552\nuser_usec 1\nsystem_usec 1\nnr_periods 1\nnr_throttled 1\nthrottled_usec 1\n").unwrap_err();
        assert_eq!(
            error.to_string(),
            "cgroup value for usage_usec in cpu.stat overflows nanoseconds"
        );
    }

    #[test]
    fn parse_v2_memory_stats_reads_current_limit_peak_and_failures() {
        let got = parse_v2_memory_stats(
            "4096\n",
            "max\n",
            "8192\n",
            "low 0\nmax 4\nome 1\noom_kill 1\n",
        )
        .unwrap();
        assert_eq!(
            got,
            V2MemoryStats {
                current: 4096,
                limit: u64::MAX,
                peak: 8192,
                failures: 4,
            }
        );
    }

    #[test]
    fn parse_v2_memory_stats_reads_numeric_limit() {
        let got = parse_v2_memory_stats("4096\n", "16384\n", "8192\n", "max 4\n").unwrap();
        assert_eq!(got.limit, 16_384);
    }

    #[test]
    fn parse_v1_memory_usage_preserves_failcnt_semantics() {
        let got = parse_v1_memory_usage("4096\n", "16384\n", "8192\n", "4\n").unwrap();
        assert_eq!(
            got,
            V1MemoryUsage {
                current: 4096,
                limit: 16_384,
                peak: 8192,
                failures: 4,
            }
        );
    }

    #[test]
    fn parse_v2_memory_stats_rejects_missing_or_malformed_values() {
        let error = parse_v2_memory_stats("4096\n", "1024\n", "8192\n", "low 0\n").unwrap_err();
        assert_eq!(
            error.to_string(),
            "missing required cgroup key max in memory.events"
        );
        assert!(parse_v2_memory_stats("bad\n", "1024\n", "8192\n", "max 1\n").is_err());
        assert!(parse_v2_memory_stats("4096\n", "bad\n", "8192\n", "max 1\n").is_err());
    }

    #[test]
    fn read_required_rejects_missing_file() {
        let dir = tempfile::tempdir().unwrap();
        assert!(read_required(&dir.path().join("cpu.stat")).is_err());
    }
}

// Copyright (c) 2019, 2020 Ant Group
//
// SPDX-License-Identifier: Apache-2.0
//

use crate::cgroups::Manager as CgroupManager;
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
use std::collections::HashMap;
use std::fs;
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
        self.cgroup.add_task(CgroupPid::from(pid as u64))?;
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
        let cpu_usage = get_cpuacct_stats(&self.cgroup);

        let throttling_data = get_cpu_stats(&self.cgroup);

        let cpu_stats = MessageField::some(CpuStats {
            cpu_usage,
            throttling_data,
            ..Default::default()
        });

        // Memorystats
        let memory_stats = get_memory_stats(&self.cgroup);

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
        let _ = self.cgroup.delete();
        Ok(())
    }

    fn get_pids(&self) -> Result<Vec<pid_t>> {
        let mem_controller: &MemController = self.cgroup.controller_of().unwrap();
        let pids = mem_controller.tasks();
        let result = pids.iter().map(|x| x.pid as i32).collect::<Vec<i32>>();

        Ok(result)
    }
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
        // the new swap, then we should set the swap first and then set the memory limit.
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

// split space separated values into an vector of u64
fn line_to_vec(line: &str) -> Vec<u64> {
    line.split_whitespace()
        .filter_map(|x| x.parse::<u64>().ok())
        .collect::<Vec<u64>>()
}

// split flat keyed values into an hashmap of <String, u64>
fn lines_to_map(content: &str) -> HashMap<String, u64> {
    content
        .lines()
        .map(|x| x.split_whitespace().collect::<Vec<&str>>())
        .filter(|x| x.len() == 2 && x[1].parse::<u64>().is_ok())
        .fold(HashMap::new(), |mut hm, x| {
            hm.insert(x[0].to_string(), x[1].parse::<u64>().unwrap());
            hm
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

fn get_cpu_stats(cg: &cgroups::Cgroup) -> MessageField<ThrottlingData> {
    let cpu_controller: &CpuController = get_controller_or_return_singular_none!(cg);
    let stat = cpu_controller.cpu().stat;
    let h = lines_to_map(&stat);

    MessageField::some(ThrottlingData {
        periods: *h.get("nr_periods").unwrap_or(&0),
        throttled_periods: *h.get("nr_throttled").unwrap_or(&0),
        throttled_time: *h.get("throttled_time").unwrap_or(&0),
        ..Default::default()
    })
}

fn get_cpuacct_stats(cg: &cgroups::Cgroup) -> MessageField<CpuUsage> {
    if let Some(cpuacct_controller) = cg.controller_of::<CpuAcctController>() {
        let cpuacct = cpuacct_controller.cpuacct();

        let h = lines_to_map(&cpuacct.stat);
        let usage_in_usermode =
            (((*h.get("user").unwrap_or(&0) * NANO_PER_SECOND) as f64) / *CLOCK_TICKS) as u64;
        let usage_in_kernelmode =
            (((*h.get("system").unwrap_or(&0) * NANO_PER_SECOND) as f64) / *CLOCK_TICKS) as u64;

        let total_usage = cpuacct.usage;

        let percpu_usage = line_to_vec(&cpuacct.usage_percpu);

        return MessageField::some(CpuUsage {
            total_usage,
            percpu_usage,
            usage_in_kernelmode,
            usage_in_usermode,
            ..Default::default()
        });
    }

    if cg.v2() {
        return MessageField::some(CpuUsage {
            total_usage: 0,
            percpu_usage: vec![],
            usage_in_kernelmode: 0,
            usage_in_usermode: 0,
            ..Default::default()
        });
    }

    // try to get from cpu controller
    let cpu_controller: &CpuController = get_controller_or_return_singular_none!(cg);
    let stat = cpu_controller.cpu().stat;
    let h = lines_to_map(&stat);
    let usage_in_usermode = *h.get("user_usec").unwrap_or(&0);
    let usage_in_kernelmode = *h.get("system_usec").unwrap_or(&0);
    let total_usage = *h.get("usage_usec").unwrap_or(&0);
    let percpu_usage = vec![];

    MessageField::some(CpuUsage {
        total_usage,
        percpu_usage,
        usage_in_kernelmode,
        usage_in_usermode,
        ..Default::default()
    })
}

fn get_memory_stats(cg: &cgroups::Cgroup) -> MessageField<MemoryStats> {
    let memory_controller: &MemController = get_controller_or_return_singular_none!(cg);

    // cache from memory stat
    let memory = memory_controller.memory_stat();
    let cache = memory.stat.cache;

    // use_hierarchy
    let value = memory.use_hierarchy;
    let use_hierarchy = value == 1;

    // gte memory data
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

fn new_cgroup(h: Box<dyn cgroups::Hierarchy>, path: &str) -> Cgroup {
    let valid_path = path.trim_start_matches('/').to_string();
    cgroups::Cgroup::new_with_specified_controllers(
        h,
        valid_path.as_str(),
        Some(CUBE_CONTROLLER.to_vec()),
    )
    .unwrap()
}

impl Manager {
    pub fn new(cpath: &str) -> Result<Self> {
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
        Ok(Self {
            paths: m,
            mounts,
            // rels: paths,
            cpath: cpath.to_string(),
            cgroup: new_cgroup(cgroups::hierarchies::auto(), cpath),
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
    fn test_line_to_vec() {
        let test_cases = vec![
            ("1 2 3", vec![1, 2, 3]),
            ("a 1 b 2 3 c", vec![1, 2, 3]),
            ("a b c", vec![]),
        ];

        for test_case in test_cases {
            let result = line_to_vec(test_case.0);
            assert_eq!(
                result, test_case.1,
                "except: {:?} for input {}",
                test_case.1, test_case.0
            );
        }
    }

    #[test]
    fn test_lines_to_map() {
        let hm1: HashMap<String, u64> = [
            ("a".to_string(), 1),
            ("b".to_string(), 2),
            ("c".to_string(), 3),
            ("e".to_string(), 5),
        ]
        .iter()
        .cloned()
        .collect();
        let hm2: HashMap<String, u64> = [("a".to_string(), 1)].iter().cloned().collect();

        let test_cases = vec![
            ("a 1\nb 2\nc 3\nd X\ne 5\n", hm1),
            ("a 1", hm2),
            ("a c", HashMap::new()),
        ];

        for test_case in test_cases {
            let result = lines_to_map(test_case.0);
            assert_eq!(
                result, test_case.1,
                "except: {:?} for input {}",
                test_case.1, test_case.0
            );
        }
    }
}

// Copyright (c) 2019 Ant Financial
//
// SPDX-License-Identifier: Apache-2.0
//

/// Linux ABI related constants.

#[cfg(target_arch = "aarch64")]
use std::fs;

pub const SYSFS_DIR: &str = "/sys";
#[cfg(any(target_arch = "x86_64", target_arch = "x86"))]
pub fn create_pci_root_bus_path() -> String {
    String::from("/devices/pci0000:00")
}

#[cfg(target_arch = "aarch64")]
pub fn create_pci_root_bus_path() -> String {
    let ret = String::from("/devices/platform/4010000000.pcie/pci0000:00");

    let acpi_root_bus_path = String::from("/devices/pci0000:00");
    let mut acpi_sysfs_dir = String::from(SYSFS_DIR);
    let mut sysfs_dir = String::from(SYSFS_DIR);
    let mut start_root_bus_path = String::from("/devices/platform/");
    let end_root_bus_path = String::from("/pci0000:00");

    // check if there is pci bus path for acpi
    acpi_sysfs_dir.push_str(&acpi_root_bus_path);
    if let Ok(_) = fs::metadata(&acpi_sysfs_dir) {
        return acpi_root_bus_path;
    }

    sysfs_dir.push_str(&start_root_bus_path);
    let entries = match fs::read_dir(sysfs_dir) {
        Ok(e) => e,
        Err(_) => return ret,
    };
    for entry in entries {
        let pathname = match entry {
            Ok(p) => p.path(),
            Err(_) => return ret,
        };
        let dir_name = match pathname.file_name() {
            Some(p) => p.to_str(),
            None => return ret,
        };
        let dir_name = match dir_name {
            Some(p) => p,
            None => return ret,
        };
        let dir_name = String::from(dir_name);
        if dir_name.ends_with(".pcie") {
            start_root_bus_path.push_str(&dir_name);
            start_root_bus_path.push_str(&end_root_bus_path);
            return start_root_bus_path;
        }
    }

    ret
}

// From https://www.kernel.org/doc/Documentation/acpi/namespace.txt
// The Linux kernel's core ACPI subsystem creates struct acpi_device
// objects for ACPI namespace objects representing devices, power resources
// processors, thermal zones. Those objects are exported to user space via
// sysfs as directories in the subtree under /sys/devices/LNXSYSTM:00

pub const SYSFS_CPU_ONLINE_PATH: &str = "/sys/devices/system/cpu";

pub const SYSFS_MEMORY_BLOCK_SIZE_PATH: &str = "/sys/devices/system/memory/block_size_bytes";
pub const SYSFS_MEMORY_HOTPLUG_PROBE_PATH: &str = "/sys/devices/system/memory/probe";
pub const SYSFS_MEMORY_ONLINE_PATH: &str = "/sys/devices/system/memory";

pub const SYSFS_SCSI_HOST_PATH: &str = "/sys/class/scsi_host";

pub const SYSFS_BUS_PCI_PATH: &str = "/sys/bus/pci";

pub const SYSFS_CGROUPPATH: &str = "/sys/fs/cgroup";
pub const SYSFS_ONLINE_FILE: &str = "online";

pub const PROC_MOUNTSTATS: &str = "/proc/self/mountstats";
pub const PROC_CGROUPS: &str = "/proc/cgroups";

pub const SYSTEM_DEV_PATH: &str = "/dev";
// Linux UEvent related consts.
pub const U_EVENT_ACTION: &str = "ACTION";
pub const U_EVENT_ACTION_ADD: &str = "add";
pub const U_EVENT_ACTION_REMOVE: &str = "remove";
pub const U_EVENT_DEV_PATH: &str = "DEVPATH";
pub const U_EVENT_SUB_SYSTEM: &str = "SUBSYSTEM";
pub const U_EVENT_SEQ_NUM: &str = "SEQNUM";
pub const U_EVENT_DEV_NAME: &str = "DEVNAME";
pub const U_EVENT_INTERFACE: &str = "INTERFACE";

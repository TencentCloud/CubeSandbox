// Copyright 2020 Arm Limited (or its affiliates). All rights reserved.
// Copyright © 2020, Oracle and/or its affiliates.
//
// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//! Implements platform specific functionality.
//! Supported platforms: x86_64, aarch64.

#[macro_use]
extern crate log;

#[cfg(target_arch = "x86_64")]
use crate::x86_64::SgxEpcSection;
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::fmt;
use std::result;
use std::sync::Arc;
use thiserror::Error;

type GuestMemoryMmap = vm_memory::GuestMemoryMmap<vm_memory::bitmap::AtomicBitmap>;
type GuestRegionMmap = vm_memory::GuestRegionMmap<vm_memory::bitmap::AtomicBitmap>;

/// Type for returning error code.
#[derive(Debug, Error)]
pub enum Error {
    #[cfg(target_arch = "x86_64")]
    #[error("Platform specific error (x86_64): {0:?}")]
    PlatformSpecific(x86_64::Error),
    #[cfg(target_arch = "aarch64")]
    #[error("Platform specific error (aarch64): {0:?}")]
    PlatformSpecific(aarch64::Error),
    #[error("The memory map table extends past the end of guest memory")]
    MemmapTablePastRamEnd,
    #[error("Error writing memory map table to guest memory")]
    MemmapTableSetup,
    #[error("The hvm_start_info structure extends past the end of guest memory")]
    StartInfoPastRamEnd,
    #[error("Error writing hvm_start_info to guest memory")]
    StartInfoSetup,
    #[error("Failed to compute initramfs address")]
    InitramfsAddress,
    #[error("Error writing module entry to guest memory: {0}")]
    ModlistSetup(#[source] vm_memory::GuestMemoryError),
    #[error("RSDP extends past the end of guest memory")]
    RsdpPastRamEnd,
}

/// Type for returning public functions outcome.
pub type Result<T> = result::Result<T, Error>;

/// Type for memory region types.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Serialize, Deserialize)]
pub enum RegionType {
    /// RAM type
    Ram,

    /// SubRegion memory region.
    /// A SubRegion is a memory region sub-region, allowing for a region
    /// to be split into sub regions managed separately.
    /// For example, the x86 32-bit memory hole is a SubRegion.
    SubRegion,

    /// Reserved type.
    /// A Reserved memory region is one that should not be used for memory
    /// allocation. This type can be used to prevent the VMM from allocating
    /// memory ranges in a specific address range.
    Reserved,
}

/// Module for aarch64 related functionality.
#[cfg(target_arch = "aarch64")]
pub mod aarch64;

#[cfg(target_arch = "aarch64")]
pub use aarch64::{
    arch_memory_regions, configure_system, configure_vcpu, fdt::DeviceInfoForFdt,
    get_host_cpu_phys_bits, initramfs_load_addr, layout, layout::CMDLINE_MAX_SIZE,
    layout::IRQ_BASE, uefi, EntryPoint,
};

#[cfg(target_arch = "x86_64")]
pub mod x86_64;

#[cfg(target_arch = "x86_64")]
pub use x86_64::{
    arch_memory_regions, configure_system, configure_vcpu, generate_common_cpuid,
    get_host_cpu_phys_bits, initramfs_load_addr, layout, layout::CMDLINE_MAX_SIZE,
    layout::CMDLINE_START, regs, CpuidFeatureEntry, EntryPoint,
};

/// Safe wrapper for `sysconf(_SC_PAGESIZE)`.
#[cfg(target_arch = "x86_64")]
#[inline(always)]
fn pagesize() -> usize {
    // Trivially safe
    unsafe { libc::sysconf(libc::_SC_PAGESIZE) as usize }
}

#[derive(Clone, Default)]
pub struct NumaNode {
    pub memory_regions: Vec<Arc<GuestRegionMmap>>,
    pub hotplug_regions: Vec<Arc<GuestRegionMmap>>,
    pub cpus: Vec<u8>,
    pub distances: BTreeMap<u32, u8>,
    pub memory_zones: Vec<String>,
    #[cfg(target_arch = "x86_64")]
    pub sgx_epc_sections: Vec<SgxEpcSection>,
}

pub type NumaNodes = BTreeMap<u32, NumaNode>;

/// Type for passing information about the initramfs in the guest memory.
pub struct InitramfsConfig {
    /// Load address of initramfs in guest memory
    pub address: vm_memory::GuestAddress,
    /// Size of initramfs in guest memory
    pub size: usize,
}

/// Types of devices that can get attached to this platform.
#[derive(Clone, Debug, PartialEq, Eq, Hash, Copy)]
pub enum DeviceType {
    /// Device Type: Virtio.
    Virtio(u32),
    /// Device Type: Serial.
    #[cfg(target_arch = "aarch64")]
    Serial,
    /// Device Type: RTC.
    #[cfg(target_arch = "aarch64")]
    Rtc,
    /// Device Type: GPIO.
    #[cfg(target_arch = "aarch64")]
    Gpio,
}

/// Default (smallest) memory page size for the supported architectures.
pub const PAGE_SIZE: usize = 4096;

impl fmt::Display for DeviceType {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        write!(f, "{:?}", self)
    }
}

/// Structure to describe MMIO device information
#[derive(Clone, Debug)]
#[cfg(target_arch = "aarch64")]
pub struct MmioDeviceInfo {
    pub addr: u64,
    pub len: u64,
    pub irq: u32,
}

/// Structure to describe PCI space information
#[derive(Clone, Debug)]
#[cfg(target_arch = "aarch64")]
pub struct PciSpaceInfo {
    pub pci_segment_id: u16,
    pub mmio_config_address: u64,
    pub pci_device_space_start: u64,
    pub pci_device_space_size: u64,
}

#[cfg(target_arch = "aarch64")]
impl DeviceInfoForFdt for MmioDeviceInfo {
    fn addr(&self) -> u64 {
        self.addr
    }
    fn irq(&self) -> u32 {
        self.irq
    }
    fn length(&self) -> u64 {
        self.len
    }
}

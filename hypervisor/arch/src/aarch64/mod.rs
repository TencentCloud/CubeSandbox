// Copyright 2020 Arm Limited (or its affiliates). All rights reserved.
// Copyright 2019 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

/// Module for the flattened device tree.
pub mod fdt;
/// Layout for this aarch64 system.
pub mod layout;
/// Module for system registers definition
pub mod regs;
/// Module for loading UEFI binary.
pub mod uefi;

pub use self::fdt::DeviceInfoForFdt;
use crate::{DeviceType, GuestMemoryMmap, NumaNodes, PciSpaceInfo, RegionType};
use hypervisor::arch::aarch64::gic::Vgic;
use log::{log_enabled, Level};
use std::collections::HashMap;
use std::convert::TryInto;
use std::fmt::Debug;
use std::sync::{Arc, Mutex};
use vm_memory::{Address, GuestAddress, GuestMemory, GuestUsize};

/// Errors thrown while configuring aarch64 system.
#[derive(Debug)]
pub enum Error {
    /// Failed to create a FDT.
    SetupFdt,

    /// Failed to write FDT to memory.
    WriteFdtToMemory(fdt::Error),

    /// Failed to create a GIC.
    SetupGic,

    /// Failed to compute the initramfs address.
    InitramfsAddress,

    /// Error configuring the general purpose registers
    RegsConfiguration(hypervisor::HypervisorCpuError),

    /// Error configuring the MPIDR register
    VcpuRegMpidr(hypervisor::HypervisorCpuError),

    /// Error initializing PMU for vcpu
    VcpuInitPmu,
}

impl From<Error> for super::Error {
    fn from(e: Error) -> super::Error {
        super::Error::PlatformSpecific(e)
    }
}

#[derive(Debug, Copy, Clone)]
/// Specifies the entry point address where the guest must start
/// executing code.
pub struct EntryPoint {
    /// Address in guest memory where the guest must start execution
    pub entry_addr: GuestAddress,
}

/// Configure the specified VCPU, and return its MPIDR.
pub fn configure_vcpu(
    vcpu: &Arc<dyn hypervisor::Vcpu>,
    id: u8,
    kernel_entry_point: Option<EntryPoint>,
) -> super::Result<u64> {
    if let Some(kernel_entry_point) = kernel_entry_point {
        vcpu.setup_regs(
            id,
            kernel_entry_point.entry_addr.raw_value(),
            super::layout::FDT_START.raw_value(),
        )
        .map_err(Error::RegsConfiguration)?;
    }

    let mpidr = vcpu
        .get_sys_reg(regs::MPIDR_EL1)
        .map_err(Error::VcpuRegMpidr)?;
    Ok(mpidr)
}

pub fn arch_memory_regions(size: GuestUsize) -> Vec<(GuestAddress, usize, RegionType)> {
    let mut regions = vec![
        // 0 MiB ~ 256 MiB: UEFI, GIC and legacy devices
        (
            GuestAddress(0),
            layout::MEM_32BIT_DEVICES_START.0 as usize,
            RegionType::Reserved,
        ),
        // 256 MiB ~ 768 MiB: MMIO space
        (
            layout::MEM_32BIT_DEVICES_START,
            layout::MEM_32BIT_DEVICES_SIZE as usize,
            RegionType::SubRegion,
        ),
        // 768 MiB ~ 1 GiB: reserved. The leading 256M for PCIe MMCONFIG space
        (
            layout::PCI_MMCONFIG_START,
            layout::PCI_MMCONFIG_SIZE as usize,
            RegionType::Reserved,
        ),
    ];

    let ram_32bit_space_size =
        layout::MEM_32BIT_RESERVED_START.unchecked_offset_from(layout::RAM_START);

    // RAM space
    // Case1: guest memory fits before the gap
    if size as u64 <= ram_32bit_space_size {
        regions.push((layout::RAM_START, size as usize, RegionType::Ram));
    // Case2: guest memory extends beyond the gap
    } else {
        // Push memory before the gap
        regions.push((
            layout::RAM_START,
            ram_32bit_space_size as usize,
            RegionType::Ram,
        ));
        // Other memory is placed after 4GiB
        regions.push((
            layout::RAM_64BIT_START,
            (size - ram_32bit_space_size) as usize,
            RegionType::Ram,
        ));
    }

    // Add the 32-bit reserved memory hole as a reserved region
    regions.push((
        layout::MEM_32BIT_RESERVED_START,
        layout::MEM_32BIT_RESERVED_SIZE as usize,
        RegionType::Reserved,
    ));

    regions
}

/// Configures the system and should be called once per vm before starting vcpu threads.
#[allow(clippy::too_many_arguments)]
pub fn configure_system<T: DeviceInfoForFdt + Clone + Debug, S: ::std::hash::BuildHasher>(
    guest_mem: &GuestMemoryMmap,
    cmdline: &str,
    vcpu_mpidr: Vec<u64>,
    vcpu_topology: Option<(u8, u8, u8)>,
    device_info: &HashMap<(DeviceType, String), T, S>,
    initrd: &Option<super::InitramfsConfig>,
    pci_space_info: &[PciSpaceInfo],
    virtio_iommu_bdf: Option<u32>,
    gic_device: &Arc<Mutex<dyn Vgic>>,
    numa_nodes: &NumaNodes,
    pmu_supported: bool,
) -> super::Result<()> {
    let fdt_final = fdt::create_fdt(
        guest_mem,
        cmdline,
        vcpu_mpidr,
        vcpu_topology,
        device_info,
        gic_device,
        initrd,
        pci_space_info,
        numa_nodes,
        virtio_iommu_bdf,
        pmu_supported,
    )
    .map_err(|_| Error::SetupFdt)?;

    if log_enabled!(Level::Debug) {
        fdt::print_fdt(&fdt_final);
    }

    fdt::write_fdt_to_memory(fdt_final, guest_mem).map_err(Error::WriteFdtToMemory)?;

    Ok(())
}

/// Returns the memory address where the initramfs could be loaded.
pub fn initramfs_load_addr(
    guest_mem: &GuestMemoryMmap,
    initramfs_size: usize,
) -> super::Result<u64> {
    let round_to_pagesize = |size| (size + (super::PAGE_SIZE - 1)) & !(super::PAGE_SIZE - 1);
    match guest_mem
        .last_addr()
        .checked_sub(round_to_pagesize(initramfs_size) as u64 - 1)
    {
        Some(offset) => {
            if guest_mem.address_in_range(offset) {
                Ok(offset.raw_value())
            } else {
                Err(super::Error::PlatformSpecific(Error::InitramfsAddress))
            }
        }
        None => Err(super::Error::PlatformSpecific(Error::InitramfsAddress)),
    }
}

pub fn get_host_cpu_phys_bits() -> u8 {
    // A dummy hypervisor created only for querying the host IPA size and will
    // be freed after the query.
    let hv = hypervisor::new().unwrap();
    let host_cpu_phys_bits = hv.get_host_ipa_limit().try_into().unwrap();
    if host_cpu_phys_bits == 0 {
        // Host kernel does not support `get_host_ipa_limit`,
        // we return the default value 40 here.
        40
    } else {
        host_cpu_phys_bits
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_arch_memory_regions_dram_2gb() {
        let regions = arch_memory_regions((1usize << 31) as u64); //2GB
        assert_eq!(5, regions.len());
        assert_eq!(layout::RAM_START, regions[3].0);
        assert_eq!((1usize << 31), regions[3].1);
        assert_eq!(RegionType::Ram, regions[3].2);
        assert_eq!(RegionType::Reserved, regions[4].2);
    }

    #[test]
    fn test_arch_memory_regions_dram_4gb() {
        let regions = arch_memory_regions((1usize << 32) as u64); //4GB
        let ram_32bit_space_size =
            layout::MEM_32BIT_RESERVED_START.unchecked_offset_from(layout::RAM_START) as usize;
        assert_eq!(6, regions.len());
        assert_eq!(layout::RAM_START, regions[3].0);
        assert_eq!(ram_32bit_space_size as usize, regions[3].1);
        assert_eq!(RegionType::Ram, regions[3].2);
        assert_eq!(RegionType::Reserved, regions[5].2);
        assert_eq!(RegionType::Ram, regions[4].2);
        assert_eq!(((1usize << 32) - ram_32bit_space_size), regions[4].1);
    }
}

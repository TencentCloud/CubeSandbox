// Copyright 2020 Arm Limited (or its affiliates). All rights reserved.
// Copyright 2019 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the THIRD-PARTY file.

use crate::{NumaNodes, PciSpaceInfo};
use byteorder::{BigEndian, ByteOrder};
use hypervisor::arch::aarch64::gic::Vgic;
use std::cmp;
use std::collections::HashMap;
use std::ffi::CStr;
use std::fmt::Debug;
use std::result;
use std::str;
use std::sync::{Arc, Mutex};

use super::super::DeviceType;
use super::super::GuestMemoryMmap;
use super::super::InitramfsConfig;
use super::layout::{
    IRQ_BASE, MEM_32BIT_DEVICES_SIZE, MEM_32BIT_DEVICES_START, MEM_PCI_IO_SIZE, MEM_PCI_IO_START,
    PCI_HIGH_BASE, PCI_MMIO_CONFIG_SIZE_PER_SEGMENT,
};
use vm_fdt::{FdtWriter, FdtWriterResult};
use vm_memory::{Address, Bytes, GuestMemory, GuestMemoryError, GuestMemoryRegion};

// This is a value for uniquely identifying the FDT node declaring the interrupt controller.
const GIC_PHANDLE: u32 = 1;
// This is a value for uniquely identifying the FDT node declaring the MSI controller.
const MSI_PHANDLE: u32 = 2;
// This is a value for uniquely identifying the FDT node containing the clock definition.
const CLOCK_PHANDLE: u32 = 3;
// This is a value for uniquely identifying the FDT node containing the gpio controller.
const GPIO_PHANDLE: u32 = 4;
// This is a value for virtio-iommu. Now only one virtio-iommu device is supported.
const VIRTIO_IOMMU_PHANDLE: u32 = 5;
// NOTE: Keep FIRST_VCPU_PHANDLE the last PHANDLE defined.
// This is a value for uniquely identifying the FDT node containing the first vCPU.
// The last number of vCPU phandle depends on the number of vCPUs.
const FIRST_VCPU_PHANDLE: u32 = 6;

// Read the documentation specified when appending the root node to the FDT.
const ADDRESS_CELLS: u32 = 0x2;
const SIZE_CELLS: u32 = 0x2;

// As per kvm tool and
// https://www.kernel.org/doc/Documentation/devicetree/bindings/interrupt-controller/arm%2Cgic.txt
// Look for "The 1st cell..."
const GIC_FDT_IRQ_TYPE_SPI: u32 = 0;
const GIC_FDT_IRQ_TYPE_PPI: u32 = 1;

// From https://elixir.bootlin.com/linux/v4.9.62/source/include/dt-bindings/interrupt-controller/irq.h#L17
const IRQ_TYPE_EDGE_RISING: u32 = 1;
const IRQ_TYPE_LEVEL_HI: u32 = 4;

// PMU PPI interrupt number
pub const AARCH64_PMU_IRQ: u32 = 7;

// Keys and Buttons
// System Power Down
const KEY_POWER: u32 = 116;

/// Trait for devices to be added to the Flattened Device Tree.
pub trait DeviceInfoForFdt {
    /// Returns the address where this device will be loaded.
    fn addr(&self) -> u64;
    /// Returns the associated interrupt for this device.
    fn irq(&self) -> u32;
    /// Returns the amount of memory that needs to be reserved for this device.
    fn length(&self) -> u64;
}

/// Errors thrown while configuring the Flattened Device Tree for aarch64.
#[derive(Debug)]
pub enum Error {
    /// Failure in writing FDT in memory.
    WriteFdtToMemory(GuestMemoryError),
}
type Result<T> = result::Result<T, Error>;

/// Creates the flattened device tree for this aarch64 VM.
#[allow(clippy::too_many_arguments)]
pub fn create_fdt<T: DeviceInfoForFdt + Clone + Debug, S: ::std::hash::BuildHasher>(
    guest_mem: &GuestMemoryMmap,
    cmdline: &str,
    vcpu_mpidr: Vec<u64>,
    vcpu_topology: Option<(u8, u8, u8)>,
    device_info: &HashMap<(DeviceType, String), T, S>,
    gic_device: &Arc<Mutex<dyn Vgic>>,
    initrd: &Option<InitramfsConfig>,
    pci_space_info: &[PciSpaceInfo],
    numa_nodes: &NumaNodes,
    virtio_iommu_bdf: Option<u32>,
    pmu_supported: bool,
) -> FdtWriterResult<Vec<u8>> {
    // Allocate stuff necessary for the holding the blob.
    let mut fdt = FdtWriter::new().unwrap();

    // For an explanation why these nodes were introduced in the blob take a look at
    // https://github.com/torvalds/linux/blob/master/Documentation/devicetree/booting-without-of.txt#L845
    // Look for "Required nodes and properties".

    // Header or the root node as per above mentioned documentation.
    let root_node = fdt.begin_node("")?;
    fdt.property_string("compatible", "linux,dummy-virt")?;
    // For info on #address-cells and size-cells read "Note about cells and address representation"
    // from the above mentioned txt file.
    fdt.property_u32("#address-cells", ADDRESS_CELLS)?;
    fdt.property_u32("#size-cells", SIZE_CELLS)?;
    // This is not mandatory but we use it to point the root node to the node
    // containing description of the interrupt controller for this VM.
    fdt.property_u32("interrupt-parent", GIC_PHANDLE)?;
    create_cpu_nodes(&mut fdt, &vcpu_mpidr, vcpu_topology, numa_nodes)?;
    create_memory_node(&mut fdt, guest_mem, numa_nodes)?;
    create_chosen_node(&mut fdt, cmdline, initrd)?;
    create_gic_node(&mut fdt, gic_device)?;
    create_timer_node(&mut fdt)?;
    if pmu_supported {
        create_pmu_node(&mut fdt)?;
    }
    create_clock_node(&mut fdt)?;
    create_psci_node(&mut fdt)?;
    create_devices_node(&mut fdt, device_info)?;
    create_pci_nodes(&mut fdt, pci_space_info, virtio_iommu_bdf)?;
    if numa_nodes.len() > 1 {
        create_distance_map_node(&mut fdt, numa_nodes)?;
    }

    // End Header node.
    fdt.end_node(root_node)?;

    let fdt_final = fdt.finish()?;

    Ok(fdt_final)
}

pub fn write_fdt_to_memory(fdt_final: Vec<u8>, guest_mem: &GuestMemoryMmap) -> Result<()> {
    // Write FDT to memory.
    guest_mem
        .write_slice(fdt_final.as_slice(), super::layout::FDT_START)
        .map_err(Error::WriteFdtToMemory)?;
    Ok(())
}

// Following are the auxiliary function for creating the different nodes that we append to our FDT.
fn create_cpu_nodes(
    fdt: &mut FdtWriter,
    vcpu_mpidr: &[u64],
    vcpu_topology: Option<(u8, u8, u8)>,
    numa_nodes: &NumaNodes,
) -> FdtWriterResult<()> {
    // See https://github.com/torvalds/linux/blob/master/Documentation/devicetree/bindings/arm/cpus.yaml.
    let cpus_node = fdt.begin_node("cpus")?;
    fdt.property_u32("#address-cells", 0x1)?;
    fdt.property_u32("#size-cells", 0x0)?;

    let num_cpus = vcpu_mpidr.len();

    for (cpu_id, mpidr) in vcpu_mpidr.iter().enumerate().take(num_cpus) {
        let cpu_name = format!("cpu@{:x}", cpu_id);
        let cpu_node = fdt.begin_node(&cpu_name)?;
        fdt.property_string("device_type", "cpu")?;
        fdt.property_string("compatible", "arm,arm-v8")?;
        if num_cpus > 1 {
            // This is required on armv8 64-bit. See aforementioned documentation.
            fdt.property_string("enable-method", "psci")?;
        }
        // Set the field to first 24 bits of the MPIDR - Multiprocessor Affinity Register.
        // See http://infocenter.arm.com/help/index.jsp?topic=/com.arm.doc.ddi0488c/BABHBJCI.html.
        fdt.property_u32("reg", (mpidr & 0x7FFFFF) as u32)?;
        fdt.property_u32("phandle", cpu_id as u32 + FIRST_VCPU_PHANDLE)?;

        // Add `numa-node-id` property if there is any numa config.
        if numa_nodes.len() > 1 {
            for numa_node_idx in 0..numa_nodes.len() {
                let numa_node = numa_nodes.get(&(numa_node_idx as u32));
                if numa_node.unwrap().cpus.contains(&(cpu_id as u8)) {
                    fdt.property_u32("numa-node-id", numa_node_idx as u32)?;
                }
            }
        }

        fdt.end_node(cpu_node)?;
    }

    if let Some(topology) = vcpu_topology {
        let (threads_per_core, cores_per_package, packages) = topology;
        let cpu_map_node = fdt.begin_node("cpu-map")?;

        // Create device tree nodes with regard of above mapping.
        for cluster_idx in 0..packages {
            let cluster_name = format!("cluster{:x}", cluster_idx);
            let cluster_node = fdt.begin_node(&cluster_name)?;

            for core_idx in 0..cores_per_package {
                let core_name = format!("core{:x}", core_idx);
                let core_node = fdt.begin_node(&core_name)?;

                for thread_idx in 0..threads_per_core {
                    let thread_name = format!("thread{:x}", thread_idx);
                    let thread_node = fdt.begin_node(&thread_name)?;
                    let cpu_idx = threads_per_core * cores_per_package * cluster_idx
                        + threads_per_core * core_idx
                        + thread_idx;
                    fdt.property_u32("cpu", cpu_idx as u32 + FIRST_VCPU_PHANDLE)?;
                    fdt.end_node(thread_node)?;
                }

                fdt.end_node(core_node)?;
            }
            fdt.end_node(cluster_node)?;
        }
        fdt.end_node(cpu_map_node)?;
    } else {
        debug!("Boot using device tree, CPU topology is not (correctly) specified");
    }

    fdt.end_node(cpus_node)?;

    Ok(())
}

fn create_memory_node(
    fdt: &mut FdtWriter,
    guest_mem: &GuestMemoryMmap,
    numa_nodes: &NumaNodes,
) -> FdtWriterResult<()> {
    // See https://github.com/torvalds/linux/blob/58ae0b51506802713aa0e9956d1853ba4c722c98/Documentation/devicetree/bindings/numa.txt
    // for NUMA setting in memory node.
    if numa_nodes.len() > 1 {
        for numa_node_idx in 0..numa_nodes.len() {
            let numa_node = numa_nodes.get(&(numa_node_idx as u32));
            let mut mem_reg_prop: Vec<u64> = Vec::new();
            let mut node_memory_addr: u64 = 0;
            // Each memory zone of numa will have its own memory node, but
            // different numa nodes should not share same memory zones.
            for memory_region in numa_node.unwrap().memory_regions.iter() {
                let memory_region_start_addr: u64 = memory_region.start_addr().raw_value();
                let memory_region_size: u64 = memory_region.size() as u64;
                mem_reg_prop.push(memory_region_start_addr);
                mem_reg_prop.push(memory_region_size);
                // Set the node address the first non-zero regison address
                if node_memory_addr == 0 {
                    node_memory_addr = memory_region_start_addr;
                }
            }
            let memory_node_name = format!("memory@{:x}", node_memory_addr);
            let memory_node = fdt.begin_node(&memory_node_name)?;
            fdt.property_string("device_type", "memory")?;
            fdt.property_array_u64("reg", &mem_reg_prop)?;
            fdt.property_u32("numa-node-id", numa_node_idx as u32)?;
            fdt.end_node(memory_node)?;
        }
    } else {
        let last_addr = guest_mem.last_addr().raw_value();
        if last_addr < super::layout::MEM_32BIT_RESERVED_START.raw_value() {
            // Case 1: all RAM is under the hole
            let mem_size = last_addr - super::layout::RAM_START.raw_value() + 1;
            let mem_reg_prop = [super::layout::RAM_START.raw_value() as u64, mem_size as u64];
            let memory_node = fdt.begin_node("memory")?;
            fdt.property_string("device_type", "memory")?;
            fdt.property_array_u64("reg", &mem_reg_prop)?;
            fdt.end_node(memory_node)?;
        } else {
            // Case 2: RAM is split by the hole
            // Region 1: RAM before the hole
            let mem_size = super::layout::MEM_32BIT_RESERVED_START.raw_value()
                - super::layout::RAM_START.raw_value();
            let mem_reg_prop = [super::layout::RAM_START.raw_value() as u64, mem_size as u64];
            let memory_node_name = format!("memory@{:x}", super::layout::RAM_START.raw_value());
            let memory_node = fdt.begin_node(&memory_node_name)?;
            fdt.property_string("device_type", "memory")?;
            fdt.property_array_u64("reg", &mem_reg_prop)?;
            fdt.end_node(memory_node)?;

            // Region 2: RAM after the hole
            let mem_size = last_addr - super::layout::RAM_64BIT_START.raw_value() + 1;
            let mem_reg_prop = [
                super::layout::RAM_64BIT_START.raw_value() as u64,
                mem_size as u64,
            ];
            let memory_node_name =
                format!("memory@{:x}", super::layout::RAM_64BIT_START.raw_value());
            let memory_node = fdt.begin_node(&memory_node_name)?;
            fdt.property_string("device_type", "memory")?;
            fdt.property_array_u64("reg", &mem_reg_prop)?;
            fdt.end_node(memory_node)?;
        }
    }

    Ok(())
}

fn create_chosen_node(
    fdt: &mut FdtWriter,
    cmdline: &str,
    initrd: &Option<InitramfsConfig>,
) -> FdtWriterResult<()> {
    let chosen_node = fdt.begin_node("chosen")?;
    fdt.property_string("bootargs", cmdline)?;

    if let Some(initrd_config) = initrd {
        let initrd_start = initrd_config.address.raw_value() as u64;
        let initrd_end = initrd_config.address.raw_value() + initrd_config.size as u64;
        fdt.property_u64("linux,initrd-start", initrd_start)?;
        fdt.property_u64("linux,initrd-end", initrd_end)?;
    }

    fdt.end_node(chosen_node)?;

    Ok(())
}

fn create_gic_node(fdt: &mut FdtWriter, gic_device: &Arc<Mutex<dyn Vgic>>) -> FdtWriterResult<()> {
    let gic_reg_prop = gic_device.lock().unwrap().device_properties();

    let intc_node = fdt.begin_node("intc")?;

    fdt.property_string("compatible", gic_device.lock().unwrap().fdt_compatibility())?;
    fdt.property_null("interrupt-controller")?;
    // "interrupt-cells" field specifies the number of cells needed to encode an
    // interrupt source. The type shall be a <u32> and the value shall be 3 if no PPI affinity description
    // is required.
    fdt.property_u32("#interrupt-cells", 3)?;
    fdt.property_array_u64("reg", &gic_reg_prop)?;
    fdt.property_u32("phandle", GIC_PHANDLE)?;
    fdt.property_u32("#address-cells", 2)?;
    fdt.property_u32("#size-cells", 2)?;
    fdt.property_null("ranges")?;

    let gic_intr_prop = [
        GIC_FDT_IRQ_TYPE_PPI,
        gic_device.lock().unwrap().fdt_maint_irq(),
        IRQ_TYPE_LEVEL_HI,
    ];
    fdt.property_array_u32("interrupts", &gic_intr_prop)?;

    if gic_device.lock().unwrap().msi_compatible() {
        let msic_node = fdt.begin_node("msic")?;
        fdt.property_string("compatible", gic_device.lock().unwrap().msi_compatibility())?;
        fdt.property_null("msi-controller")?;
        fdt.property_u32("phandle", MSI_PHANDLE)?;
        let msi_reg_prop = gic_device.lock().unwrap().msi_properties();
        fdt.property_array_u64("reg", &msi_reg_prop)?;
        fdt.end_node(msic_node)?;
    }

    fdt.end_node(intc_node)?;

    Ok(())
}

fn create_clock_node(fdt: &mut FdtWriter) -> FdtWriterResult<()> {
    // The Advanced Peripheral Bus (APB) is part of the Advanced Microcontroller Bus Architecture
    // (AMBA) protocol family. It defines a low-cost interface that is optimized for minimal power
    // consumption and reduced interface complexity.
    // PCLK is the clock source and this node defines exactly the clock for the APB.
    let clock_node = fdt.begin_node("apb-pclk")?;
    fdt.property_string("compatible", "fixed-clock")?;
    fdt.property_u32("#clock-cells", 0x0)?;
    fdt.property_u32("clock-frequency", 24000000)?;
    fdt.property_string("clock-output-names", "clk24mhz")?;
    fdt.property_u32("phandle", CLOCK_PHANDLE)?;
    fdt.end_node(clock_node)?;

    Ok(())
}

fn create_timer_node(fdt: &mut FdtWriter) -> FdtWriterResult<()> {
    // See
    // https://github.com/torvalds/linux/blob/master/Documentation/devicetree/bindings/interrupt-controller/arch_timer.txt
    // These are fixed interrupt numbers for the timer device.
    let irqs = [13, 14, 11, 10];
    let compatible = "arm,armv8-timer";

    let mut timer_reg_cells: Vec<u32> = Vec::new();
    for &irq in irqs.iter() {
        timer_reg_cells.push(GIC_FDT_IRQ_TYPE_PPI);
        timer_reg_cells.push(irq);
        timer_reg_cells.push(IRQ_TYPE_LEVEL_HI);
    }

    let timer_node = fdt.begin_node("timer")?;
    fdt.property_string("compatible", compatible)?;
    fdt.property_null("always-on")?;
    fdt.property_array_u32("interrupts", &timer_reg_cells)?;
    fdt.end_node(timer_node)?;

    Ok(())
}

fn create_psci_node(fdt: &mut FdtWriter) -> FdtWriterResult<()> {
    let compatible = "arm,psci-0.2";
    let psci_node = fdt.begin_node("psci")?;
    fdt.property_string("compatible", compatible)?;
    // Two methods available: hvc and smc.
    // As per documentation, PSCI calls between a guest and hypervisor may use the HVC conduit instead of SMC.
    // So, since we are using kvm, we need to use hvc.
    fdt.property_string("method", "hvc")?;
    fdt.end_node(psci_node)?;

    Ok(())
}

fn create_virtio_node<T: DeviceInfoForFdt + Clone + Debug>(
    fdt: &mut FdtWriter,
    dev_info: &T,
) -> FdtWriterResult<()> {
    let device_reg_prop = [dev_info.addr(), dev_info.length()];
    let irq = [GIC_FDT_IRQ_TYPE_SPI, dev_info.irq(), IRQ_TYPE_EDGE_RISING];

    let virtio_node = fdt.begin_node(&format!("virtio_mmio@{:x}", dev_info.addr()))?;
    fdt.property_string("compatible", "virtio,mmio")?;
    fdt.property_array_u64("reg", &device_reg_prop)?;
    fdt.property_array_u32("interrupts", &irq)?;
    fdt.property_u32("interrupt-parent", GIC_PHANDLE)?;
    fdt.end_node(virtio_node)?;

    Ok(())
}

fn create_serial_node<T: DeviceInfoForFdt + Clone + Debug>(
    fdt: &mut FdtWriter,
    dev_info: &T,
) -> FdtWriterResult<()> {
    let compatible = b"arm,pl011\0arm,primecell\0";
    let serial_reg_prop = [dev_info.addr(), dev_info.length()];
    let irq = [
        GIC_FDT_IRQ_TYPE_SPI,
        dev_info.irq() - IRQ_BASE,
        IRQ_TYPE_EDGE_RISING,
    ];

    let serial_node = fdt.begin_node(&format!("pl011@{:x}", dev_info.addr()))?;
    fdt.property("compatible", compatible)?;
    fdt.property_array_u64("reg", &serial_reg_prop)?;
    fdt.property_u32("clocks", CLOCK_PHANDLE)?;
    fdt.property_string("clock-names", "apb_pclk")?;
    fdt.property_array_u32("interrupts", &irq)?;
    fdt.end_node(serial_node)?;

    Ok(())
}

fn create_rtc_node<T: DeviceInfoForFdt + Clone + Debug>(
    fdt: &mut FdtWriter,
    dev_info: &T,
) -> FdtWriterResult<()> {
    let compatible = b"arm,pl031\0arm,primecell\0";
    let rtc_reg_prop = [dev_info.addr(), dev_info.length()];
    let irq = [
        GIC_FDT_IRQ_TYPE_SPI,
        dev_info.irq() - IRQ_BASE,
        IRQ_TYPE_LEVEL_HI,
    ];

    let rtc_node = fdt.begin_node(&format!("rtc@{:x}", dev_info.addr()))?;
    fdt.property("compatible", compatible)?;
    fdt.property_array_u64("reg", &rtc_reg_prop)?;
    fdt.property_array_u32("interrupts", &irq)?;
    fdt.property_u32("clocks", CLOCK_PHANDLE)?;
    fdt.property_string("clock-names", "apb_pclk")?;
    fdt.end_node(rtc_node)?;

    Ok(())
}

fn create_gpio_node<T: DeviceInfoForFdt + Clone + Debug>(
    fdt: &mut FdtWriter,
    dev_info: &T,
) -> FdtWriterResult<()> {
    // PL061 GPIO controller node
    let compatible = b"arm,pl061\0arm,primecell\0";
    let gpio_reg_prop = [dev_info.addr(), dev_info.length()];
    let irq = [
        GIC_FDT_IRQ_TYPE_SPI,
        dev_info.irq() - IRQ_BASE,
        IRQ_TYPE_EDGE_RISING,
    ];

    let gpio_node = fdt.begin_node(&format!("pl061@{:x}", dev_info.addr()))?;
    fdt.property("compatible", compatible)?;
    fdt.property_array_u64("reg", &gpio_reg_prop)?;
    fdt.property_array_u32("interrupts", &irq)?;
    fdt.property_null("gpio-controller")?;
    fdt.property_u32("#gpio-cells", 2)?;
    fdt.property_u32("clocks", CLOCK_PHANDLE)?;
    fdt.property_string("clock-names", "apb_pclk")?;
    fdt.property_u32("phandle", GPIO_PHANDLE)?;
    fdt.end_node(gpio_node)?;

    // gpio-keys node
    let gpio_keys_node = fdt.begin_node("gpio-keys")?;
    fdt.property_string("compatible", "gpio-keys")?;
    fdt.property_u32("#size-cells", 0)?;
    fdt.property_u32("#address-cells", 1)?;
    let gpio_keys_poweroff_node = fdt.begin_node("button@1")?;
    fdt.property_string("label", "GPIO Key Poweroff")?;
    fdt.property_u32("linux,code", KEY_POWER)?;
    let gpios = [GPIO_PHANDLE, 3, 0];
    fdt.property_array_u32("gpios", &gpios)?;
    fdt.end_node(gpio_keys_poweroff_node)?;
    fdt.end_node(gpio_keys_node)?;

    Ok(())
}

fn create_devices_node<T: DeviceInfoForFdt + Clone + Debug, S: ::std::hash::BuildHasher>(
    fdt: &mut FdtWriter,
    dev_info: &HashMap<(DeviceType, String), T, S>,
) -> FdtWriterResult<()> {
    // Create one temp Vec to store all virtio devices
    let mut ordered_virtio_device: Vec<&T> = Vec::new();

    for ((device_type, _device_id), info) in dev_info {
        match device_type {
            DeviceType::Gpio => create_gpio_node(fdt, info)?,
            DeviceType::Rtc => create_rtc_node(fdt, info)?,
            DeviceType::Serial => create_serial_node(fdt, info)?,
            DeviceType::Virtio(_) => {
                ordered_virtio_device.push(info);
            }
        }
    }

    // Sort out virtio devices by address from low to high and insert them into fdt table.
    ordered_virtio_device.sort_by_key(|&a| a.addr());
    // Current address allocation strategy in cloud-hypervisor is: the first created device
    // will be allocated to higher address. Here we reverse the vector to make sure that
    // the older created device will appear in front of the newer created device in FDT.
    ordered_virtio_device.reverse();
    for ordered_device_info in ordered_virtio_device.drain(..) {
        create_virtio_node(fdt, ordered_device_info)?;
    }

    Ok(())
}

fn create_pmu_node(fdt: &mut FdtWriter) -> FdtWriterResult<()> {
    let compatible = "arm,armv8-pmuv3";
    let irq = [GIC_FDT_IRQ_TYPE_PPI, AARCH64_PMU_IRQ, IRQ_TYPE_LEVEL_HI];

    let pmu_node = fdt.begin_node("pmu")?;
    fdt.property_string("compatible", compatible)?;
    fdt.property_array_u32("interrupts", &irq)?;
    fdt.end_node(pmu_node)?;
    Ok(())
}

fn create_pci_nodes(
    fdt: &mut FdtWriter,
    pci_device_info: &[PciSpaceInfo],
    virtio_iommu_bdf: Option<u32>,
) -> FdtWriterResult<()> {
    // Add node for PCIe controller.
    // See Documentation/devicetree/bindings/pci/host-generic-pci.txt in the kernel
    // and https://elinux.org/Device_Tree_Usage.
    // In multiple PCI segments setup, each PCI segment needs a PCI node.
    for pci_device_info_elem in pci_device_info.iter() {
        // EDK2 requires the PCIe high space above 4G address.
        // The actual space in CLH follows the RAM. If the RAM space is small, the PCIe high space
        // could fall bellow 4G.
        // Here we cut off PCI device space below 8G in FDT to workaround the EDK2 check.
        // But the address written in ACPI is not impacted.
        let (pci_device_base_64bit, pci_device_size_64bit) =
            if pci_device_info_elem.pci_device_space_start < PCI_HIGH_BASE.raw_value() {
                (
                    PCI_HIGH_BASE.raw_value(),
                    pci_device_info_elem.pci_device_space_size
                        - (PCI_HIGH_BASE.raw_value() - pci_device_info_elem.pci_device_space_start),
                )
            } else {
                (
                    pci_device_info_elem.pci_device_space_start,
                    pci_device_info_elem.pci_device_space_size,
                )
            };
        // There is no specific requirement of the 32bit MMIO range, and
        // therefore at least we can make these ranges 4K aligned.
        let pci_device_size_32bit: u64 =
            MEM_32BIT_DEVICES_SIZE / ((1 << 12) * pci_device_info.len() as u64) * (1 << 12);
        let pci_device_base_32bit: u64 = MEM_32BIT_DEVICES_START.0
            + pci_device_size_32bit * pci_device_info_elem.pci_segment_id as u64;

        let ranges = [
            // io addresses. Since AArch64 will not use IO address,
            // we can set the same IO address range for every segment.
            0x1000000,
            0_u32,
            0_u32,
            (MEM_PCI_IO_START.0 >> 32) as u32,
            MEM_PCI_IO_START.0 as u32,
            (MEM_PCI_IO_SIZE >> 32) as u32,
            MEM_PCI_IO_SIZE as u32,
            // mmio addresses
            0x2000000,                            // (ss = 10: 32-bit memory space)
            (pci_device_base_32bit >> 32) as u32, // PCI address
            pci_device_base_32bit as u32,
            (pci_device_base_32bit >> 32) as u32, // CPU address
            pci_device_base_32bit as u32,
            (pci_device_size_32bit >> 32) as u32, // size
            pci_device_size_32bit as u32,
            // device addresses
            0x3000000,                            // (ss = 11: 64-bit memory space)
            (pci_device_base_64bit >> 32) as u32, // PCI address
            pci_device_base_64bit as u32,
            (pci_device_base_64bit >> 32) as u32, // CPU address
            pci_device_base_64bit as u32,
            (pci_device_size_64bit >> 32) as u32, // size
            pci_device_size_64bit as u32,
        ];
        let bus_range = [0, 0]; // Only bus 0
        let reg = [
            pci_device_info_elem.mmio_config_address,
            PCI_MMIO_CONFIG_SIZE_PER_SEGMENT,
        ];
        // See kernel document Documentation/devicetree/bindings/pci/pci-msi.txt
        let msi_map = [
            // rid-base: A single cell describing the first RID matched by the entry.
            0x0,
            // msi-controller: A single phandle to an MSI controller.
            MSI_PHANDLE,
            // msi-base: An msi-specifier describing the msi-specifier produced for the
            // first RID matched by the entry.
            (pci_device_info_elem.pci_segment_id as u32) << 8,
            // length: A single cell describing how many consecutive RIDs are matched
            // following the rid-base.
            0x100,
        ];

        let pci_node_name = format!("pci@{:x}", pci_device_info_elem.mmio_config_address);
        let pci_node = fdt.begin_node(&pci_node_name)?;

        fdt.property_string("compatible", "pci-host-ecam-generic")?;
        fdt.property_string("device_type", "pci")?;
        fdt.property_array_u32("ranges", &ranges)?;
        fdt.property_array_u32("bus-range", &bus_range)?;
        fdt.property_u32(
            "linux,pci-domain",
            pci_device_info_elem.pci_segment_id as u32,
        )?;
        fdt.property_u32("#address-cells", 3)?;
        fdt.property_u32("#size-cells", 2)?;
        fdt.property_array_u64("reg", &reg)?;
        fdt.property_u32("#interrupt-cells", 1)?;
        fdt.property_null("interrupt-map")?;
        fdt.property_null("interrupt-map-mask")?;
        fdt.property_null("dma-coherent")?;
        fdt.property_array_u32("msi-map", &msi_map)?;
        fdt.property_u32("msi-parent", MSI_PHANDLE)?;

        if pci_device_info_elem.pci_segment_id == 0 {
            if let Some(virtio_iommu_bdf) = virtio_iommu_bdf {
                // See kernel document Documentation/devicetree/bindings/pci/pci-iommu.txt
                // for 'iommu-map' attribute setting.
                let iommu_map = [
                    0_u32,
                    VIRTIO_IOMMU_PHANDLE,
                    0_u32,
                    virtio_iommu_bdf,
                    virtio_iommu_bdf + 1,
                    VIRTIO_IOMMU_PHANDLE,
                    virtio_iommu_bdf + 1,
                    0xffff - virtio_iommu_bdf,
                ];
                fdt.property_array_u32("iommu-map", &iommu_map)?;

                // See kernel document Documentation/devicetree/bindings/virtio/iommu.txt
                // for virtio-iommu node settings.
                let virtio_iommu_node_name = format!("virtio_iommu@{:x}", virtio_iommu_bdf);
                let virtio_iommu_node = fdt.begin_node(&virtio_iommu_node_name)?;
                fdt.property_u32("#iommu-cells", 1)?;
                fdt.property_string("compatible", "virtio,pci-iommu")?;

                // 'reg' is a five-cell address encoded as
                // (phys.hi phys.mid phys.lo size.hi size.lo). phys.hi should contain the
                // device's BDF as 0b00000000 bbbbbbbb dddddfff 00000000. The other cells
                // should be zero.
                let reg = [virtio_iommu_bdf << 8, 0_u32, 0_u32, 0_u32, 0_u32];
                fdt.property_array_u32("reg", &reg)?;
                fdt.property_u32("phandle", VIRTIO_IOMMU_PHANDLE)?;

                fdt.end_node(virtio_iommu_node)?;
            }
        }

        fdt.end_node(pci_node)?;
    }

    Ok(())
}

fn create_distance_map_node(fdt: &mut FdtWriter, numa_nodes: &NumaNodes) -> FdtWriterResult<()> {
    let distance_map_node = fdt.begin_node("distance-map")?;
    fdt.property_string("compatible", "numa-distance-map-v1")?;
    // Construct the distance matrix.
    // 1. We use the word entry to describe a distance from a node to
    // its destination, e.g. 0 -> 1 = 20 is described as <0 1 20>.
    // 2. Each entry represents distance from first node to second node.
    // The distances are equal in either direction.
    // 3. The distance from a node to self (local distance) is represented
    // with value 10 and all internode distance should be represented with
    // a value greater than 10.
    // 4. distance-matrix should have entries in lexicographical ascending
    // order of nodes.
    let mut distance_matrix = Vec::new();
    for numa_node_idx in 0..numa_nodes.len() {
        let numa_node = numa_nodes.get(&(numa_node_idx as u32));
        for dest_numa_node in 0..numa_node.unwrap().distances.len() + 1 {
            if numa_node_idx == dest_numa_node {
                distance_matrix.push(numa_node_idx as u32);
                distance_matrix.push(dest_numa_node as u32);
                distance_matrix.push(10_u32);
                continue;
            }

            distance_matrix.push(numa_node_idx as u32);
            distance_matrix.push(dest_numa_node as u32);
            distance_matrix.push(
                *numa_node
                    .unwrap()
                    .distances
                    .get(&(dest_numa_node as u32))
                    .unwrap() as u32,
            );
        }
    }
    fdt.property_array_u32("distance-matrix", distance_matrix.as_ref())?;
    fdt.end_node(distance_map_node)?;

    Ok(())
}

// Parse the DTB binary and print for debugging
pub fn print_fdt(dtb: &[u8]) {
    match fdt_parser::Fdt::new(dtb) {
        Ok(fdt) => {
            if let Some(root) = fdt.find_node("/") {
                debug!("Printing the FDT:");
                print_node(root, 0);
            } else {
                debug!("Failed to find root node in FDT for debugging.");
            }
        }
        Err(_) => debug!("Failed to parse FDT for debugging."),
    }
}

fn print_node(node: fdt_parser::node::FdtNode<'_, '_>, n_spaces: usize) {
    debug!("{:indent$}{}/", "", node.name, indent = n_spaces);
    for property in node.properties() {
        let name = property.name;

        // If the property is 'compatible', its value requires special handling.
        // The u8 array could contain multiple null-terminated strings.
        // We copy the original array and simply replace all 'null' characters with spaces.
        let value = if name == "compatible" {
            let mut compatible = vec![0u8; 256];
            let handled_value = property
                .value
                .iter()
                .map(|&c| if c == 0 { b' ' } else { c })
                .collect::<Vec<_>>();
            let len = cmp::min(255, handled_value.len());
            compatible[..len].copy_from_slice(&handled_value[..len]);
            compatible[..(len + 1)].to_vec()
        } else {
            property.value.to_vec()
        };
        let value = &value;

        // Now the value can be either:
        //   - A null-terminated C string, or
        //   - Binary data
        // We follow a very simple logic to present the value:
        //   - At first, try to convert it to CStr and print,
        //   - If failed, print it as u32 array.
        let value_result = match CStr::from_bytes_with_nul(value) {
            Ok(value_cstr) => match value_cstr.to_str() {
                Ok(value_str) => Some(value_str),
                Err(_e) => None,
            },
            Err(_e) => None,
        };

        if let Some(value_str) = value_result {
            debug!(
                "{:indent$}{} : {:#?}",
                "",
                name,
                value_str,
                indent = (n_spaces + 2)
            );
        } else {
            let mut array = Vec::with_capacity(256);
            array.resize(value.len() / 4, 0u32);
            BigEndian::read_u32_into(value, &mut array);
            debug!(
                "{:indent$}{} : {:X?}",
                "",
                name,
                array,
                indent = (n_spaces + 2)
            );
        };
    }

    // Print children nodes if there is any
    for child in node.children() {
        print_node(child, n_spaces + 2);
    }
}

// Copyright © 2020, Oracle and/or its affiliates.
//
// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.
use crate::layout::{BOOT_GDT_START, BOOT_IDT_START, PVH_INFO_START};
use crate::GuestMemoryMmap;
use hypervisor::arch::x86::gdt::{gdt_entry, segment_from_gdt};
use hypervisor::arch::x86::regs::CR0_PE;
use hypervisor::arch::x86::{FpuState, SpecialRegisters, StandardRegisters};
use std::sync::Arc;
use std::{mem, result};
use vm_memory::{Address, Bytes, GuestMemory, GuestMemoryError};

#[derive(Debug)]
pub enum Error {
    /// Failed to get SREGs for this CPU.
    GetStatusRegisters(hypervisor::HypervisorCpuError),
    /// Failed to set base registers for this CPU.
    SetBaseRegisters(hypervisor::HypervisorCpuError),
    /// Failed to configure the FPU.
    SetFpuRegisters(hypervisor::HypervisorCpuError),
    /// Setting up MSRs failed.
    SetModelSpecificRegisters(hypervisor::HypervisorCpuError),
    /// Failed to set SREGs for this CPU.
    SetStatusRegisters(hypervisor::HypervisorCpuError),
    /// Checking the GDT address failed.
    CheckGdtAddr,
    /// Writing the GDT to RAM failed.
    WriteGdt(GuestMemoryError),
    /// Writing the IDT to RAM failed.
    WriteIdt(GuestMemoryError),
    /// Writing PDPTE to RAM failed.
    WritePdpteAddress(GuestMemoryError),
    /// Writing PDE to RAM failed.
    WritePdeAddress(GuestMemoryError),
    /// Writing PML4 to RAM failed.
    WritePml4Address(GuestMemoryError),
    /// Writing PML5 to RAM failed.
    WritePml5Address(GuestMemoryError),
}

pub type Result<T> = result::Result<T, Error>;

/// Configure Floating-Point Unit (FPU) registers for a given CPU.
///
/// # Arguments
///
/// * `vcpu` - Structure for the VCPU that holds the VCPU's fd.
pub fn setup_fpu(vcpu: &Arc<dyn hypervisor::Vcpu>) -> Result<()> {
    let fpu: FpuState = FpuState {
        fcw: 0x37f,
        mxcsr: 0x1f80,
        ..Default::default()
    };

    vcpu.set_fpu(&fpu).map_err(Error::SetFpuRegisters)
}

/// Configure Model Specific Registers (MSRs) for a given CPU.
///
/// # Arguments
///
/// * `vcpu` - Structure for the VCPU that holds the VCPU's fd.
pub fn setup_msrs(vcpu: &Arc<dyn hypervisor::Vcpu>) -> Result<()> {
    vcpu.set_msrs(&vcpu.boot_msr_entries())
        .map_err(Error::SetModelSpecificRegisters)?;

    Ok(())
}

/// Configure base registers for a given CPU.
///
/// # Arguments
///
/// * `vcpu` - Structure for the VCPU that holds the VCPU's fd.
/// * `boot_ip` - Starting instruction pointer.
pub fn setup_regs(vcpu: &Arc<dyn hypervisor::Vcpu>, boot_ip: u64) -> Result<()> {
    let regs = StandardRegisters {
        rflags: 0x0000000000000002u64,
        rbx: PVH_INFO_START.raw_value(),
        rip: boot_ip,
        ..Default::default()
    };
    vcpu.set_regs(&regs).map_err(Error::SetBaseRegisters)
}

/// Configures the segment registers and system page tables for a given CPU.
///
/// # Arguments
///
/// * `mem` - The memory that will be passed to the guest.
/// * `vcpu` - Structure for the VCPU that holds the VCPU's fd.
pub fn setup_sregs(mem: &GuestMemoryMmap, vcpu: &Arc<dyn hypervisor::Vcpu>) -> Result<()> {
    let mut sregs: SpecialRegisters = vcpu.get_sregs().map_err(Error::GetStatusRegisters)?;
    configure_segments_and_sregs(mem, &mut sregs)?;
    vcpu.set_sregs(&sregs).map_err(Error::SetStatusRegisters)
}

const BOOT_GDT_MAX: usize = 4;

fn write_gdt_table(table: &[u64], guest_mem: &GuestMemoryMmap) -> Result<()> {
    let boot_gdt_addr = BOOT_GDT_START;
    for (index, entry) in table.iter().enumerate() {
        let addr = guest_mem
            .checked_offset(boot_gdt_addr, index * mem::size_of::<u64>())
            .ok_or(Error::CheckGdtAddr)?;
        guest_mem.write_obj(*entry, addr).map_err(Error::WriteGdt)?;
    }
    Ok(())
}

fn write_idt_value(val: u64, guest_mem: &GuestMemoryMmap) -> Result<()> {
    let boot_idt_addr = BOOT_IDT_START;
    guest_mem
        .write_obj(val, boot_idt_addr)
        .map_err(Error::WriteIdt)
}

pub fn configure_segments_and_sregs(
    mem: &GuestMemoryMmap,
    sregs: &mut SpecialRegisters,
) -> Result<()> {
    let gdt_table: [u64; BOOT_GDT_MAX] = {
        // Configure GDT entries as specified by PVH boot protocol
        [
            gdt_entry(0, 0, 0),               // NULL
            gdt_entry(0xc09b, 0, 0xffffffff), // CODE
            gdt_entry(0xc093, 0, 0xffffffff), // DATA
            gdt_entry(0x008b, 0, 0x67),       // TSS
        ]
    };

    let code_seg = segment_from_gdt(gdt_table[1], 1);
    let data_seg = segment_from_gdt(gdt_table[2], 2);
    let tss_seg = segment_from_gdt(gdt_table[3], 3);

    // Write segments
    write_gdt_table(&gdt_table[..], mem)?;
    sregs.gdt.base = BOOT_GDT_START.raw_value();
    sregs.gdt.limit = mem::size_of_val(&gdt_table) as u16 - 1;

    write_idt_value(0, mem)?;
    sregs.idt.base = BOOT_IDT_START.raw_value();
    sregs.idt.limit = mem::size_of::<u64>() as u16 - 1;

    sregs.cs = code_seg;
    sregs.ds = data_seg;
    sregs.es = data_seg;
    sregs.fs = data_seg;
    sregs.gs = data_seg;
    sregs.ss = data_seg;
    sregs.tr = tss_seg;

    sregs.cr0 = CR0_PE;
    sregs.cr4 = 0;

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::GuestMemoryMmap;
    use vm_memory::GuestAddress;

    fn create_guest_mem() -> GuestMemoryMmap {
        GuestMemoryMmap::from_ranges(&[(GuestAddress(0), 0x10000)]).unwrap()
    }

    fn read_u64(gm: &GuestMemoryMmap, offset: GuestAddress) -> u64 {
        gm.read_obj(offset).unwrap()
    }

    #[test]
    fn segments_and_sregs() {
        let mut sregs: SpecialRegisters = Default::default();
        let gm = create_guest_mem();
        configure_segments_and_sregs(&gm, &mut sregs).unwrap();
        assert_eq!(0x0, read_u64(&gm, BOOT_GDT_START));
        assert_eq!(
            0xcf9b000000ffff,
            read_u64(&gm, BOOT_GDT_START.unchecked_add(8))
        );
        assert_eq!(
            0xcf93000000ffff,
            read_u64(&gm, BOOT_GDT_START.unchecked_add(16))
        );
        assert_eq!(
            0x8b0000000067,
            read_u64(&gm, BOOT_GDT_START.unchecked_add(24))
        );
        assert_eq!(0x0, read_u64(&gm, BOOT_IDT_START));

        assert_eq!(0, sregs.cs.base);
        assert_eq!(0xffffffff, sregs.ds.limit);
        assert_eq!(0x10, sregs.es.selector);
        assert_eq!(1, sregs.fs.present);
        assert_eq!(1, sregs.gs.g);
        assert_eq!(0, sregs.ss.avl);
        assert_eq!(0, sregs.tr.base);
        assert_eq!(0, sregs.tr.g);
        assert_eq!(0x67, sregs.tr.limit);
        assert_eq!(0xb, sregs.tr.type_);
        assert_eq!(0, sregs.tr.avl);
        assert_eq!(CR0_PE, sregs.cr0);
        assert_eq!(0, sregs.cr4);
    }
}

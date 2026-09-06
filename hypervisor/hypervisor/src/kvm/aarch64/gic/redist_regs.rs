// Copyright 2022 Arm Limited (or its affiliates). All rights reserved.
// Copyright 2020 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

use crate::arch::aarch64::gic::{Error, Result};
use crate::device::HypervisorDeviceError;
use crate::kvm::kvm_bindings::{
    kvm_device_attr, KVM_DEV_ARM_VGIC_GRP_REDIST_REGS, KVM_REG_ARM64, KVM_REG_ARM64_SYSREG,
    KVM_REG_ARM64_SYSREG_OP0_MASK, KVM_REG_ARM64_SYSREG_OP0_SHIFT, KVM_REG_ARM64_SYSREG_OP2_MASK,
    KVM_REG_ARM64_SYSREG_OP2_SHIFT, KVM_REG_SIZE_U64,
};
use crate::kvm::Register;
use crate::kvm::VcpuKvmState;
use crate::CpuState;
use kvm_ioctls::DeviceFd;

// Relevant redistributor registers that we want to save/restore.
const GICR_CTLR: u32 = 0x0000;
const GICR_STATUSR: u32 = 0x0010;
const GICR_WAKER: u32 = 0x0014;
const GICR_PROPBASER: u32 = 0x0070;
const GICR_PENDBASER: u32 = 0x0078;

/* SGI and PPI Redistributor registers, offsets from RD_base */
/*
 * Redistributor frame offsets from RD_base which is actually SZ_
 */
const GICR_SGI_OFFSET: u32 = 0x0001_0000;
const GICR_IGROUPR0: u32 = GICR_SGI_OFFSET + 0x0080;
const GICR_ICENABLER0: u32 = GICR_SGI_OFFSET + 0x0180;
const GICR_ISENABLER0: u32 = GICR_SGI_OFFSET + 0x0100;
const GICR_ISPENDR0: u32 = GICR_SGI_OFFSET + 0x0200;
const GICR_ICPENDR0: u32 = GICR_SGI_OFFSET + 0x0280;
const GICR_ISACTIVER0: u32 = GICR_SGI_OFFSET + 0x0300;
const GICR_ICACTIVER0: u32 = GICR_SGI_OFFSET + 0x0380;
const GICR_IPRIORITYR0: u32 = GICR_SGI_OFFSET + 0x0400;
const GICR_ICFGR0: u32 = GICR_SGI_OFFSET + 0x0C00;

const KVM_DEV_ARM_VGIC_V3_MPIDR_SHIFT: u32 = 32;
const KVM_DEV_ARM_VGIC_V3_MPIDR_MASK: u64 = 0xffffffff << KVM_DEV_ARM_VGIC_V3_MPIDR_SHIFT as u64;

const KVM_ARM64_SYSREG_MPIDR_EL1: u64 = KVM_REG_ARM64 as u64
    | KVM_REG_SIZE_U64 as u64
    | KVM_REG_ARM64_SYSREG as u64
    | (((3_u64) << KVM_REG_ARM64_SYSREG_OP0_SHIFT) & KVM_REG_ARM64_SYSREG_OP0_MASK as u64)
    | (((5_u64) << KVM_REG_ARM64_SYSREG_OP2_SHIFT) & KVM_REG_ARM64_SYSREG_OP2_MASK as u64);

/// This is how we represent the registers of a distributor.
/// It is relrvant their offset from the base address of the
/// distributor.
/// Each register has a different number
/// of bits_per_irq and is therefore variable length.
/// First 32 interrupts (0-32) are private to each CPU (SGIs and PPIs).
/// and so we save the first irq to identify between the type of the interrupt
/// that the respective register deals with.
struct RdistReg {
    /// Offset from distributor address.
    base: u32,
    /// Length of the register.
    length: u8,
}

// All or at least the registers we are interested in are 32 bit, so
// we use a constant for size(u32).
const REG_SIZE: u8 = 4;

// Creates a vgic redistributor register.
macro_rules! VGIC_RDIST_REG {
    ($base:expr, $len:expr) => {
        RdistReg {
            base: $base,
            length: $len,
        }
    };
}

// List with relevant distributor registers that we will be restoring.
static VGIC_RDIST_REGS: &[RdistReg] = &[
    VGIC_RDIST_REG!(GICR_STATUSR, 4),
    VGIC_RDIST_REG!(GICR_WAKER, 4),
    VGIC_RDIST_REG!(GICR_PROPBASER, 8),
    VGIC_RDIST_REG!(GICR_PENDBASER, 8),
    VGIC_RDIST_REG!(GICR_CTLR, 4),
];

// List with relevant distributor registers that we will be restoring.
static VGIC_SGI_REGS: &[RdistReg] = &[
    VGIC_RDIST_REG!(GICR_IGROUPR0, 4),
    VGIC_RDIST_REG!(GICR_ICENABLER0, 4),
    VGIC_RDIST_REG!(GICR_ISENABLER0, 4),
    VGIC_RDIST_REG!(GICR_ICFGR0, 8),
    VGIC_RDIST_REG!(GICR_ICPENDR0, 4),
    VGIC_RDIST_REG!(GICR_ISPENDR0, 4),
    VGIC_RDIST_REG!(GICR_ICACTIVER0, 4),
    VGIC_RDIST_REG!(GICR_ISACTIVER0, 4),
    VGIC_RDIST_REG!(GICR_IPRIORITYR0, 32),
];

/// Read a redistributor register.
///
/// The destination buffer must be a local `mut` whose address is handed to
/// KVM_GET_DEVICE_ATTR; going through a `&u32` would let LLVM treat the kernel
/// write-back as a readonly-noalias violation and fold the result to 0.
fn redist_attr_get(gic: &DeviceFd, offset: u32, typer: u64) -> Result<u32> {
    let mut val: u32 = 0;
    let mut gic_redist_attr = kvm_device_attr {
        group: KVM_DEV_ARM_VGIC_GRP_REDIST_REGS,
        attr: (typer & KVM_DEV_ARM_VGIC_V3_MPIDR_MASK) | (offset as u64), // this needs the mpidr
        addr: &mut val as *mut u32 as u64,
        flags: 0,
    };
    gic.get_device_attr(&mut gic_redist_attr).map_err(|e| {
        Error::GetDeviceAttribute(HypervisorDeviceError::GetDeviceAttribute(e.into()))
    })?;
    Ok(val)
}

/// Write a redistributor register.
fn redist_attr_set(gic: &DeviceFd, offset: u32, typer: u64, val: u32) -> Result<()> {
    let gic_redist_attr = kvm_device_attr {
        group: KVM_DEV_ARM_VGIC_GRP_REDIST_REGS,
        attr: (typer & KVM_DEV_ARM_VGIC_V3_MPIDR_MASK) | (offset as u64), // this needs the mpidr
        addr: &val as *const u32 as u64,
        flags: 0,
    };
    gic.set_device_attr(&gic_redist_attr).map_err(|e| {
        Error::SetDeviceAttribute(HypervisorDeviceError::SetDeviceAttribute(e.into()))
    })
}

fn access_redists_aux(
    gic: &DeviceFd,
    gicr_typer: &[u64],
    state: &mut Vec<u32>,
    reg_list: &[RdistReg],
    idx: &mut usize,
    set: bool,
) -> Result<()> {
    for i in gicr_typer {
        for rdreg in reg_list {
            let mut base = rdreg.base;
            let end = base + rdreg.length as u32;

            while base < end {
                if set {
                    redist_attr_set(gic, base, *i, state[*idx])?;
                    *idx += 1;
                } else {
                    state.push(redist_attr_get(gic, base, *i)?);
                }
                base += REG_SIZE as u32;
            }
        }
    }
    Ok(())
}

/// Get redistributor registers.
pub fn get_redist_regs(gic: &DeviceFd, gicr_typer: &[u64]) -> Result<Vec<u32>> {
    let mut state = Vec::new();
    let mut idx: usize = 0;
    access_redists_aux(
        gic,
        gicr_typer,
        &mut state,
        VGIC_RDIST_REGS,
        &mut idx,
        false,
    )?;

    access_redists_aux(gic, gicr_typer, &mut state, VGIC_SGI_REGS, &mut idx, false)?;
    Ok(state)
}

/// Set redistributor registers.
pub fn set_redist_regs(gic: &DeviceFd, gicr_typer: &[u64], state: &[u32]) -> Result<()> {
    let mut idx: usize = 0;
    let mut mut_state = state.to_owned();
    access_redists_aux(
        gic,
        gicr_typer,
        &mut mut_state,
        VGIC_RDIST_REGS,
        &mut idx,
        true,
    )?;
    access_redists_aux(
        gic,
        gicr_typer,
        &mut mut_state,
        VGIC_SGI_REGS,
        &mut idx,
        true,
    )
}

pub fn construct_gicr_typers(vcpu_states: &[CpuState]) -> Vec<u64> {
    /* Pre-construct the GICR_TYPER:
     * For our implementation:
     *  Top 32 bits are the affinity value of the associated CPU
     *  CommonLPIAff == 01 (redistributors with same Aff3 share LPI table)
     *  Processor_Number == CPU index starting from 0
     *  DPGS == 0 (GICR_CTLR.DPG* not supported)
     *  Last == 1 if this is the last redistributor in a series of
     *            contiguous redistributor pages
     *  DirectLPI == 0 (direct injection of LPIs not supported)
     *  VLPIS == 0 (virtual LPIs not supported)
     *  PLPIS == 0 (physical LPIs not supported)
     */
    let mut gicr_typers: Vec<u64> = Vec::new();
    for (index, state) in vcpu_states.iter().enumerate() {
        let state: VcpuKvmState = state.clone().into();
        let last = (index == vcpu_states.len() - 1) as u64;
        // state.sys_regs is a big collection of system registers, including MIPDR_EL1
        let mpidr: Vec<Register> = state
            .sys_regs
            .into_iter()
            .filter(|reg| reg.id == KVM_ARM64_SYSREG_MPIDR_EL1)
            .collect();
        //calculate affinity
        let mut cpu_affid = mpidr[0].addr & 1095233437695;
        cpu_affid = ((cpu_affid & 0xFF00000000) >> 8) | (cpu_affid & 0xFFFFFF);
        gicr_typers.push((cpu_affid << 32) | (1 << 24) | (index as u64) << 8 | (last << 4));
    }

    gicr_typers
}

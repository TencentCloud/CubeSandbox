// Copyright 2022 Arm Limited (or its affiliates). All rights reserved.
// Copyright 2020 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

use crate::arch::aarch64::gic::{Error, Result};
use crate::device::HypervisorDeviceError;
use crate::kvm::kvm_bindings::{
    kvm_device_attr, KVM_DEV_ARM_VGIC_GRP_CPU_SYSREGS, KVM_REG_ARM64_SYSREG_CRM_MASK,
    KVM_REG_ARM64_SYSREG_CRM_SHIFT, KVM_REG_ARM64_SYSREG_CRN_MASK, KVM_REG_ARM64_SYSREG_CRN_SHIFT,
    KVM_REG_ARM64_SYSREG_OP0_MASK, KVM_REG_ARM64_SYSREG_OP0_SHIFT, KVM_REG_ARM64_SYSREG_OP1_MASK,
    KVM_REG_ARM64_SYSREG_OP1_SHIFT, KVM_REG_ARM64_SYSREG_OP2_MASK, KVM_REG_ARM64_SYSREG_OP2_SHIFT,
};
use kvm_ioctls::DeviceFd;

const KVM_DEV_ARM_VGIC_V3_MPIDR_SHIFT: u32 = 32;
const KVM_DEV_ARM_VGIC_V3_MPIDR_MASK: u64 = 0xffffffff << KVM_DEV_ARM_VGIC_V3_MPIDR_SHIFT as u64;

const ICC_CTLR_EL1_PRIBITS_SHIFT: u32 = 8;
const ICC_CTLR_EL1_PRIBITS_MASK: u32 = 7 << ICC_CTLR_EL1_PRIBITS_SHIFT;

macro_rules! arm64_vgic_sys_reg {
    ($name: tt, $op0: tt, $op1: tt, $crn: tt, $crm: tt, $op2: expr) => {
        const $name: u64 = ((($op0 as u64) << KVM_REG_ARM64_SYSREG_OP0_SHIFT)
            & KVM_REG_ARM64_SYSREG_OP0_MASK as u64)
            | ((($op1 as u64) << KVM_REG_ARM64_SYSREG_OP1_SHIFT)
                & KVM_REG_ARM64_SYSREG_OP1_MASK as u64)
            | ((($crn as u64) << KVM_REG_ARM64_SYSREG_CRN_SHIFT)
                & KVM_REG_ARM64_SYSREG_CRN_MASK as u64)
            | ((($crm as u64) << KVM_REG_ARM64_SYSREG_CRM_SHIFT)
                & KVM_REG_ARM64_SYSREG_CRM_MASK as u64)
            | ((($op2 as u64) << KVM_REG_ARM64_SYSREG_OP2_SHIFT)
                & KVM_REG_ARM64_SYSREG_OP2_MASK as u64);
    };
}

macro_rules! SYS_ICC_AP0Rn_EL1 {
    ($name: tt, $n: tt) => {
        arm64_vgic_sys_reg!($name, 3, 0, 12, 8, (4 | $n));
    };
}

macro_rules! SYS_ICC_AP1Rn_EL1 {
    ($name: tt, $n: tt) => {
        arm64_vgic_sys_reg!($name, 3, 0, 12, 9, $n);
    };
}

arm64_vgic_sys_reg!(SYS_ICC_SRE_EL1, 3, 0, 12, 12, 5);
arm64_vgic_sys_reg!(SYS_ICC_CTLR_EL1, 3, 0, 12, 12, 4);
arm64_vgic_sys_reg!(SYS_ICC_IGRPEN0_EL1, 3, 0, 12, 12, 6);
arm64_vgic_sys_reg!(SYS_ICC_IGRPEN1_EL1, 3, 0, 12, 12, 7);
arm64_vgic_sys_reg!(SYS_ICC_PMR_EL1, 3, 0, 4, 6, 0);
arm64_vgic_sys_reg!(SYS_ICC_BPR0_EL1, 3, 0, 12, 8, 3);
arm64_vgic_sys_reg!(SYS_ICC_BPR1_EL1, 3, 0, 12, 12, 3);
SYS_ICC_AP0Rn_EL1!(SYS_ICC_AP0R0_EL1, 0);
SYS_ICC_AP0Rn_EL1!(SYS_ICC_AP0R1_EL1, 1);
SYS_ICC_AP0Rn_EL1!(SYS_ICC_AP0R2_EL1, 2);
SYS_ICC_AP0Rn_EL1!(SYS_ICC_AP0R3_EL1, 3);
SYS_ICC_AP1Rn_EL1!(SYS_ICC_AP1R0_EL1, 0);
SYS_ICC_AP1Rn_EL1!(SYS_ICC_AP1R1_EL1, 1);
SYS_ICC_AP1Rn_EL1!(SYS_ICC_AP1R2_EL1, 2);
SYS_ICC_AP1Rn_EL1!(SYS_ICC_AP1R3_EL1, 3);

static VGIC_ICC_REGS: &[u64] = &[
    SYS_ICC_SRE_EL1,
    SYS_ICC_CTLR_EL1,
    SYS_ICC_IGRPEN0_EL1,
    SYS_ICC_IGRPEN1_EL1,
    SYS_ICC_PMR_EL1,
    SYS_ICC_BPR0_EL1,
    SYS_ICC_BPR1_EL1,
    SYS_ICC_AP0R0_EL1,
    SYS_ICC_AP0R1_EL1,
    SYS_ICC_AP0R2_EL1,
    SYS_ICC_AP0R3_EL1,
    SYS_ICC_AP1R0_EL1,
    SYS_ICC_AP1R1_EL1,
    SYS_ICC_AP1R2_EL1,
    SYS_ICC_AP1R3_EL1,
];

/// Read an ICC register.
///
/// `KVM_DEV_ARM_VGIC_GRP_CPU_SYSREGS` attributes go through the kvm_one_reg
/// ABI, which accesses the user buffer as a `__u64` (see `kvm_sys_reg_get_user`
/// in the kernel), so the ioctl must be backed by a full `u64` even though the
/// register is 32-bit — a `u32` buffer would make KVM write 8 bytes into 4
/// bytes of stack. The value is converted (and validated) at the `Vec<u32>`
/// serialization boundary. The buffer must also be a local `mut` whose address
/// is handed to KVM_GET_DEVICE_ATTR: going through a shared reference would
/// let LLVM treat the kernel write-back as a readonly-noalias violation and
/// fold the result to 0 (observed with rustc 1.96, release + lto).
fn icc_attr_get(gic: &DeviceFd, offset: u64, typer: u64) -> Result<u32> {
    let mut val: u64 = 0;
    let mut gic_icc_attr = kvm_device_attr {
        group: KVM_DEV_ARM_VGIC_GRP_CPU_SYSREGS,
        attr: ((typer & KVM_DEV_ARM_VGIC_V3_MPIDR_MASK) | offset), // this needs the mpidr
        addr: &mut val as *mut u64 as u64,
        flags: 0,
    };
    gic.get_device_attr(&mut gic_icc_attr).map_err(|e| {
        Error::GetDeviceAttribute(HypervisorDeviceError::GetDeviceAttribute(e.into()))
    })?;
    u32::try_from(val).map_err(|_| {
        Error::GetDeviceAttribute(HypervisorDeviceError::GetDeviceAttribute(anyhow::anyhow!(
            "ICC register {:#x} read-back exceeds 32 bits: {:#x}",
            offset,
            val
        )))
    })
}

/// Write an ICC register.
///
/// Same `__u64` buffer requirement as [`icc_attr_get`].
fn icc_attr_set(gic: &DeviceFd, offset: u64, typer: u64, val: u32) -> Result<()> {
    let val: u64 = u64::from(val);
    let gic_icc_attr = kvm_device_attr {
        group: KVM_DEV_ARM_VGIC_GRP_CPU_SYSREGS,
        attr: ((typer & KVM_DEV_ARM_VGIC_V3_MPIDR_MASK) | offset), // this needs the mpidr
        addr: &val as *const u64 as u64,
        flags: 0,
    };
    gic.set_device_attr(&gic_icc_attr)
        .map_err(|e| Error::SetDeviceAttribute(HypervisorDeviceError::SetDeviceAttribute(e.into())))
}

/// Get ICC registers.
pub fn get_icc_regs(gic: &DeviceFd, gicr_typer: &[u64]) -> Result<Vec<u32>> {
    let mut state: Vec<u32> = Vec::new();
    // We need this for the ICC_AP<m>R<n>_EL1 registers.
    let mut num_priority_bits = 0;

    for ix in gicr_typer {
        let i = *ix;
        for icc_offset in VGIC_ICC_REGS {
            if *icc_offset == SYS_ICC_CTLR_EL1 {
                // calculate priority bits by reading the ctrl_el1 register.
                let val = icc_attr_get(gic, *icc_offset, i)?;
                // The priority bits are found in the ICC_CTLR_EL1 register (bits from  10:8).
                // See page 194 from https://static.docs.arm.com/ihi0069/c/IHI0069C_gic_
                // architecture_specification.pdf.
                // Citation:
                // "Priority bits. Read-only and writes are ignored. The number of priority bits
                // implemented, minus one."
                num_priority_bits =
                    ((val & ICC_CTLR_EL1_PRIBITS_MASK) >> ICC_CTLR_EL1_PRIBITS_SHIFT) + 1;
                state.push(val);
            }
            // As per ARMv8 documentation: https://static.docs.arm.com/ihi0069/c/IHI0069C_
            // gic_architecture_specification.pdf
            // page 178,
            // ICC_AP0R1_EL1 is only implemented in implementations that support 6 or more bits of
            // priority.
            // ICC_AP0R2_EL1 and ICC_AP0R3_EL1 are only implemented in implementations that support
            // 7 bits of priority.
            else if *icc_offset == SYS_ICC_AP0R1_EL1 || *icc_offset == SYS_ICC_AP1R1_EL1 {
                if num_priority_bits >= 6 {
                    state.push(icc_attr_get(gic, *icc_offset, i)?);
                }
            } else if *icc_offset == SYS_ICC_AP0R2_EL1
                || *icc_offset == SYS_ICC_AP0R3_EL1
                || *icc_offset == SYS_ICC_AP1R2_EL1
                || *icc_offset == SYS_ICC_AP1R3_EL1
            {
                if num_priority_bits == 7 {
                    state.push(icc_attr_get(gic, *icc_offset, i)?);
                }
            } else {
                state.push(icc_attr_get(gic, *icc_offset, i)?);
            }
        }
    }
    Ok(state)
}

/// Set ICC registers.
pub fn set_icc_regs(gic: &DeviceFd, gicr_typer: &[u64], state: &[u32]) -> Result<()> {
    let mut num_priority_bits = 0;
    let mut idx = 0;
    for ix in gicr_typer {
        let i = *ix;
        for icc_offset in VGIC_ICC_REGS {
            if *icc_offset == SYS_ICC_CTLR_EL1 {
                let ctrl_el1 = state[idx];
                num_priority_bits =
                    ((ctrl_el1 & ICC_CTLR_EL1_PRIBITS_MASK) >> ICC_CTLR_EL1_PRIBITS_SHIFT) + 1;
            }
            if *icc_offset == SYS_ICC_AP0R1_EL1 || *icc_offset == SYS_ICC_AP1R1_EL1 {
                if num_priority_bits >= 6 {
                    icc_attr_set(gic, *icc_offset, i, state[idx])?;
                    idx += 1;
                }
                continue;
            }
            if *icc_offset == SYS_ICC_AP0R2_EL1
                || *icc_offset == SYS_ICC_AP0R3_EL1
                || *icc_offset == SYS_ICC_AP1R2_EL1
                || *icc_offset == SYS_ICC_AP1R3_EL1
            {
                if num_priority_bits == 7 {
                    icc_attr_set(gic, *icc_offset, i, state[idx])?;
                    idx += 1;
                }
                continue;
            }
            icc_attr_set(gic, *icc_offset, i, state[idx])?;
            idx += 1;
        }
    }
    Ok(())
}

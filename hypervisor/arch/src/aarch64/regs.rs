// Copyright 2022 Arm Limited (or its affiliates). All rights reserved.
// SPDX-License-Identifier: Apache-2.0

///
/// AArch64 system register encoding:
/// See https://developer.arm.com/documentation/ddi0487 (chapter D12)
///
///   31      22  21 20 19 18 16 15 12 11  8 7   5 4  0
///  +----------+---+-----+-----+-----+-----+-----+----+
///  |1101010100| L | op0 | op1 | CRn | CRm | op2 | Rt |
///  +----------+---+-----+-----+-----+-----+-----+----+
///
/// Notes:
/// - L and Rt are reserved as implementation defined fields, ignored.
///
const SYSREG_HEAD: u32 = 0b1101010100u32 << 22;
const SYSREG_OP0_SHIFT: u32 = 19;
const SYSREG_OP0_MASK: u32 = 0b11u32 << 19;
const SYSREG_OP1_SHIFT: u32 = 16;
const SYSREG_OP1_MASK: u32 = 0b111u32 << 16;
const SYSREG_CRN_SHIFT: u32 = 12;
const SYSREG_CRN_MASK: u32 = 0b1111u32 << 12;
const SYSREG_CRM_SHIFT: u32 = 8;
const SYSREG_CRM_MASK: u32 = 0b1111u32 << 8;
const SYSREG_OP2_SHIFT: u32 = 5;
const SYSREG_OP2_MASK: u32 = 0b111u32 << 5;

/// Define the ID of system registers
#[macro_export]
macro_rules! arm64_sys_reg {
    ($name: tt, $op0: tt, $op1: tt, $crn: tt, $crm: tt, $op2: tt) => {
        pub const $name: u32 = SYSREG_HEAD
            | ((($op0 as u32) << SYSREG_OP0_SHIFT) & SYSREG_OP0_MASK as u32)
            | ((($op1 as u32) << SYSREG_OP1_SHIFT) & SYSREG_OP1_MASK as u32)
            | ((($crn as u32) << SYSREG_CRN_SHIFT) & SYSREG_CRN_MASK as u32)
            | ((($crm as u32) << SYSREG_CRM_SHIFT) & SYSREG_CRM_MASK as u32)
            | ((($op2 as u32) << SYSREG_OP2_SHIFT) & SYSREG_OP2_MASK as u32);
    };
}

arm64_sys_reg!(MPIDR_EL1, 3, 0, 0, 0, 5);
arm64_sys_reg!(ID_AA64MMFR0_EL1, 3, 0, 0, 7, 0);
arm64_sys_reg!(TTBR1_EL1, 3, 0, 2, 0, 1);
arm64_sys_reg!(TCR_EL1, 3, 0, 2, 0, 2);

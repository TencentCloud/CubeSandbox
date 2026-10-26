// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.

use std::result;
use std::sync::Arc;

pub type Result<T> = result::Result<T, hypervisor::HypervisorCpuError>;

// Defines poached from apicdef.h kernel header.
pub const APIC_LVT0: usize = 0x350;
pub const APIC_LVT1: usize = 0x360;
pub const APIC_MODE_NMI: u32 = 0x4;
pub const APIC_MODE_EXTINT: u32 = 0x7;

pub fn set_apic_delivery_mode(reg: u32, mode: u32) -> u32 {
    ((reg) & !0x700) | ((mode) << 8)
}

/// Configures LAPICs.  LAPIC0 is set for external interrupts, LAPIC1 is set for NMI.
///
/// # Arguments
/// * `vcpu` - The VCPU object to configure.
pub fn set_lint(vcpu: &Arc<dyn hypervisor::Vcpu>) -> Result<()> {
    let mut klapic = vcpu.get_lapic()?;

    let lvt_lint0 = klapic.get_klapic_reg(APIC_LVT0);
    klapic.set_klapic_reg(
        APIC_LVT0,
        set_apic_delivery_mode(lvt_lint0, APIC_MODE_EXTINT),
    );
    let lvt_lint1 = klapic.get_klapic_reg(APIC_LVT1);
    klapic.set_klapic_reg(APIC_LVT1, set_apic_delivery_mode(lvt_lint1, APIC_MODE_NMI));

    vcpu.set_lapic(&klapic)
}

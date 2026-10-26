// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 OR BSD-3-Clause
//
// Copyright © 2020, Microsoft Corporation
//
// Copyright 2018-2019 CrowdStrike, Inc.
//
//

use crate::arch::x86::{
    CpuIdEntry, DescriptorTable, FpuState, LapicState, MsrEntry, SegmentRegister, SpecialRegisters,
    StandardRegisters, XsaveState, CPUID_FLAG_VALID_INDEX,
};
use crate::kvm::{Cap, Kvm, KvmError, KvmResult};
use serde::{Deserialize, Serialize};

/// Replace an ambiguous zero TSC deadline with the captured TSC value.
///
/// KVM can clear `MSR_IA32_TSC_DEADLINE` after the timer expires before the
/// corresponding interrupt reaches a paused guest. Persisting that transient
/// zero would leave the restored vCPU without a deadline timer. A zero value is
/// meaningful in other LAPIC timer modes, so only normalize it when the saved
/// LVT timer is in TSC-deadline mode. If malformed input contains more than one
/// TSC entry, use the greatest value, matching Firecracker's conservative
/// handling.
pub fn normalize_zero_tsc_deadline(msrs: &mut [MsrEntry], lapic_state: &LapicState) -> Option<u64> {
    use crate::arch::x86::msr_index::{MSR_IA32_TSC, MSR_IA32_TSC_DEADLINE};

    const APIC_LVTT: usize = 0x320;
    const APIC_LVT_TIMER_MODE_MASK: u32 = 3 << 17;
    const APIC_LVT_TIMER_TSC_DEADLINE: u32 = 2 << 17;

    if lapic_state.get_klapic_reg(APIC_LVTT) & APIC_LVT_TIMER_MODE_MASK
        != APIC_LVT_TIMER_TSC_DEADLINE
    {
        return None;
    }

    let tsc = msrs
        .iter()
        .filter(|msr| msr.index == MSR_IA32_TSC)
        .map(|msr| msr.data)
        .max()?;

    let mut normalized = false;
    for msr in msrs {
        if msr.index == MSR_IA32_TSC_DEADLINE && msr.data == 0 {
            msr.data = tsc;
            normalized = true;
        }
    }

    normalized.then_some(tsc)
}

/// Return MSRs in restore order, with TSC deadlines after every other MSR.
///
/// KVM evaluates a deadline against the vCPU's current TSC when the deadline
/// is written. Keeping deadlines in a final group guarantees that the saved
/// TSC has already been restored, independent of the order KVM advertised its
/// MSR index list in or the order in an older snapshot. Restore intentionally
/// fails if the destination rejects any saved MSR: resuming with partial CPU
/// state is unsupported, including across hosts with mismatched MSR support.
pub fn order_msrs_for_restore(msrs: &[MsrEntry]) -> Vec<MsrEntry> {
    use crate::arch::x86::msr_index::MSR_IA32_TSC_DEADLINE;

    msrs.iter()
        .filter(|msr| msr.index != MSR_IA32_TSC_DEADLINE)
        .chain(msrs.iter().filter(|msr| msr.index == MSR_IA32_TSC_DEADLINE))
        .copied()
        .collect()
}

///
/// Export generically-named wrappers of kvm-bindings for Unix-based platforms
///
pub use {
    kvm_bindings::kvm_cpuid_entry2, kvm_bindings::kvm_dtable, kvm_bindings::kvm_fpu,
    kvm_bindings::kvm_lapic_state, kvm_bindings::kvm_mp_state as MpState,
    kvm_bindings::kvm_msr_entry, kvm_bindings::kvm_regs, kvm_bindings::kvm_segment,
    kvm_bindings::kvm_sregs, kvm_bindings::kvm_vcpu_events as VcpuEvents,
    kvm_bindings::kvm_xcrs as ExtendedControlRegisters, kvm_bindings::kvm_xsave as Xsave,
    kvm_bindings::CpuId, kvm_bindings::MsrList, kvm_bindings::Msrs as MsrEntries,
    kvm_bindings::KVM_CPUID_FLAG_SIGNIFCANT_INDEX,
};

///
/// Check KVM extension for Linux
///
pub fn check_required_kvm_extensions(kvm: &Kvm) -> KvmResult<()> {
    if !kvm.check_extension(Cap::SignalMsi) {
        return Err(KvmError::CapabilityMissing(Cap::SignalMsi));
    }
    if !kvm.check_extension(Cap::TscDeadlineTimer) {
        return Err(KvmError::CapabilityMissing(Cap::TscDeadlineTimer));
    }
    if !kvm.check_extension(Cap::SplitIrqchip) {
        return Err(KvmError::CapabilityMissing(Cap::SplitIrqchip));
    }
    if !kvm.check_extension(Cap::SetIdentityMapAddr) {
        return Err(KvmError::CapabilityMissing(Cap::SetIdentityMapAddr));
    }
    if !kvm.check_extension(Cap::SetTssAddr) {
        return Err(KvmError::CapabilityMissing(Cap::SetTssAddr));
    }
    if !kvm.check_extension(Cap::ImmediateExit) {
        return Err(KvmError::CapabilityMissing(Cap::ImmediateExit));
    }
    Ok(())
}
#[derive(Clone, Serialize, Deserialize)]
pub struct VcpuKvmState {
    pub cpuid: Vec<CpuIdEntry>,
    pub msrs: Vec<MsrEntry>,
    pub vcpu_events: VcpuEvents,
    pub regs: kvm_regs,
    pub sregs: kvm_sregs,
    pub fpu: FpuState,
    pub lapic_state: LapicState,
    pub xsave: XsaveState,
    pub xcrs: ExtendedControlRegisters,
    pub mp_state: MpState,
}

impl From<StandardRegisters> for kvm_regs {
    fn from(regs: StandardRegisters) -> Self {
        Self {
            rax: regs.rax,
            rbx: regs.rbx,
            rcx: regs.rcx,
            rdx: regs.rdx,
            rsi: regs.rsi,
            rdi: regs.rdi,
            rsp: regs.rsp,
            rbp: regs.rbp,
            r8: regs.r8,
            r9: regs.r9,
            r10: regs.r10,
            r11: regs.r11,
            r12: regs.r12,
            r13: regs.r13,
            r14: regs.r14,
            r15: regs.r15,
            rip: regs.rip,
            rflags: regs.rflags,
        }
    }
}

impl From<kvm_regs> for StandardRegisters {
    fn from(regs: kvm_regs) -> Self {
        Self {
            rax: regs.rax,
            rbx: regs.rbx,
            rcx: regs.rcx,
            rdx: regs.rdx,
            rsi: regs.rsi,
            rdi: regs.rdi,
            rsp: regs.rsp,
            rbp: regs.rbp,
            r8: regs.r8,
            r9: regs.r9,
            r10: regs.r10,
            r11: regs.r11,
            r12: regs.r12,
            r13: regs.r13,
            r14: regs.r14,
            r15: regs.r15,
            rip: regs.rip,
            rflags: regs.rflags,
        }
    }
}

impl From<SegmentRegister> for kvm_segment {
    fn from(s: SegmentRegister) -> Self {
        Self {
            base: s.base,
            limit: s.limit,
            selector: s.selector,
            type_: s.type_,
            present: s.present,
            dpl: s.dpl,
            db: s.db,
            s: s.s,
            l: s.l,
            g: s.g,
            avl: s.avl,
            unusable: s.unusable,
            ..Default::default()
        }
    }
}

impl From<kvm_segment> for SegmentRegister {
    fn from(s: kvm_segment) -> Self {
        Self {
            base: s.base,
            limit: s.limit,
            selector: s.selector,
            type_: s.type_,
            present: s.present,
            dpl: s.dpl,
            db: s.db,
            s: s.s,
            l: s.l,
            g: s.g,
            avl: s.avl,
            unusable: s.unusable,
        }
    }
}

impl From<DescriptorTable> for kvm_dtable {
    fn from(dt: DescriptorTable) -> Self {
        Self {
            base: dt.base,
            limit: dt.limit,
            ..Default::default()
        }
    }
}

impl From<kvm_dtable> for DescriptorTable {
    fn from(dt: kvm_dtable) -> Self {
        Self {
            base: dt.base,
            limit: dt.limit,
        }
    }
}

impl From<SpecialRegisters> for kvm_sregs {
    fn from(s: SpecialRegisters) -> Self {
        Self {
            cs: s.cs.into(),
            ds: s.ds.into(),
            es: s.es.into(),
            fs: s.fs.into(),
            gs: s.gs.into(),
            ss: s.ss.into(),
            tr: s.tr.into(),
            ldt: s.ldt.into(),
            gdt: s.gdt.into(),
            idt: s.idt.into(),
            cr0: s.cr0,
            cr2: s.cr2,
            cr3: s.cr3,
            cr4: s.cr4,
            cr8: s.cr8,
            efer: s.efer,
            apic_base: s.apic_base,
            interrupt_bitmap: s.interrupt_bitmap,
        }
    }
}

impl From<kvm_sregs> for SpecialRegisters {
    fn from(s: kvm_sregs) -> Self {
        Self {
            cs: s.cs.into(),
            ds: s.ds.into(),
            es: s.es.into(),
            fs: s.fs.into(),
            gs: s.gs.into(),
            ss: s.ss.into(),
            tr: s.tr.into(),
            ldt: s.ldt.into(),
            gdt: s.gdt.into(),
            idt: s.idt.into(),
            cr0: s.cr0,
            cr2: s.cr2,
            cr3: s.cr3,
            cr4: s.cr4,
            cr8: s.cr8,
            efer: s.efer,
            apic_base: s.apic_base,
            interrupt_bitmap: s.interrupt_bitmap,
        }
    }
}

impl From<CpuIdEntry> for kvm_cpuid_entry2 {
    fn from(e: CpuIdEntry) -> Self {
        let flags = if e.flags & CPUID_FLAG_VALID_INDEX != 0 {
            KVM_CPUID_FLAG_SIGNIFCANT_INDEX
        } else {
            0
        };
        Self {
            function: e.function,
            index: e.index,
            flags,
            eax: e.eax,
            ebx: e.ebx,
            ecx: e.ecx,
            edx: e.edx,
            ..Default::default()
        }
    }
}

impl From<kvm_cpuid_entry2> for CpuIdEntry {
    fn from(e: kvm_cpuid_entry2) -> Self {
        let flags = if e.flags & KVM_CPUID_FLAG_SIGNIFCANT_INDEX != 0 {
            CPUID_FLAG_VALID_INDEX
        } else {
            0
        };
        Self {
            function: e.function,
            index: e.index,
            flags,
            eax: e.eax,
            ebx: e.ebx,
            ecx: e.ecx,
            edx: e.edx,
        }
    }
}

impl From<kvm_fpu> for FpuState {
    fn from(s: kvm_fpu) -> Self {
        Self {
            fpr: s.fpr,
            fcw: s.fcw,
            fsw: s.fsw,
            ftwx: s.ftwx,
            last_opcode: s.last_opcode,
            last_ip: s.last_ip,
            last_dp: s.last_dp,
            xmm: s.xmm,
            mxcsr: s.mxcsr,
        }
    }
}

impl From<FpuState> for kvm_fpu {
    fn from(s: FpuState) -> Self {
        Self {
            fpr: s.fpr,
            fcw: s.fcw,
            fsw: s.fsw,
            ftwx: s.ftwx,
            last_opcode: s.last_opcode,
            last_ip: s.last_ip,
            last_dp: s.last_dp,
            xmm: s.xmm,
            mxcsr: s.mxcsr,
            ..Default::default()
        }
    }
}

impl From<LapicState> for kvm_lapic_state {
    fn from(s: LapicState) -> Self {
        Self { regs: s.regs }
    }
}

impl From<kvm_lapic_state> for LapicState {
    fn from(s: kvm_lapic_state) -> Self {
        Self { regs: s.regs }
    }
}

impl From<kvm_msr_entry> for MsrEntry {
    fn from(e: kvm_msr_entry) -> Self {
        Self {
            index: e.index,
            data: e.data,
        }
    }
}

impl From<MsrEntry> for kvm_msr_entry {
    fn from(e: MsrEntry) -> Self {
        Self {
            index: e.index,
            data: e.data,
            ..Default::default()
        }
    }
}

impl From<Xsave> for XsaveState {
    fn from(s: Xsave) -> Self {
        Self { region: s.region }
    }
}

impl From<XsaveState> for Xsave {
    fn from(s: XsaveState) -> Self {
        Self {
            region: s.region,
            extra: Default::default(),
        }
    }
}

// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 OR BSD-3-Clause
//
// Copyright © 2020, Microsoft Corporation
//
// Copyright 2018-2019 CrowdStrike, Inc.
//
//

#[cfg(target_arch = "aarch64")]
use crate::aarch64::{RegList, StandardRegisters, VcpuInit};
#[cfg(target_arch = "x86_64")]
use crate::arch::x86::{
    CpuIdEntry, FpuState, LapicState, MsrEntry, SpecialRegisters, StandardRegisters,
};
#[cfg(feature = "tdx")]
use crate::kvm::{TdxExitDetails, TdxExitStatus};
use crate::CpuState;
use crate::MpState;
use thiserror::Error;
use vm_memory::GuestAddress;

#[derive(Error, Debug)]
///
/// Enum for CPU error
pub enum HypervisorCpuError {
    ///
    /// Setting standard registers error
    ///
    #[error("Failed to set standard register: {0}")]
    SetStandardRegs(#[source] anyhow::Error),
    ///
    /// Setting standard registers error
    ///
    #[error("Failed to get standard registers: {0}")]
    GetStandardRegs(#[source] anyhow::Error),
    ///
    /// Setting special register error
    ///
    #[error("Failed to set special registers: {0}")]
    SetSpecialRegs(#[source] anyhow::Error),
    ///
    /// Getting standard register error
    ///
    #[error("Failed to get special registers: {0}")]
    GetSpecialRegs(#[source] anyhow::Error),
    ///
    /// Setting floating point registers error
    ///
    #[error("Failed to set floating point registers: {0}")]
    SetFloatingPointRegs(#[source] anyhow::Error),
    ///
    /// Getting floating point register error
    ///
    #[error("Failed to get floating points registers: {0}")]
    GetFloatingPointRegs(#[source] anyhow::Error),
    ///
    /// Setting Cpuid error
    ///
    #[error("Failed to set Cpuid: {0}")]
    SetCpuid(#[source] anyhow::Error),
    ///
    /// Getting Cpuid error
    ///
    #[error("Failed to get Cpuid: {0}")]
    GetCpuid(#[source] anyhow::Error),
    ///
    /// Setting lapic state error
    ///
    #[error("Failed to set Lapic state: {0}")]
    SetLapicState(#[source] anyhow::Error),
    ///
    /// Getting Lapic state error
    ///
    #[error("Failed to get Lapic state: {0}")]
    GetlapicState(#[source] anyhow::Error),
    ///
    /// Setting MSR entries error
    ///
    #[error("Failed to set Msr entries: {0}")]
    SetMsrEntries(#[source] anyhow::Error),
    ///
    /// Getting Msr entries error
    ///
    #[error("Failed to get Msr entries: {0}")]
    GetMsrEntries(#[source] anyhow::Error),
    ///
    /// Setting MSR entries error
    ///
    #[error("Failed to set MP state: {0}")]
    SetMpState(#[source] anyhow::Error),
    ///
    /// Getting Msr entries error
    ///
    #[error("Failed to get MP state: {0}")]
    GetMpState(#[source] anyhow::Error),
    ///
    /// Setting Saved Processor Extended States error
    ///
    #[error("Failed to set Saved Processor Extended States: {0}")]
    SetXsaveState(#[source] anyhow::Error),
    ///
    /// Getting Saved Processor Extended States error
    ///
    #[error("Failed to get Saved Processor Extended States: {0}")]
    GetXsaveState(#[source] anyhow::Error),
    ///
    /// Setting Extended Control Registers error
    ///
    #[error("Failed to set Extended Control Registers: {0}")]
    SetXcsr(#[source] anyhow::Error),
    ///
    /// Getting Extended Control Registers error
    ///
    #[error("Failed to get Extended Control Registers: {0}")]
    GetXcsr(#[source] anyhow::Error),
    ///
    /// Running Vcpu error
    ///
    #[error("Failed to run vcpu: {0}")]
    RunVcpu(#[source] anyhow::Error),
    ///
    /// Getting Vcpu events error
    ///
    #[error("Failed to get Vcpu events: {0}")]
    GetVcpuEvents(#[source] anyhow::Error),
    ///
    /// Setting Vcpu events error
    ///
    #[error("Failed to set Vcpu events: {0}")]
    SetVcpuEvents(#[source] anyhow::Error),
    ///
    /// Vcpu Init error
    ///
    #[error("Failed to init vcpu: {0}")]
    VcpuInit(#[source] anyhow::Error),
    ///
    /// Setting one reg error
    ///
    #[error("Failed to init vcpu: {0}")]
    SetRegister(#[source] anyhow::Error),
    ///
    /// Getting one reg error
    ///
    #[error("Failed to init vcpu: {0}")]
    GetRegister(#[source] anyhow::Error),
    ///
    /// Getting guest clock paused error
    ///
    #[error("Failed to notify guest its clock was paused: {0}")]
    NotifyGuestClockPaused(#[source] anyhow::Error),
    ///
    /// Setting debug register error
    ///
    #[error("Failed to set debug registers: {0}")]
    SetDebugRegs(#[source] anyhow::Error),
    ///
    /// Getting debug register error
    ///
    #[error("Failed to get debug registers: {0}")]
    GetDebugRegs(#[source] anyhow::Error),
    ///
    /// Setting misc register error
    ///
    #[error("Failed to set misc registers: {0}")]
    SetMiscRegs(#[source] anyhow::Error),
    ///
    /// Getting misc register error
    ///
    #[error("Failed to get misc registers: {0}")]
    GetMiscRegs(#[source] anyhow::Error),
    ///
    /// Write to Guest Mem
    ///
    #[error("Failed to write to Guest Mem at: {0}")]
    GuestMemWrite(#[source] anyhow::Error),
    /// Enabling HyperV SynIC error
    ///
    #[error("Failed to enable HyperV SynIC")]
    EnableHyperVSyncIc(#[source] anyhow::Error),
    ///
    /// Getting AArch64 core register error
    ///
    #[error("Failed to get core register: {0}")]
    GetCoreRegister(#[source] anyhow::Error),
    ///
    /// Setting AArch64 core register error
    ///
    #[error("Failed to set core register: {0}")]
    SetCoreRegister(#[source] anyhow::Error),
    ///
    /// Getting AArch64 registers list error
    ///
    #[error("Failed to retrieve list of registers: {0}")]
    GetRegList(#[source] anyhow::Error),
    ///
    /// Getting AArch64 system register error
    ///
    #[error("Failed to get system register: {0}")]
    GetSysRegister(#[source] anyhow::Error),
    ///
    /// Setting AArch64 system register error
    ///
    #[error("Failed to set system register: {0}")]
    SetSysRegister(#[source] anyhow::Error),
    ///
    /// GVA translation error
    ///
    #[error("Failed to translate GVA: {0}")]
    TranslateVirtualAddress(#[source] anyhow::Error),
    ///
    /// Set cpu attribute error
    ///
    #[error("Failed to set vcpu attribute: {0}")]
    SetVcpuAttribute(#[source] anyhow::Error),
    ///
    /// Check if cpu has a certain attribute error
    ///
    #[error("Failed to check if vcpu has attribute: {0}")]
    HasVcpuAttribute(#[source] anyhow::Error),
    ///
    /// Failed to initialize TDX on CPU
    ///
    #[cfg(feature = "tdx")]
    #[error("Failed to initialize TDX: {0}")]
    InitializeTdx(#[source] std::io::Error),
    ///
    /// Unknown TDX VM call
    ///
    #[cfg(feature = "tdx")]
    #[error("Unknown TDX VM call")]
    UnknownTdxVmCall,
    #[cfg(target_arch = "aarch64")]
    ///
    /// Failed to initialize PMU
    ///
    #[error("Failed to initialize PMU")]
    InitializePmu,
}

#[derive(Debug)]
pub enum VmExit {
    #[cfg(target_arch = "x86_64")]
    IoapicEoi(u8 /* vector */),
    Ignore,
    Reset,
    Shutdown,
    Hyperv,
    #[cfg(feature = "tdx")]
    Tdx,
    #[cfg(feature = "kvm")]
    Debug,
}

///
/// Result type for returning from a function
///
pub type Result<T> = anyhow::Result<T, HypervisorCpuError>;
///
/// Trait to represent a generic Vcpu
///
pub trait Vcpu: Send + Sync {
    ///
    /// Returns the vCPU general purpose registers.
    ///
    fn get_regs(&self) -> Result<StandardRegisters>;
    ///
    /// Sets the vCPU general purpose registers.
    ///
    fn set_regs(&self, regs: &StandardRegisters) -> Result<()>;
    #[cfg(target_arch = "x86_64")]
    ///
    /// Returns the vCPU special registers.
    ///
    fn get_sregs(&self) -> Result<SpecialRegisters>;
    #[cfg(target_arch = "x86_64")]
    ///
    /// Sets the vCPU special registers
    ///
    fn set_sregs(&self, sregs: &SpecialRegisters) -> Result<()>;
    #[cfg(target_arch = "x86_64")]
    ///
    /// Returns the floating point state (FPU) from the vCPU.
    ///
    fn get_fpu(&self) -> Result<FpuState>;
    #[cfg(target_arch = "x86_64")]
    ///
    /// Set the floating point state (FPU) of a vCPU
    ///
    fn set_fpu(&self, fpu: &FpuState) -> Result<()>;
    #[cfg(target_arch = "x86_64")]
    ///
    /// X86 specific call to setup the CPUID registers.
    ///
    fn set_cpuid2(&self, cpuid: &[CpuIdEntry]) -> Result<()>;
    #[cfg(target_arch = "x86_64")]
    ///
    /// X86 specific call to enable HyperV SynIC
    ///
    fn enable_hyperv_synic(&self) -> Result<()>;
    #[cfg(target_arch = "x86_64")]
    ///
    /// X86 specific call to retrieve the CPUID registers.
    ///
    fn get_cpuid2(&self, num_entries: usize) -> Result<Vec<CpuIdEntry>>;
    #[cfg(target_arch = "x86_64")]
    ///
    /// Returns the state of the LAPIC (Local Advanced Programmable Interrupt Controller).
    ///
    fn get_lapic(&self) -> Result<LapicState>;
    #[cfg(target_arch = "x86_64")]
    ///
    /// Sets the state of the LAPIC (Local Advanced Programmable Interrupt Controller).
    ///
    fn set_lapic(&self, lapic: &LapicState) -> Result<()>;
    #[cfg(target_arch = "x86_64")]
    ///
    /// Returns the model-specific registers (MSR) for this vCPU.
    ///
    fn get_msrs(&self, msrs: &mut Vec<MsrEntry>) -> Result<usize>;
    #[cfg(target_arch = "x86_64")]
    ///
    /// Setup the model-specific registers (MSR) for this vCPU.
    ///
    fn set_msrs(&self, msrs: &[MsrEntry]) -> Result<usize>;
    ///
    /// Returns the vcpu's current "multiprocessing state".
    ///
    fn get_mp_state(&self) -> Result<MpState>;
    ///
    /// Sets the vcpu's current "multiprocessing state".
    ///
    fn set_mp_state(&self, mp_state: MpState) -> Result<()>;
    #[cfg(target_arch = "x86_64")]
    ///
    /// Let the guest know that it has been paused, which prevents from
    /// potential soft lockups when being resumed.
    ///
    fn notify_guest_clock_paused(&self) -> Result<()> {
        Ok(())
    }
    ///
    /// Sets debug registers to set hardware breakpoints and/or enable single step.
    ///
    fn set_guest_debug(&self, _addrs: &[GuestAddress], _singlestep: bool) -> Result<()> {
        Err(HypervisorCpuError::SetDebugRegs(anyhow!("unimplemented")))
    }
    ///
    /// Sets the type of CPU to be exposed to the guest and optional features.
    ///
    #[cfg(target_arch = "aarch64")]
    fn vcpu_init(&self, kvi: &VcpuInit) -> Result<()>;
    ///
    /// Gets a list of the guest registers that are supported for the
    /// KVM_GET_ONE_REG/KVM_SET_ONE_REG calls.
    ///
    #[cfg(target_arch = "aarch64")]
    fn get_reg_list(&self, reg_list: &mut RegList) -> Result<()>;
    ///
    /// Gets the value of a system register
    ///
    #[cfg(target_arch = "aarch64")]
    fn get_sys_reg(&self, sys_reg: u32) -> Result<u64>;
    ///
    /// Configure core registers for a given CPU.
    ///
    #[cfg(target_arch = "aarch64")]
    fn setup_regs(&self, cpu_id: u8, boot_ip: u64, fdt_start: u64) -> Result<()>;
    ///
    /// Check if the CPU supports PMU
    ///
    #[cfg(target_arch = "aarch64")]
    fn has_pmu_support(&self) -> bool;
    ///
    /// Initialize PMU
    ///
    #[cfg(target_arch = "aarch64")]
    fn init_pmu(&self, irq: u32) -> Result<()>;
    ///
    /// Retrieve the vCPU state.
    /// This function is necessary to snapshot the VM
    ///
    fn state(&self) -> Result<CpuState>;
    ///
    /// Set the vCPU state.
    /// This function is required when restoring the VM
    ///
    fn set_state(&self, state: &CpuState) -> Result<()>;
    ///
    /// Triggers the running of the current virtual CPU returning an exit reason.
    ///
    fn run(&self) -> std::result::Result<VmExit, HypervisorCpuError>;
    #[cfg(target_arch = "x86_64")]
    ///
    /// Translate guest virtual address to guest physical address
    ///
    fn translate_gva(&self, gva: u64, flags: u64) -> Result<(u64, u32)>;
    ///
    /// Initialize TDX support on the vCPU
    ///
    #[cfg(feature = "tdx")]
    fn tdx_init(&self, _hob_address: u64) -> Result<()> {
        unimplemented!()
    }
    ///
    /// Set the "immediate_exit" state
    ///
    fn set_immediate_exit(&self, _exit: bool) {}
    #[cfg(feature = "tdx")]
    ///
    /// Returns the details about TDX exit reason
    ///
    fn get_tdx_exit_details(&mut self) -> Result<TdxExitDetails> {
        unimplemented!()
    }
    #[cfg(feature = "tdx")]
    ///
    /// Set the status code for TDX exit
    ///
    fn set_tdx_status(&mut self, _status: TdxExitStatus) {
        unimplemented!()
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// Return the list of initial MSR entries for a VCPU
    ///
    fn boot_msr_entries(&self) -> Vec<MsrEntry>;
}

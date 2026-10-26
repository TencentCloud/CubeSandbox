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
use crate::aarch64::VcpuInit;
#[cfg(target_arch = "aarch64")]
use crate::arch::aarch64::gic::{Vgic, VgicConfig};
#[cfg(feature = "tdx")]
use crate::arch::x86::CpuIdEntry;
use crate::cpu::Vcpu;
#[cfg(target_arch = "x86_64")]
use crate::ClockData;
use crate::UserMemoryRegion;
use crate::{IoEventAddress, IrqRoutingEntry};
use std::any::Any;
#[cfg(target_arch = "x86_64")]
use std::fs::File;
use std::sync::Arc;
#[cfg(target_arch = "aarch64")]
use std::sync::Mutex;
use thiserror::Error;
use vmm_sys_util::eventfd::EventFd;

///
/// I/O events data matches (32 or 64 bits).
///
#[derive(Debug)]
pub enum DataMatch {
    DataMatch32(u32),
    DataMatch64(u64),
}

impl From<DataMatch> for u64 {
    fn from(dm: DataMatch) -> u64 {
        match dm {
            DataMatch::DataMatch32(dm) => dm.into(),
            DataMatch::DataMatch64(dm) => dm,
        }
    }
}

#[derive(Error, Debug)]
///
/// Enum for VM error
pub enum HypervisorVmError {
    ///
    /// Create Vcpu error
    ///
    #[error("Failed to create Vcpu: {0}")]
    CreateVcpu(#[source] anyhow::Error),
    ///
    /// Identity map address error
    ///
    #[error("Failed to set identity map address: {0}")]
    SetIdentityMapAddress(#[source] anyhow::Error),
    ///
    /// TSS address error
    ///
    #[error("Failed to set TSS address: {0}")]
    SetTssAddress(#[source] anyhow::Error),
    ///
    /// Create interrupt controller error
    ///
    #[error("Failed to create interrupt controller: {0}")]
    CreateIrq(#[source] anyhow::Error),
    ///
    /// Register interrupt event error
    ///
    #[error("Failed to register interrupt event: {0}")]
    RegisterIrqFd(#[source] anyhow::Error),
    ///
    /// Un register interrupt event error
    ///
    #[error("Failed to unregister interrupt event: {0}")]
    UnregisterIrqFd(#[source] anyhow::Error),
    ///
    /// Register IO event error
    ///
    #[error("Failed to register IO event: {0}")]
    RegisterIoEvent(#[source] anyhow::Error),
    ///
    /// Unregister IO event error
    ///
    #[error("Failed to unregister IO event: {0}")]
    UnregisterIoEvent(#[source] anyhow::Error),
    ///
    /// Set GSI routing error
    ///
    #[error("Failed to set GSI routing: {0}")]
    SetGsiRouting(#[source] anyhow::Error),
    ///
    /// Create user memory error
    ///
    #[error("Failed to create user memory: {0}")]
    CreateUserMemory(#[source] anyhow::Error),
    ///
    /// Remove user memory region error
    ///
    #[error("Failed to remove user memory: {0}")]
    RemoveUserMemory(#[source] anyhow::Error),
    ///
    /// Create device error
    ///
    #[error("Failed to set GSI routing: {0}")]
    CreateDevice(#[source] anyhow::Error),
    ///
    /// Get preferred target error
    ///
    #[error("Failed to get preferred target: {0}")]
    GetPreferredTarget(#[source] anyhow::Error),
    ///
    /// Enable split Irq error
    ///
    #[error("Failed to enable split Irq: {0}")]
    EnableSplitIrq(#[source] anyhow::Error),
    ///
    /// Enable SGX attribute error
    ///
    #[error("Failed to enable SGX attribute: {0}")]
    EnableSgxAttribute(#[source] anyhow::Error),
    ///
    /// Get clock error
    ///
    #[error("Failed to get clock: {0}")]
    GetClock(#[source] anyhow::Error),
    ///
    /// Set clock error
    ///
    #[error("Failed to set clock: {0}")]
    SetClock(#[source] anyhow::Error),
    ///
    /// Create passthrough device
    ///
    #[error("Failed to create passthrough device: {0}")]
    CreatePassthroughDevice(#[source] anyhow::Error),
    /// Write to Guest memory
    ///
    #[error("Failed to write to guest memory: {0}")]
    GuestMemWrite(#[source] anyhow::Error),
    ///
    /// Read Guest memory
    ///
    #[error("Failed to read guest memory: {0}")]
    GuestMemRead(#[source] anyhow::Error),
    ///
    /// Read from MMIO Bus
    ///
    #[error("Failed to read from MMIO Bus: {0}")]
    MmioBusRead(#[source] anyhow::Error),
    ///
    /// Write to MMIO Bus
    ///
    #[error("Failed to write to MMIO Bus: {0}")]
    MmioBusWrite(#[source] anyhow::Error),
    ///
    /// Read from IO Bus
    ///
    #[error("Failed to read from IO Bus: {0}")]
    IoBusRead(#[source] anyhow::Error),
    ///
    /// Write to IO Bus
    ///
    #[error("Failed to write to IO Bus: {0}")]
    IoBusWrite(#[source] anyhow::Error),
    ///
    /// Start dirty log error
    ///
    #[error("Failed to get dirty log: {0}")]
    StartDirtyLog(#[source] anyhow::Error),
    ///
    /// Stop dirty log error
    ///
    #[error("Failed to get dirty log: {0}")]
    StopDirtyLog(#[source] anyhow::Error),
    ///
    /// Get dirty log error
    ///
    #[error("Failed to get dirty log: {0}")]
    GetDirtyLog(#[source] anyhow::Error),
    ///
    /// Assert virtual interrupt error
    ///
    #[error("Failed to assert virtual Interrupt: {0}")]
    AsserttVirtualInterrupt(#[source] anyhow::Error),

    #[cfg(feature = "tdx")]
    ///
    /// Error initializing TDX on the VM
    ///
    #[error("Failed to initialize TDX: {0}")]
    InitializeTdx(#[source] std::io::Error),
    #[cfg(feature = "tdx")]
    ///
    /// Error finalizing the TDX configuration on the VM
    ///
    #[error("Failed to finalize TDX: {0}")]
    FinalizeTdx(#[source] std::io::Error),
    #[cfg(feature = "tdx")]
    ///
    /// Error initializing the TDX memory region
    ///
    #[error("Failed to initialize memory region TDX: {0}")]
    InitMemRegionTdx(#[source] std::io::Error),
    ///
    /// Create Vgic error
    ///
    #[error("Failed to create Vgic: {0}")]
    CreateVgic(#[source] anyhow::Error),
}
///
/// Result type for returning from a function
///
pub type Result<T> = std::result::Result<T, HypervisorVmError>;

/// Configuration data for legacy interrupts.
///
/// On x86 platforms, legacy interrupts means those interrupts routed through PICs or IOAPICs.
#[derive(Copy, Clone, Debug)]
pub struct LegacyIrqSourceConfig {
    pub irqchip: u32,
    pub pin: u32,
}

/// Configuration data for MSI/MSI-X interrupts.
///
/// On x86 platforms, these interrupts are vectors delivered directly to the LAPIC.
#[derive(Copy, Clone, Debug, Default)]
pub struct MsiIrqSourceConfig {
    /// High address to delivery message signaled interrupt.
    pub high_addr: u32,
    /// Low address to delivery message signaled interrupt.
    pub low_addr: u32,
    /// Data to write to delivery message signaled interrupt.
    pub data: u32,
    /// Unique ID of the device to delivery message signaled interrupt.
    pub devid: u32,
}

/// Configuration data for an interrupt source.
#[derive(Copy, Clone, Debug)]
pub enum InterruptSourceConfig {
    /// Configuration data for Legacy interrupts.
    LegacyIrq(LegacyIrqSourceConfig),
    /// Configuration data for PciMsi, PciMsix and generic MSI interrupts.
    MsiIrq(MsiIrqSourceConfig),
}

///
/// Trait to represent a Vm
///
/// This crate provides a hypervisor-agnostic interfaces for Vm
///
pub trait Vm: Send + Sync + Any {
    #[cfg(target_arch = "x86_64")]
    /// Sets the address of the one-page region in the VM's address space.
    fn set_identity_map_address(&self, address: u64) -> Result<()>;
    #[cfg(target_arch = "x86_64")]
    /// Sets the address of the three-page region in the VM's address space.
    fn set_tss_address(&self, offset: usize) -> Result<()>;
    /// Creates an in-kernel interrupt controller.
    fn create_irq_chip(&self) -> Result<()>;
    /// Registers an event that will, when signaled, trigger the `gsi` IRQ.
    fn register_irqfd(&self, fd: &EventFd, gsi: u32) -> Result<()>;
    /// Unregister an event that will, when signaled, trigger the `gsi` IRQ.
    fn unregister_irqfd(&self, fd: &EventFd, gsi: u32) -> Result<()>;
    /// Creates a new KVM vCPU file descriptor and maps the memory corresponding
    fn create_vcpu(&self, id: u8, vm_ops: Option<Arc<dyn VmOps>>) -> Result<Arc<dyn Vcpu>>;
    #[cfg(target_arch = "aarch64")]
    fn create_vgic(&self, config: VgicConfig) -> Result<Arc<Mutex<dyn Vgic>>>;

    /// Registers an event to be signaled whenever a certain address is written to.
    fn register_ioevent(
        &self,
        fd: &EventFd,
        addr: &IoEventAddress,
        datamatch: Option<DataMatch>,
    ) -> Result<()>;
    /// Unregister an event from a certain address it has been previously registered to.
    fn unregister_ioevent(&self, fd: &EventFd, addr: &IoEventAddress) -> Result<()>;
    // Construct a routing entry
    fn make_routing_entry(&self, gsi: u32, config: &InterruptSourceConfig) -> IrqRoutingEntry;
    /// Sets the GSI routing table entries, overwriting any previously set
    fn set_gsi_routing(&self, entries: &[IrqRoutingEntry]) -> Result<()>;
    /// Creates a memory region structure that can be used with {create/remove}_user_memory_region
    fn make_user_memory_region(
        &self,
        slot: u32,
        guest_phys_addr: u64,
        memory_size: u64,
        userspace_addr: u64,
        readonly: bool,
        log_dirty_pages: bool,
    ) -> UserMemoryRegion;
    /// Creates a guest physical memory slot.
    fn create_user_memory_region(&self, user_memory_region: UserMemoryRegion) -> Result<()>;
    /// Removes a guest physical memory slot.
    fn remove_user_memory_region(&self, user_memory_region: UserMemoryRegion) -> Result<()>;
    /// Returns the preferred CPU target type which can be emulated by KVM on underlying host.
    #[cfg(target_arch = "aarch64")]
    fn get_preferred_target(&self, kvi: &mut VcpuInit) -> Result<()>;
    /// Enable split Irq capability
    #[cfg(target_arch = "x86_64")]
    fn enable_split_irq(&self) -> Result<()>;
    #[cfg(target_arch = "x86_64")]
    fn enable_sgx_attribute(&self, file: File) -> Result<()>;
    /// Retrieve guest clock.
    #[cfg(target_arch = "x86_64")]
    fn get_clock(&self) -> Result<ClockData>;
    /// Set guest clock.
    #[cfg(target_arch = "x86_64")]
    fn set_clock(&self, data: &ClockData) -> Result<()>;
    /// Create a device that is used for passthrough
    fn create_passthrough_device(&self) -> Result<vfio_ioctls::VfioDeviceFd>;
    /// Start logging dirty pages
    fn start_dirty_log(&self) -> Result<()>;
    /// Stop logging dirty pages
    fn stop_dirty_log(&self) -> Result<()>;
    /// Get dirty pages bitmap
    fn get_dirty_log(&self, slot: u32, base_gpa: u64, memory_size: u64) -> Result<Vec<u64>>;
    #[cfg(feature = "tdx")]
    /// Initalize TDX on this VM
    fn tdx_init(&self, _cpuid: &[CpuIdEntry], _max_vcpus: u32) -> Result<()> {
        unimplemented!()
    }
    #[cfg(feature = "tdx")]
    /// Finalize the configuration of TDX on this VM
    fn tdx_finalize(&self) -> Result<()> {
        unimplemented!()
    }
    #[cfg(feature = "tdx")]
    /// Initalize a TDX memory region for this VM
    fn tdx_init_memory_region(
        &self,
        _host_address: u64,
        _guest_address: u64,
        _size: u64,
        _measure: bool,
    ) -> Result<()> {
        unimplemented!()
    }
    /// Downcast to the underlying hypervisor VM type
    fn as_any(&self) -> &dyn Any;
}

pub trait VmOps: Send + Sync {
    fn guest_mem_write(&self, gpa: u64, buf: &[u8]) -> Result<usize>;
    fn guest_mem_read(&self, gpa: u64, buf: &mut [u8]) -> Result<usize>;
    fn mmio_read(&self, gpa: u64, data: &mut [u8]) -> Result<()>;
    fn mmio_write(&self, gpa: u64, data: &[u8]) -> Result<()>;
    #[cfg(target_arch = "x86_64")]
    fn pio_read(&self, port: u64, data: &mut [u8]) -> Result<()>;
    #[cfg(target_arch = "x86_64")]
    fn pio_write(&self, port: u64, data: &[u8]) -> Result<()>;
}

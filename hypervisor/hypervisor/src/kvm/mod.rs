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
use crate::aarch64::gic::KvmGicV3Its;
#[cfg(target_arch = "aarch64")]
pub use crate::aarch64::{
    check_required_kvm_extensions, gic::Gicv3ItsState as GicState, is_system_register, VcpuInit,
    VcpuKvmState,
};
#[cfg(target_arch = "aarch64")]
use crate::arch::aarch64::gic::{Vgic, VgicConfig};
use crate::cpu;
use crate::hypervisor;
use crate::vec_with_array_field;
use crate::vm::{self, InterruptSourceConfig, VmOps};
use crate::HypervisorType;
#[cfg(target_arch = "aarch64")]
use crate::{arm64_core_reg_id, offset__of};
use kvm_ioctls::{NoDatamatch, VcpuFd, VmFd};
use std::any::Any;
use std::collections::HashMap;
#[cfg(target_arch = "aarch64")]
use std::convert::TryInto;
#[cfg(target_arch = "x86_64")]
use std::fs::File;
#[cfg(target_arch = "x86_64")]
use std::os::unix::io::AsRawFd;
#[cfg(feature = "tdx")]
use std::os::unix::io::RawFd;
use std::result;
#[cfg(target_arch = "x86_64")]
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Mutex;
use std::sync::{Arc, RwLock};
use vmm_sys_util::eventfd::EventFd;
// x86_64 dependencies
#[cfg(target_arch = "x86_64")]
pub mod x86_64;
#[cfg(target_arch = "x86_64")]
use crate::arch::x86::{
    CpuIdEntry, FpuState, LapicState, MsrEntry, SpecialRegisters, StandardRegisters, XsaveState,
    NUM_IOAPIC_PINS,
};
#[cfg(target_arch = "x86_64")]
use crate::ClockData;
use crate::{
    CpuState, IoEventAddress, IrqRoutingEntry, MpState, UserMemoryRegion,
    USER_MEMORY_REGION_LOG_DIRTY, USER_MEMORY_REGION_READ, USER_MEMORY_REGION_WRITE,
};
#[cfg(target_arch = "aarch64")]
use aarch64::{RegList, Register, StandardRegisters};
#[cfg(target_arch = "x86_64")]
use kvm_bindings::{
    kvm_enable_cap, kvm_msr_entry, MsrList, KVM_CAP_HYPERV_SYNIC, KVM_CAP_SPLIT_IRQCHIP,
    KVM_GUESTDBG_USE_HW_BP,
};
#[cfg(target_arch = "x86_64")]
use x86_64::check_required_kvm_extensions;
#[cfg(target_arch = "x86_64")]
pub use x86_64::{CpuId, ExtendedControlRegisters, MsrEntries, VcpuKvmState};
// aarch64 dependencies
#[cfg(target_arch = "aarch64")]
pub mod aarch64;
pub use kvm_bindings;
#[cfg(feature = "tdx")]
pub use kvm_bindings::kvm_run__bindgen_ty_1;
pub use kvm_bindings::{
    kvm_clock_data, kvm_create_device, kvm_device_type_KVM_DEV_TYPE_VFIO, kvm_guest_debug,
    kvm_irq_routing, kvm_irq_routing_entry, kvm_irqfd, kvm_mp_state, kvm_userspace_memory_region,
    KVMIO, KVM_GUESTDBG_ENABLE, KVM_GUESTDBG_SINGLESTEP, KVM_IRQ_ROUTING_IRQCHIP,
    KVM_IRQ_ROUTING_MSI, KVM_MEM_LOG_DIRTY_PAGES, KVM_MEM_READONLY, KVM_MSI_VALID_DEVID,
};
#[cfg(target_arch = "aarch64")]
use kvm_bindings::{
    kvm_regs, user_fpsimd_state, user_pt_regs, KVM_GUESTDBG_USE_HW, KVM_NR_SPSR, KVM_REG_ARM64,
    KVM_REG_ARM64_SYSREG, KVM_REG_ARM64_SYSREG_CRM_MASK, KVM_REG_ARM64_SYSREG_CRN_MASK,
    KVM_REG_ARM64_SYSREG_OP0_MASK, KVM_REG_ARM64_SYSREG_OP1_MASK, KVM_REG_ARM64_SYSREG_OP2_MASK,
    KVM_REG_ARM_CORE, KVM_REG_SIZE_U128, KVM_REG_SIZE_U32, KVM_REG_SIZE_U64,
};
pub use kvm_ioctls;
pub use kvm_ioctls::{Cap, Kvm};
#[cfg(target_arch = "aarch64")]
use std::mem;
use thiserror::Error;
use vfio_ioctls::VfioDeviceFd;
#[cfg(feature = "tdx")]
use vmm_sys_util::{ioctl::ioctl_with_val, ioctl_ioc_nr, ioctl_iowr_nr};
///
/// Export generically-named wrappers of kvm-bindings for Unix-based platforms
///
pub use {
    kvm_bindings::kvm_create_device as CreateDevice, kvm_bindings::kvm_device_attr as DeviceAttr,
    kvm_bindings::kvm_run, kvm_bindings::kvm_vcpu_events as VcpuEvents, kvm_ioctls::VcpuExit,
};

#[cfg(target_arch = "x86_64")]
const KVM_CAP_SGX_ATTRIBUTE: u32 = 196;

#[cfg(feature = "tdx")]
const KVM_EXIT_TDX: u32 = 35;
#[cfg(feature = "tdx")]
const TDG_VP_VMCALL_GET_QUOTE: u64 = 0x10002;
#[cfg(feature = "tdx")]
const TDG_VP_VMCALL_SETUP_EVENT_NOTIFY_INTERRUPT: u64 = 0x10004;
#[cfg(feature = "tdx")]
const TDG_VP_VMCALL_SUCCESS: u64 = 0;
#[cfg(feature = "tdx")]
const TDG_VP_VMCALL_INVALID_OPERAND: u64 = 0x8000000000000000;

#[cfg(feature = "tdx")]
ioctl_iowr_nr!(KVM_MEMORY_ENCRYPT_OP, KVMIO, 0xba, std::os::raw::c_ulong);

#[cfg(feature = "tdx")]
#[repr(u32)]
enum TdxCommand {
    Capabilities = 0,
    InitVm,
    InitVcpu,
    InitMemRegion,
    Finalize,
}

#[cfg(feature = "tdx")]
pub enum TdxExitDetails {
    GetQuote,
    SetupEventNotifyInterrupt,
}

#[cfg(feature = "tdx")]
pub enum TdxExitStatus {
    Success,
    InvalidOperand,
}

#[cfg(feature = "tdx")]
const TDX_MAX_NR_CPUID_CONFIGS: usize = 6;

#[cfg(feature = "tdx")]
#[repr(C)]
#[derive(Debug, Default)]
pub struct TdxCpuidConfig {
    pub leaf: u32,
    pub sub_leaf: u32,
    pub eax: u32,
    pub ebx: u32,
    pub ecx: u32,
    pub edx: u32,
}

#[cfg(feature = "tdx")]
#[repr(C)]
#[derive(Debug, Default)]
pub struct TdxCapabilities {
    pub attrs_fixed0: u64,
    pub attrs_fixed1: u64,
    pub xfam_fixed0: u64,
    pub xfam_fixed1: u64,
    pub nr_cpuid_configs: u32,
    pub padding: u32,
    pub cpuid_configs: [TdxCpuidConfig; TDX_MAX_NR_CPUID_CONFIGS],
}

#[cfg(feature = "tdx")]
#[derive(Copy, Clone)]
pub struct KvmTdxExit {
    pub type_: u32,
    pub pad: u32,
    pub u: KvmTdxExitU,
}

#[cfg(feature = "tdx")]
#[repr(C)]
#[derive(Copy, Clone)]
pub union KvmTdxExitU {
    pub vmcall: KvmTdxExitVmcall,
}

#[cfg(feature = "tdx")]
#[repr(C)]
#[derive(Debug, Default, Copy, Clone, PartialEq)]
pub struct KvmTdxExitVmcall {
    pub type_: u64,
    pub subfunction: u64,
    pub reg_mask: u64,
    pub in_r12: u64,
    pub in_r13: u64,
    pub in_r14: u64,
    pub in_r15: u64,
    pub in_rbx: u64,
    pub in_rdi: u64,
    pub in_rsi: u64,
    pub in_r8: u64,
    pub in_r9: u64,
    pub in_rdx: u64,
    pub status_code: u64,
    pub out_r11: u64,
    pub out_r12: u64,
    pub out_r13: u64,
    pub out_r14: u64,
    pub out_r15: u64,
    pub out_rbx: u64,
    pub out_rdi: u64,
    pub out_rsi: u64,
    pub out_r8: u64,
    pub out_r9: u64,
    pub out_rdx: u64,
}

impl From<kvm_userspace_memory_region> for UserMemoryRegion {
    fn from(region: kvm_userspace_memory_region) -> Self {
        let mut flags = USER_MEMORY_REGION_READ;
        if region.flags & KVM_MEM_READONLY == 0 {
            flags |= USER_MEMORY_REGION_WRITE;
        }
        if region.flags & KVM_MEM_LOG_DIRTY_PAGES != 0 {
            flags |= USER_MEMORY_REGION_LOG_DIRTY;
        }

        UserMemoryRegion {
            slot: region.slot,
            guest_phys_addr: region.guest_phys_addr,
            memory_size: region.memory_size,
            userspace_addr: region.userspace_addr,
            flags,
        }
    }
}

impl From<UserMemoryRegion> for kvm_userspace_memory_region {
    fn from(region: UserMemoryRegion) -> Self {
        assert!(
            region.flags & USER_MEMORY_REGION_READ != 0,
            "KVM mapped memory is always readable"
        );

        let mut flags = 0;
        if region.flags & USER_MEMORY_REGION_WRITE == 0 {
            flags |= KVM_MEM_READONLY;
        }
        if region.flags & USER_MEMORY_REGION_LOG_DIRTY != 0 {
            flags |= KVM_MEM_LOG_DIRTY_PAGES;
        }

        kvm_userspace_memory_region {
            slot: region.slot,
            guest_phys_addr: region.guest_phys_addr,
            memory_size: region.memory_size,
            userspace_addr: region.userspace_addr,
            flags,
        }
    }
}

impl From<kvm_mp_state> for MpState {
    fn from(s: kvm_mp_state) -> Self {
        MpState::Kvm(s)
    }
}

impl From<MpState> for kvm_mp_state {
    fn from(ms: MpState) -> Self {
        match ms {
            MpState::Kvm(s) => s,
            /* Needed in case other hypervisors are enabled */
            #[allow(unreachable_patterns)]
            _ => panic!("CpuState is not valid"),
        }
    }
}

impl From<kvm_ioctls::IoEventAddress> for IoEventAddress {
    fn from(a: kvm_ioctls::IoEventAddress) -> Self {
        match a {
            kvm_ioctls::IoEventAddress::Pio(x) => Self::Pio(x),
            kvm_ioctls::IoEventAddress::Mmio(x) => Self::Mmio(x),
        }
    }
}

impl From<IoEventAddress> for kvm_ioctls::IoEventAddress {
    fn from(a: IoEventAddress) -> Self {
        match a {
            IoEventAddress::Pio(x) => Self::Pio(x),
            IoEventAddress::Mmio(x) => Self::Mmio(x),
        }
    }
}

impl From<VcpuKvmState> for CpuState {
    fn from(s: VcpuKvmState) -> Self {
        CpuState::Kvm(s)
    }
}

impl From<CpuState> for VcpuKvmState {
    fn from(s: CpuState) -> Self {
        match s {
            CpuState::Kvm(s) => s,
            /* Needed in case other hypervisors are enabled */
            #[allow(unreachable_patterns)]
            _ => panic!("CpuState is not valid"),
        }
    }
}

#[cfg(target_arch = "x86_64")]
impl From<kvm_clock_data> for ClockData {
    fn from(d: kvm_clock_data) -> Self {
        ClockData::Kvm(d)
    }
}

#[cfg(target_arch = "x86_64")]
impl From<ClockData> for kvm_clock_data {
    fn from(ms: ClockData) -> Self {
        match ms {
            ClockData::Kvm(s) => s,
            /* Needed in case other hypervisors are enabled */
            #[allow(unreachable_patterns)]
            _ => panic!("CpuState is not valid"),
        }
    }
}

impl From<kvm_irq_routing_entry> for IrqRoutingEntry {
    fn from(s: kvm_irq_routing_entry) -> Self {
        IrqRoutingEntry::Kvm(s)
    }
}

impl From<IrqRoutingEntry> for kvm_irq_routing_entry {
    fn from(e: IrqRoutingEntry) -> Self {
        match e {
            IrqRoutingEntry::Kvm(e) => e,
            /* Needed in case other hypervisors are enabled */
            #[allow(unreachable_patterns)]
            _ => panic!("IrqRoutingEntry is not valid"),
        }
    }
}

struct KvmDirtyLogSlot {
    slot: u32,
    guest_phys_addr: u64,
    memory_size: u64,
    userspace_addr: u64,
}

/// Wrapper over KVM VM ioctls.
pub struct KvmVm {
    fd: Arc<VmFd>,
    #[cfg(target_arch = "x86_64")]
    msrs: Vec<MsrEntry>,
    dirty_log_slots: Arc<RwLock<HashMap<u32, KvmDirtyLogSlot>>>,
}

impl KvmVm {
    ///
    /// Creates an emulated device in the kernel.
    ///
    /// See the documentation for `KVM_CREATE_DEVICE`.
    fn create_device(&self, device: &mut CreateDevice) -> vm::Result<vfio_ioctls::VfioDeviceFd> {
        let device_fd = self
            .fd
            .create_device(device)
            .map_err(|e| vm::HypervisorVmError::CreateDevice(e.into()))?;
        Ok(VfioDeviceFd::new_from_kvm(device_fd))
    }
    /// Checks if a particular `Cap` is available.
    fn check_extension(&self, c: Cap) -> bool {
        self.fd.check_extension(c)
    }
}

///
/// Implementation of Vm trait for KVM
/// Example:
/// #[cfg(feature = "kvm")]
/// extern crate hypervisor
/// let kvm = hypervisor::kvm::KvmHypervisor::new().unwrap();
/// let hypervisor: Arc<dyn hypervisor::Hypervisor> = Arc::new(kvm);
/// let vm = hypervisor.create_vm().expect("new VM fd creation failed");
/// vm.set/get().unwrap()
///
impl vm::Vm for KvmVm {
    #[cfg(target_arch = "x86_64")]
    ///
    /// Sets the address of the one-page region in the VM's address space.
    ///
    fn set_identity_map_address(&self, address: u64) -> vm::Result<()> {
        self.fd
            .set_identity_map_address(address)
            .map_err(|e| vm::HypervisorVmError::SetIdentityMapAddress(e.into()))
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// Sets the address of the three-page region in the VM's address space.
    ///
    fn set_tss_address(&self, offset: usize) -> vm::Result<()> {
        self.fd
            .set_tss_address(offset)
            .map_err(|e| vm::HypervisorVmError::SetTssAddress(e.into()))
    }
    ///
    /// Creates an in-kernel interrupt controller.
    ///
    fn create_irq_chip(&self) -> vm::Result<()> {
        self.fd
            .create_irq_chip()
            .map_err(|e| vm::HypervisorVmError::CreateIrq(e.into()))
    }
    ///
    /// Registers an event that will, when signaled, trigger the `gsi` IRQ.
    ///
    fn register_irqfd(&self, fd: &EventFd, gsi: u32) -> vm::Result<()> {
        self.fd
            .register_irqfd(fd, gsi)
            .map_err(|e| vm::HypervisorVmError::RegisterIrqFd(e.into()))
    }
    ///
    /// Unregisters an event that will, when signaled, trigger the `gsi` IRQ.
    ///
    fn unregister_irqfd(&self, fd: &EventFd, gsi: u32) -> vm::Result<()> {
        self.fd
            .unregister_irqfd(fd, gsi)
            .map_err(|e| vm::HypervisorVmError::UnregisterIrqFd(e.into()))
    }
    ///
    /// Creates a VcpuFd object from a vcpu RawFd.
    ///
    fn create_vcpu(
        &self,
        id: u8,
        vm_ops: Option<Arc<dyn VmOps>>,
    ) -> vm::Result<Arc<dyn cpu::Vcpu>> {
        let fd = self
            .fd
            .create_vcpu(id as u64)
            .map_err(|e| vm::HypervisorVmError::CreateVcpu(e.into()))?;
        let vcpu = KvmVcpu {
            fd: Arc::new(Mutex::new(fd)),
            #[cfg(target_arch = "x86_64")]
            msrs: self.msrs.clone(),
            vm_ops,
            #[cfg(target_arch = "x86_64")]
            hyperv_synic: AtomicBool::new(false),
        };
        Ok(Arc::new(vcpu))
    }
    #[cfg(target_arch = "aarch64")]
    ///
    /// Creates a virtual GIC device.
    ///
    fn create_vgic(&self, config: VgicConfig) -> vm::Result<Arc<Mutex<dyn Vgic>>> {
        let gic_device = KvmGicV3Its::new(self, config)
            .map_err(|e| vm::HypervisorVmError::CreateVgic(anyhow!("Vgic error {:?}", e)))?;
        Ok(Arc::new(Mutex::new(gic_device)))
    }
    ///
    /// Registers an event to be signaled whenever a certain address is written to.
    ///
    fn register_ioevent(
        &self,
        fd: &EventFd,
        addr: &IoEventAddress,
        datamatch: Option<vm::DataMatch>,
    ) -> vm::Result<()> {
        let addr = &kvm_ioctls::IoEventAddress::from(*addr);
        if let Some(dm) = datamatch {
            match dm {
                vm::DataMatch::DataMatch32(kvm_dm32) => self
                    .fd
                    .register_ioevent(fd, addr, kvm_dm32)
                    .map_err(|e| vm::HypervisorVmError::RegisterIoEvent(e.into())),
                vm::DataMatch::DataMatch64(kvm_dm64) => self
                    .fd
                    .register_ioevent(fd, addr, kvm_dm64)
                    .map_err(|e| vm::HypervisorVmError::RegisterIoEvent(e.into())),
            }
        } else {
            self.fd
                .register_ioevent(fd, addr, NoDatamatch)
                .map_err(|e| vm::HypervisorVmError::RegisterIoEvent(e.into()))
        }
    }
    ///
    /// Unregisters an event from a certain address it has been previously registered to.
    ///
    fn unregister_ioevent(&self, fd: &EventFd, addr: &IoEventAddress) -> vm::Result<()> {
        let addr = &kvm_ioctls::IoEventAddress::from(*addr);
        self.fd
            .unregister_ioevent(fd, addr, NoDatamatch)
            .map_err(|e| vm::HypervisorVmError::UnregisterIoEvent(e.into()))
    }

    ///
    /// Constructs a routing entry
    ///
    fn make_routing_entry(&self, gsi: u32, config: &InterruptSourceConfig) -> IrqRoutingEntry {
        match &config {
            InterruptSourceConfig::MsiIrq(cfg) => {
                let mut kvm_route = kvm_irq_routing_entry {
                    gsi,
                    type_: KVM_IRQ_ROUTING_MSI,
                    ..Default::default()
                };

                kvm_route.u.msi.address_lo = cfg.low_addr;
                kvm_route.u.msi.address_hi = cfg.high_addr;
                kvm_route.u.msi.data = cfg.data;

                if self.check_extension(crate::kvm::Cap::MsiDevid) {
                    // On AArch64, there is limitation on the range of the 'devid',
                    // it can not be greater than 65536 (the max of u16).
                    //
                    // BDF can not be used directly, because 'segment' is in high
                    // 16 bits. The layout of the u32 BDF is:
                    // |---- 16 bits ----|-- 8 bits --|-- 5 bits --|-- 3 bits --|
                    // |      segment    |     bus    |   device   |  function  |
                    //
                    // Now that we support 1 bus only in a segment, we can build a
                    // 'devid' by replacing the 'bus' bits with the low 8 bits of
                    // 'segment' data.
                    // This way we can resolve the range checking problem and give
                    // different `devid` to all the devices. Limitation is that at
                    // most 256 segments can be supported.
                    //
                    let modified_devid = (cfg.devid & 0x00ff_0000) >> 8 | cfg.devid & 0xff;

                    kvm_route.flags = KVM_MSI_VALID_DEVID;
                    kvm_route.u.msi.__bindgen_anon_1.devid = modified_devid;
                }
                kvm_route.into()
            }
            InterruptSourceConfig::LegacyIrq(cfg) => {
                let mut kvm_route = kvm_irq_routing_entry {
                    gsi,
                    type_: KVM_IRQ_ROUTING_IRQCHIP,
                    ..Default::default()
                };
                kvm_route.u.irqchip.irqchip = cfg.irqchip;
                kvm_route.u.irqchip.pin = cfg.pin;

                kvm_route.into()
            }
        }
    }

    ///
    /// Sets the GSI routing table entries, overwriting any previously set
    /// entries, as per the `KVM_SET_GSI_ROUTING` ioctl.
    ///
    fn set_gsi_routing(&self, entries: &[IrqRoutingEntry]) -> vm::Result<()> {
        let mut irq_routing =
            vec_with_array_field::<kvm_irq_routing, kvm_irq_routing_entry>(entries.len());
        irq_routing[0].nr = entries.len() as u32;
        irq_routing[0].flags = 0;
        let entries: Vec<kvm_irq_routing_entry> = entries
            .iter()
            .map(|entry| match entry {
                IrqRoutingEntry::Kvm(e) => *e,
                #[allow(unreachable_patterns)]
                _ => panic!("IrqRoutingEntry type is wrong"),
            })
            .collect();

        // SAFETY: irq_routing initialized with entries.len() and now it is being turned into
        // entries_slice with entries.len() again. It is guaranteed to be large enough to hold
        // everything from entries.
        unsafe {
            let entries_slice: &mut [kvm_irq_routing_entry] =
                irq_routing[0].entries.as_mut_slice(entries.len());
            entries_slice.copy_from_slice(&entries);
        }

        self.fd
            .set_gsi_routing(&irq_routing[0])
            .map_err(|e| vm::HypervisorVmError::SetGsiRouting(e.into()))
    }
    ///
    /// Creates a memory region structure that can be used with {create/remove}_user_memory_region
    ///
    fn make_user_memory_region(
        &self,
        slot: u32,
        guest_phys_addr: u64,
        memory_size: u64,
        userspace_addr: u64,
        readonly: bool,
        log_dirty_pages: bool,
    ) -> UserMemoryRegion {
        kvm_userspace_memory_region {
            slot,
            guest_phys_addr,
            memory_size,
            userspace_addr,
            flags: if readonly { KVM_MEM_READONLY } else { 0 }
                | if log_dirty_pages {
                    KVM_MEM_LOG_DIRTY_PAGES
                } else {
                    0
                },
        }
        .into()
    }
    ///
    /// Creates a guest physical memory region.
    ///
    fn create_user_memory_region(&self, user_memory_region: UserMemoryRegion) -> vm::Result<()> {
        let mut region: kvm_userspace_memory_region = user_memory_region.into();

        if (region.flags & KVM_MEM_LOG_DIRTY_PAGES) != 0 {
            if (region.flags & KVM_MEM_READONLY) != 0 {
                return Err(vm::HypervisorVmError::CreateUserMemory(anyhow!(
                    "Error creating regions with both 'dirty-pages-log' and 'read-only'."
                )));
            }

            // Keep track of the regions that need dirty pages log
            self.dirty_log_slots.write().unwrap().insert(
                region.slot,
                KvmDirtyLogSlot {
                    slot: region.slot,
                    guest_phys_addr: region.guest_phys_addr,
                    memory_size: region.memory_size,
                    userspace_addr: region.userspace_addr,
                },
            );

            // Always create guest physical memory region without `KVM_MEM_LOG_DIRTY_PAGES`.
            // For regions that need this flag, dirty pages log will be turned on in `start_dirty_log`.
            region.flags = 0;
        }

        // SAFETY: Safe because guest regions are guaranteed not to overlap.
        unsafe {
            self.fd
                .set_user_memory_region(region)
                .map_err(|e| vm::HypervisorVmError::CreateUserMemory(e.into()))
        }
    }
    ///
    /// Removes a guest physical memory region.
    ///
    fn remove_user_memory_region(&self, user_memory_region: UserMemoryRegion) -> vm::Result<()> {
        let mut region: kvm_userspace_memory_region = user_memory_region.into();

        // Remove the corresponding entry from "self.dirty_log_slots" if needed
        self.dirty_log_slots.write().unwrap().remove(&region.slot);

        // Setting the size to 0 means "remove"
        region.memory_size = 0;
        // SAFETY: Safe because guest regions are guaranteed not to overlap.
        unsafe {
            self.fd
                .set_user_memory_region(region)
                .map_err(|e| vm::HypervisorVmError::RemoveUserMemory(e.into()))
        }
    }
    ///
    /// Returns the preferred CPU target type which can be emulated by KVM on underlying host.
    ///
    #[cfg(target_arch = "aarch64")]
    fn get_preferred_target(&self, kvi: &mut VcpuInit) -> vm::Result<()> {
        self.fd
            .get_preferred_target(kvi)
            .map_err(|e| vm::HypervisorVmError::GetPreferredTarget(e.into()))
    }
    #[cfg(target_arch = "x86_64")]
    fn enable_split_irq(&self) -> vm::Result<()> {
        // Create split irqchip
        // Only the local APIC is emulated in kernel, both PICs and IOAPIC
        // are not.
        let mut cap = kvm_enable_cap {
            cap: KVM_CAP_SPLIT_IRQCHIP,
            ..Default::default()
        };
        cap.args[0] = NUM_IOAPIC_PINS as u64;
        self.fd
            .enable_cap(&cap)
            .map_err(|e| vm::HypervisorVmError::EnableSplitIrq(e.into()))?;
        Ok(())
    }
    #[cfg(target_arch = "x86_64")]
    fn enable_sgx_attribute(&self, file: File) -> vm::Result<()> {
        let mut cap = kvm_enable_cap {
            cap: KVM_CAP_SGX_ATTRIBUTE,
            ..Default::default()
        };
        cap.args[0] = file.as_raw_fd() as u64;
        self.fd
            .enable_cap(&cap)
            .map_err(|e| vm::HypervisorVmError::EnableSgxAttribute(e.into()))?;
        Ok(())
    }
    /// Retrieve guest clock.
    #[cfg(target_arch = "x86_64")]
    fn get_clock(&self) -> vm::Result<ClockData> {
        Ok(self
            .fd
            .get_clock()
            .map_err(|e| vm::HypervisorVmError::GetClock(e.into()))?
            .into())
    }
    /// Set guest clock.
    #[cfg(target_arch = "x86_64")]
    fn set_clock(&self, data: &ClockData) -> vm::Result<()> {
        let data = (*data).into();
        self.fd
            .set_clock(&data)
            .map_err(|e| vm::HypervisorVmError::SetClock(e.into()))
    }
    /// Create a device that is used for passthrough
    fn create_passthrough_device(&self) -> vm::Result<VfioDeviceFd> {
        let mut vfio_dev = kvm_create_device {
            type_: kvm_device_type_KVM_DEV_TYPE_VFIO,
            fd: 0,
            flags: 0,
        };

        self.create_device(&mut vfio_dev)
            .map_err(|e| vm::HypervisorVmError::CreatePassthroughDevice(e.into()))
    }
    ///
    /// Start logging dirty pages
    ///
    fn start_dirty_log(&self) -> vm::Result<()> {
        let dirty_log_slots = self.dirty_log_slots.read().unwrap();
        for (_, s) in dirty_log_slots.iter() {
            let region = kvm_userspace_memory_region {
                slot: s.slot,
                guest_phys_addr: s.guest_phys_addr,
                memory_size: s.memory_size,
                userspace_addr: s.userspace_addr,
                flags: KVM_MEM_LOG_DIRTY_PAGES,
            };
            // SAFETY: Safe because guest regions are guaranteed not to overlap.
            unsafe {
                self.fd
                    .set_user_memory_region(region)
                    .map_err(|e| vm::HypervisorVmError::StartDirtyLog(e.into()))?;
            }
        }

        Ok(())
    }

    ///
    /// Stop logging dirty pages
    ///
    fn stop_dirty_log(&self) -> vm::Result<()> {
        let dirty_log_slots = self.dirty_log_slots.read().unwrap();
        for (_, s) in dirty_log_slots.iter() {
            let region = kvm_userspace_memory_region {
                slot: s.slot,
                guest_phys_addr: s.guest_phys_addr,
                memory_size: s.memory_size,
                userspace_addr: s.userspace_addr,
                flags: 0,
            };
            // SAFETY: Safe because guest regions are guaranteed not to overlap.
            unsafe {
                self.fd
                    .set_user_memory_region(region)
                    .map_err(|e| vm::HypervisorVmError::StartDirtyLog(e.into()))?;
            }
        }

        Ok(())
    }

    ///
    /// Get dirty pages bitmap (one bit per page)
    ///
    fn get_dirty_log(&self, slot: u32, _base_gpa: u64, memory_size: u64) -> vm::Result<Vec<u64>> {
        self.fd
            .get_dirty_log(slot, memory_size as usize)
            .map_err(|e| vm::HypervisorVmError::GetDirtyLog(e.into()))
    }

    ///
    /// Initialize TDX for this VM
    ///
    #[cfg(feature = "tdx")]
    fn tdx_init(&self, cpuid: &[CpuIdEntry], max_vcpus: u32) -> vm::Result<()> {
        use std::io::{Error, ErrorKind};
        let cpuid: Vec<kvm_bindings::kvm_cpuid_entry2> =
            cpuid.iter().map(|e| (*e).into()).collect();
        let kvm_cpuid = kvm_bindings::CpuId::from_entries(&cpuid).map_err(|_| {
            vm::HypervisorVmError::InitializeTdx(Error::new(
                ErrorKind::Other,
                "failed to allocate CpuId",
            ))
        })?;

        #[repr(C)]
        struct TdxInitVm {
            max_vcpus: u32,
            tsc_khz: u32,
            attributes: u64,
            cpuid: u64,
            mrconfigid: [u64; 6],
            mrowner: [u64; 6],
            mrownerconfig: [u64; 6],
            reserved: [u64; 43],
        }
        let data = TdxInitVm {
            max_vcpus,
            tsc_khz: 0,
            attributes: 0,
            cpuid: kvm_cpuid.as_fam_struct_ptr() as u64,
            mrconfigid: [0; 6],
            mrowner: [0; 6],
            mrownerconfig: [0; 6],
            reserved: [0; 43],
        };

        tdx_command(
            &self.fd.as_raw_fd(),
            TdxCommand::InitVm,
            0,
            &data as *const _ as u64,
        )
        .map_err(vm::HypervisorVmError::InitializeTdx)
    }

    ///
    /// Finalize the TDX setup for this VM
    ///
    #[cfg(feature = "tdx")]
    fn tdx_finalize(&self) -> vm::Result<()> {
        tdx_command(&self.fd.as_raw_fd(), TdxCommand::Finalize, 0, 0)
            .map_err(vm::HypervisorVmError::FinalizeTdx)
    }

    ///
    /// Initialize memory regions for the TDX VM
    ///
    #[cfg(feature = "tdx")]
    fn tdx_init_memory_region(
        &self,
        host_address: u64,
        guest_address: u64,
        size: u64,
        measure: bool,
    ) -> vm::Result<()> {
        #[repr(C)]
        struct TdxInitMemRegion {
            host_address: u64,
            guest_address: u64,
            pages: u64,
        }
        let data = TdxInitMemRegion {
            host_address,
            guest_address,
            pages: size / 4096,
        };

        tdx_command(
            &self.fd.as_raw_fd(),
            TdxCommand::InitMemRegion,
            u32::from(measure),
            &data as *const _ as u64,
        )
        .map_err(vm::HypervisorVmError::InitMemRegionTdx)
    }
    /// Downcast to the underlying KvmVm type
    fn as_any(&self) -> &dyn Any {
        self
    }
}

#[cfg(feature = "tdx")]
fn tdx_command(
    fd: &RawFd,
    command: TdxCommand,
    metadata: u32,
    data: u64,
) -> std::result::Result<(), std::io::Error> {
    #[repr(C)]
    struct TdxIoctlCmd {
        command: TdxCommand,
        metadata: u32,
        data: u64,
    }
    let cmd = TdxIoctlCmd {
        command,
        metadata,
        data,
    };
    // SAFETY: FFI call. All input parameters are valid.
    let ret = unsafe {
        ioctl_with_val(
            fd,
            KVM_MEMORY_ENCRYPT_OP(),
            &cmd as *const TdxIoctlCmd as std::os::raw::c_ulong,
        )
    };

    if ret < 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(())
}

/// Wrapper over KVM system ioctls.
pub struct KvmHypervisor {
    kvm: Kvm,
    hypervisor_type: HypervisorType,
}

impl KvmHypervisor {
    #[cfg(target_arch = "x86_64")]
    ///
    /// Retrieve the list of MSRs supported by the hypervisor.
    ///
    fn get_msr_list(&self) -> hypervisor::Result<MsrList> {
        self.kvm
            .get_msr_index_list()
            .map_err(|e| hypervisor::HypervisorError::GetMsrList(e.into()))
    }
}

/// Enum for KVM related error
#[derive(Debug, Error)]
pub enum KvmError {
    #[error("Capability missing: {0:?}")]
    CapabilityMissing(Cap),
}
pub type KvmResult<T> = result::Result<T, KvmError>;
impl KvmHypervisor {
    /// Create a hypervisor based on Kvm
    #[allow(clippy::new_ret_no_self)]
    pub fn new() -> hypervisor::Result<Arc<dyn hypervisor::Hypervisor>> {
        let kvm_obj = Kvm::new().map_err(|e| hypervisor::HypervisorError::VmCreate(e.into()))?;
        let api_version = kvm_obj.get_api_version();

        if api_version != kvm_bindings::KVM_API_VERSION as i32 {
            return Err(hypervisor::HypervisorError::IncompatibleApiVersion);
        }

        #[cfg_attr(not(target_arch = "x86_64"), allow(unused_mut))]
        let mut hypervisor_type = HypervisorType::Kvm;

        #[cfg(target_arch = "x86_64")]
        if let Ok(cpuid) = kvm_obj.get_supported_cpuid(kvm_bindings::KVM_MAX_CPUID_ENTRIES) {
            for entry in cpuid.as_slice().iter() {
                if entry.function == 0x4000_0002 && entry.ebx == 0x4d_5650 {
                    hypervisor_type = HypervisorType::KvmPvm;
                }
            }
        }

        Ok(Arc::new(KvmHypervisor {
            kvm: kvm_obj,
            hypervisor_type,
        }))
    }
    /// Check if the hypervisor is available
    pub fn is_available() -> hypervisor::Result<bool> {
        match std::fs::metadata("/dev/kvm") {
            Ok(_) => Ok(true),
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(false),
            Err(err) => Err(hypervisor::HypervisorError::HypervisorAvailableCheck(
                err.into(),
            )),
        }
    }
}
/// Implementation of Hypervisor trait for KVM
/// Example:
/// #[cfg(feature = "kvm")]
/// extern crate hypervisor
/// let kvm = hypervisor::kvm::KvmHypervisor::new().unwrap();
/// let hypervisor: Arc<dyn hypervisor::Hypervisor> = Arc::new(kvm);
/// let vm = hypervisor.create_vm().expect("new VM fd creation failed");
///
impl hypervisor::Hypervisor for KvmHypervisor {
    ///
    /// Returns the type of the hypervisor
    ///
    fn hypervisor_type(&self) -> HypervisorType {
        self.hypervisor_type
    }

    /// Create a KVM vm object of a specific VM type and return the object as Vm trait object
    /// Example
    /// # extern crate hypervisor;
    /// # use hypervisor::KvmHypervisor;
    /// use hypervisor::KvmVm;
    /// let hypervisor = KvmHypervisor::new().unwrap();
    /// let vm = hypervisor.create_vm_with_type(KvmVmType::LegacyVm).unwrap()
    ///
    fn create_vm_with_type(&self, vm_type: u64) -> hypervisor::Result<Arc<dyn vm::Vm>> {
        let fd: VmFd;
        loop {
            match self.kvm.create_vm_with_type(vm_type) {
                Ok(res) => fd = res,
                Err(e) => {
                    if e.errno() == libc::EINTR {
                        // If the error returned is EINTR, which means the
                        // ioctl has been interrupted, we have to retry as
                        // this can't be considered as a regular error.
                        continue;
                    } else {
                        return Err(hypervisor::HypervisorError::VmCreate(e.into()));
                    }
                }
            }
            break;
        }

        let vm_fd = Arc::new(fd);

        #[cfg(target_arch = "x86_64")]
        {
            let msr_list = self.get_msr_list()?;
            let num_msrs = msr_list.as_fam_struct_ref().nmsrs as usize;
            let mut msrs: Vec<MsrEntry> = vec![
                MsrEntry {
                    ..Default::default()
                };
                num_msrs
            ];
            let indices = msr_list.as_slice();
            for (pos, index) in indices.iter().enumerate() {
                msrs[pos].index = *index;
            }

            Ok(Arc::new(KvmVm {
                fd: vm_fd,
                msrs,
                dirty_log_slots: Arc::new(RwLock::new(HashMap::new())),
            }))
        }

        #[cfg(target_arch = "aarch64")]
        {
            Ok(Arc::new(KvmVm {
                fd: vm_fd,
                dirty_log_slots: Arc::new(RwLock::new(HashMap::new())),
            }))
        }
    }

    /// Create a KVM vm object and return the object as Vm trait object
    /// Example
    /// # extern crate hypervisor;
    /// # use hypervisor::KvmHypervisor;
    /// use hypervisor::KvmVm;
    /// let hypervisor = KvmHypervisor::new().unwrap();
    /// let vm = hypervisor.create_vm().unwrap()
    ///
    fn create_vm(&self) -> hypervisor::Result<Arc<dyn vm::Vm>> {
        #[allow(unused_mut)]
        let mut vm_type: u64 = 0; // Create with default platform type

        // When KVM supports Cap::ArmVmIPASize, it is better to get the IPA
        // size from the host and use that when creating the VM, which may
        // avoid unnecessary VM creation failures.
        #[cfg(target_arch = "aarch64")]
        if self.kvm.check_extension(Cap::ArmVmIPASize) {
            vm_type = self.kvm.get_host_ipa_limit().try_into().unwrap();
        }

        self.create_vm_with_type(vm_type)
    }

    fn check_required_extensions(&self) -> hypervisor::Result<()> {
        check_required_kvm_extensions(&self.kvm)
            .map_err(|e| hypervisor::HypervisorError::CheckExtensions(e.into()))
    }

    #[cfg(target_arch = "x86_64")]
    ///
    /// X86 specific call to get the system supported CPUID values.
    ///
    fn get_cpuid(&self) -> hypervisor::Result<Vec<CpuIdEntry>> {
        let kvm_cpuid = self
            .kvm
            .get_supported_cpuid(kvm_bindings::KVM_MAX_CPUID_ENTRIES)
            .map_err(|e| hypervisor::HypervisorError::GetCpuId(e.into()))?;

        let v = kvm_cpuid.as_slice().iter().map(|e| (*e).into()).collect();

        Ok(v)
    }

    #[cfg(target_arch = "aarch64")]
    ///
    /// Retrieve AArch64 host maximum IPA size supported by KVM.
    ///
    fn get_host_ipa_limit(&self) -> i32 {
        self.kvm.get_host_ipa_limit()
    }

    ///
    /// Retrieve TDX capabilities
    ///
    #[cfg(feature = "tdx")]
    fn tdx_capabilities(&self) -> hypervisor::Result<TdxCapabilities> {
        let data = TdxCapabilities {
            nr_cpuid_configs: TDX_MAX_NR_CPUID_CONFIGS as u32,
            ..Default::default()
        };

        tdx_command(
            &self.kvm.as_raw_fd(),
            TdxCommand::Capabilities,
            0,
            &data as *const _ as u64,
        )
        .map_err(|e| hypervisor::HypervisorError::TdxCapabilities(e.into()))?;

        Ok(data)
    }

    ///
    /// Get the number of supported hardware breakpoints
    ///
    fn get_guest_debug_hw_bps(&self) -> usize {
        #[cfg(target_arch = "x86_64")]
        {
            4
        }
        #[cfg(target_arch = "aarch64")]
        {
            self.kvm.get_guest_debug_hw_bps() as usize
        }
    }
}
/// Vcpu struct for KVM
pub struct KvmVcpu {
    fd: Arc<Mutex<VcpuFd>>,
    #[cfg(target_arch = "x86_64")]
    msrs: Vec<MsrEntry>,
    vm_ops: Option<Arc<dyn vm::VmOps>>,
    #[cfg(target_arch = "x86_64")]
    hyperv_synic: AtomicBool,
}
/// Implementation of Vcpu trait for KVM
/// Example:
/// #[cfg(feature = "kvm")]
/// extern crate hypervisor
/// let kvm = hypervisor::kvm::KvmHypervisor::new().unwrap();
/// let hypervisor: Arc<dyn hypervisor::Hypervisor> = Arc::new(kvm);
/// let vm = hypervisor.create_vm().expect("new VM fd creation failed");
/// let vcpu = vm.create_vcpu(0, None).unwrap();
/// vcpu.get/set().unwrap()
///
impl cpu::Vcpu for KvmVcpu {
    #[cfg(target_arch = "x86_64")]
    ///
    /// Returns the vCPU general purpose registers.
    ///
    fn get_regs(&self) -> cpu::Result<StandardRegisters> {
        Ok(self
            .fd
            .lock()
            .unwrap()
            .get_regs()
            .map_err(|e| cpu::HypervisorCpuError::GetStandardRegs(e.into()))?
            .into())
    }
    ///
    /// Returns the vCPU general purpose registers.
    /// The `KVM_GET_REGS` ioctl is not available on AArch64, `KVM_GET_ONE_REG`
    /// is used to get registers one by one.
    ///
    #[cfg(target_arch = "aarch64")]
    fn get_regs(&self) -> cpu::Result<StandardRegisters> {
        let mut state: StandardRegisters = kvm_regs::default();
        let mut off = offset__of!(user_pt_regs, regs);
        // There are 31 user_pt_regs:
        // https://elixir.free-electrons.com/linux/v4.14.174/source/arch/arm64/include/uapi/asm/ptrace.h#L72
        // These actually are the general-purpose registers of the Armv8-a
        // architecture (i.e x0-x30 if used as a 64bit register or w0-30 when used as a 32bit register).
        for i in 0..31 {
            let mut bytes = [0_u8; 8];
            self.fd
                .lock()
                .unwrap()
                .get_one_reg(arm64_core_reg_id!(KVM_REG_SIZE_U64, off), &mut bytes)
                .map_err(|e| cpu::HypervisorCpuError::GetCoreRegister(e.into()))?;
            state.regs.regs[i] = u64::from_le_bytes(bytes);
            off += std::mem::size_of::<u64>();
        }

        // We are now entering the "Other register" section of the ARMv8-a architecture.
        // First one, stack pointer.
        let off = offset__of!(user_pt_regs, sp);
        let mut bytes = [0_u8; 8];
        self.fd
            .lock()
            .unwrap()
            .get_one_reg(arm64_core_reg_id!(KVM_REG_SIZE_U64, off), &mut bytes)
            .map_err(|e| cpu::HypervisorCpuError::GetCoreRegister(e.into()))?;
        state.regs.sp = u64::from_le_bytes(bytes);

        // Second one, the program counter.
        let off = offset__of!(user_pt_regs, pc);
        let mut bytes = [0_u8; 8];
        self.fd
            .lock()
            .unwrap()
            .get_one_reg(arm64_core_reg_id!(KVM_REG_SIZE_U64, off), &mut bytes)
            .map_err(|e| cpu::HypervisorCpuError::GetCoreRegister(e.into()))?;
        state.regs.pc = u64::from_le_bytes(bytes);

        // Next is the processor state.
        let off = offset__of!(user_pt_regs, pstate);
        let mut bytes = [0_u8; 8];
        self.fd
            .lock()
            .unwrap()
            .get_one_reg(arm64_core_reg_id!(KVM_REG_SIZE_U64, off), &mut bytes)
            .map_err(|e| cpu::HypervisorCpuError::GetCoreRegister(e.into()))?;
        state.regs.pstate = u64::from_le_bytes(bytes);

        // The stack pointer associated with EL1
        let off = offset__of!(kvm_regs, sp_el1);
        let mut bytes = [0_u8; 8];
        self.fd
            .lock()
            .unwrap()
            .get_one_reg(arm64_core_reg_id!(KVM_REG_SIZE_U64, off), &mut bytes)
            .map_err(|e| cpu::HypervisorCpuError::GetCoreRegister(e.into()))?;
        state.sp_el1 = u64::from_le_bytes(bytes);

        // Exception Link Register for EL1, when taking an exception to EL1, this register
        // holds the address to which to return afterwards.
        let off = offset__of!(kvm_regs, elr_el1);
        let mut bytes = [0_u8; 8];
        self.fd
            .lock()
            .unwrap()
            .get_one_reg(arm64_core_reg_id!(KVM_REG_SIZE_U64, off), &mut bytes)
            .map_err(|e| cpu::HypervisorCpuError::GetCoreRegister(e.into()))?;
        state.elr_el1 = u64::from_le_bytes(bytes);

        // Saved Program Status Registers, there are 5 of them used in the kernel.
        let mut off = offset__of!(kvm_regs, spsr);
        for i in 0..KVM_NR_SPSR as usize {
            let mut bytes = [0_u8; 8];
            self.fd
                .lock()
                .unwrap()
                .get_one_reg(arm64_core_reg_id!(KVM_REG_SIZE_U64, off), &mut bytes)
                .map_err(|e| cpu::HypervisorCpuError::GetCoreRegister(e.into()))?;
            state.spsr[i] = u64::from_le_bytes(bytes);
            off += std::mem::size_of::<u64>();
        }

        // Now moving on to floating point registers which are stored in the user_fpsimd_state in the kernel:
        // https://elixir.free-electrons.com/linux/v4.9.62/source/arch/arm64/include/uapi/asm/kvm.h#L53
        let mut off = offset__of!(kvm_regs, fp_regs) + offset__of!(user_fpsimd_state, vregs);
        for i in 0..32 {
            let mut bytes = [0_u8; 16];
            self.fd
                .lock()
                .unwrap()
                .get_one_reg(arm64_core_reg_id!(KVM_REG_SIZE_U128, off), &mut bytes)
                .map_err(|e| cpu::HypervisorCpuError::GetCoreRegister(e.into()))?;
            state.fp_regs.vregs[i] = u128::from_le_bytes(bytes);
            off += mem::size_of::<u128>();
        }

        // Floating-point Status Register
        let off = offset__of!(kvm_regs, fp_regs) + offset__of!(user_fpsimd_state, fpsr);
        let mut bytes = [0_u8; 4];
        self.fd
            .lock()
            .unwrap()
            .get_one_reg(arm64_core_reg_id!(KVM_REG_SIZE_U32, off), &mut bytes)
            .map_err(|e| cpu::HypervisorCpuError::GetCoreRegister(e.into()))?;
        state.fp_regs.fpsr = u32::from_le_bytes(bytes);

        // Floating-point Control Register
        let off = offset__of!(kvm_regs, fp_regs) + offset__of!(user_fpsimd_state, fpcr);
        let mut bytes = [0_u8; 4];
        self.fd
            .lock()
            .unwrap()
            .get_one_reg(arm64_core_reg_id!(KVM_REG_SIZE_U32, off), &mut bytes)
            .map_err(|e| cpu::HypervisorCpuError::GetCoreRegister(e.into()))?;
        state.fp_regs.fpcr = u32::from_le_bytes(bytes);
        Ok(state)
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// Sets the vCPU general purpose registers using the `KVM_SET_REGS` ioctl.
    ///
    fn set_regs(&self, regs: &StandardRegisters) -> cpu::Result<()> {
        let regs = (*regs).into();
        self.fd
            .lock()
            .unwrap()
            .set_regs(&regs)
            .map_err(|e| cpu::HypervisorCpuError::SetStandardRegs(e.into()))
    }

    ///
    /// Sets the vCPU general purpose registers.
    /// The `KVM_SET_REGS` ioctl is not available on AArch64, `KVM_SET_ONE_REG`
    /// is used to set registers one by one.
    ///
    #[cfg(target_arch = "aarch64")]
    fn set_regs(&self, state: &StandardRegisters) -> cpu::Result<()> {
        // The function follows the exact identical order from `state`. Look there
        // for some additional info on registers.
        let mut off = offset__of!(user_pt_regs, regs);
        for i in 0..31 {
            self.fd
                .lock()
                .unwrap()
                .set_one_reg(
                    arm64_core_reg_id!(KVM_REG_SIZE_U64, off),
                    &state.regs.regs[i].to_le_bytes(),
                )
                .map_err(|e| cpu::HypervisorCpuError::SetCoreRegister(e.into()))?;
            off += std::mem::size_of::<u64>();
        }

        let off = offset__of!(user_pt_regs, sp);
        self.fd
            .lock()
            .unwrap()
            .set_one_reg(
                arm64_core_reg_id!(KVM_REG_SIZE_U64, off),
                &state.regs.sp.to_le_bytes(),
            )
            .map_err(|e| cpu::HypervisorCpuError::SetCoreRegister(e.into()))?;

        let off = offset__of!(user_pt_regs, pc);
        self.fd
            .lock()
            .unwrap()
            .set_one_reg(
                arm64_core_reg_id!(KVM_REG_SIZE_U64, off),
                &state.regs.pc.to_le_bytes(),
            )
            .map_err(|e| cpu::HypervisorCpuError::SetCoreRegister(e.into()))?;

        let off = offset__of!(user_pt_regs, pstate);
        self.fd
            .lock()
            .unwrap()
            .set_one_reg(
                arm64_core_reg_id!(KVM_REG_SIZE_U64, off),
                &state.regs.pstate.to_le_bytes(),
            )
            .map_err(|e| cpu::HypervisorCpuError::SetCoreRegister(e.into()))?;

        let off = offset__of!(kvm_regs, sp_el1);
        self.fd
            .lock()
            .unwrap()
            .set_one_reg(
                arm64_core_reg_id!(KVM_REG_SIZE_U64, off),
                &state.sp_el1.to_le_bytes(),
            )
            .map_err(|e| cpu::HypervisorCpuError::SetCoreRegister(e.into()))?;

        let off = offset__of!(kvm_regs, elr_el1);
        self.fd
            .lock()
            .unwrap()
            .set_one_reg(
                arm64_core_reg_id!(KVM_REG_SIZE_U64, off),
                &state.elr_el1.to_le_bytes(),
            )
            .map_err(|e| cpu::HypervisorCpuError::SetCoreRegister(e.into()))?;

        let mut off = offset__of!(kvm_regs, spsr);
        for i in 0..KVM_NR_SPSR as usize {
            self.fd
                .lock()
                .unwrap()
                .set_one_reg(
                    arm64_core_reg_id!(KVM_REG_SIZE_U64, off),
                    &state.spsr[i].to_le_bytes(),
                )
                .map_err(|e| cpu::HypervisorCpuError::SetCoreRegister(e.into()))?;
            off += std::mem::size_of::<u64>();
        }

        let mut off = offset__of!(kvm_regs, fp_regs) + offset__of!(user_fpsimd_state, vregs);
        for i in 0..32 {
            self.fd
                .lock()
                .unwrap()
                .set_one_reg(
                    arm64_core_reg_id!(KVM_REG_SIZE_U128, off),
                    &state.fp_regs.vregs[i].to_le_bytes(),
                )
                .map_err(|e| cpu::HypervisorCpuError::SetCoreRegister(e.into()))?;
            off += mem::size_of::<u128>();
        }

        let off = offset__of!(kvm_regs, fp_regs) + offset__of!(user_fpsimd_state, fpsr);
        self.fd
            .lock()
            .unwrap()
            .set_one_reg(
                arm64_core_reg_id!(KVM_REG_SIZE_U32, off),
                &state.fp_regs.fpsr.to_le_bytes(),
            )
            .map_err(|e| cpu::HypervisorCpuError::SetCoreRegister(e.into()))?;

        let off = offset__of!(kvm_regs, fp_regs) + offset__of!(user_fpsimd_state, fpcr);
        self.fd
            .lock()
            .unwrap()
            .set_one_reg(
                arm64_core_reg_id!(KVM_REG_SIZE_U32, off),
                &state.fp_regs.fpcr.to_le_bytes(),
            )
            .map_err(|e| cpu::HypervisorCpuError::SetCoreRegister(e.into()))?;
        Ok(())
    }

    #[cfg(target_arch = "x86_64")]
    ///
    /// Returns the vCPU special registers.
    ///
    fn get_sregs(&self) -> cpu::Result<SpecialRegisters> {
        Ok(self
            .fd
            .lock()
            .unwrap()
            .get_sregs()
            .map_err(|e| cpu::HypervisorCpuError::GetSpecialRegs(e.into()))?
            .into())
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// Sets the vCPU special registers using the `KVM_SET_SREGS` ioctl.
    ///
    fn set_sregs(&self, sregs: &SpecialRegisters) -> cpu::Result<()> {
        let sregs = (*sregs).into();
        self.fd
            .lock()
            .unwrap()
            .set_sregs(&sregs)
            .map_err(|e| cpu::HypervisorCpuError::SetSpecialRegs(e.into()))
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// Returns the floating point state (FPU) from the vCPU.
    ///
    fn get_fpu(&self) -> cpu::Result<FpuState> {
        Ok(self
            .fd
            .lock()
            .unwrap()
            .get_fpu()
            .map_err(|e| cpu::HypervisorCpuError::GetFloatingPointRegs(e.into()))?
            .into())
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// Set the floating point state (FPU) of a vCPU using the `KVM_SET_FPU` ioct.
    ///
    fn set_fpu(&self, fpu: &FpuState) -> cpu::Result<()> {
        let fpu: kvm_bindings::kvm_fpu = (*fpu).clone().into();
        self.fd
            .lock()
            .unwrap()
            .set_fpu(&fpu)
            .map_err(|e| cpu::HypervisorCpuError::SetFloatingPointRegs(e.into()))
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// X86 specific call to setup the CPUID registers.
    ///
    fn set_cpuid2(&self, cpuid: &[CpuIdEntry]) -> cpu::Result<()> {
        let cpuid: Vec<kvm_bindings::kvm_cpuid_entry2> =
            cpuid.iter().map(|e| (*e).into()).collect();
        let kvm_cpuid = <CpuId>::from_entries(&cpuid)
            .map_err(|_| cpu::HypervisorCpuError::SetCpuid(anyhow!("failed to create CpuId")))?;

        self.fd
            .lock()
            .unwrap()
            .set_cpuid2(&kvm_cpuid)
            .map_err(|e| cpu::HypervisorCpuError::SetCpuid(e.into()))
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// X86 specific call to enable HyperV SynIC
    ///
    fn enable_hyperv_synic(&self) -> cpu::Result<()> {
        // Update the information about Hyper-V SynIC being enabled and
        // emulated as it will influence later which MSRs should be saved.
        self.hyperv_synic.store(true, Ordering::Release);

        let cap = kvm_enable_cap {
            cap: KVM_CAP_HYPERV_SYNIC,
            ..Default::default()
        };
        self.fd
            .lock()
            .unwrap()
            .enable_cap(&cap)
            .map_err(|e| cpu::HypervisorCpuError::EnableHyperVSyncIc(e.into()))
    }
    ///
    /// X86 specific call to retrieve the CPUID registers.
    ///
    #[cfg(target_arch = "x86_64")]
    fn get_cpuid2(&self, num_entries: usize) -> cpu::Result<Vec<CpuIdEntry>> {
        let kvm_cpuid = self
            .fd
            .lock()
            .unwrap()
            .get_cpuid2(num_entries)
            .map_err(|e| cpu::HypervisorCpuError::GetCpuid(e.into()))?;

        let v = kvm_cpuid.as_slice().iter().map(|e| (*e).into()).collect();

        Ok(v)
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// Returns the state of the LAPIC (Local Advanced Programmable Interrupt Controller).
    ///
    fn get_lapic(&self) -> cpu::Result<LapicState> {
        Ok(self
            .fd
            .lock()
            .unwrap()
            .get_lapic()
            .map_err(|e| cpu::HypervisorCpuError::GetlapicState(e.into()))?
            .into())
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// Sets the state of the LAPIC (Local Advanced Programmable Interrupt Controller).
    ///
    fn set_lapic(&self, klapic: &LapicState) -> cpu::Result<()> {
        let klapic: kvm_bindings::kvm_lapic_state = (*klapic).clone().into();
        self.fd
            .lock()
            .unwrap()
            .set_lapic(&klapic)
            .map_err(|e| cpu::HypervisorCpuError::SetLapicState(e.into()))
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// Returns the model-specific registers (MSR) for this vCPU.
    ///
    fn get_msrs(&self, msrs: &mut Vec<MsrEntry>) -> cpu::Result<usize> {
        let kvm_msrs: Vec<kvm_msr_entry> = msrs.iter().map(|e| (*e).into()).collect();
        let mut kvm_msrs = MsrEntries::from_entries(&kvm_msrs).unwrap();
        let succ = self
            .fd
            .lock()
            .unwrap()
            .get_msrs(&mut kvm_msrs)
            .map_err(|e| cpu::HypervisorCpuError::GetMsrEntries(e.into()))?;

        msrs[..succ].copy_from_slice(
            &kvm_msrs.as_slice()[..succ]
                .iter()
                .map(|e| (*e).into())
                .collect::<Vec<MsrEntry>>(),
        );

        Ok(succ)
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// Setup the model-specific registers (MSR) for this vCPU.
    /// Returns the number of MSR entries actually written.
    ///
    fn set_msrs(&self, msrs: &[MsrEntry]) -> cpu::Result<usize> {
        let kvm_msrs: Vec<kvm_msr_entry> = msrs.iter().map(|e| (*e).into()).collect();
        let kvm_msrs = MsrEntries::from_entries(&kvm_msrs).unwrap();
        self.fd
            .lock()
            .unwrap()
            .set_msrs(&kvm_msrs)
            .map_err(|e| cpu::HypervisorCpuError::SetMsrEntries(e.into()))
    }
    ///
    /// Returns the vcpu's current "multiprocessing state".
    ///
    fn get_mp_state(&self) -> cpu::Result<MpState> {
        Ok(self
            .fd
            .lock()
            .unwrap()
            .get_mp_state()
            .map_err(|e| cpu::HypervisorCpuError::GetMpState(e.into()))?
            .into())
    }
    ///
    /// Sets the vcpu's current "multiprocessing state".
    ///
    fn set_mp_state(&self, mp_state: MpState) -> cpu::Result<()> {
        self.fd
            .lock()
            .unwrap()
            .set_mp_state(mp_state.into())
            .map_err(|e| cpu::HypervisorCpuError::SetMpState(e.into()))
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// Translates guest virtual address to guest physical address using the `KVM_TRANSLATE` ioctl.
    ///
    fn translate_gva(&self, gva: u64, _flags: u64) -> cpu::Result<(u64, u32)> {
        let tr = self
            .fd
            .lock()
            .unwrap()
            .translate_gva(gva)
            .map_err(|e| cpu::HypervisorCpuError::TranslateVirtualAddress(e.into()))?;
        // tr.valid is set if the GVA is mapped to valid GPA.
        match tr.valid {
            0 => Err(cpu::HypervisorCpuError::TranslateVirtualAddress(anyhow!(
                "Invalid GVA: {:#x}",
                gva
            ))),
            _ => Ok((tr.physical_address, 0)),
        }
    }
    ///
    /// Triggers the running of the current virtual CPU returning an exit reason.
    ///
    fn run(&self) -> std::result::Result<cpu::VmExit, cpu::HypervisorCpuError> {
        match self.fd.lock().unwrap().run() {
            Ok(run) => match run {
                #[cfg(target_arch = "x86_64")]
                VcpuExit::IoIn(addr, data) => {
                    if let Some(vm_ops) = &self.vm_ops {
                        return vm_ops
                            .pio_read(addr.into(), data)
                            .map(|_| cpu::VmExit::Ignore)
                            .map_err(|e| cpu::HypervisorCpuError::RunVcpu(e.into()));
                    }

                    Ok(cpu::VmExit::Ignore)
                }
                #[cfg(target_arch = "x86_64")]
                VcpuExit::IoOut(addr, data) => {
                    if let Some(vm_ops) = &self.vm_ops {
                        return vm_ops
                            .pio_write(addr.into(), data)
                            .map(|_| cpu::VmExit::Ignore)
                            .map_err(|e| cpu::HypervisorCpuError::RunVcpu(e.into()));
                    }

                    Ok(cpu::VmExit::Ignore)
                }
                #[cfg(target_arch = "x86_64")]
                VcpuExit::IoapicEoi(vector) => Ok(cpu::VmExit::IoapicEoi(vector)),
                #[cfg(target_arch = "x86_64")]
                VcpuExit::Shutdown | VcpuExit::Hlt => Ok(cpu::VmExit::Reset),

                #[cfg(target_arch = "aarch64")]
                VcpuExit::SystemEvent(event_type, flags) => {
                    use kvm_bindings::{KVM_SYSTEM_EVENT_RESET, KVM_SYSTEM_EVENT_SHUTDOWN};
                    // On Aarch64, when the VM is shutdown, run() returns
                    // VcpuExit::SystemEvent with reason KVM_SYSTEM_EVENT_SHUTDOWN
                    if event_type == KVM_SYSTEM_EVENT_RESET {
                        Ok(cpu::VmExit::Reset)
                    } else if event_type == KVM_SYSTEM_EVENT_SHUTDOWN {
                        Ok(cpu::VmExit::Shutdown)
                    } else {
                        Err(cpu::HypervisorCpuError::RunVcpu(anyhow!(
                            "Unexpected system event with type 0x{:x}, flags 0x{:x?}",
                            event_type,
                            flags
                        )))
                    }
                }

                VcpuExit::MmioRead(addr, data) => {
                    if let Some(vm_ops) = &self.vm_ops {
                        return vm_ops
                            .mmio_read(addr, data)
                            .map(|_| cpu::VmExit::Ignore)
                            .map_err(|e| cpu::HypervisorCpuError::RunVcpu(e.into()));
                    }

                    Ok(cpu::VmExit::Ignore)
                }
                VcpuExit::MmioWrite(addr, data) => {
                    if let Some(vm_ops) = &self.vm_ops {
                        return vm_ops
                            .mmio_write(addr, data)
                            .map(|_| cpu::VmExit::Ignore)
                            .map_err(|e| cpu::HypervisorCpuError::RunVcpu(e.into()));
                    }

                    Ok(cpu::VmExit::Ignore)
                }
                VcpuExit::Hyperv => Ok(cpu::VmExit::Hyperv),
                #[cfg(feature = "tdx")]
                VcpuExit::Unsupported(KVM_EXIT_TDX) => Ok(cpu::VmExit::Tdx),
                VcpuExit::Debug(_) => Ok(cpu::VmExit::Debug),

                r => Err(cpu::HypervisorCpuError::RunVcpu(anyhow!(
                    "Unexpected exit reason on vcpu run: {:?}",
                    r
                ))),
            },

            Err(ref e) => match e.errno() {
                libc::EAGAIN | libc::EINTR => Ok(cpu::VmExit::Ignore),
                _ => Err(cpu::HypervisorCpuError::RunVcpu(anyhow!(
                    "VCPU error {:?}",
                    e
                ))),
            },
        }
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// Let the guest know that it has been paused, which prevents from
    /// potential soft lockups when being resumed.
    ///
    fn notify_guest_clock_paused(&self) -> cpu::Result<()> {
        if let Err(e) = self.fd.lock().unwrap().kvmclock_ctrl() {
            // Linux kernel returns -EINVAL if the PV clock isn't yet initialised
            // which could be because we're still in firmware or the guest doesn't
            // use KVM clock.
            if e.errno() != libc::EINVAL {
                return Err(cpu::HypervisorCpuError::NotifyGuestClockPaused(e.into()));
            }
        }

        Ok(())
    }
    ///
    /// Sets debug registers to set hardware breakpoints and/or enable single step.
    ///
    fn set_guest_debug(
        &self,
        addrs: &[vm_memory::GuestAddress],
        singlestep: bool,
    ) -> cpu::Result<()> {
        let mut dbg = kvm_guest_debug {
            #[cfg(target_arch = "x86_64")]
            control: KVM_GUESTDBG_ENABLE | KVM_GUESTDBG_USE_HW_BP,
            #[cfg(target_arch = "aarch64")]
            control: KVM_GUESTDBG_ENABLE | KVM_GUESTDBG_USE_HW,
            ..Default::default()
        };
        if singlestep {
            dbg.control |= KVM_GUESTDBG_SINGLESTEP;
        }

        // Set the debug registers.
        // Here we assume that the number of addresses do not exceed what
        // `Hypervisor::get_guest_debug_hw_bps()` specifies.
        #[cfg(target_arch = "x86_64")]
        {
            // Set bits 9 and 10.
            // bit 9: GE (global exact breakpoint enable) flag.
            // bit 10: always 1.
            dbg.arch.debugreg[7] = 0x0600;

            for (i, addr) in addrs.iter().enumerate() {
                dbg.arch.debugreg[i] = addr.0;
                // Set global breakpoint enable flag
                dbg.arch.debugreg[7] |= 2 << (i * 2);
            }
        }
        #[cfg(target_arch = "aarch64")]
        {
            for (i, addr) in addrs.iter().enumerate() {
                // DBGBCR_EL1 (Debug Breakpoint Control Registers, D13.3.2):
                // bit 0: 1 (Enabled)
                // bit 1~2: 0b11 (PMC = EL1/EL0)
                // bit 5~8: 0b1111 (BAS = AArch64)
                // others: 0
                dbg.arch.dbg_bcr[i] = 0b1u64 | 0b110u64 | 0b1_1110_0000u64;
                // DBGBVR_EL1 (Debug Breakpoint Value Registers, D13.3.3):
                // bit 2~52: VA[2:52]
                dbg.arch.dbg_bvr[i] = (!0u64 >> 11) & addr.0;
            }
        }
        self.fd
            .lock()
            .unwrap()
            .set_guest_debug(&dbg)
            .map_err(|e| cpu::HypervisorCpuError::SetDebugRegs(e.into()))
    }
    #[cfg(target_arch = "aarch64")]
    fn vcpu_init(&self, kvi: &VcpuInit) -> cpu::Result<()> {
        self.fd
            .lock()
            .unwrap()
            .vcpu_init(kvi)
            .map_err(|e| cpu::HypervisorCpuError::VcpuInit(e.into()))
    }
    ///
    /// Gets a list of the guest registers that are supported for the
    /// KVM_GET_ONE_REG/KVM_SET_ONE_REG calls.
    ///
    #[cfg(target_arch = "aarch64")]
    fn get_reg_list(&self, reg_list: &mut RegList) -> cpu::Result<()> {
        self.fd
            .lock()
            .unwrap()
            .get_reg_list(reg_list)
            .map_err(|e| cpu::HypervisorCpuError::GetRegList(e.into()))
    }
    ///
    /// Gets the value of a system register
    ///
    #[cfg(target_arch = "aarch64")]
    fn get_sys_reg(&self, sys_reg: u32) -> cpu::Result<u64> {
        //
        // Arm Architecture Reference Manual defines the encoding of
        // AArch64 system registers, see
        // https://developer.arm.com/documentation/ddi0487 (chapter D12).
        // While KVM defines another ID for each AArch64 system register,
        // which is used in calling `KVM_G/SET_ONE_REG` to access a system
        // register of a guest.
        // A mapping exists between the Arm standard encoding and the KVM ID.
        // This function takes the standard u32 ID as input parameter, converts
        // it to the corresponding KVM ID, and call `KVM_GET_ONE_REG` API to
        // get the value of the system parameter.
        //
        let id: u64 = KVM_REG_ARM64 as u64
            | KVM_REG_SIZE_U64 as u64
            | KVM_REG_ARM64_SYSREG as u64
            | ((((sys_reg) >> 5)
                & (KVM_REG_ARM64_SYSREG_OP0_MASK
                    | KVM_REG_ARM64_SYSREG_OP1_MASK
                    | KVM_REG_ARM64_SYSREG_CRN_MASK
                    | KVM_REG_ARM64_SYSREG_CRM_MASK
                    | KVM_REG_ARM64_SYSREG_OP2_MASK)) as u64);
        let mut bytes = [0_u8; 8];
        self.fd
            .lock()
            .unwrap()
            .get_one_reg(id, &mut bytes)
            .map_err(|e| cpu::HypervisorCpuError::GetSysRegister(e.into()))?;
        Ok(u64::from_le_bytes(bytes))
    }
    ///
    /// Configure core registers for a given CPU.
    ///
    #[cfg(target_arch = "aarch64")]
    fn setup_regs(&self, cpu_id: u8, boot_ip: u64, fdt_start: u64) -> cpu::Result<()> {
        #[allow(non_upper_case_globals)]
        // PSR (Processor State Register) bits.
        // Taken from arch/arm64/include/uapi/asm/ptrace.h.
        const PSR_MODE_EL1h: u64 = 0x0000_0005;
        const PSR_F_BIT: u64 = 0x0000_0040;
        const PSR_I_BIT: u64 = 0x0000_0080;
        const PSR_A_BIT: u64 = 0x0000_0100;
        const PSR_D_BIT: u64 = 0x0000_0200;
        // Taken from arch/arm64/kvm/inject_fault.c.
        const PSTATE_FAULT_BITS_64: u64 =
            PSR_MODE_EL1h | PSR_A_BIT | PSR_F_BIT | PSR_I_BIT | PSR_D_BIT;

        let kreg_off = offset__of!(kvm_regs, regs);

        // Get the register index of the PSTATE (Processor State) register.
        let pstate = offset__of!(user_pt_regs, pstate) + kreg_off;
        self.fd
            .lock()
            .unwrap()
            .set_one_reg(
                arm64_core_reg_id!(KVM_REG_SIZE_U64, pstate),
                &PSTATE_FAULT_BITS_64.to_le_bytes(),
            )
            .map_err(|e| cpu::HypervisorCpuError::SetCoreRegister(e.into()))?;

        // Other vCPUs are powered off initially awaiting PSCI wakeup.
        if cpu_id == 0 {
            // Setting the PC (Processor Counter) to the current program address (kernel address).
            let pc = offset__of!(user_pt_regs, pc) + kreg_off;
            self.fd
                .lock()
                .unwrap()
                .set_one_reg(
                    arm64_core_reg_id!(KVM_REG_SIZE_U64, pc),
                    &boot_ip.to_le_bytes(),
                )
                .map_err(|e| cpu::HypervisorCpuError::SetCoreRegister(e.into()))?;

            // Last mandatory thing to set -> the address pointing to the FDT (also called DTB).
            // "The device tree blob (dtb) must be placed on an 8-byte boundary and must
            // not exceed 2 megabytes in size." -> https://www.kernel.org/doc/Documentation/arm64/booting.txt.
            // We are choosing to place it the end of DRAM. See `get_fdt_addr`.
            let regs0 = offset__of!(user_pt_regs, regs) + kreg_off;
            self.fd
                .lock()
                .unwrap()
                .set_one_reg(
                    arm64_core_reg_id!(KVM_REG_SIZE_U64, regs0),
                    &fdt_start.to_le_bytes(),
                )
                .map_err(|e| cpu::HypervisorCpuError::SetCoreRegister(e.into()))?;
        }
        Ok(())
    }

    #[cfg(target_arch = "x86_64")]
    ///
    /// Get the current CPU state
    ///
    /// Ordering requirements:
    ///
    /// KVM_GET_MP_STATE calls kvm_apic_accept_events(), which might modify
    /// vCPU/LAPIC state. As such, it must be done before most everything
    /// else, otherwise we cannot restore everything and expect it to work.
    ///
    /// KVM_GET_VCPU_EVENTS/KVM_SET_VCPU_EVENTS is unsafe if other vCPUs are
    /// still running.
    ///
    /// KVM_GET_LAPIC may change state of LAPIC before returning it.
    ///
    /// GET_VCPU_EVENTS should probably be last to save. The code looks as
    /// it might as well be affected by internal state modifications of the
    /// GET ioctls.
    ///
    /// SREGS saves/restores a pending interrupt, similar to what
    /// VCPU_EVENTS also does.
    ///
    /// GET_MSRS requires a pre-populated data structure to do something
    /// meaningful. For SET_MSRS it will then contain good data.
    ///
    /// # Example
    ///
    /// ```rust
    /// # extern crate hypervisor;
    /// # use hypervisor::KvmHypervisor;
    /// # use std::sync::Arc;
    /// let kvm = hypervisor::kvm::KvmHypervisor::new().unwrap();
    /// let hv: Arc<dyn hypervisor::Hypervisor> = Arc::new(kvm);
    /// let vm = hv.create_vm().expect("new VM fd creation failed");
    /// vm.enable_split_irq().unwrap();
    /// let vcpu = vm.create_vcpu(0, None).unwrap();
    /// let state = vcpu.state().unwrap();
    /// ```
    fn state(&self) -> cpu::Result<CpuState> {
        let cpuid = self.get_cpuid2(kvm_bindings::KVM_MAX_CPUID_ENTRIES)?;
        let mp_state = self.get_mp_state()?.into();
        let regs = self.get_regs()?;
        let sregs = self.get_sregs()?;
        let xsave = self.get_xsave()?;
        let xcrs = self.get_xcrs()?;
        let lapic_state = self.get_lapic()?;
        let fpu = self.get_fpu()?;

        // Try to get all MSRs based on the list previously retrieved from KVM.
        // If the number of MSRs obtained from GET_MSRS is different from the
        // expected amount, we fallback onto a slower method by getting MSRs
        // by chunks. This is the only way to make sure we try to get as many
        // MSRs as possible, even if some MSRs are not supported.
        let mut msr_entries = self.msrs.clone();

        // Save extra MSRs if the Hyper-V synthetic interrupt controller is
        // emulated.
        if self.hyperv_synic.load(Ordering::Acquire) {
            let hyperv_synic_msrs = vec![
                0x40000020, 0x40000021, 0x40000080, 0x40000081, 0x40000082, 0x40000083, 0x40000084,
                0x40000090, 0x40000091, 0x40000092, 0x40000093, 0x40000094, 0x40000095, 0x40000096,
                0x40000097, 0x40000098, 0x40000099, 0x4000009a, 0x4000009b, 0x4000009c, 0x4000009d,
                0x4000009e, 0x4000009f, 0x400000b0, 0x400000b1, 0x400000b2, 0x400000b3, 0x400000b4,
                0x400000b5, 0x400000b6, 0x400000b7,
            ];
            for index in hyperv_synic_msrs {
                let msr = kvm_msr_entry {
                    index,
                    ..Default::default()
                };
                msr_entries.push(msr.into());
            }
        }

        let expected_num_msrs = msr_entries.len();
        let num_msrs = self.get_msrs(&mut msr_entries)?;
        let msrs = if num_msrs != expected_num_msrs {
            let mut faulty_msr_index = num_msrs;
            let mut msr_entries_tmp = msr_entries[..faulty_msr_index].to_vec();

            loop {
                warn!(
                    "Detected faulty MSR 0x{:x} while getting MSRs",
                    msr_entries[faulty_msr_index].index
                );

                // Skip the first bad MSR
                let start_pos = faulty_msr_index + 1;

                let mut sub_msr_entries = msr_entries[start_pos..].to_vec();
                let num_msrs = self.get_msrs(&mut sub_msr_entries)?;

                msr_entries_tmp.extend(&sub_msr_entries[..num_msrs]);

                if num_msrs == sub_msr_entries.len() {
                    break;
                }

                faulty_msr_index = start_pos + num_msrs;
            }

            msr_entries_tmp
        } else {
            msr_entries
        };

        let vcpu_events = self.get_vcpu_events()?;

        Ok(VcpuKvmState {
            cpuid,
            msrs,
            vcpu_events,
            regs: regs.into(),
            sregs: sregs.into(),
            fpu,
            lapic_state,
            xsave: xsave.into(),
            xcrs,
            mp_state,
        }
        .into())
    }
    ///
    /// Get the current AArch64 CPU state
    ///
    #[cfg(target_arch = "aarch64")]
    fn state(&self) -> cpu::Result<CpuState> {
        let mut state = VcpuKvmState {
            mp_state: self.get_mp_state()?.into(),
            ..Default::default()
        };
        // Get core registers
        state.core_regs = self.get_regs()?;

        // Get systerm register
        // Call KVM_GET_REG_LIST to get all registers available to the guest.
        // For ArmV8 there are around 500 registers.
        let mut sys_regs: Vec<Register> = Vec::new();
        let mut reg_list = RegList::new(500).unwrap();
        self.fd
            .lock()
            .unwrap()
            .get_reg_list(&mut reg_list)
            .map_err(|e| cpu::HypervisorCpuError::GetRegList(e.into()))?;

        // At this point reg_list should contain: core registers and system
        // registers.
        // The register list contains the number of registers and their ids. We
        // will be needing to call KVM_GET_ONE_REG on each id in order to save
        // all of them. We carve out from the list  the core registers which are
        // represented in the kernel by kvm_regs structure and for which we can
        // calculate the id based on the offset in the structure.
        reg_list.retain(|regid| is_system_register(*regid));

        // Now, for the rest of the registers left in the previously fetched
        // register list, we are simply calling KVM_GET_ONE_REG.
        let indices = reg_list.as_slice();
        for index in indices.iter() {
            let mut bytes = [0_u8; 8];
            self.fd
                .lock()
                .unwrap()
                .get_one_reg(*index, &mut bytes)
                .map_err(|e| cpu::HypervisorCpuError::GetSysRegister(e.into()))?;
            sys_regs.push(kvm_bindings::kvm_one_reg {
                id: *index,
                addr: u64::from_le_bytes(bytes),
            });
        }

        state.sys_regs = sys_regs;

        Ok(state.into())
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// Restore the previously saved CPU state
    ///
    /// Ordering requirements:
    ///
    /// KVM_GET_VCPU_EVENTS/KVM_SET_VCPU_EVENTS is unsafe if other vCPUs are
    /// still running.
    ///
    /// Some SET ioctls (like set_mp_state) depend on kvm_vcpu_is_bsp(), so
    /// if we ever change the BSP, we have to do that before restoring anything.
    /// The same seems to be true for CPUID stuff.
    ///
    /// SREGS saves/restores a pending interrupt, similar to what
    /// VCPU_EVENTS also does.
    ///
    /// SET_REGS clears pending exceptions unconditionally, thus, it must be
    /// done before SET_VCPU_EVENTS, which restores it.
    ///
    /// SET_LAPIC must come after SET_SREGS, because the latter restores
    /// the apic base msr.
    ///
    /// SET_LAPIC must come before SET_MSRS, because the TSC deadline MSR
    /// only restores successfully, when the LAPIC is correctly configured.
    ///
    /// Arguments: CpuState
    /// # Example
    ///
    /// ```rust
    /// # extern crate hypervisor;
    /// # use hypervisor::KvmHypervisor;
    /// # use std::sync::Arc;
    /// let kvm = hypervisor::kvm::KvmHypervisor::new().unwrap();
    /// let hv: Arc<dyn hypervisor::Hypervisor> = Arc::new(kvm);
    /// let vm = hv.create_vm().expect("new VM fd creation failed");
    /// vm.enable_split_irq().unwrap();
    /// let vcpu = vm.create_vcpu(0, None).unwrap();
    /// let state = vcpu.state().unwrap();
    /// vcpu.set_state(&state).unwrap();
    /// ```
    fn set_state(&self, state: &CpuState) -> cpu::Result<()> {
        let state: VcpuKvmState = state.clone().into();
        self.set_cpuid2(&state.cpuid)?;
        self.set_mp_state(state.mp_state.into())?;
        self.set_regs(&state.regs.into())?;
        self.set_sregs(&state.sregs.into())?;
        self.set_xsave(&state.xsave.into())?;
        self.set_xcrs(&state.xcrs)?;
        self.set_lapic(&state.lapic_state)?;
        self.set_fpu(&state.fpu)?;

        // Try to set all MSRs previously stored.
        // If the number of MSRs set from SET_MSRS is different from the
        // expected amount, we fallback onto a slower method by setting MSRs
        // by chunks. This is the only way to make sure we try to set as many
        // MSRs as possible, even if some MSRs are not supported.
        let expected_num_msrs = state.msrs.len();
        let num_msrs = self.set_msrs(&state.msrs)?;
        if num_msrs != expected_num_msrs {
            let mut faulty_msr_index = num_msrs;

            loop {
                warn!(
                    "Detected faulty MSR 0x{:x} while setting MSRs",
                    state.msrs[faulty_msr_index].index
                );

                // Skip the first bad MSR
                let start_pos = faulty_msr_index + 1;

                let sub_msr_entries = state.msrs[start_pos..].to_vec();

                let num_msrs = self.set_msrs(&sub_msr_entries)?;

                if num_msrs == sub_msr_entries.len() {
                    break;
                }

                faulty_msr_index = start_pos + num_msrs;
            }
        }

        self.set_vcpu_events(&state.vcpu_events)?;

        Ok(())
    }
    ///
    /// Restore the previously saved AArch64 CPU state
    ///
    #[cfg(target_arch = "aarch64")]
    fn set_state(&self, state: &CpuState) -> cpu::Result<()> {
        let state: VcpuKvmState = state.clone().into();
        // Set core registers
        self.set_regs(&state.core_regs)?;
        // Set system registers
        for reg in &state.sys_regs {
            self.fd
                .lock()
                .unwrap()
                .set_one_reg(reg.id, &reg.addr.to_le_bytes())
                .map_err(|e| cpu::HypervisorCpuError::SetSysRegister(e.into()))?;
        }

        self.set_mp_state(state.mp_state.into())?;

        Ok(())
    }

    ///
    /// Initialize TDX for this CPU
    ///
    #[cfg(feature = "tdx")]
    fn tdx_init(&self, hob_address: u64) -> cpu::Result<()> {
        tdx_command(
            &self.fd.lock().unwrap().as_raw_fd(),
            TdxCommand::InitVcpu,
            0,
            hob_address,
        )
        .map_err(cpu::HypervisorCpuError::InitializeTdx)
    }

    ///
    /// Set the "immediate_exit" state
    ///
    fn set_immediate_exit(&self, exit: bool) {
        self.fd.lock().unwrap().set_kvm_immediate_exit(exit.into());
    }

    ///
    /// Returns the details about TDX exit reason
    ///
    #[cfg(feature = "tdx")]
    fn get_tdx_exit_details(&mut self) -> cpu::Result<TdxExitDetails> {
        let mut fd = self.fd.as_ref().lock().unwrap();
        let kvm_run = fd.get_kvm_run();
        // SAFETY: accessing a union field in a valid structure
        let tdx_vmcall = unsafe {
            &mut (*((&mut kvm_run.__bindgen_anon_1) as *mut kvm_run__bindgen_ty_1
                as *mut KvmTdxExit))
                .u
                .vmcall
        };

        tdx_vmcall.status_code = TDG_VP_VMCALL_INVALID_OPERAND;

        if tdx_vmcall.type_ != 0 {
            return Err(cpu::HypervisorCpuError::UnknownTdxVmCall);
        }

        match tdx_vmcall.subfunction {
            TDG_VP_VMCALL_GET_QUOTE => Ok(TdxExitDetails::GetQuote),
            TDG_VP_VMCALL_SETUP_EVENT_NOTIFY_INTERRUPT => {
                Ok(TdxExitDetails::SetupEventNotifyInterrupt)
            }
            _ => Err(cpu::HypervisorCpuError::UnknownTdxVmCall),
        }
    }

    ///
    /// Set the status code for TDX exit
    ///
    #[cfg(feature = "tdx")]
    fn set_tdx_status(&mut self, status: TdxExitStatus) {
        let mut fd = self.fd.as_ref().lock().unwrap();
        let kvm_run = fd.get_kvm_run();
        // SAFETY: accessing a union field in a valid structure
        let tdx_vmcall = unsafe {
            &mut (*((&mut kvm_run.__bindgen_anon_1) as *mut kvm_run__bindgen_ty_1
                as *mut KvmTdxExit))
                .u
                .vmcall
        };

        tdx_vmcall.status_code = match status {
            TdxExitStatus::Success => TDG_VP_VMCALL_SUCCESS,
            TdxExitStatus::InvalidOperand => TDG_VP_VMCALL_INVALID_OPERAND,
        };
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// Return the list of initial MSR entries for a VCPU
    ///
    fn boot_msr_entries(&self) -> Vec<MsrEntry> {
        use crate::arch::x86::{msr_index, MTRR_ENABLE, MTRR_MEM_TYPE_WB};

        [
            msr!(msr_index::MSR_IA32_SYSENTER_CS),
            msr!(msr_index::MSR_IA32_SYSENTER_ESP),
            msr!(msr_index::MSR_IA32_SYSENTER_EIP),
            msr!(msr_index::MSR_STAR),
            msr!(msr_index::MSR_CSTAR),
            msr!(msr_index::MSR_LSTAR),
            msr!(msr_index::MSR_KERNEL_GS_BASE),
            msr!(msr_index::MSR_SYSCALL_MASK),
            msr!(msr_index::MSR_IA32_TSC),
            msr_data!(
                msr_index::MSR_IA32_MISC_ENABLE,
                msr_index::MSR_IA32_MISC_ENABLE_FAST_STRING as u64
            ),
            msr_data!(msr_index::MSR_MTRRdefType, MTRR_ENABLE | MTRR_MEM_TYPE_WB),
        ]
        .to_vec()
    }
    #[cfg(target_arch = "aarch64")]
    fn has_pmu_support(&self) -> bool {
        let cpu_attr = kvm_bindings::kvm_device_attr {
            group: kvm_bindings::KVM_ARM_VCPU_PMU_V3_CTRL,
            attr: u64::from(kvm_bindings::KVM_ARM_VCPU_PMU_V3_INIT),
            addr: 0x0,
            flags: 0,
        };
        self.fd.lock().unwrap().has_device_attr(&cpu_attr).is_ok()
    }
    #[cfg(target_arch = "aarch64")]
    fn init_pmu(&self, irq: u32) -> cpu::Result<()> {
        let cpu_attr = kvm_bindings::kvm_device_attr {
            group: kvm_bindings::KVM_ARM_VCPU_PMU_V3_CTRL,
            attr: u64::from(kvm_bindings::KVM_ARM_VCPU_PMU_V3_INIT),
            addr: 0x0,
            flags: 0,
        };
        let cpu_attr_irq = kvm_bindings::kvm_device_attr {
            group: kvm_bindings::KVM_ARM_VCPU_PMU_V3_CTRL,
            attr: u64::from(kvm_bindings::KVM_ARM_VCPU_PMU_V3_IRQ),
            addr: &irq as *const u32 as u64,
            flags: 0,
        };
        self.fd
            .lock()
            .unwrap()
            .set_device_attr(&cpu_attr_irq)
            .map_err(|_| cpu::HypervisorCpuError::InitializePmu)?;
        self.fd
            .lock()
            .unwrap()
            .set_device_attr(&cpu_attr)
            .map_err(|_| cpu::HypervisorCpuError::InitializePmu)
    }
}

impl KvmVcpu {
    #[cfg(target_arch = "x86_64")]
    ///
    /// X86 specific call that returns the vcpu's current "xsave struct".
    ///
    fn get_xsave(&self) -> cpu::Result<XsaveState> {
        Ok(self
            .fd
            .lock()
            .unwrap()
            .get_xsave()
            .map_err(|e| cpu::HypervisorCpuError::GetXsaveState(e.into()))?
            .into())
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// X86 specific call that sets the vcpu's current "xsave struct".
    ///
    fn set_xsave(&self, xsave: &XsaveState) -> cpu::Result<()> {
        let xsave: kvm_bindings::kvm_xsave = (*xsave).clone().into();
        self.fd
            .lock()
            .unwrap()
            .set_xsave(&xsave)
            .map_err(|e| cpu::HypervisorCpuError::SetXsaveState(e.into()))
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// X86 specific call that returns the vcpu's current "xcrs".
    ///
    fn get_xcrs(&self) -> cpu::Result<ExtendedControlRegisters> {
        self.fd
            .lock()
            .unwrap()
            .get_xcrs()
            .map_err(|e| cpu::HypervisorCpuError::GetXcsr(e.into()))
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// X86 specific call that sets the vcpu's current "xcrs".
    ///
    fn set_xcrs(&self, xcrs: &ExtendedControlRegisters) -> cpu::Result<()> {
        self.fd
            .lock()
            .unwrap()
            .set_xcrs(xcrs)
            .map_err(|e| cpu::HypervisorCpuError::SetXcsr(e.into()))
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// Returns currently pending exceptions, interrupts, and NMIs as well as related
    /// states of the vcpu.
    ///
    fn get_vcpu_events(&self) -> cpu::Result<VcpuEvents> {
        self.fd
            .lock()
            .unwrap()
            .get_vcpu_events()
            .map_err(|e| cpu::HypervisorCpuError::GetVcpuEvents(e.into()))
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// Sets pending exceptions, interrupts, and NMIs as well as related states
    /// of the vcpu.
    ///
    fn set_vcpu_events(&self, events: &VcpuEvents) -> cpu::Result<()> {
        self.fd
            .lock()
            .unwrap()
            .set_vcpu_events(events)
            .map_err(|e| cpu::HypervisorCpuError::SetVcpuEvents(e.into()))
    }
}

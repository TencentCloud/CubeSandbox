// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.
//
// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//


use crate::device_tree::{DeviceNode, DeviceTree};
use crate::interrupt::LegacyUserspaceInterruptManager;
use crate::interrupt::MsiInterruptManager;
use crate::memory_manager::{Error as MemoryManagerError, MemoryManager, MEMORY_MANAGER_ACPI_SIZE};
use crate::pci_segment::PciSegment;
use crate::seccomp_filters::{get_seccomp_filter, Thread};
use crate::serial_manager::{Error as SerialManagerError, SerialManager};
use crate::sigwinch_listener::start_sigwinch_listener;
use crate::vm_config::{
    ConsoleOutputMode, DeviceConfig, DiskConfig, FsConfig, IvshmemConfig, NetConfig, PmemConfig,
    UserDeviceConfig, VdpaConfig, VhostMode, VmConfig, VsockConfig,
};
use crate::GuestRegionMmap;
use crate::PciDeviceInfo;
use crate::{device_node, DEVICE_MANAGER_SNAPSHOT_ID};
use acpi_tables::sdt::GenericAddress;
use acpi_tables::{aml, aml::Aml};
use anyhow::anyhow;
use arch::layout;
#[cfg(target_arch = "x86_64")]
use arch::layout::{APIC_START, IOAPIC_SIZE, IOAPIC_START};
use arch::NumaNodes;
#[cfg(target_arch = "aarch64")]
use arch::{DeviceType, MmioDeviceInfo};
use block_util::{
    async_io::DiskFile, block_io_uring_is_supported, detect_image_type,
    fixed_vhd_async::FixedVhdDiskAsync, fixed_vhd_sync::FixedVhdDiskSync, qcow_sync::QcowDiskSync,
    raw_async::RawFileDisk, raw_sync::RawFileDiskSync, vhdx_sync::VhdxDiskSync, ImageType,
};
#[cfg(target_arch = "aarch64")]
use devices::gic;
#[cfg(target_arch = "x86_64")]
use devices::ioapic;
#[cfg(target_arch = "aarch64")]
use devices::legacy::Pl011;
#[cfg(target_arch = "x86_64")]
use devices::legacy::Serial;
use devices::{
    interrupt_controller, interrupt_controller::InterruptController, AcpiNotificationFlags,
};
use hypervisor::{HypervisorType, IoEventAddress};
use libc::EFD_NONBLOCK;
use libc::{
    cfmakeraw, isatty, tcgetattr, tcsetattr, termios, MAP_NORESERVE, MAP_PRIVATE, MAP_SHARED,
    O_TMPFILE, PROT_READ, PROT_WRITE, TCSANOW,
};
#[cfg(target_arch = "x86_64")]
use pci::PciConfigIo;
use pci::{
    DeviceRelocation, PciBarRegionType, PciBdf, PciDevice, VfioPciDevice, VfioUserDmaMapping,
    VfioUserPciDevice, VfioUserPciDeviceError,
};
use seccompiler::SeccompAction;
use serde::{Deserialize, Serialize};
use std::collections::{BTreeSet, HashMap};
use std::convert::TryInto;
use std::fs::{read_link, File, OpenOptions};
use std::io::{self, stdout, Seek, SeekFrom};
use std::mem::zeroed;
use std::num::Wrapping;
use std::os::unix::fs::OpenOptionsExt;
use std::os::unix::io::{AsRawFd, FromRawFd, RawFd};
use std::path::PathBuf;
use std::result;
use std::sync::{Arc, Mutex};
use std::time::Instant;
use tokio::task::JoinHandle;
use tracer::trace_scoped;
use vfio_ioctls::{VfioContainer, VfioDevice, VfioDeviceFd};
use virtio_devices::fs::{BackendFsConfig, FsEvent};
use virtio_devices::transport::VirtioTransport;
use virtio_devices::transport::{VirtioPciDevice, VirtioPciDeviceActivator};
use virtio_devices::vhost_user::VhostUserConfig;
use virtio_devices::{
    AccessPlatformMapping, ActivateError, VdpaDmaMapping, VirtioMemMappingSource,
};
use virtio_devices::{Endpoint, IommuMapping};
use vm_allocator::{AddressAllocator, SystemAllocator};
use vm_device::dma_mapping::vfio::VfioDmaMapping;
use vm_device::dma_mapping::ExternalDmaMapping;
use vm_device::interrupt::{
    InterruptIndex, InterruptManager, LegacyIrqGroupConfig, MsiIrqGroupConfig,
};
use vm_device::{Bus, BusDevice, Resource};
use vm_memory::guest_memory::FileOffset;
use vm_memory::GuestMemoryRegion;
use vm_memory::{Address, GuestAddress, GuestUsize, MmapRegion};
#[cfg(target_arch = "x86_64")]
use vm_memory::{GuestAddressSpace, GuestMemory};
use vm_migration::{
    protocol::MemoryRangeTable, snapshot_from_id, state_from_id, Migratable, MigratableError,
    Pausable, Snapshot, SnapshotDataSection, Snapshottable, Transportable,
};
use vm_virtio::AccessPlatform;
use vm_virtio::VirtioDeviceType;
use vmm_sys_util::eventfd::EventFd;

#[cfg(target_arch = "aarch64")]
const MMIO_LEN: u64 = 0x1000;

// Singleton devices / devices the user cannot name
#[cfg(target_arch = "x86_64")]
const IOAPIC_DEVICE_NAME: &str = "__ioapic";
const SERIAL_DEVICE_NAME: &str = "__serial";
#[cfg(target_arch = "aarch64")]
const GPIO_DEVICE_NAME: &str = "__gpio";
const RNG_DEVICE_NAME: &str = "__rng";
const IOMMU_DEVICE_NAME: &str = "__iommu";
const BALLOON_DEVICE_NAME: &str = "__balloon";
const CONSOLE_DEVICE_NAME: &str = "__console";
const PVPANIC_DEVICE_NAME: &str = "__pvpanic";
const IVSHMEM_DEVICE_NAME: &str = "__ivshmem";
#[cfg(target_arch = "x86_64")]
const SYS_CTRL_DEVICE_NAME: &str = "__sys_ctrl";

// Devices that the user may name and for which we generate
// identifiers if the user doesn't give one
const DISK_DEVICE_NAME_PREFIX: &str = "_disk";
const FS_DEVICE_NAME_PREFIX: &str = "_fs";
const NET_DEVICE_NAME_PREFIX: &str = "_net";
const PMEM_DEVICE_NAME_PREFIX: &str = "_pmem";
const VDPA_DEVICE_NAME_PREFIX: &str = "_vdpa";
const VSOCK_DEVICE_NAME_PREFIX: &str = "_vsock";
const WATCHDOG_DEVICE_NAME: &str = "__watchdog";
const VFIO_DEVICE_NAME_PREFIX: &str = "_vfio";
const VFIO_USER_DEVICE_NAME_PREFIX: &str = "_vfio_user";
const VIRTIO_PCI_DEVICE_NAME_PREFIX: &str = "_virtio-pci";
const MAX_WORKER_THREADS: usize = 5;

/// Errors associated with device manager
#[derive(Debug)]
pub enum DeviceManagerError {
    /// Cannot create EventFd.
    EventFd(io::Error),

    /// Cannot open disk path
    Disk(io::Error),

    /// Cannot create vhost-user-net device
    CreateVhostUserNet(virtio_devices::vhost_user::Error),

    /// Cannot create virtio-blk device
    CreateVirtioBlock(io::Error),

    /// Cannot create virtio-net device
    CreateVirtioNet(virtio_devices::net::Error),

    /// Cannot create virtio-console device
    CreateVirtioConsole(io::Error),

    /// Cannot create virtio-rng device
    CreateVirtioRng(io::Error),

    /// Cannot create virtio-fs device
    CreateVirtioFs(virtio_devices::vhost_user::Error),

    /// Cannot create native virtio-fs device
    CreateNativeVirtioFs(io::Error),

    /// Virtio-fs device was created without a socket.
    NoVirtioFsSock,

    /// Cannot create vhost-user-blk device
    CreateVhostUserBlk(virtio_devices::vhost_user::Error),

    /// Cannot create virtio-pmem device
    CreateVirtioPmem(io::Error),

    /// Cannot create vDPA device
    CreateVdpa(virtio_devices::vdpa::Error),

    /// Cannot create virtio-vsock device
    CreateVirtioVsock(io::Error),

    /// Cannot create tpm device
    CreateTpmDevice(anyhow::Error),

    /// Failed to convert Path to &str for the vDPA device.
    CreateVdpaConvertPath,

    /// Failed to convert Path to &str for the virtio-vsock device.
    CreateVsockConvertPath,

    /// Cannot create virtio-vsock backend
    CreateVsockBackend(virtio_devices::vsock::VsockUnixError),

    /// Cannot create virtio-iommu device
    CreateVirtioIommu(io::Error),

    /// Cannot create virtio-balloon device
    CreateVirtioBalloon(io::Error),

    /// Cannot create virtio-watchdog device
    CreateVirtioWatchdog(io::Error),

    /// Failed to parse disk image format
    DetectImageType(io::Error),

    /// Cannot open qcow disk path
    QcowDeviceCreate(qcow::Error),

    /// Cannot create serial manager
    CreateSerialManager(SerialManagerError),

    /// Cannot spawn the serial manager thread
    SpawnSerialManager(SerialManagerError),

    /// Cannot open tap interface
    OpenTap(net_util::TapError),

    /// Cannot allocate IRQ.
    AllocateIrq,

    /// Cannot configure the IRQ.
    Irq(vmm_sys_util::errno::Error),

    /// Cannot allocate PCI BARs
    AllocateBars(pci::PciDeviceError),

    /// Could not free the BARs associated with a PCI device.
    FreePciBars(pci::PciDeviceError),

    /// Cannot register ioevent.
    RegisterIoevent(anyhow::Error),

    /// Cannot unregister ioevent.
    UnRegisterIoevent(anyhow::Error),

    /// Cannot create virtio device
    VirtioDevice(vmm_sys_util::errno::Error),

    /// Cannot add PCI device
    AddPciDevice(pci::PciRootError),

    /// Cannot open persistent memory file
    PmemFileOpen(io::Error),

    /// Cannot set persistent memory file size
    PmemFileSetLen(io::Error),

    /// Cannot find a memory range for persistent memory
    PmemRangeAllocation,

    /// Cannot find a memory range for virtio-fs
    FsRangeAllocation,

    /// Error creating serial output file
    SerialOutputFileOpen(io::Error),

    /// Error creating console output file
    ConsoleOutputFileOpen(io::Error),

    /// Error creating serial pty
    SerialPtyOpen(io::Error),

    /// Error creating console pty
    ConsolePtyOpen(io::Error),

    /// Error setting pty raw mode
    SetPtyRaw(vmm_sys_util::errno::Error),

    /// Error getting pty peer
    GetPtyPeer(vmm_sys_util::errno::Error),

    /// Cannot create a VFIO device
    VfioCreate(vfio_ioctls::VfioError),

    /// Cannot create a VFIO PCI device
    VfioPciCreate(pci::VfioPciError),

    /// Failed to map VFIO MMIO region.
    VfioMapRegion(pci::VfioPciError),

    /// Failed to DMA map VFIO device.
    VfioDmaMap(vfio_ioctls::VfioError),

    /// Failed to DMA unmap VFIO device.
    VfioDmaUnmap(pci::VfioPciError),

    /// Failed to create the passthrough device.
    CreatePassthroughDevice(anyhow::Error),

    /// Failed to memory map.
    Mmap(io::Error),

    /// Cannot add legacy device to Bus.
    BusError(vm_device::BusError),

    /// Failed to allocate IO port
    AllocateIoPort,

    /// Failed to allocate MMIO address
    AllocateMmioAddress,

    /// Failed to make hotplug notification
    HotPlugNotification(io::Error),

    /// Error from a memory manager operation
    MemoryManager(MemoryManagerError),

    /// Failed to create new interrupt source group.
    CreateInterruptGroup(io::Error),

    /// Failed to update interrupt source group.
    UpdateInterruptGroup(io::Error),

    /// Failed to create interrupt controller.
    CreateInterruptController(interrupt_controller::Error),

    /// Failed to create a new MmapRegion instance.
    NewMmapRegion(vm_memory::mmap::MmapRegionError),

    /// Failed to clone a File.
    CloneFile(io::Error),

    /// Failed to create socket file
    CreateSocketFile(io::Error),

    /// Failed to spawn the network backend
    SpawnNetBackend(io::Error),

    /// Failed to spawn the block backend
    SpawnBlockBackend(io::Error),

    /// Missing PCI bus.
    NoPciBus,

    /// Could not find an available device name.
    NoAvailableDeviceName,

    /// Missing PCI device.
    MissingPciDevice,

    /// Failed to remove a PCI device from the PCI bus.
    RemoveDeviceFromPciBus(pci::PciRootError),

    /// Failed to remove a bus device from the IO bus.
    RemoveDeviceFromIoBus(vm_device::BusError),

    /// Failed to remove a bus device from the MMIO bus.
    RemoveDeviceFromMmioBus(vm_device::BusError),

    /// Failed to find the device corresponding to a specific PCI b/d/f.
    UnknownPciBdf(u32),

    /// Not allowed to remove this type of device from the VM.
    RemovalNotAllowed(vm_virtio::VirtioDeviceType),

    /// Failed to find device corresponding to the given identifier.
    UnknownDeviceId(String),

    /// Failed to find an available PCI device ID.
    NextPciDeviceId(pci::PciRootError),

    /// Could not reserve the PCI device ID.
    GetPciDeviceId(pci::PciRootError),

    /// Could not give the PCI device ID back.
    PutPciDeviceId(pci::PciRootError),

    /// No disk path was specified when one was expected
    NoDiskPath,

    /// Failed to update guest memory for virtio device.
    UpdateMemoryForVirtioDevice(virtio_devices::Error),

    /// Cannot create virtio-mem device
    CreateVirtioMem(io::Error),

    /// Cannot find a memory range for virtio-mem memory
    VirtioMemRangeAllocation,

    /// Failed to update guest memory for VFIO PCI device.
    UpdateMemoryForVfioPciDevice(vfio_ioctls::VfioError),

    /// Trying to use a directory for pmem but no size specified
    PmemWithDirectorySizeMissing,

    /// Trying to use a size that is not multiple of 2MiB
    PmemSizeNotAligned,

    /// Could not find the node in the device tree.
    MissingNode,

    /// Resource was already found.
    ResourceAlreadyExists,

    /// Expected resources for virtio-pmem could not be found.
    MissingVirtioPmemResources,

    /// Missing PCI b/d/f from the DeviceNode.
    MissingDeviceNodePciBdf,

    /// No support for device passthrough
    NoDevicePassthroughSupport,

    /// Failed to resize virtio-balloon
    VirtioBalloonResize(virtio_devices::balloon::Error),

    /// Missing virtio-balloon, can't proceed as expected.
    MissingVirtioBalloon,

    /// Missing virtual IOMMU device
    MissingVirtualIommu,

    /// Failed to do power button notification
    PowerButtonNotification(io::Error),

    /// Failed to do AArch64 GPIO power button notification
    #[cfg(target_arch = "aarch64")]
    AArch64PowerButtonNotification(devices::legacy::GpioDeviceError),

    /// Failed to set O_DIRECT flag to file descriptor
    SetDirectIo,

    /// Failed to create FixedVhdDiskAsync
    CreateFixedVhdDiskAsync(io::Error),

    /// Failed to create FixedVhdDiskSync
    CreateFixedVhdDiskSync(io::Error),

    /// Failed to create QcowDiskSync
    CreateQcowDiskSync(qcow::Error),

    /// Failed to create FixedVhdxDiskSync
    CreateFixedVhdxDiskSync(vhdx::vhdx::VhdxError),

    /// Failed to add DMA mapping handler to virtio-mem device.
    AddDmaMappingHandlerVirtioMem(virtio_devices::mem::Error),

    /// Failed to remove DMA mapping handler from virtio-mem device.
    RemoveDmaMappingHandlerVirtioMem(virtio_devices::mem::Error),

    /// Failed to create vfio-user client
    VfioUserCreateClient(vfio_user::Error),

    /// Failed to create VFIO user device
    VfioUserCreate(VfioUserPciDeviceError),

    /// Failed to map region from VFIO user device into guest
    VfioUserMapRegion(VfioUserPciDeviceError),

    /// Failed to DMA map VFIO user device.
    VfioUserDmaMap(VfioUserPciDeviceError),

    /// Failed to DMA unmap VFIO user device.
    VfioUserDmaUnmap(VfioUserPciDeviceError),

    /// Failed to update memory mappings for VFIO user device
    UpdateMemoryForVfioUserPciDevice(VfioUserPciDeviceError),

    /// Cannot duplicate file descriptor
    DupFd(vmm_sys_util::errno::Error),

    /// Failed to DMA map virtio device.
    VirtioDmaMap(std::io::Error),

    /// Failed to DMA unmap virtio device.
    VirtioDmaUnmap(std::io::Error),

    /// Cannot hotplug device behind vIOMMU
    InvalidIommuHotplug,

    /// Invalid identifier as it is not unique.
    IdentifierNotUnique(String),

    /// Invalid identifier
    InvalidIdentifier(String),

    /// Error activating virtio device
    VirtioActivate(ActivateError),

    /// Failed retrieving device state from snapshot
    RestoreGetState(MigratableError),

    /// Cannot create a PvPanic device
    PvPanicCreate(devices::pvpanic::PvPanicError),
    /// Channel recv error.
    VirtioFsServerRecv(std::sync::mpsc::RecvError),

    /// Virtio-Fs server update filter failed.
    VirtioFsServerUpdate(String),
}
pub type DeviceManagerResult<T> = result::Result<T, DeviceManagerError>;

const DEVICE_MANAGER_ACPI_SIZE: usize = 0x10;

const TIOCSPTLCK: libc::c_int = 0x4004_5431;
const TIOCGTPEER: libc::c_int = 0x5441;

// const IVSHMEM_START_ADDR: u64 = 0x8000_0000;

pub fn create_pty() -> io::Result<(File, File, PathBuf)> {
    // Try to use /dev/pts/ptmx first then fall back to /dev/ptmx
    // This is done to try and use the devpts filesystem that
    // could be available for use in the process's namespace first.
    // Ideally these are all the same file though but different
    // kernels could have things setup differently.
    // See https://www.kernel.org/doc/Documentation/filesystems/devpts.txt
    // for further details.

    let custom_flags = libc::O_NONBLOCK;
    let main = match OpenOptions::new()
        .read(true)
        .write(true)
        .custom_flags(custom_flags)
        .open("/dev/pts/ptmx")
    {
        Ok(f) => f,
        _ => OpenOptions::new()
            .read(true)
            .write(true)
            .custom_flags(custom_flags)
            .open("/dev/ptmx")?,
    };
    let mut unlock: libc::c_ulong = 0;
    // SAFETY: FFI call into libc, trivially safe
    unsafe {
        libc::ioctl(
            main.as_raw_fd(),
            TIOCSPTLCK.try_into().unwrap(),
            &mut unlock,
        )
    };

    // SAFETY: FFI call into libc, trivially safe
    let sub_fd = unsafe {
        libc::ioctl(
            main.as_raw_fd(),
            TIOCGTPEER.try_into().unwrap(),
            libc::O_NOCTTY | libc::O_RDWR,
        )
    };
    if sub_fd == -1 {
        return vmm_sys_util::errno::errno_result().map_err(|e| e.into());
    }

    let proc_path = PathBuf::from(format!("/proc/self/fd/{}", sub_fd));
    let path = read_link(proc_path)?;

    // SAFETY: sub_fd is checked to be valid before being wrapped in File
    Ok((main, unsafe { File::from_raw_fd(sub_fd) }, path))
}

#[derive(Default)]
pub struct Console {
    console_resizer: Option<Arc<virtio_devices::ConsoleResizer>>,
}

impl Console {
    pub fn need_resize(&self) -> bool {
        if let Some(_resizer) = self.console_resizer.as_ref() {
            return true;
        }

        false
    }

    pub fn update_console_size(&self) {
        if let Some(resizer) = self.console_resizer.as_ref() {
            resizer.update_console_size()
        }
    }
}

pub(crate) struct AddressManager {
    pub(crate) allocator: Arc<Mutex<SystemAllocator>>,
    #[cfg(target_arch = "x86_64")]
    pub(crate) io_bus: Arc<Bus>,
    pub(crate) mmio_bus: Arc<Bus>,
    vm: Arc<dyn hypervisor::Vm>,
    device_tree: Arc<Mutex<DeviceTree>>,
    pci_mmio_allocators: Vec<Arc<Mutex<AddressAllocator>>>,
}

impl DeviceRelocation for AddressManager {
    fn move_bar(
        &self,
        old_base: u64,
        new_base: u64,
        len: u64,
        pci_dev: &mut dyn PciDevice,
        region_type: PciBarRegionType,
    ) -> std::result::Result<(), std::io::Error> {
        match region_type {
            PciBarRegionType::IoRegion => {
                #[cfg(target_arch = "x86_64")]
                {
                    // Update system allocator
                    self.allocator
                        .lock()
                        .unwrap()
                        .free_io_addresses(GuestAddress(old_base), len as GuestUsize);

                    self.allocator
                        .lock()
                        .unwrap()
                        .allocate_io_addresses(
                            Some(GuestAddress(new_base)),
                            len as GuestUsize,
                            None,
                        )
                        .ok_or_else(|| {
                            io::Error::new(io::ErrorKind::Other, "failed allocating new IO range")
                        })?;

                    // Update PIO bus
                    self.io_bus
                        .update_range(old_base, len, new_base, len)
                        .map_err(|e| io::Error::new(io::ErrorKind::Other, e))?;
                }
                #[cfg(target_arch = "aarch64")]
                error!("I/O region is not supported");
            }
            PciBarRegionType::Memory32BitRegion | PciBarRegionType::Memory64BitRegion => {
                // Update system allocator
                if region_type == PciBarRegionType::Memory32BitRegion {
                    self.allocator
                        .lock()
                        .unwrap()
                        .free_mmio_hole_addresses(GuestAddress(old_base), len as GuestUsize);

                    self.allocator
                        .lock()
                        .unwrap()
                        .allocate_mmio_hole_addresses(
                            Some(GuestAddress(new_base)),
                            len as GuestUsize,
                            Some(len),
                        )
                        .ok_or_else(|| {
                            io::Error::new(
                                io::ErrorKind::Other,
                                "failed allocating new 32 bits MMIO range",
                            )
                        })?;
                } else {
                    // Find the specific allocator that this BAR was allocated from and use it for new one
                    for allocator in &self.pci_mmio_allocators {
                        let allocator_base = allocator.lock().unwrap().base();
                        let allocator_end = allocator.lock().unwrap().end();

                        if old_base >= allocator_base.0 && old_base <= allocator_end.0 {
                            allocator
                                .lock()
                                .unwrap()
                                .free(GuestAddress(old_base), len as GuestUsize);

                            allocator
                                .lock()
                                .unwrap()
                                .allocate(
                                    Some(GuestAddress(new_base)),
                                    len as GuestUsize,
                                    Some(len),
                                )
                                .ok_or_else(|| {
                                    io::Error::new(
                                        io::ErrorKind::Other,
                                        "failed allocating new 64 bits MMIO range",
                                    )
                                })?;

                            break;
                        }
                    }
                }

                // Update MMIO bus
                self.mmio_bus
                    .update_range(old_base, len, new_base, len)
                    .map_err(|e| io::Error::new(io::ErrorKind::Other, e))?;
            }
        }

        // Update the device_tree resources associated with the device
        if let Some(id) = pci_dev.id() {
            if let Some(node) = self.device_tree.lock().unwrap().get_mut(&id) {
                let mut resource_updated = false;
                for resource in node.resources.iter_mut() {
                    if let Resource::PciBar { base, type_, .. } = resource {
                        if PciBarRegionType::from(*type_) == region_type && *base == old_base {
                            *base = new_base;
                            resource_updated = true;
                            break;
                        }
                    }
                }

                if !resource_updated {
                    return Err(io::Error::new(
                        io::ErrorKind::Other,
                        format!(
                            "Couldn't find a resource with base 0x{:x} for device {}",
                            old_base, id
                        ),
                    ));
                }
            } else {
                return Err(io::Error::new(
                    io::ErrorKind::Other,
                    format!("Couldn't find device {} from device tree", id),
                ));
            }
        }

        let any_dev = pci_dev.as_any();
        if let Some(virtio_pci_dev) = any_dev.downcast_ref::<VirtioPciDevice>() {
            let bar_addr = virtio_pci_dev.config_bar_addr();
            if bar_addr == new_base {
                for (event, addr) in virtio_pci_dev.ioeventfds(old_base) {
                    let io_addr = IoEventAddress::Mmio(addr);
                    self.vm.unregister_ioevent(event, &io_addr).map_err(|e| {
                        io::Error::new(
                            io::ErrorKind::Other,
                            format!("failed to unregister ioevent: {:?}", e),
                        )
                    })?;
                }
                for (event, addr) in virtio_pci_dev.ioeventfds(new_base) {
                    let io_addr = IoEventAddress::Mmio(addr);
                    self.vm
                        .register_ioevent(event, &io_addr, None)
                        .map_err(|e| {
                            io::Error::new(
                                io::ErrorKind::Other,
                                format!("failed to register ioevent: {:?}", e),
                            )
                        })?;
                }
            } else {
                let virtio_dev = virtio_pci_dev.virtio_device();
                let mut virtio_dev = virtio_dev.lock().unwrap();
                if let Some(mut shm_regions) = virtio_dev.get_shm_regions() {
                    if shm_regions.addr.raw_value() == old_base {
                        let mem_region = self.vm.make_user_memory_region(
                            shm_regions.mem_slot,
                            old_base,
                            shm_regions.len,
                            shm_regions.host_addr,
                            false,
                            false,
                        );

                        self.vm.remove_user_memory_region(mem_region).map_err(|e| {
                            io::Error::new(
                                io::ErrorKind::Other,
                                format!("failed to remove user memory region: {:?}", e),
                            )
                        })?;

                        // Create new mapping by inserting new region to KVM.
                        let mem_region = self.vm.make_user_memory_region(
                            shm_regions.mem_slot,
                            new_base,
                            shm_regions.len,
                            shm_regions.host_addr,
                            false,
                            false,
                        );

                        self.vm.create_user_memory_region(mem_region).map_err(|e| {
                            io::Error::new(
                                io::ErrorKind::Other,
                                format!("failed to create user memory regions: {:?}", e),
                            )
                        })?;

                        // Update shared memory regions to reflect the new mapping.
                        shm_regions.addr = GuestAddress(new_base);
                        virtio_dev.set_shm_regions(shm_regions).map_err(|e| {
                            io::Error::new(
                                io::ErrorKind::Other,
                                format!("failed to update shared memory regions: {:?}", e),
                            )
                        })?;
                    }
                }
            }
        }

        pci_dev.move_bar(old_base, new_base)
    }
}

#[derive(Serialize, Deserialize)]
struct DeviceManagerState {
    device_tree: DeviceTree,
    device_id_cnt: Wrapping<usize>,
}

#[derive(Debug)]
pub struct PtyPair {
    pub main: File,
    pub path: PathBuf,
}

impl Clone for PtyPair {
    fn clone(&self) -> Self {
        PtyPair {
            main: self.main.try_clone().unwrap(),
            path: self.path.clone(),
        }
    }
}

#[derive(Clone)]
pub enum PciDeviceHandle {
    Vfio(Arc<Mutex<VfioPciDevice>>),
    Virtio(Arc<Mutex<VirtioPciDevice>>),
    VfioUser(Arc<Mutex<VfioUserPciDevice>>),
}

#[derive(Clone)]
struct MetaVirtioDevice {
    virtio_device: Arc<Mutex<dyn virtio_devices::VirtioDevice>>,
    iommu: bool,
    id: String,
    pci_segment: u16,
    dma_handler: Option<Arc<dyn ExternalDmaMapping>>,
}

#[derive(Default)]
pub struct AcpiPlatformAddresses {
    pub pm_timer_address: Option<GenericAddress>,
    pub reset_reg_address: Option<GenericAddress>,
    pub sleep_control_reg_address: Option<GenericAddress>,
    pub sleep_status_reg_address: Option<GenericAddress>,
}

pub struct DeviceManager {
    // The underlying hypervisor
    hypervisor_type: HypervisorType,

    // Manage address space related to devices
    address_manager: Arc<AddressManager>,

    // Console abstraction
    console: Arc<Console>,

    // console PTY
    console_pty: Option<Arc<Mutex<PtyPair>>>,

    // serial PTY
    serial_pty: Option<Arc<Mutex<PtyPair>>>,

    // Serial Manager
    serial_manager: Option<Arc<SerialManager>>,

    // pty foreground status,
    console_resize_pipe: Option<Arc<File>>,

    // Interrupt controller
    #[cfg(target_arch = "x86_64")]
    interrupt_controller: Option<Arc<Mutex<ioapic::Ioapic>>>,
    #[cfg(target_arch = "aarch64")]
    interrupt_controller: Option<Arc<Mutex<gic::Gic>>>,

    // Things to be added to the commandline (e.g. aarch64 early console)
    #[cfg(target_arch = "aarch64")]
    cmdline_additions: Vec<String>,

    // ACPI GED notification device
    ged_notification_device: Option<Arc<Mutex<devices::AcpiGedDevice>>>,

    // VM configuration
    config: Arc<Mutex<VmConfig>>,

    // Memory Manager
    memory_manager: Arc<Mutex<MemoryManager>>,

    // The virtio devices on the system
    virtio_devices: Vec<MetaVirtioDevice>,

    // List of bus devices
    // Let the DeviceManager keep strong references to the BusDevice devices.
    // This allows the IO and MMIO buses to be provided with Weak references,
    // which prevents cyclic dependencies.
    bus_devices: Vec<Arc<Mutex<dyn BusDevice>>>,

    // Counter to keep track of the consumed device IDs.
    device_id_cnt: Wrapping<usize>,

    pci_segments: Vec<PciSegment>,

    #[cfg_attr(target_arch = "aarch64", allow(dead_code))]
    // MSI Interrupt Manager
    msi_interrupt_manager: Arc<dyn InterruptManager<GroupConfig = MsiIrqGroupConfig>>,

    #[cfg_attr(feature = "mshv", allow(dead_code))]
    // Legacy Interrupt Manager
    legacy_interrupt_manager: Option<Arc<dyn InterruptManager<GroupConfig = LegacyIrqGroupConfig>>>,

    // Passthrough device handle
    passthrough_device: Option<VfioDeviceFd>,

    // VFIO container
    // Only one container can be created, therefore it is stored as part of the
    // DeviceManager to be reused.
    vfio_container: Option<Arc<VfioContainer>>,

    // Paravirtualized IOMMU
    iommu_device: Option<Arc<Mutex<virtio_devices::Iommu>>>,
    iommu_mapping: Option<Arc<IommuMapping>>,

    // PCI information about devices attached to the paravirtualized IOMMU
    // It contains the virtual IOMMU PCI BDF along with the list of PCI BDF
    // representing the devices attached to the virtual IOMMU. This is useful
    // information for filling the ACPI VIOT table.
    iommu_attached_devices: Option<(PciBdf, Vec<PciBdf>)>,

    // Tree of devices, representing the dependencies between devices.
    // Useful for introspection, snapshot and restore.
    device_tree: Arc<Mutex<DeviceTree>>,

    // Exit event
    exit_evt: EventFd,
    reset_evt: EventFd,

    #[cfg(target_arch = "aarch64")]
    id_to_dev_info: HashMap<(DeviceType, String), MmioDeviceInfo>,

    // seccomp action
    seccomp_action: SeccompAction,

    // List of guest NUMA nodes.
    numa_nodes: NumaNodes,

    // Possible handle to the virtio-balloon device
    balloon: Option<Arc<Mutex<virtio_devices::Balloon>>>,

    // Virtio Device activation EventFd to allow the VMM thread to trigger device
    // activation and thus start the threads from the VMM thread
    activate_evt: EventFd,

    acpi_address: GuestAddress,

    selected_segment: usize,

    // Possible handle to the virtio-mem device
    virtio_mem_devices: Vec<Arc<Mutex<virtio_devices::Mem>>>,

    #[cfg(target_arch = "aarch64")]
    // GPIO device for AArch64
    gpio_device: Option<Arc<Mutex<devices::legacy::Gpio>>>,

    // pvpanic device
    pvpanic_device: Option<Arc<Mutex<devices::PvPanicDevice>>>,

    // Flag to force setting the iommu on virtio devices
    force_iommu: bool,

    // Helps identify if the VM is currently being restored
    restoring: bool,

    // io_uring availability if detected
    io_uring_supported: Option<bool>,

    // List of unique identifiers provided at boot through the configuration.
    boot_id_list: BTreeSet<String>,

    // Start time of the VM
    timestamp: Instant,

    // Pending activations
    pending_activations: Arc<Mutex<Vec<VirtioPciDeviceActivator>>>,

    // Addresses for ACPI platform devices e.g. ACPI PM timer, sleep/reset registers
    acpi_platform_addresses: AcpiPlatformAddresses,

    snapshot: Option<Snapshot>,

    sandbox_id: String,

    #[cfg(target_arch = "x86_64")]
    sys_ctrl: Option<Arc<Mutex<devices::legacy::SysCtrl>>>,

    // ivshmem device
    ivshmem_device: Option<Arc<Mutex<devices::IvshmemDevice>>>,
}

impl DeviceManager {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        hypervisor_type: HypervisorType,
        vm: Arc<dyn hypervisor::Vm>,
        config: Arc<Mutex<VmConfig>>,
        memory_manager: Arc<Mutex<MemoryManager>>,
        exit_evt: &EventFd,
        reset_evt: &EventFd,
        seccomp_action: SeccompAction,
        numa_nodes: NumaNodes,
        activate_evt: &EventFd,
        force_iommu: bool,
        restoring: bool,
        boot_id_list: BTreeSet<String>,
        timestamp: Instant,
        snapshot: Option<Snapshot>,
        sandbox_id: String,
    ) -> DeviceManagerResult<Arc<Mutex<Self>>> {
        trace_scoped!("DeviceManager::new");

        let device_tree = Arc::new(Mutex::new(DeviceTree::new()));

        let num_pci_segments =
            if let Some(platform_config) = config.lock().unwrap().platform.as_ref() {
                platform_config.num_pci_segments
            } else {
                1
            };

        let start_of_device_area = memory_manager.lock().unwrap().start_of_device_area().0;
        let end_of_device_area = memory_manager.lock().unwrap().end_of_device_area().0;

        // Start each PCI segment range on a 4GiB boundary
        let pci_segment_size = (end_of_device_area - start_of_device_area + 1)
            / ((4 << 30) * num_pci_segments as u64)
            * (4 << 30);

        let mut pci_mmio_allocators = vec![];
        for i in 0..num_pci_segments as u64 {
            let mmio_start = start_of_device_area + i * pci_segment_size;
            let allocator = Arc::new(Mutex::new(
                AddressAllocator::new(GuestAddress(mmio_start), pci_segment_size).unwrap(),
            ));
            pci_mmio_allocators.push(allocator)
        }

        let address_manager = Arc::new(AddressManager {
            allocator: memory_manager.lock().unwrap().allocator(),
            #[cfg(target_arch = "x86_64")]
            io_bus: Arc::new(Bus::new()),
            mmio_bus: Arc::new(Bus::new()),
            vm: vm.clone(),
            device_tree: Arc::clone(&device_tree),
            pci_mmio_allocators,
        });

        // First we create the MSI interrupt manager, the legacy one is created
        // later, after the IOAPIC device creation.
        // The reason we create the MSI one first is because the IOAPIC needs it,
        // and then the legacy interrupt manager needs an IOAPIC. So we're
        // handling a linear dependency chain:
        // msi_interrupt_manager <- IOAPIC <- legacy_interrupt_manager.
        let msi_interrupt_manager: Arc<dyn InterruptManager<GroupConfig = MsiIrqGroupConfig>> =
            Arc::new(MsiInterruptManager::new(
                Arc::clone(&address_manager.allocator),
                vm,
            ));

        let acpi_address = address_manager
            .allocator
            .lock()
            .unwrap()
            .allocate_platform_mmio_addresses(None, DEVICE_MANAGER_ACPI_SIZE as u64, None)
            .ok_or(DeviceManagerError::AllocateIoPort)?;

        let mut pci_irq_slots = [0; 32];
        PciSegment::reserve_legacy_interrupts_for_pci_devices(
            &address_manager,
            &mut pci_irq_slots,
        )?;

        let mut pci_segments = vec![PciSegment::new_default_segment(
            &address_manager,
            Arc::clone(&address_manager.pci_mmio_allocators[0]),
            &pci_irq_slots,
        )?];

        for i in 1..num_pci_segments as usize {
            pci_segments.push(PciSegment::new(
                i as u16,
                &address_manager,
                Arc::clone(&address_manager.pci_mmio_allocators[i]),
                &pci_irq_slots,
            )?);
        }

        let device_manager = DeviceManager {
            hypervisor_type,
            address_manager: Arc::clone(&address_manager),
            console: Arc::new(Console::default()),
            interrupt_controller: None,
            #[cfg(target_arch = "aarch64")]
            cmdline_additions: Vec::new(),
            ged_notification_device: None,
            config,
            memory_manager,
            virtio_devices: Vec::new(),
            bus_devices: Vec::new(),
            device_id_cnt: Wrapping(0),
            msi_interrupt_manager,
            legacy_interrupt_manager: None,
            passthrough_device: None,
            vfio_container: None,
            iommu_device: None,
            iommu_mapping: None,
            iommu_attached_devices: None,
            pci_segments,
            device_tree,
            exit_evt: exit_evt.try_clone().map_err(DeviceManagerError::EventFd)?,
            reset_evt: reset_evt.try_clone().map_err(DeviceManagerError::EventFd)?,
            #[cfg(target_arch = "aarch64")]
            id_to_dev_info: HashMap::new(),
            seccomp_action,
            numa_nodes,
            balloon: None,
            activate_evt: activate_evt
                .try_clone()
                .map_err(DeviceManagerError::EventFd)?,
            acpi_address,
            selected_segment: 0,
            serial_pty: None,
            serial_manager: None,
            console_pty: None,
            console_resize_pipe: None,
            virtio_mem_devices: Vec::new(),
            #[cfg(target_arch = "aarch64")]
            gpio_device: None,
            pvpanic_device: None,
            force_iommu,
            restoring,
            io_uring_supported: None,
            boot_id_list,
            timestamp,
            pending_activations: Arc::new(Mutex::new(Vec::default())),
            acpi_platform_addresses: AcpiPlatformAddresses::default(),
            snapshot,
            sandbox_id,
            #[cfg(target_arch = "x86_64")]
            sys_ctrl: None,
            ivshmem_device: None,
        };

        let device_manager = Arc::new(Mutex::new(device_manager));

        address_manager
            .mmio_bus
            .insert(
                Arc::clone(&device_manager) as Arc<Mutex<dyn BusDevice>>,
                acpi_address.0,
                DEVICE_MANAGER_ACPI_SIZE as u64,
            )
            .map_err(DeviceManagerError::BusError)?;

        Ok(device_manager)
    }

    pub fn serial_pty(&self) -> Option<PtyPair> {
        self.serial_pty
            .as_ref()
            .map(|pty| pty.lock().unwrap().clone())
    }

    pub fn console_pty(&self) -> Option<PtyPair> {
        self.console_pty
            .as_ref()
            .map(|pty| pty.lock().unwrap().clone())
    }

    pub fn console_resize_pipe(&self) -> Option<Arc<File>> {
        self.console_resize_pipe.as_ref().map(Arc::clone)
    }

    pub fn fs_device_update(&self, fs_cfg: FsConfig) -> DeviceManagerResult<()> {
        if let Some(id) = fs_cfg.id {
            let device_tree = self.device_tree.lock().unwrap();
            let node = device_tree
                .get(&id)
                .ok_or(DeviceManagerError::UnknownDeviceId(id))?;
            let mut channel_wait = false;
            let (tx, rx): (
                std::sync::mpsc::Sender<bool>,
                std::sync::mpsc::Receiver<bool>,
            ) = std::sync::mpsc::channel();
            if node.dev_fd.is_some()
                && node.pending_dev_message.is_some()
                && fs_cfg.backendfs_config.is_some()
            {
                let mut pending_dev_message =
                    node.pending_dev_message.as_ref().unwrap().lock().unwrap();
                let event = FsEvent {
                    tx,
                    backendfs_config: fs_cfg.backendfs_config.unwrap(),
                };
                pending_dev_message.push(event);
                // SAFETY: FFI call to dup. Trivially safe.
                let dev_fd = unsafe { libc::dup(node.dev_fd.unwrap()) };
                // SAFETY: dev_fd is valid and owned solely by us.
                let dev_evt = unsafe { EventFd::from_raw_fd(dev_fd) };
                dev_evt.write(1).unwrap();
                channel_wait = true;
            }
            if channel_wait {
                debug!("Waiting on channel");
                let success = rx.recv().map_err(DeviceManagerError::VirtioFsServerRecv)?;
                if !success {
                    return Err(DeviceManagerError::VirtioFsServerUpdate(
                        "failed to update filter list".into(),
                    ));
                }
                debug!("Waiting finished with {:?}", success);
            }
        }
        Ok(())
    }

    pub fn create_devices(
        &mut self,
        serial_pty: Option<PtyPair>,
        console_pty: Option<PtyPair>,
        console_resize_pipe: Option<File>,
    ) -> DeviceManagerResult<()> {
        trace_scoped!("create_devices");

        let mut virtio_devices: Vec<MetaVirtioDevice> = Vec::new();

        let interrupt_controller = self.add_interrupt_controller()?;

        // Now we can create the legacy interrupt manager, which needs the freshly
        // formed IOAPIC device.
        let legacy_interrupt_manager: Arc<
            dyn InterruptManager<GroupConfig = LegacyIrqGroupConfig>,
        > = Arc::new(LegacyUserspaceInterruptManager::new(Arc::clone(
            &interrupt_controller,
        )));

        {
            if let Some(acpi_address) = self.memory_manager.lock().unwrap().acpi_address() {
                self.address_manager
                    .mmio_bus
                    .insert(
                        Arc::clone(&self.memory_manager) as Arc<Mutex<dyn BusDevice>>,
                        acpi_address.0,
                        MEMORY_MANAGER_ACPI_SIZE as u64,
                    )
                    .map_err(DeviceManagerError::BusError)?;
            }
        }

        #[cfg(target_arch = "x86_64")]
        // WORKAROUND: force reset event into shutdown.
        self.add_legacy_devices(
            self.exit_evt
                .try_clone()
                .map_err(DeviceManagerError::EventFd)?,
        )?;

        #[cfg(target_arch = "aarch64")]
        self.add_legacy_devices(&legacy_interrupt_manager)?;

        {
            // WORKAROUND: force reset event into shutdown.
            self.ged_notification_device = self.add_acpi_devices(
                &legacy_interrupt_manager,
                self.exit_evt
                    .try_clone()
                    .map_err(DeviceManagerError::EventFd)?,
                self.exit_evt
                    .try_clone()
                    .map_err(DeviceManagerError::EventFd)?,
            )?;
        }

        self.console = self.add_console_device(
            &legacy_interrupt_manager,
            &mut virtio_devices,
            serial_pty,
            console_pty,
            console_resize_pipe,
        )?;

        if let Some(tpm) = self.config.clone().lock().unwrap().tpm.as_ref() {
            let tpm_dev = self.add_tpm_device(tpm.socket.clone())?;
            self.bus_devices
                .push(Arc::clone(&tpm_dev) as Arc<Mutex<dyn BusDevice>>)
        }
        self.legacy_interrupt_manager = Some(legacy_interrupt_manager);

        virtio_devices.append(&mut self.make_virtio_devices()?);

        self.add_pci_devices(virtio_devices.clone())?;

        self.virtio_devices = virtio_devices;

        if self.config.clone().lock().unwrap().pvpanic {
            self.pvpanic_device = self.add_pvpanic_device()?;
        }

        if let Some(ivshmem) = self.config.clone().lock().unwrap().ivshmem.as_ref() {
            self.ivshmem_device = self.add_ivshmem_device(ivshmem)?;
        }

        Ok(())
    }

    fn state(&self) -> DeviceManagerState {
        DeviceManagerState {
            device_tree: self.device_tree.lock().unwrap().clone(),
            device_id_cnt: self.device_id_cnt,
        }
    }

    fn set_state(&mut self, state: &DeviceManagerState) {
        *self.device_tree.lock().unwrap() = state.device_tree.clone();
        self.device_id_cnt = state.device_id_cnt;
    }

    fn get_msi_iova_space(&mut self) -> (u64, u64) {
        #[cfg(target_arch = "aarch64")]
        {
            let vcpus = self.config.lock().unwrap().cpus.boot_vcpus;
            let vgic_config = gic::Gic::create_default_config(vcpus.into());
            (
                vgic_config.msi_addr,
                vgic_config.msi_addr + vgic_config.msi_size - 1,
            )
        }
        #[cfg(target_arch = "x86_64")]
        (0xfee0_0000, 0xfeef_ffff)
    }

    #[cfg(target_arch = "aarch64")]
    /// Gets the information of the devices registered up to some point in time.
    pub fn get_device_info(&self) -> &HashMap<(DeviceType, String), MmioDeviceInfo> {
        &self.id_to_dev_info
    }

    #[allow(unused_variables)]
    fn add_pci_devices(
        &mut self,
        virtio_devices: Vec<MetaVirtioDevice>,
    ) -> DeviceManagerResult<()> {
        let iommu_id = String::from(IOMMU_DEVICE_NAME);

        let iommu_device = if self.config.lock().unwrap().iommu {
            let (device, mapping) = virtio_devices::Iommu::new(
                iommu_id.clone(),
                self.seccomp_action.clone(),
                self.exit_evt
                    .try_clone()
                    .map_err(DeviceManagerError::EventFd)?,
                self.get_msi_iova_space(),
                state_from_id(self.snapshot.as_ref(), iommu_id.as_str())
                    .map_err(DeviceManagerError::RestoreGetState)?,
            )
            .map_err(DeviceManagerError::CreateVirtioIommu)?;
            let device = Arc::new(Mutex::new(device));
            self.iommu_device = Some(Arc::clone(&device));
            self.iommu_mapping = Some(mapping);

            // Fill the device tree with a new node. In case of restore, we
            // know there is nothing to do, so we can simply override the
            // existing entry.
            self.device_tree
                .lock()
                .unwrap()
                .insert(iommu_id.clone(), device_node!(iommu_id, device));

            Some(device)
        } else {
            None
        };

        let mut iommu_attached_devices = Vec::new();
        {
            for handle in virtio_devices {
                let mapping: Option<Arc<IommuMapping>> = if handle.iommu {
                    self.iommu_mapping.clone()
                } else {
                    None
                };

                let dev_id = self.add_virtio_pci_device(
                    handle.virtio_device,
                    &mapping,
                    handle.id,
                    handle.pci_segment,
                    handle.dma_handler,
                )?;

                if handle.iommu {
                    iommu_attached_devices.push(dev_id);
                }
            }

            let mut vfio_iommu_device_ids = self.add_vfio_devices()?;
            iommu_attached_devices.append(&mut vfio_iommu_device_ids);

            let mut vfio_user_iommu_device_ids = self.add_user_devices()?;
            iommu_attached_devices.append(&mut vfio_user_iommu_device_ids);

            // Add all devices from forced iommu segments
            if let Some(platform_config) = self.config.lock().unwrap().platform.as_ref() {
                if let Some(iommu_segments) = platform_config.iommu_segments.as_ref() {
                    for segment in iommu_segments {
                        for device in 0..32 {
                            let bdf = PciBdf::new(*segment, 0, device, 0);
                            if !iommu_attached_devices.contains(&bdf) {
                                iommu_attached_devices.push(bdf);
                            }
                        }
                    }
                }
            }

            if let Some(iommu_device) = iommu_device {
                let dev_id = self.add_virtio_pci_device(iommu_device, &None, iommu_id, 0, None)?;
                self.iommu_attached_devices = Some((dev_id, iommu_attached_devices));
            }
        }

        for segment in &self.pci_segments {
            #[cfg(target_arch = "x86_64")]
            if let Some(pci_config_io) = segment.pci_config_io.as_ref() {
                self.bus_devices
                    .push(Arc::clone(pci_config_io) as Arc<Mutex<dyn BusDevice>>);
            }

            self.bus_devices
                .push(Arc::clone(&segment.pci_config_mmio) as Arc<Mutex<dyn BusDevice>>);
        }

        Ok(())
    }

    #[cfg(target_arch = "aarch64")]
    fn add_interrupt_controller(
        &mut self,
    ) -> DeviceManagerResult<Arc<Mutex<dyn InterruptController>>> {
        let interrupt_controller: Arc<Mutex<gic::Gic>> = Arc::new(Mutex::new(
            gic::Gic::new(
                self.config.lock().unwrap().cpus.boot_vcpus,
                Arc::clone(&self.msi_interrupt_manager),
            )
            .map_err(DeviceManagerError::CreateInterruptController)?,
        ));

        self.interrupt_controller = Some(interrupt_controller.clone());

        // Unlike x86_64, the "interrupt_controller" here for AArch64 is only
        // a `Gic` object that implements the `InterruptController` to provide
        // interrupt delivery service. This is not the real GIC device so that
        // we do not need to insert it to the device tree.

        Ok(interrupt_controller)
    }

    #[cfg(target_arch = "aarch64")]
    pub fn get_interrupt_controller(&mut self) -> Option<&Arc<Mutex<gic::Gic>>> {
        self.interrupt_controller.as_ref()
    }

    #[cfg(target_arch = "x86_64")]
    fn add_interrupt_controller(
        &mut self,
    ) -> DeviceManagerResult<Arc<Mutex<dyn InterruptController>>> {
        let id = String::from(IOAPIC_DEVICE_NAME);

        // Create IOAPIC
        let interrupt_controller = Arc::new(Mutex::new(
            ioapic::Ioapic::new(
                id.clone(),
                APIC_START,
                Arc::clone(&self.msi_interrupt_manager),
            )
            .map_err(DeviceManagerError::CreateInterruptController)?,
        ));

        self.interrupt_controller = Some(interrupt_controller.clone());

        self.address_manager
            .mmio_bus
            .insert(interrupt_controller.clone(), IOAPIC_START.0, IOAPIC_SIZE)
            .map_err(DeviceManagerError::BusError)?;

        self.bus_devices
            .push(Arc::clone(&interrupt_controller) as Arc<Mutex<dyn BusDevice>>);

        // Fill the device tree with a new node. In case of restore, we
        // know there is nothing to do, so we can simply override the
        // existing entry.
        self.device_tree
            .lock()
            .unwrap()
            .insert(id.clone(), device_node!(id, interrupt_controller));

        Ok(interrupt_controller)
    }

    fn add_acpi_devices(
        &mut self,
        interrupt_manager: &Arc<dyn InterruptManager<GroupConfig = LegacyIrqGroupConfig>>,
        reset_evt: EventFd,
        exit_evt: EventFd,
    ) -> DeviceManagerResult<Option<Arc<Mutex<devices::AcpiGedDevice>>>> {
        let shutdown_device = Arc::new(Mutex::new(devices::AcpiShutdownDevice::new(
            exit_evt, reset_evt,
        )));

        self.bus_devices
            .push(Arc::clone(&shutdown_device) as Arc<Mutex<dyn BusDevice>>);

        #[cfg(target_arch = "x86_64")]
        {
            let shutdown_pio_address: u16 = 0x600;

            self.address_manager
                .allocator
                .lock()
                .unwrap()
                .allocate_io_addresses(Some(GuestAddress(shutdown_pio_address.into())), 0x8, None)
                .ok_or(DeviceManagerError::AllocateIoPort)?;

            self.address_manager
                .io_bus
                .insert(shutdown_device, shutdown_pio_address.into(), 0x4)
                .map_err(DeviceManagerError::BusError)?;

            self.acpi_platform_addresses.sleep_control_reg_address =
                Some(GenericAddress::io_port_address::<u8>(shutdown_pio_address));
            self.acpi_platform_addresses.sleep_status_reg_address =
                Some(GenericAddress::io_port_address::<u8>(shutdown_pio_address));
            self.acpi_platform_addresses.reset_reg_address =
                Some(GenericAddress::io_port_address::<u8>(shutdown_pio_address));
        }

        let ged_irq = self
            .address_manager
            .allocator
            .lock()
            .unwrap()
            .allocate_irq()
            .unwrap();
        let interrupt_group = interrupt_manager
            .create_group(LegacyIrqGroupConfig {
                irq: ged_irq as InterruptIndex,
            })
            .map_err(DeviceManagerError::CreateInterruptGroup)?;
        let ged_address = self
            .address_manager
            .allocator
            .lock()
            .unwrap()
            .allocate_platform_mmio_addresses(
                None,
                devices::acpi::GED_DEVICE_ACPI_SIZE as u64,
                None,
            )
            .ok_or(DeviceManagerError::AllocateMmioAddress)?;
        let ged_device = Arc::new(Mutex::new(devices::AcpiGedDevice::new(
            interrupt_group,
            ged_irq,
            ged_address,
        )));
        self.address_manager
            .mmio_bus
            .insert(
                ged_device.clone(),
                ged_address.0,
                devices::acpi::GED_DEVICE_ACPI_SIZE as u64,
            )
            .map_err(DeviceManagerError::BusError)?;
        self.bus_devices
            .push(Arc::clone(&ged_device) as Arc<Mutex<dyn BusDevice>>);

        let pm_timer_device = Arc::new(Mutex::new(devices::AcpiPmTimerDevice::new()));

        self.bus_devices
            .push(Arc::clone(&pm_timer_device) as Arc<Mutex<dyn BusDevice>>);

        #[cfg(target_arch = "x86_64")]
        {
            let pm_timer_pio_address: u16 = 0x608;

            self.address_manager
                .allocator
                .lock()
                .unwrap()
                .allocate_io_addresses(Some(GuestAddress(pm_timer_pio_address.into())), 0x4, None)
                .ok_or(DeviceManagerError::AllocateIoPort)?;

            self.address_manager
                .io_bus
                .insert(pm_timer_device, pm_timer_pio_address.into(), 0x4)
                .map_err(DeviceManagerError::BusError)?;

            self.acpi_platform_addresses.pm_timer_address =
                Some(GenericAddress::io_port_address::<u32>(pm_timer_pio_address));
        }

        Ok(Some(ged_device))
    }

    #[cfg(target_arch = "x86_64")]
    fn add_legacy_devices(&mut self, reset_evt: EventFd) -> DeviceManagerResult<()> {
        // Add a shutdown device (i8042)
        let i8042 = Arc::new(Mutex::new(devices::legacy::I8042Device::new(
            reset_evt.try_clone().unwrap(),
        )));

        self.bus_devices
            .push(Arc::clone(&i8042) as Arc<Mutex<dyn BusDevice>>);

        self.address_manager
            .io_bus
            .insert(i8042, 0x61, 0x4)
            .map_err(DeviceManagerError::BusError)?;
        {
            // Add a CMOS emulated device
            let mem_size = self
                .memory_manager
                .lock()
                .unwrap()
                .guest_memory()
                .memory()
                .last_addr()
                .0
                + 1;
            let mem_below_4g = std::cmp::min(arch::layout::MEM_32BIT_RESERVED_START.0, mem_size);
            let mem_above_4g = mem_size.saturating_sub(arch::layout::RAM_64BIT_START.0);

            let cmos = Arc::new(Mutex::new(devices::legacy::Cmos::new(
                mem_below_4g,
                mem_above_4g,
                reset_evt,
            )));

            self.bus_devices
                .push(Arc::clone(&cmos) as Arc<Mutex<dyn BusDevice>>);

            self.address_manager
                .io_bus
                .insert(cmos, 0x70, 0x2)
                .map_err(DeviceManagerError::BusError)?;

            let fwdebug = Arc::new(Mutex::new(devices::legacy::FwDebugDevice::new()));

            self.bus_devices
                .push(Arc::clone(&fwdebug) as Arc<Mutex<dyn BusDevice>>);

            self.address_manager
                .io_bus
                .insert(fwdebug, 0x402, 0x1)
                .map_err(DeviceManagerError::BusError)?;
        }

        // 0x80 debug port
        let debug_port = Arc::new(Mutex::new(devices::legacy::DebugPort::new(self.timestamp)));
        self.bus_devices
            .push(Arc::clone(&debug_port) as Arc<Mutex<dyn BusDevice>>);
        self.address_manager
            .io_bus
            .insert(debug_port, 0x80, 0x1)
            .map_err(DeviceManagerError::BusError)?;

        // 0x0680 system control port
        #[cfg(target_arch = "x86_64")]
        if self.config.lock().unwrap().sys_ctrl {
            let id = String::from(SYS_CTRL_DEVICE_NAME);
            let sys_ctrl = Arc::new(Mutex::new(devices::legacy::SysCtrl::new(
                id.clone(),
                state_from_id(self.snapshot.as_ref(), id.as_str())
                    .map_err(DeviceManagerError::RestoreGetState)?,
            )));
            self.sys_ctrl = Some(Arc::clone(&sys_ctrl));
            self.bus_devices
                .push(Arc::clone(&sys_ctrl) as Arc<Mutex<dyn BusDevice>>);
            self.address_manager
                .io_bus
                .insert(
                    Arc::clone(&sys_ctrl) as Arc<Mutex<dyn BusDevice>>,
                    0x680,
                    0x1,
                )
                .map_err(DeviceManagerError::BusError)?;
            self.device_tree
                .lock()
                .unwrap()
                .insert(id.clone(), device_node!(id, sys_ctrl));
        }

        Ok(())
    }

    #[cfg(target_arch = "aarch64")]
    fn add_legacy_devices(
        &mut self,
        interrupt_manager: &Arc<dyn InterruptManager<GroupConfig = LegacyIrqGroupConfig>>,
    ) -> DeviceManagerResult<()> {
        // Add a RTC device
        let rtc_irq = self
            .address_manager
            .allocator
            .lock()
            .unwrap()
            .allocate_irq()
            .unwrap();

        let interrupt_group = interrupt_manager
            .create_group(LegacyIrqGroupConfig {
                irq: rtc_irq as InterruptIndex,
            })
            .map_err(DeviceManagerError::CreateInterruptGroup)?;

        let rtc_device = Arc::new(Mutex::new(devices::legacy::Rtc::new(interrupt_group)));

        self.bus_devices
            .push(Arc::clone(&rtc_device) as Arc<Mutex<dyn BusDevice>>);

        let addr = arch::layout::LEGACY_RTC_MAPPED_IO_START;

        self.address_manager
            .mmio_bus
            .insert(rtc_device, addr.0, MMIO_LEN)
            .map_err(DeviceManagerError::BusError)?;

        self.id_to_dev_info.insert(
            (DeviceType::Rtc, "rtc".to_string()),
            MmioDeviceInfo {
                addr: addr.0,
                len: MMIO_LEN,
                irq: rtc_irq,
            },
        );

        // Add a GPIO device
        let id = String::from(GPIO_DEVICE_NAME);
        let gpio_irq = self
            .address_manager
            .allocator
            .lock()
            .unwrap()
            .allocate_irq()
            .unwrap();

        let interrupt_group = interrupt_manager
            .create_group(LegacyIrqGroupConfig {
                irq: gpio_irq as InterruptIndex,
            })
            .map_err(DeviceManagerError::CreateInterruptGroup)?;

        let gpio_device = Arc::new(Mutex::new(devices::legacy::Gpio::new(
            id.clone(),
            interrupt_group,
        )));

        self.bus_devices
            .push(Arc::clone(&gpio_device) as Arc<Mutex<dyn BusDevice>>);

        let addr = arch::layout::LEGACY_GPIO_MAPPED_IO_START;

        self.address_manager
            .mmio_bus
            .insert(gpio_device.clone(), addr.0, MMIO_LEN)
            .map_err(DeviceManagerError::BusError)?;

        self.gpio_device = Some(gpio_device.clone());

        self.id_to_dev_info.insert(
            (DeviceType::Gpio, "gpio".to_string()),
            MmioDeviceInfo {
                addr: addr.0,
                len: MMIO_LEN,
                irq: gpio_irq,
            },
        );

        self.device_tree
            .lock()
            .unwrap()
            .insert(id.clone(), device_node!(id, gpio_device));

        Ok(())
    }

    #[cfg(target_arch = "x86_64")]
    fn add_serial_device(
        &mut self,
        interrupt_manager: &Arc<dyn InterruptManager<GroupConfig = LegacyIrqGroupConfig>>,
        serial_writer: Option<Box<dyn io::Write + Send>>,
    ) -> DeviceManagerResult<Arc<Mutex<Serial>>> {
        // Serial is tied to IRQ #4
        let serial_irq = 4;

        let id = String::from(SERIAL_DEVICE_NAME);

        let interrupt_group = interrupt_manager
            .create_group(LegacyIrqGroupConfig {
                irq: serial_irq as InterruptIndex,
            })
            .map_err(DeviceManagerError::CreateInterruptGroup)?;

        let serial = Arc::new(Mutex::new(Serial::new(
            id.clone(),
            interrupt_group,
            serial_writer,
        )));

        self.bus_devices
            .push(Arc::clone(&serial) as Arc<Mutex<dyn BusDevice>>);

        self.address_manager
            .allocator
            .lock()
            .unwrap()
            .allocate_io_addresses(Some(GuestAddress(0x3f8)), 0x8, None)
            .ok_or(DeviceManagerError::AllocateIoPort)?;

        self.address_manager
            .io_bus
            .insert(serial.clone(), 0x3f8, 0x8)
            .map_err(DeviceManagerError::BusError)?;

        // Fill the device tree with a new node. In case of restore, we
        // know there is nothing to do, so we can simply override the
        // existing entry.
        self.device_tree
            .lock()
            .unwrap()
            .insert(id.clone(), device_node!(id, serial));

        Ok(serial)
    }

    #[cfg(target_arch = "aarch64")]
    fn add_serial_device(
        &mut self,
        interrupt_manager: &Arc<dyn InterruptManager<GroupConfig = LegacyIrqGroupConfig>>,
        serial_writer: Option<Box<dyn io::Write + Send>>,
    ) -> DeviceManagerResult<Arc<Mutex<Pl011>>> {
        let id = String::from(SERIAL_DEVICE_NAME);

        let serial_irq = self
            .address_manager
            .allocator
            .lock()
            .unwrap()
            .allocate_irq()
            .unwrap();

        let interrupt_group = interrupt_manager
            .create_group(LegacyIrqGroupConfig {
                irq: serial_irq as InterruptIndex,
            })
            .map_err(DeviceManagerError::CreateInterruptGroup)?;

        let serial = Arc::new(Mutex::new(devices::legacy::Pl011::new(
            id.clone(),
            interrupt_group,
            serial_writer,
            self.timestamp,
        )));

        self.bus_devices
            .push(Arc::clone(&serial) as Arc<Mutex<dyn BusDevice>>);

        let addr = arch::layout::LEGACY_SERIAL_MAPPED_IO_START;

        self.address_manager
            .mmio_bus
            .insert(serial.clone(), addr.0, MMIO_LEN)
            .map_err(DeviceManagerError::BusError)?;

        self.id_to_dev_info.insert(
            (DeviceType::Serial, DeviceType::Serial.to_string()),
            MmioDeviceInfo {
                addr: addr.0,
                len: MMIO_LEN,
                irq: serial_irq,
            },
        );

        self.cmdline_additions
            .push(format!("earlycon=pl011,mmio,0x{:08x}", addr.0));

        // Fill the device tree with a new node. In case of restore, we
        // know there is nothing to do, so we can simply override the
        // existing entry.
        self.device_tree
            .lock()
            .unwrap()
            .insert(id.clone(), device_node!(id, serial));

        Ok(serial)
    }

    fn modify_mode<F: FnOnce(&mut termios)>(
        &self,
        fd: RawFd,
        f: F,
    ) -> vmm_sys_util::errno::Result<()> {
        // SAFETY: safe because we check the return value of isatty.
        if unsafe { isatty(fd) } != 1 {
            return Ok(());
        }

        // SAFETY: The following pair are safe because termios gets totally overwritten by tcgetattr
        // and we check the return result.
        let mut termios: termios = unsafe { zeroed() };
        let ret = unsafe { tcgetattr(fd, &mut termios as *mut _) };
        if ret < 0 {
            return vmm_sys_util::errno::errno_result();
        }
        f(&mut termios);
        // SAFETY: Safe because the syscall will only read the extent of termios and we check
        // the return result.
        let ret = unsafe { tcsetattr(fd, TCSANOW, &termios as *const _) };
        if ret < 0 {
            return vmm_sys_util::errno::errno_result();
        }

        Ok(())
    }

    fn set_raw_mode(&self, f: &mut File) -> vmm_sys_util::errno::Result<()> {
        // SAFETY: FFI call. Variable t is guaranteed to be a valid termios from modify_mode.
        self.modify_mode(f.as_raw_fd(), |t| unsafe { cfmakeraw(t) })
    }

    fn listen_for_sigwinch_on_tty(&mut self, pty_main: File, pty_sub: File) -> std::io::Result<()> {
        let seccomp_filter = get_seccomp_filter(
            &self.seccomp_action,
            Thread::PtyForeground,
            self.hypervisor_type,
        )
        .unwrap();

        match start_sigwinch_listener(seccomp_filter, pty_main, pty_sub) {
            Ok(pipe) => {
                self.console_resize_pipe = Some(Arc::new(pipe));
            }
            Err(e) => {
                warn!("Ignoring error from setting up SIGWINCH listener: {}", e)
            }
        }

        Ok(())
    }

    fn add_virtio_console_device(
        &mut self,
        virtio_devices: &mut Vec<MetaVirtioDevice>,
        console_pty: Option<PtyPair>,
        resize_pipe: Option<File>,
    ) -> DeviceManagerResult<Option<Arc<virtio_devices::ConsoleResizer>>> {
        let console_config = self.config.lock().unwrap().console.clone();
        let endpoint = match console_config.mode {
            ConsoleOutputMode::File => {
                let file = File::create(console_config.file.as_ref().unwrap())
                    .map_err(DeviceManagerError::ConsoleOutputFileOpen)?;
                Endpoint::File(file)
            }
            ConsoleOutputMode::Pty => {
                if let Some(pty) = console_pty {
                    self.config.lock().unwrap().console.file = Some(pty.path.clone());
                    let file = pty.main.try_clone().unwrap();
                    self.console_pty = Some(Arc::new(Mutex::new(pty)));
                    self.console_resize_pipe = resize_pipe.map(Arc::new);
                    Endpoint::PtyPair(file.try_clone().unwrap(), file)
                } else {
                    let (main, mut sub, path) =
                        create_pty().map_err(DeviceManagerError::ConsolePtyOpen)?;
                    self.set_raw_mode(&mut sub)
                        .map_err(DeviceManagerError::SetPtyRaw)?;
                    self.config.lock().unwrap().console.file = Some(path.clone());
                    let file = main.try_clone().unwrap();
                    assert!(resize_pipe.is_none());
                    self.listen_for_sigwinch_on_tty(main.try_clone().unwrap(), sub)
                        .unwrap();
                    self.console_pty = Some(Arc::new(Mutex::new(PtyPair { main, path })));
                    Endpoint::PtyPair(file.try_clone().unwrap(), file)
                }
            }
            ConsoleOutputMode::Tty => {
                // Duplicating the file descriptors like this is needed as otherwise
                // they will be closed on a reboot and the numbers reused

                // SAFETY: FFI call to dup. Trivially safe.
                let stdout = unsafe { libc::dup(libc::STDOUT_FILENO) };
                if stdout == -1 {
                    return vmm_sys_util::errno::errno_result().map_err(DeviceManagerError::DupFd);
                }
                // SAFETY: stdout is valid and owned solely by us.
                let stdout = unsafe { File::from_raw_fd(stdout) };

                // If an interactive TTY then we can accept input
                // SAFETY: FFI call. Trivially safe.
                if unsafe { libc::isatty(libc::STDIN_FILENO) == 1 } {
                    // SAFETY: FFI call to dup. Trivially safe.
                    let stdin = unsafe { libc::dup(libc::STDIN_FILENO) };
                    if stdin == -1 {
                        return vmm_sys_util::errno::errno_result()
                            .map_err(DeviceManagerError::DupFd);
                    }
                    // SAFETY: stdin is valid and owned solely by us.
                    let stdin = unsafe { File::from_raw_fd(stdin) };

                    Endpoint::FilePair(stdout, stdin)
                } else {
                    Endpoint::File(stdout)
                }
            }
            ConsoleOutputMode::Null => Endpoint::Null,
            ConsoleOutputMode::Off => return Ok(None),
        };
        let id = String::from(CONSOLE_DEVICE_NAME);

        let (virtio_console_device, console_resizer) = virtio_devices::Console::new(
            id.clone(),
            endpoint,
            self.console_resize_pipe
                .as_ref()
                .map(|p| p.try_clone().unwrap()),
            self.force_iommu | console_config.iommu,
            self.seccomp_action.clone(),
            self.exit_evt
                .try_clone()
                .map_err(DeviceManagerError::EventFd)?,
            state_from_id(self.snapshot.as_ref(), id.as_str())
                .map_err(DeviceManagerError::RestoreGetState)?,
        )
        .map_err(DeviceManagerError::CreateVirtioConsole)?;
        let virtio_console_device = Arc::new(Mutex::new(virtio_console_device));
        virtio_devices.push(MetaVirtioDevice {
            virtio_device: Arc::clone(&virtio_console_device)
                as Arc<Mutex<dyn virtio_devices::VirtioDevice>>,
            iommu: console_config.iommu,
            id: id.clone(),
            pci_segment: 0,
            dma_handler: None,
        });

        // Fill the device tree with a new node. In case of restore, we
        // know there is nothing to do, so we can simply override the
        // existing entry.
        self.device_tree
            .lock()
            .unwrap()
            .insert(id.clone(), device_node!(id, virtio_console_device));

        // Only provide a resizer (for SIGWINCH handling) if the console is attached to the TTY
        Ok(
            if matches!(console_config.mode, ConsoleOutputMode::Tty) && console_config.sigwinch {
                Some(console_resizer)
            } else {
                None
            },
        )
    }

    fn add_console_device(
        &mut self,
        interrupt_manager: &Arc<dyn InterruptManager<GroupConfig = LegacyIrqGroupConfig>>,
        virtio_devices: &mut Vec<MetaVirtioDevice>,
        serial_pty: Option<PtyPair>,
        console_pty: Option<PtyPair>,
        console_resize_pipe: Option<File>,
    ) -> DeviceManagerResult<Arc<Console>> {
        let serial_config = self.config.lock().unwrap().serial.clone();
        let serial_writer: Option<Box<dyn io::Write + Send>> = match serial_config.mode {
            ConsoleOutputMode::File => Some(Box::new(
                File::create(serial_config.file.as_ref().unwrap())
                    .map_err(DeviceManagerError::SerialOutputFileOpen)?,
            )),
            ConsoleOutputMode::Pty => {
                if let Some(pty) = serial_pty {
                    self.config.lock().unwrap().serial.file = Some(pty.path.clone());
                    self.serial_pty = Some(Arc::new(Mutex::new(pty)));
                } else {
                    let (main, mut sub, path) =
                        create_pty().map_err(DeviceManagerError::SerialPtyOpen)?;
                    self.set_raw_mode(&mut sub)
                        .map_err(DeviceManagerError::SetPtyRaw)?;
                    self.config.lock().unwrap().serial.file = Some(path.clone());
                    self.serial_pty = Some(Arc::new(Mutex::new(PtyPair { main, path })));
                }
                None
            }
            ConsoleOutputMode::Tty => Some(Box::new(stdout())),
            ConsoleOutputMode::Off | ConsoleOutputMode::Null => None,
        };
        if serial_config.mode != ConsoleOutputMode::Off {
            let serial = self.add_serial_device(interrupt_manager, serial_writer)?;
            self.serial_manager = match serial_config.mode {
                ConsoleOutputMode::Pty | ConsoleOutputMode::Tty => {
                    let serial_manager =
                        SerialManager::new(serial, self.serial_pty.clone(), serial_config.mode)
                            .map_err(DeviceManagerError::CreateSerialManager)?;
                    if let Some(mut serial_manager) = serial_manager {
                        serial_manager
                            .start_thread(
                                self.exit_evt
                                    .try_clone()
                                    .map_err(DeviceManagerError::EventFd)?,
                            )
                            .map_err(DeviceManagerError::SpawnSerialManager)?;
                        Some(Arc::new(serial_manager))
                    } else {
                        None
                    }
                }
                _ => None,
            };
        }

        let console_resizer =
            self.add_virtio_console_device(virtio_devices, console_pty, console_resize_pipe)?;

        Ok(Arc::new(Console { console_resizer }))
    }

    fn add_tpm_device(
        &mut self,
        tpm_path: PathBuf,
    ) -> DeviceManagerResult<Arc<Mutex<devices::tpm::Tpm>>> {
        // Create TPM Device
        let tpm = devices::tpm::Tpm::new(tpm_path.to_str().unwrap().to_string()).map_err(|e| {
            DeviceManagerError::CreateTpmDevice(anyhow!("Failed to create TPM Device : {:?}", e))
        })?;
        let tpm = Arc::new(Mutex::new(tpm));

        // Add TPM Device to mmio
        self.address_manager
            .mmio_bus
            .insert(
                tpm.clone(),
                arch::layout::TPM_START.0,
                arch::layout::TPM_SIZE,
            )
            .map_err(DeviceManagerError::BusError)?;

        Ok(tpm)
    }

    fn make_virtio_devices(&mut self) -> DeviceManagerResult<Vec<MetaVirtioDevice>> {
        let mut devices: Vec<MetaVirtioDevice> = Vec::new();

        // Create "standard" virtio devices (net/block/rng)
        devices.append(&mut self.make_virtio_block_devices()?);
        devices.append(&mut self.make_virtio_net_devices()?);
        devices.append(&mut self.make_virtio_rng_devices()?);

        // Add virtio-fs if required
        devices.append(&mut self.make_virtio_fs_devices()?);

        // Add virtio-pmem if required
        devices.append(&mut self.make_virtio_pmem_devices()?);

        // Add virtio-vsock if required
        devices.append(&mut self.make_virtio_vsock_devices()?);

        devices.append(&mut self.make_virtio_mem_devices()?);

        // Add virtio-balloon if required
        devices.append(&mut self.make_virtio_balloon_devices()?);

        // Add virtio-watchdog device
        devices.append(&mut self.make_virtio_watchdog_devices()?);

        // Add vDPA devices if required
        devices.append(&mut self.make_vdpa_devices()?);

        Ok(devices)
    }

    // Cache whether io_uring is supported to avoid probing for very block device
    fn io_uring_is_supported(&mut self) -> bool {
        if let Some(supported) = self.io_uring_supported {
            return supported;
        }

        let supported = block_io_uring_is_supported();
        self.io_uring_supported = Some(supported);
        supported
    }

    fn make_virtio_block_device(
        &mut self,
        disk_cfg: &mut DiskConfig,
    ) -> DeviceManagerResult<MetaVirtioDevice> {
        let id = if let Some(id) = &disk_cfg.id {
            id.clone()
        } else {
            let id = self.next_device_name(DISK_DEVICE_NAME_PREFIX)?;
            disk_cfg.id = Some(id.clone());
            id
        };

        info!("Creating virtio-block device: {:?}", disk_cfg);

        let (virtio_device, migratable_device) = if disk_cfg.vhost_user {
            let socket = disk_cfg.vhost_socket.as_ref().unwrap().clone();
            let vu_cfg = VhostUserConfig {
                socket,
                num_queues: disk_cfg.num_queues,
                queue_size: disk_cfg.queue_size,
            };
            let vhost_user_block = Arc::new(Mutex::new(
                match virtio_devices::vhost_user::Blk::new(
                    id.clone(),
                    vu_cfg,
                    self.seccomp_action.clone(),
                    self.exit_evt
                        .try_clone()
                        .map_err(DeviceManagerError::EventFd)?,
                    self.force_iommu,
                    state_from_id(self.snapshot.as_ref(), id.as_str())
                        .map_err(DeviceManagerError::RestoreGetState)?,
                ) {
                    Ok(vub_device) => vub_device,
                    Err(e) => {
                        return Err(DeviceManagerError::CreateVhostUserBlk(e));
                    }
                },
            ));

            (
                Arc::clone(&vhost_user_block) as Arc<Mutex<dyn virtio_devices::VirtioDevice>>,
                vhost_user_block as Arc<Mutex<dyn Migratable>>,
            )
        } else {
            let mut options = OpenOptions::new();
            options.read(true);
            options.write(!disk_cfg.readonly);
            if disk_cfg.direct {
                options.custom_flags(libc::O_DIRECT);
            }
            // Open block device path
            let mut file: File = options
                .open(
                    disk_cfg
                        .path
                        .as_ref()
                        .ok_or(DeviceManagerError::NoDiskPath)?
                        .clone(),
                )
                .map_err(DeviceManagerError::Disk)?;
            let image_type =
                detect_image_type(&mut file).map_err(DeviceManagerError::DetectImageType)?;

            let image = match image_type {
                ImageType::FixedVhd => {
                    // Use asynchronous backend relying on io_uring if the
                    // syscalls are supported.
                    if !disk_cfg.disable_io_uring && self.io_uring_is_supported() {
                        info!("Using asynchronous fixed VHD disk file (io_uring)");
                        Box::new(
                            FixedVhdDiskAsync::new(file)
                                .map_err(DeviceManagerError::CreateFixedVhdDiskAsync)?,
                        ) as Box<dyn DiskFile>
                    } else {
                        info!("Using synchronous fixed VHD disk file");
                        Box::new(
                            FixedVhdDiskSync::new(file)
                                .map_err(DeviceManagerError::CreateFixedVhdDiskSync)?,
                        ) as Box<dyn DiskFile>
                    }
                }
                ImageType::Raw => {
                    // Use asynchronous backend relying on io_uring if the
                    // syscalls are supported.
                    if !disk_cfg.disable_io_uring && self.io_uring_is_supported() {
                        info!("Using asynchronous RAW disk file (io_uring)");
                        Box::new(RawFileDisk::new(file)) as Box<dyn DiskFile>
                    } else {
                        info!("Using synchronous RAW disk file");
                        Box::new(RawFileDiskSync::new(file)) as Box<dyn DiskFile>
                    }
                }
                ImageType::Qcow2 => {
                    info!("Using synchronous QCOW disk file");
                    Box::new(
                        QcowDiskSync::new(file, disk_cfg.direct)
                            .map_err(DeviceManagerError::CreateQcowDiskSync)?,
                    ) as Box<dyn DiskFile>
                }
                ImageType::Vhdx => {
                    info!("Using synchronous VHDX disk file");
                    Box::new(
                        VhdxDiskSync::new(file)
                            .map_err(DeviceManagerError::CreateFixedVhdxDiskSync)?,
                    ) as Box<dyn DiskFile>
                }
            };

            let virtio_block = Arc::new(Mutex::new(
                virtio_devices::Block::new(
                    id.clone(),
                    image,
                    disk_cfg
                        .path
                        .as_ref()
                        .ok_or(DeviceManagerError::NoDiskPath)?
                        .clone(),
                    disk_cfg.readonly,
                    self.force_iommu | disk_cfg.iommu,
                    disk_cfg.num_queues,
                    disk_cfg.queue_size,
                    self.seccomp_action.clone(),
                    disk_cfg.rate_limiter_config,
                    self.exit_evt
                        .try_clone()
                        .map_err(DeviceManagerError::EventFd)?,
                    state_from_id(self.snapshot.as_ref(), id.as_str())
                        .map_err(DeviceManagerError::RestoreGetState)?,
                )
                .map_err(DeviceManagerError::CreateVirtioBlock)?,
            ));

            (
                Arc::clone(&virtio_block) as Arc<Mutex<dyn virtio_devices::VirtioDevice>>,
                virtio_block as Arc<Mutex<dyn Migratable>>,
            )
        };

        // Fill the device tree with a new node. In case of restore, we
        // know there is nothing to do, so we can simply override the
        // existing entry.
        self.device_tree
            .lock()
            .unwrap()
            .insert(id.clone(), device_node!(id, migratable_device));

        Ok(MetaVirtioDevice {
            virtio_device,
            iommu: disk_cfg.iommu,
            id,
            pci_segment: disk_cfg.pci_segment,
            dma_handler: None,
        })
    }

    fn make_virtio_block_devices(&mut self) -> DeviceManagerResult<Vec<MetaVirtioDevice>> {
        let mut devices = Vec::new();

        let mut block_devices = self.config.lock().unwrap().disks.clone();
        if let Some(disk_list_cfg) = &mut block_devices {
            for disk_cfg in disk_list_cfg.iter_mut() {
                devices.push(self.make_virtio_block_device(disk_cfg)?);
            }
        }
        self.config.lock().unwrap().disks = block_devices;

        Ok(devices)
    }

    fn make_virtio_net_device(
        &mut self,
        net_cfg: &mut NetConfig,
    ) -> DeviceManagerResult<MetaVirtioDevice> {
        let id = if let Some(id) = &net_cfg.id {
            id.clone()
        } else {
            let id = self.next_device_name(NET_DEVICE_NAME_PREFIX)?;
            net_cfg.id = Some(id.clone());
            id
        };
        info!("Creating virtio-net device: {:?}", net_cfg);

        let (virtio_device, migratable_device) = if net_cfg.vhost_user {
            let socket = net_cfg.vhost_socket.as_ref().unwrap().clone();
            let vu_cfg = VhostUserConfig {
                socket,
                num_queues: net_cfg.num_queues,
                queue_size: net_cfg.queue_size,
            };
            let server = match net_cfg.vhost_mode {
                VhostMode::Client => false,
                VhostMode::Server => true,
            };
            let vhost_user_net = Arc::new(Mutex::new(
                match virtio_devices::vhost_user::Net::new(
                    id.clone(),
                    net_cfg.mac,
                    net_cfg.mtu,
                    vu_cfg,
                    server,
                    self.seccomp_action.clone(),
                    self.exit_evt
                        .try_clone()
                        .map_err(DeviceManagerError::EventFd)?,
                    self.force_iommu,
                    state_from_id(self.snapshot.as_ref(), id.as_str())
                        .map_err(DeviceManagerError::RestoreGetState)?,
                ) {
                    Ok(vun_device) => vun_device,
                    Err(e) => {
                        return Err(DeviceManagerError::CreateVhostUserNet(e));
                    }
                },
            ));

            (
                Arc::clone(&vhost_user_net) as Arc<Mutex<dyn virtio_devices::VirtioDevice>>,
                vhost_user_net as Arc<Mutex<dyn Migratable>>,
            )
        } else {
            let state = state_from_id(self.snapshot.as_ref(), id.as_str())
                .map_err(DeviceManagerError::RestoreGetState)?;

            let virtio_net = if let Some(ref tap_if_name) = net_cfg.tap {
                Arc::new(Mutex::new(
                    virtio_devices::Net::new(
                        id.clone(),
                        Some(tap_if_name),
                        None,
                        None,
                        Some(net_cfg.mac),
                        &mut net_cfg.host_mac,
                        net_cfg.mtu,
                        self.force_iommu | net_cfg.iommu,
                        net_cfg.num_queues,
                        net_cfg.queue_size,
                        self.seccomp_action.clone(),
                        net_cfg.rate_limiter_config,
                        self.exit_evt
                            .try_clone()
                            .map_err(DeviceManagerError::EventFd)?,
                        state,
                        Some(self.sandbox_id.clone()),
                    )
                    .map_err(DeviceManagerError::CreateVirtioNet)?,
                ))
            } else if let Some(fds) = &net_cfg.fds {
                Arc::new(Mutex::new(
                    virtio_devices::Net::from_tap_fds(
                        id.clone(),
                        fds,
                        Some(net_cfg.mac),
                        net_cfg.mtu,
                        self.force_iommu | net_cfg.iommu,
                        net_cfg.queue_size,
                        self.seccomp_action.clone(),
                        net_cfg.rate_limiter_config,
                        self.exit_evt
                            .try_clone()
                            .map_err(DeviceManagerError::EventFd)?,
                        state,
                    )
                    .map_err(DeviceManagerError::CreateVirtioNet)?,
                ))
            } else {
                Arc::new(Mutex::new(
                    virtio_devices::Net::new(
                        id.clone(),
                        None,
                        Some(net_cfg.ip),
                        Some(net_cfg.mask),
                        Some(net_cfg.mac),
                        &mut net_cfg.host_mac,
                        net_cfg.mtu,
                        self.force_iommu | net_cfg.iommu,
                        net_cfg.num_queues,
                        net_cfg.queue_size,
                        self.seccomp_action.clone(),
                        net_cfg.rate_limiter_config,
                        self.exit_evt
                            .try_clone()
                            .map_err(DeviceManagerError::EventFd)?,
                        state,
                        None,
                    )
                    .map_err(DeviceManagerError::CreateVirtioNet)?,
                ))
            };

            (
                Arc::clone(&virtio_net) as Arc<Mutex<dyn virtio_devices::VirtioDevice>>,
                virtio_net as Arc<Mutex<dyn Migratable>>,
            )
        };

        // Fill the device tree with a new node. In case of restore, we
        // know there is nothing to do, so we can simply override the
        // existing entry.
        self.device_tree
            .lock()
            .unwrap()
            .insert(id.clone(), device_node!(id, migratable_device));

        Ok(MetaVirtioDevice {
            virtio_device,
            iommu: net_cfg.iommu,
            id,
            pci_segment: net_cfg.pci_segment,
            dma_handler: None,
        })
    }

    /// Add virto-net and vhost-user-net devices
    fn make_virtio_net_devices(&mut self) -> DeviceManagerResult<Vec<MetaVirtioDevice>> {
        let mut devices = Vec::new();
        let mut net_devices = self.config.lock().unwrap().net.clone();
        if let Some(net_list_cfg) = &mut net_devices {
            for net_cfg in net_list_cfg.iter_mut() {
                devices.push(self.make_virtio_net_device(net_cfg)?);
            }
        }
        self.config.lock().unwrap().net = net_devices;

        Ok(devices)
    }

    fn make_virtio_rng_devices(&mut self) -> DeviceManagerResult<Vec<MetaVirtioDevice>> {
        let mut devices = Vec::new();

        // Add virtio-rng if required
        let rng_config = self.config.lock().unwrap().rng.clone();
        if let Some(rng_path) = rng_config.src.to_str() {
            info!("Creating virtio-rng device: {:?}", rng_config);
            let id = String::from(RNG_DEVICE_NAME);

            let virtio_rng_device = Arc::new(Mutex::new(
                virtio_devices::Rng::new(
                    id.clone(),
                    rng_path,
                    self.force_iommu | rng_config.iommu,
                    self.seccomp_action.clone(),
                    self.exit_evt
                        .try_clone()
                        .map_err(DeviceManagerError::EventFd)?,
                    state_from_id(self.snapshot.as_ref(), id.as_str())
                        .map_err(DeviceManagerError::RestoreGetState)?,
                )
                .map_err(DeviceManagerError::CreateVirtioRng)?,
            ));
            devices.push(MetaVirtioDevice {
                virtio_device: Arc::clone(&virtio_rng_device)
                    as Arc<Mutex<dyn virtio_devices::VirtioDevice>>,
                iommu: rng_config.iommu,
                id: id.clone(),
                pci_segment: 0,
                dma_handler: None,
            });

            // Fill the device tree with a new node. In case of restore, we
            // know there is nothing to do, so we can simply override the
            // existing entry.
            self.device_tree
                .lock()
                .unwrap()
                .insert(id.clone(), device_node!(id, virtio_rng_device));
        }

        Ok(devices)
    }

    fn make_vhost_virtio_fs_device(
        &mut self,
        id: String,
        mut node: DeviceNode,
        fs_cfg: &FsConfig,
    ) -> DeviceManagerResult<MetaVirtioDevice> {
        let virtio_fs_device;
        if let Some(fs_socket) = fs_cfg.socket.to_str() {
            virtio_fs_device = Arc::new(Mutex::new(
                virtio_devices::vhost_user::Fs::new(
                    id.clone(),
                    fs_socket,
                    &fs_cfg.tag,
                    fs_cfg.num_queues,
                    fs_cfg.queue_size,
                    None,
                    self.seccomp_action.clone(),
                    self.exit_evt
                        .try_clone()
                        .map_err(DeviceManagerError::EventFd)?,
                    self.force_iommu,
                    state_from_id(self.snapshot.as_ref(), id.as_str())
                        .map_err(DeviceManagerError::RestoreGetState)?,
                )
                .map_err(DeviceManagerError::CreateVirtioFs)?,
            ));
        } else {
            return Err(DeviceManagerError::NoVirtioFsSock);
        }

        // Update the device tree with the migratable device.
        node.migratable = Some(Arc::clone(&virtio_fs_device) as Arc<Mutex<dyn Migratable>>);
        self.device_tree.lock().unwrap().insert(id.clone(), node);
        Ok(MetaVirtioDevice {
            virtio_device: Arc::clone(&virtio_fs_device)
                as Arc<Mutex<dyn virtio_devices::VirtioDevice>>,
            iommu: false,
            id,
            pci_segment: fs_cfg.pci_segment,
            dma_handler: None,
        })
    }

    fn make_native_virtio_fs_device(
        &mut self,
        id: String,
        mut node: DeviceNode,
        fs_cfg: &FsConfig,
        bfs_cfg: &BackendFsConfig,
    ) -> DeviceManagerResult<MetaVirtioDevice> {
        let dev_evt = EventFd::new(EFD_NONBLOCK).unwrap();
        let dev_fd = dev_evt.as_raw_fd();
        let pending_dev_message = Arc::new(Mutex::new(Vec::default()));
        let virtio_fs_device = Arc::new(Mutex::new(
            virtio_devices::Fs::new(
                id.clone(),
                &fs_cfg.tag,
                fs_cfg.num_queues,
                fs_cfg.queue_size,
                self.seccomp_action.clone(),
                self.exit_evt
                    .try_clone()
                    .map_err(DeviceManagerError::EventFd)?,
                self.force_iommu,
                state_from_id(self.snapshot.as_ref(), id.as_str())
                    .map_err(DeviceManagerError::RestoreGetState)?,
                bfs_cfg,
                None,
                fs_cfg.rate_limiter_config,
                dev_evt,
                pending_dev_message.clone(),
            )
            .map_err(DeviceManagerError::CreateNativeVirtioFs)?,
        ));

        // Update the device tree with the migratable device.
        node.migratable = Some(Arc::clone(&virtio_fs_device) as Arc<Mutex<dyn Migratable>>);
        node.dev_fd = Some(dev_fd);
        node.pending_dev_message = Some(pending_dev_message);
        self.device_tree.lock().unwrap().insert(id.clone(), node);
        Ok(MetaVirtioDevice {
            virtio_device: Arc::clone(&virtio_fs_device)
                as Arc<Mutex<dyn virtio_devices::VirtioDevice>>,
            iommu: false,
            id,
            pci_segment: fs_cfg.pci_segment,
            dma_handler: None,
        })
    }

    fn make_virtio_fs_device(
        &mut self,
        fs_cfg: &mut FsConfig,
    ) -> DeviceManagerResult<MetaVirtioDevice> {
        let id = if let Some(id) = &fs_cfg.id {
            id.clone()
        } else {
            let id = self.next_device_name(FS_DEVICE_NAME_PREFIX)?;
            fs_cfg.id = Some(id.clone());
            id
        };

        info!("Creating virtio-fs device: {:?}", fs_cfg);

        let node = device_node!(id);

        if let Some(bfs_cfg) = &fs_cfg.backendfs_config {
            self.make_native_virtio_fs_device(id, node, fs_cfg, bfs_cfg)
        } else {
            self.make_vhost_virtio_fs_device(id, node, fs_cfg)
        }
    }

    fn make_virtio_fs_devices(&mut self) -> DeviceManagerResult<Vec<MetaVirtioDevice>> {
        let mut devices = Vec::new();

        let mut fs_devices = self.config.lock().unwrap().fs.clone();
        if let Some(fs_list_cfg) = &mut fs_devices {
            for fs_cfg in fs_list_cfg.iter_mut() {
                devices.push(self.make_virtio_fs_device(fs_cfg)?);
            }
        }
        self.config.lock().unwrap().fs = fs_devices;

        Ok(devices)
    }

    fn make_virtio_pmem_device(
        &mut self,
        pmem_cfg: &mut PmemConfig,
    ) -> DeviceManagerResult<MetaVirtioDevice> {
        let id = if let Some(id) = &pmem_cfg.id {
            id.clone()
        } else {
            let id = self.next_device_name(PMEM_DEVICE_NAME_PREFIX)?;
            pmem_cfg.id = Some(id.clone());
            id
        };

        info!("Creating virtio-pmem device: {:?}", pmem_cfg);

        let mut node = device_node!(id);

        // Look for the id in the device tree. If it can be found, that means
        // the device is being restored, otherwise it's created from scratch.
        let region_range = if let Some(node) = self.device_tree.lock().unwrap().get(&id) {
            debug!("Restoring virtio-pmem {} resources", id);

            let mut region_range: Option<(u64, u64)> = None;
            for resource in node.resources.iter() {
                match resource {
                    Resource::MmioAddressRange { base, size } => {
                        if region_range.is_some() {
                            return Err(DeviceManagerError::ResourceAlreadyExists);
                        }

                        region_range = Some((*base, *size));
                    }
                    _ => {
                        error!("Unexpected resource {:?} for {}", resource, id);
                    }
                }
            }

            if region_range.is_none() {
                return Err(DeviceManagerError::MissingVirtioPmemResources);
            }

            region_range
        } else {
            None
        };

        let (custom_flags, set_len) = if pmem_cfg.file.is_dir() {
            if pmem_cfg.size.is_none() {
                return Err(DeviceManagerError::PmemWithDirectorySizeMissing);
            }
            (O_TMPFILE, true)
        } else {
            (0, false)
        };

        let mut file = OpenOptions::new()
            .read(true)
            .write(!pmem_cfg.discard_writes)
            .custom_flags(custom_flags)
            .open(&pmem_cfg.file)
            .map_err(DeviceManagerError::PmemFileOpen)?;

        let size = if let Some(size) = pmem_cfg.size {
            if set_len {
                file.set_len(size)
                    .map_err(DeviceManagerError::PmemFileSetLen)?;
            }
            size
        } else {
            file.seek(SeekFrom::End(0))
                .map_err(DeviceManagerError::PmemFileSetLen)?
        };

        if size % 0x20_0000 != 0 {
            return Err(DeviceManagerError::PmemSizeNotAligned);
        }

        let (region_base, region_size) = if let Some((base, size)) = region_range {
            // The memory needs to be 2MiB aligned in order to support
            // hugepages.
            self.pci_segments[pmem_cfg.pci_segment as usize]
                .allocator
                .lock()
                .unwrap()
                .allocate(
                    Some(GuestAddress(base)),
                    size as GuestUsize,
                    Some(0x0020_0000),
                )
                .ok_or(DeviceManagerError::PmemRangeAllocation)?;

            (base, size)
        } else {
            // The memory needs to be 2MiB aligned in order to support
            // hugepages.
            let base = self.pci_segments[pmem_cfg.pci_segment as usize]
                .allocator
                .lock()
                .unwrap()
                .allocate(None, size as GuestUsize, Some(0x0020_0000))
                .ok_or(DeviceManagerError::PmemRangeAllocation)?;

            (base.raw_value(), size)
        };

        let cloned_file = file.try_clone().map_err(DeviceManagerError::CloneFile)?;
        let mmap_region = MmapRegion::build(
            Some(FileOffset::new(cloned_file, 0)),
            region_size as usize,
            PROT_READ | PROT_WRITE,
            MAP_NORESERVE
                | if pmem_cfg.discard_writes {
                    MAP_PRIVATE
                } else {
                    MAP_SHARED
                },
        )
        .map_err(DeviceManagerError::NewMmapRegion)?;
        let host_addr: u64 = mmap_region.as_ptr() as u64;

        let mem_slot = self
            .memory_manager
            .lock()
            .unwrap()
            .create_userspace_mapping(region_base, region_size, host_addr, false, false, false)
            .map_err(DeviceManagerError::MemoryManager)?;

        let mapping = virtio_devices::UserspaceMapping {
            host_addr,
            mem_slot,
            addr: GuestAddress(region_base),
            len: region_size,
            mergeable: false,
        };

        let virtio_pmem_device = Arc::new(Mutex::new(
            virtio_devices::Pmem::new(
                id.clone(),
                file,
                GuestAddress(region_base),
                mapping,
                mmap_region,
                self.force_iommu | pmem_cfg.iommu,
                self.seccomp_action.clone(),
                self.exit_evt
                    .try_clone()
                    .map_err(DeviceManagerError::EventFd)?,
                state_from_id(self.snapshot.as_ref(), id.as_str())
                    .map_err(DeviceManagerError::RestoreGetState)?,
            )
            .map_err(DeviceManagerError::CreateVirtioPmem)?,
        ));

        // Update the device tree with correct resource information and with
        // the migratable device.
        node.resources.push(Resource::MmioAddressRange {
            base: region_base,
            size: region_size,
        });
        node.migratable = Some(Arc::clone(&virtio_pmem_device) as Arc<Mutex<dyn Migratable>>);
        self.device_tree.lock().unwrap().insert(id.clone(), node);

        Ok(MetaVirtioDevice {
            virtio_device: Arc::clone(&virtio_pmem_device)
                as Arc<Mutex<dyn virtio_devices::VirtioDevice>>,
            iommu: pmem_cfg.iommu,
            id,
            pci_segment: pmem_cfg.pci_segment,
            dma_handler: None,
        })
    }

    fn make_virtio_pmem_devices(&mut self) -> DeviceManagerResult<Vec<MetaVirtioDevice>> {
        let mut devices = Vec::new();
        // Add virtio-pmem if required
        let mut pmem_devices = self.config.lock().unwrap().pmem.clone();
        if let Some(pmem_list_cfg) = &mut pmem_devices {
            for pmem_cfg in pmem_list_cfg.iter_mut() {
                devices.push(self.make_virtio_pmem_device(pmem_cfg)?);
            }
        }
        self.config.lock().unwrap().pmem = pmem_devices;

        Ok(devices)
    }

    fn make_virtio_vsock_device(
        &mut self,
        vsock_cfg: &mut VsockConfig,
    ) -> DeviceManagerResult<MetaVirtioDevice> {
        let id = if let Some(id) = &vsock_cfg.id {
            id.clone()
        } else {
            let id = self.next_device_name(VSOCK_DEVICE_NAME_PREFIX)?;
            vsock_cfg.id = Some(id.clone());
            id
        };
        let backend_id = format!("{id}-backend");

        info!("Creating virtio-vsock device: {:?}", vsock_cfg);

        let socket_path = vsock_cfg
            .socket
            .to_str()
            .ok_or(DeviceManagerError::CreateVsockConvertPath)?;

        let vsock_snapshot = snapshot_from_id(self.snapshot.as_ref(), id.as_str());
        let backend = virtio_devices::vsock::VsockUnixBackend::new(
            backend_id.clone(),
            vsock_cfg.cid,
            socket_path.to_string(),
            vsock_cfg.muxer_epoll_nested,
            state_from_id(vsock_snapshot.as_ref(), backend_id.as_str())
                .map_err(DeviceManagerError::RestoreGetState)?,
        )
        .map_err(DeviceManagerError::CreateVsockBackend)?;

        let vsock_device = Arc::new(Mutex::new(
            virtio_devices::Vsock::new(
                id.clone(),
                vsock_cfg.cid,
                vsock_cfg.socket.clone(),
                backend,
                self.force_iommu | vsock_cfg.iommu,
                self.seccomp_action.clone(),
                self.exit_evt
                    .try_clone()
                    .map_err(DeviceManagerError::EventFd)?,
                state_from_id(self.snapshot.as_ref(), id.as_str())
                    .map_err(DeviceManagerError::RestoreGetState)?,
            )
            .map_err(DeviceManagerError::CreateVirtioVsock)?,
        ));

        // Fill the device tree with a new node. In case of restore, we
        // know there is nothing to do, so we can simply override the
        // existing entry.
        self.device_tree
            .lock()
            .unwrap()
            .insert(id.clone(), device_node!(id, vsock_device));

        Ok(MetaVirtioDevice {
            virtio_device: Arc::clone(&vsock_device)
                as Arc<Mutex<dyn virtio_devices::VirtioDevice>>,
            iommu: vsock_cfg.iommu,
            id,
            pci_segment: vsock_cfg.pci_segment,
            dma_handler: None,
        })
    }

    fn make_virtio_vsock_devices(&mut self) -> DeviceManagerResult<Vec<MetaVirtioDevice>> {
        let mut devices = Vec::new();

        let mut vsock = self.config.lock().unwrap().vsock.clone();
        if let Some(ref mut vsock_cfg) = &mut vsock {
            devices.push(self.make_virtio_vsock_device(vsock_cfg)?);
        }
        self.config.lock().unwrap().vsock = vsock;

        Ok(devices)
    }

    fn make_virtio_mem_devices(&mut self) -> DeviceManagerResult<Vec<MetaVirtioDevice>> {
        let mut devices = Vec::new();

        let mm = self.memory_manager.clone();
        let mut mm = mm.lock().unwrap();
        for (memory_zone_id, memory_zone) in mm.memory_zones_mut().iter_mut() {
            if let Some(virtio_mem_zone) = memory_zone.virtio_mem_zone_mut() {
                info!("Creating virtio-mem device: id = {}", memory_zone_id);

                let node_id = numa_node_id_from_memory_zone_id(&self.numa_nodes, memory_zone_id)
                    .map(|i| i as u16);

                let virtio_mem_device = Arc::new(Mutex::new(
                    virtio_devices::Mem::new(
                        memory_zone_id.clone(),
                        virtio_mem_zone.region(),
                        self.seccomp_action.clone(),
                        node_id,
                        virtio_mem_zone.hotplugged_size(),
                        virtio_mem_zone.hugepages(),
                        self.exit_evt
                            .try_clone()
                            .map_err(DeviceManagerError::EventFd)?,
                        virtio_mem_zone.blocks_state().clone(),
                        state_from_id(self.snapshot.as_ref(), memory_zone_id.as_str())
                            .map_err(DeviceManagerError::RestoreGetState)?,
                    )
                    .map_err(DeviceManagerError::CreateVirtioMem)?,
                ));

                // Update the virtio-mem zone so that it has a handle onto the
                // virtio-mem device, which will be used for triggering a resize
                // if needed.
                virtio_mem_zone.set_virtio_device(Arc::clone(&virtio_mem_device));

                self.virtio_mem_devices.push(Arc::clone(&virtio_mem_device));

                devices.push(MetaVirtioDevice {
                    virtio_device: Arc::clone(&virtio_mem_device)
                        as Arc<Mutex<dyn virtio_devices::VirtioDevice>>,
                    iommu: false,
                    id: memory_zone_id.clone(),
                    pci_segment: 0,
                    dma_handler: None,
                });

                // Fill the device tree with a new node. In case of restore, we
                // know there is nothing to do, so we can simply override the
                // existing entry.
                self.device_tree.lock().unwrap().insert(
                    memory_zone_id.clone(),
                    device_node!(memory_zone_id, virtio_mem_device),
                );
            }
        }

        Ok(devices)
    }

    fn make_virtio_balloon_devices(&mut self) -> DeviceManagerResult<Vec<MetaVirtioDevice>> {
        let mut devices = Vec::new();

        if let Some(balloon_config) = &self.config.lock().unwrap().balloon {
            let id = String::from(BALLOON_DEVICE_NAME);
            info!("Creating virtio-balloon device: id = {}", id);

            let virtio_balloon_device = Arc::new(Mutex::new(
                virtio_devices::Balloon::new(
                    id.clone(),
                    balloon_config.size,
                    balloon_config.deflate_on_oom,
                    balloon_config.free_page_reporting,
                    self.seccomp_action.clone(),
                    self.exit_evt
                        .try_clone()
                        .map_err(DeviceManagerError::EventFd)?,
                    state_from_id(self.snapshot.as_ref(), id.as_str())
                        .map_err(DeviceManagerError::RestoreGetState)?,
                )
                .map_err(DeviceManagerError::CreateVirtioBalloon)?,
            ));

            self.balloon = Some(virtio_balloon_device.clone());

            devices.push(MetaVirtioDevice {
                virtio_device: Arc::clone(&virtio_balloon_device)
                    as Arc<Mutex<dyn virtio_devices::VirtioDevice>>,
                iommu: false,
                id: id.clone(),
                pci_segment: 0,
                dma_handler: None,
            });

            self.device_tree
                .lock()
                .unwrap()
                .insert(id.clone(), device_node!(id, virtio_balloon_device));
        }

        Ok(devices)
    }

    fn make_virtio_watchdog_devices(&mut self) -> DeviceManagerResult<Vec<MetaVirtioDevice>> {
        let mut devices = Vec::new();

        if !self.config.lock().unwrap().watchdog {
            return Ok(devices);
        }

        let id = String::from(WATCHDOG_DEVICE_NAME);
        info!("Creating virtio-watchdog device: id = {}", id);

        let virtio_watchdog_device = Arc::new(Mutex::new(
            virtio_devices::Watchdog::new(
                id.clone(),
                self.reset_evt.try_clone().unwrap(),
                self.seccomp_action.clone(),
                self.exit_evt
                    .try_clone()
                    .map_err(DeviceManagerError::EventFd)?,
                state_from_id(self.snapshot.as_ref(), id.as_str())
                    .map_err(DeviceManagerError::RestoreGetState)?,
            )
            .map_err(DeviceManagerError::CreateVirtioWatchdog)?,
        ));
        devices.push(MetaVirtioDevice {
            virtio_device: Arc::clone(&virtio_watchdog_device)
                as Arc<Mutex<dyn virtio_devices::VirtioDevice>>,
            iommu: false,
            id: id.clone(),
            pci_segment: 0,
            dma_handler: None,
        });

        self.device_tree
            .lock()
            .unwrap()
            .insert(id.clone(), device_node!(id, virtio_watchdog_device));

        Ok(devices)
    }

    fn make_vdpa_device(
        &mut self,
        vdpa_cfg: &mut VdpaConfig,
    ) -> DeviceManagerResult<MetaVirtioDevice> {
        let id = if let Some(id) = &vdpa_cfg.id {
            id.clone()
        } else {
            let id = self.next_device_name(VDPA_DEVICE_NAME_PREFIX)?;
            vdpa_cfg.id = Some(id.clone());
            id
        };

        info!("Creating vDPA device: {:?}", vdpa_cfg);

        let device_path = vdpa_cfg
            .path
            .to_str()
            .ok_or(DeviceManagerError::CreateVdpaConvertPath)?;

        let vdpa_device = Arc::new(Mutex::new(
            virtio_devices::Vdpa::new(
                id.clone(),
                device_path,
                self.memory_manager.lock().unwrap().guest_memory(),
                vdpa_cfg.num_queues as u16,
                state_from_id(self.snapshot.as_ref(), id.as_str())
                    .map_err(DeviceManagerError::RestoreGetState)?,
            )
            .map_err(DeviceManagerError::CreateVdpa)?,
        ));

        // Create the DMA handler that is required by the vDPA device
        let vdpa_mapping = Arc::new(VdpaDmaMapping::new(
            Arc::clone(&vdpa_device),
            Arc::new(self.memory_manager.lock().unwrap().guest_memory()),
        ));

        self.device_tree
            .lock()
            .unwrap()
            .insert(id.clone(), device_node!(id, vdpa_device));

        Ok(MetaVirtioDevice {
            virtio_device: vdpa_device as Arc<Mutex<dyn virtio_devices::VirtioDevice>>,
            iommu: vdpa_cfg.iommu,
            id,
            pci_segment: vdpa_cfg.pci_segment,
            dma_handler: Some(vdpa_mapping),
        })
    }

    fn make_vdpa_devices(&mut self) -> DeviceManagerResult<Vec<MetaVirtioDevice>> {
        let mut devices = Vec::new();
        // Add vdpa if required
        let mut vdpa_devices = self.config.lock().unwrap().vdpa.clone();
        if let Some(vdpa_list_cfg) = &mut vdpa_devices {
            for vdpa_cfg in vdpa_list_cfg.iter_mut() {
                devices.push(self.make_vdpa_device(vdpa_cfg)?);
            }
        }
        self.config.lock().unwrap().vdpa = vdpa_devices;

        Ok(devices)
    }

    fn next_device_name(&mut self, prefix: &str) -> DeviceManagerResult<String> {
        let start_id = self.device_id_cnt;
        loop {
            // Generate the temporary name.
            let name = format!("{}{}", prefix, self.device_id_cnt);
            // Increment the counter.
            self.device_id_cnt += Wrapping(1);
            // Check if the name is already in use.
            if !self.boot_id_list.contains(&name)
                && !self.device_tree.lock().unwrap().contains_key(&name)
            {
                return Ok(name);
            }

            if self.device_id_cnt == start_id {
                // We went through a full loop and there's nothing else we can
                // do.
                break;
            }
        }
        Err(DeviceManagerError::NoAvailableDeviceName)
    }

    fn add_passthrough_device(
        &mut self,
        device_cfg: &mut DeviceConfig,
    ) -> DeviceManagerResult<(PciBdf, String)> {
        // If the passthrough device has not been created yet, it is created
        // here and stored in the DeviceManager structure for future needs.
        if self.passthrough_device.is_none() {
            self.passthrough_device = Some(
                self.address_manager
                    .vm
                    .create_passthrough_device()
                    .map_err(|e| DeviceManagerError::CreatePassthroughDevice(e.into()))?,
            );
        }

        self.add_vfio_device(device_cfg)
    }

    fn create_vfio_container(&self) -> DeviceManagerResult<Arc<VfioContainer>> {
        let passthrough_device = self
            .passthrough_device
            .as_ref()
            .ok_or(DeviceManagerError::NoDevicePassthroughSupport)?;

        let dup = passthrough_device
            .try_clone()
            .map_err(DeviceManagerError::VfioCreate)?;

        Ok(Arc::new(
            VfioContainer::new(Some(Arc::new(dup))).map_err(DeviceManagerError::VfioCreate)?,
        ))
    }

    fn add_vfio_device(
        &mut self,
        device_cfg: &mut DeviceConfig,
    ) -> DeviceManagerResult<(PciBdf, String)> {
        let vfio_name = if let Some(id) = &device_cfg.id {
            id.clone()
        } else {
            let id = self.next_device_name(VFIO_DEVICE_NAME_PREFIX)?;
            device_cfg.id = Some(id.clone());
            id
        };

        let (pci_segment_id, pci_device_bdf, resources) =
            self.pci_resources(&vfio_name, device_cfg.pci_segment)?;

        let mut needs_dma_mapping = false;

        // Here we create a new VFIO container for two reasons. Either this is
        // the first VFIO device, meaning we need a new VFIO container, which
        // will be shared with other VFIO devices. Or the new VFIO device is
        // attached to a vIOMMU, meaning we must create a dedicated VFIO
        // container. In the vIOMMU use case, we can't let all devices under
        // the same VFIO container since we couldn't map/unmap memory for each
        // device. That's simply because the map/unmap operations happen at the
        // VFIO container level.
        let vfio_container = if device_cfg.iommu {
            let vfio_container = self.create_vfio_container()?;

            let vfio_mapping = Arc::new(VfioDmaMapping::new(
                Arc::clone(&vfio_container),
                Arc::new(self.memory_manager.lock().unwrap().guest_memory()),
            ));

            if let Some(iommu) = &self.iommu_device {
                iommu
                    .lock()
                    .unwrap()
                    .add_external_mapping(pci_device_bdf.into(), vfio_mapping);
            } else {
                return Err(DeviceManagerError::MissingVirtualIommu);
            }

            vfio_container
        } else if let Some(vfio_container) = &self.vfio_container {
            Arc::clone(vfio_container)
        } else {
            let vfio_container = self.create_vfio_container()?;
            needs_dma_mapping = true;
            self.vfio_container = Some(Arc::clone(&vfio_container));

            vfio_container
        };

        let vfio_device = VfioDevice::new(&device_cfg.path, Arc::clone(&vfio_container))
            .map_err(DeviceManagerError::VfioCreate)?;

        if needs_dma_mapping {
            // Register DMA mapping in IOMMU.
            // Do not register virtio-mem regions, as they are handled directly by
            // virtio-mem device itself.
            for (_, zone) in self.memory_manager.lock().unwrap().memory_zones().iter() {
                for region in zone.regions() {
                    vfio_container
                        .vfio_dma_map(
                            region.start_addr().raw_value(),
                            region.len(),
                            region.as_ptr() as u64,
                        )
                        .map_err(DeviceManagerError::VfioDmaMap)?;
                }
            }

            let vfio_mapping = Arc::new(VfioDmaMapping::new(
                Arc::clone(&vfio_container),
                Arc::new(self.memory_manager.lock().unwrap().guest_memory()),
            ));

            for virtio_mem_device in self.virtio_mem_devices.iter() {
                virtio_mem_device
                    .lock()
                    .unwrap()
                    .add_dma_mapping_handler(
                        VirtioMemMappingSource::Container,
                        vfio_mapping.clone(),
                    )
                    .map_err(DeviceManagerError::AddDmaMappingHandlerVirtioMem)?;
            }
        }

        let legacy_interrupt_group =
            if let Some(legacy_interrupt_manager) = &self.legacy_interrupt_manager {
                Some(
                    legacy_interrupt_manager
                        .create_group(LegacyIrqGroupConfig {
                            irq: self.pci_segments[pci_segment_id as usize].pci_irq_slots
                                [pci_device_bdf.device() as usize]
                                as InterruptIndex,
                        })
                        .map_err(DeviceManagerError::CreateInterruptGroup)?,
                )
            } else {
                None
            };

        let memory_manager = self.memory_manager.clone();

        let vfio_pci_device = VfioPciDevice::new(
            vfio_name.clone(),
            &self.address_manager.vm,
            vfio_device,
            vfio_container,
            self.msi_interrupt_manager.clone(),
            legacy_interrupt_group,
            device_cfg.iommu,
            pci_device_bdf,
            self.restoring,
            Arc::new(move || memory_manager.lock().unwrap().allocate_memory_slot()),
        )
        .map_err(DeviceManagerError::VfioPciCreate)?;

        let vfio_pci_device = Arc::new(Mutex::new(vfio_pci_device));

        let new_resources = self.add_pci_device(
            vfio_pci_device.clone(),
            vfio_pci_device.clone(),
            pci_segment_id,
            pci_device_bdf,
            resources,
        )?;

        // When restoring a VM, the restore codepath will take care of mapping
        // the MMIO regions based on the information from the snapshot.
        if !self.restoring {
            vfio_pci_device
                .lock()
                .unwrap()
                .map_mmio_regions()
                .map_err(DeviceManagerError::VfioMapRegion)?;
        }

        let mut node = device_node!(vfio_name, vfio_pci_device);

        // Update the device tree with correct resource information.
        node.resources = new_resources;
        node.pci_bdf = Some(pci_device_bdf);
        node.pci_device_handle = Some(PciDeviceHandle::Vfio(vfio_pci_device));

        self.device_tree
            .lock()
            .unwrap()
            .insert(vfio_name.clone(), node);

        Ok((pci_device_bdf, vfio_name))
    }

    fn add_pci_device(
        &mut self,
        bus_device: Arc<Mutex<dyn BusDevice>>,
        pci_device: Arc<Mutex<dyn PciDevice>>,
        segment_id: u16,
        bdf: PciBdf,
        resources: Option<Vec<Resource>>,
    ) -> DeviceManagerResult<Vec<Resource>> {
        let bars = pci_device
            .lock()
            .unwrap()
            .allocate_bars(
                &self.address_manager.allocator,
                &mut self.pci_segments[segment_id as usize]
                    .allocator
                    .lock()
                    .unwrap(),
                resources,
            )
            .map_err(DeviceManagerError::AllocateBars)?;

        let mut pci_bus = self.pci_segments[segment_id as usize]
            .pci_bus
            .lock()
            .unwrap();

        pci_bus
            .add_device(bdf.device() as u32, pci_device)
            .map_err(DeviceManagerError::AddPciDevice)?;

        self.bus_devices.push(Arc::clone(&bus_device));

        pci_bus
            .register_mapping(
                bus_device,
                #[cfg(target_arch = "x86_64")]
                self.address_manager.io_bus.as_ref(),
                self.address_manager.mmio_bus.as_ref(),
                bars.clone(),
            )
            .map_err(DeviceManagerError::AddPciDevice)?;

        let mut new_resources = Vec::new();
        for bar in bars {
            new_resources.push(Resource::PciBar {
                index: bar.idx(),
                base: bar.addr(),
                size: bar.size(),
                type_: bar.region_type().into(),
                prefetchable: bar.prefetchable().into(),
            });
        }

        Ok(new_resources)
    }

    fn add_vfio_devices(&mut self) -> DeviceManagerResult<Vec<PciBdf>> {
        let mut iommu_attached_device_ids = Vec::new();
        let mut devices = self.config.lock().unwrap().devices.clone();

        if let Some(device_list_cfg) = &mut devices {
            for device_cfg in device_list_cfg.iter_mut() {
                let (device_id, _) = self.add_passthrough_device(device_cfg)?;
                if device_cfg.iommu && self.iommu_device.is_some() {
                    iommu_attached_device_ids.push(device_id);
                }
            }
        }

        // Update the list of devices
        self.config.lock().unwrap().devices = devices;

        Ok(iommu_attached_device_ids)
    }

    fn add_vfio_user_device(
        &mut self,
        device_cfg: &mut UserDeviceConfig,
    ) -> DeviceManagerResult<(PciBdf, String)> {
        let vfio_user_name = if let Some(id) = &device_cfg.id {
            id.clone()
        } else {
            let id = self.next_device_name(VFIO_USER_DEVICE_NAME_PREFIX)?;
            device_cfg.id = Some(id.clone());
            id
        };

        let (pci_segment_id, pci_device_bdf, resources) =
            self.pci_resources(&vfio_user_name, device_cfg.pci_segment)?;

        let legacy_interrupt_group =
            if let Some(legacy_interrupt_manager) = &self.legacy_interrupt_manager {
                Some(
                    legacy_interrupt_manager
                        .create_group(LegacyIrqGroupConfig {
                            irq: self.pci_segments[pci_segment_id as usize].pci_irq_slots
                                [pci_device_bdf.device() as usize]
                                as InterruptIndex,
                        })
                        .map_err(DeviceManagerError::CreateInterruptGroup)?,
                )
            } else {
                None
            };

        let client = Arc::new(Mutex::new(
            vfio_user::Client::new(&device_cfg.socket)
                .map_err(DeviceManagerError::VfioUserCreateClient)?,
        ));

        let memory_manager = self.memory_manager.clone();

        let mut vfio_user_pci_device = VfioUserPciDevice::new(
            vfio_user_name.clone(),
            &self.address_manager.vm,
            client.clone(),
            self.msi_interrupt_manager.clone(),
            legacy_interrupt_group,
            pci_device_bdf,
            self.restoring,
            Arc::new(move || memory_manager.lock().unwrap().allocate_memory_slot()),
        )
        .map_err(DeviceManagerError::VfioUserCreate)?;

        let memory = self.memory_manager.lock().unwrap().guest_memory();
        let vfio_user_mapping = Arc::new(VfioUserDmaMapping::new(client, Arc::new(memory)));
        for virtio_mem_device in self.virtio_mem_devices.iter() {
            virtio_mem_device
                .lock()
                .unwrap()
                .add_dma_mapping_handler(
                    VirtioMemMappingSource::Device(pci_device_bdf.into()),
                    vfio_user_mapping.clone(),
                )
                .map_err(DeviceManagerError::AddDmaMappingHandlerVirtioMem)?;
        }

        for (_, zone) in self.memory_manager.lock().unwrap().memory_zones().iter() {
            for region in zone.regions() {
                vfio_user_pci_device
                    .dma_map(region)
                    .map_err(DeviceManagerError::VfioUserDmaMap)?;
            }
        }

        let vfio_user_pci_device = Arc::new(Mutex::new(vfio_user_pci_device));

        let new_resources = self.add_pci_device(
            vfio_user_pci_device.clone(),
            vfio_user_pci_device.clone(),
            pci_segment_id,
            pci_device_bdf,
            resources,
        )?;

        // When restoring a VM, the restore codepath will take care of mapping
        // the MMIO regions based on the information from the snapshot.
        if !self.restoring {
            // Note it is required to call 'add_pci_device()' in advance to have the list of
            // mmio regions provisioned correctly
            vfio_user_pci_device
                .lock()
                .unwrap()
                .map_mmio_regions()
                .map_err(DeviceManagerError::VfioUserMapRegion)?;
        }

        let mut node = device_node!(vfio_user_name, vfio_user_pci_device);

        // Update the device tree with correct resource information.
        node.resources = new_resources;
        node.pci_bdf = Some(pci_device_bdf);
        node.pci_device_handle = Some(PciDeviceHandle::VfioUser(vfio_user_pci_device));

        self.device_tree
            .lock()
            .unwrap()
            .insert(vfio_user_name.clone(), node);

        Ok((pci_device_bdf, vfio_user_name))
    }

    fn add_user_devices(&mut self) -> DeviceManagerResult<Vec<PciBdf>> {
        let mut user_devices = self.config.lock().unwrap().user_devices.clone();

        if let Some(device_list_cfg) = &mut user_devices {
            for device_cfg in device_list_cfg.iter_mut() {
                let (_device_id, _id) = self.add_vfio_user_device(device_cfg)?;
            }
        }

        // Update the list of devices
        self.config.lock().unwrap().user_devices = user_devices;

        Ok(vec![])
    }

    fn add_virtio_pci_device(
        &mut self,
        virtio_device: Arc<Mutex<dyn virtio_devices::VirtioDevice>>,
        iommu_mapping: &Option<Arc<IommuMapping>>,
        virtio_device_id: String,
        pci_segment_id: u16,
        dma_handler: Option<Arc<dyn ExternalDmaMapping>>,
    ) -> DeviceManagerResult<PciBdf> {
        let id = format!("{}-{}", VIRTIO_PCI_DEVICE_NAME_PREFIX, virtio_device_id);

        // Add the new virtio-pci node to the device tree.
        let mut node = device_node!(id);
        node.children = vec![virtio_device_id.clone()];

        let (pci_segment_id, pci_device_bdf, resources) =
            self.pci_resources(&id, pci_segment_id)?;

        // Update the existing virtio node by setting the parent.
        if let Some(node) = self.device_tree.lock().unwrap().get_mut(&virtio_device_id) {
            node.parent = Some(id.clone());
        } else {
            return Err(DeviceManagerError::MissingNode);
        }

        // Allows support for one MSI-X vector per queue. It also adds 1
        // as we need to take into account the dedicated vector to notify
        // about a virtio config change.
        let msix_num = (virtio_device.lock().unwrap().queue_max_sizes().len() + 1) as u16;

        // Create the AccessPlatform trait from the implementation IommuMapping.
        // This will provide address translation for any virtio device sitting
        // behind a vIOMMU.
        let access_platform: Option<Arc<dyn AccessPlatform>> = if let Some(mapping) = iommu_mapping
        {
            Some(Arc::new(AccessPlatformMapping::new(
                pci_device_bdf.into(),
                mapping.clone(),
            )))
        } else {
            None
        };

        let memory = self.memory_manager.lock().unwrap().guest_memory();

        // Map DMA ranges if a DMA handler is available and if the device is
        // not attached to a virtual IOMMU.
        if let Some(dma_handler) = &dma_handler {
            if iommu_mapping.is_some() {
                if let Some(iommu) = &self.iommu_device {
                    iommu
                        .lock()
                        .unwrap()
                        .add_external_mapping(pci_device_bdf.into(), dma_handler.clone());
                } else {
                    return Err(DeviceManagerError::MissingVirtualIommu);
                }
            } else {
                // Let every virtio-mem device handle the DMA map/unmap through the
                // DMA handler provided.
                for virtio_mem_device in self.virtio_mem_devices.iter() {
                    virtio_mem_device
                        .lock()
                        .unwrap()
                        .add_dma_mapping_handler(
                            VirtioMemMappingSource::Device(pci_device_bdf.into()),
                            dma_handler.clone(),
                        )
                        .map_err(DeviceManagerError::AddDmaMappingHandlerVirtioMem)?;
                }

                // Do not register virtio-mem regions, as they are handled directly by
                // virtio-mem devices.
                for (_, zone) in self.memory_manager.lock().unwrap().memory_zones().iter() {
                    for region in zone.regions() {
                        let gpa = region.start_addr().0;
                        let size = region.len();
                        dma_handler
                            .map(gpa, gpa, size)
                            .map_err(DeviceManagerError::VirtioDmaMap)?;
                    }
                }
            }
        }

        let device_type = virtio_device.lock().unwrap().device_type();
        let virtio_pci_device = Arc::new(Mutex::new(
            VirtioPciDevice::new(
                id.clone(),
                memory,
                virtio_device,
                msix_num,
                access_platform,
                &self.msi_interrupt_manager,
                pci_device_bdf.into(),
                self.activate_evt
                    .try_clone()
                    .map_err(DeviceManagerError::EventFd)?,
                // All device types *except* virtio block devices should be allocated a 64-bit bar
                // The block devices should be given a 32-bit BAR so that they are easily accessible
                // to firmware without requiring excessive identity mapping.
                // The exception being if not on the default PCI segment.
                pci_segment_id > 0 || device_type != VirtioDeviceType::Block as u32,
                dma_handler,
                self.pending_activations.clone(),
            )
            .map_err(DeviceManagerError::VirtioDevice)?,
        ));

        let new_resources = self.add_pci_device(
            virtio_pci_device.clone(),
            virtio_pci_device.clone(),
            pci_segment_id,
            pci_device_bdf,
            resources,
        )?;

        let bar_addr = virtio_pci_device.lock().unwrap().config_bar_addr();
        for (event, addr) in virtio_pci_device.lock().unwrap().ioeventfds(bar_addr) {
            let io_addr = IoEventAddress::Mmio(addr);
            self.address_manager
                .vm
                .register_ioevent(event, &io_addr, None)
                .map_err(|e| DeviceManagerError::RegisterIoevent(e.into()))?;
        }

        // Update the device tree with correct resource information.
        node.resources = new_resources;
        node.migratable = Some(Arc::clone(&virtio_pci_device) as Arc<Mutex<dyn Migratable>>);
        node.pci_bdf = Some(pci_device_bdf);
        node.pci_device_handle = Some(PciDeviceHandle::Virtio(virtio_pci_device));
        self.device_tree.lock().unwrap().insert(id, node);

        Ok(pci_device_bdf)
    }

    fn add_pvpanic_device(
        &mut self,
    ) -> DeviceManagerResult<Option<Arc<Mutex<devices::PvPanicDevice>>>> {
        let id = String::from(PVPANIC_DEVICE_NAME);
        let pci_segment_id = 0x0_u16;

        info!("Creating pvpanic device {}", id);

        let (pci_segment_id, pci_device_bdf, resources) =
            self.pci_resources(&id, pci_segment_id)?;

        let pvpanic_device = devices::PvPanicDevice::new(
            id.clone(),
            state_from_id(self.snapshot.as_ref(), id.as_str())
                .map_err(DeviceManagerError::RestoreGetState)?,
        )
        .map_err(DeviceManagerError::PvPanicCreate)?;

        let pvpanic_device = Arc::new(Mutex::new(pvpanic_device));

        let new_resources = self.add_pci_device(
            pvpanic_device.clone(),
            pvpanic_device.clone(),
            pci_segment_id,
            pci_device_bdf,
            resources,
        )?;

        let mut node = device_node!(id, pvpanic_device);

        node.resources = new_resources;
        node.pci_bdf = Some(pci_device_bdf);
        node.pci_device_handle = None;

        self.device_tree.lock().unwrap().insert(id, node);

        Ok(Some(pvpanic_device))
    }

    fn add_ivshmem_device(
        &mut self,
        ivshmem_cfg: &IvshmemConfig,
    ) -> DeviceManagerResult<Option<Arc<Mutex<devices::IvshmemDevice>>>> {
        let id = String::from(IVSHMEM_DEVICE_NAME);
        let pci_segment_id = 0x0_u16;
        info!("Creating ivshmem device {}", id);

        let (pci_segment_id, pci_device_bdf, resources) =
            self.pci_resources(&id, pci_segment_id)?;

        let ivshmem_device = Arc::new(Mutex::new(devices::IvshmemDevice::new(
            id.clone(),
            state_from_id(self.snapshot.as_ref(), id.as_str())
                .map_err(DeviceManagerError::RestoreGetState)?,
            ivshmem_cfg.size as u64,
        )));
        let new_resources = self.add_pci_device(
            ivshmem_device.clone(),
            ivshmem_device.clone(),
            pci_segment_id,
            pci_device_bdf,
            resources,
        )?;

        let start_addr = ivshmem_device.lock().unwrap().data_bar_addr();
        let region = MemoryManager::create_ram_region(
            &Some(ivshmem_cfg.path.clone()),
            0,
            GuestAddress(start_addr),
            ivshmem_cfg.size,
            false,
            true,
            false,
            None,
            None,
            None,
            None,
            0,
            false,
        )
        .map_err(DeviceManagerError::MemoryManager)?;
        let mem_slot = self
            .memory_manager
            .lock()
            .unwrap()
            .create_userspace_mapping(
                region.start_addr().0,
                region.len(),
                region.as_ptr() as u64,
                false,
                false,
                false,
            )
            .map_err(DeviceManagerError::MemoryManager)?;
        let _mapping = virtio_devices::UserspaceMapping {
            host_addr: region.as_ptr() as u64,
            mem_slot,
            addr: GuestAddress(region.start_addr().0),
            len: region.len(),
            mergeable: false,
        };
        ivshmem_device.lock().unwrap().assign_region(region);

        let mut node = device_node!(id, ivshmem_device);
        node.resources = new_resources;
        node.pci_bdf = Some(pci_device_bdf);
        node.pci_device_handle = None;
        self.device_tree.lock().unwrap().insert(id, node);

        Ok(Some(ivshmem_device))
    }


    fn pci_resources(
        &self,
        id: &str,
        pci_segment_id: u16,
    ) -> DeviceManagerResult<(u16, PciBdf, Option<Vec<Resource>>)> {
        // Look for the id in the device tree. If it can be found, that means
        // the device is being restored, otherwise it's created from scratch.
        Ok(
            if let Some(node) = self.device_tree.lock().unwrap().get(id) {
                debug!("Restoring virtio-pci {} resources", id);
                let pci_device_bdf: PciBdf = node
                    .pci_bdf
                    .ok_or(DeviceManagerError::MissingDeviceNodePciBdf)?;
                let pci_segment_id = pci_device_bdf.segment();

                self.pci_segments[pci_segment_id as usize]
                    .pci_bus
                    .lock()
                    .unwrap()
                    .get_device_id(pci_device_bdf.device() as usize)
                    .map_err(DeviceManagerError::GetPciDeviceId)?;

                (pci_segment_id, pci_device_bdf, Some(node.resources.clone()))
            } else {
                let pci_device_bdf =
                    self.pci_segments[pci_segment_id as usize].next_device_bdf()?;

                (pci_segment_id, pci_device_bdf, None)
            },
        )
    }

    #[cfg(target_arch = "x86_64")]
    pub fn io_bus(&self) -> &Arc<Bus> {
        &self.address_manager.io_bus
    }

    pub fn mmio_bus(&self) -> &Arc<Bus> {
        &self.address_manager.mmio_bus
    }

    pub fn allocator(&self) -> &Arc<Mutex<SystemAllocator>> {
        &self.address_manager.allocator
    }

    pub fn interrupt_controller(&self) -> Option<Arc<Mutex<dyn InterruptController>>> {
        self.interrupt_controller
            .as_ref()
            .map(|ic| ic.clone() as Arc<Mutex<dyn InterruptController>>)
    }

    #[cfg(target_arch = "x86_64")]
    // Used to provide a fast path for handling PIO exits
    pub fn pci_config_io(&self) -> Arc<Mutex<PciConfigIo>> {
        Arc::clone(self.pci_segments[0].pci_config_io.as_ref().unwrap())
    }

    pub(crate) fn pci_segments(&self) -> &Vec<PciSegment> {
        &self.pci_segments
    }

    pub fn console(&self) -> &Arc<Console> {
        &self.console
    }

    #[cfg(target_arch = "aarch64")]
    pub fn cmdline_additions(&self) -> &[String] {
        self.cmdline_additions.as_slice()
    }

    pub fn update_memory(&self, new_region: &Arc<GuestRegionMmap>) -> DeviceManagerResult<()> {
        for handle in self.virtio_devices.iter() {
            handle
                .virtio_device
                .lock()
                .unwrap()
                .add_memory_region(new_region)
                .map_err(DeviceManagerError::UpdateMemoryForVirtioDevice)?;

            if let Some(dma_handler) = &handle.dma_handler {
                if !handle.iommu {
                    let gpa = new_region.start_addr().0;
                    let size = new_region.len();
                    dma_handler
                        .map(gpa, gpa, size)
                        .map_err(DeviceManagerError::VirtioDmaMap)?;
                }
            }
        }

        // Take care of updating the memory for VFIO PCI devices.
        if let Some(vfio_container) = &self.vfio_container {
            vfio_container
                .vfio_dma_map(
                    new_region.start_addr().raw_value(),
                    new_region.len(),
                    new_region.as_ptr() as u64,
                )
                .map_err(DeviceManagerError::UpdateMemoryForVfioPciDevice)?;
        }

        // Take care of updating the memory for vfio-user devices.
        {
            let device_tree = self.device_tree.lock().unwrap();
            for pci_device_node in device_tree.pci_devices() {
                if let PciDeviceHandle::VfioUser(vfio_user_pci_device) = pci_device_node
                    .pci_device_handle
                    .as_ref()
                    .ok_or(DeviceManagerError::MissingPciDevice)?
                {
                    vfio_user_pci_device
                        .lock()
                        .unwrap()
                        .dma_map(new_region)
                        .map_err(DeviceManagerError::UpdateMemoryForVfioUserPciDevice)?;
                }
            }
        }

        Ok(())
    }

    pub fn activate_virtio_devices(&self) -> DeviceManagerResult<()> {
        for mut activator in self.pending_activations.lock().unwrap().drain(..) {
            activator
                .activate()
                .map_err(DeviceManagerError::VirtioActivate)?;
        }
        Ok(())
    }

    pub fn notify_hotplug(
        &self,
        _notification_type: AcpiNotificationFlags,
    ) -> DeviceManagerResult<()> {
        return self
            .ged_notification_device
            .as_ref()
            .unwrap()
            .lock()
            .unwrap()
            .notify(_notification_type)
            .map_err(DeviceManagerError::HotPlugNotification);
    }

    pub fn add_device(
        &mut self,
        device_cfg: &mut DeviceConfig,
    ) -> DeviceManagerResult<PciDeviceInfo> {
        self.validate_identifier(&device_cfg.id)?;

        if device_cfg.iommu && !self.is_iommu_segment(device_cfg.pci_segment) {
            return Err(DeviceManagerError::InvalidIommuHotplug);
        }

        let (bdf, device_name) = self.add_passthrough_device(device_cfg)?;

        // Update the PCIU bitmap
        self.pci_segments[device_cfg.pci_segment as usize].pci_devices_up |= 1 << bdf.device();

        Ok(PciDeviceInfo {
            id: device_name,
            bdf,
        })
    }

    pub fn add_user_device(
        &mut self,
        device_cfg: &mut UserDeviceConfig,
    ) -> DeviceManagerResult<PciDeviceInfo> {
        self.validate_identifier(&device_cfg.id)?;

        let (bdf, device_name) = self.add_vfio_user_device(device_cfg)?;

        // Update the PCIU bitmap
        self.pci_segments[device_cfg.pci_segment as usize].pci_devices_up |= 1 << bdf.device();

        Ok(PciDeviceInfo {
            id: device_name,
            bdf,
        })
    }

    pub fn remove_device(&mut self, id: String) -> DeviceManagerResult<()> {
        // The node can be directly a PCI node in case the 'id' refers to a
        // VFIO device or a virtio-pci one.
        // In case the 'id' refers to a virtio device, we must find the PCI
        // node by looking at the parent.
        let device_tree = self.device_tree.lock().unwrap();
        let node = device_tree
            .get(&id)
            .ok_or(DeviceManagerError::UnknownDeviceId(id))?;

        let pci_device_node = if node.pci_bdf.is_some() && node.pci_device_handle.is_some() {
            node
        } else {
            let parent = node
                .parent
                .as_ref()
                .ok_or(DeviceManagerError::MissingNode)?;
            device_tree
                .get(parent)
                .ok_or(DeviceManagerError::MissingNode)?
        };

        let pci_device_bdf: PciBdf = pci_device_node
            .pci_bdf
            .ok_or(DeviceManagerError::MissingDeviceNodePciBdf)?;
        let pci_segment_id = pci_device_bdf.segment();

        let pci_device_handle = pci_device_node
            .pci_device_handle
            .as_ref()
            .ok_or(DeviceManagerError::MissingPciDevice)?;
        #[allow(irrefutable_let_patterns)]
        if let PciDeviceHandle::Virtio(virtio_pci_device) = pci_device_handle {
            let device_type = VirtioDeviceType::from(
                virtio_pci_device
                    .lock()
                    .unwrap()
                    .virtio_device()
                    .lock()
                    .unwrap()
                    .device_type(),
            );
            match device_type {
                VirtioDeviceType::Net
                | VirtioDeviceType::Block
                | VirtioDeviceType::Pmem
                | VirtioDeviceType::Fs
                | VirtioDeviceType::Vsock => {}
                _ => return Err(DeviceManagerError::RemovalNotAllowed(device_type)),
            }
        }

        // Update the PCID bitmap
        self.pci_segments[pci_segment_id as usize].pci_devices_down |= 1 << pci_device_bdf.device();

        Ok(())
    }

    pub fn eject_device(&mut self, pci_segment_id: u16, device_id: u8) -> DeviceManagerResult<()> {
        info!(
            "Ejecting device_id = {} on segment_id={}",
            device_id, pci_segment_id
        );

        // Convert the device ID into the corresponding b/d/f.
        let pci_device_bdf = PciBdf::new(pci_segment_id, 0, device_id, 0);

        // Give the PCI device ID back to the PCI bus.
        self.pci_segments[pci_segment_id as usize]
            .pci_bus
            .lock()
            .unwrap()
            .put_device_id(device_id as usize)
            .map_err(DeviceManagerError::PutPciDeviceId)?;

        // Remove the device from the device tree along with its children.
        let mut device_tree = self.device_tree.lock().unwrap();
        let pci_device_node = device_tree
            .remove_node_by_pci_bdf(pci_device_bdf)
            .ok_or(DeviceManagerError::MissingPciDevice)?;

        // For VFIO and vfio-user the PCI device id is the id.
        // For virtio we overwrite it later as we want the id of the
        // underlying device.
        let mut id = pci_device_node.id;
        let pci_device_handle = pci_device_node
            .pci_device_handle
            .ok_or(DeviceManagerError::MissingPciDevice)?;
        if matches!(pci_device_handle, PciDeviceHandle::Virtio(_)) {
            // The virtio-pci device has a single child
            if !pci_device_node.children.is_empty() {
                assert_eq!(pci_device_node.children.len(), 1);
                let child_id = &pci_device_node.children[0];
                id = child_id.clone();
            }
        }
        for child in pci_device_node.children.iter() {
            device_tree.remove(child);
        }

        let mut iommu_attached = false;
        if let Some((_, iommu_attached_devices)) = &self.iommu_attached_devices {
            if iommu_attached_devices.contains(&pci_device_bdf) {
                iommu_attached = true;
            }
        }

        let (pci_device, bus_device, virtio_device, remove_dma_handler) = match pci_device_handle {
            // No need to remove any virtio-mem mapping here as the container outlives all devices
            PciDeviceHandle::Vfio(vfio_pci_device) => (
                Arc::clone(&vfio_pci_device) as Arc<Mutex<dyn PciDevice>>,
                Arc::clone(&vfio_pci_device) as Arc<Mutex<dyn BusDevice>>,
                None as Option<Arc<Mutex<dyn virtio_devices::VirtioDevice>>>,
                false,
            ),
            PciDeviceHandle::Virtio(virtio_pci_device) => {
                let dev = virtio_pci_device.lock().unwrap();
                let bar_addr = dev.config_bar_addr();
                for (event, addr) in dev.ioeventfds(bar_addr) {
                    let io_addr = IoEventAddress::Mmio(addr);
                    self.address_manager
                        .vm
                        .unregister_ioevent(event, &io_addr)
                        .map_err(|e| DeviceManagerError::UnRegisterIoevent(e.into()))?;
                }

                if let Some(dma_handler) = dev.dma_handler() {
                    if !iommu_attached {
                        for (_, zone) in self.memory_manager.lock().unwrap().memory_zones().iter() {
                            for region in zone.regions() {
                                let iova = region.start_addr().0;
                                let size = region.len();
                                dma_handler
                                    .unmap(iova, size)
                                    .map_err(DeviceManagerError::VirtioDmaUnmap)?;
                            }
                        }
                    }
                }

                (
                    Arc::clone(&virtio_pci_device) as Arc<Mutex<dyn PciDevice>>,
                    Arc::clone(&virtio_pci_device) as Arc<Mutex<dyn BusDevice>>,
                    Some(dev.virtio_device()),
                    dev.dma_handler().is_some() && !iommu_attached,
                )
            }
            PciDeviceHandle::VfioUser(vfio_user_pci_device) => {
                let mut dev = vfio_user_pci_device.lock().unwrap();
                for (_, zone) in self.memory_manager.lock().unwrap().memory_zones().iter() {
                    for region in zone.regions() {
                        dev.dma_unmap(region)
                            .map_err(DeviceManagerError::VfioUserDmaUnmap)?;
                    }
                }

                (
                    Arc::clone(&vfio_user_pci_device) as Arc<Mutex<dyn PciDevice>>,
                    Arc::clone(&vfio_user_pci_device) as Arc<Mutex<dyn BusDevice>>,
                    None as Option<Arc<Mutex<dyn virtio_devices::VirtioDevice>>>,
                    true,
                )
            }
        };

        if remove_dma_handler {
            for virtio_mem_device in self.virtio_mem_devices.iter() {
                virtio_mem_device
                    .lock()
                    .unwrap()
                    .remove_dma_mapping_handler(VirtioMemMappingSource::Device(
                        pci_device_bdf.into(),
                    ))
                    .map_err(DeviceManagerError::RemoveDmaMappingHandlerVirtioMem)?;
            }
        }

        // Free the allocated BARs
        pci_device
            .lock()
            .unwrap()
            .free_bars(
                &mut self.address_manager.allocator.lock().unwrap(),
                &mut self.pci_segments[pci_segment_id as usize]
                    .allocator
                    .lock()
                    .unwrap(),
            )
            .map_err(DeviceManagerError::FreePciBars)?;

        // Remove the device from the PCI bus
        self.pci_segments[pci_segment_id as usize]
            .pci_bus
            .lock()
            .unwrap()
            .remove_by_device(&pci_device)
            .map_err(DeviceManagerError::RemoveDeviceFromPciBus)?;

        #[cfg(target_arch = "x86_64")]
        // Remove the device from the IO bus
        self.io_bus()
            .remove_by_device(&bus_device)
            .map_err(DeviceManagerError::RemoveDeviceFromIoBus)?;

        // Remove the device from the MMIO bus
        self.mmio_bus()
            .remove_by_device(&bus_device)
            .map_err(DeviceManagerError::RemoveDeviceFromMmioBus)?;

        // Remove the device from the list of BusDevice held by the
        // DeviceManager.
        self.bus_devices
            .retain(|dev| !Arc::ptr_eq(dev, &bus_device));

        // Shutdown and remove the underlying virtio-device if present
        if let Some(virtio_device) = virtio_device {
            for mapping in virtio_device.lock().unwrap().userspace_mappings() {
                self.memory_manager
                    .lock()
                    .unwrap()
                    .remove_userspace_mapping(
                        mapping.addr.raw_value(),
                        mapping.len,
                        mapping.host_addr,
                        mapping.mergeable,
                        mapping.mem_slot,
                    )
                    .map_err(DeviceManagerError::MemoryManager)?;
            }

            virtio_device.lock().unwrap().shutdown();

            self.virtio_devices
                .retain(|handler| !Arc::ptr_eq(&handler.virtio_device, &virtio_device));
        }

        event!(
            "vm",
            "device-removed",
            "id",
            &id,
            "bdf",
            pci_device_bdf.to_string()
        );

        // At this point, the device has been removed from all the list and
        // buses where it was stored. At the end of this function, after
        // any_device, bus_device and pci_device are released, the actual
        // device will be dropped.
        Ok(())
    }

    fn hotplug_virtio_pci_device(
        &mut self,
        handle: MetaVirtioDevice,
    ) -> DeviceManagerResult<PciDeviceInfo> {
        // Add the virtio device to the device manager list. This is important
        // as the list is used to notify virtio devices about memory updates
        // for instance.
        self.virtio_devices.push(handle.clone());

        let mapping: Option<Arc<IommuMapping>> = if handle.iommu {
            self.iommu_mapping.clone()
        } else {
            None
        };

        let bdf = self.add_virtio_pci_device(
            handle.virtio_device,
            &mapping,
            handle.id.clone(),
            handle.pci_segment,
            handle.dma_handler,
        )?;

        // Update the PCIU bitmap
        self.pci_segments[handle.pci_segment as usize].pci_devices_up |= 1 << bdf.device();

        Ok(PciDeviceInfo { id: handle.id, bdf })
    }

    fn is_iommu_segment(&self, pci_segment_id: u16) -> bool {
        self.config
            .lock()
            .as_ref()
            .unwrap()
            .platform
            .as_ref()
            .map(|pc| {
                pc.iommu_segments
                    .as_ref()
                    .map(|v| v.contains(&pci_segment_id))
                    .unwrap_or_default()
            })
            .unwrap_or_default()
    }

    pub fn add_disk(&mut self, disk_cfg: &mut DiskConfig) -> DeviceManagerResult<PciDeviceInfo> {
        self.validate_identifier(&disk_cfg.id)?;

        if disk_cfg.iommu && !self.is_iommu_segment(disk_cfg.pci_segment) {
            return Err(DeviceManagerError::InvalidIommuHotplug);
        }

        let device = self.make_virtio_block_device(disk_cfg)?;
        self.hotplug_virtio_pci_device(device)
    }

    pub fn add_fs(&mut self, fs_cfg: &mut FsConfig) -> DeviceManagerResult<PciDeviceInfo> {
        self.validate_identifier(&fs_cfg.id)?;

        let device = self.make_virtio_fs_device(fs_cfg)?;
        self.hotplug_virtio_pci_device(device)
    }

    pub fn add_pmem(&mut self, pmem_cfg: &mut PmemConfig) -> DeviceManagerResult<PciDeviceInfo> {
        self.validate_identifier(&pmem_cfg.id)?;

        if pmem_cfg.iommu && !self.is_iommu_segment(pmem_cfg.pci_segment) {
            return Err(DeviceManagerError::InvalidIommuHotplug);
        }

        let device = self.make_virtio_pmem_device(pmem_cfg)?;
        self.hotplug_virtio_pci_device(device)
    }

    pub fn add_net(&mut self, net_cfg: &mut NetConfig) -> DeviceManagerResult<PciDeviceInfo> {
        self.validate_identifier(&net_cfg.id)?;

        if net_cfg.iommu && !self.is_iommu_segment(net_cfg.pci_segment) {
            return Err(DeviceManagerError::InvalidIommuHotplug);
        }

        let device = self.make_virtio_net_device(net_cfg)?;
        self.hotplug_virtio_pci_device(device)
    }

    pub fn add_vdpa(&mut self, vdpa_cfg: &mut VdpaConfig) -> DeviceManagerResult<PciDeviceInfo> {
        self.validate_identifier(&vdpa_cfg.id)?;

        if vdpa_cfg.iommu && !self.is_iommu_segment(vdpa_cfg.pci_segment) {
            return Err(DeviceManagerError::InvalidIommuHotplug);
        }

        let device = self.make_vdpa_device(vdpa_cfg)?;
        self.hotplug_virtio_pci_device(device)
    }

    pub fn add_vsock(&mut self, vsock_cfg: &mut VsockConfig) -> DeviceManagerResult<PciDeviceInfo> {
        self.validate_identifier(&vsock_cfg.id)?;

        if vsock_cfg.iommu && !self.is_iommu_segment(vsock_cfg.pci_segment) {
            return Err(DeviceManagerError::InvalidIommuHotplug);
        }

        let device = self.make_virtio_vsock_device(vsock_cfg)?;
        self.hotplug_virtio_pci_device(device)
    }

    pub fn counters(&self) -> HashMap<String, HashMap<&'static str, Wrapping<u64>>> {
        let mut counters = HashMap::new();

        for handle in &self.virtio_devices {
            let virtio_device = handle.virtio_device.lock().unwrap();
            if let Some(device_counters) = virtio_device.counters() {
                counters.insert(handle.id.clone(), device_counters.clone());
            }
        }

        counters
    }

    pub fn resize_balloon(&mut self, size: u64) -> DeviceManagerResult<()> {
        if let Some(balloon) = &self.balloon {
            return balloon
                .lock()
                .unwrap()
                .resize(size)
                .map_err(DeviceManagerError::VirtioBalloonResize);
        }

        warn!("No balloon setup: Can't resize the balloon");
        Err(DeviceManagerError::MissingVirtioBalloon)
    }

    pub fn balloon_size(&self) -> u64 {
        if let Some(balloon) = &self.balloon {
            return balloon.lock().unwrap().get_actual();
        }

        0
    }

    pub fn device_tree(&self) -> Arc<Mutex<DeviceTree>> {
        self.device_tree.clone()
    }

    pub fn sys_started(&self) -> bool {
        #[cfg(target_arch = "x86_64")]
        if let Some(sys_ctrl) = &self.sys_ctrl {
            return sys_ctrl.clone().lock().unwrap().sys_started();
        }

        true
    }

    pub fn restore_device_node(
        &mut self,
        device_groups: (Vec<DeviceNode>, Vec<DeviceNode>),
        snapshot: Snapshot,
    ) -> std::result::Result<(), MigratableError> {
        let total_nodes = device_groups.0.len() + device_groups.1.len();
        let work_thread_num = if total_nodes > MAX_WORKER_THREADS {
            MAX_WORKER_THREADS
        } else {
            total_nodes
        };

        let rt = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(work_thread_num)
            .enable_all()
            .build()
            .unwrap();

        rt.block_on(async move {
            // Process device groups in order to ensure correct restoration sequence
            let groups = [device_groups.0, device_groups.1];
            for nodes in groups {
                let mut thread_pool: Vec<JoinHandle<Result<(), MigratableError>>> = vec![];

                for node in nodes {
                    if let Some(migratable) = node.migratable {
                        debug!("Restoring {} from DeviceManager", node.id);
                        if let Some(snapshot) = snapshot.snapshots.get(&node.id).cloned() {
                            thread_pool.push(tokio::spawn(async move {
                                let mut guard = migratable.lock().unwrap();
                                guard.pause()?;
                                guard.restore(*snapshot)?;
                                Ok(())
                            }));
                        } else {
                            return Err(MigratableError::Restore(anyhow!(
                                "Missing device {}",
                                node.id
                            )));
                        }
                    }
                }

                // Wait for all devices in current group to complete before processing next group
                for t in thread_pool {
                    t.await.unwrap()?;
                }
            }
            Ok(())
        })
    }

    pub fn restore_devices(
        &mut self,
        snapshot: Snapshot,
    ) -> std::result::Result<(), MigratableError> {
        // Finally, restore all devices associated with the DeviceManager.
        // It's important to restore devices in the right order, that's why
        // the device tree is the right way to ensure we restore a child before
        // its parent node.
        let devices_children: Vec<_> = self
            .device_tree
            .lock()
            .unwrap()
            .breadth_first_traversal_children()
            .rev()
            .cloned()
            .collect();

        let devices_parent: Vec<_> = self
            .device_tree
            .lock()
            .unwrap()
            .breadth_first_traversal_parent()
            .rev()
            .cloned()
            .collect();

        let device_groups = (devices_children, devices_parent);
        self.restore_device_node(device_groups, snapshot)?;

        // The devices have been fully restored, we can now update the
        // restoring state of the DeviceManager.
        self.restoring = false;

        Ok(())
    }

    #[cfg(target_arch = "x86_64")]
    pub fn notify_power_button(&self) -> DeviceManagerResult<()> {
        self.ged_notification_device
            .as_ref()
            .unwrap()
            .lock()
            .unwrap()
            .notify(AcpiNotificationFlags::POWER_BUTTON_CHANGED)
            .map_err(DeviceManagerError::PowerButtonNotification)
    }

    #[cfg(target_arch = "aarch64")]
    pub fn notify_power_button(&self) -> DeviceManagerResult<()> {
        // There are two use cases:
        // 1. Users will use direct kernel boot with device tree.
        // 2. Users will use ACPI+UEFI boot.

        // Trigger a GPIO pin 3 event to satisfy use case 1.
        self.gpio_device
            .as_ref()
            .unwrap()
            .lock()
            .unwrap()
            .trigger_key(3)
            .map_err(DeviceManagerError::AArch64PowerButtonNotification)?;
        // Trigger a GED power button event to satisfy use case 2.
        return self
            .ged_notification_device
            .as_ref()
            .unwrap()
            .lock()
            .unwrap()
            .notify(AcpiNotificationFlags::POWER_BUTTON_CHANGED)
            .map_err(DeviceManagerError::PowerButtonNotification);
    }

    pub fn iommu_attached_devices(&self) -> &Option<(PciBdf, Vec<PciBdf>)> {
        &self.iommu_attached_devices
    }

    fn validate_identifier(&self, id: &Option<String>) -> DeviceManagerResult<()> {
        if let Some(id) = id {
            if id.starts_with("__") {
                return Err(DeviceManagerError::InvalidIdentifier(id.clone()));
            }

            if self.device_tree.lock().unwrap().contains_key(id) {
                return Err(DeviceManagerError::IdentifierNotUnique(id.clone()));
            }
        }

        Ok(())
    }

    pub(crate) fn acpi_platform_addresses(&self) -> &AcpiPlatformAddresses {
        &self.acpi_platform_addresses
    }
}

fn numa_node_id_from_memory_zone_id(numa_nodes: &NumaNodes, memory_zone_id: &str) -> Option<u32> {
    for (numa_node_id, numa_node) in numa_nodes.iter() {
        if numa_node.memory_zones.contains(&memory_zone_id.to_owned()) {
            return Some(*numa_node_id);
        }
    }

    None
}

struct TpmDevice {}

impl Aml for TpmDevice {
    fn to_aml_bytes(&self) -> Vec<u8> {
        aml::Device::new(
            "TPM2".into(),
            vec![
                &aml::Name::new("_HID".into(), &"MSFT0101"),
                &aml::Name::new("_STA".into(), &(0xF_usize)),
                &aml::Name::new(
                    "_CRS".into(),
                    &aml::ResourceTemplate::new(vec![&aml::Memory32Fixed::new(
                        true,
                        layout::TPM_START.0 as u32,
                        layout::TPM_SIZE as u32,
                    )]),
                ),
            ],
        )
        .to_aml_bytes()
    }
}

impl Aml for DeviceManager {
    fn append_aml_bytes(&self, bytes: &mut Vec<u8>) {
        #[cfg(target_arch = "aarch64")]
        use arch::aarch64::DeviceInfoForFdt;

        let mut pci_scan_methods = Vec::new();
        for i in 0..self.pci_segments.len() {
            pci_scan_methods.push(aml::MethodCall::new(
                format!("\\_SB_.PCI{:X}.PCNT", i).as_str().into(),
                vec![],
            ));
        }
        let mut pci_scan_inner: Vec<&dyn Aml> = Vec::new();
        for method in &pci_scan_methods {
            pci_scan_inner.push(method)
        }

        // PCI hotplug controller
        aml::Device::new(
            "_SB_.PHPR".into(),
            vec![
                &aml::Name::new("_HID".into(), &aml::EisaName::new("PNP0A06")),
                &aml::Name::new("_STA".into(), &0x0bu8),
                &aml::Name::new("_UID".into(), &"PCI Hotplug Controller"),
                &aml::Mutex::new("BLCK".into(), 0),
                &aml::Name::new(
                    "_CRS".into(),
                    &aml::ResourceTemplate::new(vec![&aml::AddressSpace::new_memory(
                        aml::AddressSpaceCacheable::NotCacheable,
                        true,
                        self.acpi_address.0,
                        self.acpi_address.0 + DEVICE_MANAGER_ACPI_SIZE as u64 - 1,
                    )]),
                ),
                // OpRegion and Fields map MMIO range into individual field values
                &aml::OpRegion::new(
                    "PCST".into(),
                    aml::OpRegionSpace::SystemMemory,
                    self.acpi_address.0 as usize,
                    DEVICE_MANAGER_ACPI_SIZE,
                ),
                &aml::Field::new(
                    "PCST".into(),
                    aml::FieldAccessType::DWord,
                    aml::FieldUpdateRule::WriteAsZeroes,
                    vec![
                        aml::FieldEntry::Named(*b"PCIU", 32),
                        aml::FieldEntry::Named(*b"PCID", 32),
                        aml::FieldEntry::Named(*b"B0EJ", 32),
                        aml::FieldEntry::Named(*b"PSEG", 32),
                    ],
                ),
                &aml::Method::new(
                    "PCEJ".into(),
                    2,
                    true,
                    vec![
                        // Take lock defined above
                        &aml::Acquire::new("BLCK".into(), 0xffff),
                        // Choose the current segment
                        &aml::Store::new(&aml::Path::new("PSEG"), &aml::Arg(1)),
                        // Write PCI bus number (in first argument) to I/O port via field
                        &aml::ShiftLeft::new(&aml::Path::new("B0EJ"), &aml::ONE, &aml::Arg(0)),
                        // Release lock
                        &aml::Release::new("BLCK".into()),
                        // Return 0
                        &aml::Return::new(&aml::ZERO),
                    ],
                ),
                &aml::Method::new("PSCN".into(), 0, true, pci_scan_inner),
            ],
        )
        .append_aml_bytes(bytes);

        for segment in &self.pci_segments {
            segment.append_aml_bytes(bytes);
        }

        let mut mbrd_memory = Vec::new();

        for segment in &self.pci_segments {
            mbrd_memory.push(aml::Memory32Fixed::new(
                true,
                segment.mmio_config_address as u32,
                layout::PCI_MMIO_CONFIG_SIZE_PER_SEGMENT as u32,
            ))
        }

        let mut mbrd_memory_refs = Vec::new();
        for mbrd_memory_ref in &mbrd_memory {
            mbrd_memory_refs.push(mbrd_memory_ref as &dyn Aml);
        }

        aml::Device::new(
            "_SB_.MBRD".into(),
            vec![
                &aml::Name::new("_HID".into(), &aml::EisaName::new("PNP0C02")),
                &aml::Name::new("_UID".into(), &aml::ZERO),
                &aml::Name::new("_CRS".into(), &aml::ResourceTemplate::new(mbrd_memory_refs)),
            ],
        )
        .append_aml_bytes(bytes);

        // Serial device
        #[cfg(target_arch = "x86_64")]
        let serial_irq = 4;
        #[cfg(target_arch = "aarch64")]
        let serial_irq =
            if self.config.lock().unwrap().serial.clone().mode != ConsoleOutputMode::Off {
                self.get_device_info()
                    .clone()
                    .get(&(DeviceType::Serial, DeviceType::Serial.to_string()))
                    .unwrap()
                    .irq()
            } else {
                // If serial is turned off, add a fake device with invalid irq.
                31
            };
        if self.config.lock().unwrap().serial.mode != ConsoleOutputMode::Off {
            aml::Device::new(
                "_SB_.COM1".into(),
                vec![
                    &aml::Name::new(
                        "_HID".into(),
                        #[cfg(target_arch = "x86_64")]
                        &aml::EisaName::new("PNP0501"),
                        #[cfg(target_arch = "aarch64")]
                        &"ARMH0011",
                    ),
                    &aml::Name::new("_UID".into(), &aml::ZERO),
                    &aml::Name::new("_DDN".into(), &"COM1"),
                    &aml::Name::new(
                        "_CRS".into(),
                        &aml::ResourceTemplate::new(vec![
                            &aml::Interrupt::new(true, true, false, false, serial_irq),
                            #[cfg(target_arch = "x86_64")]
                            &aml::Io::new(0x3f8, 0x3f8, 0, 0x8),
                            #[cfg(target_arch = "aarch64")]
                            &aml::Memory32Fixed::new(
                                true,
                                arch::layout::LEGACY_SERIAL_MAPPED_IO_START.raw_value() as u32,
                                MMIO_LEN as u32,
                            ),
                        ]),
                    ),
                ],
            )
            .append_aml_bytes(bytes);
        }

        aml::Name::new("_S5_".into(), &aml::Package::new(vec![&5u8])).append_aml_bytes(bytes);

        aml::Device::new(
            "_SB_.PWRB".into(),
            vec![
                &aml::Name::new("_HID".into(), &aml::EisaName::new("PNP0C0C")),
                &aml::Name::new("_UID".into(), &aml::ZERO),
            ],
        )
        .append_aml_bytes(bytes);

        if self.config.lock().unwrap().tpm.is_some() {
            // Add tpm device
            let tpm_acpi = TpmDevice {};
            let tpm_dsdt_data = tpm_acpi.to_aml_bytes();
            bytes.extend_from_slice(tpm_dsdt_data.as_slice());
        }

        self.ged_notification_device
            .as_ref()
            .unwrap()
            .lock()
            .unwrap()
            .append_aml_bytes(bytes);
    }
}

impl Pausable for DeviceManager {
    fn pause(&mut self) -> result::Result<(), MigratableError> {
        for (_, device_node) in self.device_tree.lock().unwrap().iter() {
            if let Some(migratable) = &device_node.migratable {
                migratable.lock().unwrap().pause()?;
            }
        }
        // On AArch64, the pause of device manager needs to trigger
        // a "pause" of GIC, which will flush the GIC pending tables
        // and ITS tables to guest RAM.
        #[cfg(target_arch = "aarch64")]
        {
            self.get_interrupt_controller()
                .unwrap()
                .lock()
                .unwrap()
                .pause()?;
        };

        Ok(())
    }

    fn resume(&mut self) -> result::Result<(), MigratableError> {
        for (_, device_node) in self.device_tree.lock().unwrap().iter() {
            if let Some(migratable) = &device_node.migratable {
                migratable.lock().unwrap().resume()?;
            }
        }

        Ok(())
    }
}

impl Snapshottable for DeviceManager {
    fn id(&self) -> String {
        DEVICE_MANAGER_SNAPSHOT_ID.to_string()
    }

    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        // We aggregate all devices snapshots.
        let devices: Vec<_> = self
            .device_tree
            .lock()
            .unwrap()
            .iter()
            .map(|(_, node)| node.clone())
            .collect();

        let work_thread_num = if devices.len() > MAX_WORKER_THREADS {
            MAX_WORKER_THREADS
        } else {
            // always greater than 0
            devices.len()
        };
        let rt = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(work_thread_num)
            .enable_all()
            .build()
            .unwrap();

        let mut snapshot = rt.block_on(async move {
            let snapshot = Arc::new(tokio::sync::Mutex::new(Snapshot::new(
                DEVICE_MANAGER_SNAPSHOT_ID,
            )));
            let mut thread_pool = vec![];

            for device_node in devices.into_iter() {
                if let Some(migratable) = device_node.migratable {
                    let snapshot_arc = snapshot.clone();
                    thread_pool.push(tokio::spawn(async move {
                        // sync::Mutex is safe since vm has been paused and vmm action is serial.
                        let device_snapshot = migratable.lock().unwrap().snapshot()?;
                        snapshot_arc.lock().await.add_snapshot(device_snapshot);
                        Ok(())
                    }));
                }
            }
            for t in thread_pool {
                t.await.unwrap()?;
            }
            if let Ok(mutex) = Arc::try_unwrap(snapshot) {
                Ok(mutex.into_inner())
            } else {
                Err(MigratableError::Snapshot(anyhow!(
                    "Could not unwrap snapshot mutex"
                )))
            }
        })?;

        // Then we store the DeviceManager state.
        snapshot.add_data_section(SnapshotDataSection::new_from_state(
            DEVICE_MANAGER_SNAPSHOT_ID,
            &self.state(),
        )?);

        Ok(snapshot)
    }

    fn restore(&mut self, snapshot: Snapshot) -> std::result::Result<(), MigratableError> {
        // Let's first restore the DeviceManager.

        self.set_state(&snapshot.to_state(DEVICE_MANAGER_SNAPSHOT_ID)?);

        // Now that DeviceManager is updated with the right states, it's time
        // to create the devices based on the configuration.
        self.create_devices(None, None, None)
            .map_err(|e| MigratableError::Restore(anyhow!("Could not create devices {:?}", e)))?;

        Ok(())
    }
}

impl Transportable for DeviceManager {}

impl Migratable for DeviceManager {
    fn start_dirty_log(&mut self) -> std::result::Result<(), MigratableError> {
        for (_, device_node) in self.device_tree.lock().unwrap().iter() {
            if let Some(migratable) = &device_node.migratable {
                migratable.lock().unwrap().start_dirty_log()?;
            }
        }
        Ok(())
    }

    fn stop_dirty_log(&mut self) -> std::result::Result<(), MigratableError> {
        for (_, device_node) in self.device_tree.lock().unwrap().iter() {
            if let Some(migratable) = &device_node.migratable {
                migratable.lock().unwrap().stop_dirty_log()?;
            }
        }
        Ok(())
    }

    fn dirty_log(&mut self) -> std::result::Result<MemoryRangeTable, MigratableError> {
        let mut tables = Vec::new();
        for (_, device_node) in self.device_tree.lock().unwrap().iter() {
            if let Some(migratable) = &device_node.migratable {
                tables.push(migratable.lock().unwrap().dirty_log()?);
            }
        }
        Ok(MemoryRangeTable::new_from_tables(tables))
    }

    fn start_migration(&mut self) -> std::result::Result<(), MigratableError> {
        for (_, device_node) in self.device_tree.lock().unwrap().iter() {
            if let Some(migratable) = &device_node.migratable {
                migratable.lock().unwrap().start_migration()?;
            }
        }
        Ok(())
    }

    fn complete_migration(&mut self) -> std::result::Result<(), MigratableError> {
        for (_, device_node) in self.device_tree.lock().unwrap().iter() {
            if let Some(migratable) = &device_node.migratable {
                migratable.lock().unwrap().complete_migration()?;
            }
        }
        Ok(())
    }
}

const PCIU_FIELD_OFFSET: u64 = 0;
const PCID_FIELD_OFFSET: u64 = 4;
const B0EJ_FIELD_OFFSET: u64 = 8;
const PSEG_FIELD_OFFSET: u64 = 12;
const PCIU_FIELD_SIZE: usize = 4;
const PCID_FIELD_SIZE: usize = 4;
const B0EJ_FIELD_SIZE: usize = 4;
const PSEG_FIELD_SIZE: usize = 4;

impl BusDevice for DeviceManager {
    fn read(&mut self, base: u64, offset: u64, data: &mut [u8]) {
        match offset {
            PCIU_FIELD_OFFSET => {
                assert!(data.len() == PCIU_FIELD_SIZE);
                data.copy_from_slice(
                    &self.pci_segments[self.selected_segment]
                        .pci_devices_up
                        .to_le_bytes(),
                );
                // Clear the PCIU bitmap
                self.pci_segments[self.selected_segment].pci_devices_up = 0;
            }
            PCID_FIELD_OFFSET => {
                assert!(data.len() == PCID_FIELD_SIZE);
                data.copy_from_slice(
                    &self.pci_segments[self.selected_segment]
                        .pci_devices_down
                        .to_le_bytes(),
                );
                // Clear the PCID bitmap
                self.pci_segments[self.selected_segment].pci_devices_down = 0;
            }
            B0EJ_FIELD_OFFSET => {
                assert!(data.len() == B0EJ_FIELD_SIZE);
                // Always return an empty bitmap since the eject is always
                // taken care of right away during a write access.
                data.fill(0);
            }
            PSEG_FIELD_OFFSET => {
                assert_eq!(data.len(), PSEG_FIELD_SIZE);
                data.copy_from_slice(&(self.selected_segment as u32).to_le_bytes());
            }
            _ => error!(
                "Accessing unknown location at base 0x{:x}, offset 0x{:x}",
                base, offset
            ),
        }

        debug!(
            "PCI_HP_REG_R: base 0x{:x}, offset 0x{:x}, data {:?}",
            base, offset, data
        )
    }

    fn write(&mut self, base: u64, offset: u64, data: &[u8]) -> Option<Arc<std::sync::Barrier>> {
        match offset {
            B0EJ_FIELD_OFFSET => {
                assert!(data.len() == B0EJ_FIELD_SIZE);
                let mut data_array: [u8; 4] = [0, 0, 0, 0];
                data_array.copy_from_slice(data);
                let mut slot_bitmap = u32::from_le_bytes(data_array);

                while slot_bitmap > 0 {
                    let slot_id = slot_bitmap.trailing_zeros();
                    if let Err(e) = self.eject_device(self.selected_segment as u16, slot_id as u8) {
                        error!("Failed ejecting device {}: {:?}", slot_id, e);
                    }
                    slot_bitmap &= !(1 << slot_id);
                }
            }
            PSEG_FIELD_OFFSET => {
                assert_eq!(data.len(), PSEG_FIELD_SIZE);
                let mut data_array: [u8; 4] = [0, 0, 0, 0];
                data_array.copy_from_slice(data);
                let selected_segment = u32::from_le_bytes(data_array) as usize;
                if selected_segment >= self.pci_segments.len() {
                    error!(
                        "Segment selection out of range: {} >= {}",
                        selected_segment,
                        self.pci_segments.len()
                    );
                    return None;
                }
                self.selected_segment = selected_segment;
            }
            _ => error!(
                "Accessing unknown location at base 0x{:x}, offset 0x{:x}",
                base, offset
            ),
        }

        debug!(
            "PCI_HP_REG_W: base 0x{:x}, offset 0x{:x}, data {:?}",
            base, offset, data
        );

        None
    }
}

impl Drop for DeviceManager {
    fn drop(&mut self) {
        // Wake up the DeviceManager threads (mainly virtio device workers),
        // to avoid deadlock on waiting for paused/parked worker threads.
        if let Err(e) = self.resume() {
            error!("Error resuming DeviceManager: {:?}", e);
        }

        for handle in self.virtio_devices.drain(..) {
            handle.virtio_device.lock().unwrap().shutdown();
        }
    }
}

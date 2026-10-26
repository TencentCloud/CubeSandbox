// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use crate::vfio::{Interrupt, UserMemoryRegion, Vfio, VfioCommon, VfioError};
use crate::{BarReprogrammingParams, PciBarConfiguration, VfioPciError};
use crate::{
    PciBdf, PciClassCode, PciConfiguration, PciDevice, PciDeviceError, PciHeaderType, PciSubclass,
};
use anyhow::anyhow;
use hypervisor::HypervisorVmError;
use std::any::Any;
use std::collections::HashMap;
use std::os::unix::prelude::AsRawFd;
use std::ptr::null_mut;
use std::sync::{Arc, Barrier, Mutex};
use std::u32;
use thiserror::Error;
use vfio_bindings::bindings::vfio::*;
use vfio_ioctls::VfioIrq;
use vfio_user::{Client, Error as VfioUserError};
use vm_allocator::{AddressAllocator, SystemAllocator};
use vm_device::dma_mapping::ExternalDmaMapping;
use vm_device::interrupt::{InterruptManager, InterruptSourceGroup, MsiIrqGroupConfig};
use vm_device::{BusDevice, Resource};
use vm_memory::bitmap::AtomicBitmap;
use vm_memory::{
    Address, GuestAddress, GuestAddressSpace, GuestMemory, GuestMemoryRegion, GuestRegionMmap,
};
use vm_migration::{Migratable, MigratableError, Pausable, Snapshot, Snapshottable, Transportable};
use vmm_sys_util::eventfd::EventFd;

pub struct VfioUserPciDevice {
    id: String,
    vm: Arc<dyn hypervisor::Vm>,
    client: Arc<Mutex<Client>>,
    common: VfioCommon,
    memory_slot: Arc<dyn Fn() -> u32 + Send + Sync>,
}

#[derive(Error, Debug)]
pub enum VfioUserPciDeviceError {
    #[error("Client error: {0}")]
    Client(#[source] VfioUserError),
    #[error("Failed to map VFIO PCI region into guest: {0}")]
    MapRegionGuest(#[source] HypervisorVmError),
    #[error("Failed to DMA map: {0}")]
    DmaMap(#[source] VfioUserError),
    #[error("Failed to DMA unmap: {0}")]
    DmaUnmap(#[source] VfioUserError),
    #[error("Failed to initialize legacy interrupts: {0}")]
    InitializeLegacyInterrupts(#[source] VfioPciError),
}

#[derive(Copy, Clone)]
enum PciVfioUserSubclass {
    VfioUserSubclass = 0xff,
}

impl PciSubclass for PciVfioUserSubclass {
    fn get_register_value(&self) -> u8 {
        *self as u8
    }
}

impl VfioUserPciDevice {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        id: String,
        vm: &Arc<dyn hypervisor::Vm>,
        client: Arc<Mutex<Client>>,
        msi_interrupt_manager: Arc<dyn InterruptManager<GroupConfig = MsiIrqGroupConfig>>,
        legacy_interrupt_group: Option<Arc<dyn InterruptSourceGroup>>,
        bdf: PciBdf,
        restoring: bool,
        memory_slot: Arc<dyn Fn() -> u32 + Send + Sync>,
    ) -> Result<Self, VfioUserPciDeviceError> {
        // This is used for the BAR and capabilities only
        let configuration = PciConfiguration::new(
            0,
            0,
            0,
            PciClassCode::Other,
            &PciVfioUserSubclass::VfioUserSubclass,
            None,
            PciHeaderType::Device,
            0,
            0,
            None,
        );
        let resettable = client.lock().unwrap().resettable();
        if resettable {
            client
                .lock()
                .unwrap()
                .reset()
                .map_err(VfioUserPciDeviceError::Client)?;
        }

        let vfio_wrapper = VfioUserClientWrapper {
            client: client.clone(),
        };

        let mut common = VfioCommon {
            mmio_regions: Vec::new(),
            configuration,
            interrupt: Interrupt {
                intx: None,
                msi: None,
                msix: None,
            },
            msi_interrupt_manager,
            legacy_interrupt_group,
            vfio_wrapper: Arc::new(vfio_wrapper) as Arc<dyn Vfio>,
            patches: HashMap::new(),
        };

        // No need to parse capabilities from the device if on the restore path.
        // The initialization will be performed later when restore() will be
        // called.
        if !restoring {
            common.parse_capabilities(bdf);
            common
                .initialize_legacy_interrupt()
                .map_err(VfioUserPciDeviceError::InitializeLegacyInterrupts)?;
        }

        Ok(Self {
            id,
            vm: vm.clone(),
            client,
            common,
            memory_slot,
        })
    }

    pub fn map_mmio_regions(&mut self) -> Result<(), VfioUserPciDeviceError> {
        for mmio_region in &mut self.common.mmio_regions {
            let region_flags = self
                .client
                .lock()
                .unwrap()
                .region(mmio_region.index)
                .unwrap()
                .flags;
            let file_offset = self
                .client
                .lock()
                .unwrap()
                .region(mmio_region.index)
                .unwrap()
                .file_offset
                .clone();

            let sparse_areas = self
                .client
                .lock()
                .unwrap()
                .region(mmio_region.index)
                .unwrap()
                .sparse_areas
                .clone();

            if region_flags & VFIO_REGION_INFO_FLAG_MMAP != 0 {
                let mut prot = 0;
                if region_flags & VFIO_REGION_INFO_FLAG_READ != 0 {
                    prot |= libc::PROT_READ;
                }
                if region_flags & VFIO_REGION_INFO_FLAG_WRITE != 0 {
                    prot |= libc::PROT_WRITE;
                }

                let mmaps = if sparse_areas.is_empty() {
                    vec![vfio_region_sparse_mmap_area {
                        offset: 0,
                        size: mmio_region.length,
                    }]
                } else {
                    sparse_areas
                };

                for s in mmaps.iter() {
                    let host_addr = unsafe {
                        libc::mmap(
                            null_mut(),
                            s.size as usize,
                            prot,
                            libc::MAP_SHARED,
                            file_offset.as_ref().unwrap().file().as_raw_fd(),
                            file_offset.as_ref().unwrap().start() as libc::off_t
                                + s.offset as libc::off_t,
                        )
                    };

                    if host_addr == libc::MAP_FAILED {
                        error!(
                            "Could not mmap regions, error:{}",
                            std::io::Error::last_os_error()
                        );
                        continue;
                    }

                    let user_memory_region = UserMemoryRegion {
                        slot: (self.memory_slot)(),
                        start: mmio_region.start.0 + s.offset,
                        size: s.size,
                        host_addr: host_addr as u64,
                    };

                    mmio_region.user_memory_regions.push(user_memory_region);

                    let mem_region = self.vm.make_user_memory_region(
                        user_memory_region.slot,
                        user_memory_region.start,
                        user_memory_region.size,
                        user_memory_region.host_addr,
                        false,
                        false,
                    );

                    self.vm
                        .create_user_memory_region(mem_region)
                        .map_err(VfioUserPciDeviceError::MapRegionGuest)?;
                }
            }
        }

        Ok(())
    }

    pub fn unmap_mmio_regions(&mut self) {
        for mmio_region in self.common.mmio_regions.iter() {
            for user_memory_region in mmio_region.user_memory_regions.iter() {
                // Remove region
                let r = self.vm.make_user_memory_region(
                    user_memory_region.slot,
                    user_memory_region.start,
                    user_memory_region.size,
                    user_memory_region.host_addr,
                    false,
                    false,
                );

                if let Err(e) = self.vm.remove_user_memory_region(r) {
                    error!("Could not remove the userspace memory region: {}", e);
                }

                // Remove mmaps
                let ret = unsafe {
                    libc::munmap(
                        user_memory_region.host_addr as *mut libc::c_void,
                        user_memory_region.size as usize,
                    )
                };
                if ret != 0 {
                    error!(
                        "Could not unmap region {}, error:{}",
                        mmio_region.index,
                        std::io::Error::last_os_error()
                    );
                }
            }
        }
    }

    pub fn dma_map(
        &mut self,
        region: &GuestRegionMmap<AtomicBitmap>,
    ) -> Result<(), VfioUserPciDeviceError> {
        let (fd, offset) = match region.file_offset() {
            Some(_file_offset) => (_file_offset.file().as_raw_fd(), _file_offset.start()),
            None => return Ok(()),
        };

        self.client
            .lock()
            .unwrap()
            .dma_map(offset, region.start_addr().raw_value(), region.len(), fd)
            .map_err(VfioUserPciDeviceError::DmaMap)
    }

    pub fn dma_unmap(
        &mut self,
        region: &GuestRegionMmap<AtomicBitmap>,
    ) -> Result<(), VfioUserPciDeviceError> {
        self.client
            .lock()
            .unwrap()
            .dma_unmap(region.start_addr().raw_value(), region.len())
            .map_err(VfioUserPciDeviceError::DmaUnmap)
    }
}

impl BusDevice for VfioUserPciDevice {
    fn read(&mut self, base: u64, offset: u64, data: &mut [u8]) {
        self.read_bar(base, offset, data)
    }

    fn write(&mut self, base: u64, offset: u64, data: &[u8]) -> Option<Arc<Barrier>> {
        self.write_bar(base, offset, data)
    }
}

#[repr(u32)]
#[derive(Debug, PartialEq, Eq, PartialOrd, Ord)]
#[allow(dead_code)]
enum Regions {
    Bar0,
    Bar1,
    Bar2,
    Bar3,
    Bar4,
    Bar5,
    Rom,
    Config,
    Vga,
    Migration,
}

struct VfioUserClientWrapper {
    client: Arc<Mutex<Client>>,
}

impl Vfio for VfioUserClientWrapper {
    fn region_read(&self, index: u32, offset: u64, data: &mut [u8]) {
        self.client
            .lock()
            .unwrap()
            .region_read(index, offset, data)
            .ok();
    }

    fn region_write(&self, index: u32, offset: u64, data: &[u8]) {
        self.client
            .lock()
            .unwrap()
            .region_write(index, offset, data)
            .ok();
    }

    fn get_irq_info(&self, irq_index: u32) -> Option<VfioIrq> {
        self.client
            .lock()
            .unwrap()
            .get_irq_info(irq_index)
            .ok()
            .map(|i| VfioIrq {
                index: i.index,
                flags: i.flags,
                count: i.count,
            })
    }

    fn enable_irq(&self, irq_index: u32, event_fds: Vec<&EventFd>) -> Result<(), VfioError> {
        info!(
            "Enabling IRQ {:x} number of fds = {:?}",
            irq_index,
            event_fds.len()
        );
        let fds: Vec<i32> = event_fds.iter().map(|e| e.as_raw_fd()).collect();

        // Batch into blocks of 16 fds as sendmsg() has a size limit
        let mut sent_fds = 0;
        let num_fds = event_fds.len() as u32;
        while sent_fds < num_fds {
            let remaining_fds = num_fds - sent_fds;
            let count = if remaining_fds > 16 {
                16
            } else {
                remaining_fds
            };

            self.client
                .lock()
                .unwrap()
                .set_irqs(
                    irq_index,
                    VFIO_IRQ_SET_DATA_EVENTFD | VFIO_IRQ_SET_ACTION_TRIGGER,
                    sent_fds,
                    count,
                    &fds[sent_fds as usize..(sent_fds + count) as usize],
                )
                .map_err(VfioError::VfioUser)?;

            sent_fds += count;
        }

        Ok(())
    }

    fn disable_irq(&self, irq_index: u32) -> Result<(), VfioError> {
        info!("Disabling IRQ {:x}", irq_index);
        self.client
            .lock()
            .unwrap()
            .set_irqs(
                irq_index,
                VFIO_IRQ_SET_DATA_NONE | VFIO_IRQ_SET_ACTION_TRIGGER,
                0,
                0,
                &[],
            )
            .map_err(VfioError::VfioUser)
    }

    fn unmask_irq(&self, irq_index: u32) -> Result<(), VfioError> {
        info!("Unmasking IRQ {:x}", irq_index);
        self.client
            .lock()
            .unwrap()
            .set_irqs(
                irq_index,
                VFIO_IRQ_SET_DATA_NONE | VFIO_IRQ_SET_ACTION_UNMASK,
                0,
                1,
                &[],
            )
            .map_err(VfioError::VfioUser)
    }
}

impl PciDevice for VfioUserPciDevice {
    fn allocate_bars(
        &mut self,
        allocator: &Arc<Mutex<SystemAllocator>>,
        mmio_allocator: &mut AddressAllocator,
        resources: Option<Vec<Resource>>,
    ) -> Result<Vec<PciBarConfiguration>, PciDeviceError> {
        self.common
            .allocate_bars(allocator, mmio_allocator, resources)
    }

    fn free_bars(
        &mut self,
        allocator: &mut SystemAllocator,
        mmio_allocator: &mut AddressAllocator,
    ) -> Result<(), PciDeviceError> {
        self.common.free_bars(allocator, mmio_allocator)
    }

    fn as_any(&mut self) -> &mut dyn Any {
        self
    }

    fn detect_bar_reprogramming(
        &mut self,
        reg_idx: usize,
        data: &[u8],
    ) -> Option<BarReprogrammingParams> {
        self.common
            .configuration
            .detect_bar_reprogramming(reg_idx, data)
    }

    fn write_config_register(
        &mut self,
        reg_idx: usize,
        offset: u64,
        data: &[u8],
    ) -> Option<Arc<Barrier>> {
        self.common.write_config_register(reg_idx, offset, data)
    }

    fn read_config_register(&mut self, reg_idx: usize) -> u32 {
        self.common.read_config_register(reg_idx)
    }

    fn read_bar(&mut self, base: u64, offset: u64, data: &mut [u8]) {
        self.common.read_bar(base, offset, data)
    }

    fn write_bar(&mut self, base: u64, offset: u64, data: &[u8]) -> Option<Arc<Barrier>> {
        self.common.write_bar(base, offset, data)
    }

    fn move_bar(&mut self, old_base: u64, new_base: u64) -> Result<(), std::io::Error> {
        info!("Moving BAR 0x{:x} -> 0x{:x}", old_base, new_base);
        for mmio_region in self.common.mmio_regions.iter_mut() {
            if mmio_region.start.raw_value() == old_base {
                mmio_region.start = GuestAddress(new_base);

                for user_memory_region in mmio_region.user_memory_regions.iter_mut() {
                    // Remove old region
                    let old_region = self.vm.make_user_memory_region(
                        user_memory_region.slot,
                        user_memory_region.start,
                        user_memory_region.size,
                        user_memory_region.host_addr,
                        false,
                        false,
                    );

                    self.vm
                        .remove_user_memory_region(old_region)
                        .map_err(|e| std::io::Error::new(std::io::ErrorKind::Other, e))?;

                    // Update the user memory region with the correct start address.
                    if new_base > old_base {
                        user_memory_region.start += new_base - old_base;
                    } else {
                        user_memory_region.start -= old_base - new_base;
                    }

                    // Insert new region
                    let new_region = self.vm.make_user_memory_region(
                        user_memory_region.slot,
                        user_memory_region.start,
                        user_memory_region.size,
                        user_memory_region.host_addr,
                        false,
                        false,
                    );

                    self.vm
                        .create_user_memory_region(new_region)
                        .map_err(|e| std::io::Error::new(std::io::ErrorKind::Other, e))?;
                }
                info!("Moved bar 0x{:x} -> 0x{:x}", old_base, new_base);
            }
        }

        Ok(())
    }

    fn id(&self) -> Option<String> {
        Some(self.id.clone())
    }
}

impl Drop for VfioUserPciDevice {
    fn drop(&mut self) {
        self.unmap_mmio_regions();

        if let Some(msix) = &self.common.interrupt.msix {
            if msix.bar.enabled() {
                self.common.disable_msix();
            }
        }

        if let Some(msi) = &self.common.interrupt.msi {
            if msi.cfg.enabled() {
                self.common.disable_msi()
            }
        }

        if self.common.interrupt.intx_in_use() {
            self.common.disable_intx();
        }

        if let Err(e) = self.client.lock().unwrap().shutdown() {
            error!("Failed shutting down vfio-user client: {}", e);
        }
    }
}

impl Pausable for VfioUserPciDevice {}

impl Snapshottable for VfioUserPciDevice {
    fn id(&self) -> String {
        self.id.clone()
    }

    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        let mut vfio_pci_dev_snapshot = Snapshot::new(&self.id);

        // Snapshot VfioCommon
        vfio_pci_dev_snapshot.add_snapshot(self.common.snapshot()?);

        Ok(vfio_pci_dev_snapshot)
    }

    fn restore(&mut self, snapshot: Snapshot) -> std::result::Result<(), MigratableError> {
        // Restore VfioCommon
        if let Some(vfio_common_snapshot) = snapshot.snapshots.get(&self.common.id()) {
            self.common.restore(*vfio_common_snapshot.clone())?;
            self.map_mmio_regions().map_err(|e| {
                MigratableError::Restore(anyhow!(
                    "Could not map MMIO regions for VfioUserPciDevice on restore {:?}",
                    e
                ))
            })?;
        }

        Ok(())
    }
}
impl Transportable for VfioUserPciDevice {}
impl Migratable for VfioUserPciDevice {}

pub struct VfioUserDmaMapping<M: GuestAddressSpace> {
    client: Arc<Mutex<Client>>,
    memory: Arc<M>,
}

impl<M: GuestAddressSpace> VfioUserDmaMapping<M> {
    pub fn new(client: Arc<Mutex<Client>>, memory: Arc<M>) -> Self {
        Self { client, memory }
    }
}

impl<M: GuestAddressSpace + Sync + Send> ExternalDmaMapping for VfioUserDmaMapping<M> {
    fn map(&self, iova: u64, gpa: u64, size: u64) -> std::result::Result<(), std::io::Error> {
        let mem = self.memory.memory();
        let guest_addr = GuestAddress(gpa);
        let region = mem.find_region(guest_addr);

        if let Some(region) = region {
            let file_offset = region.file_offset().unwrap();
            let offset = (GuestAddress(gpa).checked_offset_from(region.start_addr())).unwrap()
                + file_offset.start();

            self.client
                .lock()
                .unwrap()
                .dma_map(offset, iova, size, file_offset.file().as_raw_fd())
                .map_err(|e| {
                    std::io::Error::new(
                        std::io::ErrorKind::Other,
                        format!("Error mapping region: {}", e),
                    )
                })
        } else {
            Err(std::io::Error::new(
                std::io::ErrorKind::Other,
                format!("Region not found for 0x{:x}", gpa),
            ))
        }
    }

    fn unmap(&self, iova: u64, size: u64) -> std::result::Result<(), std::io::Error> {
        self.client
            .lock()
            .unwrap()
            .dma_unmap(iova, size)
            .map_err(|e| {
                std::io::Error::new(
                    std::io::ErrorKind::Other,
                    format!("Error unmapping region: {}", e),
                )
            })
    }
}

// Copyright © 2024 Tencent Corporation. All rights reserved.
//
// SPDX-License-Identifier: Apache-2.0
//

use std::any::Any;
use std::result;
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Arc, Barrier, Mutex};

use anyhow::anyhow;
use byteorder::{ByteOrder, LittleEndian};
use pci::{
    BarReprogrammingParams, PciBarConfiguration, PciBarPrefetchable, PciBarRegionType,
    PciClassCode, PciConfiguration, PciDevice, PciDeviceError, PciHeaderType, PciSubclass,
};
use serde::{Deserialize, Serialize};
use thiserror::Error;
use vm_allocator::{AddressAllocator, SystemAllocator};
use vm_device::{BusDevice, Resource};
use vm_memory::bitmap::AtomicBitmap;
use vm_memory::{Address, GuestAddress};
use vm_migration::{Migratable, MigratableError, Pausable, Snapshot, Snapshottable, Transportable};

const IVSHMEM_BAR0_IDX: usize = 0;
const IVSHMEM_BAR1_IDX: usize = 1;
const IVSHMEM_BAR2_IDX: usize = 2;

const IVSHMEM_VENDOR_ID: u16 = 0x1af4;
const IVSHMEM_DEVICE_ID: u16 = 0x1110;

const IVSHMEM_REG_BAR_SIZE: u64 = 0x100;

type GuestRegionMmap = vm_memory::GuestRegionMmap<AtomicBitmap>;

#[derive(Debug, Error)]
pub enum IvshmemError {
    #[error("Failed to retrieve PciConfigurationState: {0}")]
    RetrievePciConfigurationState(#[source] anyhow::Error),
    #[error("Failed to retrieve IvshmemDeviceState: {0}")]
    RetrieveIvshmemDeviceStateState(#[source] anyhow::Error),
}

#[derive(Copy, Clone)]
pub enum IvshmemSubclass {
    Other = 0x00,
}

impl PciSubclass for IvshmemSubclass {
    fn get_register_value(&self) -> u8 {
        *self as u8
    }
}

pub struct IvshmemDevice {
    id: String,

    // ivshmem device registers
    interrupt_mask: u32,
    interrupt_status: Arc<AtomicU32>,
    iv_position: u32,
    doorbell: u32,

    // PCI configuration registers.
    configuration: PciConfiguration,
    bar_regions: Vec<PciBarConfiguration>,

    region: Option<Arc<GuestRegionMmap>>,
    region_size: u64,
}

#[derive(Serialize, Deserialize, Default, Clone)]
pub struct IvshmemDeviceState {
    interrupt_mask: u32,
    interrupt_status: u32,
    iv_position: u32,
    doorbell: u32,
}

impl IvshmemDevice {
    pub fn new(
        id: String,
        region_size: u64,
        snapshot: Option<Snapshot>,
    ) -> Result<Self, IvshmemError> {
        let state: Option<IvshmemDeviceState> = snapshot
            .as_ref()
            .map(|s| s.to_state(&id))
            .transpose()
            .map_err(|e| {
                IvshmemError::RetrieveIvshmemDeviceStateState(anyhow!(
                    "Failed to get IvshmemDeviceState from Snapshot: {e}",
                ))
            })?;

        let mut configuration = PciConfiguration::new(
            IVSHMEM_VENDOR_ID,
            IVSHMEM_DEVICE_ID,
            0x1,
            PciClassCode::MemoryController,
            &IvshmemSubclass::Other,
            None,
            PciHeaderType::Device,
            0,
            0,
            None,
        );

        // When restoring, immediately apply the saved PciConfiguration state
        // (including the BAR2 address the guest last programmed) so that
        // `data_bar_addr()` returns the correct GPA before the VMM tries to
        // (re)create the host-side userspace mapping for BAR2 in
        // `add_ivshmem_device`. Without this, PciConfiguration only gets
        // restored later in `restore_devices`, and the initial ivshmem
        // mapping would be created at address 0 which breaks
        // snapshot/restore and live-migration.
        if let Some(snapshot) = snapshot.as_ref() {
            if let Some(pci_config_snapshot) = snapshot.snapshots.get(&configuration.id()) {
                configuration
                    .restore(*pci_config_snapshot.clone())
                    .map_err(|e| {
                        IvshmemError::RetrieveIvshmemDeviceStateState(anyhow!(
                            "Failed to restore PciConfiguration from Snapshot: {e}",
                        ))
                    })?;
            }
        }

        let device = if let Some(s) = state {
            IvshmemDevice {
                id,
                configuration,
                bar_regions: vec![],
                interrupt_mask: s.interrupt_mask,
                interrupt_status: Arc::new(AtomicU32::new(s.interrupt_status)),
                iv_position: s.iv_position,
                doorbell: s.doorbell,
                region_size,
                region: None,
            }
        } else {
            IvshmemDevice {
                id,
                configuration,
                bar_regions: vec![],
                interrupt_mask: 0,
                interrupt_status: Arc::new(AtomicU32::new(0)),
                iv_position: 0,
                doorbell: 0,
                region_size,
                region: None,
            }
        };
        Ok(device)
    }

    pub fn config_bar_addr(&self) -> u64 {
        self.configuration.get_bar_addr(IVSHMEM_BAR0_IDX)
    }

    pub fn data_bar_addr(&self) -> u64 {
        self.configuration.get_bar_addr(IVSHMEM_BAR2_IDX)
    }

    fn state(&self) -> IvshmemDeviceState {
        IvshmemDeviceState {
            interrupt_mask: self.interrupt_mask,
            interrupt_status: self.interrupt_status.load(Ordering::SeqCst),
            iv_position: self.iv_position,
            doorbell: self.doorbell,
        }
    }
}

impl BusDevice for IvshmemDevice {
    fn read(&mut self, base: u64, offset: u64, data: &mut [u8]) {
        self.read_bar(base, offset, data)
    }

    fn write(&mut self, base: u64, offset: u64, data: &[u8]) -> Option<Arc<Barrier>> {
        self.write_bar(base, offset, data)
    }
}

impl PciDevice for IvshmemDevice {
    fn allocate_bars(
        &mut self,
        allocator: &Arc<Mutex<SystemAllocator>>,
        mmio_allocator: &mut AddressAllocator,
        resources: Option<Vec<Resource>>,
    ) -> std::result::Result<Vec<PciBarConfiguration>, PciDeviceError> {
        let mut bars = Vec::new();
        let mut bar0_addr = None;
        let mut bar2_addr = None;

        let restoring = resources.is_some();
        if let Some(resources) = resources {
            for resource in resources {
                match resource {
                    Resource::PciBar { index, base, .. } => {
                        match index {
                            IVSHMEM_BAR0_IDX => {
                                bar0_addr = Some(GuestAddress(base));
                            }
                            IVSHMEM_BAR1_IDX => {}
                            IVSHMEM_BAR2_IDX => {
                                bar2_addr = Some(GuestAddress(base));
                            }
                            _ => {
                                error!("Unexpected pci bar index {index}");
                            }
                        };
                    }
                    _ => {
                        error!("Unexpected resource {resource:?}");
                    }
                }
            }
            if bar0_addr.is_none() || bar2_addr.is_none() {
                return Err(PciDeviceError::MissingResource);
            }
        }

        // BAR0 holds device registers (256 Byte MMIO)
        let bar0_addr = allocator
            .lock()
            .unwrap()
            .allocate_mmio_hole_addresses(bar0_addr, IVSHMEM_REG_BAR_SIZE, None)
            .ok_or(PciDeviceError::IoAllocationFailed(IVSHMEM_REG_BAR_SIZE))?;
        debug!("ivshmem bar0 address 0x{:x}", bar0_addr.0);

        let bar0 = PciBarConfiguration::default()
            .set_index(IVSHMEM_BAR0_IDX)
            .set_address(bar0_addr.raw_value())
            .set_size(IVSHMEM_REG_BAR_SIZE)
            .set_region_type(PciBarRegionType::Memory32BitRegion)
            .set_prefetchable(PciBarPrefetchable::NotPrefetchable);

        // BAR1 holds MSI-X table and PBA (only ivshmem-doorbell).

        // BAR2 maps the shared memory object
        let bar2_size = self.region_size;
        let bar2_addr = mmio_allocator
            .allocate(bar2_addr, bar2_size, None)
            .ok_or(PciDeviceError::IoAllocationFailed(bar2_size))?;
        debug!("ivshmem bar2 address 0x{:x}", bar2_addr.0);

        let bar2 = PciBarConfiguration::default()
            .set_index(IVSHMEM_BAR2_IDX)
            .set_address(bar2_addr.raw_value())
            .set_size(bar2_size)
            .set_region_type(PciBarRegionType::Memory64BitRegion)
            .set_prefetchable(PciBarPrefetchable::Prefetchable);

        if !restoring {
            self.configuration
                .add_pci_bar(&bar0)
                .map_err(|e| PciDeviceError::IoRegistrationFailed(bar0_addr.raw_value(), e))?;
            self.configuration
                .add_pci_bar(&bar2)
                .map_err(|e| PciDeviceError::IoRegistrationFailed(bar2_addr.raw_value(), e))?;
        }

        bars.push(bar0);
        bars.push(bar2);
        self.bar_regions = bars.clone();

        Ok(bars)
    }

    fn free_bars(
        &mut self,
        _allocator: &mut SystemAllocator,
        _mmio_allocator: &mut AddressAllocator,
    ) -> std::result::Result<(), PciDeviceError> {
        warn!("Device hotplug are not supported for ivshmem");
        Ok(())
    }

    fn write_config_register(
        &mut self,
        reg_idx: usize,
        offset: u64,
        data: &[u8],
    ) -> Option<Arc<Barrier>> {
        self.configuration
            .write_config_register(reg_idx, offset, data);
        None
    }

    fn detect_bar_reprogramming(
        &mut self,
        reg_idx: usize,
        data: &[u8],
    ) -> Option<BarReprogrammingParams> {
        self.configuration.detect_bar_reprogramming(reg_idx, data)
    }

    fn read_config_register(&mut self, reg_idx: usize) -> u32 {
        self.configuration.read_reg(reg_idx)
    }

    fn read_bar(&mut self, base: u64, offset: u64, data: &mut [u8]) {
        debug!("read base {base:x} offset {offset}");

        let mut bar_idx = 0;
        for (idx, bar) in self.bar_regions.iter().enumerate() {
            if bar.addr() == base {
                bar_idx = idx;
            }
        }
        match bar_idx {
            // bar 0
            0 => {
                // ivshmem don't use interrupt, we return zero now.
                LittleEndian::write_u32(data, 0);
            }
            // bar 2
            1 => warn!("Unexpected read ivshmem memory idx: {offset}"),
            _ => {
                warn!("Invalid bar_idx: {bar_idx}");
            }
        };
    }

    fn write_bar(&mut self, base: u64, offset: u64, _data: &[u8]) -> Option<Arc<Barrier>> {
        debug!("write base {base:x} offset {offset}");
        warn!("Unexpected write ivshmem memory idx: {offset}");
        None
    }

    fn move_bar(&mut self, old_base: u64, new_base: u64) -> result::Result<(), std::io::Error> {
        for bar in self.bar_regions.iter_mut() {
            if bar.addr() == old_base {
                *bar = bar.set_address(new_base);
            }
        }

        Ok(())
    }

    fn as_any(&mut self) -> &mut dyn Any {
        self
    }

    fn id(&self) -> Option<String> {
        Some(self.id.clone())
    }
}

impl Pausable for IvshmemDevice {}

impl Snapshottable for IvshmemDevice {
    fn id(&self) -> String {
        self.id.clone()
    }

    // The snapshot/restore (also live migration) support only work for ivshmem-plain mode.
    // Additional work is needed for supporting ivshmem-doorbell.
    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        let mut snapshot = Snapshot::new_from_state(&self.id, &self.state())?;

        // Snapshot PciConfiguration
        snapshot.add_snapshot(self.configuration.snapshot()?);

        Ok(snapshot)
    }
}

impl Transportable for IvshmemDevice {}

impl Migratable for IvshmemDevice {}

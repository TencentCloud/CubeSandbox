// Copyright © 2023 Tencent Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use event_monitor::event;
use pci::{
    BarReprogrammingParams, PciBarConfiguration, PciBarPrefetchable, PciBarRegionType,
    PciClassCode, PciConfiguration, PciDevice, PciDeviceError, PciHeaderType, PciSubclass,
};
use serde::{Deserialize, Serialize};
use std::any::Any;
use std::result;
use std::sync::{Arc, Barrier, Mutex};
use thiserror::Error;
use vm_allocator::{AddressAllocator, SystemAllocator};
use vm_device::{BusDevice, Resource};
use vm_memory::{Address, GuestAddress};
use vm_migration::{Migratable, MigratableError, Pausable, Snapshot, Snapshottable, Transportable};

const PVPANIC_VENDOR_ID: u16 = 0x1b36;
const PVPANIC_DEVICE_ID: u16 = 0x0011;

pub const PVPANIC_DEVICE_MMIO_SIZE: u64 = 0x2;

const PVPANIC_PANICKED: u8 = 1 << 0;
const PVPANIC_CRASH_LOADED: u8 = 1 << 1;

#[derive(Debug, Error)]
pub enum PvPanicError {
    #[error("Failed creating PvPanicDevice: {0}")]
    CreatePvPanicDevice(#[source] anyhow::Error),
    #[error("Failed to retrieve PciConfigurationState: {0}")]
    RetrievePciConfigurationState(#[source] anyhow::Error),
}

#[allow(dead_code)]
#[derive(Copy, Clone)]
pub enum PvPanicSubclass {
    Other = 0x80,
}

impl PciSubclass for PvPanicSubclass {
    fn get_register_value(&self) -> u8 {
        *self as u8
    }
}

/// A device for handling guest panic event
pub struct PvPanicDevice {
    id: String,
    events: u8,

    // PCI configuration registers.
    configuration: PciConfiguration,
    bar_regions: Vec<PciBarConfiguration>,
}

#[derive(Serialize, Deserialize)]
pub struct PvPanicDeviceState {
    events: u8,
}

impl PvPanicDevice {
    pub fn new(id: String, state: Option<PvPanicDeviceState>) -> Result<Self, PvPanicError> {
        let mut configuration = PciConfiguration::new(
            PVPANIC_VENDOR_ID,
            PVPANIC_DEVICE_ID,
            0x1, // modern pci devices
            PciClassCode::BaseSystemPeripheral,
            &PvPanicSubclass::Other,
            None,
            PciHeaderType::Device,
            0,
            0,
            None,
        );

        let command: [u8; 2] = [0x03, 0x01];
        configuration.write_config_register(1, 0, &command);

        let events = if let Some(state) = state {
            state.events
        } else {
            PVPANIC_PANICKED | PVPANIC_CRASH_LOADED
        };

        let pvpanic_device = PvPanicDevice {
            id,
            events,
            configuration,
            bar_regions: vec![],
        };

        Ok(pvpanic_device)
    }

    pub fn event_to_string(&self, event: u8) -> String {
        if event == PVPANIC_PANICKED {
            "panic".to_string()
        } else if event == PVPANIC_CRASH_LOADED {
            "crash_loaded".to_string()
        } else {
            "unknown_event".to_string()
        }
    }

    fn state(&self) -> PvPanicDeviceState {
        PvPanicDeviceState {
            events: self.events,
        }
    }

    pub fn config_bar_addr(&self) -> u64 {
        self.configuration.get_bar_addr(0)
    }
}

impl BusDevice for PvPanicDevice {
    fn read(&mut self, base: u64, offset: u64, data: &mut [u8]) {
        self.read_bar(base, offset, data)
    }

    fn write(&mut self, _base: u64, _offset: u64, data: &[u8]) -> Option<Arc<Barrier>> {
        let event = self.event_to_string(data[0]);
        warn!("pvpanic got guest event {}", event);
        event!("guest", "panic", "event", &event);
        None
    }
}

impl PciDevice for PvPanicDevice {
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

    fn read_config_register(&mut self, reg_idx: usize) -> u32 {
        self.configuration.read_reg(reg_idx)
    }

    fn detect_bar_reprogramming(
        &mut self,
        reg_idx: usize,
        data: &[u8],
    ) -> Option<BarReprogrammingParams> {
        self.configuration.detect_bar_reprogramming(reg_idx, data)
    }

    fn allocate_bars(
        &mut self,
        allocator: &Arc<Mutex<SystemAllocator>>,
        _mmio_allocator: &mut AddressAllocator,
        resources: Option<Vec<Resource>>,
    ) -> std::result::Result<Vec<PciBarConfiguration>, PciDeviceError> {
        let mut bars = Vec::new();
        let region_type = PciBarRegionType::Memory32BitRegion;
        let bar_id = 0;
        let region_size = PVPANIC_DEVICE_MMIO_SIZE;
        let restoring = resources.is_some();
        let bar_addr = allocator
            .lock()
            .unwrap()
            .allocate_mmio_hole_addresses(None, region_size, None)
            .ok_or(PciDeviceError::IoAllocationFailed(region_size))?;

        let bar = PciBarConfiguration::default()
            .set_index(bar_id as usize)
            .set_address(bar_addr.raw_value())
            .set_size(region_size)
            .set_region_type(region_type)
            .set_prefetchable(PciBarPrefetchable::NotPrefetchable);

        debug!("pvpanic bar address 0x{:x}", bar_addr.0);
        if !restoring {
            self.configuration
                .add_pci_bar(&bar)
                .map_err(|e| PciDeviceError::IoRegistrationFailed(bar_addr.raw_value(), e))?;
        }

        bars.push(bar);
        self.bar_regions = bars.clone();

        Ok(bars)
    }

    fn free_bars(
        &mut self,
        allocator: &mut SystemAllocator,
        _mmio_allocator: &mut AddressAllocator,
    ) -> std::result::Result<(), PciDeviceError> {
        for bar in self.bar_regions.drain(..) {
            allocator.free_mmio_hole_addresses(GuestAddress(bar.addr()), bar.size());
        }

        Ok(())
    }

    fn move_bar(&mut self, old_base: u64, new_base: u64) -> result::Result<(), std::io::Error> {
        for bar in self.bar_regions.iter_mut() {
            if bar.addr() == old_base {
                *bar = bar.set_address(new_base);
            }
        }

        Ok(())
    }

    fn read_bar(&mut self, _base: u64, _offset: u64, data: &mut [u8]) {
        data[0] = self.events;
    }

    fn as_any(&mut self) -> &mut dyn Any {
        self
    }

    fn id(&self) -> Option<String> {
        Some(self.id.clone())
    }
}

impl Pausable for PvPanicDevice {}

impl Snapshottable for PvPanicDevice {
    fn id(&self) -> String {
        self.id.clone()
    }

    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        let mut snapshot = Snapshot::new_from_state(&self.id, &self.state())?;

        // Snapshot PciConfiguration
        snapshot.add_snapshot(self.configuration.snapshot()?);

        Ok(snapshot)
    }

    fn restore(&mut self, snapshot: Snapshot) -> std::result::Result<(), MigratableError> {
        // Restore PciConfiguration
        if let Some(pci_config_snapshot) = snapshot.snapshots.get(&self.configuration.id()) {
            self.configuration.restore(*pci_config_snapshot.clone())?;
        }

        Ok(())
    }
}

impl Transportable for PvPanicDevice {}
impl Migratable for PvPanicDevice {}

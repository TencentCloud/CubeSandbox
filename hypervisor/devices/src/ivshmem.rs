// Copyright © 2024 Tencent Corporation. All rights reserved.
//
// SPDX-License-Identifier: Apache-2.0
//

use byteorder::{ByteOrder, LittleEndian};
use pci::{
    BarReprogrammingParams, PciBarConfiguration, PciBarPrefetchable, PciBarRegionType,
    PciClassCode, PciConfiguration, PciDevice, PciDeviceError, PciHeaderType, PciSubclass,
};
use serde::{Deserialize, Serialize};
use std::any::Any;
use std::result;
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Arc, Barrier, Mutex};
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

///
/// ```text
/// Offset  Size  Access      On reset  Function
///     0     4   read/write        0   Interrupt Mask
///                                     bit 0: peer interrupt (rev 0)
///                                            reserved       (rev 1)
///                                     bit 1..31: reserved
///     4     4   read/write        0   Interrupt Status
///                                     bit 0: peer interrupt (rev 0)
///                                            reserved       (rev 1)
///                                     bit 1..31: reserved
///     8     4   read-only   0 or ID   IVPosition
///    12     4   write-only      N/A   Doorbell
///                                     bit 0..15: vector
///                                     bit 16..31: peer ID
///    16   240   none            N/A   reserved
/// ```
///

const IVSHMEM_REG_INTERRUPT_MASK: u64 = 0;
const IVSHMEM_REG_INTERRUPT_STATUS: u64 = 4;
const IVSHMEM_REG_IV_POSITION: u64 = 8;
const IVSHMEM_REG_DOORBELL: u64 = 12;

type GuestRegionMmap = vm_memory::GuestRegionMmap<AtomicBitmap>;

#[allow(dead_code)]
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
    pub fn new(id: String, state: Option<IvshmemDeviceState>, region_size: u64) -> Self {
        let configuration = PciConfiguration::new(
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

        if let Some(s) = state {
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
        }
    }

    pub fn config_bar_addr(&self) -> u64 {
        self.configuration.get_bar_addr(0)
    }

    pub fn data_bar_addr(&self) -> u64 {
        self.configuration.get_bar_addr(2)
    }

    pub fn assign_region(&mut self, region: Arc<GuestRegionMmap>) {
        self.region = Some(region);
    }

    fn state(&self) -> IvshmemDeviceState {
        IvshmemDeviceState {
            interrupt_mask: self.interrupt_mask,
            interrupt_status: self.interrupt_status.load(Ordering::SeqCst),
            iv_position: self.iv_position,
            doorbell: self.doorbell,
        }
    }

    fn set_state(&mut self, state: &IvshmemDeviceState) {
        self.interrupt_mask = state.interrupt_mask;
        self.interrupt_status = Arc::new(AtomicU32::new(state.interrupt_status));
        self.iv_position = state.iv_position;
        self.doorbell = state.doorbell;
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
        _mmio_allocator: &mut AddressAllocator,
        resources: Option<Vec<Resource>>,
    ) -> std::result::Result<Vec<PciBarConfiguration>, PciDeviceError> {
        let mut bars = Vec::new();
        let mut bar0_addr = None;
        let mut bar2_addr = None;

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

        let bar0 = PciBarConfiguration::default()
            .set_index(IVSHMEM_BAR0_IDX)
            .set_address(bar0_addr.raw_value())
            .set_size(IVSHMEM_REG_BAR_SIZE)
            .set_region_type(PciBarRegionType::Memory32BitRegion)
            .set_prefetchable(PciBarPrefetchable::NotPrefetchable);

        debug!("ivshmem bar0 address 0x{:x}", bar0_addr.0);
        self.configuration
            .add_pci_bar(&bar0)
            .map_err(|e| PciDeviceError::IoRegistrationFailed(bar0_addr.raw_value(), e))?;

        // BAR1 holds MSI-X table and PBA (only ivshmem-doorbell).

        // BAR2 maps the shared memory object
        let bar2_size = self.region_size;
        let bar2_addr = allocator
            .lock()
            .unwrap()
            .allocate_mmio_hole_addresses(bar2_addr, bar2_size, None)
            .ok_or(PciDeviceError::IoAllocationFailed(bar2_size))?;

        let bar2 = PciBarConfiguration::default()
            .set_index(IVSHMEM_BAR2_IDX)
            .set_address(bar2_addr.raw_value())
            .set_size(bar2_size)
            .set_region_type(PciBarRegionType::Memory64BitRegion)
            .set_prefetchable(PciBarPrefetchable::Prefetchable);

        debug!("ivshmem bar2 address 0x{:x}", bar2_addr.0);
        self.configuration
            .add_pci_bar(&bar2)
            .map_err(|e| PciDeviceError::IoRegistrationFailed(bar2_addr.raw_value(), e))?;

        bars.push(bar0);
        bars.push(bar2);
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
                let v = match offset {
                    IVSHMEM_REG_INTERRUPT_MASK => self.interrupt_mask,
                    IVSHMEM_REG_INTERRUPT_STATUS => self.interrupt_status.load(Ordering::SeqCst),
                    IVSHMEM_REG_IV_POSITION => self.iv_position,
                    IVSHMEM_REG_DOORBELL => self.doorbell,
                    _ => {
                        warn!("Unknown offset: {offset}");
                        0u32
                    }
                };
                LittleEndian::write_u32(data, v);
            }
            // bar 2
            1 => warn!("unexpect read ivshmem memory idx: {offset}"),
            _ => {
                warn!("invalid bar_idx: {bar_idx}");
            }
        };
    }

    fn write_bar(&mut self, base: u64, offset: u64, _data: &[u8]) -> Option<Arc<Barrier>> {
        debug!("write base {base:x} offset {offset}");
        warn!("unexpect write ivshmem memory idx: {offset}");
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

    fn snapshot(&mut self) -> Result<Snapshot, MigratableError> {
        let mut snapshot = Snapshot::new_from_state(&self.id, &self.state())?;

        snapshot.add_snapshot(self.configuration.snapshot()?);

        Ok(snapshot)
    }

    fn restore(&mut self, snapshot: Snapshot) -> Result<(), MigratableError> {
        self.set_state(&snapshot.to_state(&self.id)?);
        if let Some(pci_config_snapshot) = snapshot.snapshots.get(&self.configuration.id()) {
            self.configuration.restore(*pci_config_snapshot.clone())?;
        }
        Ok(())
    }
}

impl Transportable for IvshmemDevice {}
impl Migratable for IvshmemDevice {}

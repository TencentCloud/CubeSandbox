// Copyright 2018 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.
//
// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause

use super::VirtioPciCommonConfig;
use crate::transport::VirtioTransport;
use crate::GuestMemoryMmap;
use crate::{
    ActivateResult, VirtioDevice, VirtioDeviceType, VirtioInterrupt, VirtioInterruptType,
    DEVICE_ACKNOWLEDGE, DEVICE_DRIVER, DEVICE_DRIVER_OK, DEVICE_FAILED, DEVICE_FEATURES_OK,
    DEVICE_INIT,
};
use anyhow::anyhow;
use libc::EFD_NONBLOCK;
use pci::{
    BarReprogrammingParams, MsixCap, MsixConfig, PciBarConfiguration, PciBarRegionType,
    PciCapability, PciCapabilityId, PciClassCode, PciConfiguration, PciDevice, PciDeviceError,
    PciHeaderType, PciMassStorageSubclass, PciNetworkControllerSubclass, PciSubclass,
};
use serde::{Deserialize, Serialize};
use std::any::Any;
use std::cmp;
use std::io::Write;
use std::ops::Deref;
use std::result;
use std::sync::atomic::{AtomicBool, AtomicU16, AtomicUsize, Ordering};
use std::sync::{Arc, Barrier, Mutex};
use virtio_queue::{Error as QueueError, Queue, QueueT};
use vm_allocator::{AddressAllocator, SystemAllocator};
use vm_device::dma_mapping::ExternalDmaMapping;
use vm_device::interrupt::{
    InterruptIndex, InterruptManager, InterruptSourceGroup, MsiIrqGroupConfig,
};
use vm_device::{BusDevice, PciBarType, Resource};
use vm_memory::{Address, ByteValued, GuestAddress, GuestAddressSpace, GuestMemoryAtomic, Le32};
use vm_migration::{Migratable, MigratableError, Pausable, Snapshot, Snapshottable, Transportable};
use vm_virtio::AccessPlatform;
use vmm_sys_util::{errno::Result, eventfd::EventFd};

/// Vector value used to disable MSI for a queue.
const VIRTQ_MSI_NO_VECTOR: u16 = 0xffff;

pub enum Error {
    /// Failed to retrieve queue ring's index.
    QueueRingIndex(QueueError),
    /// Missing virtqueue state in snapshot.
    QueueStateMissing(usize),
}

impl std::fmt::Debug for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::QueueRingIndex(e) => f.debug_tuple("QueueRingIndex").field(e).finish(),
            Self::QueueStateMissing(i) => f.debug_tuple("QueueStateMissing").field(i).finish(),
        }
    }
}

#[allow(clippy::enum_variant_names)]
enum PciCapabilityType {
    CommonConfig = 1,
    NotifyConfig = 2,
    IsrConfig = 3,
    DeviceConfig = 4,
    PciConfig = 5,
    SharedMemoryConfig = 8,
}

// This offset represents the 2 bytes omitted from the VirtioPciCap structure
// as they are already handled through add_capability(). These 2 bytes are the
// fields cap_vndr (1 byte) and cap_next (1 byte) defined in the virtio spec.
const VIRTIO_PCI_CAP_OFFSET: usize = 2;

#[allow(dead_code)]
#[repr(packed)]
#[derive(Clone, Copy, Default)]
struct VirtioPciCap {
    cap_len: u8,      // Generic PCI field: capability length
    cfg_type: u8,     // Identifies the structure.
    pci_bar: u8,      // Where to find it.
    id: u8,           // Multiple capabilities of the same type
    padding: [u8; 2], // Pad to full dword.
    offset: Le32,     // Offset within bar.
    length: Le32,     // Length of the structure, in bytes.
}
// SAFETY: All members are simple numbers and any value is valid.
unsafe impl ByteValued for VirtioPciCap {}

impl PciCapability for VirtioPciCap {
    fn bytes(&self) -> &[u8] {
        self.as_slice()
    }

    fn id(&self) -> PciCapabilityId {
        PciCapabilityId::VendorSpecific
    }
}

const VIRTIO_PCI_CAP_LEN_OFFSET: u8 = 2;

impl VirtioPciCap {
    pub fn new(cfg_type: PciCapabilityType, pci_bar: u8, offset: u32, length: u32) -> Self {
        VirtioPciCap {
            cap_len: (std::mem::size_of::<VirtioPciCap>() as u8) + VIRTIO_PCI_CAP_LEN_OFFSET,
            cfg_type: cfg_type as u8,
            pci_bar,
            id: 0,
            padding: [0; 2],
            offset: Le32::from(offset),
            length: Le32::from(length),
        }
    }
}

#[allow(dead_code)]
#[repr(packed)]
#[derive(Clone, Copy, Default)]
struct VirtioPciNotifyCap {
    cap: VirtioPciCap,
    notify_off_multiplier: Le32,
}
// SAFETY: All members are simple numbers and any value is valid.
unsafe impl ByteValued for VirtioPciNotifyCap {}

impl PciCapability for VirtioPciNotifyCap {
    fn bytes(&self) -> &[u8] {
        self.as_slice()
    }

    fn id(&self) -> PciCapabilityId {
        PciCapabilityId::VendorSpecific
    }
}

impl VirtioPciNotifyCap {
    pub fn new(
        cfg_type: PciCapabilityType,
        pci_bar: u8,
        offset: u32,
        length: u32,
        multiplier: Le32,
    ) -> Self {
        VirtioPciNotifyCap {
            cap: VirtioPciCap {
                cap_len: (std::mem::size_of::<VirtioPciNotifyCap>() as u8)
                    + VIRTIO_PCI_CAP_LEN_OFFSET,
                cfg_type: cfg_type as u8,
                pci_bar,
                id: 0,
                padding: [0; 2],
                offset: Le32::from(offset),
                length: Le32::from(length),
            },
            notify_off_multiplier: multiplier,
        }
    }
}

#[allow(dead_code)]
#[repr(packed)]
#[derive(Clone, Copy, Default)]
struct VirtioPciCap64 {
    cap: VirtioPciCap,
    offset_hi: Le32,
    length_hi: Le32,
}
// SAFETY: All members are simple numbers and any value is valid.
unsafe impl ByteValued for VirtioPciCap64 {}

impl PciCapability for VirtioPciCap64 {
    fn bytes(&self) -> &[u8] {
        self.as_slice()
    }

    fn id(&self) -> PciCapabilityId {
        PciCapabilityId::VendorSpecific
    }
}

impl VirtioPciCap64 {
    pub fn new(cfg_type: PciCapabilityType, pci_bar: u8, id: u8, offset: u64, length: u64) -> Self {
        VirtioPciCap64 {
            cap: VirtioPciCap {
                cap_len: (std::mem::size_of::<VirtioPciCap64>() as u8) + VIRTIO_PCI_CAP_LEN_OFFSET,
                cfg_type: cfg_type as u8,
                pci_bar,
                id,
                padding: [0; 2],
                offset: Le32::from(offset as u32),
                length: Le32::from(length as u32),
            },
            offset_hi: Le32::from((offset >> 32) as u32),
            length_hi: Le32::from((length >> 32) as u32),
        }
    }
}

#[allow(dead_code)]
#[repr(packed)]
#[derive(Clone, Copy, Default)]
struct VirtioPciCfgCap {
    cap: VirtioPciCap,
    pci_cfg_data: [u8; 4],
}
// SAFETY: All members are simple numbers and any value is valid.
unsafe impl ByteValued for VirtioPciCfgCap {}

impl PciCapability for VirtioPciCfgCap {
    fn bytes(&self) -> &[u8] {
        self.as_slice()
    }

    fn id(&self) -> PciCapabilityId {
        PciCapabilityId::VendorSpecific
    }
}

impl VirtioPciCfgCap {
    fn new() -> Self {
        VirtioPciCfgCap {
            cap: VirtioPciCap::new(PciCapabilityType::PciConfig, 0, 0, 0),
            ..Default::default()
        }
    }
}

#[derive(Clone, Copy, Default)]
struct VirtioPciCfgCapInfo {
    offset: usize,
    cap: VirtioPciCfgCap,
}

#[allow(dead_code)]
#[derive(Copy, Clone)]
pub enum PciVirtioSubclass {
    NonTransitionalBase = 0xff,
}

impl PciSubclass for PciVirtioSubclass {
    fn get_register_value(&self) -> u8 {
        *self as u8
    }
}

// Allocate one bar for the structs pointed to by the capability structures.
// As per the PCI specification, because the same BAR shares MSI-X and non
// MSI-X structures, it is recommended to use 8KiB alignment for all those
// structures.
const COMMON_CONFIG_BAR_OFFSET: u64 = 0x0000;
const COMMON_CONFIG_SIZE: u64 = 56;
const ISR_CONFIG_BAR_OFFSET: u64 = 0x2000;
const ISR_CONFIG_SIZE: u64 = 1;
const DEVICE_CONFIG_BAR_OFFSET: u64 = 0x4000;
const DEVICE_CONFIG_SIZE: u64 = 0x1000;
const NOTIFICATION_BAR_OFFSET: u64 = 0x6000;
const NOTIFICATION_SIZE: u64 = 0x1000;
const MSIX_TABLE_BAR_OFFSET: u64 = 0x8000;
// The size is 256KiB because the table can hold up to 2048 entries, with each
// entry being 128 bits (4 DWORDS).
const MSIX_TABLE_SIZE: u64 = 0x40000;
const MSIX_PBA_BAR_OFFSET: u64 = 0x48000;
// The size is 2KiB because the Pending Bit Array has one bit per vector and it
// can support up to 2048 vectors.
const MSIX_PBA_SIZE: u64 = 0x800;
// The BAR size must be a power of 2.
const CAPABILITY_BAR_SIZE: u64 = 0x80000;
const VIRTIO_COMMON_BAR_INDEX: usize = 0;
const VIRTIO_SHM_BAR_INDEX: usize = 2;

const NOTIFY_OFF_MULTIPLIER: u32 = 4; // A dword per notification address.

const VIRTIO_PCI_VENDOR_ID: u16 = 0x1af4;
const VIRTIO_PCI_DEVICE_ID_BASE: u16 = 0x1040; // Add to device type to get device ID.

#[derive(Serialize, Deserialize)]
struct QueueState {
    max_size: u16,
    size: u16,
    ready: bool,
    desc_table: u64,
    avail_ring: u64,
    used_ring: u64,
}

#[derive(Serialize, Deserialize)]
struct VirtioPciDeviceState {
    device_activated: bool,
    queues: Vec<QueueState>,
    interrupt_status: usize,
}

pub struct VirtioPciDeviceActivator {
    interrupt: Option<Arc<dyn VirtioInterrupt>>,
    memory: Option<GuestMemoryAtomic<GuestMemoryMmap>>,
    device: Arc<Mutex<dyn VirtioDevice>>,
    device_activated: Arc<AtomicBool>,
    queues: Option<Vec<(usize, Queue, EventFd)>>,
    barrier: Option<Arc<Barrier>>,
    id: String,
}

impl VirtioPciDeviceActivator {
    pub fn activate(&mut self) -> ActivateResult {
        self.device.lock().unwrap().activate(
            self.memory.take().unwrap(),
            self.interrupt.take().unwrap(),
            self.queues.take().unwrap(),
        )?;
        self.device_activated.store(true, Ordering::SeqCst);

        if let Some(barrier) = self.barrier.take() {
            info!("{}: Waiting for barrier", self.id);
            barrier.wait();
            info!("{}: Barrier released", self.id);
        }

        Ok(())
    }
}

pub struct VirtioPciDevice {
    id: String,

    // PCI configuration registers.
    configuration: PciConfiguration,

    // virtio PCI common configuration
    common_config: VirtioPciCommonConfig,

    // MSI-X config
    msix_config: Option<Arc<Mutex<MsixConfig>>>,

    // Number of MSI-X vectors
    msix_num: u16,

    // Virtio device reference and status
    device: Arc<Mutex<dyn VirtioDevice>>,
    device_activated: Arc<AtomicBool>,

    // PCI interrupts.
    interrupt_status: Arc<AtomicUsize>,
    virtio_interrupt: Option<Arc<dyn VirtioInterrupt>>,
    interrupt_source_group: Arc<dyn InterruptSourceGroup>,

    // virtio queues
    queues: Vec<Queue>,
    queue_evts: Vec<EventFd>,

    // Guest memory
    memory: GuestMemoryAtomic<GuestMemoryMmap>,

    // Settings PCI BAR
    settings_bar: u8,

    // Whether to use 64-bit bar location or 32-bit
    use_64bit_bar: bool,

    // Add a dedicated structure to hold information about the very specific
    // virtio-pci capability VIRTIO_PCI_CAP_PCI_CFG. This is needed to support
    // the legacy/backward compatible mechanism of letting the guest access the
    // other virtio capabilities without mapping the PCI BARs. This can be
    // needed when the guest tries to early access the virtio configuration of
    // a device.
    cap_pci_cfg_info: VirtioPciCfgCapInfo,

    // Details of bar regions to free
    bar_regions: Vec<PciBarConfiguration>,

    // EventFd to signal on to request activation
    activate_evt: EventFd,

    // Optional DMA handler
    dma_handler: Option<Arc<dyn ExternalDmaMapping>>,

    // Pending activations
    pending_activations: Arc<Mutex<Vec<VirtioPciDeviceActivator>>>,
}

impl VirtioPciDevice {
    /// Constructs a new PCI transport for the given virtio device.
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        id: String,
        memory: GuestMemoryAtomic<GuestMemoryMmap>,
        device: Arc<Mutex<dyn VirtioDevice>>,
        msix_num: u16,
        access_platform: Option<Arc<dyn AccessPlatform>>,
        interrupt_manager: &Arc<dyn InterruptManager<GroupConfig = MsiIrqGroupConfig>>,
        pci_device_bdf: u32,
        activate_evt: EventFd,
        use_64bit_bar: bool,
        dma_handler: Option<Arc<dyn ExternalDmaMapping>>,
        pending_activations: Arc<Mutex<Vec<VirtioPciDeviceActivator>>>,
    ) -> Result<Self> {
        let device_clone = device.clone();
        let mut locked_device = device_clone.lock().unwrap();
        let mut queue_evts = Vec::new();
        for _ in locked_device.queue_max_sizes().iter() {
            queue_evts.push(EventFd::new(EFD_NONBLOCK)?)
        }
        let num_queues = locked_device.queue_max_sizes().len();

        if let Some(access_platform) = &access_platform {
            locked_device.set_access_platform(access_platform.clone());
        }

        let queues = locked_device
            .queue_max_sizes()
            .iter()
            .map(|&s| Queue::new(s).unwrap())
            .collect();

        let pci_device_id = VIRTIO_PCI_DEVICE_ID_BASE + locked_device.device_type() as u16;

        let interrupt_source_group = interrupt_manager.create_group(MsiIrqGroupConfig {
            base: 0,
            count: msix_num as InterruptIndex,
        })?;

        let (msix_config, msix_config_clone) = if msix_num > 0 {
            let msix_config = Arc::new(Mutex::new(MsixConfig::new(
                msix_num,
                interrupt_source_group.clone(),
                pci_device_bdf,
            )));
            let msix_config_clone = msix_config.clone();
            (Some(msix_config), Some(msix_config_clone))
        } else {
            (None, None)
        };

        let (class, subclass) = match VirtioDeviceType::from(locked_device.device_type()) {
            VirtioDeviceType::Net => (
                PciClassCode::NetworkController,
                &PciNetworkControllerSubclass::EthernetController as &dyn PciSubclass,
            ),
            VirtioDeviceType::Block => (
                PciClassCode::MassStorage,
                &PciMassStorageSubclass::MassStorage as &dyn PciSubclass,
            ),
            _ => (
                PciClassCode::Other,
                &PciVirtioSubclass::NonTransitionalBase as &dyn PciSubclass,
            ),
        };

        let configuration = PciConfiguration::new(
            VIRTIO_PCI_VENDOR_ID,
            pci_device_id,
            0x1, // For modern virtio-PCI devices
            class,
            subclass,
            None,
            PciHeaderType::Device,
            VIRTIO_PCI_VENDOR_ID,
            pci_device_id,
            msix_config_clone,
        );

        let mut virtio_pci_device = VirtioPciDevice {
            id,
            configuration,
            common_config: VirtioPciCommonConfig {
                access_platform,
                driver_status: 0,
                config_generation: 0,
                device_feature_select: 0,
                driver_feature_select: 0,
                queue_select: 0,
                msix_config: Arc::new(AtomicU16::new(VIRTQ_MSI_NO_VECTOR)),
                msix_queues: Arc::new(Mutex::new(vec![VIRTQ_MSI_NO_VECTOR; num_queues])),
            },
            msix_config,
            msix_num,
            device,
            device_activated: Arc::new(AtomicBool::new(false)),
            interrupt_status: Arc::new(AtomicUsize::new(0)),
            virtio_interrupt: None,
            queues,
            queue_evts,
            memory,
            settings_bar: 0,
            use_64bit_bar,
            interrupt_source_group,
            cap_pci_cfg_info: VirtioPciCfgCapInfo::default(),
            bar_regions: vec![],
            activate_evt,
            dma_handler,
            pending_activations,
        };

        if let Some(msix_config) = &virtio_pci_device.msix_config {
            virtio_pci_device.virtio_interrupt = Some(Arc::new(VirtioInterruptMsix::new(
                msix_config.clone(),
                virtio_pci_device.common_config.msix_config.clone(),
                virtio_pci_device.common_config.msix_queues.clone(),
                virtio_pci_device.interrupt_source_group.clone(),
            )));
        }

        Ok(virtio_pci_device)
    }

    fn state(&self) -> VirtioPciDeviceState {
        VirtioPciDeviceState {
            device_activated: self.device_activated.load(Ordering::Acquire),
            interrupt_status: self.interrupt_status.load(Ordering::Acquire),
            queues: self
                .queues
                .iter()
                .map(|q| QueueState {
                    max_size: q.max_size(),
                    size: q.size(),
                    ready: q.ready(),
                    desc_table: q.desc_table(),
                    avail_ring: q.avail_ring(),
                    used_ring: q.used_ring(),
                })
                .collect(),
        }
    }

    fn set_state(&mut self, state: &VirtioPciDeviceState) -> std::result::Result<(), Error> {
        self.device_activated
            .store(state.device_activated, Ordering::Release);
        self.interrupt_status
            .store(state.interrupt_status, Ordering::Release);

        // Update virtqueues indexes for both available and used rings.
        for (i, queue) in self.queues.iter_mut().enumerate() {
            let queue_state = state.queues.get(i).ok_or(Error::QueueStateMissing(i))?;

            queue.set_size(queue_state.size);
            queue.set_ready(queue_state.ready);

            if !queue_state.ready {
                continue;
            }

            queue
                .try_set_desc_table_address(GuestAddress(queue_state.desc_table))
                .unwrap();
            queue
                .try_set_avail_ring_address(GuestAddress(queue_state.avail_ring))
                .unwrap();
            queue
                .try_set_used_ring_address(GuestAddress(queue_state.used_ring))
                .unwrap();
            queue.set_next_avail(
                queue
                    .used_idx(self.memory.memory().deref(), Ordering::Acquire)
                    .map_err(Error::QueueRingIndex)?
                    .0,
            );
            queue.set_next_used(
                queue
                    .used_idx(self.memory.memory().deref(), Ordering::Acquire)
                    .map_err(Error::QueueRingIndex)?
                    .0,
            );
        }

        Ok(())
    }

    /// Gets the list of queue events that must be triggered whenever the VM writes to
    /// `virtio::NOTIFY_REG_OFFSET` past the MMIO base. Each event must be triggered when the
    /// value being written equals the index of the event in this list.
    fn queue_evts(&self) -> &[EventFd] {
        self.queue_evts.as_slice()
    }

    fn is_driver_ready(&self) -> bool {
        let ready_bits =
            (DEVICE_ACKNOWLEDGE | DEVICE_DRIVER | DEVICE_DRIVER_OK | DEVICE_FEATURES_OK) as u8;
        self.common_config.driver_status == ready_bits
            && self.common_config.driver_status & DEVICE_FAILED as u8 == 0
    }

    /// Determines if the driver has requested the device (re)init / reset itself
    fn is_driver_init(&self) -> bool {
        self.common_config.driver_status == DEVICE_INIT as u8
    }

    pub fn config_bar_addr(&self) -> u64 {
        self.configuration.get_bar_addr(self.settings_bar as usize)
    }

    fn add_pci_capabilities(
        &mut self,
        settings_bar: u8,
    ) -> std::result::Result<(), PciDeviceError> {
        // Add pointers to the different configuration structures from the PCI capabilities.
        let common_cap = VirtioPciCap::new(
            PciCapabilityType::CommonConfig,
            settings_bar,
            COMMON_CONFIG_BAR_OFFSET as u32,
            COMMON_CONFIG_SIZE as u32,
        );
        self.configuration
            .add_capability(&common_cap)
            .map_err(PciDeviceError::CapabilitiesSetup)?;

        let isr_cap = VirtioPciCap::new(
            PciCapabilityType::IsrConfig,
            settings_bar,
            ISR_CONFIG_BAR_OFFSET as u32,
            ISR_CONFIG_SIZE as u32,
        );
        self.configuration
            .add_capability(&isr_cap)
            .map_err(PciDeviceError::CapabilitiesSetup)?;

        // TODO(dgreid) - set based on device's configuration size?
        let device_cap = VirtioPciCap::new(
            PciCapabilityType::DeviceConfig,
            settings_bar,
            DEVICE_CONFIG_BAR_OFFSET as u32,
            DEVICE_CONFIG_SIZE as u32,
        );
        self.configuration
            .add_capability(&device_cap)
            .map_err(PciDeviceError::CapabilitiesSetup)?;

        let notify_cap = VirtioPciNotifyCap::new(
            PciCapabilityType::NotifyConfig,
            settings_bar,
            NOTIFICATION_BAR_OFFSET as u32,
            NOTIFICATION_SIZE as u32,
            Le32::from(NOTIFY_OFF_MULTIPLIER),
        );
        self.configuration
            .add_capability(&notify_cap)
            .map_err(PciDeviceError::CapabilitiesSetup)?;

        let configuration_cap = VirtioPciCfgCap::new();
        self.cap_pci_cfg_info.offset = self
            .configuration
            .add_capability(&configuration_cap)
            .map_err(PciDeviceError::CapabilitiesSetup)?
            + VIRTIO_PCI_CAP_OFFSET;
        self.cap_pci_cfg_info.cap = configuration_cap;

        if self.msix_config.is_some() {
            let msix_cap = MsixCap::new(
                settings_bar,
                self.msix_num,
                MSIX_TABLE_BAR_OFFSET as u32,
                settings_bar,
                MSIX_PBA_BAR_OFFSET as u32,
            );
            self.configuration
                .add_capability(&msix_cap)
                .map_err(PciDeviceError::CapabilitiesSetup)?;
        }

        self.settings_bar = settings_bar;
        Ok(())
    }

    fn read_cap_pci_cfg(&mut self, offset: usize, mut data: &mut [u8]) {
        let cap_slice = self.cap_pci_cfg_info.cap.as_slice();
        let data_len = data.len();
        let cap_len = cap_slice.len();
        if offset + data_len > cap_len {
            error!("Failed to read cap_pci_cfg from config space");
            return;
        }

        if offset < std::mem::size_of::<VirtioPciCap>() {
            if let Some(end) = offset.checked_add(data_len) {
                // This write can't fail, offset and end are checked against config_len.
                data.write_all(&cap_slice[offset..cmp::min(end, cap_len)])
                    .unwrap();
            }
        } else {
            // Safe since we know self.cap_pci_cfg_info.cap.cap.offset is 32bits long.
            let bar_offset: u32 =
                unsafe { std::mem::transmute(self.cap_pci_cfg_info.cap.cap.offset) };
            self.read_bar(0, bar_offset as u64, data)
        }
    }

    fn write_cap_pci_cfg(&mut self, offset: usize, data: &[u8]) -> Option<Arc<Barrier>> {
        let cap_slice = self.cap_pci_cfg_info.cap.as_mut_slice();
        let data_len = data.len();
        let cap_len = cap_slice.len();
        if offset + data_len > cap_len {
            error!("Failed to write cap_pci_cfg to config space");
            return None;
        }

        if offset < std::mem::size_of::<VirtioPciCap>() {
            let (_, right) = cap_slice.split_at_mut(offset);
            right[..data_len].copy_from_slice(data);
            None
        } else {
            // Safe since we know self.cap_pci_cfg_info.cap.cap.offset is 32bits long.
            let bar_offset: u32 =
                unsafe { std::mem::transmute(self.cap_pci_cfg_info.cap.cap.offset) };
            self.write_bar(0, bar_offset as u64, data)
        }
    }

    pub fn virtio_device(&self) -> Arc<Mutex<dyn VirtioDevice>> {
        self.device.clone()
    }

    fn prepare_activator(&mut self, barrier: Option<Arc<Barrier>>) -> VirtioPciDeviceActivator {
        let mut queues = Vec::new();

        for (queue_index, queue) in self.queues.iter().enumerate() {
            if !queue.ready() {
                continue;
            }

            if !queue.is_valid(self.memory.memory().deref()) {
                error!("Queue {} is not valid", queue_index);
            }

            queues.push((
                queue_index,
                vm_virtio::clone_queue(queue),
                self.queue_evts[queue_index].try_clone().unwrap(),
            ));
        }

        VirtioPciDeviceActivator {
            interrupt: self.virtio_interrupt.take(),
            memory: Some(self.memory.clone()),
            device: self.device.clone(),
            queues: Some(queues),
            device_activated: self.device_activated.clone(),
            barrier,
            id: self.id.clone(),
        }
    }

    fn activate(&mut self) -> ActivateResult {
        self.prepare_activator(None).activate()
    }

    fn needs_activation(&self) -> bool {
        !self.device_activated.load(Ordering::SeqCst) && self.is_driver_ready()
    }

    pub fn dma_handler(&self) -> Option<&Arc<dyn ExternalDmaMapping>> {
        self.dma_handler.as_ref()
    }
}

impl VirtioTransport for VirtioPciDevice {
    fn ioeventfds(&self, base_addr: u64) -> Vec<(&EventFd, u64)> {
        let notify_base = base_addr + NOTIFICATION_BAR_OFFSET;
        self.queue_evts()
            .iter()
            .enumerate()
            .map(|(i, event)| {
                (
                    event,
                    notify_base + i as u64 * u64::from(NOTIFY_OFF_MULTIPLIER),
                )
            })
            .collect()
    }
}

pub struct VirtioInterruptMsix {
    msix_config: Arc<Mutex<MsixConfig>>,
    config_vector: Arc<AtomicU16>,
    queues_vectors: Arc<Mutex<Vec<u16>>>,
    interrupt_source_group: Arc<dyn InterruptSourceGroup>,
}

impl VirtioInterruptMsix {
    pub fn new(
        msix_config: Arc<Mutex<MsixConfig>>,
        config_vector: Arc<AtomicU16>,
        queues_vectors: Arc<Mutex<Vec<u16>>>,
        interrupt_source_group: Arc<dyn InterruptSourceGroup>,
    ) -> Self {
        VirtioInterruptMsix {
            msix_config,
            config_vector,
            queues_vectors,
            interrupt_source_group,
        }
    }
}

impl VirtioInterrupt for VirtioInterruptMsix {
    fn trigger(&self, int_type: VirtioInterruptType) -> std::result::Result<(), std::io::Error> {
        let vector = match int_type {
            VirtioInterruptType::Config => self.config_vector.load(Ordering::Acquire),
            VirtioInterruptType::Queue(queue_index) => {
                self.queues_vectors.lock().unwrap()[queue_index as usize]
            }
        };

        if vector == VIRTQ_MSI_NO_VECTOR {
            return Ok(());
        }

        let config = &mut self.msix_config.lock().unwrap();
        let entry = &config.table_entries[vector as usize];
        // In case the vector control register associated with the entry
        // has its first bit set, this means the vector is masked and the
        // device should not inject the interrupt.
        // Instead, the Pending Bit Array table is updated to reflect there
        // is a pending interrupt for this specific vector.
        if config.masked() || entry.masked() {
            config.set_pba_bit(vector, false);
            return Ok(());
        }

        self.interrupt_source_group
            .trigger(vector as InterruptIndex)
    }

    fn notifier(&self, int_type: VirtioInterruptType) -> Option<EventFd> {
        let vector = match int_type {
            VirtioInterruptType::Config => self.config_vector.load(Ordering::Acquire),
            VirtioInterruptType::Queue(queue_index) => {
                self.queues_vectors.lock().unwrap()[queue_index as usize]
            }
        };

        self.interrupt_source_group
            .notifier(vector as InterruptIndex)
    }
}

impl PciDevice for VirtioPciDevice {
    fn write_config_register(
        &mut self,
        reg_idx: usize,
        offset: u64,
        data: &[u8],
    ) -> Option<Arc<Barrier>> {
        // Handle the special case where the capability VIRTIO_PCI_CAP_PCI_CFG
        // is accessed. This capability has a special meaning as it allows the
        // guest to access other capabilities without mapping the PCI BAR.
        let base = reg_idx * 4;
        if base + offset as usize >= self.cap_pci_cfg_info.offset
            && base + offset as usize + data.len()
                <= self.cap_pci_cfg_info.offset + self.cap_pci_cfg_info.cap.bytes().len()
        {
            let offset = base + offset as usize - self.cap_pci_cfg_info.offset;
            self.write_cap_pci_cfg(offset, data)
        } else {
            self.configuration
                .write_config_register(reg_idx, offset, data);
            None
        }
    }

    fn read_config_register(&mut self, reg_idx: usize) -> u32 {
        // Handle the special case where the capability VIRTIO_PCI_CAP_PCI_CFG
        // is accessed. This capability has a special meaning as it allows the
        // guest to access other capabilities without mapping the PCI BAR.
        let base = reg_idx * 4;
        if base >= self.cap_pci_cfg_info.offset
            && base + 4 <= self.cap_pci_cfg_info.offset + self.cap_pci_cfg_info.cap.bytes().len()
        {
            let offset = base - self.cap_pci_cfg_info.offset;
            let mut data = [0u8; 4];
            self.read_cap_pci_cfg(offset, &mut data);
            u32::from_le_bytes(data)
        } else {
            self.configuration.read_reg(reg_idx)
        }
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
        mmio_allocator: &mut AddressAllocator,
        resources: Option<Vec<Resource>>,
    ) -> std::result::Result<Vec<PciBarConfiguration>, PciDeviceError> {
        let mut bars = Vec::new();
        let device_clone = self.device.clone();
        let device = device_clone.lock().unwrap();

        let mut settings_bar_addr = None;
        let mut use_64bit_bar = self.use_64bit_bar;
        if let Some(resources) = resources {
            for resource in resources {
                if let Resource::PciBar {
                    index, base, type_, ..
                } = resource
                {
                    if index == VIRTIO_COMMON_BAR_INDEX {
                        settings_bar_addr = Some(GuestAddress(base));
                        use_64bit_bar = match type_ {
                            PciBarType::Io => {
                                return Err(PciDeviceError::InvalidResource(resource))
                            }
                            PciBarType::Mmio32 => false,
                            PciBarType::Mmio64 => true,
                        };
                        break;
                    }
                }
            }
            // Error out if no resource was matching the BAR id.
            if settings_bar_addr.is_none() {
                return Err(PciDeviceError::MissingResource);
            }
        }

        // Allocate the virtio-pci capability BAR.
        // See http://docs.oasis-open.org/virtio/virtio/v1.0/cs04/virtio-v1.0-cs04.html#x1-740004
        let (virtio_pci_bar_addr, region_type) = if use_64bit_bar {
            let region_type = PciBarRegionType::Memory64BitRegion;
            let addr = mmio_allocator
                .allocate(
                    settings_bar_addr,
                    CAPABILITY_BAR_SIZE,
                    Some(CAPABILITY_BAR_SIZE),
                )
                .ok_or(PciDeviceError::IoAllocationFailed(CAPABILITY_BAR_SIZE))?;
            (addr, region_type)
        } else {
            let region_type = PciBarRegionType::Memory32BitRegion;
            let addr = allocator
                .lock()
                .unwrap()
                .allocate_mmio_hole_addresses(
                    settings_bar_addr,
                    CAPABILITY_BAR_SIZE,
                    Some(CAPABILITY_BAR_SIZE),
                )
                .ok_or(PciDeviceError::IoAllocationFailed(CAPABILITY_BAR_SIZE))?;
            (addr, region_type)
        };

        let bar = PciBarConfiguration::default()
            .set_index(VIRTIO_COMMON_BAR_INDEX)
            .set_address(virtio_pci_bar_addr.raw_value())
            .set_size(CAPABILITY_BAR_SIZE)
            .set_region_type(region_type);
        self.configuration.add_pci_bar(&bar).map_err(|e| {
            PciDeviceError::IoRegistrationFailed(virtio_pci_bar_addr.raw_value(), e)
        })?;

        bars.push(bar);

        // Once the BARs are allocated, the capabilities can be added to the PCI configuration.
        self.add_pci_capabilities(VIRTIO_COMMON_BAR_INDEX as u8)?;

        // Allocate a dedicated BAR if there are some shared memory regions.
        if let Some(shm_list) = device.get_shm_regions() {
            let bar = PciBarConfiguration::default()
                .set_index(VIRTIO_SHM_BAR_INDEX)
                .set_address(shm_list.addr.raw_value())
                .set_size(shm_list.len);
            self.configuration
                .add_pci_bar(&bar)
                .map_err(|e| PciDeviceError::IoRegistrationFailed(shm_list.addr.raw_value(), e))?;

            bars.push(bar);

            for (idx, shm) in shm_list.region_list.iter().enumerate() {
                let shm_cap = VirtioPciCap64::new(
                    PciCapabilityType::SharedMemoryConfig,
                    VIRTIO_SHM_BAR_INDEX as u8,
                    idx as u8,
                    shm.offset,
                    shm.len,
                );
                self.configuration
                    .add_capability(&shm_cap)
                    .map_err(PciDeviceError::CapabilitiesSetup)?;
            }
        }

        self.bar_regions = bars.clone();

        Ok(bars)
    }

    fn free_bars(
        &mut self,
        allocator: &mut SystemAllocator,
        mmio_allocator: &mut AddressAllocator,
    ) -> std::result::Result<(), PciDeviceError> {
        for bar in self.bar_regions.drain(..) {
            match bar.region_type() {
                PciBarRegionType::Memory32BitRegion => {
                    allocator.free_mmio_hole_addresses(GuestAddress(bar.addr()), bar.size());
                }
                PciBarRegionType::Memory64BitRegion => {
                    mmio_allocator.free(GuestAddress(bar.addr()), bar.size());
                }
                _ => error!("Unexpected PCI bar type"),
            }
        }
        Ok(())
    }

    fn move_bar(&mut self, old_base: u64, new_base: u64) -> result::Result<(), std::io::Error> {
        // We only update our idea of the bar in order to support free_bars() above.
        // The majority of the reallocation is done inside DeviceManager.
        for bar in self.bar_regions.iter_mut() {
            if bar.addr() == old_base {
                *bar = bar.set_address(new_base);
            }
        }

        Ok(())
    }

    fn read_bar(&mut self, _base: u64, offset: u64, data: &mut [u8]) {
        match offset {
            o if o < COMMON_CONFIG_BAR_OFFSET + COMMON_CONFIG_SIZE => self.common_config.read(
                o - COMMON_CONFIG_BAR_OFFSET,
                data,
                &mut self.queues,
                self.device.clone(),
            ),
            o if (ISR_CONFIG_BAR_OFFSET..ISR_CONFIG_BAR_OFFSET + ISR_CONFIG_SIZE).contains(&o) => {
                if let Some(v) = data.get_mut(0) {
                    // Reading this register resets it to 0.
                    *v = self.interrupt_status.swap(0, Ordering::AcqRel) as u8;
                }
            }
            o if (DEVICE_CONFIG_BAR_OFFSET..DEVICE_CONFIG_BAR_OFFSET + DEVICE_CONFIG_SIZE)
                .contains(&o) =>
            {
                let device = self.device.lock().unwrap();
                device.read_config(o - DEVICE_CONFIG_BAR_OFFSET, data);
            }
            o if (NOTIFICATION_BAR_OFFSET..NOTIFICATION_BAR_OFFSET + NOTIFICATION_SIZE)
                .contains(&o) =>
            {
                // Handled with ioeventfds.
            }
            o if (MSIX_TABLE_BAR_OFFSET..MSIX_TABLE_BAR_OFFSET + MSIX_TABLE_SIZE).contains(&o) => {
                if let Some(msix_config) = &self.msix_config {
                    msix_config
                        .lock()
                        .unwrap()
                        .read_table(o - MSIX_TABLE_BAR_OFFSET, data);
                }
            }
            o if (MSIX_PBA_BAR_OFFSET..MSIX_PBA_BAR_OFFSET + MSIX_PBA_SIZE).contains(&o) => {
                if let Some(msix_config) = &self.msix_config {
                    msix_config
                        .lock()
                        .unwrap()
                        .read_pba(o - MSIX_PBA_BAR_OFFSET, data);
                }
            }
            _ => (),
        }
    }

    fn write_bar(&mut self, _base: u64, offset: u64, data: &[u8]) -> Option<Arc<Barrier>> {
        match offset {
            o if o < COMMON_CONFIG_BAR_OFFSET + COMMON_CONFIG_SIZE => self.common_config.write(
                o - COMMON_CONFIG_BAR_OFFSET,
                data,
                &mut self.queues,
                self.device.clone(),
            ),
            o if (ISR_CONFIG_BAR_OFFSET..ISR_CONFIG_BAR_OFFSET + ISR_CONFIG_SIZE).contains(&o) => {
                if let Some(v) = data.first() {
                    self.interrupt_status
                        .fetch_and(!(*v as usize), Ordering::AcqRel);
                }
            }
            o if (DEVICE_CONFIG_BAR_OFFSET..DEVICE_CONFIG_BAR_OFFSET + DEVICE_CONFIG_SIZE)
                .contains(&o) =>
            {
                let mut device = self.device.lock().unwrap();
                device.write_config(o - DEVICE_CONFIG_BAR_OFFSET, data);
            }
            o if (NOTIFICATION_BAR_OFFSET..NOTIFICATION_BAR_OFFSET + NOTIFICATION_SIZE)
                .contains(&o) =>
            {
                // Handled with ioeventfds.
                error!("Unexpected write to notification BAR: offset = 0x{:x}", o);
            }
            o if (MSIX_TABLE_BAR_OFFSET..MSIX_TABLE_BAR_OFFSET + MSIX_TABLE_SIZE).contains(&o) => {
                if let Some(msix_config) = &self.msix_config {
                    msix_config
                        .lock()
                        .unwrap()
                        .write_table(o - MSIX_TABLE_BAR_OFFSET, data);
                }
            }
            o if (MSIX_PBA_BAR_OFFSET..MSIX_PBA_BAR_OFFSET + MSIX_PBA_SIZE).contains(&o) => {
                if let Some(msix_config) = &self.msix_config {
                    msix_config
                        .lock()
                        .unwrap()
                        .write_pba(o - MSIX_PBA_BAR_OFFSET, data);
                }
            }
            _ => (),
        };

        // Try and activate the device if the driver status has changed
        if self.needs_activation() {
            let barrier = Arc::new(Barrier::new(2));
            let activator = self.prepare_activator(Some(barrier.clone()));
            self.pending_activations.lock().unwrap().push(activator);
            info!(
                "{}: Needs activation; writing to activate event fd",
                self.id
            );
            self.activate_evt.write(1).ok();
            info!("{}: Needs activation; returning barrier", self.id);
            return Some(barrier);
        }

        // Device has been reset by the driver
        if self.device_activated.load(Ordering::SeqCst) && self.is_driver_init() {
            let mut device = self.device.lock().unwrap();
            if let Some(virtio_interrupt) = device.reset() {
                // Upon reset the device returns its interrupt EventFD
                self.virtio_interrupt = Some(virtio_interrupt);
                self.device_activated.store(false, Ordering::SeqCst);

                // Reset queue readiness (changes queue_enable), queue sizes
                // and selected_queue as per spec for reset
                self.queues.iter_mut().for_each(Queue::reset);
                self.common_config.queue_select = 0;
            } else {
                error!("Attempt to reset device when not implemented in underlying device");
                self.common_config.driver_status = crate::DEVICE_FAILED as u8;
            }
        }

        None
    }

    fn as_any(&mut self) -> &mut dyn Any {
        self
    }

    fn id(&self) -> Option<String> {
        Some(self.id.clone())
    }
}

impl BusDevice for VirtioPciDevice {
    fn read(&mut self, base: u64, offset: u64, data: &mut [u8]) {
        self.read_bar(base, offset, data)
    }

    fn write(&mut self, base: u64, offset: u64, data: &[u8]) -> Option<Arc<Barrier>> {
        self.write_bar(base, offset, data)
    }
}

impl Pausable for VirtioPciDevice {
    fn pause(&mut self) -> result::Result<(), MigratableError> {
        Ok(())
    }

    fn resume(&mut self) -> result::Result<(), MigratableError> {
        Ok(())
    }
}

impl Snapshottable for VirtioPciDevice {
    fn id(&self) -> String {
        self.id.clone()
    }

    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        let mut virtio_pci_dev_snapshot = Snapshot::new_from_state(&self.id, &self.state())?;

        // Snapshot PciConfiguration
        virtio_pci_dev_snapshot.add_snapshot(self.configuration.snapshot()?);

        // Snapshot VirtioPciCommonConfig
        virtio_pci_dev_snapshot.add_snapshot(self.common_config.snapshot()?);

        // Snapshot MSI-X
        if let Some(msix_config) = &self.msix_config {
            virtio_pci_dev_snapshot.add_snapshot(msix_config.lock().unwrap().snapshot()?);
        }

        Ok(virtio_pci_dev_snapshot)
    }

    fn restore(&mut self, snapshot: Snapshot) -> std::result::Result<(), MigratableError> {
        if let Some(virtio_pci_dev_section) =
            snapshot.snapshot_data.get(&format!("{}-section", self.id))
        {
            // Restore MSI-X
            if let Some(msix_config) = &self.msix_config {
                let id = msix_config.lock().unwrap().id();
                if let Some(msix_snapshot) = snapshot.snapshots.get(&id) {
                    msix_config
                        .lock()
                        .unwrap()
                        .restore(*msix_snapshot.clone())?;
                }
            }

            // Restore VirtioPciCommonConfig
            if let Some(virtio_config_snapshot) = snapshot.snapshots.get(&self.common_config.id()) {
                self.common_config
                    .restore(*virtio_config_snapshot.clone())?;
            }

            // Restore PciConfiguration
            if let Some(pci_config_snapshot) = snapshot.snapshots.get(&self.configuration.id()) {
                self.configuration.restore(*pci_config_snapshot.clone())?;
            }

            // First restore the status of the virtqueues.
            self.set_state(&virtio_pci_dev_section.to_state()?)
                .map_err(|e| {
                    MigratableError::Restore(anyhow!(
                        "Could not restore VIRTIO_PCI_DEVICE state {:?}",
                        e
                    ))
                })?;

            // Then we can activate the device, as we know at this point that
            // the virtqueues are in the right state and the device is ready
            // to be activated, which will spawn each virtio worker thread.
            if self.device_activated.load(Ordering::SeqCst) && self.is_driver_ready() {
                self.activate().map_err(|e| {
                    MigratableError::Restore(anyhow!("Failed activating the device: {:?}", e))
                })?;
            }

            return Ok(());
        }

        Err(MigratableError::Restore(anyhow!(
            "Could not find VIRTIO_PCI_DEVICE snapshot section"
        )))
    }
}
impl Transportable for VirtioPciDevice {}
impl Migratable for VirtioPciDevice {}

// Portions Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.
//
// Copyright © 2019 - 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//

use crate::device_manager::{AddressManager, DeviceManagerError, DeviceManagerResult};
use acpi_tables::aml::{self, Aml};
use arch::layout;
use pci::{DeviceRelocation, PciBdf, PciBus, PciConfigMmio, PciRoot};
#[cfg(target_arch = "x86_64")]
use pci::{PciConfigIo, PCI_CONFIG_IO_PORT, PCI_CONFIG_IO_PORT_SIZE};
use std::sync::{Arc, Mutex};
use uuid::Uuid;
use vm_allocator::AddressAllocator;
use vm_device::BusDevice;

pub(crate) struct PciSegment {
    pub(crate) id: u16,
    pub(crate) pci_bus: Arc<Mutex<PciBus>>,
    pub(crate) pci_config_mmio: Arc<Mutex<PciConfigMmio>>,
    pub(crate) mmio_config_address: u64,

    #[cfg(target_arch = "x86_64")]
    pub(crate) pci_config_io: Option<Arc<Mutex<PciConfigIo>>>,

    // Bitmap of PCI devices to hotplug.
    pub(crate) pci_devices_up: u32,
    // Bitmap of PCI devices to hotunplug.
    pub(crate) pci_devices_down: u32,
    // List of allocated IRQs for each PCI slot.
    pub(crate) pci_irq_slots: [u8; 32],

    // Device memory covered by this segment
    pub(crate) start_of_device_area: u64,
    pub(crate) end_of_device_area: u64,

    pub(crate) allocator: Arc<Mutex<AddressAllocator>>,
}

impl PciSegment {
    pub(crate) fn new(
        id: u16,
        address_manager: &Arc<AddressManager>,
        allocator: Arc<Mutex<AddressAllocator>>,
        pci_irq_slots: &[u8; 32],
    ) -> DeviceManagerResult<PciSegment> {
        let pci_root = PciRoot::new(None);
        let pci_bus = Arc::new(Mutex::new(PciBus::new(
            pci_root,
            Arc::clone(address_manager) as Arc<dyn DeviceRelocation>,
        )));

        let pci_config_mmio = Arc::new(Mutex::new(PciConfigMmio::new(Arc::clone(&pci_bus))));
        let mmio_config_address =
            layout::PCI_MMCONFIG_START.0 + layout::PCI_MMIO_CONFIG_SIZE_PER_SEGMENT * id as u64;

        address_manager
            .mmio_bus
            .insert(
                Arc::clone(&pci_config_mmio) as Arc<Mutex<dyn BusDevice>>,
                mmio_config_address,
                layout::PCI_MMIO_CONFIG_SIZE_PER_SEGMENT,
            )
            .map_err(DeviceManagerError::BusError)?;

        let start_of_device_area = allocator.lock().unwrap().base().0;
        let end_of_device_area = allocator.lock().unwrap().end().0;

        let segment = PciSegment {
            id,
            pci_bus,
            pci_config_mmio,
            mmio_config_address,
            pci_devices_up: 0,
            pci_devices_down: 0,
            #[cfg(target_arch = "x86_64")]
            pci_config_io: None,
            allocator,
            start_of_device_area,
            end_of_device_area,
            pci_irq_slots: *pci_irq_slots,
        };

        info!(
            "Adding PCI segment: id={}, PCI MMIO config address: 0x{:x}, device area [0x{:x}-0x{:x}",
            segment.id, segment.mmio_config_address, segment.start_of_device_area, segment.end_of_device_area
        );
        Ok(segment)
    }

    #[cfg(target_arch = "x86_64")]
    pub(crate) fn new_default_segment(
        address_manager: &Arc<AddressManager>,
        allocator: Arc<Mutex<AddressAllocator>>,
        pci_irq_slots: &[u8; 32],
    ) -> DeviceManagerResult<PciSegment> {
        let mut segment = Self::new(0, address_manager, allocator, pci_irq_slots)?;
        let pci_config_io = Arc::new(Mutex::new(PciConfigIo::new(Arc::clone(&segment.pci_bus))));

        address_manager
            .io_bus
            .insert(
                pci_config_io.clone(),
                PCI_CONFIG_IO_PORT,
                PCI_CONFIG_IO_PORT_SIZE,
            )
            .map_err(DeviceManagerError::BusError)?;

        segment.pci_config_io = Some(pci_config_io);

        Ok(segment)
    }

    #[cfg(target_arch = "aarch64")]
    pub(crate) fn new_default_segment(
        address_manager: &Arc<AddressManager>,
        allocator: Arc<Mutex<AddressAllocator>>,
        pci_irq_slots: &[u8; 32],
    ) -> DeviceManagerResult<PciSegment> {
        Self::new(0, address_manager, allocator, pci_irq_slots)
    }

    pub(crate) fn next_device_bdf(&self) -> DeviceManagerResult<PciBdf> {
        Ok(PciBdf::new(
            self.id,
            0,
            self.pci_bus
                .lock()
                .unwrap()
                .next_device_id()
                .map_err(DeviceManagerError::NextPciDeviceId)? as u8,
            0,
        ))
    }

    pub fn reserve_legacy_interrupts_for_pci_devices(
        address_manager: &Arc<AddressManager>,
        pci_irq_slots: &mut [u8; 32],
    ) -> DeviceManagerResult<()> {
        // Reserve 8 IRQs which will be shared across all PCI devices.
        let num_irqs = 8;
        let mut irqs: Vec<u8> = Vec::new();
        for _ in 0..num_irqs {
            irqs.push(
                address_manager
                    .allocator
                    .lock()
                    .unwrap()
                    .allocate_irq()
                    .ok_or(DeviceManagerError::AllocateIrq)? as u8,
            );
        }

        // There are 32 devices on the PCI bus, let's assign them an IRQ.
        for i in 0..32 {
            pci_irq_slots[i] = irqs[i % num_irqs];
        }

        Ok(())
    }
}

struct PciDevSlot {
    device_id: u8,
}

impl Aml for PciDevSlot {
    fn append_aml_bytes(&self, bytes: &mut Vec<u8>) {
        let sun = self.device_id;
        let adr: u32 = (self.device_id as u32) << 16;
        aml::Device::new(
            format!("S{:03}", self.device_id).as_str().into(),
            vec![
                &aml::Name::new("_SUN".into(), &sun),
                &aml::Name::new("_ADR".into(), &adr),
                &aml::Method::new(
                    "_EJ0".into(),
                    1,
                    true,
                    vec![&aml::MethodCall::new(
                        "\\_SB_.PHPR.PCEJ".into(),
                        vec![&aml::Path::new("_SUN"), &aml::Path::new("_SEG")],
                    )],
                ),
            ],
        )
        .append_aml_bytes(bytes)
    }
}

struct PciDevSlotNotify {
    device_id: u8,
}

impl Aml for PciDevSlotNotify {
    fn append_aml_bytes(&self, bytes: &mut Vec<u8>) {
        let device_id_mask: u32 = 1 << self.device_id;
        let object = aml::Path::new(&format!("S{:03}", self.device_id));
        aml::And::new(&aml::Local(0), &aml::Arg(0), &device_id_mask).append_aml_bytes(bytes);
        aml::If::new(
            &aml::Equal::new(&aml::Local(0), &device_id_mask),
            vec![&aml::Notify::new(&object, &aml::Arg(1))],
        )
        .append_aml_bytes(bytes);
    }
}

struct PciDevSlotMethods {}

impl Aml for PciDevSlotMethods {
    fn append_aml_bytes(&self, bytes: &mut Vec<u8>) {
        let mut device_notifies = Vec::new();
        for device_id in 0..32 {
            device_notifies.push(PciDevSlotNotify { device_id });
        }

        let mut device_notifies_refs: Vec<&dyn aml::Aml> = Vec::new();
        for device_notify in device_notifies.iter() {
            device_notifies_refs.push(device_notify);
        }

        aml::Method::new("DVNT".into(), 2, true, device_notifies_refs).append_aml_bytes(bytes);
        aml::Method::new(
            "PCNT".into(),
            0,
            true,
            vec![
                &aml::Acquire::new("\\_SB_.PHPR.BLCK".into(), 0xffff),
                &aml::Store::new(&aml::Path::new("\\_SB_.PHPR.PSEG"), &aml::Path::new("_SEG")),
                &aml::MethodCall::new(
                    "DVNT".into(),
                    vec![&aml::Path::new("\\_SB_.PHPR.PCIU"), &aml::ONE],
                ),
                &aml::MethodCall::new(
                    "DVNT".into(),
                    vec![&aml::Path::new("\\_SB_.PHPR.PCID"), &3usize],
                ),
                &aml::Release::new("\\_SB_.PHPR.BLCK".into()),
            ],
        )
        .append_aml_bytes(bytes)
    }
}

struct PciDsmMethod {}

impl Aml for PciDsmMethod {
    fn append_aml_bytes(&self, bytes: &mut Vec<u8>) {
        // Refer to ACPI spec v6.3 Ch 9.1.1 and PCI Firmware spec v3.3 Ch 4.6.1
        // _DSM (Device Specific Method), the following is the implementation in ASL.
        /*
        Method (_DSM, 4, NotSerialized)  // _DSM: Device-Specific Method
        {
              If ((Arg0 == ToUUID ("e5c937d0-3553-4d7a-9117-ea4d19c3434d") /* Device Labeling Interface */))
              {
                  If ((Arg2 == Zero))
                  {
                      Return (Buffer (One) { 0x21 })
                  }
                  If ((Arg2 == 0x05))
                  {
                      Return (Zero)
                  }
              }

              Return (Buffer (One) { 0x00 })
        }
         */
        /*
         * As per ACPI v6.3 Ch 19.6.142, the UUID is required to be in mixed endian:
         * Among the fields of a UUID:
         *   {d1 (8 digits)} - {d2 (4 digits)} - {d3 (4 digits)} - {d4 (16 digits)}
         * d1 ~ d3 need to be little endian, d4 be big endian.
         * See https://en.wikipedia.org/wiki/Universally_unique_identifier#Encoding .
         */
        let uuid = Uuid::parse_str("E5C937D0-3553-4D7A-9117-EA4D19C3434D").unwrap();
        let (uuid_d1, uuid_d2, uuid_d3, uuid_d4) = uuid.as_fields();
        let mut uuid_buf = vec![];
        uuid_buf.extend(uuid_d1.to_le_bytes());
        uuid_buf.extend(uuid_d2.to_le_bytes());
        uuid_buf.extend(uuid_d3.to_le_bytes());
        uuid_buf.extend(uuid_d4);
        aml::Method::new(
            "_DSM".into(),
            4,
            false,
            vec![
                &aml::If::new(
                    &aml::Equal::new(&aml::Arg(0), &aml::Buffer::new(uuid_buf)),
                    vec![
                        &aml::If::new(
                            &aml::Equal::new(&aml::Arg(2), &aml::ZERO),
                            vec![&aml::Return::new(&aml::Buffer::new(vec![0x21]))],
                        ),
                        &aml::If::new(
                            &aml::Equal::new(&aml::Arg(2), &0x05u8),
                            vec![&aml::Return::new(&aml::ZERO)],
                        ),
                    ],
                ),
                &aml::Return::new(&aml::Buffer::new(vec![0])),
            ],
        )
        .append_aml_bytes(bytes)
    }
}

impl Aml for PciSegment {
    fn append_aml_bytes(&self, bytes: &mut Vec<u8>) {
        let mut pci_dsdt_inner_data: Vec<&dyn aml::Aml> = Vec::new();
        let hid = aml::Name::new("_HID".into(), &aml::EisaName::new("PNP0A08"));
        pci_dsdt_inner_data.push(&hid);
        let cid = aml::Name::new("_CID".into(), &aml::EisaName::new("PNP0A03"));
        pci_dsdt_inner_data.push(&cid);
        let adr = aml::Name::new("_ADR".into(), &aml::ZERO);
        pci_dsdt_inner_data.push(&adr);
        let seg = aml::Name::new("_SEG".into(), &self.id);
        pci_dsdt_inner_data.push(&seg);
        let uid = aml::Name::new("_UID".into(), &aml::ZERO);
        pci_dsdt_inner_data.push(&uid);
        let cca = aml::Name::new("_CCA".into(), &aml::ONE);
        pci_dsdt_inner_data.push(&cca);
        let supp = aml::Name::new("SUPP".into(), &aml::ZERO);
        pci_dsdt_inner_data.push(&supp);

        // Since Cloud Hypervisor supports only one PCI bus, it can be tied
        // to the NUMA node 0. It's up to the user to organize the NUMA nodes
        // so that the PCI bus relates to the expected vCPUs and guest RAM.
        let proximity_domain = 0u32;
        let pxm_return = aml::Return::new(&proximity_domain);
        let pxm = aml::Method::new("_PXM".into(), 0, false, vec![&pxm_return]);
        pci_dsdt_inner_data.push(&pxm);

        let pci_dsm = PciDsmMethod {};
        pci_dsdt_inner_data.push(&pci_dsm);

        let crs = if self.id == 0 {
            aml::Name::new(
                "_CRS".into(),
                &aml::ResourceTemplate::new(vec![
                    &aml::AddressSpace::new_bus_number(0x0u16, 0x0u16),
                    #[cfg(target_arch = "x86_64")]
                    &aml::Io::new(0xcf8, 0xcf8, 1, 0x8),
                    &aml::AddressSpace::new_memory(
                        aml::AddressSpaceCachable::NotCacheable,
                        true,
                        layout::MEM_32BIT_DEVICES_START.0 as u32,
                        (layout::MEM_32BIT_DEVICES_START.0 + layout::MEM_32BIT_DEVICES_SIZE - 1)
                            as u32,
                    ),
                    &aml::AddressSpace::new_memory(
                        aml::AddressSpaceCachable::NotCacheable,
                        true,
                        self.start_of_device_area,
                        self.end_of_device_area,
                    ),
                    #[cfg(target_arch = "x86_64")]
                    &aml::AddressSpace::new_io(0u16, 0x0cf7u16),
                    #[cfg(target_arch = "x86_64")]
                    &aml::AddressSpace::new_io(0x0d00u16, 0xffffu16),
                ]),
            )
        } else {
            aml::Name::new(
                "_CRS".into(),
                &aml::ResourceTemplate::new(vec![
                    &aml::AddressSpace::new_bus_number(0x0u16, 0x0u16),
                    &aml::Memory32Fixed::new(
                        true,
                        self.mmio_config_address as u32,
                        layout::PCI_MMIO_CONFIG_SIZE_PER_SEGMENT as u32,
                    ),
                    &aml::AddressSpace::new_memory(
                        aml::AddressSpaceCachable::NotCacheable,
                        true,
                        self.start_of_device_area,
                        self.end_of_device_area,
                    ),
                ]),
            )
        };
        pci_dsdt_inner_data.push(&crs);

        let mut pci_devices = Vec::new();
        for device_id in 0..32 {
            let pci_device = PciDevSlot { device_id };
            pci_devices.push(pci_device);
        }
        for pci_device in pci_devices.iter() {
            pci_dsdt_inner_data.push(pci_device);
        }

        let pci_device_methods = PciDevSlotMethods {};
        pci_dsdt_inner_data.push(&pci_device_methods);

        // Build PCI routing table, listing IRQs assigned to PCI devices.
        let prt_package_list: Vec<(u32, u32)> = self
            .pci_irq_slots
            .iter()
            .enumerate()
            .map(|(i, irq)| (((((i as u32) & 0x1fu32) << 16) | 0xffffu32), *irq as u32))
            .collect();
        let prt_package_list: Vec<aml::Package> = prt_package_list
            .iter()
            .map(|(bdf, irq)| aml::Package::new(vec![bdf, &0u8, &0u8, irq]))
            .collect();
        let prt_package_list: Vec<&dyn Aml> = prt_package_list
            .iter()
            .map(|item| item as &dyn Aml)
            .collect();
        let prt = aml::Name::new("_PRT".into(), &aml::Package::new(prt_package_list));
        pci_dsdt_inner_data.push(&prt);

        aml::Device::new(
            format!("_SB_.PCI{:X}", self.id).as_str().into(),
            pci_dsdt_inner_data,
        )
        .append_aml_bytes(bytes)
    }
}

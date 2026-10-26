// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use super::AcpiNotificationFlags;
use acpi_tables::{aml, aml::Aml};
use std::sync::{Arc, Barrier};
use std::time::Instant;
use vm_device::interrupt::InterruptSourceGroup;
use vm_device::BusDevice;
use vm_memory::GuestAddress;
use vmm_sys_util::eventfd::EventFd;

pub const GED_DEVICE_ACPI_SIZE: usize = 0x1;

/// A device for handling ACPI shutdown and reboot
pub struct AcpiShutdownDevice {
    exit_evt: EventFd,
    reset_evt: EventFd,
}

impl AcpiShutdownDevice {
    /// Constructs a device that will signal the given event when the guest requests it.
    pub fn new(exit_evt: EventFd, reset_evt: EventFd) -> AcpiShutdownDevice {
        AcpiShutdownDevice {
            exit_evt,
            reset_evt,
        }
    }
}

// Same I/O port used for shutdown and reboot
impl BusDevice for AcpiShutdownDevice {
    // Spec has all fields as zero
    fn read(&mut self, _base: u64, _offset: u64, data: &mut [u8]) {
        data.fill(0)
    }

    fn write(&mut self, _base: u64, _offset: u64, data: &[u8]) -> Option<Arc<Barrier>> {
        if data[0] == 1 {
            warn!("Shutdown because of ACPI Reboot signalled");
            if let Err(e) = self.reset_evt.write(1) {
                error!("Error triggering ACPI reset event: {}", e);
            }
        }
        // The ACPI DSDT table specifies the S5 sleep state (shutdown) as value 5
        const S5_SLEEP_VALUE: u8 = 5;
        const SLEEP_STATUS_EN_BIT: u8 = 5;
        const SLEEP_VALUE_BIT: u8 = 2;
        if data[0] == (S5_SLEEP_VALUE << SLEEP_VALUE_BIT) | (1 << SLEEP_STATUS_EN_BIT) {
            info!("ACPI Shutdown signalled and set exit_evt");

            if let Err(e) = self.exit_evt.write(1) {
                error!("Error triggering ACPI shutdown event: {}", e);
            }
        }
        None
    }
}

/// A device for handling ACPI GED event generation
pub struct AcpiGedDevice {
    interrupt: Arc<dyn InterruptSourceGroup>,
    notification_type: AcpiNotificationFlags,
    ged_irq: u32,
    address: GuestAddress,
}

impl AcpiGedDevice {
    pub fn new(
        interrupt: Arc<dyn InterruptSourceGroup>,
        ged_irq: u32,
        address: GuestAddress,
    ) -> AcpiGedDevice {
        AcpiGedDevice {
            interrupt,
            notification_type: AcpiNotificationFlags::NO_DEVICES_CHANGED,
            ged_irq,
            address,
        }
    }

    pub fn notify(
        &mut self,
        notification_type: AcpiNotificationFlags,
    ) -> Result<(), std::io::Error> {
        self.notification_type |= notification_type;
        self.interrupt.trigger(0)
    }

    pub fn irq(&self) -> u32 {
        self.ged_irq
    }
}

// I/O port reports what type of notification was made
impl BusDevice for AcpiGedDevice {
    // Spec has all fields as zero
    fn read(&mut self, _base: u64, _offset: u64, data: &mut [u8]) {
        data[0] = self.notification_type.bits();
        self.notification_type = AcpiNotificationFlags::NO_DEVICES_CHANGED;
    }
}

impl Aml for AcpiGedDevice {
    fn append_aml_bytes(&self, bytes: &mut Vec<u8>) {
        aml::Device::new(
            "_SB_.GEC_".into(),
            vec![
                &aml::Name::new("_HID".into(), &aml::EisaName::new("PNP0A06")),
                &aml::Name::new("_UID".into(), &"Generic Event Controller"),
                &aml::Name::new(
                    "_CRS".into(),
                    &aml::ResourceTemplate::new(vec![&aml::AddressSpace::new_memory(
                        aml::AddressSpaceCachable::NotCacheable,
                        true,
                        self.address.0,
                        self.address.0 + GED_DEVICE_ACPI_SIZE as u64 - 1,
                    )]),
                ),
                &aml::OpRegion::new(
                    "GDST".into(),
                    aml::OpRegionSpace::SystemMemory,
                    self.address.0 as usize,
                    GED_DEVICE_ACPI_SIZE,
                ),
                &aml::Field::new(
                    "GDST".into(),
                    aml::FieldAccessType::Byte,
                    aml::FieldUpdateRule::WriteAsZeroes,
                    vec![aml::FieldEntry::Named(*b"GDAT", 8)],
                ),
                &aml::Method::new(
                    "ESCN".into(),
                    0,
                    true,
                    vec![
                        &aml::Store::new(&aml::Local(0), &aml::Path::new("GDAT")),
                        &aml::And::new(&aml::Local(1), &aml::Local(0), &aml::ONE),
                        &aml::If::new(
                            &aml::Equal::new(&aml::Local(1), &aml::ONE),
                            vec![&aml::MethodCall::new("\\_SB_.CPUS.CSCN".into(), vec![])],
                        ),
                        &aml::And::new(&aml::Local(1), &aml::Local(0), &2usize),
                        &aml::If::new(
                            &aml::Equal::new(&aml::Local(1), &2usize),
                            vec![&aml::MethodCall::new("\\_SB_.MHPC.MSCN".into(), vec![])],
                        ),
                        &aml::And::new(&aml::Local(1), &aml::Local(0), &4usize),
                        &aml::If::new(
                            &aml::Equal::new(&aml::Local(1), &4usize),
                            vec![&aml::MethodCall::new("\\_SB_.PHPR.PSCN".into(), vec![])],
                        ),
                        &aml::And::new(&aml::Local(1), &aml::Local(0), &8usize),
                        &aml::If::new(
                            &aml::Equal::new(&aml::Local(1), &8usize),
                            vec![&aml::Notify::new(
                                &aml::Path::new("\\_SB_.PWRB"),
                                &0x80usize,
                            )],
                        ),
                    ],
                ),
            ],
        )
        .append_aml_bytes(bytes);
        aml::Device::new(
            "_SB_.GED_".into(),
            vec![
                &aml::Name::new("_HID".into(), &"ACPI0013"),
                &aml::Name::new("_UID".into(), &aml::ZERO),
                &aml::Name::new(
                    "_CRS".into(),
                    &aml::ResourceTemplate::new(vec![&aml::Interrupt::new(
                        true,
                        true,
                        false,
                        false,
                        self.ged_irq,
                    )]),
                ),
                &aml::Method::new(
                    "_EVT".into(),
                    1,
                    true,
                    vec![&aml::MethodCall::new("\\_SB_.GEC_.ESCN".into(), vec![])],
                ),
            ],
        )
        .append_aml_bytes(bytes)
    }
}

pub struct AcpiPmTimerDevice {
    start: Instant,
}

impl AcpiPmTimerDevice {
    pub fn new() -> Self {
        Self {
            start: Instant::now(),
        }
    }
}

impl Default for AcpiPmTimerDevice {
    fn default() -> Self {
        Self::new()
    }
}

impl BusDevice for AcpiPmTimerDevice {
    fn read(&mut self, _base: u64, _offset: u64, data: &mut [u8]) {
        if data.len() != std::mem::size_of::<u32>() {
            warn!("Invalid sized read of PM timer: {}", data.len());
            return;
        }
        let now = Instant::now();
        let since = now.duration_since(self.start);
        let nanos = since.as_nanos();

        const PM_TIMER_FREQUENCY_HZ: u128 = 3_579_545;
        const NANOS_PER_SECOND: u128 = 1_000_000_000;

        let counter = (nanos * PM_TIMER_FREQUENCY_HZ) / NANOS_PER_SECOND;
        let counter: u32 = (counter & 0xffff_ffff) as u32;

        data.copy_from_slice(&counter.to_le_bytes());
    }
}

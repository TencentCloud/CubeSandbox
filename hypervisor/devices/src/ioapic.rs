// Copyright 2019 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.
//
// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// Implementation of an intel 82093AA Input/Output Advanced Programmable Interrupt Controller
// See https://pdos.csail.mit.edu/6.828/2016/readings/ia32/ioapic.pdf for a specification.

use super::interrupt_controller::{Error, InterruptController};
use anyhow::anyhow;
use byteorder::{ByteOrder, LittleEndian};
use serde::{Deserialize, Serialize};
use std::result;
use std::sync::{Arc, Barrier};

use vm_device::interrupt::{
    InterruptIndex, InterruptManager, InterruptSourceConfig, InterruptSourceGroup,
    MsiIrqGroupConfig, MsiIrqSourceConfig,
};
use vm_device::BusDevice;
use vm_memory::GuestAddress;
use vm_migration::{Migratable, MigratableError, Pausable, Snapshot, Snapshottable, Transportable};
use vmm_sys_util::eventfd::EventFd;

type Result<T> = result::Result<T, Error>;

// I/O REDIRECTION TABLE REGISTER
//
// There are 24 I/O Redirection Table entry registers. Each register is a
// dedicated entry for each interrupt input signal. Each register is 64 bits
// split between two 32 bits registers as follow:
//
// 63-56: Destination Field - R/W
// 55-17: Reserved
// 16:    Interrupt Mask - R/W
// 15:    Trigger Mode - R/W
// 14:    Remote IRR - RO
// 13:    Interrupt Input Pin Polarity - R/W
// 12:    Delivery Status - RO
// 11:    Destination Mode - R/W
// 10-8:  Delivery Mode - R/W
// 7-0:   Interrupt Vector - R/W
pub type RedirectionTableEntry = u64;

fn vector(entry: RedirectionTableEntry) -> u8 {
    (entry & 0xffu64) as u8
}
fn delivery_mode(entry: RedirectionTableEntry) -> u8 {
    ((entry >> 8) & 0x7u64) as u8
}
fn destination_mode(entry: RedirectionTableEntry) -> u8 {
    ((entry >> 11) & 0x1u64) as u8
}
fn remote_irr(entry: RedirectionTableEntry) -> u8 {
    ((entry >> 14) & 0x1u64) as u8
}
fn trigger_mode(entry: RedirectionTableEntry) -> u8 {
    ((entry >> 15) & 0x1u64) as u8
}
fn interrupt_mask(entry: RedirectionTableEntry) -> u8 {
    ((entry >> 16) & 0x1u64) as u8
}
fn destination_field(entry: RedirectionTableEntry) -> u8 {
    // When the destination mode is physical, the destination field should only
    // be defined through bits 56-59, as defined in the IOAPIC specification.
    // But from the APIC specification, the APIC ID is always defined on 8 bits
    // no matter which destination mode is selected. That's why we always
    // retrieve the destination field based on bits 56-63.
    ((entry >> 56) & 0xffu64) as u8
}
fn set_delivery_status(entry: &mut RedirectionTableEntry, val: u8) {
    // Clear bit 12
    *entry &= 0xffff_ffff_ffff_efff;
    // Set it with the expected value
    *entry |= u64::from(val & 0x1) << 12;
}
fn set_remote_irr(entry: &mut RedirectionTableEntry, val: u8) {
    // Clear bit 14
    *entry &= 0xffff_ffff_ffff_bfff;
    // Set it with the expected value
    *entry |= u64::from(val & 0x1) << 14;
}

pub const NUM_IOAPIC_PINS: usize = 24;
const IOAPIC_VERSION_ID: u32 = 0x0017_0011;

// Constants for IOAPIC direct register offset
const IOAPIC_REG_ID: u8 = 0x00;
const IOAPIC_REG_VERSION: u8 = 0x01;
const IOAPIC_REG_ARBITRATION_ID: u8 = 0x02;

// Register offsets
const IOREGSEL_OFF: u8 = 0x0;
const IOWIN_OFF: u8 = 0x10;
const IOWIN_SCALE: u8 = 0x2;
const REG_MAX_OFFSET: u8 = IOWIN_OFF + (NUM_IOAPIC_PINS as u8 * 2) - 1;

#[repr(u8)]
enum TriggerMode {
    Edge = 0,
    Level = 1,
}

#[repr(u8)]
enum DeliveryMode {
    Fixed = 0b000,
    Lowest = 0b001,
    Smi = 0b010,        // System management interrupt
    RemoteRead = 0b011, // This is no longer supported by intel.
    Nmi = 0b100,        // Non maskable interrupt
    Init = 0b101,
    Startup = 0b110,
    External = 0b111,
}

/// Given an offset that was read from/written to, return a tuple of the relevant IRQ and whether
/// the offset refers to the high bits of that register.
fn decode_irq_from_selector(selector: u8) -> (usize, bool) {
    (
        ((selector - IOWIN_OFF) / IOWIN_SCALE) as usize,
        selector & 1 != 0,
    )
}

pub struct Ioapic {
    id: String,
    id_reg: u32,
    reg_sel: u32,
    reg_entries: [RedirectionTableEntry; NUM_IOAPIC_PINS],
    used_entries: [bool; NUM_IOAPIC_PINS],
    apic_address: GuestAddress,
    interrupt_source_group: Arc<dyn InterruptSourceGroup>,
}

#[derive(Serialize, Deserialize)]
pub struct IoapicState {
    id_reg: u32,
    reg_sel: u32,
    reg_entries: [RedirectionTableEntry; NUM_IOAPIC_PINS],
    used_entries: [bool; NUM_IOAPIC_PINS],
    apic_address: u64,
}

impl BusDevice for Ioapic {
    fn read(&mut self, _base: u64, offset: u64, data: &mut [u8]) {
        if data.len() != std::mem::size_of::<u32>() {
            warn!("Invalid read size on IOAPIC: {}", data.len());
            return;
        }

        debug!("IOAPIC_R @ offset 0x{:x}", offset);

        let value: u32 = match offset as u8 {
            IOREGSEL_OFF => self.reg_sel,
            IOWIN_OFF => self.ioapic_read(),
            _ => {
                error!("IOAPIC: failed reading at offset {}", offset);
                return;
            }
        };

        LittleEndian::write_u32(data, value);
    }

    fn write(&mut self, _base: u64, offset: u64, data: &[u8]) -> Option<Arc<Barrier>> {
        if data.len() != std::mem::size_of::<u32>() {
            warn!("Invalid write size on IOAPIC: {}", data.len());
            return None;
        }

        debug!("IOAPIC_W @ offset 0x{:x}", offset);

        let value = LittleEndian::read_u32(data);

        match offset as u8 {
            IOREGSEL_OFF => self.reg_sel = value,
            IOWIN_OFF => self.ioapic_write(value),
            _ => {
                error!("IOAPIC: failed writing at offset {}", offset);
            }
        }
        None
    }
}

impl Ioapic {
    pub fn new(
        id: String,
        apic_address: GuestAddress,
        interrupt_manager: Arc<dyn InterruptManager<GroupConfig = MsiIrqGroupConfig>>,
    ) -> Result<Ioapic> {
        let interrupt_source_group = interrupt_manager
            .create_group(MsiIrqGroupConfig {
                base: 0,
                count: NUM_IOAPIC_PINS as InterruptIndex,
            })
            .map_err(Error::CreateInterruptSourceGroup)?;

        // The IOAPIC is created with entries already masked. The guest will be
        // in charge of unmasking them if/when necessary.
        Ok(Ioapic {
            id,
            id_reg: 0,
            reg_sel: 0,
            reg_entries: [0x10000; NUM_IOAPIC_PINS],
            used_entries: [false; NUM_IOAPIC_PINS],
            apic_address,
            interrupt_source_group,
        })
    }

    fn ioapic_write(&mut self, val: u32) {
        debug!("IOAPIC_W reg 0x{:x}, val 0x{:x}", self.reg_sel, val);

        match self.reg_sel as u8 {
            IOAPIC_REG_VERSION => {
                if val == 0 {
                    // Windows writes zero here (see #1791)
                } else {
                    error!(
                        "IOAPIC: invalid write to version register (0x{:x}): 0x{:x}",
                        self.reg_sel, val
                    );
                }
            }
            IOAPIC_REG_ID => self.id_reg = (val >> 24) & 0xf,
            IOWIN_OFF..=REG_MAX_OFFSET => {
                let (index, is_high_bits) = decode_irq_from_selector(self.reg_sel as u8);
                if index > NUM_IOAPIC_PINS {
                    warn!("IOAPIC index out of range: {}", index);
                    return;
                }
                if is_high_bits {
                    self.reg_entries[index] &= 0xffff_ffff;
                    self.reg_entries[index] |= u64::from(val) << 32;
                } else {
                    // Ensure not to override read-only bits:
                    // - Delivery Status (bit 12)
                    // - Remote IRR (bit 14)
                    self.reg_entries[index] &= 0xffff_ffff_0000_5000;
                    self.reg_entries[index] |= u64::from(val) & 0xffff_afff;
                }
                // The entry must be updated through the interrupt source
                // group.
                if let Err(e) = self.update_entry(index) {
                    error!("Failed updating IOAPIC entry: {:?}", e);
                }
                // Store the information this IRQ is now being used.
                self.used_entries[index] = true;
            }
            _ => error!(
                "IOAPIC: invalid write to register offset 0x{:x}",
                self.reg_sel
            ),
        }
    }

    fn ioapic_read(&self) -> u32 {
        debug!("IOAPIC_R reg 0x{:x}", self.reg_sel);

        match self.reg_sel as u8 {
            IOAPIC_REG_VERSION => IOAPIC_VERSION_ID,
            IOAPIC_REG_ID | IOAPIC_REG_ARBITRATION_ID => (self.id_reg & 0xf) << 24,
            IOWIN_OFF..=REG_MAX_OFFSET => {
                let (index, is_high_bits) = decode_irq_from_selector(self.reg_sel as u8);
                if index > NUM_IOAPIC_PINS {
                    warn!("IOAPIC index out of range: {}", index);
                    return 0;
                }
                if is_high_bits {
                    (self.reg_entries[index] >> 32) as u32
                } else {
                    (self.reg_entries[index] & 0xffff_ffff) as u32
                }
            }
            _ => {
                error!(
                    "IOAPIC: invalid read from register offset 0x{:x}",
                    self.reg_sel
                );
                0
            }
        }
    }

    fn state(&self) -> IoapicState {
        IoapicState {
            id_reg: self.id_reg,
            reg_sel: self.reg_sel,
            reg_entries: self.reg_entries,
            used_entries: self.used_entries,
            apic_address: self.apic_address.0,
        }
    }

    fn set_state(&mut self, state: &IoapicState) -> Result<()> {
        self.id_reg = state.id_reg;
        self.reg_sel = state.reg_sel;
        self.reg_entries = state.reg_entries;
        self.used_entries = state.used_entries;
        self.apic_address = GuestAddress(state.apic_address);
        for (irq, entry) in self.used_entries.iter().enumerate() {
            if *entry {
                self.update_entry(irq)?;
            }
        }

        Ok(())
    }

    fn update_entry(&self, irq: usize) -> Result<()> {
        let entry = self.reg_entries[irq];

        // Validate Destination Mode value, and retrieve Destination ID
        let destination_mode = destination_mode(entry);
        let destination_id = destination_field(entry);

        // When this bit is set, the message is directed to the processor with
        // the lowest interrupt priority among processors that can receive the
        // interrupt.
        let redirection_hint: u8 = 1;

        // Generate MSI message address
        let low_addr: u32 = self.apic_address.0 as u32
            | u32::from(destination_id) << 12
            | u32::from(redirection_hint) << 3
            | u32::from(destination_mode) << 2;

        // Validate Trigger Mode value
        let trigger_mode = trigger_mode(entry);
        match trigger_mode {
            x if (x == TriggerMode::Edge as u8) || (x == TriggerMode::Level as u8) => {}
            _ => return Err(Error::InvalidTriggerMode),
        }

        // Validate Delivery Mode value
        let delivery_mode = delivery_mode(entry);
        match delivery_mode {
            x if (x == DeliveryMode::Fixed as u8)
                || (x == DeliveryMode::Lowest as u8)
                || (x == DeliveryMode::Smi as u8)
                || (x == DeliveryMode::RemoteRead as u8)
                || (x == DeliveryMode::Nmi as u8)
                || (x == DeliveryMode::Init as u8)
                || (x == DeliveryMode::Startup as u8)
                || (x == DeliveryMode::External as u8) => {}
            _ => return Err(Error::InvalidDeliveryMode),
        }

        // Generate MSI message data
        let data: u32 = u32::from(trigger_mode) << 15
            | u32::from(remote_irr(entry)) << 14
            | u32::from(delivery_mode) << 8
            | u32::from(vector(entry));

        let config = MsiIrqSourceConfig {
            high_addr: 0x0,
            low_addr,
            data,
            devid: 0,
        };

        self.interrupt_source_group
            .update(
                irq as InterruptIndex,
                InterruptSourceConfig::MsiIrq(config),
                interrupt_mask(entry) == 1,
            )
            .map_err(Error::UpdateInterrupt)?;

        Ok(())
    }
}

impl InterruptController for Ioapic {
    // The ioapic must be informed about EOIs in order to deassert interrupts
    // already sent.
    fn end_of_interrupt(&mut self, vec: u8) {
        for i in 0..NUM_IOAPIC_PINS {
            let entry = &mut self.reg_entries[i];
            // Clear Remote IRR bit
            if vector(*entry) == vec && trigger_mode(*entry) == 1 {
                set_remote_irr(entry, 0);
            }
        }
    }

    // This should be called anytime an interrupt needs to be injected into the
    // running guest.
    fn service_irq(&mut self, irq: usize) -> Result<()> {
        let entry = &mut self.reg_entries[irq];

        self.interrupt_source_group
            .trigger(irq as InterruptIndex)
            .map_err(Error::TriggerInterrupt)?;
        debug!("Interrupt successfully delivered");

        // If trigger mode is level sensitive, set the Remote IRR bit.
        // It will be cleared when the EOI is received.
        if trigger_mode(*entry) == 1 {
            set_remote_irr(entry, 1);
        }
        // Clear the Delivery Status bit
        set_delivery_status(entry, 0);

        Ok(())
    }

    fn notifier(&self, irq: usize) -> Option<EventFd> {
        self.interrupt_source_group.notifier(irq as InterruptIndex)
    }
}

impl Snapshottable for Ioapic {
    fn id(&self) -> String {
        self.id.clone()
    }

    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        Snapshot::new_from_state(&self.id, &self.state())
    }

    fn restore(&mut self, snapshot: Snapshot) -> std::result::Result<(), MigratableError> {
        self.set_state(&snapshot.to_state(&self.id)?).map_err(|e| {
            MigratableError::Restore(anyhow!("Could not restore state for {}: {:?}", self.id, e))
        })
    }
}

impl Pausable for Ioapic {}
impl Transportable for Ioapic {}
impl Migratable for Ioapic {}

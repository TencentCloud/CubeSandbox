// Copyright 2021 Arm Limited (or its affiliates). All rights reserved.
//
// SPDX-License-Identifier: Apache-2.0

//! ARM PrimeCell General Purpose Input/Output(PL061)
//!
//! This module implements an ARM PrimeCell General Purpose Input/Output(PL061) to support gracefully poweroff microvm from external.
//!

use crate::{read_le_u32, write_le_u32};
use std::result;
use std::sync::{Arc, Barrier};
use std::{fmt, io};
use vm_device::interrupt::InterruptSourceGroup;
use vm_device::BusDevice;
use vm_migration::{Migratable, MigratableError, Pausable, Snapshot, Snapshottable, Transportable};

const OFS_DATA: u64 = 0x400; // Data Register
const GPIODIR: u64 = 0x400; // Direction Register
const GPIOIS: u64 = 0x404; // Interrupt Sense Register
const GPIOIBE: u64 = 0x408; // Interrupt Both Edges Register
const GPIOIEV: u64 = 0x40c; // Interrupt Event Register
const GPIOIE: u64 = 0x410; // Interrupt Mask Register
const GPIORIE: u64 = 0x414; // Raw Interrupt Status Register
const GPIOMIS: u64 = 0x418; // Masked Interrupt Status Register
const GPIOIC: u64 = 0x41c; // Interrupt Clear Register
const GPIOAFSEL: u64 = 0x420; // Mode Control Select Register
                              // From 0x424 to 0xFDC => reserved space.
                              // From 0xFE0 to 0xFFC => Peripheral and PrimeCell Identification Registers which are Read Only registers.
                              // These registers can conceptually be treated as a 32-bit register, and PartNumber[11:0] is used to identify the peripheral.
                              // We are putting the expected values (look at 'Reset value' column from above mentioned document) in an array.
const GPIO_ID: [u8; 8] = [0x61, 0x10, 0x14, 0x00, 0x0d, 0xf0, 0x05, 0xb1];
// ID Margins
const GPIO_ID_LOW: u64 = 0xfe0;
const GPIO_ID_HIGH: u64 = 0x1000;

const N_GPIOS: u32 = 8;

#[derive(Debug)]
pub enum Error {
    BadWriteOffset(u64),
    GpioInterruptDisabled,
    GpioInterruptFailure(io::Error),
    GpioTriggerKeyFailure(u32),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        match self {
            Error::BadWriteOffset(offset) => write!(f, "Bad Write Offset: {}", offset),
            Error::GpioInterruptDisabled => write!(f, "GPIO interrupt disabled by guest driver.",),
            Error::GpioInterruptFailure(ref e) => {
                write!(f, "Could not trigger GPIO interrupt: {}.", e)
            }
            Error::GpioTriggerKeyFailure(key) => {
                write!(f, "Invalid GPIO Input key triggerd: {}.", key)
            }
        }
    }
}

type Result<T> = result::Result<T, Error>;

/// A GPIO device following the PL061 specification.
pub struct Gpio {
    id: String,
    // Data Register
    data: u32,
    old_in_data: u32,
    // Direction Register
    dir: u32,
    // Interrupt Sense Register
    isense: u32,
    // Interrupt Both Edges Register
    ibe: u32,
    // Interrupt Event Register
    iev: u32,
    // Interrupt Mask Register
    im: u32,
    // Raw Interrupt Status Register
    istate: u32,
    // Mode Control Select Register
    afsel: u32,
    // GPIO irq_field
    interrupt: Arc<dyn InterruptSourceGroup>,
}

#[derive(Serialize, Deserialize)]
pub struct GpioState {
    data: u32,
    old_in_data: u32,
    dir: u32,
    isense: u32,
    ibe: u32,
    iev: u32,
    im: u32,
    istate: u32,
    afsel: u32,
}

impl Gpio {
    /// Constructs an PL061 GPIO device.
    pub fn new(id: String, interrupt: Arc<dyn InterruptSourceGroup>) -> Self {
        Self {
            id,
            data: 0,
            old_in_data: 0,
            dir: 0,
            isense: 0,
            ibe: 0,
            iev: 0,
            im: 0,
            istate: 0,
            afsel: 0,
            interrupt,
        }
    }

    fn state(&self) -> GpioState {
        GpioState {
            data: self.data,
            old_in_data: self.old_in_data,
            dir: self.dir,
            isense: self.isense,
            ibe: self.ibe,
            iev: self.iev,
            im: self.im,
            istate: self.istate,
            afsel: self.afsel,
        }
    }

    fn set_state(&mut self, state: &GpioState) {
        self.data = state.data;
        self.old_in_data = state.old_in_data;
        self.dir = state.dir;
        self.isense = state.isense;
        self.ibe = state.ibe;
        self.iev = state.iev;
        self.im = state.im;
        self.istate = state.istate;
        self.afsel = state.afsel;
    }

    fn pl061_internal_update(&mut self) {
        // FIXME:
        //  Missing Output Interrupt Emulation.

        // Input Edging Interrupt Emulation.
        let changed = ((self.old_in_data ^ self.data) & !self.dir) as u32;
        if changed > 0 {
            self.old_in_data = self.data;
            for i in 0..N_GPIOS {
                let mask = (1 << i) as u32;
                if (changed & mask) > 0 {
                    // Bits set high in GPIOIS(Interrupt sense register) configure the corresponding
                    // pins to detect levels, otherwise, detect edges.
                    if (self.isense & mask) == 0 {
                        if (self.ibe & mask) > 0 {
                            // Bits set high in GPIOIBE(Interrupt both-edges register) configure the corresponding
                            // pins to detect both falling and rising edges.
                            // Clearing a bit configures the pin to be controlled by GPIOIEV.
                            self.istate |= mask;
                        } else {
                            // Bits set to high in GPIOIEV(Interrupt event register) configure the
                            // corresponding pin to detect rising edges, otherwise, detect falling edges.
                            self.istate |= !(self.data ^ self.iev) & mask;
                        }
                    }
                }
            }
        }

        // Input Level Interrupt Emulation.
        self.istate |= !(self.data ^ self.iev) & self.isense;
    }

    fn handle_write(&mut self, offset: u64, val: u32) -> Result<()> {
        if offset < OFS_DATA {
            // In order to write to data register, the corresponding bits in the mask, resulting
            // from the offsite[9:2], must be HIGH. otherwise the bit values remain unchanged.
            let mask = (offset >> 2) as u32 & self.dir;
            self.data = (self.data & !mask) | (val & mask);
        } else {
            match offset {
                GPIODIR => {
                    /* Direction Register */
                    self.dir = val & 0xff;
                }
                GPIOIS => {
                    /* Interrupt Sense Register */
                    self.isense = val & 0xff;
                }
                GPIOIBE => {
                    /* Interrupt Both Edges Register */
                    self.ibe = val & 0xff;
                }
                GPIOIEV => {
                    /* Interrupt Event Register */
                    self.iev = val & 0xff;
                }
                GPIOIE => {
                    /* Interrupt Mask Register */
                    self.im = val & 0xff;
                }
                GPIOIC => {
                    /* Interrupt Clear Register */
                    self.istate &= !val;
                }
                GPIOAFSEL => {
                    /* Mode Control Select Register */
                    self.afsel = val & 0xff;
                }
                o => {
                    return Err(Error::BadWriteOffset(o));
                }
            }
        }
        Ok(())
    }

    pub fn trigger_key(&mut self, key: u32) -> Result<()> {
        let mask = (1 << key) as u32;
        if (!self.dir & mask) > 0 {
            // emulate key event
            // By default, Input Pin is configured to detect both rising and falling edges.
            // So reverse the input pin data to generate a pulse.
            self.data |= !(self.data & mask) & mask;
            self.pl061_internal_update();

            match self.trigger_gpio_interrupt() {
                Ok(_) | Err(Error::GpioInterruptDisabled) => return Ok(()),
                Err(e) => return Err(e),
            }
        }

        Err(Error::GpioTriggerKeyFailure(key))
    }

    fn trigger_gpio_interrupt(&self) -> Result<()> {
        // Bits set to high in GPIOIE(Interrupt mask register) allow the corresponding pins to
        // trigger their individual interrupts and then the combined GPIOINTR line.
        if (self.istate & self.im) == 0 {
            warn!("Failed to trigger GPIO input interrupt (disabled by guest OS)");
            return Err(Error::GpioInterruptDisabled);
        }
        self.interrupt
            .trigger(0)
            .map_err(Error::GpioInterruptFailure)?;
        Ok(())
    }
}

impl BusDevice for Gpio {
    fn read(&mut self, _base: u64, offset: u64, data: &mut [u8]) {
        let value;
        let mut read_ok = true;

        if (GPIO_ID_LOW..GPIO_ID_HIGH).contains(&offset) {
            let index = ((offset - GPIO_ID_LOW) >> 2) as usize;
            value = u32::from(GPIO_ID[index]);
        } else if offset < OFS_DATA {
            value = self.data & ((offset >> 2) as u32)
        } else {
            value = match offset {
                GPIODIR => self.dir,
                GPIOIS => self.isense,
                GPIOIBE => self.ibe,
                GPIOIEV => self.iev,
                GPIOIE => self.im,
                GPIORIE => self.istate,
                GPIOMIS => self.istate & self.im,
                GPIOAFSEL => self.afsel,
                _ => {
                    read_ok = false;
                    0
                }
            };
        }

        if read_ok && data.len() <= 4 {
            write_le_u32(data, value);
        } else {
            warn!(
                "Invalid GPIO PL061 read: offset {}, data length {}",
                offset,
                data.len()
            );
        }
    }

    fn write(&mut self, _base: u64, offset: u64, data: &[u8]) -> Option<Arc<Barrier>> {
        if data.len() <= 4 {
            let value = read_le_u32(data);
            if let Err(e) = self.handle_write(offset, value) {
                warn!("Failed to write to GPIO PL061 device: {}", e);
            }
        } else {
            warn!(
                "Invalid GPIO PL061 write: offset {}, data length {}",
                offset,
                data.len()
            );
        }

        None
    }
}

impl Snapshottable for Gpio {
    fn id(&self) -> String {
        self.id.clone()
    }

    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        Snapshot::new_from_state(&self.id, &self.state())
    }

    fn restore(&mut self, snapshot: Snapshot) -> std::result::Result<(), MigratableError> {
        self.set_state(&snapshot.to_state(&self.id)?);
        Ok(())
    }
}

impl Pausable for Gpio {}
impl Transportable for Gpio {}
impl Migratable for Gpio {}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{read_le_u32, write_le_u32};
    use std::sync::Arc;
    use vm_device::interrupt::{InterruptIndex, InterruptSourceConfig};
    use vmm_sys_util::eventfd::EventFd;

    const GPIO_NAME: &str = "gpio";
    const LEGACY_GPIO_MAPPED_IO_START: u64 = 0x0902_0000;

    struct TestInterrupt {
        event_fd: EventFd,
    }

    impl InterruptSourceGroup for TestInterrupt {
        fn trigger(&self, _index: InterruptIndex) -> result::Result<(), std::io::Error> {
            self.event_fd.write(1)
        }

        fn update(
            &self,
            _index: InterruptIndex,
            _config: InterruptSourceConfig,
            _masked: bool,
        ) -> result::Result<(), std::io::Error> {
            Ok(())
        }

        fn notifier(&self, _index: InterruptIndex) -> Option<EventFd> {
            Some(self.event_fd.try_clone().unwrap())
        }
    }

    impl TestInterrupt {
        fn new(event_fd: EventFd) -> Self {
            TestInterrupt { event_fd }
        }
    }

    #[test]
    fn test_gpio_read_write_and_event() {
        let intr_evt = EventFd::new(libc::EFD_NONBLOCK).unwrap();
        let mut gpio = Gpio::new(
            String::from(GPIO_NAME),
            Arc::new(TestInterrupt::new(intr_evt.try_clone().unwrap())),
        );
        let mut data = [0; 4];

        // Read and write to the GPIODIR register.
        // Set pin 0 output pin.
        write_le_u32(&mut data, 1);
        gpio.write(LEGACY_GPIO_MAPPED_IO_START, GPIODIR, &data);
        gpio.read(LEGACY_GPIO_MAPPED_IO_START, GPIODIR, &mut data);
        let v = read_le_u32(&data);
        assert_eq!(v, 1);

        // Read and write to the GPIODATA register.
        write_le_u32(&mut data, 1);
        // Set pin 0 high.
        let offset = 0x00000004_u64;
        gpio.write(LEGACY_GPIO_MAPPED_IO_START, offset, &data);
        gpio.read(LEGACY_GPIO_MAPPED_IO_START, offset, &mut data);
        let v = read_le_u32(&data);
        assert_eq!(v, 1);

        // Read and write to the GPIOIS register.
        // Configure pin 0 detecting level interrupt.
        write_le_u32(&mut data, 1);
        gpio.write(LEGACY_GPIO_MAPPED_IO_START, GPIOIS, &data);
        gpio.read(LEGACY_GPIO_MAPPED_IO_START, GPIOIS, &mut data);
        let v = read_le_u32(&data);
        assert_eq!(v, 1);

        // Read and write to the GPIOIBE register.
        // Configure pin 1 detecting both falling and rising edges.
        write_le_u32(&mut data, 2);
        gpio.write(LEGACY_GPIO_MAPPED_IO_START, GPIOIBE, &data);
        gpio.read(LEGACY_GPIO_MAPPED_IO_START, GPIOIBE, &mut data);
        let v = read_le_u32(&data);
        assert_eq!(v, 2);

        // Read and write to the GPIOIEV register.
        // Configure pin 2 detecting both falling and rising edges.
        write_le_u32(&mut data, 4);
        gpio.write(LEGACY_GPIO_MAPPED_IO_START, GPIOIEV, &data);
        gpio.read(LEGACY_GPIO_MAPPED_IO_START, GPIOIEV, &mut data);
        let v = read_le_u32(&data);
        assert_eq!(v, 4);

        // Read and write to the GPIOIE register.
        // Configure pin 0...2 capable of triggering their individual interrupts
        // and then the combined GPIOINTR line.
        write_le_u32(&mut data, 7);
        gpio.write(LEGACY_GPIO_MAPPED_IO_START, GPIOIE, &data);
        gpio.read(LEGACY_GPIO_MAPPED_IO_START, GPIOIE, &mut data);
        let v = read_le_u32(&data);
        assert_eq!(v, 7);

        let mask = 0x00000002_u32;
        // emulate an rising pulse in pin 1.
        gpio.data |= !(gpio.data & mask) & mask;
        gpio.pl061_internal_update();
        // The interrupt line on pin 1 should be on.
        // Read the GPIOMIS register.
        gpio.read(LEGACY_GPIO_MAPPED_IO_START, GPIOMIS, &mut data);
        let v = read_le_u32(&data);
        assert_eq!(v, 2);

        // Read and Write to the GPIOIC register.
        // clear interrupt in pin 1.
        write_le_u32(&mut data, 2);
        gpio.write(LEGACY_GPIO_MAPPED_IO_START, GPIOIC, &data);
        gpio.read(LEGACY_GPIO_MAPPED_IO_START, GPIOIC, &mut data);
        let v = read_le_u32(&data);
        assert_eq!(v, 2);

        // Attempts to write beyond the writable space.
        write_le_u32(&mut data, 0);
        gpio.write(LEGACY_GPIO_MAPPED_IO_START, GPIO_ID_LOW, &data);

        let mut data = [0; 4];
        gpio.read(LEGACY_GPIO_MAPPED_IO_START, GPIO_ID_LOW, &mut data);
        let index = GPIO_ID_LOW + 3;
        assert_eq!(data[0], GPIO_ID[((index - GPIO_ID_LOW) >> 2) as usize]);
    }
}

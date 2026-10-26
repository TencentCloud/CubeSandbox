// Copyright 2021 Arm Limited (or its affiliates). All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//! ARM PrimeCell UART(PL011)
//!
//! This module implements an ARM PrimeCell UART(PL011).
//!

use crate::{read_le_u32, write_le_u32};
use serde::{Deserialize, Serialize};
use std::collections::VecDeque;
use std::fmt;
use std::sync::{Arc, Barrier};
use std::time::Instant;
use std::{io, result};
use vm_device::interrupt::InterruptSourceGroup;
use vm_device::BusDevice;
use vm_migration::{Migratable, MigratableError, Pausable, Snapshot, Snapshottable, Transportable};

/* Registers */
const UARTDR: u64 = 0;
const UARTRSR_UARTECR: u64 = 1;
const UARTFR: u64 = 6;
const UARTILPR: u64 = 8;
const UARTIBRD: u64 = 9;
const UARTFBRD: u64 = 10;
const UARTLCR_H: u64 = 11;
const UARTCR: u64 = 12;
const UARTIFLS: u64 = 13;
const UARTIMSC: u64 = 14;
const UARTRIS: u64 = 15;
const UARTMIS: u64 = 16;
const UARTICR: u64 = 17;
const UARTDMACR: u64 = 18;
const UARTDEBUG: u64 = 0x3c0;

const PL011_INT_TX: u32 = 0x20;
const PL011_INT_RX: u32 = 0x10;

const PL011_FLAG_RXFF: u32 = 0x40;
const PL011_FLAG_RXFE: u32 = 0x10;

const PL011_ID: [u8; 8] = [0x11, 0x10, 0x14, 0x00, 0x0d, 0xf0, 0x05, 0xb1];
// We are only interested in the margins.
const AMBA_ID_LOW: u64 = 0x3f8;
const AMBA_ID_HIGH: u64 = 0x401;

#[derive(Debug)]
pub enum Error {
    BadWriteOffset(u64),
    DmaNotImplemented,
    InterruptFailure(io::Error),
    WriteAllFailure(io::Error),
    FlushFailure(io::Error),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        match self {
            Error::BadWriteOffset(offset) => write!(f, "pl011_write: Bad Write Offset: {}", offset),
            Error::DmaNotImplemented => write!(f, "pl011: DMA not implemented."),
            Error::InterruptFailure(e) => write!(f, "Failed to trigger interrupt: {}", e),
            Error::WriteAllFailure(e) => write!(f, "Failed to write: {}", e),
            Error::FlushFailure(e) => write!(f, "Failed to flush: {}", e),
        }
    }
}

type Result<T> = result::Result<T, Error>;

/// A PL011 device following the PL011 specification.
pub struct Pl011 {
    id: String,
    flags: u32,
    lcr: u32,
    rsr: u32,
    cr: u32,
    dmacr: u32,
    debug: u32,
    int_enabled: u32,
    int_level: u32,
    read_fifo: VecDeque<u8>,
    ilpr: u32,
    ibrd: u32,
    fbrd: u32,
    ifl: u32,
    read_count: u32,
    read_trigger: u32,
    irq: Arc<dyn InterruptSourceGroup>,
    out: Option<Box<dyn io::Write + Send>>,
    timestamp: std::time::Instant,
}

#[derive(Serialize, Deserialize)]
pub struct Pl011State {
    flags: u32,
    lcr: u32,
    rsr: u32,
    cr: u32,
    dmacr: u32,
    debug: u32,
    int_enabled: u32,
    int_level: u32,
    read_fifo: Vec<u8>,
    ilpr: u32,
    ibrd: u32,
    fbrd: u32,
    ifl: u32,
    read_count: u32,
    read_trigger: u32,
}

impl Pl011 {
    /// Constructs an AMBA PL011 UART device.
    pub fn new(
        id: String,
        irq: Arc<dyn InterruptSourceGroup>,
        out: Option<Box<dyn io::Write + Send>>,
        timestamp: Instant,
    ) -> Self {
        Self {
            id,
            flags: 0x90u32,
            lcr: 0u32,
            rsr: 0u32,
            cr: 0x300u32,
            dmacr: 0u32,
            debug: 0u32,
            int_enabled: 0u32,
            int_level: 0u32,
            read_fifo: VecDeque::new(),
            ilpr: 0u32,
            ibrd: 0u32,
            fbrd: 0u32,
            ifl: 0x12u32,
            read_count: 0u32,
            read_trigger: 1u32,
            irq,
            out,
            timestamp,
        }
    }

    pub fn set_out(&mut self, out: Box<dyn io::Write + Send>) {
        self.out = Some(out);
    }

    fn state(&self) -> Pl011State {
        Pl011State {
            flags: self.flags,
            lcr: self.lcr,
            rsr: self.rsr,
            cr: self.cr,
            dmacr: self.dmacr,
            debug: self.debug,
            int_enabled: self.int_enabled,
            int_level: self.int_level,
            read_fifo: self.read_fifo.clone().into(),
            ilpr: self.ilpr,
            ibrd: self.ibrd,
            fbrd: self.fbrd,
            ifl: self.ifl,
            read_count: self.read_count,
            read_trigger: self.read_trigger,
        }
    }

    fn set_state(&mut self, state: &Pl011State) {
        self.flags = state.flags;
        self.lcr = state.lcr;
        self.rsr = state.rsr;
        self.cr = state.cr;
        self.dmacr = state.dmacr;
        self.debug = state.debug;
        self.int_enabled = state.int_enabled;
        self.int_level = state.int_level;
        self.read_fifo = state.read_fifo.clone().into();
        self.ilpr = state.ilpr;
        self.ibrd = state.ibrd;
        self.fbrd = state.fbrd;
        self.ifl = state.ifl;
        self.read_count = state.read_count;
        self.read_trigger = state.read_trigger;
    }

    /// Queues raw bytes for the guest to read and signals the interrupt
    pub fn queue_input_bytes(&mut self, c: &[u8]) -> vmm_sys_util::errno::Result<()> {
        self.read_fifo.extend(c);
        self.read_count += c.len() as u32;
        self.flags &= !PL011_FLAG_RXFE;

        if ((self.lcr & 0x10) == 0) || (self.read_count == 16) {
            self.flags |= PL011_FLAG_RXFF;
        }

        if self.read_count >= self.read_trigger {
            self.int_level |= PL011_INT_RX;
            self.trigger_interrupt()?;
        }

        Ok(())
    }

    pub fn flush_output(&mut self) -> result::Result<(), io::Error> {
        if let Some(out) = self.out.as_mut() {
            out.flush()?;
        }
        Ok(())
    }

    fn pl011_get_baudrate(&self) -> u32 {
        if self.fbrd == 0 {
            return 0;
        }

        let clk = 24_000_000; // We set the APB_PLCK to 24M in device tree
        (clk / ((self.ibrd << 6) + self.fbrd)) << 2
    }

    fn pl011_trace_baudrate_change(&self) {
        debug!(
            "=== New baudrate: {:#?} (clk: {:#?}Hz, ibrd: {:#?}, fbrd: {:#?}) ===",
            self.pl011_get_baudrate(),
            24_000_000, // We set the APB_PLCK to 24M in device tree
            self.ibrd,
            self.fbrd
        );
    }

    fn pl011_set_read_trigger(&mut self) {
        self.read_trigger = 1;
    }

    fn handle_write(&mut self, offset: u64, val: u32) -> Result<()> {
        match offset >> 2 {
            UARTDR => {
                self.int_level |= PL011_INT_TX;
                if let Some(out) = self.out.as_mut() {
                    out.write_all(&[val.to_le_bytes()[0]])
                        .map_err(Error::WriteAllFailure)?;
                    out.flush().map_err(Error::FlushFailure)?;
                }
            }
            UARTRSR_UARTECR => {
                self.rsr = 0;
            }
            UARTFR => { /* Writes to Flag register are ignored.*/ }
            UARTILPR => {
                self.ilpr = val;
            }
            UARTIBRD => {
                self.ibrd = val;
                self.pl011_trace_baudrate_change();
            }
            UARTFBRD => {
                self.fbrd = val;
                self.pl011_trace_baudrate_change();
            }
            UARTLCR_H => {
                /* Reset the FIFO state on FIFO enable or disable */
                if ((self.lcr ^ val) & 0x10) != 0 {
                    self.read_count = 0;
                }
                self.lcr = val;
                self.pl011_set_read_trigger();
            }
            UARTCR => {
                self.cr = val;
            }
            UARTIFLS => {
                self.ifl = val;
                self.pl011_set_read_trigger();
            }
            UARTIMSC => {
                self.int_enabled = val;
                self.trigger_interrupt().map_err(Error::InterruptFailure)?;
            }
            UARTICR => {
                self.int_level &= !val;
                self.trigger_interrupt().map_err(Error::InterruptFailure)?;
            }
            UARTDMACR => {
                self.dmacr = val;
                if (val & 3) != 0 {
                    return Err(Error::DmaNotImplemented);
                }
            }
            UARTDEBUG => {
                self.debug = val;
                self.handle_debug();
            }
            off => {
                debug!("PL011: Bad write offset, offset: {}", off);
                return Err(Error::BadWriteOffset(off));
            }
        }
        Ok(())
    }

    fn handle_debug(&self) {
        let elapsed = self.timestamp.elapsed();

        match self.debug {
            0x00..=0x1f => warn!(
                "[Debug I/O port: Firmware code: 0x{:x}] {}.{:>06} seconds",
                self.debug,
                elapsed.as_secs(),
                elapsed.as_micros()
            ),
            0x20..=0x3f => warn!(
                "[Debug I/O port: Bootloader code: 0x{:x}] {}.{:>06} seconds",
                self.debug,
                elapsed.as_secs(),
                elapsed.as_micros()
            ),
            0x40..=0x5f => warn!(
                "[Debug I/O port: Kernel code: 0x{:x}] {}.{:>06} seconds",
                self.debug,
                elapsed.as_secs(),
                elapsed.as_micros()
            ),
            0x60..=0x7f => warn!(
                "[Debug I/O port: Userspace code: 0x{:x}] {}.{:>06} seconds",
                self.debug,
                elapsed.as_secs(),
                elapsed.as_micros()
            ),
            _ => {}
        }
    }

    fn trigger_interrupt(&mut self) -> result::Result<(), io::Error> {
        self.irq.trigger(0)
    }
}

impl BusDevice for Pl011 {
    fn read(&mut self, _base: u64, offset: u64, data: &mut [u8]) {
        let mut read_ok = true;
        let v = if (AMBA_ID_LOW..AMBA_ID_HIGH).contains(&(offset >> 2)) {
            let index = ((offset - 0xfe0) >> 2) as usize;
            u32::from(PL011_ID[index])
        } else {
            match offset >> 2 {
                UARTDR => {
                    self.flags &= !PL011_FLAG_RXFF;
                    let c: u32 = self.read_fifo.pop_front().unwrap_or_default().into();
                    if self.read_count > 0 {
                        self.read_count -= 1;
                    }
                    if self.read_count == 0 {
                        self.flags |= PL011_FLAG_RXFE;
                    }
                    if self.read_count == (self.read_trigger - 1) {
                        self.int_level &= !PL011_INT_RX;
                    }
                    self.rsr = c >> 8;
                    c
                }
                UARTRSR_UARTECR => self.rsr,
                UARTFR => self.flags,
                UARTILPR => self.ilpr,
                UARTIBRD => self.ibrd,
                UARTFBRD => self.fbrd,
                UARTLCR_H => self.lcr,
                UARTCR => self.cr,
                UARTIFLS => self.ifl,
                UARTIMSC => self.int_enabled,
                UARTRIS => self.int_level,
                UARTMIS => self.int_level & self.int_enabled,
                UARTDMACR => self.dmacr,
                UARTDEBUG => self.debug,
                _ => {
                    read_ok = false;
                    0
                }
            }
        };

        if read_ok && data.len() <= 4 {
            write_le_u32(data, v);
        } else {
            warn!(
                "Invalid PL011 read: offset {}, data length {}",
                offset,
                data.len()
            );
        }
    }

    fn write(&mut self, _base: u64, offset: u64, data: &[u8]) -> Option<Arc<Barrier>> {
        if data.len() <= 4 {
            let v = read_le_u32(data);
            if let Err(e) = self.handle_write(offset, v) {
                warn!("Failed to write to PL011 device: {}", e);
            }
        } else {
            warn!(
                "Invalid PL011 write: offset {}, data length {}",
                offset,
                data.len()
            );
        }

        None
    }
}

impl Snapshottable for Pl011 {
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

impl Pausable for Pl011 {}
impl Transportable for Pl011 {}
impl Migratable for Pl011 {}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io;
    use std::sync::{Arc, Mutex};
    use vm_device::interrupt::{InterruptIndex, InterruptSourceConfig};
    use vmm_sys_util::eventfd::EventFd;

    const SERIAL_NAME: &str = "serial";

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

    #[derive(Clone)]
    struct SharedBuffer {
        buf: Arc<Mutex<Vec<u8>>>,
    }

    impl SharedBuffer {
        fn new() -> SharedBuffer {
            SharedBuffer {
                buf: Arc::new(Mutex::new(Vec::new())),
            }
        }
    }

    impl io::Write for SharedBuffer {
        fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
            self.buf.lock().unwrap().write(buf)
        }
        fn flush(&mut self) -> io::Result<()> {
            self.buf.lock().unwrap().flush()
        }
    }

    #[test]
    fn pl011_output() {
        let intr_evt = EventFd::new(0).unwrap();
        let pl011_out = SharedBuffer::new();
        let mut pl011 = Pl011::new(
            String::from(SERIAL_NAME),
            Arc::new(TestInterrupt::new(intr_evt.try_clone().unwrap())),
            Some(Box::new(pl011_out.clone())),
            Instant::now(),
        );

        pl011.write(0, UARTDR as u64, &[b'x', b'y']);
        pl011.write(0, UARTDR as u64, &[b'a']);
        pl011.write(0, UARTDR as u64, &[b'b']);
        pl011.write(0, UARTDR as u64, &[b'c']);
        assert_eq!(
            pl011_out.buf.lock().unwrap().as_slice(),
            &[b'x', b'a', b'b', b'c']
        );
    }

    #[test]
    fn pl011_input() {
        let intr_evt = EventFd::new(0).unwrap();
        let pl011_out = SharedBuffer::new();
        let mut pl011 = Pl011::new(
            String::from(SERIAL_NAME),
            Arc::new(TestInterrupt::new(intr_evt.try_clone().unwrap())),
            Some(Box::new(pl011_out)),
            Instant::now(),
        );

        // write 1 to the interrupt event fd, so that read doesn't block in case the event fd
        // counter doesn't change (for 0 it blocks)
        assert!(intr_evt.write(1).is_ok());
        pl011.queue_input_bytes(&[b'a', b'b', b'c']).unwrap();

        assert_eq!(intr_evt.read().unwrap(), 2);

        let mut data = [0u8];
        pl011.read(0, UARTDR as u64, &mut data);
        assert_eq!(data[0], b'a');
        pl011.read(0, UARTDR as u64, &mut data);
        assert_eq!(data[0], b'b');
        pl011.read(0, UARTDR as u64, &mut data);
        assert_eq!(data[0], b'c');
    }
}

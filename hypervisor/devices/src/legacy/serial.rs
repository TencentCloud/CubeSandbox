// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.

use serde::{Deserialize, Serialize};
use std::collections::VecDeque;
use std::sync::{Arc, Barrier};
use std::{io, result};
use vm_device::interrupt::InterruptSourceGroup;
use vm_device::BusDevice;
use vm_migration::{Migratable, MigratableError, Pausable, Snapshot, Snapshottable, Transportable};
use vmm_sys_util::errno::Result;

const INPUT_BUFFER_SIZE: usize = 0x40;

const DATA: u8 = 0;
const IER: u8 = 1;
const IIR: u8 = 2;
const LCR: u8 = 3;
const MCR: u8 = 4;
const LSR: u8 = 5;
const MSR: u8 = 6;
const SCR: u8 = 7;

const DLAB_LOW: u8 = 0;
const DLAB_HIGH: u8 = 1;

const IER_RECV_BIT: u8 = 0x1;
const IER_THR_BIT: u8 = 0x2;
const IER_FIFO_BITS: u8 = 0x0f;

const IIR_FIFO_BITS: u8 = 0xc0;
const IIR_NONE_BIT: u8 = 0x1;
const IIR_THR_BIT: u8 = 0x2;
const IIR_RECV_BIT: u8 = 0x4;

const LCR_DLAB_BIT: u8 = 0x80;

const LSR_DATA_BIT: u8 = 0x1;
const LSR_OVERRUN_BIT: u8 = 0x2;
const LSR_EMPTY_BIT: u8 = 0x20;
const LSR_IDLE_BIT: u8 = 0x40;

const MCR_LOOP_BIT: u8 = 0x10;

const DEFAULT_INTERRUPT_IDENTIFICATION: u8 = IIR_NONE_BIT; // no pending interrupt
const DEFAULT_LINE_STATUS: u8 = LSR_EMPTY_BIT | LSR_IDLE_BIT; // THR empty and line is idle
const DEFAULT_LINE_CONTROL: u8 = 0x3; // 8-bits per character
const DEFAULT_MODEM_CONTROL: u8 = 0x8; // Auxiliary output 2
const DEFAULT_MODEM_STATUS: u8 = 0x20 | 0x10 | 0x80; // data ready, clear to send, carrier detect
const DEFAULT_BAUD_DIVISOR: u16 = 12; // 9600 bps

/// Emulates serial COM ports commonly seen on x86 I/O ports 0x3f8/0x2f8/0x3e8/0x2e8.
///
/// This can optionally write the guest's output to a Write trait object. To send input to the
/// guest, use `queue_input_bytes`.
pub struct Serial {
    id: String,
    interrupt_enable: u8,
    interrupt_identification: u8,
    interrupt: Arc<dyn InterruptSourceGroup>,
    line_control: u8,
    line_status: u8,
    modem_control: u8,
    modem_status: u8,
    scratch: u8,
    baud_divisor: u16,
    in_buffer: VecDeque<u8>,
    out: Option<Box<dyn io::Write + Send>>,
}

#[derive(Serialize, Deserialize)]
pub struct SerialState {
    interrupt_enable: u8,
    interrupt_identification: u8,
    line_control: u8,
    line_status: u8,
    modem_control: u8,
    modem_status: u8,
    scratch: u8,
    baud_divisor: u16,
    in_buffer: Vec<u8>,
}

impl Serial {
    pub fn new(
        id: String,
        interrupt: Arc<dyn InterruptSourceGroup>,
        out: Option<Box<dyn io::Write + Send>>,
    ) -> Serial {
        Serial {
            id,
            interrupt_enable: 0,
            interrupt_identification: DEFAULT_INTERRUPT_IDENTIFICATION,
            interrupt,
            line_control: DEFAULT_LINE_CONTROL,
            line_status: DEFAULT_LINE_STATUS,
            modem_control: DEFAULT_MODEM_CONTROL,
            modem_status: DEFAULT_MODEM_STATUS,
            scratch: 0,
            baud_divisor: DEFAULT_BAUD_DIVISOR,
            in_buffer: VecDeque::new(),
            out,
        }
    }

    /// Constructs a Serial port ready for output.
    pub fn new_out(
        id: String,
        interrupt: Arc<dyn InterruptSourceGroup>,
        out: Box<dyn io::Write + Send>,
    ) -> Serial {
        Self::new(id, interrupt, Some(out))
    }

    /// Constructs a Serial port with no connected output.
    pub fn new_sink(id: String, interrupt: Arc<dyn InterruptSourceGroup>) -> Serial {
        Self::new(id, interrupt, None)
    }

    pub fn set_out(&mut self, out: Box<dyn io::Write + Send>) {
        self.out = Some(out);
    }

    /// Queues raw bytes for the guest to read and signals the interrupt if the line status would
    /// change. Bytes that do not fit in the input buffer are dropped.
    pub fn queue_input_bytes(&mut self, c: &[u8]) -> Result<()> {
        if !self.is_loop() {
            let available = INPUT_BUFFER_SIZE.saturating_sub(self.in_buffer.len());
            self.in_buffer.extend(c.iter().copied().take(available));
            if c.len() > available {
                self.line_status |= LSR_OVERRUN_BIT;
            }
            if available != 0 && !c.is_empty() {
                self.recv_data()?;
            }
        }
        Ok(())
    }

    pub fn flush_output(&mut self) -> result::Result<(), io::Error> {
        if let Some(out) = self.out.as_mut() {
            out.flush()?;
        }
        Ok(())
    }

    fn is_dlab_set(&self) -> bool {
        (self.line_control & LCR_DLAB_BIT) != 0
    }

    fn is_recv_intr_enabled(&self) -> bool {
        (self.interrupt_enable & IER_RECV_BIT) != 0
    }

    fn is_thr_intr_enabled(&self) -> bool {
        (self.interrupt_enable & IER_THR_BIT) != 0
    }

    fn is_loop(&self) -> bool {
        (self.modem_control & MCR_LOOP_BIT) != 0
    }

    fn add_intr_bit(&mut self, bit: u8) {
        self.interrupt_identification &= !IIR_NONE_BIT;
        self.interrupt_identification |= bit;
    }

    fn del_intr_bit(&mut self, bit: u8) {
        self.interrupt_identification &= !bit;
        if self.interrupt_identification == 0x0 {
            self.interrupt_identification = IIR_NONE_BIT;
        }
    }

    fn thr_empty(&mut self) -> Result<()> {
        if self.is_thr_intr_enabled() {
            self.add_intr_bit(IIR_THR_BIT);
            self.trigger_interrupt()?
        }
        Ok(())
    }

    fn recv_data(&mut self) -> Result<()> {
        if self.is_recv_intr_enabled() {
            self.add_intr_bit(IIR_RECV_BIT);
            self.trigger_interrupt()?
        }
        self.line_status |= LSR_DATA_BIT;
        Ok(())
    }

    fn trigger_interrupt(&mut self) -> result::Result<(), io::Error> {
        self.interrupt.trigger(0)
    }

    fn iir_reset(&mut self) {
        self.interrupt_identification = DEFAULT_INTERRUPT_IDENTIFICATION;
    }

    fn handle_write(&mut self, offset: u8, v: u8) -> Result<()> {
        match offset {
            DLAB_LOW if self.is_dlab_set() => {
                self.baud_divisor = (self.baud_divisor & 0xff00) | u16::from(v)
            }
            DLAB_HIGH if self.is_dlab_set() => {
                self.baud_divisor = (self.baud_divisor & 0x00ff) | ((u16::from(v)) << 8)
            }
            DATA => {
                if self.is_loop() {
                    if self.in_buffer.len() < INPUT_BUFFER_SIZE {
                        self.in_buffer.push_back(v);
                        self.recv_data()?;
                    }
                } else {
                    if let Some(out) = self.out.as_mut() {
                        out.write_all(&[v])?;
                        out.flush()?;
                    }
                    self.thr_empty()?;
                }
            }
            IER => {
                let recv_intr_was_enabled = self.is_recv_intr_enabled();
                self.interrupt_enable = v & IER_FIFO_BITS;
                if !recv_intr_was_enabled
                    && self.is_recv_intr_enabled()
                    && !self.in_buffer.is_empty()
                {
                    self.recv_data()?;
                }
            }
            LCR => self.line_control = v,
            MCR => self.modem_control = v,
            SCR => self.scratch = v,
            _ => {}
        }
        Ok(())
    }

    fn state(&self) -> SerialState {
        SerialState {
            interrupt_enable: self.interrupt_enable,
            interrupt_identification: self.interrupt_identification,
            line_control: self.line_control,
            line_status: self.line_status,
            modem_control: self.modem_control,
            modem_status: self.modem_status,
            scratch: self.scratch,
            baud_divisor: self.baud_divisor,
            in_buffer: self
                .in_buffer
                .iter()
                .copied()
                .take(INPUT_BUFFER_SIZE)
                .collect(),
        }
    }

    fn set_state(&mut self, state: &SerialState) {
        self.interrupt_enable = state.interrupt_enable;
        self.interrupt_identification = state.interrupt_identification;
        self.line_control = state.line_control;
        self.line_status = state.line_status;
        self.modem_control = state.modem_control;
        self.modem_status = state.modem_status;
        self.scratch = state.scratch;
        self.baud_divisor = state.baud_divisor;
        self.in_buffer = state
            .in_buffer
            .iter()
            .copied()
            .take(INPUT_BUFFER_SIZE)
            .collect();
    }
}

impl BusDevice for Serial {
    fn read(&mut self, _base: u64, offset: u64, data: &mut [u8]) {
        if data.len() != 1 {
            return;
        }

        data[0] = match offset as u8 {
            DLAB_LOW if self.is_dlab_set() => self.baud_divisor as u8,
            DLAB_HIGH if self.is_dlab_set() => (self.baud_divisor >> 8) as u8,
            DATA => {
                self.del_intr_bit(IIR_RECV_BIT);
                if self.in_buffer.len() <= 1 {
                    self.line_status &= !LSR_DATA_BIT;
                }
                self.in_buffer.pop_front().unwrap_or_default()
            }
            IER => self.interrupt_enable,
            IIR => {
                let v = self.interrupt_identification | IIR_FIFO_BITS;
                self.iir_reset();
                v
            }
            LCR => self.line_control,
            MCR => self.modem_control,
            LSR => {
                let status = self.line_status;
                self.line_status &= !LSR_OVERRUN_BIT;
                status
            }
            MSR => self.modem_status,
            SCR => self.scratch,
            _ => 0,
        };
    }

    fn write(&mut self, _base: u64, offset: u64, data: &[u8]) -> Option<Arc<Barrier>> {
        if data.len() != 1 {
            return None;
        }

        self.handle_write(offset as u8, data[0]).ok();

        None
    }
}

impl Snapshottable for Serial {
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

impl Pausable for Serial {}
impl Transportable for Serial {}
impl Migratable for Serial {}

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
    fn serial_output() {
        let intr_evt = EventFd::new(0).unwrap();
        let serial_out = SharedBuffer::new();
        let mut serial = Serial::new_out(
            String::from(SERIAL_NAME),
            Arc::new(TestInterrupt::new(intr_evt.try_clone().unwrap())),
            Box::new(serial_out.clone()),
        );

        serial.write(0, DATA as u64, &[b'x', b'y']);
        serial.write(0, DATA as u64, &[b'a']);
        serial.write(0, DATA as u64, &[b'b']);
        serial.write(0, DATA as u64, &[b'c']);
        assert_eq!(
            serial_out.buf.lock().unwrap().as_slice(),
            &[b'a', b'b', b'c']
        );
    }

    #[test]
    fn serial_input() {
        let intr_evt = EventFd::new(0).unwrap();
        let serial_out = SharedBuffer::new();
        let mut serial = Serial::new_out(
            String::from(SERIAL_NAME),
            Arc::new(TestInterrupt::new(intr_evt.try_clone().unwrap())),
            Box::new(serial_out),
        );

        // write 1 to the interrupt event fd, so that read doesn't block in case the event fd
        // counter doesn't change (for 0 it blocks)
        assert!(intr_evt.write(1).is_ok());
        serial.write(0, IER as u64, &[IER_RECV_BIT]);
        serial.queue_input_bytes(&[b'a', b'b', b'c']).unwrap();

        assert_eq!(intr_evt.read().unwrap(), 2);

        // check if reading in a 2-length array doesn't have side effects
        let mut data = [0u8, 0u8];
        serial.read(0, DATA as u64, &mut data[..]);
        assert_eq!(data, [0u8, 0u8]);

        let mut data = [0u8];
        serial.read(0, LSR as u64, &mut data[..]);
        assert_ne!(data[0] & LSR_DATA_BIT, 0);
        serial.read(0, DATA as u64, &mut data[..]);
        assert_eq!(data[0], b'a');
        serial.read(0, DATA as u64, &mut data[..]);
        assert_eq!(data[0], b'b');
        serial.read(0, DATA as u64, &mut data[..]);
        assert_eq!(data[0], b'c');

        // check if reading from the largest u8 offset returns 0
        serial.read(0, 0xff, &mut data[..]);
        assert_eq!(data[0], 0);
    }

    #[test]
    fn serial_input_is_bounded() {
        let intr_evt = EventFd::new(0).unwrap();
        let mut serial = Serial::new_sink(
            String::from(SERIAL_NAME),
            Arc::new(TestInterrupt::new(intr_evt)),
        );
        let input: Vec<u8> = (0..INPUT_BUFFER_SIZE as u8 + 1).collect();

        serial.queue_input_bytes(&input).unwrap();

        assert_eq!(serial.in_buffer.len(), INPUT_BUFFER_SIZE);
        assert_eq!(serial.state().in_buffer.len(), INPUT_BUFFER_SIZE);
        let mut data = [0u8];
        serial.read(0, LSR as u64, &mut data);
        assert_ne!(data[0] & LSR_OVERRUN_BIT, 0);
        serial.read(0, LSR as u64, &mut data);
        assert_eq!(data[0] & LSR_OVERRUN_BIT, 0);
        for expected in input.iter().take(INPUT_BUFFER_SIZE) {
            serial.read(0, DATA as u64, &mut data);
            assert_eq!(data[0], *expected);
        }
        serial.read(0, DATA as u64, &mut data);
        assert_eq!(data[0], 0);
    }

    #[test]
    fn serial_input_interrupts_when_unmasked() {
        let intr_evt = EventFd::new(libc::EFD_NONBLOCK).unwrap();
        let mut serial = Serial::new_sink(
            String::from(SERIAL_NAME),
            Arc::new(TestInterrupt::new(intr_evt.try_clone().unwrap())),
        );

        serial.queue_input_bytes(&[b'a']).unwrap();
        assert_eq!(
            intr_evt.read().unwrap_err().raw_os_error(),
            Some(libc::EAGAIN)
        );

        serial.write(0, IER as u64, &[IER_RECV_BIT]);
        assert_eq!(intr_evt.read().unwrap(), 1);

        let mut data = [0u8];
        serial.read(0, DATA as u64, &mut data);
        assert_eq!(data[0], b'a');
    }

    #[test]
    fn serial_thr() {
        let intr_evt = EventFd::new(0).unwrap();
        let mut serial = Serial::new_sink(
            String::from(SERIAL_NAME),
            Arc::new(TestInterrupt::new(intr_evt.try_clone().unwrap())),
        );

        // write 1 to the interrupt event fd, so that read doesn't block in case the event fd
        // counter doesn't change (for 0 it blocks)
        assert!(intr_evt.write(1).is_ok());
        serial.write(0, IER as u64, &[IER_THR_BIT]);
        serial.write(0, DATA as u64, &[b'a']);

        assert_eq!(intr_evt.read().unwrap(), 2);
        let mut data = [0u8];
        serial.read(0, IER as u64, &mut data[..]);
        assert_eq!(data[0] & IER_FIFO_BITS, IER_THR_BIT);
        serial.read(0, IIR as u64, &mut data[..]);
        assert_ne!(data[0] & IIR_THR_BIT, 0);
    }

    #[test]
    fn serial_dlab() {
        let intr_evt = EventFd::new(0).unwrap();
        let mut serial = Serial::new_sink(
            String::from(SERIAL_NAME),
            Arc::new(TestInterrupt::new(intr_evt.try_clone().unwrap())),
        );

        serial.write(0, LCR as u64, &[LCR_DLAB_BIT]);
        serial.write(0, DLAB_LOW as u64, &[0x12]);
        serial.write(0, DLAB_HIGH as u64, &[0x34]);

        let mut data = [0u8];
        serial.read(0, LCR as u64, &mut data[..]);
        assert_eq!(data[0], LCR_DLAB_BIT);
        serial.read(0, DLAB_LOW as u64, &mut data[..]);
        assert_eq!(data[0], 0x12);
        serial.read(0, DLAB_HIGH as u64, &mut data[..]);
        assert_eq!(data[0], 0x34);
    }

    #[test]
    fn serial_modem() {
        let intr_evt = EventFd::new(0).unwrap();
        let mut serial = Serial::new_sink(
            String::from(SERIAL_NAME),
            Arc::new(TestInterrupt::new(intr_evt.try_clone().unwrap())),
        );

        serial.write(0, MCR as u64, &[MCR_LOOP_BIT]);
        serial.write(0, DATA as u64, &[b'a']);
        serial.write(0, DATA as u64, &[b'b']);
        serial.write(0, DATA as u64, &[b'c']);

        let mut data = [0u8];
        serial.read(0, MSR as u64, &mut data[..]);
        assert_eq!(data[0], DEFAULT_MODEM_STATUS);
        serial.read(0, MCR as u64, &mut data[..]);
        assert_eq!(data[0], MCR_LOOP_BIT);
        serial.read(0, DATA as u64, &mut data[..]);
        assert_eq!(data[0], b'a');
        serial.read(0, DATA as u64, &mut data[..]);
        assert_eq!(data[0], b'b');
        serial.read(0, DATA as u64, &mut data[..]);
        assert_eq!(data[0], b'c');
    }

    #[test]
    fn serial_scratch() {
        let intr_evt = EventFd::new(0).unwrap();
        let mut serial = Serial::new_sink(
            String::from(SERIAL_NAME),
            Arc::new(TestInterrupt::new(intr_evt.try_clone().unwrap())),
        );

        serial.write(0, SCR as u64, &[0x12]);

        let mut data = [0u8];
        serial.read(0, SCR as u64, &mut data[..]);
        assert_eq!(data[0], 0x12);
    }
}

// Copyright © 2022 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

#![no_main]
use devices::legacy::Serial;
use libc::EFD_NONBLOCK;
use libfuzzer_sys::fuzz_target;
use std::sync::Arc;
use vm_device::interrupt::{InterruptIndex, InterruptSourceConfig, InterruptSourceGroup};
use vm_device::BusDevice;
use vmm_sys_util::eventfd::EventFd;

fuzz_target!(|bytes| {
    let mut serial = Serial::new_sink(
        "serial".into(),
        Arc::new(TestInterrupt::new(EventFd::new(EFD_NONBLOCK).unwrap())),
    );

    let mut i = 0;
    while i < bytes.len() {
        let choice = bytes.get(i).unwrap_or(&0) % 3;
        i += 1;

        match choice {
            0 => {
                let offset = (bytes.get(i).unwrap_or(&0) % 8) as u64;
                i += 1;
                let mut out_bytes = vec![0];
                serial.read(0, offset, &mut out_bytes);
            }
            1 => {
                let offset = (bytes.get(i).unwrap_or(&0) % 8) as u64;
                i += 1;
                let data = vec![*bytes.get(i).unwrap_or(&0)];
                i += 1;
                serial.write(0, offset, &data);
            }
            _ => {
                let data = vec![*bytes.get(i).unwrap_or(&0)];
                i += 1;
                serial.queue_input_bytes(&data).ok();
            }
        }
    }
});

struct TestInterrupt {
    event_fd: EventFd,
}

impl InterruptSourceGroup for TestInterrupt {
    fn trigger(&self, _index: InterruptIndex) -> Result<(), std::io::Error> {
        self.event_fd.write(1)
    }
    fn update(
        &self,
        _index: InterruptIndex,
        _config: InterruptSourceConfig,
        _masked: bool,
    ) -> Result<(), std::io::Error> {
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

// Copyright © 2022 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use std::fmt;
use std::time::Instant;
use vm_device::BusDevice;

/// Debug I/O port, see:
/// https://www.intel.com/content/www/us/en/support/articles/000005500/boards-and-kits.html
///
/// Since we're not a physical platform, we can freely assign code ranges for
/// debugging specific parts of our virtual platform.
pub enum DebugIoPortRange {
    Firmware,
    Bootloader,
    Kernel,
    Userspace,
    Custom,
}

#[cfg(target_arch = "x86_64")]
const DEBUG_IOPORT_PREFIX: &str = "Debug I/O port";

#[cfg(target_arch = "x86_64")]
impl DebugIoPortRange {
    fn from_u8(value: u8) -> DebugIoPortRange {
        match value {
            0x00..=0x1f => DebugIoPortRange::Firmware,
            0x20..=0x3f => DebugIoPortRange::Bootloader,
            0x40..=0x5f => DebugIoPortRange::Kernel,
            0x60..=0x7f => DebugIoPortRange::Userspace,
            _ => DebugIoPortRange::Custom,
        }
    }
}

#[cfg(target_arch = "x86_64")]
impl fmt::Display for DebugIoPortRange {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        match self {
            DebugIoPortRange::Firmware => write!(f, "{}: Firmware", DEBUG_IOPORT_PREFIX),
            DebugIoPortRange::Bootloader => write!(f, "{}: Bootloader", DEBUG_IOPORT_PREFIX),
            DebugIoPortRange::Kernel => write!(f, "{}: Kernel", DEBUG_IOPORT_PREFIX),
            DebugIoPortRange::Userspace => write!(f, "{}: Userspace", DEBUG_IOPORT_PREFIX),
            DebugIoPortRange::Custom => write!(f, "{}: Custom", DEBUG_IOPORT_PREFIX),
        }
    }
}

pub struct DebugPort {
    timestamp: Instant,
}

impl DebugPort {
    pub fn new(timestamp: Instant) -> Self {
        Self { timestamp }
    }
}

impl BusDevice for DebugPort {
    fn read(&mut self, _base: u64, _offset: u64, _data: &mut [u8]) {
        error!("Invalid read to debug port")
    }

    fn write(
        &mut self,
        _base: u64,
        _offset: u64,
        data: &[u8],
    ) -> Option<std::sync::Arc<std::sync::Barrier>> {
        let elapsed = self.timestamp.elapsed();

        let code = data[0];
        warn!(
            "[{} code 0x{:x}] {}.{:>06} seconds",
            DebugIoPortRange::from_u8(code),
            code,
            elapsed.as_secs(),
            elapsed.as_micros()
        );

        None
    }
}

// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.

//! Emulates virtual and hardware devices.

#[macro_use]
extern crate bitflags;
#[macro_use]
extern crate log;

pub mod acpi;
#[cfg(target_arch = "aarch64")]
pub mod gic;
pub mod interrupt_controller;
#[cfg(target_arch = "x86_64")]
pub mod ioapic;
pub mod ivshmem;
pub mod legacy;
pub mod pvpanic;
pub mod tpm;

pub use self::acpi::{AcpiGedDevice, AcpiPmTimerDevice, AcpiShutdownDevice};
pub use self::ivshmem::IvshmemDevice;
pub use self::pvpanic::{PvPanicDevice, PVPANIC_DEVICE_MMIO_SIZE};

bitflags! {
    pub struct AcpiNotificationFlags: u8 {
        const NO_DEVICES_CHANGED = 0;
        const CPU_DEVICES_CHANGED = 0b1;
        const MEMORY_DEVICES_CHANGED = 0b10;
        const PCI_DEVICES_CHANGED = 0b100;
        const POWER_BUTTON_CHANGED = 0b1000;
    }
}

#[allow(unused_macros)]
#[cfg(target_arch = "aarch64")]
macro_rules! generate_read_fn {
    ($fn_name: ident, $data_type: ty, $byte_type: ty, $type_size: expr, $endian_type: ident) => {
        #[allow(dead_code)]
        pub fn $fn_name(input: &[$byte_type]) -> $data_type {
            assert!($type_size == std::mem::size_of::<$data_type>());
            let mut array = [0u8; $type_size];
            for (byte, read) in array.iter_mut().zip(input.iter().cloned()) {
                *byte = read as u8;
            }
            <$data_type>::$endian_type(array)
        }
    };
}

#[allow(unused_macros)]
#[cfg(target_arch = "aarch64")]
macro_rules! generate_write_fn {
    ($fn_name: ident, $data_type: ty, $byte_type: ty, $endian_type: ident) => {
        #[allow(dead_code)]
        pub fn $fn_name(buf: &mut [$byte_type], n: $data_type) {
            for (byte, read) in buf
                .iter_mut()
                .zip(<$data_type>::$endian_type(n).iter().cloned())
            {
                *byte = read as $byte_type;
            }
        }
    };
}

#[cfg(target_arch = "aarch64")]
generate_read_fn!(read_le_u16, u16, u8, 2, from_le_bytes);
#[cfg(target_arch = "aarch64")]
generate_read_fn!(read_le_u32, u32, u8, 4, from_le_bytes);
#[cfg(target_arch = "aarch64")]
generate_read_fn!(read_le_u64, u64, u8, 8, from_le_bytes);
#[cfg(target_arch = "aarch64")]
generate_read_fn!(read_le_i32, i32, i8, 4, from_le_bytes);

#[cfg(target_arch = "aarch64")]
generate_read_fn!(read_be_u16, u16, u8, 2, from_be_bytes);
#[cfg(target_arch = "aarch64")]
generate_read_fn!(read_be_u32, u32, u8, 4, from_be_bytes);

#[cfg(target_arch = "aarch64")]
generate_write_fn!(write_le_u16, u16, u8, to_le_bytes);
#[cfg(target_arch = "aarch64")]
generate_write_fn!(write_le_u32, u32, u8, to_le_bytes);
#[cfg(target_arch = "aarch64")]
generate_write_fn!(write_le_u64, u64, u8, to_le_bytes);
#[cfg(target_arch = "aarch64")]
generate_write_fn!(write_le_i32, i32, i8, to_le_bytes);

#[cfg(target_arch = "aarch64")]
generate_write_fn!(write_be_u16, u16, u8, to_be_bytes);
#[cfg(target_arch = "aarch64")]
generate_write_fn!(write_be_u32, u32, u8, to_be_bytes);

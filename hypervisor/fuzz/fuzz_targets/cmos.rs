// Copyright © 2022 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

#![no_main]
use devices::legacy::Cmos;
use libc::EFD_NONBLOCK;
use libfuzzer_sys::fuzz_target;
use vm_device::BusDevice;
use vmm_sys_util::eventfd::EventFd;

fuzz_target!(|bytes| {
    // Need at least 16 bytes for the test
    if bytes.len() < 16 {
        return;
    }

    let mut below_4g = [0u8; 8];
    let mut above_4g = [0u8; 8];

    below_4g.copy_from_slice(&bytes[0..8]);
    above_4g.copy_from_slice(&bytes[8..16]);

    let mut cmos = Cmos::new(
        u64::from_le_bytes(below_4g),
        u64::from_le_bytes(above_4g),
        EventFd::new(EFD_NONBLOCK).unwrap(),
    );

    let mut i = 16;
    while i < bytes.len() {
        let read = bytes.get(i).unwrap_or(&0) % 2 == 0;
        i += 1;

        if read {
            let offset = (bytes.get(i).unwrap_or(&0) % 2) as u64;
            i += 1;
            let mut out_bytes = vec![0];
            cmos.read(0, offset, &mut out_bytes);
        } else {
            let offset = (bytes.get(i).unwrap_or(&0) % 2) as u64;
            i += 1;
            let data = vec![*bytes.get(i).unwrap_or(&0)];
            i += 1;
            cmos.write(0, offset, &data);
        }
    }
});

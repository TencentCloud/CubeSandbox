// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.
//
// Copyright © 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//

use std::sync::{Arc, Barrier};
use vm_device::BusDevice;

/// Provides firmware debug output via I/O port controls
#[derive(Default)]
pub struct FwDebugDevice {}

impl FwDebugDevice {
    pub fn new() -> Self {
        Self {}
    }
}

/// FwDebugDevice sits on the I/O bus as 0x402 and receives ASCII characters
impl BusDevice for FwDebugDevice {
    /// Upon read return the magic value to indicate that there is a debug port
    fn read(&mut self, _base: u64, _offset: u64, data: &mut [u8]) {
        if data.len() == 1 {
            data[0] = 0xe9
        } else {
            error!("Invalid read size on debug port: {}", data.len())
        }
    }

    fn write(&mut self, _base: u64, _offset: u64, data: &[u8]) -> Option<Arc<Barrier>> {
        if data.len() == 1 {
            print!("{}", data[0] as char);
        } else {
            error!("Invalid write size on debug port: {}", data.len())
        }

        None
    }
}

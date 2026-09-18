// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.
//
// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause

//! Implements virtio queues

use std::fmt::{self, Debug};
use std::sync::Arc;
use virtio_queue::{Queue, QueueT};
use vm_memory::GuestAddress;

pub mod checked_descriptor;
pub mod queue;
pub use queue::*;

pub const VIRTIO_MSI_NO_VECTOR: u16 = 0xffff;

// Types taken from linux/virtio_ids.h
#[derive(Copy, Clone, Debug)]
#[allow(dead_code)]
#[allow(non_camel_case_types)]
#[repr(C)]
pub enum VirtioDeviceType {
    Net = 1,
    Block = 2,
    Console = 3,
    Rng = 4,
    Balloon = 5,
    Fs9P = 9,
    Gpu = 16,
    Input = 18,
    Vsock = 19,
    Iommu = 23,
    Mem = 24,
    Fs = 26,
    Pmem = 27,
    Watchdog = 35, // Temporary until official number allocated
    Unknown = 0xFF,
}

impl From<u32> for VirtioDeviceType {
    fn from(t: u32) -> Self {
        match t {
            1 => VirtioDeviceType::Net,
            2 => VirtioDeviceType::Block,
            3 => VirtioDeviceType::Console,
            4 => VirtioDeviceType::Rng,
            5 => VirtioDeviceType::Balloon,
            9 => VirtioDeviceType::Fs9P,
            16 => VirtioDeviceType::Gpu,
            18 => VirtioDeviceType::Input,
            19 => VirtioDeviceType::Vsock,
            23 => VirtioDeviceType::Iommu,
            24 => VirtioDeviceType::Mem,
            26 => VirtioDeviceType::Fs,
            27 => VirtioDeviceType::Pmem,
            35 => VirtioDeviceType::Watchdog,
            _ => VirtioDeviceType::Unknown,
        }
    }
}

// In order to use the `{}` marker, the trait `fmt::Display` must be implemented
// manually for the type VirtioDeviceType.
impl fmt::Display for VirtioDeviceType {
    // This trait requires `fmt` with this exact signature.
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        let output = match *self {
            VirtioDeviceType::Net => "net",
            VirtioDeviceType::Block => "block",
            VirtioDeviceType::Console => "console",
            VirtioDeviceType::Rng => "rng",
            VirtioDeviceType::Balloon => "balloon",
            VirtioDeviceType::Gpu => "gpu",
            VirtioDeviceType::Fs9P => "9p",
            VirtioDeviceType::Input => "input",
            VirtioDeviceType::Vsock => "vsock",
            VirtioDeviceType::Iommu => "iommu",
            VirtioDeviceType::Mem => "mem",
            VirtioDeviceType::Fs => "fs",
            VirtioDeviceType::Pmem => "pmem",
            VirtioDeviceType::Watchdog => "watchdog",
            VirtioDeviceType::Unknown => "UNKNOWN",
        };
        write!(f, "{}", output)
    }
}

/// Trait for devices with access to data in memory being limited and/or
/// translated.
pub trait AccessPlatform: Send + Sync + Debug {
    /// Provide a way to translate GVA address ranges into GPAs.
    fn translate_gva(&self, base: u64, size: u64) -> std::result::Result<u64, std::io::Error>;
    /// Provide a way to translate GPA address ranges into GVAs.
    fn translate_gpa(&self, base: u64, size: u64) -> std::result::Result<u64, std::io::Error>;
}

pub trait Translatable {
    fn translate_gva(&self, access_platform: Option<&Arc<dyn AccessPlatform>>, len: usize) -> Self;
    fn translate_gpa(&self, access_platform: Option<&Arc<dyn AccessPlatform>>, len: usize) -> Self;
}

impl Translatable for GuestAddress {
    fn translate_gva(&self, access_platform: Option<&Arc<dyn AccessPlatform>>, len: usize) -> Self {
        GuestAddress(self.0.translate_gva(access_platform, len))
    }
    fn translate_gpa(&self, access_platform: Option<&Arc<dyn AccessPlatform>>, len: usize) -> Self {
        GuestAddress(self.0.translate_gpa(access_platform, len))
    }
}

impl Translatable for u64 {
    fn translate_gva(&self, access_platform: Option<&Arc<dyn AccessPlatform>>, len: usize) -> Self {
        if let Some(access_platform) = access_platform {
            access_platform.translate_gva(*self, len as u64).unwrap()
        } else {
            *self
        }
    }
    fn translate_gpa(&self, access_platform: Option<&Arc<dyn AccessPlatform>>, len: usize) -> Self {
        if let Some(access_platform) = access_platform {
            access_platform.translate_gpa(*self, len as u64).unwrap()
        } else {
            *self
        }
    }
}

/// Helper for cloning a Queue since QueueState doesn't derive Clone
pub fn clone_queue(queue: &Queue) -> Queue {
    let mut q = Queue::new(queue.max_size()).unwrap();

    q.set_next_avail(queue.next_avail());
    q.set_next_used(queue.next_used());
    q.set_event_idx(queue.event_idx_enabled());
    q.set_size(queue.size());
    q.set_ready(queue.ready());
    q.try_set_desc_table_address(GuestAddress(queue.desc_table()))
        .unwrap();
    q.try_set_avail_ring_address(GuestAddress(queue.avail_ring()))
        .unwrap();
    q.try_set_used_ring_address(GuestAddress(queue.used_ring()))
        .unwrap();

    q
}

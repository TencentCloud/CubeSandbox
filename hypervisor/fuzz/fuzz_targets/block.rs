// Copyright 2018 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.
//
// Copyright © 2022 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause

#![no_main]

use block_util::{async_io::DiskFile, raw_sync::RawFileDiskSync};
use libfuzzer_sys::fuzz_target;
use seccompiler::SeccompAction;
use std::ffi;
use std::fs::File;
use std::io;
use std::os::unix::io::{AsRawFd, FromRawFd, RawFd};
use std::path::PathBuf;
use std::sync::Arc;
use virtio_devices::{Block, VirtioDevice, VirtioInterrupt, VirtioInterruptType};
use virtio_queue::{Queue, QueueT};
use vm_memory::{bitmap::AtomicBitmap, Bytes, GuestAddress, GuestMemoryAtomic};
use vmm_sys_util::eventfd::{EventFd, EFD_NONBLOCK};

type GuestMemoryMmap = vm_memory::GuestMemoryMmap<AtomicBitmap>;

const QUEUE_DATA_SIZE: usize = 4;
const MEM_SIZE: usize = 256 * 1024 * 1024;
// Max entries in the queue.
const QUEUE_SIZE: u16 = 256;
// Guest physical address for descriptor table.
const DESC_TABLE_ADDR: u64 = 0;
const DESC_TABLE_SIZE: u64 = 16_u64 * QUEUE_SIZE as u64;
// Guest physical address for available ring
const AVAIL_RING_ADDR: u64 = DESC_TABLE_ADDR + DESC_TABLE_SIZE;
const AVAIL_RING_SIZE: u64 = 6_u64 + 2 * QUEUE_SIZE as u64;
// Guest physical address for used ring (requires to 4-bytes aligned)
const USED_RING_ADDR: u64 = (AVAIL_RING_ADDR + AVAIL_RING_SIZE + 3) & !3_u64;

fuzz_target!(|bytes| {
    if bytes.len() < QUEUE_DATA_SIZE || bytes.len() > (QUEUE_DATA_SIZE + MEM_SIZE) {
        return;
    }

    let queue_data = &bytes[..QUEUE_DATA_SIZE];
    let mem_bytes = &bytes[QUEUE_DATA_SIZE..];

    // Create a virtio-block device backed by a synchronous raw file
    let shm = memfd_create(&ffi::CString::new("fuzz").unwrap(), 0).unwrap();
    let disk_file: File = unsafe { File::from_raw_fd(shm) };
    let qcow_disk = Box::new(RawFileDiskSync::new(disk_file)) as Box<dyn DiskFile>;
    let mut block = Block::new(
        "tmp".to_owned(),
        qcow_disk,
        PathBuf::from(""),
        false,
        false,
        2,
        256,
        SeccompAction::Allow,
        None,
        EventFd::new(EFD_NONBLOCK).unwrap(),
        None,
    )
    .unwrap();

    // Setup the virt queue with the input bytes
    let q = setup_virt_queue(queue_data.try_into().unwrap());

    // Setup the guest memory with the input bytes
    let mem = GuestMemoryMmap::from_ranges(&[(GuestAddress(0), MEM_SIZE)]).unwrap();
    if mem.write_slice(mem_bytes, GuestAddress(0 as u64)).is_err() {
        return;
    }
    let guest_memory = GuestMemoryAtomic::new(mem);

    let evt = EventFd::new(0).unwrap();
    let queue_evt = unsafe { EventFd::from_raw_fd(libc::dup(evt.as_raw_fd())) };

    // Kick the 'queue' event before activate the block device
    queue_evt.write(1).unwrap();

    block
        .activate(
            guest_memory,
            Arc::new(NoopVirtioInterrupt {}),
            vec![(0, q, evt)],
        )
        .ok();

    // Wait for the events to finish and block device worker thread to return
    block.wait_for_epoll_threads();
});

fn memfd_create(name: &ffi::CStr, flags: u32) -> Result<RawFd, io::Error> {
    let res = unsafe { libc::syscall(libc::SYS_memfd_create, name.as_ptr(), flags) };

    if res < 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(res as RawFd)
    }
}

pub struct NoopVirtioInterrupt {}

impl VirtioInterrupt for NoopVirtioInterrupt {
    fn trigger(&self, _int_type: VirtioInterruptType) -> std::result::Result<(), std::io::Error> {
        Ok(())
    }
}

fn setup_virt_queue(bytes: &[u8; QUEUE_DATA_SIZE]) -> Queue {
    let mut q = Queue::new(QUEUE_SIZE).unwrap();
    q.set_next_avail(bytes[0] as u16); // 'u8' is enough given the 'QUEUE_SIZE' is small
    q.set_next_used(bytes[1] as u16);
    q.set_event_idx(bytes[2] % 2 != 0);
    q.set_size(bytes[3] as u16 % QUEUE_SIZE);

    q.try_set_desc_table_address(GuestAddress(DESC_TABLE_ADDR))
        .unwrap();
    q.try_set_avail_ring_address(GuestAddress(AVAIL_RING_ADDR))
        .unwrap();
    q.try_set_used_ring_address(GuestAddress(USED_RING_ADDR))
        .unwrap();
    q.set_ready(true);

    q
}

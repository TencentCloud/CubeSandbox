// Copyright © 2022 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

#![no_main]

use libfuzzer_sys::fuzz_target;
use seccompiler::SeccompAction;
use std::os::unix::io::{AsRawFd, FromRawFd};
use std::sync::Arc;
use virtio_devices::{VirtioDevice, VirtioInterrupt, VirtioInterruptType};
use virtio_queue::{Queue, QueueT};
use vm_memory::{bitmap::AtomicBitmap, Bytes, GuestAddress, GuestMemoryAtomic};
use vmm_sys_util::eventfd::{EventFd, EFD_NONBLOCK};

type GuestMemoryMmap = vm_memory::GuestMemoryMmap<AtomicBitmap>;

const QUEUE_DATA_SIZE: usize = 4;
const MEM_SIZE: usize = 512 * 1024;
const BALLOON_SIZE: u64 = 512 * 1024;
// Number of queues
const QUEUE_NUM: usize = 3;
// Max entries in the queue.
const QUEUE_SIZE: u16 = 64;
// Descriptor table alignment
const DESC_TABLE_ALIGN_SIZE: u64 = 16;
// Avalable ring alignment
const AVAIL_RING_ALIGN_SIZE: u64 = 2;
// Used ring alignment
const USED_RING_ALIGN_SIZE: u64 = 4;
// Descriptor table size
const DESC_TABLE_SIZE: u64 = 16_u64 * QUEUE_SIZE as u64;
// Available ring size
const AVAIL_RING_SIZE: u64 = 6_u64 + 2 * QUEUE_SIZE as u64;
// Used ring size
const USED_RING_SIZE: u64 = 6_u64 + 8 * QUEUE_SIZE as u64;

fuzz_target!(|bytes| {
    if bytes.len() < QUEUE_DATA_SIZE * QUEUE_NUM
        || bytes.len() > (QUEUE_DATA_SIZE * QUEUE_NUM + MEM_SIZE)
    {
        return;
    }

    let mut balloon = virtio_devices::Balloon::new(
        "fuzzer_balloon".to_owned(),
        BALLOON_SIZE,
        true,
        true,
        SeccompAction::Allow,
        EventFd::new(EFD_NONBLOCK).unwrap(),
        None,
    )
    .unwrap();

    let queue_data = &bytes[..QUEUE_DATA_SIZE * QUEUE_NUM];
    let mem_bytes = &bytes[QUEUE_DATA_SIZE * QUEUE_NUM..];

    // Setup the guest memory with the input bytes
    let mem = GuestMemoryMmap::from_ranges(&[(GuestAddress(0), MEM_SIZE)]).unwrap();
    if mem.write_slice(mem_bytes, GuestAddress(0 as u64)).is_err() {
        return;
    }
    let guest_memory = GuestMemoryAtomic::new(mem);

    // Setup the virt queues with the input bytes
    let mut queues = setup_virt_queues(
        &[
            &queue_data[..QUEUE_DATA_SIZE].try_into().unwrap(),
            &queue_data[QUEUE_DATA_SIZE..QUEUE_DATA_SIZE * 2]
                .try_into()
                .unwrap(),
            &queue_data[QUEUE_DATA_SIZE * 2..QUEUE_DATA_SIZE * 3]
                .try_into()
                .unwrap(),
        ],
        0,
    );

    let inflate_q = queues.remove(0);
    let inflate_evt = EventFd::new(0).unwrap();
    let inflate_queue_evt = unsafe { EventFd::from_raw_fd(libc::dup(inflate_evt.as_raw_fd())) };
    let deflate_q = queues.remove(0);
    let deflate_evt = EventFd::new(0).unwrap();
    let deflate_queue_evt = unsafe { EventFd::from_raw_fd(libc::dup(deflate_evt.as_raw_fd())) };
    let reporting_q = queues.remove(0);
    let reporting_evt = EventFd::new(0).unwrap();
    let reporting_queue_evt = unsafe { EventFd::from_raw_fd(libc::dup(reporting_evt.as_raw_fd())) };

    // Kick the 'queue' events before activate the balloon device
    inflate_queue_evt.write(1).unwrap();
    deflate_queue_evt.write(1).unwrap();
    reporting_queue_evt.write(1).unwrap();

    balloon
        .activate(
            guest_memory,
            Arc::new(NoopVirtioInterrupt {}),
            vec![
                (0, inflate_q, inflate_evt),
                (1, deflate_q, deflate_evt),
                (2, reporting_q, reporting_evt),
            ],
        )
        .ok();

    // Wait for the events to finish and balloon device worker thread to return
    balloon.wait_for_epoll_threads();
});

pub struct NoopVirtioInterrupt {}

impl VirtioInterrupt for NoopVirtioInterrupt {
    fn trigger(&self, _int_type: VirtioInterruptType) -> std::result::Result<(), std::io::Error> {
        Ok(())
    }
}

macro_rules! align {
    ($n:expr, $align:expr) => {{
        (($n + $align - 1) / $align) * $align
    }};
}

fn setup_virt_queues(bytes: &[&[u8; QUEUE_DATA_SIZE]], base_addr: u64) -> Vec<Queue> {
    let mut queues = Vec::new();
    let mut base_addr = base_addr;
    for b in bytes {
        let mut q = Queue::new(QUEUE_SIZE).unwrap();

        let desc_table_addr = align!(base_addr, DESC_TABLE_ALIGN_SIZE);
        let avail_ring_addr = align!(desc_table_addr + DESC_TABLE_SIZE, AVAIL_RING_ALIGN_SIZE);
        let used_ring_addr = align!(avail_ring_addr + AVAIL_RING_SIZE, USED_RING_ALIGN_SIZE);
        q.try_set_desc_table_address(GuestAddress(desc_table_addr))
            .unwrap();
        q.try_set_avail_ring_address(GuestAddress(avail_ring_addr))
            .unwrap();
        q.try_set_used_ring_address(GuestAddress(used_ring_addr))
            .unwrap();

        q.set_next_avail(b[0] as u16); // 'u8' is enough given the 'QUEUE_SIZE' is small
        q.set_next_used(b[1] as u16);
        q.set_event_idx(b[2] % 2 != 0);
        q.set_size(b[3] as u16 % QUEUE_SIZE);

        q.set_ready(true);
        queues.push(q);

        base_addr = used_ring_addr + USED_RING_SIZE;
    }

    queues
}

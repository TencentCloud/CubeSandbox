// Copyright © 2022 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

#![no_main]

use libfuzzer_sys::fuzz_target;
use seccompiler::SeccompAction;
use std::os::unix::io::{AsRawFd, FromRawFd};
use std::sync::{Arc, Mutex};
use virtio_devices::{BlocksState, Mem, VirtioDevice, VirtioInterrupt, VirtioInterruptType};
use virtio_queue::{Queue, QueueT};
use vm_memory::{bitmap::AtomicBitmap, Bytes, GuestAddress, GuestMemoryAtomic};
use vmm_sys_util::eventfd::{EventFd, EFD_NONBLOCK};

type GuestMemoryMmap = vm_memory::GuestMemoryMmap<AtomicBitmap>;
type GuestRegionMmap = vm_memory::GuestRegionMmap<AtomicBitmap>;

macro_rules! align {
    ($n:expr, $align:expr) => {{
        (($n + $align - 1) / $align) * $align
    }};
}

const VIRTIO_MEM_DATA_SIZE: usize = 1;
const QUEUE_DATA_SIZE: usize = 4;
// The size of the guest memory for the virtio-mem region
const MEM_SIZE: usize = 128 * 1024 * 1024;
// The start address of the virtio-mem region in the guest memory
const VIRTIO_MEM_REGION_ADDRESS: u64 = 0;

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

// Guest memory gap
const GUEST_MEM_GAP: u64 = 1 * 1024 * 1024;
// Guest physical address for descriptor table.
const DESC_TABLE_ADDR: u64 = align!(MEM_SIZE as u64 + GUEST_MEM_GAP, DESC_TABLE_ALIGN_SIZE);
// Guest physical address for available ring
const AVAIL_RING_ADDR: u64 = align!(DESC_TABLE_ADDR + DESC_TABLE_SIZE, AVAIL_RING_ALIGN_SIZE);
// Guest physical address for used ring
const USED_RING_ADDR: u64 = align!(AVAIL_RING_ADDR + AVAIL_RING_SIZE, USED_RING_ALIGN_SIZE);
// Virtio-queue size in bytes
const QUEUE_BYTES_SIZE: usize = (USED_RING_ADDR + USED_RING_SIZE - DESC_TABLE_ADDR) as usize;

fuzz_target!(|bytes| {
    if bytes.len() < VIRTIO_MEM_DATA_SIZE + QUEUE_DATA_SIZE + QUEUE_BYTES_SIZE
        || bytes.len() > (VIRTIO_MEM_DATA_SIZE + QUEUE_DATA_SIZE + QUEUE_BYTES_SIZE + MEM_SIZE)
    {
        return;
    }

    let virtio_mem_data = &bytes[..VIRTIO_MEM_DATA_SIZE];
    let queue_data = &bytes[VIRTIO_MEM_DATA_SIZE..VIRTIO_MEM_DATA_SIZE + QUEUE_DATA_SIZE];
    let queue_bytes = &bytes[VIRTIO_MEM_DATA_SIZE + QUEUE_DATA_SIZE
        ..VIRTIO_MEM_DATA_SIZE + QUEUE_DATA_SIZE + QUEUE_BYTES_SIZE];
    let mem_bytes = &bytes[VIRTIO_MEM_DATA_SIZE + QUEUE_DATA_SIZE + QUEUE_BYTES_SIZE..];

    // Create a virtio-mem device based on the input bytes;
    let (mut virtio_mem, virtio_mem_region) =
        create_dummy_virtio_mem(virtio_mem_data.try_into().unwrap());

    // Setup the virt queue with the input bytes
    let q = setup_virt_queue(queue_data.try_into().unwrap());

    // Setup the guest memory with the input bytes
    let mem = GuestMemoryMmap::from_ranges(&[
        (GuestAddress(DESC_TABLE_ADDR), QUEUE_BYTES_SIZE), // guest region for the virt queue
    ])
    .unwrap();
    if mem
        .write_slice(queue_bytes, GuestAddress(DESC_TABLE_ADDR))
        .is_err()
    {
        return;
    }
    // Add the memory region for the virtio-mem device
    let mem = mem.insert_region(virtio_mem_region).unwrap();
    if mem
        .write_slice(mem_bytes, GuestAddress(VIRTIO_MEM_REGION_ADDRESS))
        .is_err()
    {
        return;
    }
    let guest_memory = GuestMemoryAtomic::new(mem);

    let evt = EventFd::new(0).unwrap();
    let queue_evt = unsafe { EventFd::from_raw_fd(libc::dup(evt.as_raw_fd())) };

    // Kick the 'queue' event before activate the virtio-mem device
    queue_evt.write(1).unwrap();

    virtio_mem
        .activate(
            guest_memory,
            Arc::new(NoopVirtioInterrupt {}),
            vec![(0, q, evt)],
        )
        .ok();

    // Wait for the events to finish and virtio-mem device worker thread to return
    virtio_mem.wait_for_epoll_threads();
});

pub struct NoopVirtioInterrupt {}

impl VirtioInterrupt for NoopVirtioInterrupt {
    fn trigger(&self, _int_type: VirtioInterruptType) -> std::result::Result<(), std::io::Error> {
        Ok(())
    }
}

// Create a dummy virtio-mem device for fuzzing purpose only
fn create_dummy_virtio_mem(bytes: &[u8; VIRTIO_MEM_DATA_SIZE]) -> (Mem, Arc<GuestRegionMmap>) {
    let numa_id = if bytes[0] % 2 != 0 { Some(0) } else { None };

    let region = vmm::memory_manager::MemoryManager::create_ram_region(
        &None,
        0,
        GuestAddress(VIRTIO_MEM_REGION_ADDRESS),
        MEM_SIZE,
        false,
        false,
        false,
        None,
        numa_id,
        None,
        false,
    )
    .unwrap();

    let blocks_state = Arc::new(Mutex::new(BlocksState::new(region.size() as u64)));

    (
        Mem::new(
            "fuzzer_mem".to_owned(),
            &region,
            SeccompAction::Allow,
            numa_id.map(|i| i as u16),
            0,
            false,
            EventFd::new(EFD_NONBLOCK).unwrap(),
            blocks_state.clone(),
            None,
        )
        .unwrap(),
        region,
    )
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

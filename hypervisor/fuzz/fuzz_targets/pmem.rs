// Copyright © 2022 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

#![no_main]

use libc::{MAP_NORESERVE, MAP_PRIVATE, PROT_READ, PROT_WRITE};
use libfuzzer_sys::fuzz_target;
use seccompiler::SeccompAction;
use std::ffi;
use std::fs::File;
use std::io;
use std::os::unix::io::{AsRawFd, FromRawFd, RawFd};
use std::sync::Arc;
use virtio_devices::{Pmem, UserspaceMapping, VirtioDevice, VirtioInterrupt, VirtioInterruptType};
use virtio_queue::{Queue, QueueT};
use vm_memory::guest_memory::FileOffset;
use vm_memory::{bitmap::AtomicBitmap, Bytes, GuestAddress, GuestMemoryAtomic, MmapRegion};
use vmm_sys_util::eventfd::{EventFd, EFD_NONBLOCK};

type GuestMemoryMmap = vm_memory::GuestMemoryMmap<AtomicBitmap>;

const QUEUE_DATA_SIZE: usize = 4;
const MEM_SIZE: usize = 256 * 1024 * 1024;
const PMEM_FILE_SIZE: usize = 128 * 1024 * 1024;
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

    let mut pmem = create_dummy_pmem();
    let queue_data = &bytes[..QUEUE_DATA_SIZE];
    let mem_bytes = &bytes[QUEUE_DATA_SIZE..];

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

    // Kick the 'queue' event before activate the pmem device
    queue_evt.write(1).unwrap();

    pmem.activate(
        guest_memory,
        Arc::new(NoopVirtioInterrupt {}),
        vec![(0, q, evt)],
    )
    .ok();

    // Wait for the events to finish and pmem device worker thread to return
    pmem.wait_for_epoll_threads();
});

fn memfd_create_with_size(name: &ffi::CStr, flags: u32, size: usize) -> Result<RawFd, io::Error> {
    let fd = unsafe { libc::syscall(libc::SYS_memfd_create, name.as_ptr(), flags) };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }

    let res = unsafe { libc::syscall(libc::SYS_ftruncate, fd, size) };
    if res < 0 {
        return Err(io::Error::last_os_error());
    }

    Ok(fd as RawFd)
}

pub struct NoopVirtioInterrupt {}

impl VirtioInterrupt for NoopVirtioInterrupt {
    fn trigger(&self, _int_type: VirtioInterruptType) -> std::result::Result<(), std::io::Error> {
        Ok(())
    }
}

// Create a dummy virtio-pmem device for fuzzing purpose only
fn create_dummy_pmem() -> Pmem {
    let shm =
        memfd_create_with_size(&ffi::CString::new("fuzz").unwrap(), 0, PMEM_FILE_SIZE).unwrap();
    let file: File = unsafe { File::from_raw_fd(shm) };

    // The fuzzer is focusing on the virtio-pmem code that processes guest inputs (e.g. virt queues),
    // so dummy mappings (both mmap and user mapping) does the job and is faster (using smaller amount of memory).
    let dummy_mapping_size = 1;
    let cloned_file = file.try_clone().unwrap();
    let dummy_mmap_region = MmapRegion::build(
        Some(FileOffset::new(cloned_file, 0)),
        dummy_mapping_size,
        PROT_READ | PROT_WRITE,
        MAP_NORESERVE | MAP_PRIVATE,
    )
    .unwrap();
    let guest_addr = GuestAddress(0);
    let dummy_user_mapping = UserspaceMapping {
        host_addr: dummy_mmap_region.as_ptr() as u64,
        mem_slot: 0,
        addr: guest_addr,
        len: dummy_mapping_size as u64,
        mergeable: false,
    };

    Pmem::new(
        "tmp".to_owned(),
        file,
        guest_addr,
        dummy_user_mapping,
        dummy_mmap_region,
        false,
        SeccompAction::Allow,
        EventFd::new(EFD_NONBLOCK).unwrap(),
        None,
    )
    .unwrap()
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

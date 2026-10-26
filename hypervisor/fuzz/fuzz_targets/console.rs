// Copyright © 2022 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

#![no_main]

use libfuzzer_sys::fuzz_target;
use seccompiler::SeccompAction;
use std::fs::File;
use std::io::Write;
use std::os::unix::io::{AsRawFd, FromRawFd, RawFd};
use std::sync::Arc;
use virtio_devices::{VirtioDevice, VirtioInterrupt, VirtioInterruptType};
use virtio_queue::{Queue, QueueT};
use vm_memory::{bitmap::AtomicBitmap, Bytes, GuestAddress, GuestMemoryAtomic};
use vmm_sys_util::eventfd::{EventFd, EFD_NONBLOCK};

type GuestMemoryMmap = vm_memory::GuestMemoryMmap<AtomicBitmap>;

macro_rules! align {
    ($n:expr, $align:expr) => {{
        (($n + $align - 1) / $align) * $align
    }};
}

const CONSOLE_INPUT_SIZE: usize = 128;
const QUEUE_DATA_SIZE: usize = 4;
const MEM_SIZE: usize = 32 * 1024 * 1024;
// Guest memory gap
const GUEST_MEM_GAP: u64 = 1 * 1024 * 1024;
// Guest physical address for the first virt queue
const BASE_VIRT_QUEUE_ADDR: u64 = MEM_SIZE as u64 + GUEST_MEM_GAP;
// Number of queues
const QUEUE_NUM: usize = 2;
// Max entries in the queue.
const QUEUE_SIZE: u16 = 256;
// Descriptor table alignment
const DESC_TABLE_ALIGN_SIZE: u64 = 16;
// Used ring alignment
const USED_RING_ALIGN_SIZE: u64 = 4;
// Descriptor table size
const DESC_TABLE_SIZE: u64 = 16_u64 * QUEUE_SIZE as u64;
// Available ring size
const AVAIL_RING_SIZE: u64 = 6_u64 + 2 * QUEUE_SIZE as u64;
// Padding size before used ring
const PADDING_SIZE: u64 = align!(AVAIL_RING_SIZE, USED_RING_ALIGN_SIZE) - AVAIL_RING_SIZE;
// Used ring size
const USED_RING_SIZE: u64 = 6_u64 + 8 * QUEUE_SIZE as u64;
// Virtio-queue size in bytes
const QUEUE_BYTES_SIZE: usize = align!(
    DESC_TABLE_SIZE + AVAIL_RING_SIZE + PADDING_SIZE + USED_RING_SIZE,
    DESC_TABLE_ALIGN_SIZE
) as usize;

fuzz_target!(|bytes| {
    if bytes.len() < (QUEUE_DATA_SIZE + QUEUE_BYTES_SIZE) * QUEUE_NUM + CONSOLE_INPUT_SIZE
        || bytes.len()
            > (QUEUE_DATA_SIZE + QUEUE_BYTES_SIZE) * QUEUE_NUM + CONSOLE_INPUT_SIZE + MEM_SIZE
    {
        return;
    }

    let (pipe_rx, mut pipe_tx) = create_pipe().unwrap();
    let output = unsafe {
        File::from_raw_fd(
            memfd_create(&std::ffi::CString::new("fuzz_console_output").unwrap()).unwrap(),
        )
    };
    let endpoint = virtio_devices::Endpoint::FilePair(output, pipe_rx);

    let (mut console, _) = virtio_devices::Console::new(
        "fuzzer_console".to_owned(),
        endpoint,
        None,  // resize_pipe
        false, // iommu
        SeccompAction::Allow,
        EventFd::new(EFD_NONBLOCK).unwrap(),
        None,
    )
    .unwrap();

    let console_input_bytes = &bytes[..CONSOLE_INPUT_SIZE];
    let queue_data = &bytes[CONSOLE_INPUT_SIZE..CONSOLE_INPUT_SIZE + QUEUE_DATA_SIZE * QUEUE_NUM];
    let queue_bytes = &bytes[CONSOLE_INPUT_SIZE + QUEUE_DATA_SIZE * QUEUE_NUM
        ..CONSOLE_INPUT_SIZE + (QUEUE_DATA_SIZE + QUEUE_BYTES_SIZE) * QUEUE_NUM];
    let mem_bytes = &bytes[CONSOLE_INPUT_SIZE + (QUEUE_DATA_SIZE + QUEUE_BYTES_SIZE) * QUEUE_NUM..];

    // Setup the virt queues with the input bytes
    let mut queues = setup_virt_queues(
        &[
            &queue_data[..QUEUE_DATA_SIZE].try_into().unwrap(),
            &queue_data[QUEUE_DATA_SIZE..QUEUE_DATA_SIZE * 2]
                .try_into()
                .unwrap(),
        ],
        BASE_VIRT_QUEUE_ADDR,
    );

    // Setup the guest memory with the input bytes
    let mem = GuestMemoryMmap::from_ranges(&[
        (GuestAddress(0), MEM_SIZE),
        (GuestAddress(BASE_VIRT_QUEUE_ADDR), queue_bytes.len()),
    ])
    .unwrap();
    if mem
        .write_slice(queue_bytes, GuestAddress(BASE_VIRT_QUEUE_ADDR))
        .is_err()
    {
        return;
    }
    if mem.write_slice(mem_bytes, GuestAddress(0 as u64)).is_err() {
        return;
    }
    let guest_memory = GuestMemoryAtomic::new(mem);

    let input_queue = queues.remove(0);
    let input_evt = EventFd::new(0).unwrap();
    let input_queue_evt = unsafe { EventFd::from_raw_fd(libc::dup(input_evt.as_raw_fd())) };
    let output_queue = queues.remove(0);
    let output_evt = EventFd::new(0).unwrap();
    let output_queue_evt = unsafe { EventFd::from_raw_fd(libc::dup(output_evt.as_raw_fd())) };

    // Kick the 'queue' events and endpoint event before activate the console device
    input_queue_evt.write(1).unwrap();
    output_queue_evt.write(1).unwrap();
    pipe_tx.write_all(console_input_bytes).unwrap(); // To use fuzzed data;

    console
        .activate(
            guest_memory,
            Arc::new(NoopVirtioInterrupt {}),
            vec![(0, input_queue, input_evt), (1, output_queue, output_evt)],
        )
        .unwrap();

    // Wait for the events to finish and console device worker thread to return
    console.wait_for_epoll_threads();
});

pub struct NoopVirtioInterrupt {}

impl VirtioInterrupt for NoopVirtioInterrupt {
    fn trigger(&self, _int_type: VirtioInterruptType) -> std::result::Result<(), std::io::Error> {
        Ok(())
    }
}

fn setup_virt_queues(bytes: &[&[u8; QUEUE_DATA_SIZE]], base_addr: u64) -> Vec<Queue> {
    let mut queues = Vec::new();
    for (i, b) in bytes.iter().enumerate() {
        let mut q = Queue::new(QUEUE_SIZE).unwrap();

        let desc_table_addr = base_addr + (QUEUE_BYTES_SIZE * i) as u64;
        let avail_ring_addr = desc_table_addr + DESC_TABLE_SIZE;
        let used_ring_addr = avail_ring_addr + PADDING_SIZE + AVAIL_RING_SIZE;
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
    }

    queues
}

fn memfd_create(name: &std::ffi::CStr) -> Result<RawFd, std::io::Error> {
    let res = unsafe { libc::syscall(libc::SYS_memfd_create, name.as_ptr(), 0) };

    if res < 0 {
        Err(std::io::Error::last_os_error())
    } else {
        Ok(res as RawFd)
    }
}

fn create_pipe() -> Result<(File, File), std::io::Error> {
    let mut pipe = [-1; 2];
    if unsafe { libc::pipe2(pipe.as_mut_ptr(), libc::O_CLOEXEC) } == -1 {
        return Err(std::io::Error::last_os_error());
    }
    let rx = unsafe { File::from_raw_fd(pipe[0]) };
    let tx = unsafe { File::from_raw_fd(pipe[1]) };

    Ok((rx, tx))
}

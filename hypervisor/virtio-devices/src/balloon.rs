// Copyright (c) 2020 Ant Financial
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

use crate::{
    seccomp_filters::Thread, thread_helper::spawn_virtio_thread, ActivateResult, EpollHelper,
    EpollHelperError, EpollHelperHandler, GuestMemoryMmap, VirtioCommon, VirtioDevice,
    VirtioDeviceType, VirtioInterrupt, VirtioInterruptType, EPOLL_HELPER_EVENT_LAST,
    VIRTIO_F_VERSION_1,
};
use anyhow::anyhow;
use seccompiler::SeccompAction;
use serde::{Deserialize, Serialize};
use std::io::{self, Write};
use std::mem::size_of;
use std::os::unix::io::AsRawFd;
use std::result;
use std::sync::{atomic::AtomicBool, Arc, Barrier};
use thiserror::Error;
use virtio_queue::{Queue, QueueT};
use vm_memory::{
    Address, ByteValued, Bytes, GuestAddress, GuestAddressSpace, GuestMemory, GuestMemoryAtomic,
    GuestMemoryError, GuestMemoryRegion,
};
use vm_migration::{Migratable, MigratableError, Pausable, Snapshot, Snapshottable, Transportable};
use vm_virtio::checked_descriptor::DescriptorChainExt;
use vmm_sys_util::eventfd::EventFd;

const QUEUE_SIZE: u16 = 128;
const REPORTING_QUEUE_SIZE: u16 = 32;
const MIN_NUM_QUEUES: usize = 2;

// Inflate virtio queue event.
const INFLATE_QUEUE_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 1;
// Deflate virtio queue event.
const DEFLATE_QUEUE_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 2;
// Reporting virtio queue event.
const REPORTING_QUEUE_EVENT: u16 = EPOLL_HELPER_EVENT_LAST + 3;

// Size of a PFN in the balloon interface.
const VIRTIO_BALLOON_PFN_SHIFT: u64 = 12;

// Upper bound on a single inflate or deflate descriptor length, in
// bytes. Matches the Linux driver, which submits at most
// VIRTIO_BALLOON_ARRAY_PFNS_MAX of 256 PFN entries of 4 bytes each per
// descriptor.
const VIRTIO_BALLOON_MAX_PFN_BYTES: u32 = 256 * 4;

// Deflate balloon on OOM
const VIRTIO_BALLOON_F_DEFLATE_ON_OOM: u64 = 2;
// Enable an additional virtqueue to let the guest notify the host about free
// pages.
const VIRTIO_BALLOON_F_REPORTING: u64 = 5;

#[derive(Error, Debug)]
pub enum Error {
    #[error("Guest gave us bad memory addresses.: {0}")]
    GuestMemory(GuestMemoryError),
    #[error("Fallocate fail.: {0}")]
    FallocateFail(std::io::Error),
    #[error("Madvise fail.: {0}")]
    MadviseFail(std::io::Error),
    #[error("Failed to EventFd write.: {0}")]
    EventFdWriteFail(std::io::Error),
    #[error("Invalid queue index: {0}")]
    InvalidQueueIndex(usize),
    #[error("Fail tp signal: {0}")]
    FailedSignal(io::Error),
    #[error("Failed adding used index: {0}")]
    QueueAddUsed(virtio_queue::Error),
    #[error("Failed creating an iterator over the queue: {0}")]
    QueueIterator(virtio_queue::Error),
}

// Got from include/uapi/linux/virtio_balloon.h
#[repr(C)]
#[derive(Copy, Clone, Debug, Default, Serialize, Deserialize)]
pub struct VirtioBalloonConfig {
    // Number of pages host wants Guest to give up.
    num_pages: u32,
    // Number of pages we've actually got in balloon.
    actual: u32,
}

const CONFIG_ACTUAL_OFFSET: u64 = 4;
const CONFIG_ACTUAL_SIZE: usize = 4;

// SAFETY: it only has data and has no implicit padding.
unsafe impl ByteValued for VirtioBalloonConfig {}

struct BalloonEpollHandler {
    mem: GuestMemoryAtomic<GuestMemoryMmap>,
    queues: Vec<Queue>,
    interrupt_cb: Arc<dyn VirtioInterrupt>,
    inflate_queue_evt: EventFd,
    deflate_queue_evt: EventFd,
    reporting_queue_evt: Option<EventFd>,
    kill_evt: EventFd,
    pause_evt: EventFd,
}

impl BalloonEpollHandler {
    fn signal(&self, int_type: VirtioInterruptType) -> result::Result<(), Error> {
        self.interrupt_cb.trigger(int_type).map_err(|e| {
            error!("Failed to signal used queue: {:?}", e);
            Error::FailedSignal(e)
        })
    }

    fn advise_memory_range(
        memory: &GuestMemoryMmap,
        range_base: GuestAddress,
        range_len: usize,
        advice: libc::c_int,
    ) -> result::Result<(), Error> {
        let hva = memory
            .get_host_address(range_base)
            .map_err(Error::GuestMemory)?;
        // Need unsafe to do syscall madvise
        let res =
            unsafe { libc::madvise(hva as *mut libc::c_void, range_len as libc::size_t, advice) };
        if res != 0 {
            return Err(Error::MadviseFail(io::Error::last_os_error()));
        }
        Ok(())
    }

    fn release_memory_range(
        memory: &GuestMemoryMmap,
        range_base: GuestAddress,
        range_len: usize,
    ) -> result::Result<(), Error> {
        let region = memory.find_region(range_base).ok_or(Error::GuestMemory(
            GuestMemoryError::InvalidGuestAddress(range_base),
        ))?;

        // No underflow possible because range_base was found in the region by `find_region`.
        let offset = range_base.0 - region.start_addr().0;
        let region_limit = region.len() - offset;
        let len = std::cmp::min(range_len as u64, region_limit);
        if len < range_len as u64 {
            warn!(
                "Clamping reported range at GPA 0x{:x} from {} to {} bytes \
                 to fit inside its memory region",
                range_base.0, range_len, len
            );
        }
        if len == 0 {
            return Ok(());
        }

        if let Some(f_off) = region.file_offset() {
            let res = unsafe {
                libc::fallocate64(
                    f_off.file().as_raw_fd(),
                    libc::FALLOC_FL_PUNCH_HOLE | libc::FALLOC_FL_KEEP_SIZE,
                    (offset + f_off.start()) as libc::off64_t,
                    len as libc::off64_t,
                )
            };

            if res != 0 {
                return Err(Error::FallocateFail(io::Error::last_os_error()));
            }
        }

        Self::advise_memory_range(memory, range_base, len as usize, libc::MADV_DONTNEED)
    }

    fn process_queue(&mut self, queue_index: usize) -> result::Result<(), Error> {
        let mut used_descs = false;
        while let Some(mut desc_chain) =
            self.queues[queue_index].pop_descriptor_chain(self.mem.memory())
        {
            let desc = match desc_chain.next_checked(None) {
                Ok(Some(desc)) => desc,
                Ok(None) => {
                    warn!("Skipping empty balloon descriptor chain");
                    self.queues[queue_index]
                        .add_used(desc_chain.memory(), desc_chain.head_index(), 0)
                        .map_err(Error::QueueAddUsed)?;
                    used_descs = true;
                    continue;
                }
                Err(addr) => {
                    warn!(
                        "Skipping balloon descriptor outside guest memory at 0x{:x}",
                        addr.0
                    );
                    self.queues[queue_index]
                        .add_used(desc_chain.memory(), desc_chain.head_index(), 0)
                        .map_err(Error::QueueAddUsed)?;
                    used_descs = true;
                    continue;
                }
            };

            let data_chunk_size = size_of::<u32>();

            if desc.is_write_only() {
                warn!("Skipping device-writable descriptor on inflate/deflate queue");
            } else if desc.len() as usize % data_chunk_size != 0 {
                warn!(
                    "Skipping descriptor with length {} not a multiple of {data_chunk_size}",
                    desc.len()
                );
            } else if desc.len() > VIRTIO_BALLOON_MAX_PFN_BYTES {
                warn!(
                    "Skipping descriptor with length {} exceeding cap {VIRTIO_BALLOON_MAX_PFN_BYTES}",
                    desc.len()
                );
            } else {
                let mut offset = 0u64;
                while offset < desc.len() as u64 {
                    let Some(addr) = desc.addr().checked_add(offset) else {
                        warn!("Address overflow in balloon descriptor");
                        break;
                    };
                    let pfn: u32 = match desc_chain.memory().read_obj(addr) {
                        Ok(value) => value,
                        Err(e) => {
                            warn!("Failed to read PFN from descriptor: {e}");
                            break;
                        }
                    };
                    offset += data_chunk_size as u64;

                    let range_base = GuestAddress((pfn as u64) << VIRTIO_BALLOON_PFN_SHIFT);
                    let range_len = 1 << VIRTIO_BALLOON_PFN_SHIFT;

                    match queue_index {
                        0 => {
                            if let Err(e) = Self::release_memory_range(
                                desc_chain.memory(),
                                range_base,
                                range_len,
                            ) {
                                warn!("Failed to release memory for PFN {pfn:#x}: {e}");
                            }
                        }
                        1 => {
                            if let Err(e) = Self::advise_memory_range(
                                desc_chain.memory(),
                                range_base,
                                range_len,
                                libc::MADV_WILLNEED,
                            ) {
                                warn!("Failed to advise memory for PFN {pfn:#x}: {e}");
                            }
                        }
                        _ => return Err(Error::InvalidQueueIndex(queue_index)),
                    }
                }
            }

            self.queues[queue_index]
                .add_used(desc_chain.memory(), desc_chain.head_index(), desc.len())
                .map_err(Error::QueueAddUsed)?;
            used_descs = true;
        }

        if used_descs {
            self.signal(VirtioInterruptType::Queue(queue_index as u16))
        } else {
            Ok(())
        }
    }

    fn process_reporting_queue(&mut self, queue_index: usize) -> result::Result<(), Error> {
        let mut used_descs = false;
        while let Some(mut desc_chain) =
            self.queues[queue_index].pop_descriptor_chain(self.mem.memory())
        {
            let mut descs_len: u32 = 0;
            let results: Vec<_> = desc_chain.checked_iter(None).collect();
            for result in results {
                let desc = match result {
                    Ok(desc) => desc,
                    Err(_) => break,
                };
                descs_len = descs_len.saturating_add(desc.len());
                if let Err(e) = Self::release_memory_range(
                    desc_chain.memory(),
                    desc.addr(),
                    desc.len() as usize,
                ) {
                    warn!("Failed to release reported memory range: {e}");
                }
            }

            self.queues[queue_index]
                .add_used(desc_chain.memory(), desc_chain.head_index(), descs_len)
                .map_err(Error::QueueAddUsed)?;
            used_descs = true;
        }

        if used_descs {
            self.signal(VirtioInterruptType::Queue(queue_index as u16))
        } else {
            Ok(())
        }
    }

    fn run(
        &mut self,
        paused: Arc<AtomicBool>,
        paused_sync: Arc<Barrier>,
    ) -> result::Result<(), EpollHelperError> {
        let mut helper = EpollHelper::new(&self.kill_evt, &self.pause_evt)?;
        helper.add_event(self.inflate_queue_evt.as_raw_fd(), INFLATE_QUEUE_EVENT)?;
        helper.add_event(self.deflate_queue_evt.as_raw_fd(), DEFLATE_QUEUE_EVENT)?;
        if let Some(reporting_queue_evt) = self.reporting_queue_evt.as_ref() {
            helper.add_event(reporting_queue_evt.as_raw_fd(), REPORTING_QUEUE_EVENT)?;
        }
        helper.run(paused, paused_sync, self)?;

        Ok(())
    }
}

impl EpollHelperHandler for BalloonEpollHandler {
    fn handle_event(
        &mut self,
        _helper: &mut EpollHelper,
        event: &epoll::Event,
    ) -> result::Result<(), EpollHelperError> {
        let ev_type = event.data as u16;
        match ev_type {
            INFLATE_QUEUE_EVENT => {
                self.inflate_queue_evt.read().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!(
                        "Failed to get inflate queue event: {:?}",
                        e
                    ))
                })?;
                self.process_queue(0).map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!(
                        "Failed to signal used inflate queue: {:?}",
                        e
                    ))
                })?;
            }
            DEFLATE_QUEUE_EVENT => {
                self.deflate_queue_evt.read().map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!(
                        "Failed to get deflate queue event: {:?}",
                        e
                    ))
                })?;
                self.process_queue(1).map_err(|e| {
                    EpollHelperError::HandleEvent(anyhow!(
                        "Failed to signal used deflate queue: {:?}",
                        e
                    ))
                })?;
            }
            REPORTING_QUEUE_EVENT => {
                if let Some(reporting_queue_evt) = self.reporting_queue_evt.as_ref() {
                    reporting_queue_evt.read().map_err(|e| {
                        EpollHelperError::HandleEvent(anyhow!(
                            "Failed to get reporting queue event: {:?}",
                            e
                        ))
                    })?;
                    self.process_reporting_queue(2).map_err(|e| {
                        EpollHelperError::HandleEvent(anyhow!(
                            "Failed to signal used inflate queue: {:?}",
                            e
                        ))
                    })?;
                } else {
                    return Err(EpollHelperError::HandleEvent(anyhow!(
                        "Invalid reporting queue event as no eventfd registered"
                    )));
                }
            }
            _ => {
                return Err(EpollHelperError::HandleEvent(anyhow!(
                    "Unknown event for virtio-balloon"
                )));
            }
        }

        Ok(())
    }
}

#[derive(Serialize, Deserialize)]
pub struct BalloonState {
    pub avail_features: u64,
    pub acked_features: u64,
    pub config: VirtioBalloonConfig,
}

// Virtio device for exposing entropy to the guest OS through virtio.
pub struct Balloon {
    common: VirtioCommon,
    id: String,
    config: VirtioBalloonConfig,
    seccomp_action: SeccompAction,
    exit_evt: EventFd,
    interrupt_cb: Option<Arc<dyn VirtioInterrupt>>,
}

impl Balloon {
    // Create a new virtio-balloon.
    pub fn new(
        id: String,
        size: u64,
        deflate_on_oom: bool,
        free_page_reporting: bool,
        seccomp_action: SeccompAction,
        exit_evt: EventFd,
        state: Option<BalloonState>,
    ) -> io::Result<Self> {
        let mut queue_sizes = vec![QUEUE_SIZE; MIN_NUM_QUEUES];

        let (avail_features, acked_features, config) = if let Some(state) = state {
            info!("Restoring virtio-balloon {}", id);
            (state.avail_features, state.acked_features, state.config)
        } else {
            let mut avail_features = 1u64 << VIRTIO_F_VERSION_1;
            if deflate_on_oom {
                avail_features |= 1u64 << VIRTIO_BALLOON_F_DEFLATE_ON_OOM;
            }
            if free_page_reporting {
                avail_features |= 1u64 << VIRTIO_BALLOON_F_REPORTING;
            }

            let config = VirtioBalloonConfig {
                num_pages: (size >> VIRTIO_BALLOON_PFN_SHIFT) as u32,
                ..Default::default()
            };

            (avail_features, 0, config)
        };

        if free_page_reporting {
            queue_sizes.push(REPORTING_QUEUE_SIZE);
        }

        Ok(Balloon {
            common: VirtioCommon {
                device_type: VirtioDeviceType::Balloon as u32,
                avail_features,
                acked_features,
                paused_sync: Some(Arc::new(Barrier::new(2))),
                queue_sizes,
                min_queues: MIN_NUM_QUEUES as u16,
                ..Default::default()
            },
            id,
            config,
            seccomp_action,
            exit_evt,
            interrupt_cb: None,
        })
    }

    pub fn resize(&mut self, size: u64) -> Result<(), Error> {
        self.config.num_pages = (size >> VIRTIO_BALLOON_PFN_SHIFT) as u32;

        if let Some(interrupt_cb) = &self.interrupt_cb {
            interrupt_cb
                .trigger(VirtioInterruptType::Config)
                .map_err(Error::FailedSignal)
        } else {
            Ok(())
        }
    }

    // Get the actual size of the virtio-balloon.
    pub fn get_actual(&self) -> u64 {
        (self.config.actual as u64) << VIRTIO_BALLOON_PFN_SHIFT
    }

    fn state(&self) -> BalloonState {
        BalloonState {
            avail_features: self.common.avail_features,
            acked_features: self.common.acked_features,
            config: self.config,
        }
    }

    #[cfg(fuzzing)]
    pub fn wait_for_epoll_threads(&mut self) {
        self.common.wait_for_epoll_threads();
    }
}

impl Drop for Balloon {
    fn drop(&mut self) {
        if let Some(kill_evt) = self.common.kill_evt.take() {
            // Ignore the result because there is nothing we can do about it.
            let _ = kill_evt.write(1);
        }
    }
}

impl VirtioDevice for Balloon {
    fn device_type(&self) -> u32 {
        self.common.device_type
    }

    fn queue_max_sizes(&self) -> &[u16] {
        &self.common.queue_sizes
    }

    fn features(&self) -> u64 {
        self.common.avail_features
    }

    fn ack_features(&mut self, value: u64) {
        self.common.ack_features(value)
    }

    fn read_config(&self, offset: u64, data: &mut [u8]) {
        self.read_config_from_slice(self.config.as_slice(), offset, data);
    }

    fn write_config(&mut self, offset: u64, data: &[u8]) {
        // The "actual" field is the only mutable field
        if offset != CONFIG_ACTUAL_OFFSET || data.len() != CONFIG_ACTUAL_SIZE {
            error!(
                "Attempt to write to read-only field: offset {:x} length {}",
                offset,
                data.len()
            );
            return;
        }

        let config = self.config.as_mut_slice();
        let config_len = config.len() as u64;
        let data_len = data.len() as u64;
        if offset + data_len > config_len {
            error!(
                    "Out-of-bound access to configuration: config_len = {} offset = {:x} length = {} for {}",
                    config_len,
                    offset,
                    data_len,
                    self.device_type()
                );
            return;
        }

        if let Some(end) = offset.checked_add(config.len() as u64) {
            let mut offset_config =
                &mut config[offset as usize..std::cmp::min(end, config_len) as usize];
            offset_config.write_all(data).unwrap();
        }
    }

    fn activate(
        &mut self,
        mem: GuestMemoryAtomic<GuestMemoryMmap>,
        interrupt_cb: Arc<dyn VirtioInterrupt>,
        mut queues: Vec<(usize, Queue, EventFd)>,
    ) -> ActivateResult {
        self.common.activate(&queues, &interrupt_cb)?;
        let (kill_evt, pause_evt) = self.common.dup_eventfds();

        let mut virtqueues = Vec::new();
        let (_, queue, queue_evt) = queues.remove(0);
        virtqueues.push(queue);
        let inflate_queue_evt = queue_evt;
        let (_, queue, queue_evt) = queues.remove(0);
        virtqueues.push(queue);
        let deflate_queue_evt = queue_evt;
        let reporting_queue_evt =
            if self.common.feature_acked(VIRTIO_BALLOON_F_REPORTING) && !queues.is_empty() {
                let (_, queue, queue_evt) = queues.remove(0);
                virtqueues.push(queue);
                Some(queue_evt)
            } else {
                None
            };

        self.interrupt_cb = Some(interrupt_cb.clone());

        let mut handler = BalloonEpollHandler {
            mem,
            queues: virtqueues,
            interrupt_cb,
            inflate_queue_evt,
            deflate_queue_evt,
            reporting_queue_evt,
            kill_evt,
            pause_evt,
        };

        let paused = self.common.paused.clone();
        let paused_sync = self.common.paused_sync.clone();
        let mut epoll_threads = Vec::new();

        spawn_virtio_thread(
            &self.id,
            &self.seccomp_action,
            Thread::VirtioBalloon,
            &mut epoll_threads,
            &self.exit_evt,
            move || handler.run(paused, paused_sync.unwrap()),
        )?;
        self.common.epoll_threads = Some(epoll_threads);

        event!("virtio-device", "activated", "id", &self.id);
        Ok(())
    }

    fn reset(&mut self) -> Option<Arc<dyn VirtioInterrupt>> {
        let result = self.common.reset();
        event!("virtio-device", "reset", "id", &self.id);
        result
    }
}

impl Pausable for Balloon {
    fn pause(&mut self) -> result::Result<(), MigratableError> {
        self.common.pause()
    }

    fn resume(&mut self) -> result::Result<(), MigratableError> {
        self.common.resume()
    }
}

impl Snapshottable for Balloon {
    fn id(&self) -> String {
        self.id.clone()
    }

    fn snapshot(&mut self) -> std::result::Result<Snapshot, MigratableError> {
        Snapshot::new_from_state(&self.id(), &self.state())
    }
}
impl Transportable for Balloon {}
impl Migratable for Balloon {}

#[cfg(test)]
mod tests {
    use super::{
        Balloon, BalloonEpollHandler, BalloonState, VirtioBalloonConfig, QUEUE_SIZE,
        REPORTING_QUEUE_SIZE, VIRTIO_BALLOON_F_REPORTING, VIRTIO_BALLOON_MAX_PFN_BYTES,
        VIRTIO_BALLOON_PFN_SHIFT,
    };
    use crate::{
        GuestMemoryMmap, GuestRegionMmap, MmapRegion, VirtioInterrupt, VirtioInterruptType,
    };
    use std::fs::{self, File, OpenOptions};
    use std::io::Write;
    use std::mem::size_of;
    use std::sync::Arc;
    use std::time::{SystemTime, UNIX_EPOCH};
    use vm_memory::{Bytes, FileOffset, GuestAddress, GuestMemoryAtomic};
    use vm_virtio::queue::testing::VirtQueue as GuestQ;
    use vmm_sys_util::eventfd::EventFd;

    const PAGE_SIZE: usize = 4096;

    struct NoopVirtioInterrupt;

    impl VirtioInterrupt for NoopVirtioInterrupt {
        fn trigger(&self, _int_type: VirtioInterruptType) -> std::io::Result<()> {
            Ok(())
        }
    }

    fn temp_file(name: &str, contents: &[u8]) -> (std::path::PathBuf, File) {
        let nonce = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let path = std::env::temp_dir().join(format!("balloon-{name}-{nonce}"));
        let mut file = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(true)
            .open(&path)
            .unwrap();
        file.write_all(contents).unwrap();
        (path, file)
    }

    #[test]
    fn release_zero_length_range_is_a_noop() {
        let memory = GuestMemoryMmap::from_ranges(&[(GuestAddress(0), PAGE_SIZE)]).unwrap();

        BalloonEpollHandler::release_memory_range(&memory, GuestAddress(0), 0).unwrap();
    }

    #[test]
    fn release_range_is_clamped_to_its_memory_region() {
        let contents = vec![0x5a; PAGE_SIZE * 2];
        let (path, snapshot) = temp_file("region-boundary", &contents);
        let mmap = MmapRegion::build(
            Some(FileOffset::new(snapshot, 0)),
            PAGE_SIZE,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_PRIVATE,
        )
        .unwrap();
        let region = GuestRegionMmap::new(mmap, GuestAddress(0)).unwrap();
        let memory = GuestMemoryMmap::from_regions(vec![region]).unwrap();

        BalloonEpollHandler::release_memory_range(&memory, GuestAddress(0), PAGE_SIZE * 2).unwrap();

        let after = fs::read(&path).unwrap();
        assert_eq!(&after[..PAGE_SIZE], &vec![0; PAGE_SIZE]);
        assert_eq!(&after[PAGE_SIZE..], &contents[PAGE_SIZE..]);
        fs::remove_file(path).unwrap();
    }

    #[test]
    fn reporting_queue_continues_after_an_invalid_range() {
        const QUEUE_ADDRESS: GuestAddress = GuestAddress(0x1_0000);
        const VALID_RANGE: GuestAddress = GuestAddress(0x2_0000);
        const INVALID_RANGE: GuestAddress = GuestAddress(0x8_0000);

        let memory = GuestMemoryMmap::from_ranges(&[(GuestAddress(0), 0x4_0000)]).unwrap();
        memory.write_obj(0xa5_u8, VALID_RANGE).unwrap();
        let guest_queue = GuestQ::new(QUEUE_ADDRESS, &memory, 16);
        guest_queue.dtable[0].set(INVALID_RANGE.0, PAGE_SIZE as u32, 0, 0);
        guest_queue.dtable[1].set(VALID_RANGE.0, PAGE_SIZE as u32, 0, 0);
        guest_queue.avail.ring[0].set(0);
        guest_queue.avail.ring[1].set(1);
        guest_queue.avail.idx.set(2);

        let mut handler = BalloonEpollHandler {
            mem: GuestMemoryAtomic::new(memory.clone()),
            queues: vec![guest_queue.create_queue()],
            interrupt_cb: Arc::new(NoopVirtioInterrupt),
            inflate_queue_evt: EventFd::new(0).unwrap(),
            deflate_queue_evt: EventFd::new(0).unwrap(),
            reporting_queue_evt: None,
            kill_evt: EventFd::new(0).unwrap(),
            pause_evt: EventFd::new(0).unwrap(),
        };

        handler.process_reporting_queue(0).unwrap();

        assert_eq!(memory.read_obj::<u8>(VALID_RANGE).unwrap(), 0);
        assert_eq!(guest_queue.used.idx.get(), 2);
    }

    #[test]
    fn inflate_queue_skips_oversized_descriptor_and_continues() {
        const QUEUE_ADDRESS: GuestAddress = GuestAddress(0x1_0000);
        const OVERSIZED_PFN_LIST: GuestAddress = GuestAddress(0x2_0000);
        const VALID_PFN_LIST: GuestAddress = GuestAddress(0x2_1000);
        const PROTECTED_RANGE: GuestAddress = GuestAddress(0x3_0000);
        const VALID_RANGE: GuestAddress = GuestAddress(0x4_0000);

        let memory = GuestMemoryMmap::from_ranges(&[(GuestAddress(0), 0x5_0000)]).unwrap();
        memory.write_obj(0xa5_u8, PROTECTED_RANGE).unwrap();
        memory.write_obj(0xa5_u8, VALID_RANGE).unwrap();
        let protected_pfn = (PROTECTED_RANGE.0 >> VIRTIO_BALLOON_PFN_SHIFT) as u32;
        for offset in (0..=VIRTIO_BALLOON_MAX_PFN_BYTES).step_by(size_of::<u32>()) {
            memory
                .write_obj(
                    protected_pfn,
                    GuestAddress(OVERSIZED_PFN_LIST.0 + offset as u64),
                )
                .unwrap();
        }
        memory
            .write_obj(
                (VALID_RANGE.0 >> VIRTIO_BALLOON_PFN_SHIFT) as u32,
                VALID_PFN_LIST,
            )
            .unwrap();

        let guest_queue = GuestQ::new(QUEUE_ADDRESS, &memory, 16);
        guest_queue.dtable[0].set(
            OVERSIZED_PFN_LIST.0,
            VIRTIO_BALLOON_MAX_PFN_BYTES + size_of::<u32>() as u32,
            0,
            0,
        );
        guest_queue.dtable[1].set(VALID_PFN_LIST.0, size_of::<u32>() as u32, 0, 0);
        guest_queue.avail.ring[0].set(0);
        guest_queue.avail.ring[1].set(1);
        guest_queue.avail.idx.set(2);

        let mut handler = BalloonEpollHandler {
            mem: GuestMemoryAtomic::new(memory.clone()),
            queues: vec![guest_queue.create_queue()],
            interrupt_cb: Arc::new(NoopVirtioInterrupt),
            inflate_queue_evt: EventFd::new(0).unwrap(),
            deflate_queue_evt: EventFd::new(0).unwrap(),
            reporting_queue_evt: None,
            kill_evt: EventFd::new(0).unwrap(),
            pause_evt: EventFd::new(0).unwrap(),
        };

        handler.process_queue(0).unwrap();

        assert_eq!(memory.read_obj::<u8>(PROTECTED_RANGE).unwrap(), 0xa5);
        assert_eq!(memory.read_obj::<u8>(VALID_RANGE).unwrap(), 0);
        assert_eq!(guest_queue.used.idx.get(), 2);
    }
    #[test]
    fn inflate_queue_continues_after_an_invalid_pfn() {
        const QUEUE_ADDRESS: GuestAddress = GuestAddress(0x1_0000);
        const PFN_LIST: GuestAddress = GuestAddress(0x2_0000);
        const VALID_RANGE: GuestAddress = GuestAddress(0x3_0000);
        const INVALID_RANGE: GuestAddress = GuestAddress(0x8_0000);

        let memory = GuestMemoryMmap::from_ranges(&[(GuestAddress(0), 0x4_0000)]).unwrap();
        memory.write_obj(0xa5_u8, VALID_RANGE).unwrap();
        memory
            .write_obj(
                (INVALID_RANGE.0 >> VIRTIO_BALLOON_PFN_SHIFT) as u32,
                PFN_LIST,
            )
            .unwrap();
        memory
            .write_obj(
                (VALID_RANGE.0 >> VIRTIO_BALLOON_PFN_SHIFT) as u32,
                GuestAddress(PFN_LIST.0 + size_of::<u32>() as u64),
            )
            .unwrap();
        let guest_queue = GuestQ::new(QUEUE_ADDRESS, &memory, 16);
        guest_queue.dtable[0].set(PFN_LIST.0, (size_of::<u32>() * 2) as u32, 0, 0);
        guest_queue.avail.ring[0].set(0);
        guest_queue.avail.idx.set(1);

        let mut handler = BalloonEpollHandler {
            mem: GuestMemoryAtomic::new(memory.clone()),
            queues: vec![guest_queue.create_queue()],
            interrupt_cb: Arc::new(NoopVirtioInterrupt),
            inflate_queue_evt: EventFd::new(0).unwrap(),
            deflate_queue_evt: EventFd::new(0).unwrap(),
            reporting_queue_evt: None,
            kill_evt: EventFd::new(0).unwrap(),
            pause_evt: EventFd::new(0).unwrap(),
        };

        handler.process_queue(0).unwrap();

        assert_eq!(memory.read_obj::<u8>(VALID_RANGE).unwrap(), 0);
        assert_eq!(guest_queue.used.idx.get(), 1);
    }
}

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
    EpollHelperError, EpollHelperHandler, GuestMemoryMmap, GuestRegionMmap, VirtioCommon,
    VirtioDevice, VirtioDeviceType, VirtioInterrupt, VirtioInterruptType, EPOLL_HELPER_EVENT_LAST,
    VIRTIO_F_VERSION_1,
};
use anyhow::anyhow;
use seccompiler::SeccompAction;
use serde::{Deserialize, Serialize};
use std::io::{self, Write};
use std::mem::size_of;
use std::os::unix::fs::FileExt;
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
    pagemap_filter: Option<PagemapFilter>,
}

pub(crate) struct PagemapFilter {
    file: std::fs::File,
    host_page_size: usize,
}

impl PagemapFilter {
    /// Validates the pagemap interface by allocating a test page, touching it,
    /// and verifying that bit 63 (present) is actually set in /proc/self/pagemap.
    /// If the kernel or procfs does not expose residency flags, this self-check
    /// fails, allowing activation to fall back safely to normal reclaim.
    fn verify_pagemap_support(file: &std::fs::File, host_page_size: usize) -> bool {
        let addr = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                host_page_size,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
                -1,
                0,
            )
        };
        if addr == libc::MAP_FAILED {
            return false;
        }

        // Write to touch the page, guaranteeing physical residency.
        unsafe {
            std::ptr::write_volatile(addr as *mut u8, 0x5a);
        }

        let pfn = (addr as u64) / (host_page_size as u64);
        let offset = pfn * (PAGEMAP_ENTRY_SIZE as u64);
        let mut entry_buf = [0u8; PAGEMAP_ENTRY_SIZE];
        let read_res = file.read_exact_at(&mut entry_buf, offset);

        // Free scratch memory immediately.
        unsafe {
            libc::munmap(addr, host_page_size);
        }

        if read_res.is_err() {
            return false;
        }

        let entry = u64::from_ne_bytes(entry_buf);
        // Bit 63 must be 1 for a resident page.
        (entry & (1u64 << 63)) != 0
    }
}

// /proc/self/pagemap 64-bit entry layout constants.
// Cross-reference: `hypervisor/vmm/src/pagemap_anon.rs` and `hypervisor/vmm/src/soft_dirty.rs`.
const PAGEMAP_ENTRY_SIZE: usize = 8;
#[cfg(test)]
const PAGEMAP_PRESENT: u64 = 1 << 63;
#[cfg(test)]
const PAGEMAP_SWAPPED: u64 = 1 << 62;
const PAGEMAP_SOFT_DIRTY: u64 = 1 << 55;
const QUICK_PROBE_PAGES: usize = 4;
/// Maximum number of host pages scanned by the residency filter per descriptor.
/// Descriptors exceeding this bound (e.g. malformed or oversized ranges >= 2 GiB)
/// immediately fall back to unconditional reclaim to prevent unbounded I/O overhead.
const MAX_FILTER_SCAN_PAGES: usize = 524_288;

/// Checks whether a single 64-bit /proc/self/pagemap entry represents an ordinary,
/// unmapped non-present hole in a private mapping that is strictly safe to skip reclaiming.
///
/// Uses an explicit conservative whitelist:
/// - Entry is 0: Clean ordinary unmapped hole without physical page allocation.
///   (Note: in environments without swap or with swap disabled via `swapoff -a`, as is standard
///   in container runtimes, entry 0 unambiguously indicates unallocated host physical memory.)
/// - Entry is only PAGEMAP_SOFT_DIRTY (bit 55): When snapshot cycles write "4" to
///   `/proc/self/clear_refs` (`hypervisor/vmm/src/soft_dirty.rs`), the kernel marks VMAs
///   `VM_SOFTDIRTY`, after which `pagemap_pte_hole()` reports bit 55 for untouched holes.
///   Tolerating bit 55 ensures the skip filter remains effective across snapshots.
/// - Any other bit set (bit 63 present, bit 62 swap, bit 61 file/shared-anon, uffd-wp,
///   guard, exclusive, or any payload): MUST conservatively fall back to normal reclaim.
#[inline]
pub(crate) fn is_safe_to_skip_reclaim_entry(entry: u64) -> bool {
    (entry & !PAGEMAP_SOFT_DIRTY) == 0
}

/// Returns true if this memory region has purely mapping-local reclaim semantics
/// (i.e. discarding private CoW pages without backing file mutations or shared hole-punching),
/// allowing reclamation to be safely skipped when the entire range is proven non-resident.
///
/// NOTE: In the current Cube memory backend, `MAP_PRIVATE` mappings (both anonymous and
/// snapshot-backed file mmaps) use mapping-local pages. If the range has never been faulted
/// into this mapping (i.e., completely non-resident), skipping `MADV_DONTNEED` is a pure no-op.
/// Conversely, any shared mapping (`MAP_SHARED`) requires `fallocate(FALLOC_FL_PUNCH_HOLE)` to
/// mutate the backing file or memfd, so it must not be skipped. This check is scoped to the
/// current memory backend architecture.
#[inline]
pub(crate) fn can_skip_reclaim_when_nonresident(region: &GuestRegionMmap) -> bool {
    let flags = region.flags();
    let is_private = (flags & libc::MAP_PRIVATE) == libc::MAP_PRIVATE;
    let is_shared = (flags & libc::MAP_SHARED) == libc::MAP_SHARED;
    is_private && !is_shared
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

        // Never punch a MAP_PRIVATE backing file: it can be an immutable snapshot or a
        // base shared by multiple VMs. MADV_DONTNEED below discards this mapping's CoW pages.
        if region.flags() & libc::MAP_SHARED == libc::MAP_SHARED {
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
                    let error = io::Error::last_os_error();
                    warn!(
                        "Failed to punch shared backing for reported range at GPA 0x{:x}: {error}; \
                         falling back to MADV_DONTNEED",
                        range_base.0
                    );
                }
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

    pub(crate) fn host_page_size() -> Option<usize> {
        let size = unsafe { libc::sysconf(libc::_SC_PAGESIZE) };
        if size <= 0 {
            None
        } else {
            let size = size as usize;
            if (size & (size - 1)) != 0 {
                None
            } else {
                Some(size)
            }
        }
    }

    /// Computes the pagemap entry span (start page, page count, and byte seek offset)
    /// for a host virtual address range.
    ///
    /// Linux `madvise(MADV_DONTNEED)` operates strictly on whole host pages.
    /// In CubeSandbox, guest and host page sizes match (4 KiB on x86_64 and standard
    /// ARM64 deployments, as configured by `CONFIG_ARM64_4K_PAGES=y`).
    /// Reported ranges that are not host-page-aligned or not a multiple of host page size
    /// are rejected (return `None`), conservatively falling back to standard reclaim where
    /// the kernel protects adjacent memory.
    pub(crate) fn calculate_pagemap_span(
        hva: u64,
        len: usize,
        page_size: usize,
    ) -> Option<(u64, usize, u64)> {
        if page_size == 0 || (page_size & (page_size - 1)) != 0 || len == 0 {
            return None;
        }
        let page_size_u64 = page_size as u64;

        // Both HVA and range length must be host page aligned.
        if hva % page_size_u64 != 0 || (len as u64) % page_size_u64 != 0 {
            return None;
        }

        let start_page = hva.checked_div(page_size_u64)?;
        let num_pages = (len as u64).checked_div(page_size_u64)? as usize;
        if num_pages > MAX_FILTER_SCAN_PAGES {
            return None;
        }

        let file_offset = start_page.checked_mul(PAGEMAP_ENTRY_SIZE as u64)?;
        Some((start_page, num_pages, file_offset))
    }

    /// Checks if the given HVA range is confirmed non-resident via /proc/self/pagemap.
    ///
    /// NOTE on concurrency: This probe and subsequent descriptor processing are not atomic
    /// with respect to guest vCPUs. If a vCPU faults in a page inside this range between
    /// the pagemap read and skipping `MADV_DONTNEED`, that page remains resident. This is
    /// benign and conservative: it never discards live guest data or causes data corruption.
    /// If the guest subsequently re-allocates and frees that page, it will be re-reported
    /// in a future cycle; if the page stays untouched, host RSS simply remains higher for
    /// that range without impacting guest correctness.
    fn is_hva_range_non_resident(&self, hva: u64, len: usize) -> bool {
        let filter = match self.pagemap_filter.as_ref() {
            Some(f) => f,
            None => return false,
        };
        let (start_page, num_pages, file_offset) =
            match Self::calculate_pagemap_span(hva, len, filter.host_page_size) {
                Some(span) => span,
                None => return false,
            };

        // Step 1: Quick Probe (read first min(num_pages, QUICK_PROBE_PAGES) entries)
        let probe_count = std::cmp::min(num_pages, QUICK_PROBE_PAGES);
        let mut probe_buf = [0u8; QUICK_PROBE_PAGES * PAGEMAP_ENTRY_SIZE];
        let probe_bytes = probe_count * PAGEMAP_ENTRY_SIZE;

        if filter
            .file
            .read_exact_at(&mut probe_buf[..probe_bytes], file_offset)
            .is_err()
        {
            return false;
        }

        for chunk in probe_buf[..probe_bytes].chunks_exact(PAGEMAP_ENTRY_SIZE) {
            let entry = u64::from_ne_bytes(chunk.try_into().unwrap());
            if !is_safe_to_skip_reclaim_entry(entry) {
                return false;
            }
        }

        if num_pages <= QUICK_PROBE_PAGES {
            return true;
        }

        // Step 2: Full Scan for remaining pages
        let mut offset_pages = QUICK_PROBE_PAGES;
        let mut buf = [0u8; 512 * PAGEMAP_ENTRY_SIZE];
        while offset_pages < num_pages {
            let chunk_pages = std::cmp::min(num_pages - offset_pages, 512);
            let read_len = chunk_pages * PAGEMAP_ENTRY_SIZE;
            let chunk_file_offset = match start_page
                .checked_add(offset_pages as u64)
                .and_then(|p| p.checked_mul(PAGEMAP_ENTRY_SIZE as u64))
            {
                Some(off) => off,
                None => return false,
            };

            if filter
                .file
                .read_exact_at(&mut buf[..read_len], chunk_file_offset)
                .is_err()
            {
                return false;
            }

            for chunk in buf[..read_len].chunks_exact(PAGEMAP_ENTRY_SIZE) {
                let entry = u64::from_ne_bytes(chunk.try_into().unwrap());
                if !is_safe_to_skip_reclaim_entry(entry) {
                    return false;
                }
            }
            offset_pages += chunk_pages;
        }

        true
    }

    fn should_skip_reported_range(
        &self,
        memory: &GuestMemoryMmap,
        range_base: GuestAddress,
        range_len: usize,
    ) -> bool {
        let region = match memory.find_region(range_base) {
            Some(r) => r,
            None => return false,
        };

        if !can_skip_reclaim_when_nonresident(region) {
            return false;
        }

        let hva = match memory.get_host_address(range_base) {
            Ok(h) => h as u64,
            Err(_) => return false,
        };

        let offset = range_base.0 - region.start_addr().0;
        let region_limit = region.len() - offset;
        if range_len as u64 > region_limit {
            // Malformed/oversized descriptor: fall back to normal reclaim path
            // to log the clamping warning.
            return false;
        }
        let len = range_len;
        if len == 0 {
            return true;
        }

        self.is_hva_range_non_resident(hva, len)
    }

    fn process_reporting_queue(&mut self, queue_index: usize) -> result::Result<(), Error> {
        let mut used_descs = false;
        let mut skipped_descs = 0usize;
        let mut skipped_bytes = 0u64;
        let mut total_descs = 0usize;

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
                total_descs = total_descs.saturating_add(1);
                descs_len = descs_len.saturating_add(desc.len());
                if self.should_skip_reported_range(
                    desc_chain.memory(),
                    desc.addr(),
                    desc.len() as usize,
                ) {
                    skipped_descs = skipped_descs.saturating_add(1);
                    skipped_bytes = skipped_bytes.saturating_add(desc.len() as u64);
                    continue;
                }
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

        if skipped_descs > 0 {
            trace!(
                "virtio-balloon: skipped reclaim for {}/{} descriptors ({} bytes)",
                skipped_descs,
                total_descs,
                skipped_bytes
            );
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

        if avail_features & (1u64 << VIRTIO_BALLOON_F_REPORTING) != 0 {
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

        let pagemap_filter = if reporting_queue_evt.is_some() {
            match BalloonEpollHandler::host_page_size() {
                Some(host_page_size) => match std::fs::File::open("/proc/self/pagemap") {
                    Ok(file) => {
                        if PagemapFilter::verify_pagemap_support(&file, host_page_size) {
                            info!(
                                "{}: free page reporting residency filter enabled (host_page_size={})",
                                self.id, host_page_size
                            );
                            Some(PagemapFilter {
                                file,
                                host_page_size,
                            })
                        } else {
                            info!(
                                "{}: free page reporting residency filter disabled: pagemap self-check failed; falling back to normal reclaim",
                                self.id
                            );
                            None
                        }
                    }
                    Err(e) => {
                        info!(
                            "{}: free page reporting residency filter disabled: failed to open /proc/self/pagemap: {}; falling back to normal reclaim",
                            self.id, e
                        );
                        None
                    }
                },
                None => {
                    info!(
                        "{}: free page reporting residency filter disabled: failed to determine host page size; falling back to normal reclaim",
                        self.id
                    );
                    None
                }
            }
        } else {
            None
        };

        let mut handler = BalloonEpollHandler {
            mem,
            queues: virtqueues,
            interrupt_cb,
            inflate_queue_evt,
            deflate_queue_evt,
            reporting_queue_evt,
            kill_evt,
            pause_evt,
            pagemap_filter,
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
        Balloon, BalloonEpollHandler, BalloonState, VirtioBalloonConfig, PAGEMAP_ENTRY_SIZE,
        QUEUE_SIZE, QUICK_PROBE_PAGES, REPORTING_QUEUE_SIZE, VIRTIO_BALLOON_F_REPORTING,
        VIRTIO_BALLOON_MAX_PFN_BYTES, VIRTIO_BALLOON_PFN_SHIFT,
    };
    use crate::{
        GuestMemoryMmap, GuestRegionMmap, MmapRegion, VirtioInterrupt, VirtioInterruptType,
    };
    use seccompiler::SeccompAction;
    use std::fs::{self, File, OpenOptions};
    use std::io::Write;
    use std::mem::size_of;
    use std::sync::{Arc, Mutex};
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
            libc::MAP_SHARED,
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
    fn release_private_page_does_not_modify_writable_backing_file() {
        let (path, backing) = temp_file("writable-private", &vec![0x5a; PAGE_SIZE]);
        let mmap = MmapRegion::build(
            Some(FileOffset::new(backing, 0)),
            PAGE_SIZE,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_PRIVATE,
        )
        .unwrap();
        let region = GuestRegionMmap::new(mmap, GuestAddress(0)).unwrap();
        let memory = GuestMemoryMmap::from_regions(vec![region]).unwrap();

        memory.write_obj(0xa5_u8, GuestAddress(0)).unwrap();
        BalloonEpollHandler::release_memory_range(&memory, GuestAddress(0), PAGE_SIZE).unwrap();

        assert_eq!(memory.read_obj::<u8>(GuestAddress(0)).unwrap(), 0x5a);
        assert_eq!(fs::read(&path).unwrap(), vec![0x5a; PAGE_SIZE]);
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
            pagemap_filter: None,
        };

        handler.process_reporting_queue(0).unwrap();

        assert_eq!(memory.read_obj::<u8>(VALID_RANGE).unwrap(), 0);
        assert_eq!(guest_queue.used.idx.get(), 2);
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn reporting_queue_skips_untouched_descriptor_and_advances_used_ring() {
        let (page_size, pagemap_file) = match (
            BalloonEpollHandler::host_page_size(),
            std::fs::File::open("/proc/self/pagemap"),
        ) {
            (Some(ps), Ok(f)) => (ps, f),
            _ => {
                eprintln!("Skipping reporting_queue_skips_untouched_descriptor_and_advances_used_ring: pagemap unavailable");
                return;
            }
        };

        const QUEUE_ADDRESS: GuestAddress = GuestAddress(0x1_0000);
        let untouched_range = GuestAddress(0x10_0000); // 1 MiB offset
        let resident_range = GuestAddress(0x20_0000); // 2 MiB offset
        let total_size = 0x30_0000;

        let mmap = MmapRegion::build(
            None,
            total_size,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
        )
        .unwrap();
        let region = GuestRegionMmap::new(mmap, GuestAddress(0)).unwrap();
        let memory = GuestMemoryMmap::from_regions(vec![region]).unwrap();

        // Write only to resident_range, leaving untouched_range completely non-resident
        memory.write_obj(0x5a_u8, resident_range).unwrap();

        let guest_queue = GuestQ::new(QUEUE_ADDRESS, &memory, 16);
        // Descriptor 0: Untouched non-resident page (should be skipped by filter)
        guest_queue.dtable[0].set(untouched_range.0, page_size as u32, 0, 0);
        // Descriptor 1: Touched resident page (must NOT be skipped, must be reclaimed)
        guest_queue.dtable[1].set(resident_range.0, page_size as u32, 0, 0);
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
            pagemap_filter: Some(super::PagemapFilter {
                file: pagemap_file,
                host_page_size: page_size,
            }),
        };

        // Assert skip filter decision: untouched is skipped, resident is not skipped
        assert!(handler.should_skip_reported_range(&memory, untouched_range, page_size));
        assert!(!handler.should_skip_reported_range(&memory, resident_range, page_size));

        // Process reporting queue through the end-to-end virtio loop
        handler.process_reporting_queue(0).unwrap();

        // 1. Used ring index advanced over both descriptors (total 2 descriptors processed)
        assert_eq!(guest_queue.used.idx.get(), 2);
        // 2. Resident page was properly reclaimed by MADV_DONTNEED (re-zeroed)
        assert_eq!(memory.read_obj::<u8>(resident_range).unwrap(), 0);
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
            pagemap_filter: None,
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
            pagemap_filter: None,
        };

        handler.process_queue(0).unwrap();

        assert_eq!(memory.read_obj::<u8>(VALID_RANGE).unwrap(), 0);
        assert_eq!(guest_queue.used.idx.get(), 1);
    }

    #[test]
    fn restore_uses_saved_reporting_feature_for_queue_topology() {
        let reporting_feature = 1u64 << VIRTIO_BALLOON_F_REPORTING;
        let restored_with_reporting = Balloon::new(
            "balloon0".to_string(),
            0,
            false,
            false,
            SeccompAction::Allow,
            EventFd::new(0).unwrap(),
            Some(BalloonState {
                avail_features: reporting_feature,
                acked_features: reporting_feature,
                config: VirtioBalloonConfig::default(),
            }),
        )
        .unwrap();
        assert_eq!(
            restored_with_reporting.common.queue_sizes,
            vec![QUEUE_SIZE, QUEUE_SIZE, REPORTING_QUEUE_SIZE]
        );

        let restored_without_reporting = Balloon::new(
            "balloon0".to_string(),
            0,
            false,
            true,
            SeccompAction::Allow,
            EventFd::new(0).unwrap(),
            Some(BalloonState {
                avail_features: 0,
                acked_features: 0,
                config: VirtioBalloonConfig::default(),
            }),
        )
        .unwrap();
        assert_eq!(
            restored_without_reporting.common.queue_sizes,
            vec![QUEUE_SIZE, QUEUE_SIZE]
        );
    }

    #[test]
    fn release_private_page_with_read_only_backing_file() {
        let (path, snapshot) = temp_file("read-only", &vec![0x5a; PAGE_SIZE]);
        drop(snapshot);

        let snapshot = OpenOptions::new().read(true).open(&path).unwrap();
        let mmap = MmapRegion::build(
            Some(FileOffset::new(snapshot, 0)),
            PAGE_SIZE,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_PRIVATE,
        )
        .unwrap();
        let region = GuestRegionMmap::new(mmap, GuestAddress(0)).unwrap();
        let memory = GuestMemoryMmap::from_regions(vec![region]).unwrap();

        memory.write_obj(0xa5_u8, GuestAddress(0)).unwrap();
        BalloonEpollHandler::release_memory_range(&memory, GuestAddress(0), PAGE_SIZE).unwrap();

        assert_eq!(memory.read_obj::<u8>(GuestAddress(0)).unwrap(), 0x5a);
        assert_eq!(fs::read(&path).unwrap(), vec![0x5a; PAGE_SIZE]);
        fs::remove_file(path).unwrap();
    }

    #[test]
    fn test_smart_filter_skip_and_fallback_semantics() {
        use super::can_skip_reclaim_when_nonresident;

        let total_size = 512 * PAGE_SIZE; // 2 MiB
        let mmap = MmapRegion::build(
            None,
            total_size,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
        )
        .unwrap();
        let region = GuestRegionMmap::new(mmap, GuestAddress(0)).unwrap();
        assert!(can_skip_reclaim_when_nonresident(&region));

        let memory = GuestMemoryMmap::from_regions(vec![region]).unwrap();
        let (host_page_size, pagemap_file) = match (
            BalloonEpollHandler::host_page_size(),
            std::fs::File::open("/proc/self/pagemap"),
        ) {
            (Some(ps), Ok(file)) => (ps, file),
            _ => {
                eprintln!("Skipping test_smart_filter_skip_and_fallback_semantics: /proc/self/pagemap unavailable");
                return;
            }
        };

        let handler = BalloonEpollHandler {
            mem: GuestMemoryAtomic::new(memory.clone()),
            queues: vec![],
            interrupt_cb: Arc::new(NoopVirtioInterrupt),
            inflate_queue_evt: EventFd::new(0).unwrap(),
            deflate_queue_evt: EventFd::new(0).unwrap(),
            reporting_queue_evt: None,
            kill_evt: EventFd::new(0).unwrap(),
            pause_evt: EventFd::new(0).unwrap(),
            pagemap_filter: Some(super::PagemapFilter {
                file: pagemap_file,
                host_page_size,
            }),
        };

        // 1. Fresh unwritten range: 100% non-resident -> should skip!
        assert!(handler.should_skip_reported_range(&memory, GuestAddress(0), total_size));

        // 2. Touch first page: resident at beginning -> Quick Probe detects -> should NOT skip!
        memory.write_obj(0x42_u8, GuestAddress(0)).unwrap();
        assert!(!handler.should_skip_reported_range(&memory, GuestAddress(0), total_size));

        // 3. Clear first page with madvise
        BalloonEpollHandler::release_memory_range(&memory, GuestAddress(0), total_size).unwrap();

        // 4. Touch page beyond QUICK_PROBE_PAGES: Quick probe passes, Full Scan detects -> should NOT skip!
        let later_addr = GuestAddress(((QUICK_PROBE_PAGES + 1) * host_page_size) as u64);
        memory.write_obj(0x42_u8, later_addr).unwrap();
        assert!(!handler.should_skip_reported_range(&memory, GuestAddress(0), total_size));

        // 5. Shared mapping bypass: MAP_SHARED must NEVER be skipped by smart filter
        let shared_mmap = MmapRegion::build(
            None,
            total_size,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_SHARED | libc::MAP_ANONYMOUS,
        )
        .unwrap();
        let shared_region = GuestRegionMmap::new(shared_mmap, GuestAddress(0)).unwrap();
        assert!(!can_skip_reclaim_when_nonresident(&shared_region));
        let shared_mem = GuestMemoryMmap::from_regions(vec![shared_region]).unwrap();
        assert!(!handler.should_skip_reported_range(&shared_mem, GuestAddress(0), total_size));

        // 6. Snapshot-backed MAP_PRIVATE: eligible for smart filter
        let (snap_path, snap_file) = temp_file("filter-snap", &vec![0u8; total_size]);
        let snap_mmap = MmapRegion::build(
            Some(FileOffset::new(snap_file, 0)),
            total_size,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_PRIVATE,
        )
        .unwrap();
        let snap_region = GuestRegionMmap::new(snap_mmap, GuestAddress(0)).unwrap();
        assert!(can_skip_reclaim_when_nonresident(&snap_region));
        let snap_mem = GuestMemoryMmap::from_regions(vec![snap_region]).unwrap();
        assert!(handler.should_skip_reported_range(&snap_mem, GuestAddress(0), total_size));
        fs::remove_file(snap_path).unwrap();

        // 7. Malformed / oversized descriptor: range_len > region_limit -> must fall back (NOT skip)
        assert!(!handler.should_skip_reported_range(&memory, GuestAddress(0), total_size + 4096));

        // 8. Zero-length descriptor: short-circuit to true (no-op)
        assert!(handler.should_skip_reported_range(&memory, GuestAddress(0), 0));
    }

    #[test]
    fn test_calculate_pagemap_span_page_sizes() {
        // --- 4 KiB Host Page Size ---
        let page_size_4k = 4096usize;
        let hva_4k = 0x7fff_0000_0000u64; // Aligned to 4K
        let len_2m = 2 * 1024 * 1024usize; // 2 MiB = 512 pages

        let span_4k =
            BalloonEpollHandler::calculate_pagemap_span(hva_4k, len_2m, page_size_4k).unwrap();
        assert_eq!(span_4k.0, hva_4k / 4096); // start_page
        assert_eq!(span_4k.1, 512); // num_pages
        assert_eq!(span_4k.2, (hva_4k / 4096) * PAGEMAP_ENTRY_SIZE as u64); // file_offset

        // Unaligned HVA for 4K: rejected (return None) to conservatively fall back to normal reclaim
        assert!(
            BalloonEpollHandler::calculate_pagemap_span(hva_4k + 1, len_2m, page_size_4k).is_none()
        );
        assert!(
            BalloonEpollHandler::calculate_pagemap_span(hva_4k + 4095, len_2m, page_size_4k)
                .is_none()
        );

        // Unaligned len for 4K: rejected (return None) to avoid sub-page madvise errors
        assert!(
            BalloonEpollHandler::calculate_pagemap_span(hva_4k, len_2m + 1, page_size_4k).is_none()
        );
        assert!(
            BalloonEpollHandler::calculate_pagemap_span(hva_4k, len_2m + 4095, page_size_4k)
                .is_none()
        );

        // --- 64 KiB Host Page Size ---
        let page_size_64k = 65536usize;
        let hva_64k = 0x7fff_0000_0000u64; // Aligned to 64K
        let span_64k =
            BalloonEpollHandler::calculate_pagemap_span(hva_64k, len_2m, page_size_64k).unwrap();
        assert_eq!(span_64k.0, hva_64k / 65536); // start_page
        assert_eq!(span_64k.1, 32); // 2 MiB / 64 KiB = 32 pages
        assert_eq!(span_64k.2, (hva_64k / 65536) * PAGEMAP_ENTRY_SIZE as u64); // file_offset

        // Sub-host-page 4K descriptor inside 64K host page is unaligned to 64K: rejected
        assert!(
            BalloonEpollHandler::calculate_pagemap_span(hva_64k + 4096, 4096, page_size_64k)
                .is_none()
        );

        // --- Oversized descriptor scan bound check ---
        let oversized_len = (super::MAX_FILTER_SCAN_PAGES + 1) * page_size_4k;
        assert!(
            BalloonEpollHandler::calculate_pagemap_span(hva_4k, oversized_len, page_size_4k)
                .is_none()
        );

        // --- Invalid Page Size / Unavailable ---
        assert!(BalloonEpollHandler::calculate_pagemap_span(hva_4k, len_2m, 0).is_none());
        assert!(BalloonEpollHandler::calculate_pagemap_span(hva_4k, len_2m, 4095).is_none());
        assert!(BalloonEpollHandler::calculate_pagemap_span(hva_4k, len_2m, 4097).is_none());
    }

    #[test]
    fn test_smart_filter_capability_fallback_when_disabled() {
        let total_size = 512 * PAGE_SIZE;
        let mmap = MmapRegion::build(
            None,
            total_size,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
        )
        .unwrap();
        let region = GuestRegionMmap::new(mmap, GuestAddress(0)).unwrap();
        let memory = GuestMemoryMmap::from_regions(vec![region]).unwrap();

        // Handler with pagemap_filter = None (capability probe failed or FPR disabled)
        let handler = BalloonEpollHandler {
            mem: GuestMemoryAtomic::new(memory.clone()),
            queues: vec![],
            interrupt_cb: Arc::new(NoopVirtioInterrupt),
            inflate_queue_evt: EventFd::new(0).unwrap(),
            deflate_queue_evt: EventFd::new(0).unwrap(),
            reporting_queue_evt: None,
            kill_evt: EventFd::new(0).unwrap(),
            pause_evt: EventFd::new(0).unwrap(),
            pagemap_filter: None,
        };

        // When capability is unavailable, should_skip_reported_range MUST return false
        // ensuring 100% fallback to existing reclaim path.
        assert!(!handler.should_skip_reported_range(&memory, GuestAddress(0), total_size));
    }

    #[test]
    fn test_is_safe_to_skip_reclaim_entry() {
        use super::{
            is_safe_to_skip_reclaim_entry, PAGEMAP_PRESENT, PAGEMAP_SOFT_DIRTY, PAGEMAP_SWAPPED,
        };

        // Whitelist allowed:
        // 1. Clean ordinary unmapped hole (0x0) -> Safe to skip
        assert!(is_safe_to_skip_reclaim_entry(0));
        // 2. Soft-dirty alone on unmapped VMA (bit 55) -> Safe to skip
        assert!(is_safe_to_skip_reclaim_entry(PAGEMAP_SOFT_DIRTY));

        // Everything else MUST conservatively fallback (must NOT skip):
        // 3. Present page (bit 63)
        assert!(!is_safe_to_skip_reclaim_entry(PAGEMAP_PRESENT));
        assert!(!is_safe_to_skip_reclaim_entry(PAGEMAP_PRESENT | 0x12345));
        assert!(!is_safe_to_skip_reclaim_entry(
            PAGEMAP_PRESENT | PAGEMAP_SOFT_DIRTY
        ));

        // 4. Swapped page (bit 62)
        assert!(!is_safe_to_skip_reclaim_entry(PAGEMAP_SWAPPED));
        assert!(!is_safe_to_skip_reclaim_entry(PAGEMAP_SWAPPED | 0x54321));
        assert!(!is_safe_to_skip_reclaim_entry(
            PAGEMAP_SWAPPED | PAGEMAP_SOFT_DIRTY
        ));

        // 5. File-page or shared-anon (bit 61)
        assert!(!is_safe_to_skip_reclaim_entry(1u64 << 61));

        // 6. UFFD-WP write-protected (bit 57)
        assert!(!is_safe_to_skip_reclaim_entry(1u64 << 57));

        // 7. Page exclusively mapped (bit 56)
        assert!(!is_safe_to_skip_reclaim_entry(1u64 << 56));

        // 8. Reserved/future bits (bits 58..=60)
        assert!(!is_safe_to_skip_reclaim_entry(1u64 << 58));
        assert!(!is_safe_to_skip_reclaim_entry(1u64 << 59));
        assert!(!is_safe_to_skip_reclaim_entry(1u64 << 60));

        // 9. Soft-dirty combined with any unknown/future/marker bit
        assert!(!is_safe_to_skip_reclaim_entry(
            PAGEMAP_SOFT_DIRTY | (1u64 << 58)
        ));
        assert!(!is_safe_to_skip_reclaim_entry(PAGEMAP_SOFT_DIRTY | 0x1));

        // 10. Unexpected or undocumented non-zero payload when not present/swapped
        assert!(!is_safe_to_skip_reclaim_entry(0x1));
        assert!(!is_safe_to_skip_reclaim_entry(0xdead_beef));
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn test_real_pagemap_anonymous_mmap_lifecycle() {
        let (page_size, pagemap_file) = match (
            BalloonEpollHandler::host_page_size(),
            std::fs::File::open("/proc/self/pagemap"),
        ) {
            (Some(ps), Ok(f)) => (ps, f),
            _ => {
                eprintln!("Skipping test_real_pagemap_anonymous_mmap_lifecycle: /proc/self/pagemap unavailable");
                return;
            }
        };
        let total_size = 512 * page_size; // 512 host pages

        let mmap = MmapRegion::build(
            None,
            total_size,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
        )
        .unwrap();
        let region = GuestRegionMmap::new(mmap, GuestAddress(0)).unwrap();
        let memory = GuestMemoryMmap::from_regions(vec![region]).unwrap();

        let pagemap_filter = Some(super::PagemapFilter {
            file: pagemap_file,
            host_page_size: page_size,
        });

        let handler = BalloonEpollHandler {
            mem: GuestMemoryAtomic::new(memory.clone()),
            queues: vec![],
            interrupt_cb: Arc::new(NoopVirtioInterrupt),
            inflate_queue_evt: EventFd::new(0).unwrap(),
            deflate_queue_evt: EventFd::new(0).unwrap(),
            reporting_queue_evt: None,
            kill_evt: EventFd::new(0).unwrap(),
            pause_evt: EventFd::new(0).unwrap(),
            pagemap_filter,
        };

        // 1. Untouched fresh anonymous memory -> Real pagemap shows 0 -> Safe to skip!
        assert!(handler.should_skip_reported_range(&memory, GuestAddress(0), total_size));

        // 2. Touch first host page -> Real pagemap shows present bit -> Must fallback reclaim!
        memory.write_obj(0x5a_u8, GuestAddress(0)).unwrap();
        assert!(!handler.should_skip_reported_range(&memory, GuestAddress(0), total_size));

        // 3. Touch 10th host page -> Probe passes, full scan detects presence -> Fallback reclaim!
        BalloonEpollHandler::release_memory_range(&memory, GuestAddress(0), total_size).unwrap();
        // After madvise DONTNEED, whole range is non-resident again -> Safe to skip!
        assert!(handler.should_skip_reported_range(&memory, GuestAddress(0), total_size));

        let later_addr = GuestAddress((10 * page_size) as u64);
        memory.write_obj(0x5a_u8, later_addr).unwrap();
        assert!(!handler.should_skip_reported_range(&memory, GuestAddress(0), total_size));

        // 4. Clean up with madvise -> Real pagemap returns 0 again -> Safe to skip!
        BalloonEpollHandler::release_memory_range(&memory, GuestAddress(0), total_size).unwrap();
        assert!(handler.should_skip_reported_range(&memory, GuestAddress(0), total_size));
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn test_real_pagemap_file_backed_mmap_lifecycle() {
        let (page_size, pagemap_file) = match (
            BalloonEpollHandler::host_page_size(),
            std::fs::File::open("/proc/self/pagemap"),
        ) {
            (Some(ps), Ok(f)) => (ps, f),
            _ => {
                eprintln!("Skipping test_real_pagemap_file_backed_mmap_lifecycle: /proc/self/pagemap unavailable");
                return;
            }
        };
        let total_size = 512 * page_size;

        // Sparse backing file (all holes)
        let (file_path, file) = temp_file("filter-file-lifecycle", &[]);
        file.set_len(total_size as u64).unwrap();

        let mmap = MmapRegion::build(
            Some(FileOffset::new(file, 0)),
            total_size,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_PRIVATE,
        )
        .unwrap();
        let region = GuestRegionMmap::new(mmap, GuestAddress(0)).unwrap();
        let memory = GuestMemoryMmap::from_regions(vec![region]).unwrap();

        let pagemap_filter = Some(super::PagemapFilter {
            file: pagemap_file,
            host_page_size: page_size,
        });

        let handler = BalloonEpollHandler {
            mem: GuestMemoryAtomic::new(memory.clone()),
            queues: vec![],
            interrupt_cb: Arc::new(NoopVirtioInterrupt),
            inflate_queue_evt: EventFd::new(0).unwrap(),
            deflate_queue_evt: EventFd::new(0).unwrap(),
            reporting_queue_evt: None,
            kill_evt: EventFd::new(0).unwrap(),
            pause_evt: EventFd::new(0).unwrap(),
            pagemap_filter,
        };

        // 1. Untouched sparse private mapping -> Safe to skip
        assert!(handler.should_skip_reported_range(&memory, GuestAddress(0), total_size));

        // 2. Private CoW write to first host page -> Present -> Must fallback reclaim
        memory.write_obj(0xa5_u8, GuestAddress(0)).unwrap();
        assert!(!handler.should_skip_reported_range(&memory, GuestAddress(0), total_size));

        // 3. Reset via MADV_DONTNEED -> Discards CoW page, returns to unmapped -> Safe to skip
        BalloonEpollHandler::release_memory_range(&memory, GuestAddress(0), total_size).unwrap();
        assert!(handler.should_skip_reported_range(&memory, GuestAddress(0), total_size));

        fs::remove_file(file_path).unwrap();
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn test_pagemap_filter_self_check_semantics() {
        let (page_size, pagemap_file) = match (
            BalloonEpollHandler::host_page_size(),
            std::fs::File::open("/proc/self/pagemap"),
        ) {
            (Some(ps), Ok(f)) => (ps, f),
            _ => {
                eprintln!("Skipping test_pagemap_filter_self_check_semantics: /proc/self/pagemap unavailable");
                return;
            }
        };

        // 1. Genuine /proc/self/pagemap on supported host: self-check MUST succeed.
        assert!(super::PagemapFilter::verify_pagemap_support(
            &pagemap_file,
            page_size
        ));

        // 2. Simulated failure: an unpopulated or mock file that returns zeroes
        // (i.e. Bit 63 is never set) must fail the self-check, triggering safe fallback.
        let (mock_path, mock_file) = temp_file("mock-pagemap-zeroes", &vec![0u8; 4096]);
        assert!(!super::PagemapFilter::verify_pagemap_support(
            &mock_file, page_size
        ));
        fs::remove_file(mock_path).unwrap();
    }

    // Global lock to serialise tests that mutate process-wide /proc/self/clear_refs,
    // mirroring the protection used in `hypervisor/vmm/src/soft_dirty.rs`.
    static CLEAR_REFS_LOCK: Mutex<()> = Mutex::new(());

    #[cfg(target_os = "linux")]
    #[test]
    fn test_real_pagemap_soft_dirty_hole_classification() {
        let _guard = CLEAR_REFS_LOCK.lock().unwrap_or_else(|e| e.into_inner());

        let (page_size, pagemap_file) = match (
            BalloonEpollHandler::host_page_size(),
            std::fs::File::open("/proc/self/pagemap"),
        ) {
            (Some(ps), Ok(f)) => (ps, f),
            _ => {
                eprintln!(
                    "Skipping test_real_pagemap_soft_dirty_hole_classification: /proc/self/pagemap unavailable"
                );
                return;
            }
        };

        // Write "4" to /proc/self/clear_refs to trigger VM_SOFTDIRTY on VMAs, simulating
        // a post-snapshot state where pagemap_pte_hole() reports bit 55 for untouched holes.
        if std::fs::write("/proc/self/clear_refs", b"4").is_err() {
            eprintln!(
                "Skipping test_real_pagemap_soft_dirty_hole_classification: cannot write /proc/self/clear_refs"
            );
            return;
        }

        let total_size = 512 * page_size;
        let mmap = MmapRegion::build(
            None,
            total_size,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
        )
        .unwrap();
        let region = GuestRegionMmap::new(mmap, GuestAddress(0)).unwrap();
        let memory = GuestMemoryMmap::from_regions(vec![region]).unwrap();

        let pagemap_filter = Some(super::PagemapFilter {
            file: pagemap_file,
            host_page_size: page_size,
        });

        let handler = BalloonEpollHandler {
            mem: GuestMemoryAtomic::new(memory.clone()),
            queues: vec![],
            interrupt_cb: Arc::new(NoopVirtioInterrupt),
            inflate_queue_evt: EventFd::new(0).unwrap(),
            deflate_queue_evt: EventFd::new(0).unwrap(),
            reporting_queue_evt: None,
            kill_evt: EventFd::new(0).unwrap(),
            pause_evt: EventFd::new(0).unwrap(),
            pagemap_filter,
        };

        // Untouched hole must still be safely skipped even when bit 55 is present.
        assert!(handler.should_skip_reported_range(&memory, GuestAddress(0), total_size));
    }
}

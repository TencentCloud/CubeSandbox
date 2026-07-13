// Copyright © 2026 Tencent Corporation
//
// SPDX-License-Identifier: Apache-2.0

//! Soft-dirty snapshot support
//!
//! This module provides functionality for creating soft-dirty-based incremental
//! snapshots that only save pages actually written by the guest **since the
//! previous soft-dirty snapshot** (a true delta), by toggling
//! `/proc/self/clear_refs` and inspecting bit 55 of `/proc/self/pagemap`.
//!
//! Compared with the pagemap_anon path (which records every CoW-anonymous page
//! ever touched since restore), soft-dirty produces a much smaller delta on
//! the second and later incremental snapshots because the bit is reset on each
//! cycle.
//!
//! Lifecycle:
//! 1. First snapshot: write the full base, then `clear_soft_dirty()` to arm
//!    the kernel tracker.
//! 2. Subsequent snapshots: read pagemap bit 55, write only pages with the
//!    bit set, then `clear_soft_dirty()` again to start the next window.
//!
//! Requires `CONFIG_MEM_SOFT_DIRTY=y`. On kernels without this config the
//! caller is expected to silently fall back to the pagemap_anon path.

use crate::pagemap_anon::{self, coalesce_pages_to_ranges, get_anon_pages, host_page_size};
use log::{debug, info, trace};
use std::fs::{File, OpenOptions};
use std::io::{self, Read, Seek, SeekFrom, Write};
use std::time::Instant;
use thiserror::Error;
use vm_memory::{GuestAddress, GuestMemory, GuestMemoryMmap};
#[cfg(test)]
use vm_migration::protocol::MemoryRange;
use vm_migration::protocol::MemoryRangeTable;

/// Size of a pagemap entry in bytes
const PAGEMAP_ENTRY_SIZE: u64 = 8;

/// Bit 55 in pagemap: page has been written since the last clear_refs(4)
const PAGEMAP_SOFT_DIRTY_BIT: u64 = 1 << 55;

/// Bit 63: page is present in RAM
const PAGEMAP_PRESENT_BIT: u64 = 1 << 63;

/// Bit 62: page is in swap
const PAGEMAP_SWAPPED_BIT: u64 = 1 << 62;

/// Magic value to write to /proc/self/clear_refs to clear the soft-dirty
/// bit on every page table entry of the current process and (re)arm the
/// VM_SOFTDIRTY tracker.
const CLEAR_REFS_SOFT_DIRTY: &[u8] = b"4\n";

/// Errors related to soft-dirty operations
#[derive(Debug, Error)]
pub enum SoftDirtyError {
    #[error("Failed to open {path}: {source}")]
    OpenFailed {
        path: String,
        #[source]
        source: io::Error,
    },

    #[error("Failed to read {path}: {source}")]
    ReadFailed {
        path: String,
        #[source]
        source: io::Error,
    },

    #[error("Failed to seek in {path}: {source}")]
    SeekFailed {
        path: String,
        #[source]
        source: io::Error,
    },

    #[error("Failed to write to {path}: {source}")]
    WriteFailed {
        path: String,
        #[source]
        source: io::Error,
    },

    #[error("Failed to get host address for guest memory region")]
    GetHostAddressFailed,

    #[error("Memory region not aligned to page boundary")]
    NotPageAligned,

    #[error("Failed to probe anonymous pages via pagemap_anon: {0}")]
    AnonProbe(#[from] pagemap_anon::PagemapAnonError),
}

/// Result type for soft-dirty operations
pub type Result<T> = std::result::Result<T, SoftDirtyError>;

/// Statistics about soft-dirty filtering results
#[derive(Debug, Default, Clone)]
pub struct SoftDirtyStats {
    /// Total number of pages in the memory regions
    pub total_pages: u64,
    /// Number of pages with the soft-dirty bit set (written since last clear)
    pub dirty_pages: u64,
    /// Total bytes in all memory regions
    pub total_bytes: u64,
    /// Bytes that will be written (dirty pages only)
    pub saved_bytes: u64,
}

impl SoftDirtyStats {
    /// Calculate the percentage of memory saved (not needing to be snapshotted)
    pub fn savings_percentage(&self) -> f64 {
        if self.total_bytes == 0 {
            return 0.0;
        }
        ((self.total_bytes - self.saved_bytes) as f64 / self.total_bytes as f64) * 100.0
    }
}

/// Probe whether the running kernel supports the soft-dirty mechanism.
///
/// This performs a non-destructive write of "4" to `/proc/self/clear_refs`.
/// On kernels with `CONFIG_MEM_SOFT_DIRTY=y` this clears soft-dirty bits and
/// returns success. On kernels without this config the write returns
/// `EINVAL`, which we map to `false`.
///
/// Note: this call is technically not "non-destructive" — it does clear any
/// existing soft-dirty bits. The probe is therefore intended to be invoked
/// once at `MemoryManager` construction time, before any soft-dirty cycle
/// has started, where clearing has no observable effect.
pub fn probe_soft_dirty_support() -> bool {
    match clear_soft_dirty() {
        Ok(()) => true,
        Err(e) => {
            debug!("Soft-dirty kernel support probe failed: {}", e);
            false
        }
    }
}

/// Clear the soft-dirty bit on every PTE of the current process and (re)arm
/// the VM_SOFTDIRTY tracker on every VMA, by writing "4" to
/// `/proc/self/clear_refs`.
///
/// After this call, any page subsequently written by the guest (vCPU stores
/// or device DMA into guest memory) will be marked with bit 55 in
/// `/proc/self/pagemap`.
pub fn clear_soft_dirty() -> Result<()> {
    let start = Instant::now();
    let mut f = OpenOptions::new()
        .write(true)
        .open("/proc/self/clear_refs")
        .map_err(|e| SoftDirtyError::OpenFailed {
            path: "/proc/self/clear_refs".to_string(),
            source: e,
        })?;
    f.write_all(CLEAR_REFS_SOFT_DIRTY)
        .map_err(|e| SoftDirtyError::WriteFailed {
            path: "/proc/self/clear_refs".to_string(),
            source: e,
        })?;
    let elapsed = start.elapsed();
    // The kernel walks every PTE of every anonymous VMA under mmap_lock
    // (write) here, and on large guests this is the dominant snapshot-time
    // stall (hundreds of ms on multi-GiB VMs). Log at info so operators
    // can correlate it with guest pause-time spikes without enabling debug.
    info!(
        "soft-dirty: clear_refs(4) took {} us ({:.3} ms)",
        elapsed.as_micros(),
        elapsed.as_secs_f64() * 1000.0
    );
    Ok(())
}

/// Get the soft-dirty bitmap for a memory region by reading
/// `/proc/self/pagemap`.
///
/// # Arguments
/// * `host_addr` - Host virtual address of the memory region (must be page-aligned)
/// * `length` - Length of the memory region in bytes
///
/// # Returns
/// A vector of bools where each bool indicates whether the corresponding
/// page has been written since the previous `clear_soft_dirty()` call.
///
/// A page counts as soft-dirty if either:
/// - It is present and bit 55 is set, or
/// - It is in swap (bit 62) — swapped pages are by definition pages that
///   were anonymous and previously written, so they must be saved on the
///   next snapshot to be safe.
pub fn get_soft_dirty_pages(host_addr: u64, length: u64) -> Result<Vec<bool>> {
    let page_size = host_page_size();
    if host_addr % page_size != 0 {
        return Err(SoftDirtyError::NotPageAligned);
    }

    let num_pages = length.div_ceil(page_size) as usize;
    let start_page = host_addr / page_size;

    let mut pagemap_file =
        File::open("/proc/self/pagemap").map_err(|e| SoftDirtyError::OpenFailed {
            path: "/proc/self/pagemap".to_string(),
            source: e,
        })?;

    let pagemap_offset = start_page * PAGEMAP_ENTRY_SIZE;
    pagemap_file
        .seek(SeekFrom::Start(pagemap_offset))
        .map_err(|e| SoftDirtyError::SeekFailed {
            path: "/proc/self/pagemap".to_string(),
            source: e,
        })?;

    let buf_size = num_pages * PAGEMAP_ENTRY_SIZE as usize;
    let mut pagemap_buf = vec![0u8; buf_size];
    pagemap_file
        .read_exact(&mut pagemap_buf)
        .map_err(|e| SoftDirtyError::ReadFailed {
            path: "/proc/self/pagemap".to_string(),
            source: e,
        })?;

    let mut result = vec![false; num_pages];
    for (i, item) in result.iter_mut().enumerate().take(num_pages) {
        let entry_offset = i * PAGEMAP_ENTRY_SIZE as usize;
        let entry = u64::from_ne_bytes(
            pagemap_buf[entry_offset..entry_offset + PAGEMAP_ENTRY_SIZE as usize]
                .try_into()
                .unwrap(),
        );

        let present = (entry & PAGEMAP_PRESENT_BIT) != 0;
        let swapped = (entry & PAGEMAP_SWAPPED_BIT) != 0;
        let soft_dirty = (entry & PAGEMAP_SOFT_DIRTY_BIT) != 0;

        // Treat swapped anonymous pages as dirty: they were written before
        // being swapped out, and the kernel does not preserve the soft-dirty
        // bit across swap-in on all kernels.
        if swapped {
            *item = true;
            continue;
        }

        if present && soft_dirty {
            *item = true;
        }
    }

    Ok(result)
}

/// Filter memory ranges by soft-dirty, returning only ranges with pages
/// written since the previous `clear_soft_dirty()` call.
///
/// # Arguments
/// * `guest_memory` - The guest memory object
/// * `ranges` - The original memory range table
///
/// # Returns
/// A tuple containing:
/// - The filtered memory range table (only soft-dirty pages, merged into
///   contiguous runs)
/// - Statistics about the filtering
pub fn filter_memory_ranges_by_soft_dirty<B: vm_memory::bitmap::Bitmap + 'static>(
    guest_memory: &GuestMemoryMmap<B>,
    ranges: &MemoryRangeTable,
) -> Result<(MemoryRangeTable, SoftDirtyStats)> {
    let mut filtered_ranges = MemoryRangeTable::default();
    let mut stats = SoftDirtyStats::default();
    let page_size = host_page_size();

    debug!(
        "Starting soft-dirty filtering for {} memory regions",
        ranges.regions().len()
    );

    for range in ranges.regions() {
        let gpa = range.gpa;
        let length = range.length;

        stats.total_bytes += length;
        stats.total_pages += length.div_ceil(page_size);

        trace!(
            "Processing memory region: GPA=0x{:x}, length={}",
            gpa,
            length
        );

        let host_addr = guest_memory
            .get_host_address(GuestAddress(gpa))
            .map_err(|_| SoftDirtyError::GetHostAddressFailed)?;

        let dirty_pages = get_soft_dirty_pages(host_addr as u64, length)?;

        let (region_ranges, dirty_count) = coalesce_pages_to_ranges(gpa, &dirty_pages, page_size);
        stats.dirty_pages += dirty_count;
        stats.saved_bytes += dirty_count * page_size;
        for r in region_ranges {
            filtered_ranges.push(r);
        }
    }

    debug!(
        "Soft-dirty filtering complete: {} dirty ranges, {} total pages, {} dirty pages",
        filtered_ranges.regions().len(),
        stats.total_pages,
        stats.dirty_pages
    );

    if stats.total_pages > 0 {
        let dirty_pct = (stats.dirty_pages as f64 / stats.total_pages as f64) * 100.0;
        debug!(
            "Soft-dirty stats: {:.1}% dirty pages, {:.1}% savings vs full snapshot",
            dirty_pct,
            stats.savings_percentage()
        );
    }

    Ok((filtered_ranges, stats))
}

/// Filter memory ranges by the **intersection** of anonymous (CoW) pages
/// and soft-dirty pages.
///
/// The set of pages we want to write into an incremental snapshot is exactly:
///   * pages that have actually been Copy-on-Written by the guest (anonymous
///     after the `MAP_PRIVATE` restore — only these can ever differ from the
///     base snapshot file), AND
///   * pages whose contents have changed **since the previous
///     `clear_soft_dirty()`** (the delta this cycle must record).
///
/// Either signal alone over- or under-approximates:
///   * "anonymous only" is correct but cumulative — every page CoW'd since
///     restore shows up forever, even if it has not been modified in the
///     current snapshot window.
///   * "soft-dirty only" can include host-side writes that landed on
///     file-backed page-cache pages (e.g. shared-mmap restore); those pages
///     are the same content as the base file on disk and re-saving them
///     would be wasteful — and on the very first soft-dirty cycle of a
///     freshly-armed tracker every guest write since arm shows up,
///     including writes that fault in non-anon pages.
///
/// Taking the intersection gives us exactly the anon pages whose contents
/// changed in the current window, which is both minimal and safe.
pub fn filter_memory_ranges_by_anon_and_soft_dirty<B: vm_memory::bitmap::Bitmap + 'static>(
    guest_memory: &GuestMemoryMmap<B>,
    ranges: &MemoryRangeTable,
) -> Result<(MemoryRangeTable, SoftDirtyStats)> {
    let mut filtered_ranges = MemoryRangeTable::default();
    let mut stats = SoftDirtyStats::default();
    let page_size = host_page_size();

    debug!(
        "Starting anon ∩ soft-dirty filtering for {} memory regions",
        ranges.regions().len()
    );

    for range in ranges.regions() {
        let gpa = range.gpa;
        let length = range.length;

        stats.total_bytes += length;
        stats.total_pages += length.div_ceil(page_size);

        trace!(
            "Processing memory region: GPA=0x{:x}, length={}",
            gpa,
            length
        );

        let host_addr = guest_memory
            .get_host_address(GuestAddress(gpa))
            .map_err(|_| SoftDirtyError::GetHostAddressFailed)?;

        let anon_pages = get_anon_pages(host_addr as u64, length)?;
        let dirty_pages = get_soft_dirty_pages(host_addr as u64, length)?;

        debug_assert_eq!(anon_pages.len(), dirty_pages.len());

        // A page must be saved only when it is both anonymous (CoW) and
        // soft-dirty in the current window.
        let must_save: Vec<bool> = anon_pages
            .iter()
            .zip(dirty_pages.iter())
            .map(|(&is_anon, &is_dirty)| is_anon && is_dirty)
            .collect();

        let (region_ranges, save_count) = coalesce_pages_to_ranges(gpa, &must_save, page_size);
        stats.dirty_pages += save_count;
        stats.saved_bytes += save_count * page_size;
        for r in region_ranges {
            filtered_ranges.push(r);
        }
    }

    debug!(
        "anon ∩ soft-dirty filtering complete: {} ranges, {} total pages, {} pages to save",
        filtered_ranges.regions().len(),
        stats.total_pages,
        stats.dirty_pages
    );

    if stats.total_pages > 0 {
        let dirty_pct = (stats.dirty_pages as f64 / stats.total_pages as f64) * 100.0;
        debug!(
            "anon ∩ soft-dirty stats: {:.1}% pages to save, {:.1}% savings vs full snapshot",
            dirty_pct,
            stats.savings_percentage()
        );
    }

    Ok((filtered_ranges, stats))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;

    /// `/proc/self/clear_refs` and the soft-dirty PTE bit are *process*-wide
    /// state. When `cargo test` runs cases concurrently, one test's
    /// `clear_soft_dirty()` can race with another test's "write then read
    /// bit 55" check and clear the bit before it is observed, producing
    /// spurious failures. Serialize every test that calls `clear_soft_dirty`
    /// or reads the pagemap of pages we just dirtied through this lock.
    static CLEAR_REFS_LOCK: Mutex<()> = Mutex::new(());

    #[test]
    fn test_soft_dirty_stats_savings_percentage() {
        let stats = SoftDirtyStats {
            total_bytes: 1000,
            saved_bytes: 250,
            ..Default::default()
        };
        assert!((stats.savings_percentage() - 75.0).abs() < 0.01);
    }

    #[test]
    fn test_soft_dirty_stats_zero_total() {
        let stats = SoftDirtyStats::default();
        assert_eq!(stats.savings_percentage(), 0.0);
    }

    #[test]
    fn test_soft_dirty_stats_full_dirty() {
        let stats = SoftDirtyStats {
            total_bytes: 4096,
            saved_bytes: 4096,
            ..Default::default()
        };
        assert!((stats.savings_percentage() - 0.0).abs() < 0.01);
    }

    #[test]
    fn test_get_soft_dirty_pages_not_page_aligned() {
        // One byte past a page boundary is never aligned on 4 KiB or 64 KiB hosts.
        let unaligned = host_page_size() + 1;
        let result = get_soft_dirty_pages(unaligned, host_page_size());
        assert!(result.is_err());
        assert!(matches!(
            result.unwrap_err(),
            SoftDirtyError::NotPageAligned
        ));
    }

    #[test]
    fn test_pagemap_bit_constants() {
        assert_eq!(PAGEMAP_SOFT_DIRTY_BIT, 1u64 << 55);
        assert_eq!(PAGEMAP_PRESENT_BIT, 1u64 << 63);
        assert_eq!(PAGEMAP_SWAPPED_BIT, 1u64 << 62);
    }

    /// Self-test: if the running kernel supports soft-dirty, allocating and
    /// writing a scratch page after `clear_soft_dirty()` should make that
    /// page show up as dirty in the pagemap.
    ///
    /// Skipped silently when the kernel does not support soft-dirty (e.g.
    /// running on a stripped-down CI kernel).
    #[test]
    fn test_clear_and_observe_dirty_page() {
        // Hold the process-wide lock for the entire "clear -> write -> read
        // pagemap" sequence; without it a parallel test calling
        // clear_soft_dirty() can wipe our bit before we read it.
        let _guard = CLEAR_REFS_LOCK.lock().unwrap_or_else(|e| e.into_inner());

        if !probe_soft_dirty_support() {
            eprintln!("kernel has no soft-dirty support; skipping");
            return;
        }

        // Allocate one page-aligned scratch buffer.
        let page = host_page_size() as usize;
        let layout = std::alloc::Layout::from_size_align(page, page).unwrap();
        // SAFETY: standard allocation followed by an explicit deallocation
        // at the end of the test.
        let ptr = unsafe { std::alloc::alloc_zeroed(layout) };
        assert!(!ptr.is_null());

        // Touch (write) the page to make sure it has a PTE, then clear so
        // the dirty bit starts at 0.
        unsafe { ptr.write_volatile(0) };
        clear_soft_dirty().expect("clear_soft_dirty failed");

        // Right after clear, the page should not be soft-dirty.
        let before = get_soft_dirty_pages(ptr as u64, page as u64).expect("get_soft_dirty_pages");
        assert_eq!(before.len(), 1);

        // Now write to the page.
        unsafe { ptr.write_volatile(0xab) };

        // It should now show up as soft-dirty.
        let after = get_soft_dirty_pages(ptr as u64, page as u64).expect("get_soft_dirty_pages");
        assert_eq!(after.len(), 1);
        assert!(
            after[0],
            "page expected to be soft-dirty after a write, but bit 55 was not set"
        );

        // SAFETY: matches the alloc above.
        unsafe { std::alloc::dealloc(ptr, layout) };
    }

    /// End-to-end integration test that drives `filter_memory_ranges_by_soft_dirty`
    /// through the same call sequence `MemoryManager::send_soft_dirty_memory()`
    /// performs at runtime, but against a real `GuestMemoryMmap` backed by an
    /// anonymous private mapping owned by the test process itself.
    ///
    /// Sequence:
    ///   round 1 (base)        : clear_soft_dirty()  -> arm tracker
    ///   round 2 (delta #1)    : write pages [2..=4] -> filter must return exactly those
    ///                            clear_soft_dirty() -> arm next window
    ///   round 3 (delta #2)    : write page  [7]     -> filter must return ONLY page 7
    ///                                                  (i.e. soft-dirty really is a delta,
    ///                                                  not a cumulative set)
    ///                            clear_soft_dirty() -> arm next window
    ///   round 4 (delta empty) : write nothing       -> filter must return empty
    ///
    /// Skipped silently when the host kernel lacks `CONFIG_MEM_SOFT_DIRTY=y`.
    #[test]
    fn test_filter_memory_ranges_by_soft_dirty_end_to_end() {
        // Serialize against any other test that touches /proc/self/clear_refs
        // — it is process-wide state.
        let _guard = CLEAR_REFS_LOCK.lock().unwrap_or_else(|e| e.into_inner());

        if !probe_soft_dirty_support() {
            eprintln!(
                "kernel has no soft-dirty support (CONFIG_MEM_SOFT_DIRTY=n); skipping {}",
                "test_filter_memory_ranges_by_soft_dirty_end_to_end"
            );
            return;
        }

        // Build a single-region GuestMemoryMmap of 16 pages starting at GPA 0.
        // GuestMemoryMmap::from_ranges() defaults to an anonymous MAP_PRIVATE
        // mapping, which is exactly the kind of memory soft-dirty tracks.
        const NUM_PAGES: u64 = 16;
        // Drive the test at the host's real page size so it exercises the
        // 64 KiB path when run on an ARM64 64 KiB kernel.
        let page_size = host_page_size();
        let region_len = (NUM_PAGES * page_size) as usize;
        let guest_memory =
            GuestMemoryMmap::<()>::from_ranges(&[(GuestAddress(0), region_len)]).unwrap();

        // Pre-fault every page once so the kernel has real PTEs for them.
        // Without this, pages may remain "not present" and the test would
        // observe zero dirty pages even after writes (because the writes
        // themselves are what fault them in).
        let host_base = guest_memory
            .get_host_address(GuestAddress(0))
            .expect("get_host_address") as *mut u8;
        for p in 0..NUM_PAGES {
            // SAFETY: host_base..host_base+region_len is a valid mapping
            // owned by `guest_memory`; we touch one byte per page.
            unsafe { host_base.add((p * page_size) as usize).write_volatile(0) };
        }

        let ranges = {
            let mut t = MemoryRangeTable::default();
            t.push(MemoryRange {
                gpa: 0,
                length: NUM_PAGES * page_size,
            });
            t
        };

        // Helper: write one byte into the page at `page_idx`.
        let dirty_page = |page_idx: u64, val: u8| {
            // SAFETY: 0 <= page_idx < NUM_PAGES, mapping is at least
            // NUM_PAGES * page_size bytes long.
            unsafe {
                host_base
                    .add((page_idx * page_size) as usize)
                    .write_volatile(val)
            };
        };

        // Helper: collect the (gpa, length) pairs that the filter returned,
        // for ergonomic assertions.
        let collect = |table: &MemoryRangeTable| -> Vec<(u64, u64)> {
            table.regions().iter().map(|r| (r.gpa, r.length)).collect()
        };

        // ---------------- Round 1: arm the tracker (the "base" cycle) ----------------
        clear_soft_dirty().expect("clear_soft_dirty: round 1");

        // ---------------- Round 2: dirty pages 2,3,4 -> must show up ----------------
        dirty_page(2, 0xa1);
        dirty_page(3, 0xa2);
        dirty_page(4, 0xa3);

        let (filtered, stats) =
            filter_memory_ranges_by_soft_dirty(&guest_memory, &ranges).expect("filter round 2");

        // The three writes must be coalesced into exactly one contiguous run
        // [page 2 .. page 5).
        assert_eq!(
            collect(&filtered),
            vec![(2 * page_size, 3 * page_size)],
            "round 2: expected exactly the three written pages, coalesced"
        );
        assert_eq!(stats.total_pages, NUM_PAGES);
        assert_eq!(
            stats.dirty_pages, 3,
            "round 2: expected exactly 3 dirty pages"
        );
        assert_eq!(stats.saved_bytes, 3 * page_size);

        // Re-arm the tracker for the next window. After this point, the bits
        // for pages 2/3/4 must be cleared again.
        clear_soft_dirty().expect("clear_soft_dirty: round 2 -> 3");

        // ---------------- Round 3: dirty only page 7 -> must be the ONLY dirty page ----------------
        // This is the key assertion: soft-dirty must behave as a *delta*,
        // not a cumulative set. If it were cumulative (like pagemap_anon),
        // pages 2/3/4 would still show up here.
        dirty_page(7, 0xb1);

        let (filtered, stats) =
            filter_memory_ranges_by_soft_dirty(&guest_memory, &ranges).expect("filter round 3");

        assert_eq!(
            collect(&filtered),
            vec![(7 * page_size, page_size)],
            "round 3: expected ONLY page 7, soft-dirty must be a delta and \
             must NOT report pages 2/3/4 from the previous window"
        );
        assert_eq!(
            stats.dirty_pages, 1,
            "round 3: expected exactly 1 dirty page (true delta)"
        );

        clear_soft_dirty().expect("clear_soft_dirty: round 3 -> 4");

        // ---------------- Round 4: write nothing -> empty delta ----------------
        let (filtered, stats) =
            filter_memory_ranges_by_soft_dirty(&guest_memory, &ranges).expect("filter round 4");

        assert!(
            filtered.regions().is_empty(),
            "round 4: expected an empty delta with no writes since clear, got {:?}",
            collect(&filtered)
        );
        assert_eq!(
            stats.dirty_pages, 0,
            "round 4: dirty_pages must be 0 with no writes since clear"
        );
        assert_eq!(stats.total_pages, NUM_PAGES);
        assert!(
            (stats.savings_percentage() - 100.0).abs() < 0.01,
            "round 4: with zero dirty pages, savings should be 100%, got {}",
            stats.savings_percentage()
        );
    }

    /// End-to-end test for `filter_memory_ranges_by_anon_and_soft_dirty`:
    /// the result must be the intersection of "anonymous (CoW)" and
    /// "soft-dirty since the last clear".
    ///
    /// We build an anonymous private mapping (every present page in it
    /// is anon by construction), so on this fixture anon == always true
    /// and the intersection collapses to "soft-dirty only" — i.e. the
    /// helper must produce exactly the same per-window delta as
    /// `filter_memory_ranges_by_soft_dirty` does.
    ///
    /// Skipped silently when the host kernel lacks `CONFIG_MEM_SOFT_DIRTY=y`,
    /// or when the test process lacks CAP_SYS_ADMIN to read PFNs from
    /// /proc/self/pagemap and classify anonymous pages through /proc/kpageflags.
    #[test]
    fn test_filter_memory_ranges_by_anon_and_soft_dirty_end_to_end() {
        let _guard = CLEAR_REFS_LOCK.lock().unwrap_or_else(|e| e.into_inner());

        if !probe_soft_dirty_support() {
            eprintln!(
                "kernel has no soft-dirty support (CONFIG_MEM_SOFT_DIRTY=n); skipping {}",
                "test_filter_memory_ranges_by_anon_and_soft_dirty_end_to_end"
            );
            return;
        }

        const NUM_PAGES: u64 = 16;
        // Use the host's real page size so this covers ARM64 64 KiB kernels.
        let page_size = host_page_size();
        let region_len = (NUM_PAGES * page_size) as usize;
        let guest_memory =
            GuestMemoryMmap::<()>::from_ranges(&[(GuestAddress(0), region_len)]).unwrap();

        let host_base = guest_memory
            .get_host_address(GuestAddress(0))
            .expect("get_host_address") as *mut u8;
        for p in 0..NUM_PAGES {
            // SAFETY: host_base..host_base+region_len is owned by guest_memory.
            unsafe { host_base.add((p * page_size) as usize).write_volatile(0) };
        }

        let ranges = {
            let mut t = MemoryRangeTable::default();
            t.push(MemoryRange {
                gpa: 0,
                length: NUM_PAGES * page_size,
            });
            t
        };

        let dirty_page = |page_idx: u64, val: u8| {
            // SAFETY: 0 <= page_idx < NUM_PAGES.
            unsafe {
                host_base
                    .add((page_idx * page_size) as usize)
                    .write_volatile(val)
            };
        };

        let collect = |table: &MemoryRangeTable| -> Vec<(u64, u64)> {
            table.regions().iter().map(|r| (r.gpa, r.length)).collect()
        };

        // Arm the tracker.
        clear_soft_dirty().expect("clear_soft_dirty: round 1");

        // Dirty pages 5 and 6 (will be one coalesced run after AND).
        dirty_page(5, 0xc1);
        dirty_page(6, 0xc2);

        let (filtered, stats) =
            match filter_memory_ranges_by_anon_and_soft_dirty(&guest_memory, &ranges) {
                Ok(result) => result,
                Err(SoftDirtyError::AnonProbe(pagemap_anon::PagemapAnonError::NoCapSysAdmin)) => {
                    eprintln!(
                        "no CAP_SYS_ADMIN to read pagemap PFNs; skipping {}",
                        "test_filter_memory_ranges_by_anon_and_soft_dirty_end_to_end"
                    );
                    return;
                }
                Err(e) => panic!("anon ∩ soft-dirty filter failed: {e}"),
            };

        // Every page in this anon MAP_PRIVATE mapping is anon, so
        // anon ∩ soft-dirty == soft-dirty exactly: only pages 5 and 6.
        assert_eq!(
            collect(&filtered),
            vec![(5 * page_size, 2 * page_size)],
            "anon ∩ soft-dirty must yield exactly the two pages \
             written in this window, coalesced"
        );
        assert_eq!(stats.total_pages, NUM_PAGES);
        assert_eq!(stats.dirty_pages, 2);
        assert_eq!(stats.saved_bytes, 2 * page_size);
    }
}

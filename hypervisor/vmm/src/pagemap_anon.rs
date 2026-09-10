// Copyright © 2026 Tencent Corporation
//
// SPDX-License-Identifier: Apache-2.0

//! PagemapAnon snapshot support
//!
//! Incremental snapshots only rewrite pages the guest CoW-dirtied on a
//! `MAP_PRIVATE` mmap of the base image:
//! - never touched: no PTE (`present=0`) — keep the reflink base
//! - read only: still file-backed (pagemap bit 61) — keep the reflink base
//! - written: exclusive anonymous (bit 61 clear) or swapped — overwrite
//!
//! Classification uses one `/proc/self/pagemap` read (bit 61 / 62 / 63).
//! Do not follow the PFN into `/proc/kpageflags`: that walk is not atomic
//! with the pagemap snapshot, and compaction can move the page before the
//! flags are read, dropping dirty pages from the dest file.

use log::{debug, trace};
use once_cell::sync::Lazy;
use std::fs::File;
use std::io::{self, Read, Seek, SeekFrom};
use thiserror::Error;
use vm_memory::{GuestAddress, GuestMemory, GuestMemoryMmap};
use vm_migration::protocol::{MemoryRange, MemoryRangeTable};

/// Host page size in bytes, probed once from `sysconf(_SC_PAGESIZE)`.
///
/// This is 4 KiB on x86_64 but 64 KiB on ARM64 hosts configured with 64 KiB
/// base pages. `/proc/self/pagemap` is indexed in units of the kernel's real
/// page size, so every page-index, seek-offset and range-length computation
/// must use this value — hardcoding 4096 would mis-index the pagemap and
/// silently corrupt snapshots on 64 KiB kernels.
///
/// The value is fixed for the process lifetime, so probe it once and cache it.
static HOST_PAGE_SIZE: Lazy<u64> = Lazy::new(|| {
    // Trivially safe: sysconf(_SC_PAGESIZE) takes no pointer argument.
    let ret = unsafe { libc::sysconf(libc::_SC_PAGESIZE) };
    // POSIX returns -1 on failure; a non-positive page size would corrupt all
    // pagemap offset math. The checked conversion rejects any negative value.
    u64::try_from(ret).expect("sysconf(_SC_PAGESIZE) returned non-positive value")
});

/// Returns the host page size in bytes (cached; probed once via sysconf).
pub fn host_page_size() -> u64 {
    *HOST_PAGE_SIZE
}

/// Coalesce a per-page boolean bitmap into contiguous guest-physical ranges.
///
/// `gpa` is the base guest-physical address the bitmap covers and `page_size`
/// is the byte granularity each bit represents. Returns the merged ranges plus
/// the number of set pages (for stats/accounting).
///
/// Kept as a pure function (no `/proc` access) with an explicit `page_size`
/// argument so it can be exercised with an injected page size — in particular
/// 65536 to prove the ARM64 64 KiB path — without a matching-page-size kernel.
pub(crate) fn coalesce_pages_to_ranges(
    gpa: u64,
    bitmap: &[bool],
    page_size: u64,
) -> (Vec<MemoryRange>, u64) {
    let mut ranges = Vec::new();
    let mut set_pages: u64 = 0;
    let mut current_range_start: Option<u64> = None;
    let mut current_range_length: u64 = 0;

    for (page_idx, &set) in bitmap.iter().enumerate() {
        let page_gpa = gpa + (page_idx as u64 * page_size);

        if set {
            set_pages += 1;
            if current_range_start.is_none() {
                current_range_start = Some(page_gpa);
                current_range_length = page_size;
            } else {
                current_range_length += page_size;
            }
        } else if let Some(start) = current_range_start.take() {
            ranges.push(MemoryRange {
                gpa: start,
                length: current_range_length,
            });
            current_range_length = 0;
        }
    }

    if let Some(start) = current_range_start {
        ranges.push(MemoryRange {
            gpa: start,
            length: current_range_length,
        });
    }

    (ranges, set_pages)
}

/// Size of a pagemap entry in bytes
const PAGEMAP_ENTRY_SIZE: u64 = 8;

/// Bit 63: page is present in RAM
const PAGEMAP_PRESENT_BIT: u64 = 1 << 63;

/// Bit 62: page is in swap
const PAGEMAP_SWAPPED_BIT: u64 = 1 << 62;

/// Bit 61: file-backed page or KSM shared-anon (`Documentation/admin-guide/mm/pagemap.rst`).
/// Exclusive MAP_PRIVATE CoW pages have this bit clear.
const PAGEMAP_FILE_OR_SHARED_ANON_BIT: u64 = 1 << 61;

/// Whether this pagemap entry is a guest-written page that incremental
/// snapshot must overlay onto the reflink base.
pub(crate) fn pagemap_entry_is_cow_anon(entry: u64) -> bool {
    if (entry & PAGEMAP_SWAPPED_BIT) != 0 {
        return true;
    }
    if (entry & PAGEMAP_PRESENT_BIT) == 0 {
        return false;
    }
    (entry & PAGEMAP_FILE_OR_SHARED_ANON_BIT) == 0
}

/// Errors related to pagemap_anon operations
#[derive(Debug, Error)]
pub enum PagemapAnonError {
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

    #[error("Failed to get host address for guest memory region")]
    GetHostAddressFailed,

    #[error("Memory region not aligned to page boundary")]
    NotPageAligned,

    #[error("No CAP_SYS_ADMIN permission: PFN is zero for a present page, cannot read kpageflags")]
    NoCapSysAdmin,
}

/// Result type for pagemap_anon operations
pub type Result<T> = std::result::Result<T, PagemapAnonError>;

/// Statistics about pagemap_anon filtering results
#[derive(Debug, Default, Clone)]
pub struct PagemapAnonStats {
    /// Total number of pages in the memory regions
    pub total_pages: u64,
    /// Number of anonymous pages (CoW pages written by Guest)
    pub anon_pages: u64,
    /// Number of pages that are swapped out (also counted as anon)
    pub swapped_pages: u64,
    /// Total bytes in all memory regions
    pub total_bytes: u64,
    /// Bytes that will be saved (anonymous pages)
    pub saved_bytes: u64,
}

impl PagemapAnonStats {
    /// Calculate the percentage of memory saved (not needing to be snapshotted)
    pub fn savings_percentage(&self) -> f64 {
        if self.total_bytes == 0 {
            return 0.0;
        }
        ((self.total_bytes - self.saved_bytes) as f64 / self.total_bytes as f64) * 100.0
    }
}

/// Get the anonymous page bitmap for a memory region from `/proc/self/pagemap`.
///
/// # Arguments
/// * `host_addr` - Host virtual address of the memory region (must be page-aligned)
/// * `length` - Length of the memory region in bytes
///
/// # Returns
/// A vector of bools where each bool indicates if the corresponding page
/// is an exclusive anonymous / swapped page that must be written out.
pub fn get_anon_pages(host_addr: u64, length: u64) -> Result<Vec<bool>> {
    let page_size = host_page_size();
    if host_addr % page_size != 0 {
        return Err(PagemapAnonError::NotPageAligned);
    }

    let num_pages = length.div_ceil(page_size) as usize;
    let start_page = host_addr / page_size;

    let mut pagemap_file =
        File::open("/proc/self/pagemap").map_err(|e| PagemapAnonError::OpenFailed {
            path: "/proc/self/pagemap".to_string(),
            source: e,
        })?;

    let pagemap_offset = start_page * PAGEMAP_ENTRY_SIZE;
    pagemap_file
        .seek(SeekFrom::Start(pagemap_offset))
        .map_err(|e| PagemapAnonError::SeekFailed {
            path: "/proc/self/pagemap".to_string(),
            source: e,
        })?;

    let buf_size = num_pages * PAGEMAP_ENTRY_SIZE as usize;
    let mut pagemap_buf = vec![0u8; buf_size];
    pagemap_file
        .read_exact(&mut pagemap_buf)
        .map_err(|e| PagemapAnonError::ReadFailed {
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
        *item = pagemap_entry_is_cow_anon(entry);
    }

    Ok(result)
}

/// Filter memory ranges by pagemap_anon, returning only ranges with anonymous (CoW) pages.
///
/// This function takes a table of memory ranges and returns a new table
/// containing only the pages that are anonymous (written by Guest via CoW).
///
/// # Arguments
/// * `guest_memory` - The guest memory object
/// * `ranges` - The original memory range table
///
/// # Returns
/// A tuple containing:
/// - The filtered memory range table (only anonymous pages)
/// - Statistics about the filtering
pub fn filter_memory_ranges_by_pagemap_anon<B: vm_memory::bitmap::Bitmap + 'static>(
    guest_memory: &GuestMemoryMmap<B>,
    ranges: &MemoryRangeTable,
) -> Result<(MemoryRangeTable, PagemapAnonStats)> {
    let mut filtered_ranges = MemoryRangeTable::default();
    let mut stats = PagemapAnonStats::default();
    let page_size = host_page_size();

    debug!(
        "Starting pagemap_anon filtering for {} memory regions",
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

        // Get host virtual address for this guest physical address
        let host_addr = guest_memory
            .get_host_address(GuestAddress(gpa))
            .map_err(|_| PagemapAnonError::GetHostAddressFailed)?;

        let anon_pages = get_anon_pages(host_addr as u64, length)?;

        // Convert bitmap to memory ranges (merge consecutive anonymous pages)
        let (region_ranges, anon_count) = coalesce_pages_to_ranges(gpa, &anon_pages, page_size);
        stats.anon_pages += anon_count;
        stats.saved_bytes += anon_count * page_size;
        for r in region_ranges {
            filtered_ranges.push(r);
        }
    }

    debug!(
        "PagemapAnon filtering complete: {} anon ranges, {} total pages, {} anon pages ({} swapped)",
        filtered_ranges.regions().len(),
        stats.total_pages,
        stats.anon_pages,
        stats.swapped_pages
    );

    if stats.total_pages > 0 {
        let anon_pct = (stats.anon_pages as f64 / stats.total_pages as f64) * 100.0;
        debug!(
            "PagemapAnon stats: {:.1}% anonymous pages, {:.1}% savings vs full snapshot",
            anon_pct,
            stats.savings_percentage()
        );
    }

    Ok((filtered_ranges, stats))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_pagemap_anon_stats_savings_percentage() {
        let stats = PagemapAnonStats {
            total_bytes: 1000,
            saved_bytes: 250,
            ..Default::default()
        };

        assert!((stats.savings_percentage() - 75.0).abs() < 0.01);
    }

    #[test]
    fn test_pagemap_anon_stats_zero_total() {
        let stats = PagemapAnonStats::default();
        assert_eq!(stats.savings_percentage(), 0.0);
    }

    #[test]
    fn test_pagemap_anon_stats_full_anon() {
        let stats = PagemapAnonStats {
            total_bytes: 4096,
            saved_bytes: 4096,
            ..Default::default()
        };

        assert!((stats.savings_percentage() - 0.0).abs() < 0.01);
    }

    #[test]
    fn test_get_anon_pages_not_page_aligned() {
        // An address one byte past a page boundary must never be page-aligned,
        // regardless of whether the host uses 4 KiB or 64 KiB pages.
        let unaligned = host_page_size() + 1;
        let result = get_anon_pages(unaligned, host_page_size());
        assert!(result.is_err());
        assert!(matches!(
            result.unwrap_err(),
            PagemapAnonError::NotPageAligned
        ));
    }

    #[test]
    fn test_host_page_size_is_power_of_two() {
        let ps = host_page_size();
        assert!(ps >= 4096, "page size unexpectedly small: {ps}");
        assert!(ps.is_power_of_two(), "page size not a power of two: {ps}");
    }

    #[test]
    fn test_pagemap_constants() {
        assert_eq!(PAGEMAP_PRESENT_BIT, 1u64 << 63);
        assert_eq!(PAGEMAP_SWAPPED_BIT, 1u64 << 62);
        assert_eq!(PAGEMAP_FILE_OR_SHARED_ANON_BIT, 1u64 << 61);
    }

    #[test]
    fn test_pagemap_entry_is_cow_anon() {
        assert!(!pagemap_entry_is_cow_anon(0), "not present");
        assert!(
            !pagemap_entry_is_cow_anon(
                PAGEMAP_PRESENT_BIT | PAGEMAP_FILE_OR_SHARED_ANON_BIT | 0x100
            ),
            "present file-backed / shared-anon stays on the reflink base"
        );
        assert!(
            pagemap_entry_is_cow_anon(PAGEMAP_PRESENT_BIT | 0x100),
            "present exclusive anon is CoW"
        );
        assert!(
            pagemap_entry_is_cow_anon(PAGEMAP_SWAPPED_BIT),
            "swapped pages were guest-written"
        );
    }

    /// Coalescing must produce byte offsets/lengths scaled by the *injected*
    /// page size. Running the identical bitmap through 4 KiB and 64 KiB proves
    /// the ARM64 64 KiB path: a hardcoded 4096 would fail the 65536 case.
    #[test]
    fn test_coalesce_pages_to_ranges_page_size_matrix() {
        // Bitmap: pages 1,2 dirty; page 3 clean; page 5 dirty.
        // Expect two ranges: [1..=2] (2 pages) and [5] (1 page).
        let bitmap = [false, true, true, false, false, true];

        for &page_size in &[4096u64, 65536u64] {
            let gpa_base = 0x1000_0000;
            let (ranges, set_pages) = coalesce_pages_to_ranges(gpa_base, &bitmap, page_size);

            assert_eq!(set_pages, 3, "page_size={page_size}: wrong set-page count");

            let got: Vec<(u64, u64)> = ranges.iter().map(|r| (r.gpa, r.length)).collect();
            assert_eq!(
                got,
                vec![
                    (gpa_base + page_size, 2 * page_size),
                    (gpa_base + 5 * page_size, page_size),
                ],
                "page_size={page_size}: ranges must scale with page size"
            );
        }
    }

    #[test]
    fn test_coalesce_pages_to_ranges_all_and_none() {
        for &page_size in &[4096u64, 65536u64] {
            // All clean -> no ranges.
            let (ranges, set) = coalesce_pages_to_ranges(0, &[false; 4], page_size);
            assert!(ranges.is_empty());
            assert_eq!(set, 0);

            // All dirty -> single coalesced range spanning every page.
            let (ranges, set) = coalesce_pages_to_ranges(0, &[true; 4], page_size);
            assert_eq!(set, 4);
            let got: Vec<(u64, u64)> = ranges.iter().map(|r| (r.gpa, r.length)).collect();
            assert_eq!(got, vec![(0, 4 * page_size)]);
        }
    }
}

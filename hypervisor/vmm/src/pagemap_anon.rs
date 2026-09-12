// Copyright © 2026 Tencent Corporation
//
// SPDX-License-Identifier: Apache-2.0

//! Incremental snapshots of Guest-written CoW anonymous pages.
//!
//! A page is saved when `swapped || (present && !PM_FILE)`.
//! Linux 6.6.44+ / 6.11+ / 7+ reads that from pagemap bit 61; older kernels
//! use `/proc/kpageflags` (`KPF_ANON`, needs `CAP_SYS_ADMIN`).
//!
//! Under `MAP_PRIVATE` restore, unread and read-only file pages are skipped;
//! Guest writes become private anon and are saved.

use crate::kernel_release::PagemapScanPath;
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

/// Bit 61: `PM_FILE` (file-backed or shared-anon; private CoW anon has this clear).
const PAGEMAP_FILE_BIT: u64 = 1 << 61;

/// Present-entry PFN. Zero without `CAP_SYS_ADMIN`.
const PAGEMAP_PFN_MASK: u64 = (1 << 55) - 1;

const KPAGEFLAGS_ENTRY_SIZE: u64 = 8;

/// `/proc/kpageflags` bit 12.
const KPF_ANON: u64 = 1 << 12;

/// `true` if this pagemap entry must be written into an incremental snapshot.
pub(crate) fn pagemap_entry_is_cow_anon(entry: u64) -> bool {
    let swapped = (entry & PAGEMAP_SWAPPED_BIT) != 0;
    let present = (entry & PAGEMAP_PRESENT_BIT) != 0;
    let pm_file = (entry & PAGEMAP_FILE_BIT) != 0;
    swapped || (present && !pm_file)
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

    #[error("CAP_SYS_ADMIN is required to read pagemap PFNs for /proc/kpageflags")]
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

/// Per-page CoW-anon bitmap. Bit 61 on new enough kernels, else kpageflags.
pub fn get_anon_pages(host_addr: u64, length: u64) -> Result<Vec<bool>> {
    Ok(scan_anon_pages(host_addr, length)?.0)
}

fn scan_anon_pages(host_addr: u64, length: u64) -> Result<(Vec<bool>, u64)> {
    if PagemapScanPath::cached().use_bit61() {
        scan_pagemap_cow_anon(host_addr, length)
    } else {
        scan_kpageflags_anon(host_addr, length)
    }
}

fn read_pagemap_entries(host_addr: u64, length: u64) -> Result<Vec<u64>> {
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

    Ok(pagemap_buf
        .chunks_exact(PAGEMAP_ENTRY_SIZE as usize)
        .map(|chunk| u64::from_ne_bytes(chunk.try_into().unwrap()))
        .collect())
}

/// Bit-61 scan. Returns (must-save bitmap, swapped-page count).
pub(crate) fn scan_pagemap_cow_anon(host_addr: u64, length: u64) -> Result<(Vec<bool>, u64)> {
    let entries = read_pagemap_entries(host_addr, length)?;
    let mut result = vec![false; entries.len()];
    let mut swapped_pages = 0u64;

    for (item, entry) in result.iter_mut().zip(entries.iter()) {
        if (entry & PAGEMAP_SWAPPED_BIT) != 0 {
            swapped_pages += 1;
        }
        *item = pagemap_entry_is_cow_anon(*entry);
    }

    Ok((result, swapped_pages))
}

/// kpageflags scan. Needs `CAP_SYS_ADMIN`.
pub(crate) fn scan_kpageflags_anon(host_addr: u64, length: u64) -> Result<(Vec<bool>, u64)> {
    let entries = read_pagemap_entries(host_addr, length)?;
    let mut kpageflags =
        File::open("/proc/kpageflags").map_err(|e| PagemapAnonError::OpenFailed {
            path: "/proc/kpageflags".to_string(),
            source: e,
        })?;

    let mut result = vec![false; entries.len()];
    let mut swapped_pages = 0u64;
    let mut flags_buf = [0u8; KPAGEFLAGS_ENTRY_SIZE as usize];

    for (item, entry) in result.iter_mut().zip(entries.into_iter()) {
        if (entry & PAGEMAP_SWAPPED_BIT) != 0 {
            swapped_pages += 1;
            *item = true;
            continue;
        }
        if (entry & PAGEMAP_PRESENT_BIT) == 0 {
            continue;
        }

        let pfn = entry & PAGEMAP_PFN_MASK;
        if pfn == 0 {
            return Err(PagemapAnonError::NoCapSysAdmin);
        }
        kpageflags
            .seek(SeekFrom::Start(pfn * KPAGEFLAGS_ENTRY_SIZE))
            .map_err(|e| PagemapAnonError::SeekFailed {
                path: "/proc/kpageflags".to_string(),
                source: e,
            })?;
        kpageflags
            .read_exact(&mut flags_buf)
            .map_err(|e| PagemapAnonError::ReadFailed {
                path: "/proc/kpageflags".to_string(),
                source: e,
            })?;
        *item = (u64::from_ne_bytes(flags_buf) & KPF_ANON) != 0;
    }

    Ok((result, swapped_pages))
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

        let (anon_pages, swapped_count) = scan_anon_pages(host_addr as u64, length)?;

        // Convert bitmap to memory ranges (merge consecutive anonymous pages)
        let (region_ranges, anon_count) = coalesce_pages_to_ranges(gpa, &anon_pages, page_size);
        stats.anon_pages += anon_count;
        stats.swapped_pages += swapped_count;
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
#[path = "pagemap_anon_bench.rs"]
mod benchmark;

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
        // Verify bit positions are correct
        assert_eq!(PAGEMAP_PRESENT_BIT, 1u64 << 63);
        assert_eq!(PAGEMAP_SWAPPED_BIT, 1u64 << 62);
        assert_eq!(PAGEMAP_FILE_BIT, 1u64 << 61);
    }

    /// Bit 55 is only used here to prove the decoder ignores soft-dirty on
    /// file-backed pages. Soft-dirty tracking itself lives in `soft_dirty.rs`.
    const PAGEMAP_SOFT_DIRTY_BIT: u64 = 1 << 55;

    #[test]
    fn test_pagemap_entry_is_cow_anon_decoder() {
        assert!(
            !pagemap_entry_is_cow_anon(0),
            "zero entry (never mapped) must not be saved"
        );
        assert!(
            pagemap_entry_is_cow_anon(PAGEMAP_PRESENT_BIT),
            "present, non-file → private anon CoW, must save"
        );
        assert!(
            !pagemap_entry_is_cow_anon(PAGEMAP_PRESENT_BIT | PAGEMAP_FILE_BIT),
            "present file-backed page must not be saved"
        );
        assert!(
            pagemap_entry_is_cow_anon(PAGEMAP_SWAPPED_BIT),
            "swapped-out anonymous page must be saved"
        );
        assert!(
            pagemap_entry_is_cow_anon(PAGEMAP_PRESENT_BIT | PAGEMAP_SWAPPED_BIT),
            "present+swap should not occur; still conservative must-save"
        );
        assert!(
            !pagemap_entry_is_cow_anon(
                PAGEMAP_PRESENT_BIT | PAGEMAP_FILE_BIT | PAGEMAP_SOFT_DIRTY_BIT
            ),
            "file page with write-protect tracking is still a file page"
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

    /// MAP_PRIVATE: skip unread/read-only file pages; save CoW writes.
    #[test]
    fn test_get_anon_pages_map_private_file() {
        let fixture = MapPrivateCowFixture::new();
        let (bitmap, _) = scan_pagemap_cow_anon(fixture.host_addr(), fixture.length())
            .expect("scan_pagemap_cow_anon");
        fixture.assert_expected_bitmap(&bitmap);
    }

    /// kpageflags: `NoCapSysAdmin` without privilege; otherwise no under-save vs bit 61.
    #[test]
    fn test_scan_kpageflags_anon_cap_or_matches_bit61() {
        let fixture = MapPrivateCowFixture::new();
        let (bit61, _) = scan_pagemap_cow_anon(fixture.host_addr(), fixture.length())
            .expect("scan_pagemap_cow_anon");
        match scan_kpageflags_anon(fixture.host_addr(), fixture.length()) {
            Err(PagemapAnonError::NoCapSysAdmin) | Err(PagemapAnonError::OpenFailed { .. }) => {}
            Ok((kpf, _)) => {
                let under = bit61
                    .iter()
                    .zip(&kpf)
                    .filter(|(want, got)| **want && !**got)
                    .count();
                assert_eq!(under, 0, "kpageflags under-saved bit61 CoW pages");
                fixture.assert_expected_bitmap(&kpf);
            }
            Err(e) => panic!("unexpected kpageflags error: {e}"),
        }
    }

    struct MapPrivateCowFixture {
        ptr: *mut libc::c_void,
        len: usize,
        num_pages: usize,
    }

    impl MapPrivateCowFixture {
        fn new() -> Self {
            use std::io::Write;
            use std::os::unix::io::AsRawFd;

            let page_size = host_page_size() as usize;
            const NUM_PAGES: usize = 8;
            let len = page_size * NUM_PAGES;

            let path = std::env::temp_dir().join(format!(
                "cube_pagemap_anon_map_private_{}_{}",
                std::process::id(),
                std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .unwrap()
                    .as_nanos()
            ));

            let mut file = std::fs::OpenOptions::new()
                .read(true)
                .write(true)
                .create_new(true)
                .open(&path)
                .expect("create temp file for MAP_PRIVATE fixture");
            let mut contents = vec![0u8; len];
            for page in 0..NUM_PAGES {
                contents[page * page_size] = 0xA0 + page as u8;
            }
            file.write_all(&contents)
                .expect("write pattern into temp file");
            let _ = std::fs::remove_file(&path);

            let ptr = unsafe {
                libc::mmap(
                    std::ptr::null_mut(),
                    len,
                    libc::PROT_READ | libc::PROT_WRITE,
                    libc::MAP_PRIVATE,
                    file.as_raw_fd(),
                    0,
                )
            };
            assert_ne!(
                ptr,
                libc::MAP_FAILED,
                "mmap MAP_PRIVATE failed: {}",
                std::io::Error::last_os_error()
            );

            let base = ptr as *mut u8;
            unsafe {
                let _ = std::ptr::read_volatile(base.add(page_size));
                std::ptr::write_volatile(base.add(2 * page_size), 0xc2);
                std::ptr::write_volatile(base.add(4 * page_size), 0xc4);
            }

            Self {
                ptr,
                len,
                num_pages: NUM_PAGES,
            }
        }

        fn host_addr(&self) -> u64 {
            self.ptr as u64
        }

        fn length(&self) -> u64 {
            self.len as u64
        }

        fn assert_expected_bitmap(&self, bitmap: &[bool]) {
            assert_eq!(bitmap.len(), self.num_pages);
            assert!(!bitmap[0], "untouched page must not be saved");
            assert!(!bitmap[1], "read-only file page must not be saved");
            assert!(bitmap[2], "written CoW page must be saved");
            assert!(!bitmap[3], "untouched page must not be saved");
            assert!(bitmap[4], "written CoW page must be saved");
            for i in 5..self.num_pages {
                assert!(!bitmap[i], "untouched page {i} must not be saved");
            }
        }
    }

    impl Drop for MapPrivateCowFixture {
        fn drop(&mut self) {
            unsafe {
                libc::munmap(self.ptr, self.len);
            }
        }
    }
}

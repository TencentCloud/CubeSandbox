// Copyright © 2026 Tencent Corporation
//
// SPDX-License-Identifier: Apache-2.0

//! Incremental snapshots of Guest-written CoW anonymous pages.
//!
//! A page is saved when `swapped || (present && !PM_FILE)`, all three flags
//! coming from a single `/proc/self/pagemap` entry: bit 63 (`present`), bit 62
//! (`swapped`) and bit 61 (`PM_FILE`). No PFN is involved, so kernel page
//! migration cannot invalidate the classification, and reading the flags needs
//! no `CAP_SYS_ADMIN`.
//!
//! `PM_FILE` is the *negative* signal: the kernel only sets it for pages it
//! reports as non-anonymous (`page && !PageAnon(page)`), so a missing
//! `PM_FILE` is classified as "save". Kernels predating upstream `3f9f022e`
//! (6.6.44 on the 6.6 stable line, ~6.11 mainline) omit `PM_FILE` on
//! PMD-mapped file THPs, which makes those pages be saved unnecessarily: that
//! costs savings, never correctness, and requires file-backed THP (tmpfs with
//! shmem THP explicitly enabled), which Cube hosts do not have
//! (`transparent_hugepage=never` and shmem THP defaults to off).
//!
//! Under `MAP_PRIVATE` restore, unread and read-only file pages are skipped;
//! Guest writes become private anon and are saved.

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

/// Default batch size (in host pages) when streaming `/proc/self/pagemap`:
/// 8,192 entries = 64 KiB raw read buffer (= 32 MiB guest RAM per syscall on
/// 4 KiB hosts, 512 MiB on 64 KiB hosts).
pub(crate) const PAGEMAP_STREAM_BATCH_PAGES: usize = 8192;

/// Bit 63: page is present in RAM
const PAGEMAP_PRESENT_BIT: u64 = 1 << 63;

/// Bit 62: page is in swap
const PAGEMAP_SWAPPED_BIT: u64 = 1 << 62;

/// Bit 61: `PM_FILE` (file-backed or shared-anon; private CoW anon has this clear).
const PAGEMAP_FILE_BIT: u64 = 1 << 61;

/// Mask for any resident page (`PRESENT` or `SWAPPED`).
const PAGEMAP_RESIDENT_MASK: u64 = PAGEMAP_PRESENT_BIT | PAGEMAP_SWAPPED_BIT;

/// `true` if this pagemap entry represents a resident (present or swapped) page.
#[inline]
pub(crate) fn pagemap_entry_is_resident(entry: u64) -> bool {
    (entry & PAGEMAP_RESIDENT_MASK) != 0
}

/// `true` if this pagemap entry must be written into an incremental snapshot.
pub(crate) fn pagemap_entry_is_cow_anon(entry: u64) -> bool {
    let swapped = (entry & PAGEMAP_SWAPPED_BIT) != 0;
    let present = (entry & PAGEMAP_PRESENT_BIT) != 0;
    let pm_file = (entry & PAGEMAP_FILE_BIT) != 0;
    swapped || (present && !pm_file)
}

/// Residency state of a single host virtual page decoded from `/proc/self/pagemap`.
#[cfg_attr(not(test), allow(dead_code))]
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum PageResidency {
    NonPresent,
    Present,
    Swapped,
}

/// Classify a raw `/proc/self/pagemap` entry solely by residency (`PRESENT` or `SWAPPED`).
///
/// Unlike [`pagemap_entry_is_cow_anon`], this ignores `PM_FILE` (bit 61) so
/// `SparseFull` preserves present file-backed/shared pages too.
#[cfg_attr(not(test), allow(dead_code))]
pub(crate) fn classify_pagemap_residency(entry: u64) -> PageResidency {
    if (entry & PAGEMAP_SWAPPED_BIT) != 0 {
        PageResidency::Swapped
    } else if (entry & PAGEMAP_PRESENT_BIT) != 0 {
        PageResidency::Present
    } else {
        PageResidency::NonPresent
    }
}

/// Open `/proc/self/pagemap` once so callers scanning multiple guest memory
/// regions can reuse the same file descriptor.
pub(crate) fn open_pagemap() -> Result<File> {
    File::open("/proc/self/pagemap").map_err(|e| PagemapAnonError::OpenFailed {
        path: "/proc/self/pagemap".to_string(),
        source: e,
    })
}

/// Stream raw `/proc/self/pagemap` entry bytes for `[host_addr, host_addr + length)`
/// in fixed-size batches of up to `batch_pages` entries into `raw_buf`.
///
/// Each invocation of `on_batch(first_page_idx, raw_bytes)` receives a slice of
/// `count * 8` native-endian `u64` entries aligned to `batch_pages` (except for
/// the final trailing batch). Reusing `pagemap_file` and `raw_buf` avoids
/// multi-MiB transient allocations when scanning multi-GiB guest RAM.
pub(crate) fn stream_pagemap_raw_batches<F>(
    pagemap_file: &mut File,
    host_addr: u64,
    length: u64,
    batch_pages: usize,
    raw_buf: &mut Vec<u8>,
    mut on_batch: F,
) -> Result<()>
where
    F: FnMut(usize, &[u8]),
{
    let page_size = host_page_size();
    if host_addr % page_size != 0 {
        return Err(PagemapAnonError::NotPageAligned);
    }

    let total_pages = length.div_ceil(page_size) as usize;
    if total_pages == 0 {
        return Ok(());
    }

    let start_page = host_addr / page_size;
    let pagemap_offset = start_page * PAGEMAP_ENTRY_SIZE;
    pagemap_file
        .seek(SeekFrom::Start(pagemap_offset))
        .map_err(|e| PagemapAnonError::SeekFailed {
            path: "/proc/self/pagemap".to_string(),
            source: e,
        })?;

    let step_pages = batch_pages.max(1).min(total_pages);
    let raw_cap = step_pages * PAGEMAP_ENTRY_SIZE as usize;
    if raw_buf.len() < raw_cap {
        raw_buf.resize(raw_cap, 0);
    }

    let mut page_offset = 0usize;
    while page_offset < total_pages {
        let count = (total_pages - page_offset).min(step_pages);
        let byte_len = count * PAGEMAP_ENTRY_SIZE as usize;
        let slice = &mut raw_buf[..byte_len];
        pagemap_file
            .read_exact(slice)
            .map_err(|e| PagemapAnonError::ReadFailed {
                path: "/proc/self/pagemap".to_string(),
                source: e,
            })?;
        on_batch(page_offset, slice);
        page_offset += count;
    }

    Ok(())
}

/// Scan `/proc/self/pagemap` for `[host_addr, host_addr + length)` and classify
/// every host page's residency.
#[cfg_attr(not(test), allow(dead_code))]
pub(crate) fn scan_pagemap_residency(host_addr: u64, length: u64) -> Result<Vec<PageResidency>> {
    let page_size = host_page_size();
    let total_pages = length.div_ceil(page_size) as usize;
    let mut pagemap_file = open_pagemap()?;
    let mut raw_buf = Vec::new();
    let mut out = Vec::with_capacity(total_pages);
    stream_pagemap_raw_batches(
        &mut pagemap_file,
        host_addr,
        length,
        PAGEMAP_STREAM_BATCH_PAGES,
        &mut raw_buf,
        |_first_page, raw| {
            out.extend(
                raw.chunks_exact(PAGEMAP_ENTRY_SIZE as usize)
                    .map(|c| classify_pagemap_residency(u64::from_ne_bytes(c.try_into().unwrap()))),
            );
        },
    )?;
    Ok(out)
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

/// Per-page CoW-anon bitmap, classified from pagemap bit 61 (`PM_FILE`).
pub fn get_anon_pages(host_addr: u64, length: u64) -> Result<Vec<bool>> {
    Ok(scan_pagemap_cow_anon(host_addr, length)?.0)
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

        let (anon_pages, swapped_count) = scan_pagemap_cow_anon(host_addr as u64, length)?;

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

    #[test]
    fn pagemap_residency_classification_matrix() {
        const SOFT_DIRTY_BIT: u64 = 1 << 55;

        assert_eq!(classify_pagemap_residency(0), PageResidency::NonPresent);
        assert_eq!(
            classify_pagemap_residency(SOFT_DIRTY_BIT),
            PageResidency::NonPresent
        );
        assert_eq!(
            classify_pagemap_residency(PAGEMAP_PRESENT_BIT),
            PageResidency::Present
        );
        assert_eq!(
            classify_pagemap_residency(PAGEMAP_PRESENT_BIT | PAGEMAP_FILE_BIT),
            PageResidency::Present
        );
        assert_eq!(
            classify_pagemap_residency(PAGEMAP_SWAPPED_BIT),
            PageResidency::Swapped
        );
        assert_eq!(
            classify_pagemap_residency(PAGEMAP_PRESENT_BIT | PAGEMAP_SWAPPED_BIT),
            PageResidency::Swapped
        );
    }

    #[test]
    fn scan_pagemap_residency_tracks_touched_vs_untouched_anon_pages() {
        let page_size = host_page_size() as usize;
        let num_pages = 4;
        let len = page_size * num_pages;
        let ptr = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                len,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
                -1,
                0,
            )
        };
        assert_ne!(ptr, libc::MAP_FAILED);

        unsafe {
            std::ptr::write_volatile((ptr as *mut u8).add(page_size), 0x5a);
            std::ptr::write_volatile((ptr as *mut u8).add(3 * page_size), 0);
        }

        let residency = scan_pagemap_residency(ptr as u64, len as u64);
        unsafe {
            libc::munmap(ptr, len);
        }

        let residency = residency.expect("scan_pagemap_residency should succeed");
        assert_eq!(
            residency,
            vec![
                PageResidency::NonPresent,
                PageResidency::Present,
                PageResidency::NonPresent,
                PageResidency::Present,
            ]
        );
    }

    #[test]
    fn stream_pagemap_raw_batches_matches_residency_across_small_batches() {
        let page_size = host_page_size() as usize;
        let num_pages = 7;
        let len = page_size * num_pages;
        let ptr = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                len,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
                -1,
                0,
            )
        };
        assert_ne!(ptr, libc::MAP_FAILED);

        unsafe {
            std::ptr::write_volatile((ptr as *mut u8).add(page_size), 0x11);
            std::ptr::write_volatile((ptr as *mut u8).add(4 * page_size), 0x22);
            std::ptr::write_volatile((ptr as *mut u8).add(6 * page_size), 0x33);
        }

        let mut pagemap_file = open_pagemap().expect("open /proc/self/pagemap");
        let mut raw_buf = Vec::new();
        let mut batches = Vec::new();
        let mut resident = vec![false; num_pages];
        let stream_res = stream_pagemap_raw_batches(
            &mut pagemap_file,
            ptr as u64,
            len as u64,
            2,
            &mut raw_buf,
            |first_page, raw| {
                let count = raw.len() / PAGEMAP_ENTRY_SIZE as usize;
                batches.push((first_page, count));
                for (i, c) in raw.chunks_exact(PAGEMAP_ENTRY_SIZE as usize).enumerate() {
                    let entry = u64::from_ne_bytes(c.try_into().unwrap());
                    resident[first_page + i] = pagemap_entry_is_resident(entry);
                }
            },
        );
        unsafe {
            libc::munmap(ptr, len);
        }

        stream_res.expect("stream_pagemap_raw_batches should succeed");
        assert_eq!(batches, vec![(0, 2), (2, 2), (4, 2), (6, 1)]);
        assert_eq!(resident, vec![false, true, false, false, true, false, true]);
    }
}

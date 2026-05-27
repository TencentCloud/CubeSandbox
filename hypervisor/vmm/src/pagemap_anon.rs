// Copyright © 2026 Tencent Corporation
//
// SPDX-License-Identifier: Apache-2.0

//! PagemapAnon snapshot support
//!
//! This module provides functionality for creating pagemap-anon-based snapshots
//! that only save CoW anonymous pages (pages actually written by the Guest)
//! by inspecting `/proc/self/pagemap` and `/proc/kpageflags`.
//!
//! In a `MAP_PRIVATE` mmap restore scenario:
//! - Pages only read by Guest remain as file-backed page cache (KPF_ANON=0)
//! - Pages written by Guest trigger CoW and become anonymous pages (KPF_ANON=1)
//! - Pages never accessed have no PTE (present=0)
//!
//! This module filters out only the anonymous pages, significantly reducing
//! snapshot size compared to mincore which also saves read-only page cache pages.

use log::{debug, trace};
use std::fs::File;
use std::io::{self, Read, Seek, SeekFrom};
use thiserror::Error;
use vm_memory::{GuestAddress, GuestMemory, GuestMemoryMmap};
use vm_migration::protocol::{MemoryRange, MemoryRangeTable};

/// Page size constant (4KB)
pub const PAGE_SIZE: u64 = 4096;

/// Size of a pagemap entry in bytes
const PAGEMAP_ENTRY_SIZE: u64 = 8;

/// Size of a kpageflags entry in bytes
const KPAGEFLAGS_ENTRY_SIZE: u64 = 8;

/// Bit 63: page is present in RAM
const PAGEMAP_PRESENT_BIT: u64 = 1 << 63;

/// Bit 62: page is in swap
const PAGEMAP_SWAPPED_BIT: u64 = 1 << 62;

/// Mask for PFN (bits 0-54)
const PAGEMAP_PFN_MASK: u64 = (1 << 55) - 1;

/// Bit 12 in kpageflags: KPF_ANON (anonymous page)
const KPF_ANON: u64 = 1 << 12;

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

/// Get the anonymous page bitmap for a memory region by reading
/// `/proc/self/pagemap` and `/proc/kpageflags`.
///
/// # Arguments
/// * `host_addr` - Host virtual address of the memory region (must be page-aligned)
/// * `length` - Length of the memory region in bytes
///
/// # Returns
/// A vector of bools where each bool indicates if the corresponding page
/// is an anonymous page (CoW written by Guest).
pub fn get_anon_pages(host_addr: u64, length: u64) -> Result<Vec<bool>> {
    if host_addr % PAGE_SIZE != 0 {
        return Err(PagemapAnonError::NotPageAligned);
    }

    let num_pages = length.div_ceil(PAGE_SIZE) as usize;
    let start_page = host_addr / PAGE_SIZE;

    // Open /proc/self/pagemap and /proc/kpageflags
    let mut pagemap_file =
        File::open("/proc/self/pagemap").map_err(|e| PagemapAnonError::OpenFailed {
            path: "/proc/self/pagemap".to_string(),
            source: e,
        })?;

    let mut kpageflags_file =
        File::open("/proc/kpageflags").map_err(|e| PagemapAnonError::OpenFailed {
            path: "/proc/kpageflags".to_string(),
            source: e,
        })?;

    // Batch read all pagemap entries for this region
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
    let mut kpageflags_buf = [0u8; KPAGEFLAGS_ENTRY_SIZE as usize];

    for (i, item) in result.iter_mut().enumerate().take(num_pages) {
        let entry_offset = i * PAGEMAP_ENTRY_SIZE as usize;
        let entry = u64::from_ne_bytes(
            pagemap_buf[entry_offset..entry_offset + PAGEMAP_ENTRY_SIZE as usize]
                .try_into()
                .unwrap(),
        );

        let present = (entry & PAGEMAP_PRESENT_BIT) != 0;
        let swapped = (entry & PAGEMAP_SWAPPED_BIT) != 0;

        // Swapped anonymous pages are also Guest-written pages that must be saved.
        // When an anonymous page is swapped out, present=0 but swapped=1.
        if swapped {
            *item = true;
            continue;
        }

        if !present {
            continue;
        }

        let pfn = entry & PAGEMAP_PFN_MASK;
        if pfn == 0 {
            // PFN is zero for a present page — this means we don't have
            // CAP_SYS_ADMIN permission to read PFN from pagemap.
            return Err(PagemapAnonError::NoCapSysAdmin);
        }

        // Read kpageflags for this PFN
        let kpageflags_offset = pfn * KPAGEFLAGS_ENTRY_SIZE;
        kpageflags_file
            .seek(SeekFrom::Start(kpageflags_offset))
            .map_err(|e| PagemapAnonError::SeekFailed {
                path: "/proc/kpageflags".to_string(),
                source: e,
            })?;

        kpageflags_file
            .read_exact(&mut kpageflags_buf)
            .map_err(|e| PagemapAnonError::ReadFailed {
                path: "/proc/kpageflags".to_string(),
                source: e,
            })?;

        let flags = u64::from_ne_bytes(kpageflags_buf);

        // KPF_ANON (bit 12) indicates this is an anonymous page,
        // meaning it was created by CoW when Guest wrote to it.
        if (flags & KPF_ANON) != 0 {
            *item = true;
        }
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

    debug!(
        "Starting pagemap_anon filtering for {} memory regions",
        ranges.regions().len()
    );

    for range in ranges.regions() {
        let gpa = range.gpa;
        let length = range.length;

        stats.total_bytes += length;
        stats.total_pages += length.div_ceil(PAGE_SIZE);

        trace!(
            "Processing memory region: GPA=0x{:x}, length={}",
            gpa,
            length
        );

        // Get host virtual address for this guest physical address
        let host_addr = guest_memory
            .get_host_address(GuestAddress(gpa))
            .map_err(|_| PagemapAnonError::GetHostAddressFailed)?;

        // Get anonymous page bitmap via pagemap + kpageflags
        let anon_pages = get_anon_pages(host_addr as u64, length)?;

        // Convert bitmap to memory ranges (merge consecutive anonymous pages)
        let mut current_range_start: Option<u64> = None;
        let mut current_range_length: u64 = 0;

        for (page_idx, &is_anon) in anon_pages.iter().enumerate() {
            let page_gpa = gpa + (page_idx as u64 * PAGE_SIZE);

            if is_anon {
                stats.anon_pages += 1;
                stats.saved_bytes += PAGE_SIZE;

                if current_range_start.is_none() {
                    current_range_start = Some(page_gpa);
                    current_range_length = PAGE_SIZE;
                } else {
                    current_range_length += PAGE_SIZE;
                }
            } else if let Some(start) = current_range_start.take() {
                // End of an anonymous range
                filtered_ranges.push(MemoryRange {
                    gpa: start,
                    length: current_range_length,
                });
                current_range_length = 0;
            }
        }

        // Don't forget the last range if it ends with anonymous pages
        if let Some(start) = current_range_start {
            filtered_ranges.push(MemoryRange {
                gpa: start,
                length: current_range_length,
            });
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
        // Address not aligned to PAGE_SIZE should return NotPageAligned error
        let result = get_anon_pages(4097, 4096);
        assert!(result.is_err());
        assert!(matches!(
            result.unwrap_err(),
            PagemapAnonError::NotPageAligned
        ));
    }

    #[test]
    fn test_pagemap_constants() {
        // Verify bit positions are correct
        assert_eq!(PAGEMAP_PRESENT_BIT, 1u64 << 63);
        assert_eq!(PAGEMAP_SWAPPED_BIT, 1u64 << 62);
        assert_eq!(PAGEMAP_PFN_MASK, (1u64 << 55) - 1);
        assert_eq!(KPF_ANON, 1u64 << 12);
    }
}

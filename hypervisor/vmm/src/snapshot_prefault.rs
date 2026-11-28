// SPDX-License-Identifier: Apache-2.0

//! Optional preparation of a private snapshot mapping before the guest resumes.

use crate::pagemap_anon::host_page_size;
use crate::GuestRegionMmap;
use std::fs::File;
use std::io::{self, Seek, SeekFrom};
use std::ops::Range;
use std::os::unix::fs::FileTypeExt;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::time::Instant;
use vm_memory::{Address, GuestMemoryRegion};
use vm_migration::protocol::MemoryRange;

pub(crate) const MAX_RANGES: usize = 65_536;
const MAX_WORKERS: usize = 128;
// Speculative reads and private CoW allocations share a per-VM budget. Any
// remaining pages are left to normal demand paging.
const MAX_BYTES: usize = 256 << 20;
const CHUNK_BYTES: usize = 1 << 20;

pub(crate) fn worker_count() -> usize {
    std::fs::read_to_string("/etc/cube/populate-read")
        .ok()
        .and_then(|value| value.trim().parse::<usize>().ok())
        .unwrap_or(0)
        .min(MAX_WORKERS)
}

fn backing_len(mut file: &File) -> io::Result<u64> {
    let metadata = file.metadata()?;
    if !metadata.file_type().is_block_device() {
        return Ok(metadata.len());
    }
    // Linux block devices have st_size == 0. The mapping fds are dup'ed
    // from this file, so restore their shared cursor after querying capacity.
    let position = file.stream_position()?;
    let length = file.seek(SeekFrom::End(0))?;
    file.seek(SeekFrom::Start(position))?;
    Ok(length)
}

// The caller supplies spans sorted by start. Both address spaces use half-open ranges.
fn append_range<T: Copy + Ord>(ranges: &mut Vec<Range<T>>, next: Range<T>) {
    if let Some(last) = ranges.last_mut() {
        if next.start <= last.end {
            last.end = last.end.max(next.end);
            return;
        }
    }
    ranges.push(next);
}

pub(super) fn guest_ranges(hints: &[MemoryRange]) -> Vec<Range<u64>> {
    let mut ranges: Vec<_> = hints
        .iter()
        .filter(|hint| hint.length != 0)
        .filter_map(|hint| Some(hint.gpa..hint.gpa.checked_add(hint.length)?))
        .collect();
    ranges.sort_unstable_by_key(|range| range.start);
    let mut merged = Vec::new();
    for range in ranges {
        append_range(&mut merged, range);
    }
    merged
}

// Offset zero in a mapping corresponds to its guest base, which need not
// have the same alignment as the host's page. Avoid partial pages at EOF.
fn mapping_ranges(
    hints: &[Range<u64>],
    guest_base: u64,
    available: u64,
    page: u64,
) -> Vec<Range<usize>> {
    let limit = available / page * page;
    let end = guest_base.saturating_add(limit);
    let first = hints.partition_point(|range| range.end <= guest_base);
    let mut merged = Vec::new();
    for hint in hints[first..].iter().take_while(|hint| hint.start < end) {
        let start = hint.start.saturating_sub(guest_base) / page * page;
        let stop = (hint.end - guest_base).min(limit).div_ceil(page) * page;
        if start < stop {
            append_range(&mut merged, start as usize..stop as usize);
        }
    }
    merged
}

// A private page already populated for write needs no additional read.
fn read_only(reads: &[Range<usize>], writes: &[Range<usize>]) -> Vec<Range<usize>> {
    let mut result = Vec::new();
    for read in reads {
        let mut cursor = read.start;
        let first = writes.partition_point(|write| write.end <= cursor);
        for write in writes[first..].iter().take_while(|w| w.start < read.end) {
            if cursor < write.start {
                result.push(cursor..write.start);
            }
            cursor = write.end;
        }
        if cursor < read.end {
            result.push(cursor..read.end);
        }
    }
    result
}

struct Job<'a> {
    region: &'a GuestRegionMmap,
    offset: usize,
    length: usize,
    advice: i32,
}

/// Split long runs so several workers can populate one contiguous mapping.
fn add_jobs<'a>(
    jobs: &mut Vec<Job<'a>>,
    region: &'a GuestRegionMmap,
    spans: &[Range<usize>],
    advice: i32,
    remaining: &mut usize,
) {
    for span in spans {
        let mut offset = span.start;
        while offset < span.end && *remaining > 0 {
            let length = (span.end - offset).min(CHUNK_BYTES).min(*remaining);
            jobs.push(Job {
                region,
                offset,
                length,
                advice,
            });
            *remaining -= length;
            offset += length;
        }
        if *remaining == 0 {
            break;
        }
    }
}

// Assign each job to one worker by its index. If spawning a worker fails,
// the caller processes that worker's jobs.
fn for_each_job<T: Sync>(
    jobs: &[T],
    workers: usize,
    failed: &AtomicBool,
    run: impl Fn(&T) + Sync,
) -> usize {
    std::thread::scope(|scope| {
        let run = &run;
        let mut spawned = 0;
        let mut inline = false;
        for lane in 0..workers {
            let run_lane = move || {
                for job in jobs.iter().skip(lane).step_by(workers) {
                    if failed.load(Ordering::Relaxed) {
                        break;
                    }
                    run(job);
                }
            };
            if std::thread::Builder::new()
                .spawn_scoped(scope, run_lane)
                .is_ok()
            {
                spawned += 1;
            } else {
                inline = true;
                run_lane();
            }
        }
        if inline {
            spawned + 1
        } else {
            spawned
        }
    })
}

/// Regions are private mappings of the same snapshot file. A failed or
/// unsupported madvise only skips speculative work; restore can proceed.
pub(crate) fn populate(
    regions: &[(&GuestRegionMmap, u64)],
    file: &File,
    writes: &[MemoryRange],
    reads: &[MemoryRange],
    workers: usize,
) {
    if workers == 0 || regions.is_empty() || (writes.is_empty() && reads.is_empty()) {
        return;
    }
    if writes.len().saturating_add(reads.len()) > MAX_RANGES {
        warn!("Skipping overly fragmented snapshot prefault hints");
        return;
    }
    let file_len = match backing_len(file) {
        Ok(size) => size,
        Err(error) => {
            warn!("Cannot determine snapshot size for prefault: {error}");
            return;
        }
    };
    let started = Instant::now();
    let page = host_page_size();
    let writes = guest_ranges(writes);
    let reads = guest_ranges(reads);
    let mut jobs = Vec::new();
    let mut remaining = MAX_BYTES;
    // Spend the budget on writes first, including when the guest has multiple memory zones.
    for advice in [libc::MADV_POPULATE_WRITE, libc::MADV_POPULATE_READ] {
        for (region, file_offset) in regions {
            if remaining == 0 {
                break;
            }
            if region.flags() & libc::MAP_PRIVATE == 0 || file_offset % page != 0 {
                continue;
            }
            let gpa = region.start_addr().raw_value();
            let available = region.len().min(file_len.saturating_sub(*file_offset));
            let write_pages = mapping_ranges(&writes, gpa, available, page);
            let spans = if advice == libc::MADV_POPULATE_WRITE {
                write_pages
            } else {
                read_only(&mapping_ranges(&reads, gpa, available, page), &write_pages)
            };
            add_jobs(&mut jobs, region, &spans, advice, &mut remaining);
        }
    }
    let workers = workers.min(MAX_WORKERS).min(jobs.len());
    if workers == 0 {
        return;
    }
    let failed = AtomicBool::new(false);
    let read_bytes = AtomicUsize::new(0);
    let write_bytes = AtomicUsize::new(0);
    let actual_workers = for_each_job(&jobs, workers, &failed, |job| {
        // SAFETY: each job covers complete, disjoint pages within a live
        // mmap. The scoped workers join before any mapping can be dropped.
        let rc = unsafe {
            libc::madvise(
                job.region.as_ptr().add(job.offset).cast(),
                job.length,
                job.advice,
            )
        };
        if rc == 0 {
            let successful = if job.advice == libc::MADV_POPULATE_WRITE {
                &write_bytes
            } else {
                &read_bytes
            };
            successful.fetch_add(job.length, Ordering::Relaxed);
        } else if !failed.swap(true, Ordering::Relaxed) {
            warn!(
                "Skipping remaining snapshot prefaults: {}",
                io::Error::last_os_error()
            );
        }
    });
    info!(
        "Snapshot prefault: planned_bytes={} workers={} failed={} elapsed_us={} successful_read_bytes={} successful_write_bytes={} chunks={}",
        MAX_BYTES - remaining,
        actual_workers,
        failed.load(Ordering::Relaxed),
        started.elapsed().as_micros(),
        read_bytes.load(Ordering::Relaxed),
        write_bytes.load(Ordering::Relaxed),
        jobs.len()
    );
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;
    use std::os::unix::fs::FileExt;
    use vm_memory::{FileOffset, GuestAddress, MmapRegion};
    use vmm_sys_util::tempfile::TempFile;

    fn hint(gpa: u64, length: u64) -> MemoryRange {
        MemoryRange { gpa, length }
    }

    #[test]
    fn strided_workers_cover_unequal_lanes_once_and_honor_an_early_stop() {
        use std::sync::Mutex;
        for (jobs, workers) in [(1, 1), (5, 4), (23, 4)] {
            let calls: Vec<_> = (0..jobs).map(|_| Mutex::new(Vec::new())).collect();
            let failed = AtomicBool::new(false);
            let n = for_each_job(&calls, workers, &failed, |record| {
                record.lock().unwrap().push(std::thread::current().id());
            });
            assert!((1..=workers).contains(&n));
            for (index, record) in calls.iter().enumerate() {
                let observed = record.lock().unwrap().clone();
                assert_eq!(observed.len(), 1);
                assert_eq!(observed[0], calls[index % workers].lock().unwrap()[0]);
            }
        }
        let failed = AtomicBool::new(false);
        let seen = AtomicUsize::new(0);
        for_each_job(&[1, 2, 3, 4], 1, &failed, |_| {
            seen.fetch_add(1, Ordering::Relaxed);
            failed.store(true, Ordering::Relaxed);
        });
        assert_eq!(seen.load(Ordering::Relaxed), 1);
    }

    #[test]
    fn guest_ranges_merge_unsorted_hints_and_skip_overflows() {
        assert_eq!(
            guest_ranges(&[
                hint(8192, 4096),
                hint(u64::MAX, 1),
                hint(2048, 2048),
                hint(0, 4096),
                hint(16384, 0),
                hint(1024, 3072),
            ]),
            vec![0..4096, 8192..12288]
        );
    }

    #[test]
    fn plan_deduplicates_by_host_page_and_respects_mapping_and_eof() {
        for page in [4096, 16384, 65536] {
            let gpa = 4096;
            let writes = guest_ranges(&[hint(0, gpa + 1), hint(gpa + 2 * page, 4)]);
            let reads = guest_ranges(&[hint(gpa, 10 * page)]);
            // Three whole pages are available at this mapping's file offset.
            let write_pages = mapping_ranges(&writes, gpa, 4 * page - 1, page);
            let read_pages = mapping_ranges(&reads, gpa, 4 * page - 1, page);
            let p = page as usize;
            assert_eq!(write_pages, vec![0..p, 2 * p..3 * p]);
            assert_eq!(read_only(&read_pages, &write_pages), vec![p..2 * p]);
            assert!(mapping_ranges(&reads, gpa, page - 1, page).is_empty());
        }
    }

    fn file_region(file: &File, offset: u64, size: usize) -> GuestRegionMmap {
        let mapping = MmapRegion::build(
            Some(FileOffset::new(file.try_clone().unwrap(), offset)),
            size,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_PRIVATE,
        )
        .unwrap();
        GuestRegionMmap::new(mapping, GuestAddress(0)).unwrap()
    }

    #[test]
    fn population_jobs_respect_the_remaining_budget() {
        let file = TempFile::new().unwrap().into_file();
        let length = 4 * CHUNK_BYTES;
        file.set_len(length as u64).unwrap();
        let region = file_region(&file, 0, length);
        let mut jobs = Vec::new();
        let mut remaining = CHUNK_BYTES + CHUNK_BYTES / 2;
        let spans = [0..length / 2, 3 * CHUNK_BYTES..length];
        add_jobs(
            &mut jobs,
            &region,
            &spans,
            libc::MADV_POPULATE_WRITE,
            &mut remaining,
        );
        assert_eq!(remaining, 0);
        assert_eq!(jobs.len(), 2);
        assert_eq!((jobs[0].offset, jobs[0].length), (0, CHUNK_BYTES));
        assert_eq!(
            (jobs[1].offset, jobs[1].length),
            (CHUNK_BYTES, CHUNK_BYTES / 2)
        );
    }

    #[test]
    fn populate_prepares_private_writes_and_preserves_shared_file_pages() {
        // Linux before 5.14 simply leaves restore on the lazy path.
        // SAFETY: a zero-length advice probe accesses no memory.
        if unsafe { libc::madvise(std::ptr::null_mut(), 0, libc::MADV_POPULATE_WRITE) } != 0 {
            return;
        }
        let p = host_page_size() as usize;
        let mut file = TempFile::new().unwrap().into_file();
        file.write_all(&vec![0x5c; 3 * p]).unwrap();
        let region = file_region(&file, p as u64, 2 * p);
        let start_position = file.seek(SeekFrom::Start(17)).unwrap();
        let regions = [(&region, p as u64)];
        let writes = [hint(0, p as u64)];
        let reads = [hint(0, (3 * p) as u64)];
        let anon =
            || crate::pagemap_anon::get_anon_pages(region.as_ptr() as u64, (2 * p) as u64).unwrap();

        populate(&regions, &file, &writes, &reads, 0);
        assert_eq!(anon(), vec![false, false]);
        populate(&regions, &file, &writes, &reads, 2);
        assert_eq!(anon(), vec![true, false]);
        // The second page was read-prefaulted, rather than left untouched.
        let mut entry = [0u8; 8];
        let second_page = region.as_ptr() as u64 / p as u64 + 1;
        File::open("/proc/self/pagemap")
            .unwrap()
            .read_exact_at(&mut entry, second_page * 8)
            .unwrap();
        assert_ne!(u64::from_ne_bytes(entry) >> 63, 0);
        assert_eq!(file.stream_position().unwrap(), start_position);

        let mut contents = vec![0; 3 * p];
        file.read_exact_at(&mut contents, 0).unwrap();
        assert!(contents.iter().all(|byte| *byte == 0x5c));
    }
}

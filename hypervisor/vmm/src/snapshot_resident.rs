// SPDX-License-Identifier: Apache-2.0

//! Verify that Pause can populate the destination cache from frozen guest memory.
//! Cubelet owns the fresh clone until publication and keeps the restore base immutable.
use crate::pagemap_anon::host_page_size;
use crate::GuestMemoryMmap;
use std::fs::File;
use std::io;
use std::os::fd::AsRawFd;
use std::os::unix::fs::MetadataExt;
use std::sync::OnceLock;
use vm_memory::{Address, GuestAddress, GuestMemory, GuestMemoryRegion};
use vm_migration::protocol::{MemoryRange, MemoryRangeTable};

const MAX_BYTES: u64 = 256 << 20;
const MAX_RANGES: usize = 8192;
const EXTENTS: usize = 512;
const MAX_EXTENTS: usize = 32_768;
const SHARED: u32 = 0x2000;
const LAST: u32 = 1;
// Linux UAPI: _IOWR('f', 11, struct fiemap); sizeof(struct fiemap) == 32.
// SYS_cachestat has number 451 on both supported VMM targets (x86_64/aarch64).
pub(crate) const FIEMAP: u64 = 0xc020660b;
// _IOR('X', 31, struct fsxattr); sizeof(struct fsxattr) == 28.
pub(crate) const FSGETXATTR: u64 = 0x801c581f;
pub(crate) const CACHESTAT: i64 = 451;

pub(crate) fn enabled() -> bool {
    // The seccomp rules and the code that issues these syscalls must agree
    // throughout the lifetime of this VMM, even when the host switch changes.
    static ENABLED: OnceLock<bool> = OnceLock::new();
    *ENABLED.get_or_init(|| {
        std::fs::read_to_string("/etc/cube/snapshot-resident-pages")
            .is_ok_and(|value| value.trim() == "1")
    })
}

pub(crate) fn regular_file_id(file: &File) -> io::Result<Option<(u64, u64)>> {
    let meta = file.metadata()?;
    Ok(meta.is_file().then_some((meta.dev(), meta.ino())))
}

fn skip(reason: &'static str) -> io::Error {
    io::Error::other(reason)
}

#[repr(C)]
#[derive(Default, Clone, Copy)]
struct Extent {
    logical: u64,
    physical: u64,
    len: u64,
    reserved64: [u64; 2],
    flags: u32,
    reserved32: [u32; 3],
}

#[repr(C)]
struct ExtentQuery {
    start: u64,
    len: u64,
    flags: u32,
    mapped: u32,
    count: u32,
    reserved: u32,
    extents: [Extent; EXTENTS],
}

// Reject dirty or writeback data before inspecting physical mappings. Buffered
// modifications can differ from the still-shared extent reported by FIEMAP.
// Skip these files without forcing writeback on the Pause path.
fn clean_stats(stats: &[u64; 5]) -> io::Result<()> {
    if stats[1] != 0 || stats[2] != 0 {
        return Err(skip("dirty-or-writeback"));
    }
    Ok(())
}

fn require_clean(file: &File, len: u64) -> io::Result<()> {
    let range = [0_u64, len];
    let mut stats = [0_u64; 5];
    // SAFETY: range and stats have the Linux cachestat UAPI layout.
    if unsafe {
        libc::syscall(
            CACHESTAT,
            file.as_raw_fd(),
            range.as_ptr(),
            stats.as_mut_ptr(),
            0_u32,
        )
    } != 0
    {
        return Err(io::Error::last_os_error());
    }
    clean_stats(&stats)
}

fn extents(file: &File, len: u64) -> io::Result<Vec<Extent>> {
    // Heap allocation avoids a large stack temporary on VMM threads.
    let mut storage = vec![0_u64; std::mem::size_of::<ExtentQuery>() / 8];
    // SAFETY: this repr(C) structure has u64 alignment, a size divisible by eight,
    // and only integer fields, all valid when zero. The storage outlives query.
    let query = unsafe { &mut *storage.as_mut_ptr().cast::<ExtentQuery>() };
    let mut all = Vec::new();
    let mut cursor = 0;
    while cursor < len {
        query.start = cursor;
        query.len = len - cursor;
        query.count = EXTENTS as u32;
        query.mapped = 0;
        // SAFETY: the header names the capacity of its initialized extent array.
        if unsafe { libc::ioctl(file.as_raw_fd(), FIEMAP as _, &mut *query) } != 0 {
            return Err(io::Error::last_os_error());
        }
        let entries = query
            .extents
            .get(..query.mapped as usize)
            .ok_or_else(|| skip("invalid-fiemap-count"))?;
        if entries.is_empty() {
            break;
        }
        if all.len() + entries.len() > MAX_EXTENTS {
            return Err(skip("too-many-extents"));
        }
        for e in entries {
            if e.len == 0 || e.logical < cursor || e.physical.checked_add(e.len).is_none() {
                return Err(skip("invalid-fiemap-extent"));
            }
            cursor = e
                .logical
                .checked_add(e.len)
                .ok_or_else(|| skip("invalid-fiemap-offset"))?;
            all.push(*e);
        }
        if entries.last().is_some_and(|e| e.flags & LAST != 0) {
            break;
        }
    }
    Ok(all)
}

// Compare a canonical representation of all logical bytes up to EOF. XFS
// holes and stable unwritten extents read as zero. For initialized extents
// require actual same-device shared block addresses, not just SHARED flags.
fn allocated_map(entries: &[Extent], len: u64) -> io::Result<Vec<(u64, u64, u64)>> {
    const UNWRITTEN: u32 = 0x0800;
    let mut map: Vec<(u64, u64, u64)> = Vec::new();
    for e in entries {
        if e.flags & !(SHARED | LAST | UNWRITTEN) != 0 {
            return Err(skip("unstable-fiemap-extent"));
        }
        if e.flags & UNWRITTEN != 0 || e.logical >= len {
            continue;
        }
        if e.physical == 0 || e.flags & SHARED == 0 {
            return Err(skip("nonshared-data-extent"));
        }
        let size = e.len.min(len - e.logical);
        if let Some((logical, physical, length)) = map.last_mut() {
            if *logical + *length == e.logical && *physical + *length == e.physical {
                *length += size;
                continue;
            }
        }
        map.push((e.logical, e.physical, size));
    }
    Ok(map)
}

fn require_xfs(file: &File) -> io::Result<()> {
    let mut stat = std::mem::MaybeUninit::<libc::statfs>::uninit();
    // SAFETY: fstatfs initializes stat on success.
    if unsafe { libc::fstatfs(file.as_raw_fd(), stat.as_mut_ptr()) } != 0 {
        return Err(io::Error::last_os_error());
    }
    if unsafe { stat.assume_init() }.f_type != 0x58465342 {
        return Err(skip("non-xfs"));
    }
    Ok(())
}

// XFS realtime and data-device physical offsets are in separate namespaces.
fn require_xfs_data_inode(file: &File) -> io::Result<()> {
    let mut attr = [0_u32; 7];
    // SAFETY: the kernel writes a 28-byte fsxattr; its first u32 is fsx_xflags.
    if unsafe { libc::ioctl(file.as_raw_fd(), FSGETXATTR as _, attr.as_mut_ptr()) } != 0 {
        return Err(io::Error::last_os_error());
    }
    if attr[0] & (0x0001 | 0x8000) != 0 {
        return Err(skip("realtime-or-dax-inode"));
    }
    Ok(())
}

/// Both held descriptors must identify the complete same-byte XFS image.
pub(crate) fn verify_clone(
    memory: &GuestMemoryMmap,
    ranges: &MemoryRangeTable,
    source: (u64, u64),
    dst: &File,
) -> io::Result<()> {
    let target = dst.metadata()?;
    if !target.is_file()
        || target.dev() != source.0
        || target.ino() == source.1
        || target.nlink() != 1
    {
        return Err(skip("target-not-independent-regular-file"));
    }
    require_xfs(dst)?;
    let ranges = ranges.regions();
    let first = ranges.first().ok_or_else(|| skip("empty-layout"))?;
    let first_region = memory
        .find_region(GuestAddress(first.gpa))
        .ok_or_else(|| skip("missing-source-region"))?;
    let src = first_region
        .file_offset()
        .ok_or_else(|| skip("missing-source-fd"))?
        .file();
    if src.metadata()?.len() != target.len() {
        return Err(skip("base-target-length-mismatch"));
    }
    require_xfs_data_inode(src)?;
    require_xfs_data_inode(dst)?;
    let mut total = 0_u64;
    for r in ranges {
        let region = memory
            .find_region(GuestAddress(r.gpa))
            .ok_or_else(|| skip("unmapped-region"))?;
        let backing = region
            .file_offset()
            .ok_or_else(|| skip("unbacked-region"))?;
        if r.length == 0
            || r.length % host_page_size() != 0
            || r.gpa.checked_add(r.length).is_none()
            || region.start_addr().raw_value() != r.gpa
            || region.len() != r.length
            || region.flags() & libc::MAP_TYPE != libc::MAP_PRIVATE
            || backing.start() != total
            || regular_file_id(backing.file())? != Some(source)
        {
            return Err(skip("non-private-or-mismatched-layout"));
        }
        total = total
            .checked_add(r.length)
            .ok_or_else(|| skip("layout-overflow"))?;
    }
    if total == 0 || total > target.len() {
        return Err(skip("layout-outside-file"));
    }
    let len = target.len();
    require_clean(src, len)?;
    require_clean(dst, len)?;
    let a = allocated_map(&extents(src, len)?, len)?;
    let b = allocated_map(&extents(dst, len)?, len)?;
    if a.is_empty() || a != b {
        return Err(skip("different-or-unrelated-files"));
    }
    Ok(())
}

/// Admit only captured complete host pages. A byte and range budget bound
/// the extra guest data selected for a single Pause.
pub(crate) fn budget(ranges: &[MemoryRange]) -> impl Iterator<Item = MemoryRange> + '_ {
    ranges.iter().take(MAX_RANGES).scan(MAX_BYTES, |left, r| {
        if *left == 0 {
            return None;
        }
        let length = r.length.min(*left);
        *left -= length;
        Some(MemoryRange { gpa: r.gpa, length })
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs::OpenOptions;
    use std::os::unix::fs::FileExt;
    use std::path::Path;
    use vm_memory::{FileOffset, GuestRegionMmap, MmapRegion};
    use vmm_sys_util::tempfile::TempFile;

    fn extent(logical: u64, physical: u64, len: u64, flags: u32) -> Extent {
        Extent {
            logical,
            physical,
            len,
            flags,
            ..Default::default()
        }
    }

    #[test]
    fn complete_physical_map_checks_holes_offsets_and_extent_flags() {
        let src = [extent(0, 128, 8, SHARED), extent(16, 256, 8, SHARED | LAST)];
        let clone = [
            extent(0, 128, 2, SHARED),
            extent(2, 130, 6, SHARED),
            extent(8, 512, 8, 0x0800),
            extent(16, 256, 8, SHARED | LAST),
        ];
        let expected = allocated_map(&src, 20).unwrap();
        assert_eq!(expected, [(0, 128, 8), (16, 256, 4)]);
        assert_eq!(allocated_map(&clone, 20).unwrap(), expected);
        assert_ne!(
            allocated_map(&[extent(4, 128, 4, SHARED)], 20).unwrap(),
            expected
        );
        assert_ne!(
            allocated_map(&[extent(0, 130, 8, SHARED)], 20).unwrap(),
            expected
        );
        for flags in [0, SHARED | 4, SHARED | 0x100, 0x0800 | 4] {
            assert!(allocated_map(&[extent(0, 128, 8, flags)], 20).is_err());
        }
        assert!(clean_stats(&[3, 0, 0, 1, 1]).is_ok());
        assert!(clean_stats(&[3, 1, 0, 1, 1]).is_err());
        assert!(clean_stats(&[3, 0, 1, 1, 1]).is_err());
        assert_eq!(std::mem::size_of::<Extent>(), 56);
        assert_eq!(std::mem::size_of::<ExtentQuery>(), 32 + EXTENTS * 56);
    }

    #[test]
    fn writes_have_byte_and_range_limits() {
        let ranges = [
            MemoryRange {
                gpa: 0,
                length: MAX_BYTES - 4096,
            },
            MemoryRange {
                gpa: MAX_BYTES,
                length: 8192,
            },
            MemoryRange {
                gpa: MAX_BYTES + 8192,
                length: 4096,
            },
        ];
        assert_eq!(
            budget(&ranges).map(|r| r.length).collect::<Vec<_>>(),
            [MAX_BYTES - 4096, 4096]
        );
        let singles = vec![
            MemoryRange {
                gpa: 0,
                length: 4096
            };
            MAX_RANGES + 1
        ];
        assert_eq!(budget(&singles).count(), MAX_RANGES);
    }

    fn memory_for(file: &File, size: usize) -> GuestMemoryMmap {
        let mapping = MmapRegion::build(
            Some(FileOffset::new(file.try_clone().unwrap(), 0)),
            size,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_PRIVATE,
        )
        .unwrap();
        let region = GuestRegionMmap::new(mapping, GuestAddress(0)).unwrap();
        vm_memory::GuestMemoryMmap::from_regions(vec![region]).unwrap()
    }

    #[test]
    fn positioned_writes_preserve_dirty_data_skip_overlap_and_report_io_failures() {
        use crate::memory_manager::write_verified_snapshot;
        use std::io::{Seek, SeekFrom};
        let page = host_page_size() as usize;
        let size = 4 * page;
        let base = TempFile::new().unwrap().into_file();
        base.set_len(size as u64).unwrap();
        base.write_all_at(&vec![65; size], 0).unwrap();
        let memory = memory_for(&base, size);
        use vm_memory::Bytes;
        memory
            .write_slice(&vec![73; page], GuestAddress((2 * page) as u64))
            .unwrap();
        let output = TempFile::new().unwrap().into_file();
        output.write_all_at(&vec![42; size], 0).unwrap();
        let mut fd = &output;
        fd.seek(SeekFrom::Start(9)).unwrap();
        let p = page as u64;
        let layout = [(0, size as u64, 0)];
        let read = [
            MemoryRange { gpa: 0, length: p },
            MemoryRange {
                gpa: p,
                length: 2 * p,
            },
            MemoryRange {
                gpa: 3 * p,
                length: p,
            },
        ];
        let dirty = [MemoryRange {
            gpa: 2 * p,
            length: p,
        }];
        assert_eq!(
            write_verified_snapshot(&memory, &output, &read, &dirty, &layout).unwrap(),
            (2 * p, 2, 2)
        );
        let mut actual = vec![0; size];
        output.read_exact_at(&mut actual, 0).unwrap();
        assert!(actual[..page].iter().all(|v| *v == 65));
        assert!(actual[page..2 * page].iter().all(|v| *v == 42));
        assert!(actual[2 * page..3 * page].iter().all(|v| *v == 73));
        assert!(actual[3 * page..].iter().all(|v| *v == 65));
        let bad = [MemoryRange {
            gpa: 3 * p,
            length: 2 * p,
        }];
        assert!(write_verified_snapshot(&memory, &output, &bad, &[], &layout).is_err());
        let full = OpenOptions::new().write(true).open("/dev/full").unwrap();
        assert!(write_verified_snapshot(&memory, &full, &read, &[], &layout).is_err());
    }

    #[test]
    fn reverse_zone_order_preserves_dirty_runs_and_writes_disjoint_neighbors() {
        use crate::memory_manager::write_verified_snapshot;
        use vm_memory::Bytes;

        let p = host_page_size();
        let zone = 6 * p;
        let high = 16 * p;
        let guest: GuestMemoryMmap = vm_memory::GuestMemoryMmap::from_ranges(&[
            (GuestAddress(0), zone as usize),
            (GuestAddress(high), zone as usize),
        ])
        .unwrap();
        guest
            .write_slice(&vec![0x51; zone as usize], GuestAddress(0))
            .unwrap();
        guest
            .write_slice(&vec![0x62; zone as usize], GuestAddress(high))
            .unwrap();
        let layout = [(high, zone, 0), (0, zone, zone)];
        let span = |gpa, length| MemoryRange { gpa, length };
        let dirty = [
            span(high + 2 * p, 2 * p),
            span(high + 2 * p, p),
            span(2 * p, 2 * p),
        ];
        // Both crossing directions overlap dirty pages. Exact boundary
        // neighbors remain eligible. Keep high GPA first in the on-disk layout.
        let resident = [
            span(high, 2 * p),
            span(high + p, 2 * p),
            span(high + 3 * p, 2 * p),
            span(high + 4 * p, 2 * p),
            span(0, 2 * p),
            span(p, 2 * p),
            span(3 * p, 2 * p),
            span(4 * p, 2 * p),
        ];
        let output = TempFile::new().unwrap().into_file();
        output
            .write_all_at(&vec![0x88; (2 * zone) as usize], 0)
            .unwrap();
        let result = write_verified_snapshot(&guest, &output, &resident, &dirty, &layout).unwrap();
        // The test intentionally includes a duplicate mandatory run.
        assert_eq!((result.0, result.1), (8 * p, 4));
        let mut expected = vec![0x62; (2 * zone) as usize];
        expected[zone as usize..].fill(0x51);
        let mut actual = vec![0; expected.len()];
        output.read_exact_at(&mut actual, 0).unwrap();
        assert_eq!(actual, expected);
    }

    #[test]
    fn coalesced_writes_stop_at_holes_and_adjacent_guest_zones() {
        use crate::memory_manager::write_verified_snapshot;
        use vm_memory::Bytes;

        let p = host_page_size();
        let size = (6 * p) as usize;
        let guest: GuestMemoryMmap = vm_memory::GuestMemoryMmap::from_ranges(&[
            (GuestAddress(0), (3 * p) as usize),
            (GuestAddress(3 * p), (3 * p) as usize),
        ])
        .unwrap();
        let span = |page: u64| MemoryRange {
            gpa: page * p,
            length: p,
        };
        guest
            .write_slice(&vec![19; (3 * p) as usize], GuestAddress(0))
            .unwrap();
        guest
            .write_slice(&vec![71; (3 * p) as usize], GuestAddress(3 * p))
            .unwrap();
        let target = TempFile::new().unwrap().into_file();
        target.write_all_at(&vec![23; size], 0).unwrap();
        // Guest and file addresses both touch at page 3, but the VMAs differ.
        // Page 4 is a real hole in the input; the pre-existing byte must survive.
        let result = write_verified_snapshot(
            &guest,
            &target,
            &[span(0), span(2), span(5)],
            &[span(1), span(3)],
            &[(0, 3 * p, 0), (3 * p, 3 * p, 3 * p)],
        )
        .unwrap();
        assert_eq!(result, (3 * p, 3, 3));
        let mut expected = vec![19; size];
        expected[(3 * p) as usize..].fill(71);
        expected[(4 * p) as usize..(5 * p) as usize].fill(23);
        let mut actual = vec![0; size];
        target.read_exact_at(&mut actual, 0).unwrap();
        assert_eq!(actual, expected);
    }

    #[test]
    #[ignore = "requires Linux >= 6.5 and SNAPSHOT_RESIDENT_TEST_DIR on XFS with reflink"]
    fn xfs_clone_proof_and_sparse_resident_round_trip() {
        use crate::memory_manager::write_verified_snapshot;
        use crate::pagemap_anon::scan_frozen_pages;
        let root =
            std::env::var_os("SNAPSHOT_RESIDENT_TEST_DIR").expect("private XFS test directory");
        let dir = Path::new(&root);
        let create = |suffix: &str| {
            let path = dir.join(format!("equivalent-{}-{}", std::process::id(), suffix));
            let fd = OpenOptions::new()
                .read(true)
                .write(true)
                .create_new(true)
                .open(&path)
                .unwrap();
            (path, fd)
        };
        let (ap, a) = create("source");
        let (bp, b) = create("clone");
        let (cp, c) = create("unrelated");
        let page = host_page_size() as usize;
        let size = 8 * page;
        a.set_len(size as u64).unwrap();
        a.write_all_at(&vec![43; 3 * page], 0).unwrap();
        a.sync_all().unwrap();
        // SAFETY: FICLONE operates on the test-owned descriptors.
        assert_eq!(
            unsafe { libc::ioctl(b.as_raw_fd(), 0x40049409, a.as_raw_fd()) },
            0
        );
        c.set_len(size as u64).unwrap();
        let memory = memory_for(&a, size);
        let gpa = memory.get_host_address(GuestAddress(0)).unwrap();
        // SAFETY: all offsets are inside the live MAP_PRIVATE guest mapping.
        unsafe {
            assert_eq!(std::ptr::read_volatile(gpa), 43);
            std::ptr::write_volatile(gpa.add(page), 73);
            assert_eq!(std::ptr::read_volatile(gpa.add(6 * page)), 0);
        }
        let source = regular_file_id(&a).unwrap().unwrap();
        let mut layout = MemoryRangeTable::default();
        layout.push(MemoryRange {
            gpa: 0,
            length: size as u64,
        });
        assert!(verify_clone(&memory, &layout, source, &a).is_err());
        assert!(verify_clone(&memory, &layout, source, &c).is_err());
        let link = dir.join(format!("equivalent-{}-link", std::process::id()));
        std::fs::hard_link(&bp, &link).unwrap();
        assert!(verify_clone(&memory, &layout, source, &b).is_err());
        std::fs::remove_file(&link).unwrap();
        verify_clone(&memory, &layout, source, &b).unwrap();
        let capture = scan_frozen_pages(&memory, &layout, true).unwrap();
        let dirty = capture.dirty;
        let read = capture.resident.unwrap();
        let (n, _, _) =
            write_verified_snapshot(&memory, &b, &read, &dirty, &[(0, size as u64, 0)]).unwrap();
        assert_eq!(n, read.iter().map(|r| r.length).sum::<u64>());
        assert!(n >= page as u64);
        let mut expected = vec![0; size];
        expected[..3 * page].fill(43);
        let mut actual = vec![0; size];
        a.read_exact_at(&mut actual, 0).unwrap();
        assert_eq!(actual, expected);
        expected[page] = 73;
        b.read_exact_at(&mut actual, 0).unwrap();
        assert_eq!(actual, expected);
        // Buffered or committed divergence must both preclude further writes.
        assert!(verify_clone(&memory, &layout, source, &b).is_err());
        b.sync_all().unwrap();
        assert!(verify_clone(&memory, &layout, source, &b).is_err());
        for path in [ap, bp, cp] {
            std::fs::remove_file(path).unwrap();
        }
    }
}

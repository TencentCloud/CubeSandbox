// Copyright 2018 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.

#[macro_use]
extern crate log;

mod qcow_raw_file;
mod raw_file;
mod refcount;
mod vec_cache;

use crate::qcow_raw_file::QcowRawFile;
use crate::refcount::RefCount;
use crate::vec_cache::{CacheMap, Cacheable, VecCache};
use byteorder::{BigEndian, ReadBytesExt, WriteBytesExt};
use libc::{EINVAL, ENOSPC, ENOTSUP};
use remain::sorted;
use std::cmp::{max, min};
use std::fmt::{self, Display};
use std::io::{self, Read, Seek, SeekFrom, Write};
use std::mem::size_of;
use vmm_sys_util::{
    file_traits::FileSetLen, file_traits::FileSync, seek_hole::SeekHole, write_zeroes::PunchHole,
    write_zeroes::WriteZeroesAt,
};

pub use crate::raw_file::RawFile;

#[sorted]
#[derive(Debug)]
pub enum Error {
    BackingFilesNotSupported,
    CompressedBlocksNotSupported,
    EvictingCache(io::Error),
    FileTooBig(u64),
    GettingFileSize(io::Error),
    GettingRefcount(refcount::Error),
    InvalidClusterIndex,
    InvalidClusterSize,
    InvalidIndex,
    InvalidL1TableOffset,
    InvalidL1TableSize(u32),
    InvalidMagic,
    InvalidOffset(u64),
    InvalidRefcountTableOffset,
    InvalidRefcountTableSize(u64),
    NoFreeClusters,
    NoRefcountClusters,
    NotEnoughSpaceForRefcounts,
    OpeningFile(io::Error),
    ReadingData(io::Error),
    ReadingHeader(io::Error),
    ReadingPointers(io::Error),
    ReadingRefCountBlock(refcount::Error),
    ReadingRefCounts(io::Error),
    RebuildingRefCounts(io::Error),
    RefcountTableOffEnd,
    RefcountTableTooLarge,
    SeekingFile(io::Error),
    SettingFileSize(io::Error),
    SettingRefcountRefcount(io::Error),
    SizeTooSmallForNumberOfClusters,
    TooManyL1Entries(u64),
    TooManyRefcounts(u64),
    UnsupportedRefcountOrder,
    UnsupportedVersion(u32),
    WritingData(io::Error),
    WritingHeader(io::Error),
}

pub type Result<T> = std::result::Result<T, Error>;

impl Display for Error {
    #[remain::check]
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        use self::Error::*;

        #[sorted]
        match self {
            BackingFilesNotSupported => write!(f, "backing files not supported"),
            CompressedBlocksNotSupported => write!(f, "compressed blocks not supported"),
            EvictingCache(e) => write!(f, "failed to evict cache: {}", e),
            FileTooBig(size) => write!(
                f,
                "file larger than max of {}: {}",
                MAX_QCOW_FILE_SIZE, size
            ),
            GettingFileSize(e) => write!(f, "failed to get file size: {}", e),
            GettingRefcount(e) => write!(f, "failed to get refcount: {}", e),
            InvalidClusterIndex => write!(f, "invalid cluster index"),
            InvalidClusterSize => write!(f, "invalid cluster size"),
            InvalidIndex => write!(f, "invalid index"),
            InvalidL1TableOffset => write!(f, "invalid L1 table offset"),
            InvalidL1TableSize(size) => write!(f, "invalid L1 table size {}", size),
            InvalidMagic => write!(f, "invalid magic"),
            InvalidOffset(_) => write!(f, "invalid offset"),
            InvalidRefcountTableOffset => write!(f, "invalid refcount table offset"),
            InvalidRefcountTableSize(size) => write!(f, "invalid refcount table size: {}", size),
            NoFreeClusters => write!(f, "no free clusters"),
            NoRefcountClusters => write!(f, "no refcount clusters"),
            NotEnoughSpaceForRefcounts => write!(f, "not enough space for refcounts"),
            OpeningFile(e) => write!(f, "failed to open file: {}", e),
            ReadingData(e) => write!(f, "failed to read data: {}", e),
            ReadingHeader(e) => write!(f, "failed to read header: {}", e),
            ReadingPointers(e) => write!(f, "failed to read pointers: {}", e),
            ReadingRefCountBlock(e) => write!(f, "failed to read ref count block: {}", e),
            ReadingRefCounts(e) => write!(f, "failed to read ref counts: {}", e),
            RebuildingRefCounts(e) => write!(f, "failed to rebuild ref counts: {}", e),
            RefcountTableOffEnd => write!(f, "refcount table offset past file end"),
            RefcountTableTooLarge => write!(f, "too many clusters specified for refcount table"),
            SeekingFile(e) => write!(f, "failed to seek file: {}", e),
            SettingFileSize(e) => write!(f, "failed to set file size: {}", e),
            SettingRefcountRefcount(e) => write!(f, "failed to set refcount refcount: {}", e),
            SizeTooSmallForNumberOfClusters => write!(f, "size too small for number of clusters"),
            TooManyL1Entries(count) => write!(f, "l1 entry table too large: {}", count),
            TooManyRefcounts(count) => write!(f, "ref count table too large: {}", count),
            UnsupportedRefcountOrder => write!(f, "unsupported refcount order"),
            UnsupportedVersion(v) => write!(f, "unsupported version: {}", v),
            WritingData(e) => write!(f, "failed to write data: {}", e),
            WritingHeader(e) => write!(f, "failed to write header: {}", e),
        }
    }
}

pub enum ImageType {
    Raw,
    Qcow2,
}

// Maximum data size supported.
const MAX_QCOW_FILE_SIZE: u64 = 0x01 << 44; // 16 TB.

// QCOW magic constant that starts the header.
const QCOW_MAGIC: u32 = 0x5146_49fb;
// Default to a cluster size of 2^DEFAULT_CLUSTER_BITS
const DEFAULT_CLUSTER_BITS: u32 = 16;
// Limit clusters to reasonable sizes. Choose the same limits as qemu. Making the clusters smaller
// increases the amount of overhead for book keeping.
const MIN_CLUSTER_BITS: u32 = 9;
const MAX_CLUSTER_BITS: u32 = 21;
// The L1 and RefCount table are kept in RAM, only handle files that require less than 35M entries.
// This easily covers 1 TB files. When support for bigger files is needed the assumptions made to
// keep these tables in RAM needs to be thrown out.
const MAX_RAM_POINTER_TABLE_SIZE: u64 = 35_000_000;
// Only support 2 byte refcounts, 2^refcount_order bits.
const DEFAULT_REFCOUNT_ORDER: u32 = 4;

const V2_BARE_HEADER_SIZE: u32 = 72;
const V3_BARE_HEADER_SIZE: u32 = 104;

// bits 0-8 and 56-63 are reserved.
const L1_TABLE_OFFSET_MASK: u64 = 0x00ff_ffff_ffff_fe00;
const L2_TABLE_OFFSET_MASK: u64 = 0x00ff_ffff_ffff_fe00;
// Flags
const COMPRESSED_FLAG: u64 = 1 << 62;
const CLUSTER_USED_FLAG: u64 = 1 << 63;
const COMPATIBLE_FEATURES_LAZY_REFCOUNTS: u64 = 1;

/// Contains the information from the header of a qcow file.
#[derive(Copy, Clone, Debug)]
pub struct QcowHeader {
    pub magic: u32,
    pub version: u32,

    pub backing_file_offset: u64,
    pub backing_file_size: u32,

    pub cluster_bits: u32,
    pub size: u64,
    pub crypt_method: u32,

    pub l1_size: u32,
    pub l1_table_offset: u64,

    pub refcount_table_offset: u64,
    pub refcount_table_clusters: u32,

    pub nb_snapshots: u32,
    pub snapshots_offset: u64,

    // v3 entries
    pub incompatible_features: u64,
    pub compatible_features: u64,
    pub autoclear_features: u64,
    pub refcount_order: u32,
    pub header_size: u32,
}

impl QcowHeader {
    /// Creates a QcowHeader from a reference to a file.
    pub fn new(f: &mut RawFile) -> Result<QcowHeader> {
        f.seek(SeekFrom::Start(0)).map_err(Error::ReadingHeader)?;
        let magic = f.read_u32::<BigEndian>().map_err(Error::ReadingHeader)?;
        if magic != QCOW_MAGIC {
            return Err(Error::InvalidMagic);
        }

        // Reads the next u32 from the file.
        fn read_u32_from_file(f: &mut RawFile) -> Result<u32> {
            f.read_u32::<BigEndian>().map_err(Error::ReadingHeader)
        }

        // Reads the next u64 from the file.
        fn read_u64_from_file(f: &mut RawFile) -> Result<u64> {
            f.read_u64::<BigEndian>().map_err(Error::ReadingHeader)
        }

        let version = read_u32_from_file(f)?;

        Ok(QcowHeader {
            magic,
            version,
            backing_file_offset: read_u64_from_file(f)?,
            backing_file_size: read_u32_from_file(f)?,
            cluster_bits: read_u32_from_file(f)?,
            size: read_u64_from_file(f)?,
            crypt_method: read_u32_from_file(f)?,
            l1_size: read_u32_from_file(f)?,
            l1_table_offset: read_u64_from_file(f)?,
            refcount_table_offset: read_u64_from_file(f)?,
            refcount_table_clusters: read_u32_from_file(f)?,
            nb_snapshots: read_u32_from_file(f)?,
            snapshots_offset: read_u64_from_file(f)?,
            incompatible_features: if version == 2 {
                0
            } else {
                read_u64_from_file(f)?
            },
            compatible_features: if version == 2 {
                0
            } else {
                read_u64_from_file(f)?
            },
            autoclear_features: if version == 2 {
                0
            } else {
                read_u64_from_file(f)?
            },
            refcount_order: if version == 2 {
                DEFAULT_REFCOUNT_ORDER
            } else {
                read_u32_from_file(f)?
            },
            header_size: if version == 2 {
                V2_BARE_HEADER_SIZE
            } else {
                read_u32_from_file(f)?
            },
        })
    }

    /// Create a header for the given `size`.
    pub fn create_for_size(version: u32, size: u64) -> QcowHeader {
        let cluster_bits: u32 = DEFAULT_CLUSTER_BITS;
        let cluster_size: u32 = 0x01 << cluster_bits;
        // L2 blocks are always one cluster long. They contain cluster_size/sizeof(u64) addresses.
        let entries_per_cluster: u32 = cluster_size / size_of::<u64>() as u32;
        let num_clusters: u32 = div_round_up_u64(size, u64::from(cluster_size)) as u32;
        let num_l2_clusters: u32 = div_round_up_u32(num_clusters, entries_per_cluster);
        let l1_clusters: u32 = div_round_up_u32(num_l2_clusters, entries_per_cluster);
        let header_clusters = div_round_up_u32(size_of::<QcowHeader>() as u32, cluster_size);
        QcowHeader {
            magic: QCOW_MAGIC,
            version,
            backing_file_offset: 0,
            backing_file_size: 0,
            cluster_bits: DEFAULT_CLUSTER_BITS,
            size,
            crypt_method: 0,
            l1_size: num_l2_clusters,
            l1_table_offset: u64::from(cluster_size),
            // The refcount table is after l1 + header.
            refcount_table_offset: u64::from(cluster_size * (l1_clusters + 1)),
            refcount_table_clusters: {
                // Pre-allocate enough clusters for the entire refcount table as it must be
                // continuous in the file. Allocate enough space to refcount all clusters, including
                // the refcount clusters.
                let max_refcount_clusters = max_refcount_clusters(
                    DEFAULT_REFCOUNT_ORDER,
                    cluster_size,
                    num_clusters + l1_clusters + num_l2_clusters + header_clusters,
                ) as u32;
                // The refcount table needs to store the offset of each refcount cluster.
                div_round_up_u32(
                    max_refcount_clusters * size_of::<u64>() as u32,
                    cluster_size,
                )
            },
            nb_snapshots: 0,
            snapshots_offset: 0,
            incompatible_features: 0,
            compatible_features: 0,
            autoclear_features: 0,
            refcount_order: DEFAULT_REFCOUNT_ORDER,
            header_size: if version == 2 {
                V2_BARE_HEADER_SIZE
            } else {
                V3_BARE_HEADER_SIZE
            },
        }
    }

    /// Write the header to `file`.
    pub fn write_to<F: Write + Seek>(&self, file: &mut F) -> Result<()> {
        // Writes the next u32 to the file.
        fn write_u32_to_file<F: Write>(f: &mut F, value: u32) -> Result<()> {
            f.write_u32::<BigEndian>(value)
                .map_err(Error::WritingHeader)
        }

        // Writes the next u64 to the file.
        fn write_u64_to_file<F: Write>(f: &mut F, value: u64) -> Result<()> {
            f.write_u64::<BigEndian>(value)
                .map_err(Error::WritingHeader)
        }

        write_u32_to_file(file, self.magic)?;
        write_u32_to_file(file, self.version)?;
        write_u64_to_file(file, self.backing_file_offset)?;
        write_u32_to_file(file, self.backing_file_size)?;
        write_u32_to_file(file, self.cluster_bits)?;
        write_u64_to_file(file, self.size)?;
        write_u32_to_file(file, self.crypt_method)?;
        write_u32_to_file(file, self.l1_size)?;
        write_u64_to_file(file, self.l1_table_offset)?;
        write_u64_to_file(file, self.refcount_table_offset)?;
        write_u32_to_file(file, self.refcount_table_clusters)?;
        write_u32_to_file(file, self.nb_snapshots)?;
        write_u64_to_file(file, self.snapshots_offset)?;
        write_u64_to_file(file, self.incompatible_features)?;
        write_u64_to_file(file, self.compatible_features)?;
        write_u64_to_file(file, self.autoclear_features)?;
        write_u32_to_file(file, self.refcount_order)?;
        write_u32_to_file(file, self.header_size)?;

        // Set the file length by seeking and writing a zero to the last byte. This avoids needing
        // a `File` instead of anything that implements seek as the `file` argument.
        // Zeros out the l1 and refcount table clusters.
        let cluster_size = 0x01u64 << self.cluster_bits;
        let refcount_blocks_size = u64::from(self.refcount_table_clusters) * cluster_size;
        file.seek(SeekFrom::Start(
            self.refcount_table_offset + refcount_blocks_size - 2,
        ))
        .map_err(Error::WritingHeader)?;
        file.write(&[0u8]).map_err(Error::WritingHeader)?;

        Ok(())
    }
}

fn max_refcount_clusters(refcount_order: u32, cluster_size: u32, num_clusters: u32) -> u64 {
    // Use u64 as the product of the u32 inputs can overflow.
    let refcount_bytes = (0x01 << u64::from(refcount_order)) / 8;
    let for_data = div_round_up_u64(
        u64::from(num_clusters) * refcount_bytes,
        u64::from(cluster_size),
    );
    let for_refcounts = div_round_up_u64(for_data * refcount_bytes, u64::from(cluster_size));
    for_data + for_refcounts
}

/// Represents a qcow2 file. This is a sparse file format maintained by the qemu project.
/// Full documentation of the format can be found in the qemu repository.
///
/// # Example
///
/// ```
/// # use std::io::{Read, Seek, SeekFrom};
/// # use qcow::{self, QcowFile, RawFile};
/// # fn test(file: std::fs::File) -> std::io::Result<()> {
///     let mut raw_img = RawFile::new(file, false);
///     let mut q = QcowFile::from(raw_img).expect("Can't open qcow file");
///     let mut buf = [0u8; 12];
///     q.seek(SeekFrom::Start(10 as u64))?;
///     q.read(&mut buf[..])?;
/// #   Ok(())
/// # }
/// ```
#[derive(Clone, Debug)]
pub struct QcowFile {
    raw_file: QcowRawFile,
    header: QcowHeader,
    l1_table: VecCache<u64>,
    l2_entries: u64,
    l2_cache: CacheMap<VecCache<u64>>,
    refcounts: RefCount,
    current_offset: u64,
    unref_clusters: Vec<u64>, // List of freshly unreferenced clusters.
    // List of unreferenced clusters available to be used. unref clusters become available once the
    // removal of references to them have been synced to disk.
    avail_clusters: Vec<u64>,
    //TODO(dgreid) Add support for backing files. - backing_file: Option<Box<QcowFile<T>>>,
}

impl QcowFile {
    /// Creates a QcowFile from `file`. File must be a valid qcow2 image.
    pub fn from(mut file: RawFile) -> Result<QcowFile> {
        let header = QcowHeader::new(&mut file)?;

        // Only v2 and v3 files are supported.
        if header.version != 2 && header.version != 3 {
            return Err(Error::UnsupportedVersion(header.version));
        }

        // Make sure that the L1 table fits in RAM.
        if u64::from(header.l1_size) > MAX_RAM_POINTER_TABLE_SIZE {
            return Err(Error::InvalidL1TableSize(header.l1_size));
        }

        let cluster_bits: u32 = header.cluster_bits;
        if !(MIN_CLUSTER_BITS..=MAX_CLUSTER_BITS).contains(&cluster_bits) {
            return Err(Error::InvalidClusterSize);
        }
        let cluster_size = 0x01u64 << cluster_bits;

        // Limit the total size of the disk.
        if header.size > MAX_QCOW_FILE_SIZE {
            return Err(Error::FileTooBig(header.size));
        }

        // No current support for backing files.
        if header.backing_file_offset != 0 {
            return Err(Error::BackingFilesNotSupported);
        }

        // Only support two byte refcounts.
        let refcount_bits: u64 = 0x01u64
            .checked_shl(header.refcount_order)
            .ok_or(Error::UnsupportedRefcountOrder)?;
        if refcount_bits != 16 {
            return Err(Error::UnsupportedRefcountOrder);
        }
        let refcount_bytes = (refcount_bits + 7) / 8;

        // Need at least one refcount cluster
        if header.refcount_table_clusters == 0 {
            return Err(Error::NoRefcountClusters);
        }
        offset_is_cluster_boundary(header.backing_file_offset, header.cluster_bits)?;
        offset_is_cluster_boundary(header.l1_table_offset, header.cluster_bits)?;
        offset_is_cluster_boundary(header.snapshots_offset, header.cluster_bits)?;
        // refcount table must be a cluster boundary, and within the file's virtual or actual size.
        offset_is_cluster_boundary(header.refcount_table_offset, header.cluster_bits)?;
        let file_size = file.metadata().map_err(Error::GettingFileSize)?.len();
        if header.refcount_table_offset > max(file_size, header.size) {
            return Err(Error::RefcountTableOffEnd);
        }

        // The first cluster should always have a non-zero refcount, so if it is 0,
        // this is an old file with broken refcounts, which requires a rebuild.
        let mut refcount_rebuild_required = true;
        file.seek(SeekFrom::Start(header.refcount_table_offset))
            .map_err(Error::SeekingFile)?;
        let first_refblock_addr = file.read_u64::<BigEndian>().map_err(Error::ReadingHeader)?;
        if first_refblock_addr != 0 {
            file.seek(SeekFrom::Start(first_refblock_addr))
                .map_err(Error::SeekingFile)?;
            let first_cluster_refcount =
                file.read_u16::<BigEndian>().map_err(Error::ReadingHeader)?;
            if first_cluster_refcount != 0 {
                refcount_rebuild_required = false;
            }
        }

        if (header.compatible_features & COMPATIBLE_FEATURES_LAZY_REFCOUNTS) != 0 {
            refcount_rebuild_required = true;
        }

        let mut raw_file =
            QcowRawFile::from(file, cluster_size).ok_or(Error::InvalidClusterSize)?;
        if refcount_rebuild_required {
            QcowFile::rebuild_refcounts(&mut raw_file, header)?;
        }

        let entries_per_cluster = cluster_size / size_of::<u64>() as u64;
        let num_clusters = div_round_up_u64(header.size, cluster_size);
        let num_l2_clusters = div_round_up_u64(num_clusters, entries_per_cluster);
        let l1_clusters = div_round_up_u64(num_l2_clusters, entries_per_cluster);
        let header_clusters = div_round_up_u64(size_of::<QcowHeader>() as u64, cluster_size);
        if num_l2_clusters > MAX_RAM_POINTER_TABLE_SIZE {
            return Err(Error::TooManyL1Entries(num_l2_clusters));
        }
        let l1_table = VecCache::from_vec(
            raw_file
                .read_pointer_table(
                    header.l1_table_offset,
                    num_l2_clusters,
                    Some(L1_TABLE_OFFSET_MASK),
                )
                .map_err(Error::ReadingHeader)?,
        );

        let num_clusters = div_round_up_u64(header.size, cluster_size);
        let refcount_clusters = max_refcount_clusters(
            header.refcount_order,
            cluster_size as u32,
            (num_clusters + l1_clusters + num_l2_clusters + header_clusters) as u32,
        );
        // Check that the given header doesn't have a suspiciously sized refcount table.
        if u64::from(header.refcount_table_clusters) > 2 * refcount_clusters {
            return Err(Error::RefcountTableTooLarge);
        }
        if l1_clusters + refcount_clusters > MAX_RAM_POINTER_TABLE_SIZE {
            return Err(Error::TooManyRefcounts(refcount_clusters));
        }
        let refcount_block_entries = cluster_size / refcount_bytes;
        let refcounts = RefCount::new(
            &mut raw_file,
            header.refcount_table_offset,
            refcount_clusters,
            refcount_block_entries,
            cluster_size,
        )
        .map_err(Error::ReadingRefCounts)?;

        let l2_entries = cluster_size / size_of::<u64>() as u64;

        // Check for compressed blocks
        for l2_addr_disk in l1_table.get_values() {
            if *l2_addr_disk != 0 {
                if let Err(e) = Self::read_l2_cluster(&mut raw_file, *l2_addr_disk) {
                    if let Some(os_error) = e.raw_os_error() {
                        if os_error == ENOTSUP {
                            return Err(Error::CompressedBlocksNotSupported);
                        }
                    }
                }
            }
        }

        let mut qcow = QcowFile {
            raw_file,
            header,
            l1_table,
            l2_entries,
            l2_cache: CacheMap::new(100),
            refcounts,
            current_offset: 0,
            unref_clusters: Vec::new(),
            avail_clusters: Vec::new(),
        };

        // Check that the L1 and refcount tables fit in a 64bit address space.
        qcow.header
            .l1_table_offset
            .checked_add(qcow.l1_address_offset(qcow.virtual_size()))
            .ok_or(Error::InvalidL1TableOffset)?;
        qcow.header
            .refcount_table_offset
            .checked_add(u64::from(qcow.header.refcount_table_clusters) * cluster_size)
            .ok_or(Error::InvalidRefcountTableOffset)?;

        qcow.find_avail_clusters()?;

        Ok(qcow)
    }

    /// Creates a new QcowFile at the given path.
    pub fn new(mut file: RawFile, version: u32, virtual_size: u64) -> Result<QcowFile> {
        let header = QcowHeader::create_for_size(version, virtual_size);
        file.seek(SeekFrom::Start(0)).map_err(Error::SeekingFile)?;
        header.write_to(&mut file)?;

        let mut qcow = Self::from(file)?;

        // Set the refcount for each refcount table cluster.
        let cluster_size = 0x01u64 << qcow.header.cluster_bits;
        let refcount_table_base = qcow.header.refcount_table_offset;
        let end_cluster_addr =
            refcount_table_base + u64::from(qcow.header.refcount_table_clusters) * cluster_size;

        let mut cluster_addr = 0;
        while cluster_addr < end_cluster_addr {
            let mut unref_clusters = qcow
                .set_cluster_refcount(cluster_addr, 1)
                .map_err(Error::SettingRefcountRefcount)?;
            qcow.unref_clusters.append(&mut unref_clusters);
            cluster_addr += cluster_size;
        }

        Ok(qcow)
    }

    /// Returns the `QcowHeader` for this file.
    pub fn header(&self) -> &QcowHeader {
        &self.header
    }

    /// Returns the L1 lookup table for this file. This is only useful for debugging.
    pub fn l1_table(&self) -> &[u64] {
        self.l1_table.get_values()
    }

    /// Returns an L2_table of cluster addresses, only used for debugging.
    pub fn l2_table(&mut self, l1_index: usize) -> Result<Option<&[u64]>> {
        let l2_addr_disk = *self.l1_table.get(l1_index).ok_or(Error::InvalidIndex)?;

        if l2_addr_disk == 0 {
            // Reading from an unallocated cluster will return zeros.
            return Ok(None);
        }

        if !self.l2_cache.contains_key(l1_index) {
            // Not in the cache.
            let table = VecCache::from_vec(
                Self::read_l2_cluster(&mut self.raw_file, l2_addr_disk)
                    .map_err(Error::ReadingPointers)?,
            );
            let l1_table = &self.l1_table;
            let raw_file = &mut self.raw_file;
            self.l2_cache
                .insert(l1_index, table, |index, evicted| {
                    raw_file.write_pointer_table(
                        l1_table[index],
                        evicted.get_values(),
                        CLUSTER_USED_FLAG,
                    )
                })
                .map_err(Error::EvictingCache)?;
        }

        // The index must exist as it was just inserted if it didn't already.
        Ok(Some(self.l2_cache.get(l1_index).unwrap().get_values()))
    }

    /// Returns the refcount table for this file. This is only useful for debugging.
    pub fn ref_table(&self) -> &[u64] {
        self.refcounts.ref_table()
    }

    /// Returns the `index`th refcount block from the file.
    pub fn refcount_block(&mut self, index: usize) -> Result<Option<&[u16]>> {
        self.refcounts
            .refcount_block(&mut self.raw_file, index)
            .map_err(Error::ReadingRefCountBlock)
    }

    /// Returns the first cluster in the file with a 0 refcount. Used for testing.
    pub fn first_zero_refcount(&mut self) -> Result<Option<u64>> {
        let file_size = self
            .raw_file
            .file_mut()
            .metadata()
            .map_err(Error::GettingFileSize)?
            .len();
        let cluster_size = 0x01u64 << self.header.cluster_bits;

        let mut cluster_addr = 0;
        while cluster_addr < file_size {
            let cluster_refcount = self
                .refcounts
                .get_cluster_refcount(&mut self.raw_file, cluster_addr)
                .map_err(Error::GettingRefcount)?;
            if cluster_refcount == 0 {
                return Ok(Some(cluster_addr));
            }
            cluster_addr += cluster_size;
        }
        Ok(None)
    }

    fn find_avail_clusters(&mut self) -> Result<()> {
        let cluster_size = self.raw_file.cluster_size();

        let file_size = self
            .raw_file
            .file_mut()
            .metadata()
            .map_err(Error::GettingFileSize)?
            .len();

        for i in (0..file_size).step_by(cluster_size as usize) {
            let refcount = self
                .refcounts
                .get_cluster_refcount(&mut self.raw_file, i)
                .map_err(Error::GettingRefcount)?;
            if refcount == 0 {
                self.avail_clusters.push(i);
            }
        }

        Ok(())
    }

    /// Rebuild the reference count tables.
    fn rebuild_refcounts(raw_file: &mut QcowRawFile, header: QcowHeader) -> Result<()> {
        fn add_ref(refcounts: &mut [u16], cluster_size: u64, cluster_address: u64) -> Result<()> {
            let idx = (cluster_address / cluster_size) as usize;
            if idx >= refcounts.len() {
                return Err(Error::InvalidClusterIndex);
            }
            refcounts[idx] += 1;
            Ok(())
        }

        // Add a reference to the first cluster (header plus extensions).
        fn set_header_refcount(refcounts: &mut [u16], cluster_size: u64) -> Result<()> {
            add_ref(refcounts, cluster_size, 0)
        }

        // Add references to the L1 table clusters.
        fn set_l1_refcounts(
            refcounts: &mut [u16],
            header: QcowHeader,
            cluster_size: u64,
        ) -> Result<()> {
            let entries_per_cluster = cluster_size / size_of::<u64>() as u64;
            let l1_clusters = div_round_up_u64(u64::from(header.l1_size), entries_per_cluster);
            let l1_table_offset = header.l1_table_offset;
            for i in 0..l1_clusters {
                add_ref(refcounts, cluster_size, l1_table_offset + i * cluster_size)?;
            }
            Ok(())
        }

        // Traverse the L1 and L2 tables to find all reachable data clusters.
        fn set_data_refcounts(
            refcounts: &mut [u16],
            header: QcowHeader,
            cluster_size: u64,
            raw_file: &mut QcowRawFile,
        ) -> Result<()> {
            let l1_table = raw_file
                .read_pointer_table(
                    header.l1_table_offset,
                    u64::from(header.l1_size),
                    Some(L1_TABLE_OFFSET_MASK),
                )
                .map_err(Error::ReadingPointers)?;
            for l1_index in 0..header.l1_size as usize {
                let l2_addr_disk = *l1_table.get(l1_index).ok_or(Error::InvalidIndex)?;
                if l2_addr_disk != 0 {
                    // Add a reference to the L2 table cluster itself.
                    add_ref(refcounts, cluster_size, l2_addr_disk)?;

                    // Read the L2 table and find all referenced data clusters.
                    let l2_table = raw_file
                        .read_pointer_table(
                            l2_addr_disk,
                            cluster_size / size_of::<u64>() as u64,
                            Some(L2_TABLE_OFFSET_MASK),
                        )
                        .map_err(Error::ReadingPointers)?;
                    for data_cluster_addr in l2_table {
                        if data_cluster_addr != 0 {
                            add_ref(refcounts, cluster_size, data_cluster_addr)?;
                        }
                    }
                }
            }

            Ok(())
        }

        // Add references to the top-level refcount table clusters.
        fn set_refcount_table_refcounts(
            refcounts: &mut [u16],
            header: QcowHeader,
            cluster_size: u64,
        ) -> Result<()> {
            let refcount_table_offset = header.refcount_table_offset;
            for i in 0..u64::from(header.refcount_table_clusters) {
                add_ref(
                    refcounts,
                    cluster_size,
                    refcount_table_offset + i * cluster_size,
                )?;
            }
            Ok(())
        }

        // Allocate clusters for refblocks.
        // This needs to be done last so that we have the correct refcounts for all other
        // clusters.
        fn alloc_refblocks(
            refcounts: &mut [u16],
            cluster_size: u64,
            refblock_clusters: u64,
            pointers_per_cluster: u64,
        ) -> Result<Vec<u64>> {
            let refcount_table_entries = div_round_up_u64(refblock_clusters, pointers_per_cluster);
            let mut ref_table = vec![0; refcount_table_entries as usize];
            let mut first_free_cluster: u64 = 0;
            for refblock_addr in &mut ref_table {
                loop {
                    if first_free_cluster >= refcounts.len() as u64 {
                        return Err(Error::NotEnoughSpaceForRefcounts);
                    }
                    if refcounts[first_free_cluster as usize] == 0 {
                        break;
                    }
                    first_free_cluster += 1;
                }

                *refblock_addr = first_free_cluster * cluster_size;
                add_ref(refcounts, cluster_size, *refblock_addr)?;

                first_free_cluster += 1;
            }

            Ok(ref_table)
        }

        // Write the updated reference count blocks and reftable.
        fn write_refblocks(
            refcounts: &[u16],
            mut header: QcowHeader,
            ref_table: &[u64],
            raw_file: &mut QcowRawFile,
            refcount_block_entries: u64,
        ) -> Result<()> {
            // Rewrite the header with lazy refcounts enabled while we are rebuilding the tables.
            header.compatible_features |= COMPATIBLE_FEATURES_LAZY_REFCOUNTS;
            raw_file
                .file_mut()
                .seek(SeekFrom::Start(0))
                .map_err(Error::SeekingFile)?;
            header.write_to(raw_file.file_mut())?;

            for (i, refblock_addr) in ref_table.iter().enumerate() {
                // Write a block of refcounts to the location indicated by refblock_addr.
                let refblock_start = i * (refcount_block_entries as usize);
                let refblock_end = min(
                    refcounts.len(),
                    refblock_start + refcount_block_entries as usize,
                );
                let refblock = &refcounts[refblock_start..refblock_end];
                raw_file
                    .write_refcount_block(*refblock_addr, refblock)
                    .map_err(Error::WritingHeader)?;

                // If this is the last (partial) cluster, pad it out to a full refblock cluster.
                if refblock.len() < refcount_block_entries as usize {
                    let refblock_padding =
                        vec![0u16; refcount_block_entries as usize - refblock.len()];
                    raw_file
                        .write_refcount_block(
                            *refblock_addr + refblock.len() as u64 * 2,
                            &refblock_padding,
                        )
                        .map_err(Error::WritingHeader)?;
                }
            }

            // Rewrite the top-level refcount table.
            raw_file
                .write_pointer_table(header.refcount_table_offset, ref_table, 0)
                .map_err(Error::WritingHeader)?;

            // Rewrite the header again, now with lazy refcounts disabled.
            header.compatible_features &= !COMPATIBLE_FEATURES_LAZY_REFCOUNTS;
            raw_file
                .file_mut()
                .seek(SeekFrom::Start(0))
                .map_err(Error::SeekingFile)?;
            header.write_to(raw_file.file_mut())?;

            Ok(())
        }

        let cluster_size = raw_file.cluster_size();

        let file_size = raw_file
            .file_mut()
            .metadata()
            .map_err(Error::GettingFileSize)?
            .len();

        let refcount_bits = 1u64 << header.refcount_order;
        let refcount_bytes = div_round_up_u64(refcount_bits, 8);
        let refcount_block_entries = cluster_size / refcount_bytes;
        let pointers_per_cluster = cluster_size / size_of::<u64>() as u64;
        let data_clusters = div_round_up_u64(header.size, cluster_size);
        let l2_clusters = div_round_up_u64(data_clusters, pointers_per_cluster);
        let l1_clusters = div_round_up_u64(l2_clusters, pointers_per_cluster);
        let header_clusters = div_round_up_u64(size_of::<QcowHeader>() as u64, cluster_size);
        let max_clusters = data_clusters + l2_clusters + l1_clusters + header_clusters;
        let mut max_valid_cluster_index = max_clusters;
        let refblock_clusters = div_round_up_u64(max_valid_cluster_index, refcount_block_entries);
        let reftable_clusters = div_round_up_u64(refblock_clusters, pointers_per_cluster);
        // Account for refblocks and the ref table size needed to address them.
        let refblocks_for_refs = div_round_up_u64(
            refblock_clusters + reftable_clusters,
            refcount_block_entries,
        );
        let reftable_clusters_for_refs =
            div_round_up_u64(refblocks_for_refs, refcount_block_entries);
        max_valid_cluster_index += refblock_clusters + reftable_clusters;
        max_valid_cluster_index += refblocks_for_refs + reftable_clusters_for_refs;

        if max_valid_cluster_index > MAX_RAM_POINTER_TABLE_SIZE {
            return Err(Error::InvalidRefcountTableSize(max_valid_cluster_index));
        }

        let max_valid_cluster_offset = max_valid_cluster_index * cluster_size;
        if max_valid_cluster_offset < file_size - cluster_size {
            return Err(Error::InvalidRefcountTableSize(max_valid_cluster_offset));
        }

        let mut refcounts = vec![0; max_valid_cluster_index as usize];

        // Find all references clusters and rebuild refcounts.
        set_header_refcount(&mut refcounts, cluster_size)?;
        set_l1_refcounts(&mut refcounts, header, cluster_size)?;
        set_data_refcounts(&mut refcounts, header, cluster_size, raw_file)?;
        set_refcount_table_refcounts(&mut refcounts, header, cluster_size)?;

        // Allocate clusters to store the new reference count blocks.
        let ref_table = alloc_refblocks(
            &mut refcounts,
            cluster_size,
            refblock_clusters,
            pointers_per_cluster,
        )?;

        // Write updated reference counts and point the reftable at them.
        write_refblocks(
            &refcounts,
            header,
            &ref_table,
            raw_file,
            refcount_block_entries,
        )
    }

    // Limits the range so that it doesn't exceed the virtual size of the file.
    fn limit_range_file(&self, address: u64, count: usize) -> usize {
        if address.checked_add(count as u64).is_none() || address > self.virtual_size() {
            return 0;
        }
        min(count as u64, self.virtual_size() - address) as usize
    }

    // Limits the range so that it doesn't overflow the end of a cluster.
    fn limit_range_cluster(&self, address: u64, count: usize) -> usize {
        let offset: u64 = self.raw_file.cluster_offset(address);
        let limit = self.raw_file.cluster_size() - offset;
        min(count as u64, limit) as usize
    }

    // Gets the maximum virtual size of this image.
    fn virtual_size(&self) -> u64 {
        self.header.size
    }

    // Gets the offset of `address` in the L1 table.
    fn l1_address_offset(&self, address: u64) -> u64 {
        let l1_index = self.l1_table_index(address);
        l1_index * size_of::<u64>() as u64
    }

    // Gets the offset of `address` in the L1 table.
    fn l1_table_index(&self, address: u64) -> u64 {
        (address / self.raw_file.cluster_size()) / self.l2_entries
    }

    // Gets the offset of `address` in the L2 table.
    fn l2_table_index(&self, address: u64) -> u64 {
        (address / self.raw_file.cluster_size()) % self.l2_entries
    }

    // Gets the offset of the given guest address in the host file. If L1, L2, or data clusters have
    // yet to be allocated, return None.
    fn file_offset_read(&mut self, address: u64) -> std::io::Result<Option<u64>> {
        if address >= self.virtual_size() {
            return Err(std::io::Error::from_raw_os_error(EINVAL));
        }

        let l1_index = self.l1_table_index(address) as usize;
        let l2_addr_disk = *self
            .l1_table
            .get(l1_index)
            .ok_or_else(|| std::io::Error::from_raw_os_error(EINVAL))?;

        if l2_addr_disk == 0 {
            // Reading from an unallocated cluster will return zeros.
            return Ok(None);
        }

        let l2_index = self.l2_table_index(address) as usize;

        if !self.l2_cache.contains_key(l1_index) {
            // Not in the cache.
            let table =
                VecCache::from_vec(Self::read_l2_cluster(&mut self.raw_file, l2_addr_disk)?);

            let l1_table = &self.l1_table;
            let raw_file = &mut self.raw_file;
            self.l2_cache.insert(l1_index, table, |index, evicted| {
                raw_file.write_pointer_table(
                    l1_table[index],
                    evicted.get_values(),
                    CLUSTER_USED_FLAG,
                )
            })?;
        };

        let cluster_addr = self.l2_cache.get(l1_index).unwrap()[l2_index];
        if cluster_addr == 0 {
            return Ok(None);
        }
        Ok(Some(cluster_addr + self.raw_file.cluster_offset(address)))
    }

    // Gets the offset of the given guest address in the host file. If L1, L2, or data clusters need
    // to be allocated, they will be.
    fn file_offset_write(&mut self, address: u64) -> std::io::Result<u64> {
        if address >= self.virtual_size() {
            return Err(std::io::Error::from_raw_os_error(EINVAL));
        }

        let l1_index = self.l1_table_index(address) as usize;
        let l2_addr_disk = *self
            .l1_table
            .get(l1_index)
            .ok_or_else(|| std::io::Error::from_raw_os_error(EINVAL))?;
        let l2_index = self.l2_table_index(address) as usize;

        let mut set_refcounts = Vec::new();

        if !self.l2_cache.contains_key(l1_index) {
            // Not in the cache.
            let l2_table = if l2_addr_disk == 0 {
                // Allocate a new cluster to store the L2 table and update the L1 table to point
                // to the new table.
                let new_addr: u64 = self.get_new_cluster()?;
                // The cluster refcount starts at one meaning it is used but doesn't need COW.
                set_refcounts.push((new_addr, 1));
                self.l1_table[l1_index] = new_addr;
                VecCache::new(self.l2_entries as usize)
            } else {
                VecCache::from_vec(Self::read_l2_cluster(&mut self.raw_file, l2_addr_disk)?)
            };
            let l1_table = &self.l1_table;
            let raw_file = &mut self.raw_file;
            self.l2_cache.insert(l1_index, l2_table, |index, evicted| {
                raw_file.write_pointer_table(
                    l1_table[index],
                    evicted.get_values(),
                    CLUSTER_USED_FLAG,
                )
            })?;
        }

        let cluster_addr = match self.l2_cache.get(l1_index).unwrap()[l2_index] {
            0 => {
                // Need to allocate a data cluster
                let cluster_addr = self.append_data_cluster()?;
                self.update_cluster_addr(l1_index, l2_index, cluster_addr, &mut set_refcounts)?;
                cluster_addr
            }
            a => a,
        };

        for (addr, count) in set_refcounts {
            let mut newly_unref = self.set_cluster_refcount(addr, count)?;
            self.unref_clusters.append(&mut newly_unref);
        }

        Ok(cluster_addr + self.raw_file.cluster_offset(address))
    }

    // Updates the l1 and l2 tables to point to the new `cluster_addr`.
    fn update_cluster_addr(
        &mut self,
        l1_index: usize,
        l2_index: usize,
        cluster_addr: u64,
        set_refcounts: &mut Vec<(u64, u16)>,
    ) -> io::Result<()> {
        if !self.l2_cache.get(l1_index).unwrap().dirty() {
            // Free the previously used cluster if one exists. Modified tables are always
            // witten to new clusters so the L1 table can be committed to disk after they
            // are and L1 never points at an invalid table.
            // The index must be valid from when it was inserted.
            let addr = self.l1_table[l1_index];
            if addr != 0 {
                self.unref_clusters.push(addr);
                set_refcounts.push((addr, 0));
            }

            // Allocate a new cluster to store the L2 table and update the L1 table to point
            // to the new table. The cluster will be written when the cache is flushed, no
            // need to copy the data now.
            let new_addr: u64 = self.get_new_cluster()?;
            // The cluster refcount starts at one indicating it is used but doesn't need
            // COW.
            set_refcounts.push((new_addr, 1));
            self.l1_table[l1_index] = new_addr;
        }
        // 'unwrap' is OK because it was just added.
        self.l2_cache.get_mut(l1_index).unwrap()[l2_index] = cluster_addr;
        Ok(())
    }

    // Allocate a new cluster and return its offset within the raw file.
    fn get_new_cluster(&mut self) -> std::io::Result<u64> {
        // First use a pre allocated cluster if one is available.
        if let Some(free_cluster) = self.avail_clusters.pop() {
            self.raw_file.zero_cluster(free_cluster)?;
            return Ok(free_cluster);
        }

        let max_valid_cluster_offset = self.refcounts.max_valid_cluster_offset();
        if let Some(new_cluster) = self.raw_file.add_cluster_end(max_valid_cluster_offset)? {
            Ok(new_cluster)
        } else {
            error!("No free clusters in get_new_cluster()");
            Err(std::io::Error::from_raw_os_error(ENOSPC))
        }
    }

    // Allocate and initialize a new data cluster. Returns the offset of the
    // cluster in to the file on success.
    fn append_data_cluster(&mut self) -> std::io::Result<u64> {
        let new_addr: u64 = self.get_new_cluster()?;
        // The cluster refcount starts at one indicating it is used but doesn't need COW.
        let mut newly_unref = self.set_cluster_refcount(new_addr, 1)?;
        self.unref_clusters.append(&mut newly_unref);
        Ok(new_addr)
    }

    // Returns true if the cluster containing `address` is already allocated.
    fn cluster_allocated(&mut self, address: u64) -> std::io::Result<bool> {
        if address >= self.virtual_size() {
            return Err(std::io::Error::from_raw_os_error(EINVAL));
        }

        let l1_index = self.l1_table_index(address) as usize;
        let l2_addr_disk = *self
            .l1_table
            .get(l1_index)
            .ok_or_else(|| std::io::Error::from_raw_os_error(EINVAL))?;
        let l2_index = self.l2_table_index(address) as usize;

        if l2_addr_disk == 0 {
            // The whole L2 table for this address is not allocated yet,
            // so the cluster must also be unallocated.
            return Ok(false);
        }

        if !self.l2_cache.contains_key(l1_index) {
            // Not in the cache.
            let table =
                VecCache::from_vec(Self::read_l2_cluster(&mut self.raw_file, l2_addr_disk)?);
            let l1_table = &self.l1_table;
            let raw_file = &mut self.raw_file;
            self.l2_cache.insert(l1_index, table, |index, evicted| {
                raw_file.write_pointer_table(
                    l1_table[index],
                    evicted.get_values(),
                    CLUSTER_USED_FLAG,
                )
            })?;
        }

        let cluster_addr = self.l2_cache.get(l1_index).unwrap()[l2_index];
        // If cluster_addr != 0, the cluster is allocated.
        Ok(cluster_addr != 0)
    }

    // Find the first guest address greater than or equal to `address` whose allocation state
    // matches `allocated`.
    fn find_allocated_cluster(
        &mut self,
        address: u64,
        allocated: bool,
    ) -> std::io::Result<Option<u64>> {
        let size = self.virtual_size();
        if address >= size {
            return Ok(None);
        }

        // If offset is already within a hole, return it.
        if self.cluster_allocated(address)? == allocated {
            return Ok(Some(address));
        }

        // Skip to the next cluster boundary.
        let cluster_size = self.raw_file.cluster_size();
        let mut cluster_addr = (address / cluster_size + 1) * cluster_size;

        // Search for clusters with the desired allocation state.
        while cluster_addr < size {
            if self.cluster_allocated(cluster_addr)? == allocated {
                return Ok(Some(cluster_addr));
            }
            cluster_addr += cluster_size;
        }

        Ok(None)
    }

    // Deallocate the storage for the cluster starting at `address`.
    // Any future reads of this cluster will return all zeroes.
    fn deallocate_cluster(&mut self, address: u64) -> std::io::Result<()> {
        if address >= self.virtual_size() {
            return Err(std::io::Error::from_raw_os_error(EINVAL));
        }

        let l1_index = self.l1_table_index(address) as usize;
        let l2_addr_disk = *self
            .l1_table
            .get(l1_index)
            .ok_or_else(|| std::io::Error::from_raw_os_error(EINVAL))?;
        let l2_index = self.l2_table_index(address) as usize;

        if l2_addr_disk == 0 {
            // The whole L2 table for this address is not allocated yet,
            // so the cluster must also be unallocated.
            return Ok(());
        }

        if !self.l2_cache.contains_key(l1_index) {
            // Not in the cache.
            let table =
                VecCache::from_vec(Self::read_l2_cluster(&mut self.raw_file, l2_addr_disk)?);
            let l1_table = &self.l1_table;
            let raw_file = &mut self.raw_file;
            self.l2_cache.insert(l1_index, table, |index, evicted| {
                raw_file.write_pointer_table(
                    l1_table[index],
                    evicted.get_values(),
                    CLUSTER_USED_FLAG,
                )
            })?;
        }

        let cluster_addr = self.l2_cache.get(l1_index).unwrap()[l2_index];
        if cluster_addr == 0 {
            // This cluster is already unallocated; nothing to do.
            return Ok(());
        }

        // Decrement the refcount.
        let refcount = self
            .refcounts
            .get_cluster_refcount(&mut self.raw_file, cluster_addr)
            .map_err(|e| {
                io::Error::new(
                    io::ErrorKind::InvalidData,
                    format!("failed to get cluster refcount: {}", e),
                )
            })?;
        if refcount == 0 {
            return Err(std::io::Error::from_raw_os_error(EINVAL));
        }

        let new_refcount = refcount - 1;
        let mut newly_unref = self.set_cluster_refcount(cluster_addr, new_refcount)?;
        self.unref_clusters.append(&mut newly_unref);

        // Rewrite the L2 entry to remove the cluster mapping.
        // unwrap is safe as we just checked/inserted this entry.
        self.l2_cache.get_mut(l1_index).unwrap()[l2_index] = 0;

        if new_refcount == 0 {
            let cluster_size = self.raw_file.cluster_size();
            // This cluster is no longer in use; deallocate the storage.
            // The underlying FS may not support FALLOC_FL_PUNCH_HOLE,
            // so don't treat an error as fatal.  Future reads will return zeros anyways.
            let _ = self
                .raw_file
                .file_mut()
                .punch_hole(cluster_addr, cluster_size);
            self.unref_clusters.push(cluster_addr);
        }
        Ok(())
    }

    // Deallocate the storage for `length` bytes starting at `address`.
    // Any future reads of this range will return all zeroes.
    fn deallocate_bytes(&mut self, address: u64, length: usize) -> std::io::Result<()> {
        let write_count: usize = self.limit_range_file(address, length);

        let mut nwritten: usize = 0;
        while nwritten < write_count {
            let curr_addr = address + nwritten as u64;
            let count = self.limit_range_cluster(curr_addr, write_count - nwritten);

            if count == self.raw_file.cluster_size() as usize {
                // Full cluster - deallocate the storage.
                self.deallocate_cluster(curr_addr)?;
            } else {
                // Partial cluster - zero out the relevant bytes if it was allocated.
                // Any space in unallocated clusters can be left alone, since
                // unallocated clusters already read back as zeroes.
                if let Some(offset) = self.file_offset_read(curr_addr)? {
                    // Partial cluster - zero it out.
                    self.raw_file.file_mut().write_zeroes_at(offset, count)?;
                }
            }

            nwritten += count;
        }
        Ok(())
    }

    // Reads an L2 cluster from the disk, returning an error if the file can't be read or if any
    // cluster is compressed.
    fn read_l2_cluster(raw_file: &mut QcowRawFile, cluster_addr: u64) -> std::io::Result<Vec<u64>> {
        let file_values = raw_file.read_pointer_cluster(cluster_addr, None)?;
        if file_values.iter().any(|entry| entry & COMPRESSED_FLAG != 0) {
            return Err(std::io::Error::from_raw_os_error(ENOTSUP));
        }
        Ok(file_values
            .iter()
            .map(|entry| *entry & L2_TABLE_OFFSET_MASK)
            .collect())
    }

    // Set the refcount for a cluster with the given address.
    // Returns a list of any refblocks that can be reused, this happens when a refblock is moved,
    // the old location can be reused.
    fn set_cluster_refcount(&mut self, address: u64, refcount: u16) -> std::io::Result<Vec<u64>> {
        let mut added_clusters = Vec::new();
        let mut unref_clusters = Vec::new();
        let mut refcount_set = false;
        let mut new_cluster = None;

        while !refcount_set {
            match self.refcounts.set_cluster_refcount(
                &mut self.raw_file,
                address,
                refcount,
                new_cluster.take(),
            ) {
                Ok(None) => {
                    refcount_set = true;
                }
                Ok(Some(freed_cluster)) => {
                    unref_clusters.push(freed_cluster);
                    refcount_set = true;
                }
                Err(refcount::Error::EvictingRefCounts(e)) => {
                    return Err(e);
                }
                Err(refcount::Error::InvalidIndex) => {
                    return Err(std::io::Error::from_raw_os_error(EINVAL));
                }
                Err(refcount::Error::NeedCluster(addr)) => {
                    // Read the address and call set_cluster_refcount again.
                    new_cluster = Some((
                        addr,
                        VecCache::from_vec(self.raw_file.read_refcount_block(addr)?),
                    ));
                }
                Err(refcount::Error::NeedNewCluster) => {
                    // Allocate the cluster and call set_cluster_refcount again.
                    let addr = self.get_new_cluster()?;
                    added_clusters.push(addr);
                    new_cluster = Some((
                        addr,
                        VecCache::new(self.refcounts.refcounts_per_block() as usize),
                    ));
                }
                Err(refcount::Error::ReadingRefCounts(e)) => {
                    return Err(e);
                }
            }
        }

        for addr in added_clusters {
            self.set_cluster_refcount(addr, 1)?;
        }
        Ok(unref_clusters)
    }

    fn sync_caches(&mut self) -> std::io::Result<()> {
        // Write out all dirty L2 tables.
        for (l1_index, l2_table) in self.l2_cache.iter_mut().filter(|(_k, v)| v.dirty()) {
            // The index must be valid from when we inserted it.
            let addr = self.l1_table[*l1_index];
            if addr != 0 {
                self.raw_file.write_pointer_table(
                    addr,
                    l2_table.get_values(),
                    CLUSTER_USED_FLAG,
                )?;
            } else {
                return Err(std::io::Error::from_raw_os_error(EINVAL));
            }
            l2_table.mark_clean();
        }
        // Write the modified refcount blocks.
        self.refcounts.flush_blocks(&mut self.raw_file)?;
        // Make sure metadata(file len) and all data clusters are written.
        self.raw_file.file_mut().sync_all()?;

        // Push L1 table and refcount table last as all the clusters they point to are now
        // guaranteed to be valid.
        let mut sync_required = if self.l1_table.dirty() {
            self.raw_file.write_pointer_table(
                self.header.l1_table_offset,
                self.l1_table.get_values(),
                0,
            )?;
            self.l1_table.mark_clean();
            true
        } else {
            false
        };
        sync_required |= self.refcounts.flush_table(&mut self.raw_file)?;
        if sync_required {
            self.raw_file.file_mut().sync_data()?;
        }
        Ok(())
    }
}

impl Drop for QcowFile {
    fn drop(&mut self) {
        let _ = self.sync_caches();
    }
}

impl Read for QcowFile {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        let address: u64 = self.current_offset;
        let read_count: usize = self.limit_range_file(address, buf.len());

        let mut nread: usize = 0;
        while nread < read_count {
            let curr_addr = address + nread as u64;
            let file_offset = self.file_offset_read(curr_addr)?;
            let count = self.limit_range_cluster(curr_addr, read_count - nread);

            if let Some(offset) = file_offset {
                self.raw_file.file_mut().seek(SeekFrom::Start(offset))?;
                self.raw_file
                    .file_mut()
                    .read_exact(&mut buf[nread..(nread + count)])?;
            } else {
                // Previously unwritten region, return zeros
                for b in &mut buf[nread..(nread + count)] {
                    *b = 0;
                }
            }

            nread += count;
        }
        self.current_offset += read_count as u64;
        Ok(read_count)
    }
}

impl Seek for QcowFile {
    fn seek(&mut self, pos: SeekFrom) -> std::io::Result<u64> {
        let new_offset: Option<u64> = match pos {
            SeekFrom::Start(off) => Some(off),
            SeekFrom::End(off) => {
                if off < 0 {
                    0i64.checked_sub(off)
                        .and_then(|increment| self.virtual_size().checked_sub(increment as u64))
                } else {
                    self.virtual_size().checked_add(off as u64)
                }
            }
            SeekFrom::Current(off) => {
                if off < 0 {
                    0i64.checked_sub(off)
                        .and_then(|increment| self.current_offset.checked_sub(increment as u64))
                } else {
                    self.current_offset.checked_add(off as u64)
                }
            }
        };

        if let Some(o) = new_offset {
            if o <= self.virtual_size() {
                self.current_offset = o;
                return Ok(o);
            }
        }
        Err(std::io::Error::from_raw_os_error(EINVAL))
    }
}

impl Write for QcowFile {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        let address: u64 = self.current_offset;
        let write_count: usize = self.limit_range_file(address, buf.len());

        let mut nwritten: usize = 0;
        while nwritten < write_count {
            let curr_addr = address + nwritten as u64;
            let offset = self.file_offset_write(curr_addr)?;
            let count = self.limit_range_cluster(curr_addr, write_count - nwritten);

            self.raw_file.file_mut().seek(SeekFrom::Start(offset))?;
            let count = self
                .raw_file
                .file_mut()
                .write(&buf[nwritten..(nwritten + count)])?;

            nwritten += count;
        }
        self.current_offset += write_count as u64;
        Ok(write_count)
    }

    fn flush(&mut self) -> std::io::Result<()> {
        self.sync_caches()?;
        self.avail_clusters.append(&mut self.unref_clusters);
        Ok(())
    }
}

impl FileSync for QcowFile {
    fn fsync(&mut self) -> std::io::Result<()> {
        self.flush()
    }
}

impl FileSetLen for QcowFile {
    fn set_len(&self, _len: u64) -> std::io::Result<()> {
        Err(std::io::Error::new(
            std::io::ErrorKind::Other,
            "set_len() not supported for QcowFile",
        ))
    }
}

impl PunchHole for QcowFile {
    fn punch_hole(&mut self, offset: u64, length: u64) -> std::io::Result<()> {
        let mut remaining = length;
        let mut offset = offset;
        while remaining > 0 {
            let chunk_length = min(remaining, std::usize::MAX as u64) as usize;
            self.deallocate_bytes(offset, chunk_length)?;
            remaining -= chunk_length as u64;
            offset += chunk_length as u64;
        }
        Ok(())
    }
}

impl WriteZeroesAt for QcowFile {
    fn write_zeroes_at(&mut self, offset: u64, length: usize) -> io::Result<usize> {
        self.punch_hole(offset, length as u64)?;
        Ok(length)
    }
}

impl SeekHole for QcowFile {
    fn seek_hole(&mut self, offset: u64) -> io::Result<Option<u64>> {
        match self.find_allocated_cluster(offset, false) {
            Err(e) => Err(e),
            Ok(None) => {
                if offset < self.virtual_size() {
                    Ok(Some(self.seek(SeekFrom::End(0))?))
                } else {
                    Ok(None)
                }
            }
            Ok(Some(o)) => {
                self.seek(SeekFrom::Start(o))?;
                Ok(Some(o))
            }
        }
    }

    fn seek_data(&mut self, offset: u64) -> io::Result<Option<u64>> {
        match self.find_allocated_cluster(offset, true) {
            Err(e) => Err(e),
            Ok(None) => Ok(None),
            Ok(Some(o)) => {
                self.seek(SeekFrom::Start(o))?;
                Ok(Some(o))
            }
        }
    }
}

// Returns an Error if the given offset doesn't align to a cluster boundary.
fn offset_is_cluster_boundary(offset: u64, cluster_bits: u32) -> Result<()> {
    if offset & ((0x01 << cluster_bits) - 1) != 0 {
        return Err(Error::InvalidOffset(offset));
    }
    Ok(())
}

// Ceiling of the division of `dividend`/`divisor`.
fn div_round_up_u64(dividend: u64, divisor: u64) -> u64 {
    dividend / divisor + u64::from(dividend % divisor != 0)
}

// Ceiling of the division of `dividend`/`divisor`.
fn div_round_up_u32(dividend: u32, divisor: u32) -> u32 {
    dividend / divisor + u32::from(dividend % divisor != 0)
}

fn convert_copy<R, W>(reader: &mut R, writer: &mut W, offset: u64, size: u64) -> Result<()>
where
    R: Read + Seek,
    W: Write + Seek,
{
    const CHUNK_SIZE: usize = 65536;
    let mut buf = [0; CHUNK_SIZE];
    let mut read_count = 0;
    reader
        .seek(SeekFrom::Start(offset))
        .map_err(Error::SeekingFile)?;
    writer
        .seek(SeekFrom::Start(offset))
        .map_err(Error::SeekingFile)?;
    loop {
        let this_count = min(CHUNK_SIZE as u64, size - read_count) as usize;
        let nread = reader
            .read(&mut buf[..this_count])
            .map_err(Error::ReadingData)?;
        writer.write(&buf[..nread]).map_err(Error::WritingData)?;
        read_count += nread as u64;
        if nread == 0 || read_count == size {
            break;
        }
    }

    Ok(())
}

fn convert_reader_writer<R, W>(reader: &mut R, writer: &mut W, size: u64) -> Result<()>
where
    R: Read + Seek + SeekHole,
    W: Write + Seek,
{
    let mut offset = 0;
    while offset < size {
        // Find the next range of data.
        let next_data = match reader.seek_data(offset).map_err(Error::SeekingFile)? {
            Some(o) => o,
            None => {
                // No more data in the file.
                break;
            }
        };
        let next_hole = match reader.seek_hole(next_data).map_err(Error::SeekingFile)? {
            Some(o) => o,
            None => {
                // This should not happen - there should always be at least one hole
                // after any data.
                return Err(Error::SeekingFile(io::Error::from_raw_os_error(EINVAL)));
            }
        };
        let count = next_hole - next_data;
        convert_copy(reader, writer, next_data, count)?;
        offset = next_hole;
    }

    Ok(())
}

fn convert_reader<R>(reader: &mut R, dst_file: RawFile, dst_type: ImageType) -> Result<()>
where
    R: Read + Seek + SeekHole,
{
    let src_size = reader.seek(SeekFrom::End(0)).map_err(Error::SeekingFile)?;
    reader
        .seek(SeekFrom::Start(0))
        .map_err(Error::SeekingFile)?;

    // Ensure the destination file is empty before writing to it.
    dst_file.set_len(0).map_err(Error::SettingFileSize)?;

    match dst_type {
        ImageType::Qcow2 => {
            let mut dst_writer = QcowFile::new(dst_file, 3, src_size)?;
            convert_reader_writer(reader, &mut dst_writer, src_size)
        }
        ImageType::Raw => {
            let mut dst_writer = dst_file;
            // Set the length of the destination file to convert it into a sparse file
            // of the desired size.
            dst_writer
                .set_len(src_size)
                .map_err(Error::SettingFileSize)?;
            convert_reader_writer(reader, &mut dst_writer, src_size)
        }
    }
}

/// Copy the contents of a disk image in `src_file` into `dst_file`.
/// The type of `src_file` is automatically detected, and the output file type is
/// determined by `dst_type`.
pub fn convert(mut src_file: RawFile, dst_file: RawFile, dst_type: ImageType) -> Result<()> {
    let src_type = detect_image_type(&mut src_file)?;
    match src_type {
        ImageType::Qcow2 => {
            let mut src_reader = QcowFile::from(src_file)?;
            convert_reader(&mut src_reader, dst_file, dst_type)
        }
        ImageType::Raw => {
            // src_file is a raw file.
            let mut src_reader = src_file;
            convert_reader(&mut src_reader, dst_file, dst_type)
        }
    }
}

/// Detect the type of an image file by checking for a valid qcow2 header.
pub fn detect_image_type(file: &mut RawFile) -> Result<ImageType> {
    let orig_seek = file
        .seek(SeekFrom::Current(0))
        .map_err(Error::SeekingFile)?;
    file.seek(SeekFrom::Start(0)).map_err(Error::SeekingFile)?;
    let magic = file.read_u32::<BigEndian>().map_err(Error::ReadingHeader)?;
    let image_type = if magic == QCOW_MAGIC {
        ImageType::Qcow2
    } else {
        ImageType::Raw
    };
    file.seek(SeekFrom::Start(orig_seek))
        .map_err(Error::SeekingFile)?;
    Ok(image_type)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Seek, SeekFrom, Write};
    use vmm_sys_util::tempfile::TempFile;
    use vmm_sys_util::write_zeroes::WriteZeroes;

    fn valid_header_v3() -> Vec<u8> {
        vec![
            0x51u8, 0x46, 0x49, 0xfb, // magic
            0x00, 0x00, 0x00, 0x03, // version
            0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // backing file offset
            0x00, 0x00, 0x00, 0x00, // backing file size
            0x00, 0x00, 0x00, 0x10, // cluster_bits
            0x00, 0x00, 0x00, 0x20, 0x00, 0x00, 0x00, 0x00, // size
            0x00, 0x00, 0x00, 0x00, // crypt method
            0x00, 0x00, 0x01, 0x00, // L1 size
            0x00, 0x00, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00, // L1 table offset
            0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, // refcount table offset
            0x00, 0x00, 0x00, 0x03, // refcount table clusters
            0x00, 0x00, 0x00, 0x00, // nb snapshots
            0x00, 0x00, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00, // snapshots offset
            0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // incompatible_features
            0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // compatible_features
            0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // autoclear_features
            0x00, 0x00, 0x00, 0x04, // refcount_order
            0x00, 0x00, 0x00, 0x68, // header_length
        ]
    }

    fn valid_header_v2() -> Vec<u8> {
        vec![
            0x51u8, 0x46, 0x49, 0xfb, // magic
            0x00, 0x00, 0x00, 0x02, // version
            0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // backing file offset
            0x00, 0x00, 0x00, 0x00, // backing file size
            0x00, 0x00, 0x00, 0x10, // cluster_bits
            0x00, 0x00, 0x00, 0x20, 0x00, 0x00, 0x00, 0x00, // size
            0x00, 0x00, 0x00, 0x00, // crypt method
            0x00, 0x00, 0x01, 0x00, // L1 size
            0x00, 0x00, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00, // L1 table offset
            0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, // refcount table offset
            0x00, 0x00, 0x00, 0x03, // refcount table clusters
            0x00, 0x00, 0x00, 0x00, // nb snapshots
            0x00, 0x00, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00, // snapshots offset
        ]
    }

    // Test case found by clusterfuzz to allocate excessive memory.
    fn test_huge_header() -> Vec<u8> {
        vec![
            0x51, 0x46, 0x49, 0xfb, // magic
            0x00, 0x00, 0x00, 0x03, // version
            0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // backing file offset
            0x00, 0x00, 0x00, 0x00, // backing file size
            0x00, 0x00, 0x00, 0x09, // cluster_bits
            0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, // size
            0x00, 0x00, 0x00, 0x00, // crypt method
            0x00, 0x00, 0x01, 0x00, // L1 size
            0x00, 0x00, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00, // L1 table offset
            0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, // refcount table offset
            0x00, 0x00, 0x00, 0x03, // refcount table clusters
            0x00, 0x00, 0x00, 0x00, // nb snapshots
            0x00, 0x00, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00, // snapshots offset
            0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // incompatible_features
            0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // compatible_features
            0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // autoclear_features
            0x00, 0x00, 0x00, 0x04, // refcount_order
            0x00, 0x00, 0x00, 0x68, // header_length
        ]
    }

    fn with_basic_file<F>(header: &[u8], mut testfn: F)
    where
        F: FnMut(RawFile),
    {
        let mut disk_file: RawFile = RawFile::new(TempFile::new().unwrap().into_file(), false);
        disk_file.write_all(header).unwrap();
        disk_file.set_len(0x1_0000_0000).unwrap();
        disk_file.seek(SeekFrom::Start(0)).unwrap();

        testfn(disk_file); // File closed when the function exits.
    }

    fn with_default_file<F>(file_size: u64, direct: bool, mut testfn: F)
    where
        F: FnMut(QcowFile),
    {
        let tmp: RawFile = RawFile::new(TempFile::new().unwrap().into_file(), direct);
        let qcow_file = QcowFile::new(tmp, 3, file_size).unwrap();

        testfn(qcow_file); // File closed when the function exits.
    }

    #[test]
    fn default_header_v2() {
        let header = QcowHeader::create_for_size(2, 0x10_0000);
        let mut disk_file: RawFile = RawFile::new(TempFile::new().unwrap().into_file(), false);
        header
            .write_to(&mut disk_file)
            .expect("Failed to write header to temporary file.");
        disk_file.seek(SeekFrom::Start(0)).unwrap();
        QcowFile::from(disk_file).expect("Failed to create Qcow from default Header");
    }

    #[test]
    fn default_header_v3() {
        let header = QcowHeader::create_for_size(3, 0x10_0000);
        let mut disk_file: RawFile = RawFile::new(TempFile::new().unwrap().into_file(), false);
        header
            .write_to(&mut disk_file)
            .expect("Failed to write header to temporary file.");
        disk_file.seek(SeekFrom::Start(0)).unwrap();
        QcowFile::from(disk_file).expect("Failed to create Qcow from default Header");
    }

    #[test]
    fn header_read() {
        with_basic_file(&valid_header_v2(), |mut disk_file: RawFile| {
            let header = QcowHeader::new(&mut disk_file).expect("Failed to create Header.");
            assert_eq!(header.version, 2);
            assert_eq!(header.refcount_order, DEFAULT_REFCOUNT_ORDER);
            assert_eq!(header.header_size, V2_BARE_HEADER_SIZE);
        });
        with_basic_file(&valid_header_v3(), |mut disk_file: RawFile| {
            let header = QcowHeader::new(&mut disk_file).expect("Failed to create Header.");
            assert_eq!(header.version, 3);
            assert_eq!(header.refcount_order, DEFAULT_REFCOUNT_ORDER);
            assert_eq!(header.header_size, V3_BARE_HEADER_SIZE);
        });
    }

    #[test]
    fn invalid_magic() {
        let invalid_header = vec![0x51u8, 0x46, 0x4a, 0xfb];
        with_basic_file(&invalid_header, |mut disk_file: RawFile| {
            QcowHeader::new(&mut disk_file).expect_err("Invalid header worked.");
        });
    }

    #[test]
    fn invalid_refcount_order() {
        let mut header = valid_header_v3();
        header[99] = 2;
        with_basic_file(&header, |disk_file: RawFile| {
            QcowFile::from(disk_file).expect_err("Invalid refcount order worked.");
        });
    }

    #[test]
    fn invalid_cluster_bits() {
        let mut header = valid_header_v3();
        header[23] = 3;
        with_basic_file(&header, |disk_file: RawFile| {
            QcowFile::from(disk_file).expect_err("Failed to create file.");
        });
    }

    #[test]
    fn test_header_huge_file() {
        let header = test_huge_header();
        with_basic_file(&header, |disk_file: RawFile| {
            QcowFile::from(disk_file).expect_err("Failed to create file.");
        });
    }

    #[test]
    fn test_header_crazy_file_size_rejected() {
        let mut header = valid_header_v3();
        header[24..32].copy_from_slice(&[0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x1e]);
        with_basic_file(&header, |disk_file: RawFile| {
            QcowFile::from(disk_file).expect_err("Failed to create file.");
        });
    }

    #[test]
    fn test_huge_l1_table() {
        let mut header = valid_header_v3();
        header[36] = 0x12;
        with_basic_file(&header, |disk_file: RawFile| {
            QcowFile::from(disk_file).expect_err("Failed to create file.");
        });
    }

    #[test]
    fn test_header_1_tb_file_min_cluster() {
        let mut header = test_huge_header();
        header[24] = 0;
        header[26] = 1;
        header[31] = 0;
        // 1 TB with the min cluster size makes the arrays too big, it should fail.
        with_basic_file(&header, |disk_file: RawFile| {
            QcowFile::from(disk_file).expect_err("Failed to create file.");
        });
    }

    #[test]
    fn test_header_1_tb_file() {
        let mut header = test_huge_header();
        // reset to 1 TB size.
        header[24] = 0;
        header[26] = 1;
        header[31] = 0;
        // set cluster_bits
        header[23] = 16;
        with_basic_file(&header, |disk_file: RawFile| {
            let mut qcow = QcowFile::from(disk_file).expect("Failed to create file.");
            qcow.seek(SeekFrom::Start(0x100_0000_0000 - 8))
                .expect("Failed to seek.");
            let value = 0x0000_0040_3f00_ffffu64;
            qcow.write_all(&value.to_le_bytes())
                .expect("failed to write data");
        });
    }

    #[test]
    fn test_header_huge_num_refcounts() {
        let mut header = valid_header_v3();
        header[56..60].copy_from_slice(&[0x02, 0x00, 0xe8, 0xff]);
        with_basic_file(&header, |disk_file: RawFile| {
            QcowFile::from(disk_file).expect_err("Created disk with crazy refcount clusters");
        });
    }

    #[test]
    fn test_header_huge_refcount_offset() {
        let mut header = valid_header_v3();
        header[48..56].copy_from_slice(&[0x00, 0x00, 0x09, 0x00, 0x00, 0x00, 0x02, 0x00]);
        with_basic_file(&header, |disk_file: RawFile| {
            QcowFile::from(disk_file).expect_err("Created disk with crazy refcount offset");
        });
    }

    #[test]
    fn write_read_start() {
        with_basic_file(&valid_header_v3(), |disk_file: RawFile| {
            let mut q = QcowFile::from(disk_file).unwrap();
            q.write_all(b"test first bytes")
                .expect("Failed to write test string.");
            let mut buf = [0u8; 4];
            q.seek(SeekFrom::Start(0)).expect("Failed to seek.");
            q.read_exact(&mut buf).expect("Failed to read.");
            assert_eq!(&buf, b"test");
        });
    }

    #[test]
    fn offset_write_read() {
        with_basic_file(&valid_header_v3(), |disk_file: RawFile| {
            let mut q = QcowFile::from(disk_file).unwrap();
            let b = [0x55u8; 0x1000];
            q.seek(SeekFrom::Start(0xfff2000)).expect("Failed to seek.");
            q.write_all(&b).expect("Failed to write test string.");
            let mut buf = [0u8; 4];
            q.seek(SeekFrom::Start(0xfff2000)).expect("Failed to seek.");
            q.read_exact(&mut buf).expect("Failed to read.");
            assert_eq!(buf[0], 0x55);
        });
    }

    #[test]
    fn write_zeroes_read() {
        with_basic_file(&valid_header_v3(), |disk_file: RawFile| {
            let mut q = QcowFile::from(disk_file).unwrap();
            // Write some test data.
            let b = [0x55u8; 0x1000];
            q.seek(SeekFrom::Start(0xfff2000)).expect("Failed to seek.");
            q.write_all(&b).expect("Failed to write test string.");
            // Overwrite the test data with zeroes.
            q.seek(SeekFrom::Start(0xfff2000)).expect("Failed to seek.");
            let nwritten = q.write_zeroes(0x200).expect("Failed to write zeroes.");
            assert_eq!(nwritten, 0x200);
            // Verify that the correct part of the data was zeroed out.
            let mut buf = [0u8; 0x1000];
            q.seek(SeekFrom::Start(0xfff2000)).expect("Failed to seek.");
            q.read_exact(&mut buf).expect("Failed to read.");
            assert_eq!(buf[0], 0);
            assert_eq!(buf[0x1FF], 0);
            assert_eq!(buf[0x200], 0x55);
            assert_eq!(buf[0xFFF], 0x55);
        });
    }

    #[test]
    fn write_zeroes_full_cluster() {
        // Choose a size that is larger than a cluster.
        // valid_header uses cluster_bits = 12, which corresponds to a cluster size of 4096.
        const CHUNK_SIZE: usize = 4096 * 2 + 512;
        with_basic_file(&valid_header_v3(), |disk_file: RawFile| {
            let mut q = QcowFile::from(disk_file).unwrap();
            // Write some test data.
            let b = [0x55u8; CHUNK_SIZE];
            q.seek(SeekFrom::Start(0)).expect("Failed to seek.");
            q.write_all(&b).expect("Failed to write test string.");
            // Overwrite the full cluster with zeroes.
            q.seek(SeekFrom::Start(0)).expect("Failed to seek.");
            let nwritten = q.write_zeroes(CHUNK_SIZE).expect("Failed to write zeroes.");
            assert_eq!(nwritten, CHUNK_SIZE);
            // Verify that the data was zeroed out.
            let mut buf = [0u8; CHUNK_SIZE];
            q.seek(SeekFrom::Start(0)).expect("Failed to seek.");
            q.read_exact(&mut buf).expect("Failed to read.");
            assert_eq!(buf[0], 0);
            assert_eq!(buf[CHUNK_SIZE - 1], 0);
        });
    }

    #[test]
    fn test_header() {
        with_basic_file(&valid_header_v2(), |disk_file: RawFile| {
            let q = QcowFile::from(disk_file).unwrap();
            assert_eq!(q.virtual_size(), 0x20_0000_0000);
        });
        with_basic_file(&valid_header_v3(), |disk_file: RawFile| {
            let q = QcowFile::from(disk_file).unwrap();
            assert_eq!(q.virtual_size(), 0x20_0000_0000);
        });
    }

    #[test]
    fn read_small_buffer() {
        with_basic_file(&valid_header_v3(), |disk_file: RawFile| {
            let mut q = QcowFile::from(disk_file).unwrap();
            let mut b = [5u8; 16];
            q.seek(SeekFrom::Start(1000)).expect("Failed to seek.");
            q.read_exact(&mut b).expect("Failed to read.");
            assert_eq!(0, b[0]);
            assert_eq!(0, b[15]);
        });
    }

    #[test]
    fn replay_ext4() {
        with_basic_file(&valid_header_v3(), |disk_file: RawFile| {
            let mut q = QcowFile::from(disk_file).unwrap();
            const BUF_SIZE: usize = 0x1000;
            let mut b = [0u8; BUF_SIZE];

            struct Transfer {
                pub write: bool,
                pub addr: u64,
            }

            // Write transactions from mkfs.ext4.
            let xfers: Vec<Transfer> = vec![
                Transfer {
                    write: false,
                    addr: 0xfff0000,
                },
                Transfer {
                    write: false,
                    addr: 0xfffe000,
                },
                Transfer {
                    write: false,
                    addr: 0x0,
                },
                Transfer {
                    write: false,
                    addr: 0x1000,
                },
                Transfer {
                    write: false,
                    addr: 0xffff000,
                },
                Transfer {
                    write: false,
                    addr: 0xffdf000,
                },
                Transfer {
                    write: false,
                    addr: 0xfff8000,
                },
                Transfer {
                    write: false,
                    addr: 0xffe0000,
                },
                Transfer {
                    write: false,
                    addr: 0xffce000,
                },
                Transfer {
                    write: false,
                    addr: 0xffb6000,
                },
                Transfer {
                    write: false,
                    addr: 0xffab000,
                },
                Transfer {
                    write: false,
                    addr: 0xffa4000,
                },
                Transfer {
                    write: false,
                    addr: 0xff8e000,
                },
                Transfer {
                    write: false,
                    addr: 0xff86000,
                },
                Transfer {
                    write: false,
                    addr: 0xff84000,
                },
                Transfer {
                    write: false,
                    addr: 0xff89000,
                },
                Transfer {
                    write: false,
                    addr: 0xfe7e000,
                },
                Transfer {
                    write: false,
                    addr: 0x100000,
                },
                Transfer {
                    write: false,
                    addr: 0x3000,
                },
                Transfer {
                    write: false,
                    addr: 0x7000,
                },
                Transfer {
                    write: false,
                    addr: 0xf000,
                },
                Transfer {
                    write: false,
                    addr: 0x2000,
                },
                Transfer {
                    write: false,
                    addr: 0x4000,
                },
                Transfer {
                    write: false,
                    addr: 0x5000,
                },
                Transfer {
                    write: false,
                    addr: 0x6000,
                },
                Transfer {
                    write: false,
                    addr: 0x8000,
                },
                Transfer {
                    write: false,
                    addr: 0x9000,
                },
                Transfer {
                    write: false,
                    addr: 0xa000,
                },
                Transfer {
                    write: false,
                    addr: 0xb000,
                },
                Transfer {
                    write: false,
                    addr: 0xc000,
                },
                Transfer {
                    write: false,
                    addr: 0xd000,
                },
                Transfer {
                    write: false,
                    addr: 0xe000,
                },
                Transfer {
                    write: false,
                    addr: 0x10000,
                },
                Transfer {
                    write: false,
                    addr: 0x11000,
                },
                Transfer {
                    write: false,
                    addr: 0x12000,
                },
                Transfer {
                    write: false,
                    addr: 0x13000,
                },
                Transfer {
                    write: false,
                    addr: 0x14000,
                },
                Transfer {
                    write: false,
                    addr: 0x15000,
                },
                Transfer {
                    write: false,
                    addr: 0x16000,
                },
                Transfer {
                    write: false,
                    addr: 0x17000,
                },
                Transfer {
                    write: false,
                    addr: 0x18000,
                },
                Transfer {
                    write: false,
                    addr: 0x19000,
                },
                Transfer {
                    write: false,
                    addr: 0x1a000,
                },
                Transfer {
                    write: false,
                    addr: 0x1b000,
                },
                Transfer {
                    write: false,
                    addr: 0x1c000,
                },
                Transfer {
                    write: false,
                    addr: 0x1d000,
                },
                Transfer {
                    write: false,
                    addr: 0x1e000,
                },
                Transfer {
                    write: false,
                    addr: 0x1f000,
                },
                Transfer {
                    write: false,
                    addr: 0x21000,
                },
                Transfer {
                    write: false,
                    addr: 0x22000,
                },
                Transfer {
                    write: false,
                    addr: 0x24000,
                },
                Transfer {
                    write: false,
                    addr: 0x40000,
                },
                Transfer {
                    write: false,
                    addr: 0x0,
                },
                Transfer {
                    write: false,
                    addr: 0x3000,
                },
                Transfer {
                    write: false,
                    addr: 0x7000,
                },
                Transfer {
                    write: false,
                    addr: 0x0,
                },
                Transfer {
                    write: false,
                    addr: 0x1000,
                },
                Transfer {
                    write: false,
                    addr: 0x2000,
                },
                Transfer {
                    write: false,
                    addr: 0x3000,
                },
                Transfer {
                    write: false,
                    addr: 0x0,
                },
                Transfer {
                    write: false,
                    addr: 0x449000,
                },
                Transfer {
                    write: false,
                    addr: 0x48000,
                },
                Transfer {
                    write: false,
                    addr: 0x48000,
                },
                Transfer {
                    write: false,
                    addr: 0x448000,
                },
                Transfer {
                    write: false,
                    addr: 0x44a000,
                },
                Transfer {
                    write: false,
                    addr: 0x48000,
                },
                Transfer {
                    write: false,
                    addr: 0x48000,
                },
                Transfer {
                    write: true,
                    addr: 0x0,
                },
                Transfer {
                    write: true,
                    addr: 0x448000,
                },
                Transfer {
                    write: true,
                    addr: 0x449000,
                },
                Transfer {
                    write: true,
                    addr: 0x44a000,
                },
                Transfer {
                    write: true,
                    addr: 0xfff0000,
                },
                Transfer {
                    write: true,
                    addr: 0xfff1000,
                },
                Transfer {
                    write: true,
                    addr: 0xfff2000,
                },
                Transfer {
                    write: true,
                    addr: 0xfff3000,
                },
                Transfer {
                    write: true,
                    addr: 0xfff4000,
                },
                Transfer {
                    write: true,
                    addr: 0xfff5000,
                },
                Transfer {
                    write: true,
                    addr: 0xfff6000,
                },
                Transfer {
                    write: true,
                    addr: 0xfff7000,
                },
                Transfer {
                    write: true,
                    addr: 0xfff8000,
                },
                Transfer {
                    write: true,
                    addr: 0xfff9000,
                },
                Transfer {
                    write: true,
                    addr: 0xfffa000,
                },
                Transfer {
                    write: true,
                    addr: 0xfffb000,
                },
                Transfer {
                    write: true,
                    addr: 0xfffc000,
                },
                Transfer {
                    write: true,
                    addr: 0xfffd000,
                },
                Transfer {
                    write: true,
                    addr: 0xfffe000,
                },
                Transfer {
                    write: true,
                    addr: 0xffff000,
                },
            ];

            for xfer in &xfers {
                q.seek(SeekFrom::Start(xfer.addr)).expect("Failed to seek.");
                if xfer.write {
                    q.write_all(&b).expect("Failed to write.");
                } else {
                    let read_count: usize = q.read(&mut b).expect("Failed to read.");
                    assert_eq!(read_count, BUF_SIZE);
                }
            }
        });
    }

    #[test]
    fn combo_write_read() {
        combo_write_read_common(false);
    }

    #[test]
    fn combo_write_read_direct() {
        combo_write_read_common(true);
    }

    fn combo_write_read_common(direct: bool) {
        with_default_file(1024 * 1024 * 1024 * 256, direct, |mut qcow_file| {
            const NUM_BLOCKS: usize = 555;
            const BLOCK_SIZE: usize = 0x1_0000;
            const OFFSET: usize = 0x1_0000_0020;
            let data = [0x55u8; BLOCK_SIZE];
            let mut readback = [0u8; BLOCK_SIZE];
            for i in 0..NUM_BLOCKS {
                let seek_offset = OFFSET + i * BLOCK_SIZE;
                qcow_file
                    .seek(SeekFrom::Start(seek_offset as u64))
                    .expect("Failed to seek.");
                let nwritten = qcow_file.write(&data).expect("Failed to write test data.");
                assert_eq!(nwritten, BLOCK_SIZE);
                // Read back the data to check it was written correctly.
                qcow_file
                    .seek(SeekFrom::Start(seek_offset as u64))
                    .expect("Failed to seek.");
                let nread = qcow_file.read(&mut readback).expect("Failed to read.");
                assert_eq!(nread, BLOCK_SIZE);
                for (orig, read) in data.iter().zip(readback.iter()) {
                    assert_eq!(orig, read);
                }
            }
            // Check that address 0 is still zeros.
            qcow_file.seek(SeekFrom::Start(0)).expect("Failed to seek.");
            let nread = qcow_file.read(&mut readback).expect("Failed to read.");
            assert_eq!(nread, BLOCK_SIZE);
            for read in readback.iter() {
                assert_eq!(*read, 0);
            }
            // Check the data again after the writes have happened.
            for i in 0..NUM_BLOCKS {
                let seek_offset = OFFSET + i * BLOCK_SIZE;
                qcow_file
                    .seek(SeekFrom::Start(seek_offset as u64))
                    .expect("Failed to seek.");
                let nread = qcow_file.read(&mut readback).expect("Failed to read.");
                assert_eq!(nread, BLOCK_SIZE);
                for (orig, read) in data.iter().zip(readback.iter()) {
                    assert_eq!(orig, read);
                }
            }

            assert_eq!(qcow_file.first_zero_refcount().unwrap(), None);
        });
    }

    fn seek_cur(file: &mut QcowFile) -> u64 {
        file.seek(SeekFrom::Current(0)).unwrap()
    }

    #[test]
    fn seek_data() {
        seek_data_common(false);
    }

    #[test]
    fn seek_data_direct() {
        seek_data_common(true);
    }

    fn seek_data_common(direct: bool) {
        with_default_file(0x30000, direct, |mut file| {
            // seek_data at or after the end of the file should return None
            assert_eq!(file.seek_data(0x10000).unwrap(), None);
            assert_eq!(seek_cur(&mut file), 0);
            assert_eq!(file.seek_data(0x10001).unwrap(), None);
            assert_eq!(seek_cur(&mut file), 0);

            // Write some data to [0x10000, 0x20000)
            let b = [0x55u8; 0x10000];
            file.seek(SeekFrom::Start(0x10000)).unwrap();
            file.write_all(&b).unwrap();
            assert_eq!(file.seek_data(0).unwrap(), Some(0x10000));
            assert_eq!(seek_cur(&mut file), 0x10000);

            // seek_data within data should return the same offset
            assert_eq!(file.seek_data(0x10000).unwrap(), Some(0x10000));
            assert_eq!(seek_cur(&mut file), 0x10000);
            assert_eq!(file.seek_data(0x10001).unwrap(), Some(0x10001));
            assert_eq!(seek_cur(&mut file), 0x10001);
            assert_eq!(file.seek_data(0x1FFFF).unwrap(), Some(0x1FFFF));
            assert_eq!(seek_cur(&mut file), 0x1FFFF);

            assert_eq!(file.seek_data(0).unwrap(), Some(0x10000));
            assert_eq!(seek_cur(&mut file), 0x10000);
            assert_eq!(file.seek_data(0x1FFFF).unwrap(), Some(0x1FFFF));
            assert_eq!(seek_cur(&mut file), 0x1FFFF);
            assert_eq!(file.seek_data(0x20000).unwrap(), None);
            assert_eq!(seek_cur(&mut file), 0x1FFFF);
        });
    }

    #[test]
    fn seek_hole() {
        seek_hole_common(false);
    }

    #[test]
    fn seek_hole_direct() {
        seek_hole_common(true);
    }

    fn seek_hole_common(direct: bool) {
        with_default_file(0x30000, direct, |mut file| {
            // File consisting entirely of a hole
            assert_eq!(file.seek_hole(0).unwrap(), Some(0));
            assert_eq!(seek_cur(&mut file), 0);
            assert_eq!(file.seek_hole(0xFFFF).unwrap(), Some(0xFFFF));
            assert_eq!(seek_cur(&mut file), 0xFFFF);

            // seek_hole at or after the end of the file should return None
            file.seek(SeekFrom::Start(0)).unwrap();
            assert_eq!(file.seek_hole(0x30000).unwrap(), None);
            assert_eq!(seek_cur(&mut file), 0);
            assert_eq!(file.seek_hole(0x30001).unwrap(), None);
            assert_eq!(seek_cur(&mut file), 0);

            // Write some data to [0x10000, 0x20000)
            let b = [0x55u8; 0x10000];
            file.seek(SeekFrom::Start(0x10000)).unwrap();
            file.write_all(&b).unwrap();

            // seek_hole within a hole should return the same offset
            assert_eq!(file.seek_hole(0).unwrap(), Some(0));
            assert_eq!(seek_cur(&mut file), 0);
            assert_eq!(file.seek_hole(0xFFFF).unwrap(), Some(0xFFFF));
            assert_eq!(seek_cur(&mut file), 0xFFFF);

            // seek_hole within data should return the next hole
            file.seek(SeekFrom::Start(0)).unwrap();
            assert_eq!(file.seek_hole(0x10000).unwrap(), Some(0x20000));
            assert_eq!(seek_cur(&mut file), 0x20000);
            file.seek(SeekFrom::Start(0)).unwrap();
            assert_eq!(file.seek_hole(0x10001).unwrap(), Some(0x20000));
            assert_eq!(seek_cur(&mut file), 0x20000);
            file.seek(SeekFrom::Start(0)).unwrap();
            assert_eq!(file.seek_hole(0x1FFFF).unwrap(), Some(0x20000));
            assert_eq!(seek_cur(&mut file), 0x20000);
            file.seek(SeekFrom::Start(0)).unwrap();
            assert_eq!(file.seek_hole(0xFFFF).unwrap(), Some(0xFFFF));
            assert_eq!(seek_cur(&mut file), 0xFFFF);
            file.seek(SeekFrom::Start(0)).unwrap();
            assert_eq!(file.seek_hole(0x10000).unwrap(), Some(0x20000));
            assert_eq!(seek_cur(&mut file), 0x20000);
            file.seek(SeekFrom::Start(0)).unwrap();
            assert_eq!(file.seek_hole(0x1FFFF).unwrap(), Some(0x20000));
            assert_eq!(seek_cur(&mut file), 0x20000);
            file.seek(SeekFrom::Start(0)).unwrap();
            assert_eq!(file.seek_hole(0x20000).unwrap(), Some(0x20000));
            assert_eq!(seek_cur(&mut file), 0x20000);
            file.seek(SeekFrom::Start(0)).unwrap();
            assert_eq!(file.seek_hole(0x20001).unwrap(), Some(0x20001));
            assert_eq!(seek_cur(&mut file), 0x20001);

            // seek_hole at EOF should return None
            file.seek(SeekFrom::Start(0)).unwrap();
            assert_eq!(file.seek_hole(0x30000).unwrap(), None);
            assert_eq!(seek_cur(&mut file), 0);

            // Write some data to [0x20000, 0x30000)
            file.seek(SeekFrom::Start(0x20000)).unwrap();
            file.write_all(&b).unwrap();

            // seek_hole within [0x20000, 0x30000) should now find the hole at EOF
            assert_eq!(file.seek_hole(0x20000).unwrap(), Some(0x30000));
            assert_eq!(seek_cur(&mut file), 0x30000);
            file.seek(SeekFrom::Start(0)).unwrap();
            assert_eq!(file.seek_hole(0x20001).unwrap(), Some(0x30000));
            assert_eq!(seek_cur(&mut file), 0x30000);
            file.seek(SeekFrom::Start(0)).unwrap();
            assert_eq!(file.seek_hole(0x30000).unwrap(), None);
            assert_eq!(seek_cur(&mut file), 0);
        });
    }

    #[test]
    fn rebuild_refcounts() {
        with_basic_file(&valid_header_v3(), |mut disk_file: RawFile| {
            let header = QcowHeader::new(&mut disk_file).expect("Failed to create Header.");
            let cluster_size = 65536;
            let mut raw_file =
                QcowRawFile::from(disk_file, cluster_size).expect("Failed to create QcowRawFile.");
            QcowFile::rebuild_refcounts(&mut raw_file, header)
                .expect("Failed to rebuild recounts.");
        });
    }
}

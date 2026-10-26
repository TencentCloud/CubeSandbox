// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

use crate::vhdx_header::RegionTableEntry;
use byteorder::{LittleEndian, ReadBytesExt};
use remain::sorted;
use std::fs::File;
use std::io::{self, Read, Seek, SeekFrom};
use std::mem::size_of;
use thiserror::Error;
use uuid::Uuid;

const METADATA_SIGN: u64 = 0x6174_6164_6174_656D;
const METADATA_ENTRY_SIZE: usize = 32;
const METADATA_MAX_ENTRIES: u16 = 2047;
// The size including the table header and entries
const METADATA_TABLE_MAX_SIZE: usize = METADATA_ENTRY_SIZE * (METADATA_MAX_ENTRIES as usize + 1);

const METADATA_FLAGS_IS_REQUIRED: u32 = 0x04;

pub const BLOCK_SIZE_MIN: u32 = 1 << 20; // 1 MiB
const BLOCK_SIZE_MAX: u32 = 256 << 20; // 256 MiB
const MAX_SECTORS_PER_BLOCK: u64 = 1 << 23;

const BLOCK_HAS_PARENT: u32 = 0x02; // Has a parent or a backing file

// GUID for known metadata items
const METADATA_FILE_PARAMETER: &str = "CAA16737-FA36-4D43-B3B6-33F0AA44E76B";
const METADATA_VIRTUAL_DISK_SIZE: &str = "2FA54224-CD1B-4876-B211-5DBED83BF4B8";
const METADATA_VIRTUAL_DISK_ID: &str = "BECA12AB-B2E6-4523-93EF-C309E000C746";
const METADATA_LOGICAL_SECTOR_SIZE: &str = "8141BF1D-A96F-4709-BA47-F233A8FAAB5F";
const METADATA_PHYSICAL_SECTOR_SIZE: &str = "CDA348C7-445D-4471-9CC9-E9885251C556";
const METADATA_PARENT_LOCATOR: &str = "A8D35F2D-B30B-454D-ABF7-D3D84834AB0C";

const METADATA_FILE_PARAMETER_PRESENT: u16 = 0x01;
const METADATA_VIRTUAL_DISK_SIZE_PRESENT: u16 = 0x02;
const METADATA_VIRTUAL_DISK_ID_PRESENT: u16 = 0x04;
const METADATA_LOGICAL_SECTOR_SIZE_PRESENT: u16 = 0x08;
const METADATA_PHYSICAL_SECTOR_SIZE_PRESENT: u16 = 0x10;
const METADATA_PARENT_LOCATOR_PRESENT: u16 = 0x20;

const METADATA_ALL_PRESENT: u16 = METADATA_FILE_PARAMETER_PRESENT
    | METADATA_VIRTUAL_DISK_SIZE_PRESENT
    | METADATA_VIRTUAL_DISK_ID_PRESENT
    | METADATA_LOGICAL_SECTOR_SIZE_PRESENT
    | METADATA_PHYSICAL_SECTOR_SIZE_PRESENT;

const METADATA_LENGTH_MAX: u32 = 1 << 20; // 1 MiB

#[sorted]
#[derive(Error, Debug)]
pub enum VhdxMetadataError {
    #[error("Invalid block size count")]
    InvalidBlockSize,
    #[error("Invalid metadata entry count")]
    InvalidEntryCount,
    #[error("Invalid logical sector size")]
    InvalidLogicalSectorSize,
    #[error("Invalid metadata ID")]
    InvalidMetadataItem,
    #[error("Invalid metadata length")]
    InvalidMetadataLength,
    #[error("Metadata sign doesn't match")]
    InvalidMetadataSign,
    #[error("Invalid physical sector size")]
    InvalidPhysicalSectorSize,
    #[error("Invalid UUID")]
    InvalidUuid(#[source] uuid::Error),
    #[error("Invalid value")]
    InvalidValue,
    #[error("Not all required metadata found")]
    MissingMetadata,
    #[error("Failed to read metadata headers {0}")]
    ReadMetadata(#[source] io::Error),
    #[error("Reserved region has non-zero value")]
    ReservedIsNonZero,
    #[error("This implementation doesn't support this metadata flag")]
    UnsupportedFlag,
}

pub type Result<T> = std::result::Result<T, VhdxMetadataError>;

#[derive(Default, Clone, Debug)]
pub struct DiskSpec {
    pub disk_id: u128,
    pub image_size: u64,
    pub block_size: u32,
    pub has_parent: bool,
    pub sectors_per_block: u32,
    pub virtual_disk_size: u64,
    pub logical_sector_size: u32,
    pub physical_sector_size: u32,
    pub chunk_ratio: u64,
    pub total_sectors: u64,
}

impl DiskSpec {
    /// Parse all meatadata from the provided file and store info in DiskSpec
    /// structure.
    pub fn new(f: &mut File, metadata_region: &RegionTableEntry) -> Result<DiskSpec> {
        let mut disk_spec = DiskSpec::default();
        let mut metadata_presence: u16 = 0;
        let mut offset = 0;
        let metadata = f.metadata().map_err(VhdxMetadataError::ReadMetadata)?;
        disk_spec.image_size = metadata.len();

        let mut buffer = [0u8; METADATA_TABLE_MAX_SIZE];
        f.seek(SeekFrom::Start(metadata_region.file_offset))
            .map_err(VhdxMetadataError::ReadMetadata)?;
        f.read_exact(&mut buffer)
            .map_err(VhdxMetadataError::ReadMetadata)?;

        let metadata_header =
            MetadataTableHeader::new(&buffer[0..size_of::<MetadataTableHeader>()])?;

        offset += size_of::<MetadataTableHeader>();
        for _ in 0..metadata_header.entry_count {
            let metadata_entry =
                MetadataTableEntry::new(&buffer[offset..offset + size_of::<MetadataTableEntry>()])?;

            f.seek(SeekFrom::Start(
                metadata_region.file_offset + metadata_entry.offset as u64,
            ))
            .map_err(VhdxMetadataError::ReadMetadata)?;

            if metadata_entry.item_id
                == Uuid::parse_str(METADATA_FILE_PARAMETER)
                    .map_err(VhdxMetadataError::InvalidUuid)?
            {
                disk_spec.block_size = f
                    .read_u32::<LittleEndian>()
                    .map_err(VhdxMetadataError::ReadMetadata)?;

                // MUST be at least 1 MiB and not greater than 256 MiB
                if disk_spec.block_size < BLOCK_SIZE_MIN && disk_spec.block_size > BLOCK_SIZE_MAX {
                    return Err(VhdxMetadataError::InvalidBlockSize);
                }

                // MUST be power of 2
                if !disk_spec.block_size.is_power_of_two() {
                    return Err(VhdxMetadataError::InvalidBlockSize);
                }

                let bits = f
                    .read_u32::<LittleEndian>()
                    .map_err(VhdxMetadataError::ReadMetadata)?;
                disk_spec.has_parent = bits & BLOCK_HAS_PARENT != 0;

                metadata_presence |= METADATA_FILE_PARAMETER_PRESENT;
            } else if metadata_entry.item_id
                == Uuid::parse_str(METADATA_VIRTUAL_DISK_SIZE)
                    .map_err(VhdxMetadataError::InvalidUuid)?
            {
                disk_spec.virtual_disk_size = f
                    .read_u64::<LittleEndian>()
                    .map_err(VhdxMetadataError::ReadMetadata)?;

                metadata_presence |= METADATA_VIRTUAL_DISK_SIZE_PRESENT;
            } else if metadata_entry.item_id
                == Uuid::parse_str(METADATA_VIRTUAL_DISK_ID)
                    .map_err(VhdxMetadataError::InvalidUuid)?
            {
                disk_spec.disk_id = f
                    .read_u128::<LittleEndian>()
                    .map_err(VhdxMetadataError::ReadMetadata)?;

                metadata_presence |= METADATA_VIRTUAL_DISK_ID_PRESENT;
            } else if metadata_entry.item_id
                == Uuid::parse_str(METADATA_LOGICAL_SECTOR_SIZE)
                    .map_err(VhdxMetadataError::InvalidUuid)?
            {
                disk_spec.logical_sector_size = f
                    .read_u32::<LittleEndian>()
                    .map_err(VhdxMetadataError::ReadMetadata)?;
                if !(disk_spec.logical_sector_size == 512 || disk_spec.logical_sector_size == 4096)
                {
                    return Err(VhdxMetadataError::InvalidLogicalSectorSize);
                }

                metadata_presence |= METADATA_LOGICAL_SECTOR_SIZE_PRESENT;
            } else if metadata_entry.item_id
                == Uuid::parse_str(METADATA_PHYSICAL_SECTOR_SIZE)
                    .map_err(VhdxMetadataError::InvalidUuid)?
            {
                disk_spec.physical_sector_size = f
                    .read_u32::<LittleEndian>()
                    .map_err(VhdxMetadataError::ReadMetadata)?;
                if !(disk_spec.physical_sector_size == 512
                    || disk_spec.physical_sector_size == 4096)
                {
                    return Err(VhdxMetadataError::InvalidPhysicalSectorSize);
                }

                metadata_presence |= METADATA_PHYSICAL_SECTOR_SIZE_PRESENT;
            } else if metadata_entry.item_id
                == Uuid::parse_str(METADATA_PARENT_LOCATOR)
                    .map_err(VhdxMetadataError::InvalidUuid)?
            {
                metadata_presence |= METADATA_PARENT_LOCATOR_PRESENT;
            } else {
                return Err(VhdxMetadataError::InvalidMetadataItem);
            }

            if (metadata_entry.flag_bits & METADATA_FLAGS_IS_REQUIRED) == 0 {
                return Err(VhdxMetadataError::UnsupportedFlag);
            }
            offset += size_of::<MetadataTableEntry>();
        }

        // Check if all required metadata are present
        if metadata_presence != METADATA_ALL_PRESENT {
            return Err(VhdxMetadataError::MissingMetadata);
        }
        // Check if the virtual disk size is a multiple of the logical sector
        // size.
        if ((metadata_presence & METADATA_LOGICAL_SECTOR_SIZE_PRESENT) != 0)
            && (disk_spec.virtual_disk_size % disk_spec.logical_sector_size as u64 != 0)
        {
            return Err(VhdxMetadataError::InvalidBlockSize);
        }

        disk_spec.sectors_per_block =
            DiskSpec::sectors_per_block(disk_spec.block_size, disk_spec.logical_sector_size)?;

        disk_spec.chunk_ratio =
            DiskSpec::chunk_ratio(disk_spec.block_size, disk_spec.logical_sector_size)?;

        disk_spec.total_sectors =
            disk_spec.virtual_disk_size / disk_spec.logical_sector_size as u64;

        Ok(disk_spec)
    }

    /// Calculates the number of sectors per block
    fn sectors_per_block(block_size: u32, logical_sector_size: u32) -> Result<u32> {
        let sectors_per_block = block_size / logical_sector_size;

        if !sectors_per_block.is_power_of_two() {
            return Err(VhdxMetadataError::InvalidValue);
        }

        Ok(sectors_per_block)
    }

    /// Calculate the chunk ratio
    fn chunk_ratio(block_size: u32, logical_sector_size: u32) -> Result<u64> {
        let chunk_ratio = (MAX_SECTORS_PER_BLOCK * logical_sector_size as u64) / block_size as u64;

        if !chunk_ratio.is_power_of_two() {
            return Err(VhdxMetadataError::InvalidValue);
        }

        Ok(chunk_ratio)
    }
}

#[repr(packed)]
#[derive(Default, Debug, Clone, Copy)]
struct MetadataTableHeader {
    signature: u64,
    reserved: u16,
    entry_count: u16,
    _reserved2: [u8; 20],
}

impl MetadataTableHeader {
    pub fn new(buffer: &[u8]) -> Result<MetadataTableHeader> {
        let metadata_table_header = unsafe { *(buffer.as_ptr() as *mut MetadataTableHeader) };

        if metadata_table_header.signature != METADATA_SIGN {
            return Err(VhdxMetadataError::InvalidMetadataSign);
        }

        if metadata_table_header.entry_count > METADATA_MAX_ENTRIES {
            return Err(VhdxMetadataError::InvalidEntryCount);
        }

        if metadata_table_header.reserved != 0 {
            return Err(VhdxMetadataError::ReservedIsNonZero);
        }

        Ok(metadata_table_header)
    }
}

#[repr(packed)]
#[derive(Default, Debug, Clone, Copy)]
pub struct MetadataTableEntry {
    item_id: Uuid,
    offset: u32,
    length: u32,
    flag_bits: u32,
    reserved: u32,
}

impl MetadataTableEntry {
    /// Parse one metadata entry from the buffer
    fn new(buffer: &[u8]) -> Result<MetadataTableEntry> {
        let mut metadata_table_entry = unsafe { *(buffer.as_ptr() as *mut MetadataTableEntry) };

        let uuid = crate::uuid_from_guid(buffer);
        metadata_table_entry.item_id = uuid;

        if metadata_table_entry.length > METADATA_LENGTH_MAX {
            return Err(VhdxMetadataError::InvalidMetadataLength);
        }

        if metadata_table_entry.length == 0 && metadata_table_entry.offset != 0 {
            return Err(VhdxMetadataError::InvalidMetadataLength);
        }

        if metadata_table_entry.reserved != 0 {
            return Err(VhdxMetadataError::ReservedIsNonZero);
        }

        Ok(metadata_table_entry)
    }
}

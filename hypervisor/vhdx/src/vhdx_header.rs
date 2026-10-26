// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

extern crate log;

use byteorder::{ByteOrder, LittleEndian, ReadBytesExt};
use remain::sorted;
use std::collections::btree_map::BTreeMap;
use std::convert::TryInto;
use std::fs::File;
use std::io::{self, Read, Seek, SeekFrom, Write};
use std::mem::size_of;
use thiserror::Error;
use uuid::Uuid;

const VHDX_SIGN: u64 = 0x656C_6966_7864_6876; // "vhdxfile"
const HEADER_SIGN: u32 = 0x6461_6568; // "head"
const REGION_SIGN: u32 = 0x6967_6572; // "regi"

const FILE_START: u64 = 0; // The first element
const HEADER_1_START: u64 = 64 * 1024; // Header 1 start in Bytes
const HEADER_2_START: u64 = 128 * 1024; // Header 2 start in Bytes
pub const REGION_TABLE_1_START: u64 = 192 * 1024; // Region 1 start in Bytes
const REGION_TABLE_2_START: u64 = 256 * 1024; // Region 2 start in Bytes

const HEADER_SIZE: u64 = 4 * 1024; // Each header is 64 KiB, but only first 4 kiB contains info
const REGION_SIZE: u64 = 64 * 1024; // Each region size is 64 KiB

const REGION_ENTRY_REQUIRED: u32 = 1;

const BAT_GUID: &str = "2DC27766-F623-4200-9D64-115E9BFD4A08"; // BAT GUID
const MDR_GUID: &str = "8B7CA206-4790-4B9A-B8FE-575F050F886E"; // Metadata GUID

#[sorted]
#[derive(Error, Debug)]
pub enum VhdxHeaderError {
    #[error("Failed to calculate checksum")]
    CalculateChecksum,
    #[error("BAT entry is not unique")]
    DuplicateBATEntry,
    #[error("Metadata region entry is not unique")]
    DuplicateMDREntry,
    #[error("Checksum doesn't match for")]
    InvalidChecksum(String),
    #[error("Invalid entry count")]
    InvalidEntryCount,
    #[error("Not a valid VHDx header")]
    InvalidHeaderSign,
    #[error("Not a valid VHDx region")]
    InvalidRegionSign,
    #[error("Couldn't parse Uuid for region entry {0}")]
    InvalidUuid(#[source] uuid::Error),
    #[error("Not a VHDx file")]
    InvalidVHDXSign,
    #[error("No valid header found")]
    NoValidHeader,
    #[error("Cannot read checksum")]
    ReadChecksum,
    #[error("Failed to read File Type Identifier {0}")]
    ReadFileTypeIdentifier(#[source] io::Error),
    #[error("Failed to read headers {0}")]
    ReadHeader(#[source] io::Error),
    #[error("Failed to read metadata {0}")]
    ReadMetadata(#[source] std::io::Error),
    #[error("Failed to read region table entries {0}")]
    ReadRegionTableEntries(#[source] io::Error),
    #[error("Failed to read region table header {0}")]
    ReadRegionTableHeader(#[source] io::Error),
    #[error("Failed to read region entries")]
    RegionEntryCollectionFailed,
    #[error("Overlapping regions found")]
    RegionOverlap,
    #[error("Reserved region has non-zero value")]
    ReservedIsNonZero,
    #[error("Failed to seek in File Type Identifier {0}")]
    SeekFileTypeIdentifier(#[source] io::Error),
    #[error("Failed to seek in headers {0}")]
    SeekHeader(#[source] io::Error),
    #[error("Failed to seek in region table entries {0}")]
    SeekRegionTableEntries(#[source] io::Error),
    #[error("Failed to seek in region table header {0}")]
    SeekRegionTableHeader(#[source] io::Error),
    #[error("We do not recongize this entry")]
    UnrecognizedRegionEntry,
    #[error("Failed to write header {0}")]
    WriteHeader(#[source] io::Error),
}

pub type Result<T> = std::result::Result<T, VhdxHeaderError>;

#[derive(Clone, Debug)]
pub struct FileTypeIdentifier {
    pub signature: u64,
}

impl FileTypeIdentifier {
    /// Reads the File Type Identifier structure from a reference VHDx file
    pub fn new(f: &mut File) -> Result<FileTypeIdentifier> {
        f.seek(SeekFrom::Start(FILE_START))
            .map_err(VhdxHeaderError::SeekFileTypeIdentifier)?;
        let signature = f
            .read_u64::<LittleEndian>()
            .map_err(VhdxHeaderError::ReadFileTypeIdentifier)?;
        if signature != VHDX_SIGN {
            return Err(VhdxHeaderError::InvalidVHDXSign);
        }

        Ok(FileTypeIdentifier { signature })
    }
}

#[repr(packed)]
#[derive(Clone, Copy, Debug)]
pub struct Header {
    pub signature: u32,
    pub checksum: u32,
    pub sequence_number: u64,
    pub file_write_guid: u128,
    pub data_write_guid: u128,
    pub log_guid: u128,
    pub log_version: u16,
    pub version: u16,
    pub log_length: u32,
    pub log_offset: u64,
}

impl Header {
    /// Reads the Header structure from a reference VHDx file
    pub fn new(f: &mut File, start: u64) -> Result<Header> {
        // Read the whole header in to a buffer. We will need it for
        // calculating checksum.
        let mut buffer = [0; HEADER_SIZE as usize];
        f.seek(SeekFrom::Start(start))
            .map_err(VhdxHeaderError::SeekHeader)?;
        f.read_exact(&mut buffer)
            .map_err(VhdxHeaderError::ReadHeader)?;

        let header = unsafe { *(buffer.as_ptr() as *mut Header) };
        if header.signature != HEADER_SIGN {
            return Err(VhdxHeaderError::InvalidHeaderSign);
        }

        let new_checksum = calculate_checksum(&mut buffer, size_of::<u32>())?;
        if header.checksum != new_checksum {
            return Err(VhdxHeaderError::InvalidChecksum(String::from("Header")));
        }

        Ok(header)
    }

    /// Converts the header structure into a buffer
    fn get_header_as_buffer(&self, buffer: &mut [u8; HEADER_SIZE as usize]) {
        let reference = unsafe {
            std::slice::from_raw_parts(self as *const Header as *const u8, HEADER_SIZE as usize)
        };
        *buffer = reference.try_into().unwrap();
    }

    /// Creates and returns new updated header from the provided current header
    pub fn update_header(
        f: &mut File,
        current_header: &Header,
        change_data_guid: bool,
        mut file_write_guid: u128,
        start: u64,
    ) -> Result<Header> {
        let mut buffer = [0u8; HEADER_SIZE as usize];
        let mut data_write_guid = current_header.data_write_guid;

        if change_data_guid {
            data_write_guid = Uuid::new_v4().as_u128();
        }

        if file_write_guid == 0 {
            file_write_guid = current_header.file_write_guid;
        }

        let mut new_header = Header {
            signature: current_header.signature,
            checksum: 0,
            sequence_number: current_header.sequence_number + 1,
            file_write_guid,
            data_write_guid,
            log_guid: current_header.log_guid,
            log_version: current_header.log_version,
            version: current_header.version,
            log_length: current_header.log_length,
            log_offset: current_header.log_offset,
        };

        new_header.get_header_as_buffer(&mut buffer);
        new_header.checksum = crc32c::crc32c(&buffer);
        new_header.get_header_as_buffer(&mut buffer);

        f.seek(SeekFrom::Start(start))
            .map_err(VhdxHeaderError::SeekHeader)?;
        f.write(&buffer).map_err(VhdxHeaderError::WriteHeader)?;

        Ok(new_header)
    }
}

#[repr(packed)]
#[derive(Clone, Copy, Debug)]
struct RegionTableHeader {
    pub signature: u32,
    pub checksum: u32,
    pub entry_count: u32,
    pub reserved: u32,
}

impl RegionTableHeader {
    /// Reads the Region Table Header structure from a reference VHDx file
    pub fn new(f: &mut File, start: u64) -> Result<RegionTableHeader> {
        // Read the whole header into a buffer. We will need it for calculating
        // checksum.
        let mut buffer = [0u8; REGION_SIZE as usize];
        f.seek(SeekFrom::Start(start))
            .map_err(VhdxHeaderError::SeekRegionTableHeader)?;
        f.read_exact(&mut buffer)
            .map_err(VhdxHeaderError::ReadRegionTableHeader)?;

        let region_table_header = unsafe { *(buffer.as_ptr() as *mut RegionTableHeader) };
        if region_table_header.signature != REGION_SIGN {
            return Err(VhdxHeaderError::InvalidRegionSign);
        }

        let new_checksum = calculate_checksum(&mut buffer, size_of::<u32>())?;
        if region_table_header.checksum != new_checksum {
            return Err(VhdxHeaderError::InvalidChecksum(String::from("Region")));
        }

        if region_table_header.entry_count > 2047 {
            return Err(VhdxHeaderError::InvalidEntryCount);
        }

        if region_table_header.reserved != 0 {
            return Err(VhdxHeaderError::ReservedIsNonZero);
        }

        Ok(region_table_header)
    }
}

pub struct RegionInfo {
    pub bat_entry: RegionTableEntry,
    pub mdr_entry: RegionTableEntry,
    pub region_entries: BTreeMap<u64, u64>,
}

impl RegionInfo {
    /// Collect all entries in a BTreeMap from the Region Table and identifies
    /// BAT and metadata regions
    pub fn new(f: &mut File, region_start: u64, entry_count: u32) -> Result<RegionInfo> {
        let mut bat_entry: Option<RegionTableEntry> = None;
        let mut mdr_entry: Option<RegionTableEntry> = None;

        let mut offset = 0;
        let mut region_entries = BTreeMap::new();

        let mut buffer = [0; REGION_SIZE as usize];
        // Seek after the Region Table Header
        f.seek(SeekFrom::Start(
            region_start + size_of::<RegionTableHeader>() as u64,
        ))
        .map_err(VhdxHeaderError::SeekRegionTableEntries)?;
        f.read_exact(&mut buffer)
            .map_err(VhdxHeaderError::ReadRegionTableEntries)?;

        for _ in 0..entry_count {
            let entry =
                RegionTableEntry::new(&buffer[offset..offset + size_of::<RegionTableEntry>()])?;

            offset += size_of::<RegionTableEntry>();
            let start = entry.file_offset;
            let end = start + entry.length as u64;

            for (region_ent_start, region_ent_end) in region_entries.iter() {
                if !((start >= *region_ent_start) || (end <= *region_ent_end)) {
                    return Err(VhdxHeaderError::RegionOverlap);
                }
            }

            region_entries.insert(entry.file_offset, entry.file_offset + entry.length as u64);

            if entry.guid == Uuid::parse_str(BAT_GUID).map_err(VhdxHeaderError::InvalidUuid)? {
                if bat_entry.is_none() {
                    bat_entry = Some(entry);
                    continue;
                }
                return Err(VhdxHeaderError::DuplicateBATEntry);
            }

            if entry.guid == Uuid::parse_str(MDR_GUID).map_err(VhdxHeaderError::InvalidUuid)? {
                if mdr_entry.is_none() {
                    mdr_entry = Some(entry);
                    continue;
                }
                return Err(VhdxHeaderError::DuplicateMDREntry);
            }

            if (entry.required & REGION_ENTRY_REQUIRED) == 1 {
                // This implementation doesn't recognize this field.
                // Therefore, accoding to the spec, we are throwing an error.
                return Err(VhdxHeaderError::UnrecognizedRegionEntry);
            }
        }

        if bat_entry.is_none() || mdr_entry.is_none() {
            region_entries.clear();
            return Err(VhdxHeaderError::RegionEntryCollectionFailed);
        }

        // It's safe to unwrap as we checked both entries have been filled.
        // Otherwise, an error is already returned.
        let bat_entry = bat_entry.unwrap();
        let mdr_entry = mdr_entry.unwrap();

        Ok(RegionInfo {
            bat_entry,
            mdr_entry,
            region_entries,
        })
    }
}

#[repr(packed)]
#[derive(Clone, Copy, Debug)]
pub struct RegionTableEntry {
    pub guid: Uuid,
    pub file_offset: u64,
    pub length: u32,
    pub required: u32,
}

impl RegionTableEntry {
    /// Reads one Region Entry from a Region Table index that starts from 0
    pub fn new(buffer: &[u8]) -> Result<RegionTableEntry> {
        let mut region_table_entry = unsafe { *(buffer.as_ptr() as *mut RegionTableEntry) };

        let uuid = crate::uuid_from_guid(buffer);
        region_table_entry.guid = uuid;

        Ok(region_table_entry)
    }
}

#[derive(Clone, Debug)]
struct RegionEntry {
    _start: u64,
    _end: u64,
}

enum HeaderNo {
    First,
    Second,
}

/// Contains the information from the header of a VHDx file
#[derive(Clone, Debug)]
pub struct VhdxHeader {
    _file_type_identifier: FileTypeIdentifier,
    header_1: Header,
    header_2: Header,
    region_table_1: RegionTableHeader,
    _region_table_2: RegionTableHeader,
}

impl VhdxHeader {
    /// Creates a VhdxHeader from a reference to a file
    pub fn new(f: &mut File) -> Result<VhdxHeader> {
        let _file_type_identifier: FileTypeIdentifier = FileTypeIdentifier::new(f)?;
        let header_1 = Header::new(f, HEADER_1_START);
        let header_2 = Header::new(f, HEADER_2_START);

        let mut file_write_guid: u128 = 0;
        let metadata = f.metadata().map_err(VhdxHeaderError::ReadMetadata)?;
        if !metadata.permissions().readonly() {
            file_write_guid = Uuid::new_v4().as_u128();
        }

        let (header_1, header_2) =
            VhdxHeader::update_headers(f, header_1, header_2, file_write_guid)?;
        Ok(VhdxHeader {
            _file_type_identifier,
            header_1,
            header_2,
            region_table_1: RegionTableHeader::new(f, REGION_TABLE_1_START)?,
            _region_table_2: RegionTableHeader::new(f, REGION_TABLE_2_START)?,
        })
    }

    /// Identify the current header and return both headers along with an
    /// integer indicating the current header.
    fn current_header(
        header_1: Result<Header>,
        header_2: Result<Header>,
    ) -> Result<(HeaderNo, Header)> {
        let header_1 = header_1.ok();
        let header_2 = header_2.ok();

        match (header_1, header_2) {
            (None, None) => Err(VhdxHeaderError::NoValidHeader),
            (Some(header_1), None) => Ok((HeaderNo::First, header_1)),
            (None, Some(header_2)) => Ok((HeaderNo::Second, header_2)),
            (Some(header_1), Some(header_2)) => {
                if header_1.sequence_number >= header_2.sequence_number {
                    Ok((HeaderNo::First, header_1))
                } else {
                    Ok((HeaderNo::Second, header_2))
                }
            }
        }
    }

    /// This takes two headers and update the noncurrent header with the
    /// current one. Returns both headers as a tuple sequenced the way it was
    /// received from the parameter list.
    fn update_header(
        f: &mut File,
        header_1: Result<Header>,
        header_2: Result<Header>,
        guid: u128,
    ) -> Result<(Header, Header)> {
        let (header_no, current_header) = VhdxHeader::current_header(header_1, header_2)?;

        match header_no {
            HeaderNo::First => {
                let other_header =
                    Header::update_header(f, &current_header, true, guid, HEADER_2_START)?;
                Ok((current_header, other_header))
            }
            HeaderNo::Second => {
                let other_header =
                    Header::update_header(f, &current_header, true, guid, HEADER_1_START)?;
                Ok((other_header, current_header))
            }
        }
    }

    // Update the provided headers accoding to the spec
    fn update_headers(
        f: &mut File,
        header_1: Result<Header>,
        header_2: Result<Header>,
        guid: u128,
    ) -> Result<(Header, Header)> {
        // According to the spec, update twice
        let (header_1, header_2) = VhdxHeader::update_header(f, header_1, header_2, guid)?;
        VhdxHeader::update_header(f, Ok(header_1), Ok(header_2), guid)
    }

    pub fn update(&mut self, f: &mut File) -> Result<()> {
        let headers = VhdxHeader::update_headers(f, Ok(self.header_1), Ok(self.header_2), 0)?;
        self.header_1 = headers.0;
        self.header_2 = headers.1;
        Ok(())
    }

    pub fn region_entry_count(&self) -> u32 {
        self.region_table_1.entry_count
    }
}

/// Calculates the checksum of a buffer that itself containts its checksum
/// Therefore, before calculating, the existing checksum is retrieved and the
/// corresponding field is made zero. After the calculation, the existing checksum
/// is put back to the buffer.
pub fn calculate_checksum(buffer: &mut [u8], csum_offset: usize) -> Result<u32> {
    // Read the checksum into a mutable slice
    let csum_buf = &mut buffer[csum_offset..csum_offset + 4];
    // Convert the checksum chunk in to a u32 integer
    let orig_csum = LittleEndian::read_u32(csum_buf);
    // Zero the checksum in the buffer
    LittleEndian::write_u32(csum_buf, 0);
    // Calculate the checksum on the resulting buffer
    let new_csum = crc32c::crc32c(buffer);
    // Put back the original checksum in the buffer
    LittleEndian::write_u32(&mut buffer[csum_offset..csum_offset + 4], orig_csum);

    Ok(new_csum)
}

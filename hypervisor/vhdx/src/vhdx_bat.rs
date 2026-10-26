// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

use crate::vhdx_header::RegionTableEntry;
use crate::vhdx_metadata::DiskSpec;
use byteorder::{LittleEndian, ReadBytesExt, WriteBytesExt};
use remain::sorted;
use std::fs::File;
use std::io::{self, Seek, SeekFrom};
use std::mem::size_of;
use thiserror::Error;

// Payload BAT Entry States
pub const PAYLOAD_BLOCK_NOT_PRESENT: u64 = 0;
pub const PAYLOAD_BLOCK_UNDEFINED: u64 = 1;
pub const PAYLOAD_BLOCK_ZERO: u64 = 2;
pub const PAYLOAD_BLOCK_UNMAPPED: u64 = 3;
pub const PAYLOAD_BLOCK_FULLY_PRESENT: u64 = 6;
pub const PAYLOAD_BLOCK_PARTIALLY_PRESENT: u64 = 7;

// Mask for the BAT state
pub const BAT_STATE_BIT_MASK: u64 = 0x07;
// Mask for the offset within the file in units of 1 MB
pub const BAT_FILE_OFF_MASK: u64 = 0xFFFFFFFFFFF00000;

#[sorted]
#[derive(Error, Debug)]
pub enum VhdxBatError {
    #[error("Invalid BAT entry")]
    InvalidBatEntry,
    #[error("Invalid BAT entry count")]
    InvalidEntryCount,
    #[error("Failed to read BAT entry {0}")]
    ReadBat(#[source] io::Error),
    #[error("Failed to write BAT entry {0}")]
    WriteBat(#[source] io::Error),
}

pub type Result<T> = std::result::Result<T, VhdxBatError>;

#[derive(Default, Clone, Debug)]
pub struct BatEntry(pub u64);

impl BatEntry {
    // Read all BAT entries presented on the disk and insert them to a vector
    pub fn collect_bat_entries(
        f: &mut File,
        disk_spec: &DiskSpec,
        bat_entry: &RegionTableEntry,
    ) -> Result<Vec<BatEntry>> {
        let entry_count = BatEntry::calculate_entries(
            disk_spec.block_size,
            disk_spec.virtual_disk_size,
            disk_spec.chunk_ratio,
        );
        if entry_count as usize > (bat_entry.length as usize / size_of::<BatEntry>()) {
            return Err(VhdxBatError::InvalidEntryCount);
        }

        let mut bat: Vec<BatEntry> = Vec::with_capacity(bat_entry.length as usize);
        let offset = bat_entry.file_offset;
        for i in 0..entry_count {
            f.seek(SeekFrom::Start(offset + i * size_of::<u64>() as u64))
                .map_err(VhdxBatError::ReadBat)?;

            let bat_entry = BatEntry(
                f.read_u64::<LittleEndian>()
                    .map_err(VhdxBatError::ReadBat)?,
            );
            bat.insert(i as usize, bat_entry);
        }

        Ok(bat)
    }

    // Calculate the number of entries in the BAT
    fn calculate_entries(block_size: u32, virtual_disk_size: u64, chunk_ratio: u64) -> u64 {
        let data_blocks_count = div_round_up!(virtual_disk_size, block_size as u64);
        data_blocks_count + (data_blocks_count - 1) / chunk_ratio
    }

    // Routine for writing BAT entries to the disk
    pub fn write_bat_entries(
        f: &mut File,
        bat_offset: u64,
        bat_entries: &[BatEntry],
    ) -> Result<()> {
        for i in 0..bat_entries.len() as u64 {
            f.seek(SeekFrom::Start(bat_offset + i * size_of::<u64>() as u64))
                .map_err(VhdxBatError::WriteBat)?;
            let bat_entry = match bat_entries.get(i as usize) {
                Some(entry) => entry.0,
                None => {
                    return Err(VhdxBatError::InvalidBatEntry);
                }
            };

            f.write_u64::<LittleEndian>(bat_entry)
                .map_err(VhdxBatError::WriteBat)?;
        }
        Ok(())
    }
}

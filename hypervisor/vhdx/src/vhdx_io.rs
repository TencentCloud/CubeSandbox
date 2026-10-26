// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

use crate::vhdx_bat::{self, BatEntry, VhdxBatError};
use crate::vhdx_metadata::{self, DiskSpec};
use remain::sorted;
use std::fs::File;
use std::io::{self, Read, Seek, SeekFrom, Write};
use thiserror::Error;

const SECTOR_SIZE: u64 = 512;

#[sorted]
#[derive(Error, Debug)]
pub enum VhdxIoError {
    #[error("Invalid BAT entry state")]
    InvalidBatEntryState,
    #[error("Invalid BAT entry count")]
    InvalidBatIndex,
    #[error("Invalid disk size")]
    InvalidDiskSize,
    #[error("Failed reading sector blocks from file {0}")]
    ReadSectorBlock(#[source] io::Error),
    #[error("Failed changing file length {0}")]
    ResizeFile(#[source] io::Error),
    #[error("Differencing mode is not supported yet")]
    UnsupportedMode,
    #[error("Failed writing BAT to file {0}")]
    WriteBat(#[source] VhdxBatError),
}

pub type Result<T> = std::result::Result<T, VhdxIoError>;

macro_rules! align {
    ($n:expr, $align:expr) => {{
        (($n + $align - 1) / $align) * $align
    }};
}

#[derive(Default)]
struct Sector {
    bat_index: u64,
    free_sectors: u64,
    free_bytes: u64,
    file_offset: u64,
    block_offset: u64,
}

impl Sector {
    /// Translate sector index and count of data in file to actual offsets and
    /// BAT index.
    pub fn new(
        disk_spec: &DiskSpec,
        bat: &[BatEntry],
        sector_index: u64,
        sector_count: u64,
    ) -> Result<Sector> {
        let mut sector = Sector::default();

        sector.bat_index = sector_index / disk_spec.sectors_per_block as u64;
        sector.block_offset = sector_index % disk_spec.sectors_per_block as u64;
        sector.free_sectors = disk_spec.sectors_per_block as u64 - sector.block_offset;
        if sector.free_sectors > sector_count {
            sector.free_sectors = sector_count;
        }

        sector.free_bytes = sector.free_sectors * disk_spec.logical_sector_size as u64;
        sector.block_offset *= disk_spec.logical_sector_size as u64;

        let bat_entry = match bat.get(sector.bat_index as usize) {
            Some(entry) => entry.0,
            None => {
                return Err(VhdxIoError::InvalidBatIndex);
            }
        };
        sector.file_offset = bat_entry & vhdx_bat::BAT_FILE_OFF_MASK;
        if sector.file_offset != 0 {
            sector.file_offset += sector.block_offset;
        }

        Ok(sector)
    }
}

/// VHDx IO read routine: requires relative sector index and count for the
/// requested data.
pub fn read(
    f: &mut File,
    buf: &mut [u8],
    disk_spec: &DiskSpec,
    bat: &[BatEntry],
    mut sector_index: u64,
    mut sector_count: u64,
) -> Result<usize> {
    let mut read_count: usize = 0;

    while sector_count > 0 {
        if disk_spec.has_parent {
            return Err(VhdxIoError::UnsupportedMode);
        } else {
            let sector = Sector::new(disk_spec, bat, sector_index, sector_count)?;

            let bat_entry = match bat.get(sector.bat_index as usize) {
                Some(entry) => entry.0,
                None => {
                    return Err(VhdxIoError::InvalidBatIndex);
                }
            };

            match bat_entry & vhdx_bat::BAT_STATE_BIT_MASK {
                vhdx_bat::PAYLOAD_BLOCK_NOT_PRESENT
                | vhdx_bat::PAYLOAD_BLOCK_UNDEFINED
                | vhdx_bat::PAYLOAD_BLOCK_UNMAPPED
                | vhdx_bat::PAYLOAD_BLOCK_ZERO => {}
                vhdx_bat::PAYLOAD_BLOCK_FULLY_PRESENT => {
                    f.seek(SeekFrom::Start(sector.file_offset))
                        .map_err(VhdxIoError::ReadSectorBlock)?;
                    f.read_exact(
                        &mut buf[read_count
                            ..(read_count + (sector.free_sectors * SECTOR_SIZE) as usize)],
                    )
                    .map_err(VhdxIoError::ReadSectorBlock)?;
                }
                vhdx_bat::PAYLOAD_BLOCK_PARTIALLY_PRESENT => {
                    return Err(VhdxIoError::UnsupportedMode);
                }
                _ => {
                    return Err(VhdxIoError::InvalidBatEntryState);
                }
            };
            sector_count -= sector.free_sectors;
            sector_index += sector.free_sectors;
            read_count = sector.free_bytes as usize;
        };
    }
    Ok(read_count)
}

/// VHDx IO write routine: requires relative sector index and count for the
/// requested data.
pub fn write(
    f: &mut File,
    buf: &[u8],
    disk_spec: &mut DiskSpec,
    bat_offset: u64,
    bat: &mut [BatEntry],
    mut sector_index: u64,
    mut sector_count: u64,
) -> Result<usize> {
    let mut write_count: usize = 0;

    while sector_count > 0 {
        if disk_spec.has_parent {
            return Err(VhdxIoError::UnsupportedMode);
        } else {
            let sector = Sector::new(disk_spec, bat, sector_index, sector_count)?;

            let bat_entry = match bat.get(sector.bat_index as usize) {
                Some(entry) => entry.0,
                None => {
                    return Err(VhdxIoError::InvalidBatIndex);
                }
            };

            match bat_entry & vhdx_bat::BAT_STATE_BIT_MASK {
                vhdx_bat::PAYLOAD_BLOCK_NOT_PRESENT
                | vhdx_bat::PAYLOAD_BLOCK_UNDEFINED
                | vhdx_bat::PAYLOAD_BLOCK_UNMAPPED
                | vhdx_bat::PAYLOAD_BLOCK_ZERO => {
                    let file_offset =
                        align!(disk_spec.image_size, vhdx_metadata::BLOCK_SIZE_MIN as u64);
                    let new_size = file_offset
                        .checked_add(disk_spec.block_size as u64)
                        .ok_or(VhdxIoError::InvalidDiskSize)?;

                    f.set_len(new_size).map_err(VhdxIoError::ResizeFile)?;
                    disk_spec.image_size = new_size;

                    let new_bat_entry = file_offset
                        | (vhdx_bat::PAYLOAD_BLOCK_FULLY_PRESENT & vhdx_bat::BAT_STATE_BIT_MASK);
                    bat[sector.bat_index as usize] = BatEntry(new_bat_entry);
                    BatEntry::write_bat_entries(f, bat_offset, bat)
                        .map_err(VhdxIoError::WriteBat)?;

                    if file_offset < vhdx_metadata::BLOCK_SIZE_MIN as u64 {
                        break;
                    }

                    f.seek(SeekFrom::Start(file_offset))
                        .map_err(VhdxIoError::ReadSectorBlock)?;
                    f.write_all(
                        &buf[write_count
                            ..(write_count + (sector.free_sectors * SECTOR_SIZE) as usize)],
                    )
                    .map_err(VhdxIoError::ReadSectorBlock)?;
                }
                vhdx_bat::PAYLOAD_BLOCK_FULLY_PRESENT => {
                    if sector.file_offset < vhdx_metadata::BLOCK_SIZE_MIN as u64 {
                        break;
                    }

                    f.seek(SeekFrom::Start(sector.file_offset))
                        .map_err(VhdxIoError::ReadSectorBlock)?;
                    f.write_all(
                        &buf[write_count
                            ..(write_count + (sector.free_sectors * SECTOR_SIZE) as usize)],
                    )
                    .map_err(VhdxIoError::ReadSectorBlock)?;
                }
                vhdx_bat::PAYLOAD_BLOCK_PARTIALLY_PRESENT => {
                    return Err(VhdxIoError::UnsupportedMode);
                }
                _ => {
                    return Err(VhdxIoError::InvalidBatEntryState);
                }
            };
            sector_count -= sector.free_sectors;
            sector_index += sector.free_sectors;
            write_count = sector.free_bytes as usize;
        };
    }
    Ok(write_count)
}

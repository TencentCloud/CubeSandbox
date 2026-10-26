// Copyright © 2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

use crate::vhdx_bat::{BatEntry, VhdxBatError};
use crate::vhdx_header::{self, RegionInfo, RegionTableEntry, VhdxHeader, VhdxHeaderError};
use crate::vhdx_io::{self, VhdxIoError};
use crate::vhdx_metadata::{DiskSpec, VhdxMetadataError};
use remain::sorted;
use std::collections::btree_map::BTreeMap;
use std::fs::File;
use std::io::{Read, Seek, SeekFrom, Write};
use thiserror::Error;

#[sorted]
#[derive(Error, Debug)]
pub enum VhdxError {
    #[error("Not a VHDx file {0}")]
    NotVhdx(#[source] VhdxHeaderError),
    #[error("Failed to parse VHDx header {0}")]
    ParseVhdxHeader(#[source] VhdxHeaderError),
    #[error("Failed to parse VHDx metadata {0}")]
    ParseVhdxMetadata(#[source] VhdxMetadataError),
    #[error("Failed to parse VHDx region entries {0}")]
    ParseVhdxRegionEntry(#[source] VhdxHeaderError),
    #[error("Failed reading metadata {0}")]
    ReadBatEntry(#[source] VhdxBatError),
    #[error("Failed reading sector from disk {0}")]
    ReadFailed(#[source] VhdxIoError),
    #[error("Failed writing to sector on disk {0}")]
    WriteFailed(#[source] VhdxIoError),
}

pub type Result<T> = std::result::Result<T, VhdxError>;

#[derive(Debug)]
pub struct Vhdx {
    file: File,
    vhdx_header: VhdxHeader,
    region_entries: BTreeMap<u64, u64>,
    bat_entry: RegionTableEntry,
    mdr_entry: RegionTableEntry,
    disk_spec: DiskSpec,
    bat_entries: Vec<BatEntry>,
    current_offset: u64,
    first_write: bool,
}

impl Vhdx {
    /// Parse the Vhdx header, BAT, and metadata from a file and store info
    // in Vhdx structure.
    pub fn new(mut file: File) -> Result<Vhdx> {
        let vhdx_header = VhdxHeader::new(&mut file).map_err(VhdxError::ParseVhdxHeader)?;

        let collected_entries = RegionInfo::new(
            &mut file,
            vhdx_header::REGION_TABLE_1_START,
            vhdx_header.region_entry_count(),
        )
        .map_err(VhdxError::ParseVhdxRegionEntry)?;

        let bat_entry = collected_entries.bat_entry;
        let mdr_entry = collected_entries.mdr_entry;

        let disk_spec =
            DiskSpec::new(&mut file, &mdr_entry).map_err(VhdxError::ParseVhdxMetadata)?;
        let bat_entries = BatEntry::collect_bat_entries(&mut file, &disk_spec, &bat_entry)
            .map_err(VhdxError::ReadBatEntry)?;

        Ok(Vhdx {
            file,
            vhdx_header,
            region_entries: collected_entries.region_entries,
            bat_entry,
            mdr_entry,
            disk_spec,
            bat_entries,
            current_offset: 0,
            first_write: true,
        })
    }

    pub fn virtual_disk_size(&self) -> u64 {
        self.disk_spec.virtual_disk_size
    }
}

impl Read for Vhdx {
    /// Wrapper function to satisfy Read trait implementation for VHDx disk.
    /// Convert the offset to sector index and buffer length to sector count.
    fn read(&mut self, buf: &mut [u8]) -> std::result::Result<usize, std::io::Error> {
        let sector_count =
            div_round_up!(buf.len() as u64, self.disk_spec.logical_sector_size as u64);
        let sector_index = self.current_offset / self.disk_spec.logical_sector_size as u64;

        vhdx_io::read(
            &mut self.file,
            buf,
            &self.disk_spec,
            &self.bat_entries,
            sector_index,
            sector_count,
        )
        .map_err(|e| {
            std::io::Error::new(
                std::io::ErrorKind::Other,
                format!(
                    "Failed reading {} sectors from VHDx at index {}: {}",
                    sector_count, sector_index, e
                ),
            )
        })
    }
}

impl Write for Vhdx {
    fn flush(&mut self) -> std::result::Result<(), std::io::Error> {
        self.file.flush()
    }

    /// Wrapper function to satisfy Write trait implementation for VHDx disk.
    /// Convert the offset to sector index and buffer length to sector count.
    fn write(&mut self, buf: &[u8]) -> std::result::Result<usize, std::io::Error> {
        let sector_count =
            div_round_up!(buf.len() as u64, self.disk_spec.logical_sector_size as u64);
        let sector_index = self.current_offset / self.disk_spec.logical_sector_size as u64;

        if self.first_write {
            self.first_write = false;
            self.vhdx_header.update(&mut self.file).map_err(|e| {
                std::io::Error::new(
                    std::io::ErrorKind::Other,
                    format!("Failed to update VHDx header: {}", e),
                )
            })?;
        }

        vhdx_io::write(
            &mut self.file,
            buf,
            &mut self.disk_spec,
            self.bat_entry.file_offset,
            &mut self.bat_entries,
            sector_index,
            sector_count,
        )
        .map_err(|e| {
            std::io::Error::new(
                std::io::ErrorKind::Other,
                format!(
                    "Failed writing {} sectors on VHDx at index {}: {}",
                    sector_count, sector_index, e
                ),
            )
        })
    }
}

impl Seek for Vhdx {
    /// Wrapper function to satisfy Seek trait implementation for VHDx disk.
    /// Updates the offset field in the Vhdx struct.
    fn seek(&mut self, pos: SeekFrom) -> std::io::Result<u64> {
        let new_offset: Option<u64> = match pos {
            SeekFrom::Start(off) => Some(off),
            SeekFrom::End(off) => {
                if off < 0 {
                    0i64.checked_sub(off).and_then(|increment| {
                        self.virtual_disk_size().checked_sub(increment as u64)
                    })
                } else {
                    self.virtual_disk_size().checked_add(off as u64)
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
            if o <= self.virtual_disk_size() {
                self.current_offset = o;
                return Ok(o);
            }
        }

        Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            "Failed seek operation",
        ))
    }
}

impl Clone for Vhdx {
    fn clone(&self) -> Self {
        Vhdx {
            file: self.file.try_clone().unwrap(),
            vhdx_header: self.vhdx_header.clone(),
            region_entries: self.region_entries.clone(),
            bat_entry: self.bat_entry,
            mdr_entry: self.mdr_entry,
            disk_spec: self.disk_spec.clone(),
            bat_entries: self.bat_entries.clone(),
            current_offset: self.current_offset,
            first_write: self.first_write,
        }
    }
}

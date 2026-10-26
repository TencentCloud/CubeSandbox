// Copyright © 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use crate::MigratableError;
use serde::{Deserialize, Serialize};
use std::io::{Read, Write};
use vm_memory::ByteValued;

// Migration protocol
// 1: Source establishes communication with destination (file socket or TCP connection.)
// (The establishment is out of scope.)
// 2: Source -> Dest : send "start command"
// 3: Dest -> Source : sends "ok response" when read to accept state data
// 4: Source -> Dest : sends "config command" followed by config data, length
//                     in command is length of config data
// 5: Dest -> Source : sends "ok response" when ready to accept memory data
// 6: Source -> Dest : send "memory command" followed by table of u64 pairs (GPA, size)
//                     followed by the memory described in those pairs.
//                     !! length is size of table i.e. 16 * number of ranges !!
// 7: Dest -> Source : sends "ok response" when ready to accept more memory data
// 8..(n-4): Repeat steps 6 and 7 until source has no more memory to send
// (n-3): Source -> Dest : sends "state command" followed by state data, length
//                     in command is length of config data
// (n-2): Dest -> Source : sends "ok response"
// (n-1): Source -> Dest : send "complete command"
// n: Dest -> Source: sends "ok response"
//
// "Local version": (Handing FDs across socket for memory)
// 1: Source establishes communication with destination (file socket or TCP connection.)
// (The establishment is out of scope.)
// 2: Source -> Dest : send "start command"
// 3: Dest -> Source : sends "ok response" when read to accept state data
// 4: Source -> Dest : sends "config command" followed by config data, length
//                     in command is length of config data
// 5: Dest -> Source : sends "ok response" when ready to accept memory data
// 6: Source -> Dest : send "memory fd command" followed by u16 slot ID and FD for memory
// 7: Dest -> Source : sends "ok response" when received
// 8..(n-4): Repeat steps 6 and 7 until source has no more memory to send
// (n-3): Source -> Dest : sends "state command" followed by state data, length
//                     in command is length of config data
// (n-2): Dest -> Source : sends "ok response"
// (n-1): Source -> Dest : send "complete command"
// n: Dest -> Source: sends "ok response"
//
// The destination can at any time send an "error response" to cancel
// The source can at any time send an "abandon request" to cancel

#[repr(u16)]
#[derive(Copy, Clone)]
pub enum Command {
    Invalid,
    Start,
    Config,
    State,
    Memory,
    Complete,
    Abandon,
    MemoryFd,
}

impl Default for Command {
    fn default() -> Self {
        Self::Invalid
    }
}

#[repr(C)]
#[derive(Default, Copy, Clone)]
pub struct Request {
    command: Command,
    padding: [u8; 6],
    length: u64, // Length of payload for command excluding the Request struct
}

// SAFETY: Request contains a series of integers with no implicit padding
unsafe impl ByteValued for Request {}

impl Request {
    pub fn new(command: Command, length: u64) -> Self {
        Self {
            command,
            length,
            ..Default::default()
        }
    }

    pub fn start() -> Self {
        Self::new(Command::Start, 0)
    }

    pub fn state(length: u64) -> Self {
        Self::new(Command::State, length)
    }

    pub fn config(length: u64) -> Self {
        Self::new(Command::Config, length)
    }

    pub fn memory(length: u64) -> Self {
        Self::new(Command::Memory, length)
    }

    pub fn memory_fd(length: u64) -> Self {
        Self::new(Command::MemoryFd, length)
    }

    pub fn complete() -> Self {
        Self::new(Command::Complete, 0)
    }

    pub fn abandon() -> Self {
        Self::new(Command::Abandon, 0)
    }

    pub fn command(&self) -> Command {
        self.command
    }

    pub fn length(&self) -> u64 {
        self.length
    }

    pub fn read_from(fd: &mut dyn Read) -> Result<Request, MigratableError> {
        let mut request = Request::default();
        fd.read_exact(Self::as_mut_slice(&mut request))
            .map_err(MigratableError::MigrateSocket)?;

        Ok(request)
    }

    pub fn write_to(&self, fd: &mut dyn Write) -> Result<(), MigratableError> {
        fd.write_all(Self::as_slice(self))
            .map_err(MigratableError::MigrateSocket)
    }
}

#[repr(u16)]
#[derive(Copy, Clone, PartialEq, Eq)]
pub enum Status {
    Invalid,
    Ok,
    Error,
}

impl Default for Status {
    fn default() -> Self {
        Self::Invalid
    }
}

#[repr(C)]
#[derive(Default, Copy, Clone)]
pub struct Response {
    status: Status,
    padding: [u8; 6],
    length: u64, // Length of payload for command excluding the Response struct
}

// SAFETY: Response contains a series of integers with no implicit padding
unsafe impl ByteValued for Response {}

impl Response {
    pub fn new(status: Status, length: u64) -> Self {
        Self {
            status,
            length,
            ..Default::default()
        }
    }

    pub fn ok() -> Self {
        Self::new(Status::Ok, 0)
    }

    pub fn error() -> Self {
        Self::new(Status::Error, 0)
    }

    pub fn status(&self) -> Status {
        self.status
    }

    pub fn read_from(fd: &mut dyn Read) -> Result<Response, MigratableError> {
        let mut response = Response::default();
        fd.read_exact(Self::as_mut_slice(&mut response))
            .map_err(MigratableError::MigrateSocket)?;

        Ok(response)
    }

    pub fn write_to(&self, fd: &mut dyn Write) -> Result<(), MigratableError> {
        fd.write_all(Self::as_slice(self))
            .map_err(MigratableError::MigrateSocket)
    }
}

#[repr(C)]
#[derive(Clone, Default, Serialize, Deserialize)]
pub struct MemoryRange {
    pub gpa: u64,
    pub length: u64,
}

#[derive(Clone, Default, Serialize, Deserialize)]
pub struct MemoryRangeTable {
    data: Vec<MemoryRange>,
}

impl MemoryRangeTable {
    pub fn from_bitmap(bitmap: Vec<u64>, start_addr: u64, page_size: u64) -> Self {
        let mut table = MemoryRangeTable::default();
        let mut entry: Option<MemoryRange> = None;
        for (i, block) in bitmap.iter().enumerate() {
            for j in 0..64 {
                let is_page_dirty = ((block >> j) & 1u64) != 0u64;
                let page_offset = ((i * 64) + j) as u64 * page_size;
                if is_page_dirty {
                    if let Some(entry) = &mut entry {
                        entry.length += page_size;
                    } else {
                        entry = Some(MemoryRange {
                            gpa: start_addr + page_offset,
                            length: page_size,
                        });
                    }
                } else if let Some(entry) = entry.take() {
                    table.push(entry);
                }
            }
        }
        if let Some(entry) = entry.take() {
            table.push(entry);
        }

        table
    }

    pub fn regions(&self) -> &[MemoryRange] {
        &self.data
    }

    pub fn push(&mut self, range: MemoryRange) {
        self.data.push(range)
    }

    pub fn read_from(fd: &mut dyn Read, length: u64) -> Result<MemoryRangeTable, MigratableError> {
        assert!(length as usize % std::mem::size_of::<MemoryRange>() == 0);

        let mut data: Vec<MemoryRange> = Vec::new();
        data.resize_with(
            length as usize / (std::mem::size_of::<MemoryRange>()),
            Default::default,
        );
        fd.read_exact(unsafe {
            std::slice::from_raw_parts_mut(
                data.as_ptr() as *mut MemoryRange as *mut u8,
                length as usize,
            )
        })
        .map_err(MigratableError::MigrateSocket)?;

        Ok(Self { data })
    }

    pub fn length(&self) -> u64 {
        (std::mem::size_of::<MemoryRange>() * self.data.len()) as u64
    }

    pub fn write_to(&self, fd: &mut dyn Write) -> Result<(), MigratableError> {
        fd.write_all(unsafe {
            std::slice::from_raw_parts(self.data.as_ptr() as *const u8, self.length() as usize)
        })
        .map_err(MigratableError::MigrateSocket)
    }

    pub fn is_empty(&self) -> bool {
        self.data.is_empty()
    }

    pub fn extend(&mut self, table: Self) {
        self.data.extend(table.data)
    }

    pub fn new_from_tables(tables: Vec<Self>) -> Self {
        let mut data = Vec::new();
        for table in tables {
            data.extend(table.data);
        }
        Self { data }
    }
}

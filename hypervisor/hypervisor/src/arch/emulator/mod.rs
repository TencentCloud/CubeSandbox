//
// Copyright © 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use core::fmt::Debug;
use std::fmt::{self, Display};
use thiserror::Error;

#[derive(Clone, Copy, Error, Debug)]
pub struct Exception<T: Debug> {
    vector: T,
    ip: u64,
    error: Option<u32>,
    payload: Option<u64>,
}

impl<T: Debug> Display for Exception<T> {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        write!(
            f,
            "Exception {:?} at IP {:#x}{}{}",
            self.vector,
            self.ip,
            self.error
                .map(|e| format!(": error {:x}", e))
                .unwrap_or_else(|| "".to_owned()),
            self.payload
                .map(|payload| format!(": payload {:x}", payload))
                .unwrap_or_else(|| "".to_owned())
        )
    }
}

#[derive(Error, Debug)]
pub enum PlatformError {
    #[error("Invalid address: {0}")]
    InvalidAddress(#[source] anyhow::Error),

    #[error("Invalid register: {0}")]
    InvalidRegister(#[source] anyhow::Error),

    #[error("Invalid state: {0}")]
    InvalidState(#[source] anyhow::Error),

    #[error("Memory read failure: {0}")]
    MemoryReadFailure(#[source] anyhow::Error),

    #[error("Memory write failure: {0}")]
    MemoryWriteFailure(#[source] anyhow::Error),

    #[error("Get CPU state failure: {0}")]
    GetCpuStateFailure(#[source] anyhow::Error),

    #[error("Set CPU state failure: {0}")]
    SetCpuStateFailure(#[source] anyhow::Error),

    #[error("Translate virtual address: {0}")]
    TranslateVirtualAddress(#[source] anyhow::Error),

    #[error("Unsupported CPU Mode: {0}")]
    UnsupportedCpuMode(#[source] anyhow::Error),

    #[error("Invalid instruction operand: {0}")]
    InvalidOperand(#[source] anyhow::Error),
}

#[derive(Error, Debug)]
pub enum EmulationError<T: Debug> {
    #[error("Unsupported instruction: {0}")]
    UnsupportedInstruction(#[source] anyhow::Error),

    #[error("Unsupported memory size: {0}")]
    UnsupportedMemorySize(#[source] anyhow::Error),

    #[error("Invalid operand: {0}")]
    InvalidOperand(#[source] anyhow::Error),

    #[error("Wrong number of operands: {0}")]
    WrongNumberOperands(#[source] anyhow::Error),

    #[error("Instruction Exception: {0}")]
    InstructionException(Exception<T>),

    #[error("Instruction fetching error: {0}")]
    InstructionFetchingError(#[source] anyhow::Error),

    #[error("Platform emulation error: {0}")]
    PlatformEmulationError(PlatformError),

    #[error(transparent)]
    EmulationError(#[from] anyhow::Error),
}

/// The PlatformEmulator trait emulates a guest platform.
/// It's mostly a guest resources (memory and CPU state) getter and setter.
///
/// A CpuState is an architecture specific type, representing a CPU state.
/// The emulator and its instruction handlers modify a given CPU state and
/// eventually ask the platform to commit it back through `set_cpu_state`.
pub trait PlatformEmulator {
    type CpuState: Clone;

    /// Read guest memory into a u8 slice.
    ///
    /// # Arguments
    ///
    /// * `gva` - Guest virtual address to read from.
    /// * `data` - Data slice to read into.
    ///
    fn read_memory(&self, gva: u64, data: &mut [u8]) -> Result<(), PlatformError>;

    /// Write a u8 slice into guest memory.
    ///
    /// # Arguments
    ///
    /// * `gva` - Guest virtual address to write into.
    /// * `data` - Data slice to be written.
    ///
    fn write_memory(&mut self, gva: u64, data: &[u8]) -> Result<(), PlatformError>;

    /// Get a CPU state from the guest.
    ///
    /// # Arguments
    ///
    /// * `cpu_id` - Logical CPU ID.
    ///
    fn cpu_state(&self, cpu_id: usize) -> Result<Self::CpuState, PlatformError>;

    /// Set a guest CPU state.
    ///
    /// # Arguments
    ///
    /// * `cpu_id` - Logical CPU ID.
    /// * `state` - State to set the CPU into.
    ///
    fn set_cpu_state(&self, cpu_id: usize, state: Self::CpuState) -> Result<(), PlatformError>;

    /// Translate a guest virtual address into a physical one
    ///
    /// # Arguments
    ///
    /// * `gva` - Guest virtual address to translate.
    ///
    fn gva_to_gpa(&self, gva: u64) -> Result<u64, PlatformError>;

    /// Fetch instruction bytes from memory.
    ///
    /// # Arguments
    ///
    /// * `ip` - Instruction pointer virtual address to start fetching instructions from.
    ///
    fn fetch(&self, ip: u64, instruction_bytes: &mut [u8]) -> Result<(), PlatformError>;
}

pub type EmulationResult<S, E> = std::result::Result<S, EmulationError<E>>;

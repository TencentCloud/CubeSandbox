// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 OR BSD-3-Clause
//
// Copyright © 2020, Microsoft Corporation
//
// Copyright 2018-2019 CrowdStrike, Inc.
//
//

//! A generic abstraction around hypervisor functionality
//!
//! This crate offers a trait abstraction for underlying hypervisors
//!
//! # Platform support
//!
//! - x86_64
//! - arm64
//!

#![allow(clippy::significant_drop_in_scrutinee)]

#[macro_use]
extern crate anyhow;
#[cfg(target_arch = "x86_64")]
#[macro_use]
extern crate log;

/// Architecture specific definitions
#[macro_use]
pub mod arch;

#[cfg(feature = "kvm")]
/// KVM implementation module
pub mod kvm;

/// Microsoft Hypervisor implementation module
#[cfg(all(feature = "mshv", target_arch = "x86_64"))]
pub mod mshv;

/// Hypevisor related module
mod hypervisor;

/// Vm related module
mod vm;

/// CPU related module
mod cpu;

/// Device related module
mod device;

pub use cpu::{HypervisorCpuError, Vcpu, VmExit};
pub use device::HypervisorDeviceError;
pub use hypervisor::{Hypervisor, HypervisorError};
#[cfg(all(feature = "kvm", target_arch = "aarch64"))]
pub use kvm::{aarch64, GicState};
use std::sync::Arc;
pub use vm::{
    DataMatch, HypervisorVmError, InterruptSourceConfig, LegacyIrqSourceConfig, MsiIrqSourceConfig,
    Vm, VmOps,
};

#[derive(Debug, Copy, Clone)]
pub enum HypervisorType {
    Kvm,
    Mshv,
    KvmPvm,
}

pub fn new() -> std::result::Result<Arc<dyn Hypervisor>, HypervisorError> {
    #[cfg(feature = "kvm")]
    if kvm::KvmHypervisor::is_available()? {
        return kvm::KvmHypervisor::new();
    }

    #[cfg(feature = "mshv")]
    if mshv::MshvHypervisor::is_available()? {
        return mshv::MshvHypervisor::new();
    }

    Err(HypervisorError::HypervisorCreate(anyhow!(
        "no supported hypervisor"
    )))
}

// Returns a `Vec<T>` with a size in bytes at least as large as `size_in_bytes`.
fn vec_with_size_in_bytes<T: Default>(size_in_bytes: usize) -> Vec<T> {
    let rounded_size = (size_in_bytes + size_of::<T>() - 1) / size_of::<T>();
    let mut v = Vec::with_capacity(rounded_size);
    v.resize_with(rounded_size, T::default);
    v
}

// The kvm API has many structs that resemble the following `Foo` structure:
//
// ```
// #[repr(C)]
// struct Foo {
//    some_data: u32
//    entries: __IncompleteArrayField<__u32>,
// }
// ```
//
// In order to allocate such a structure, `size_of::<Foo>()` would be too small because it would not
// include any space for `entries`. To make the allocation large enough while still being aligned
// for `Foo`, a `Vec<Foo>` is created. Only the first element of `Vec<Foo>` would actually be used
// as a `Foo`. The remaining memory in the `Vec<Foo>` is for `entries`, which must be contiguous
// with `Foo`. This function is used to make the `Vec<Foo>` with enough space for `count` entries.
use std::mem::size_of;
pub fn vec_with_array_field<T: Default, F>(count: usize) -> Vec<T> {
    let element_space = count * size_of::<F>();
    let vec_size_bytes = size_of::<T>() + element_space;
    vec_with_size_in_bytes(vec_size_bytes)
}

///
/// User memory region structure
///
#[derive(Debug, Default, Eq, PartialEq)]
pub struct UserMemoryRegion {
    pub slot: u32,
    pub guest_phys_addr: u64,
    pub memory_size: u64,
    pub userspace_addr: u64,
    pub flags: u32,
}

///
/// Flags for user memory region
///
pub const USER_MEMORY_REGION_READ: u32 = 1;
pub const USER_MEMORY_REGION_WRITE: u32 = 1 << 1;
pub const USER_MEMORY_REGION_EXECUTE: u32 = 1 << 2;
pub const USER_MEMORY_REGION_LOG_DIRTY: u32 = 1 << 3;

#[derive(Debug)]
pub enum MpState {
    #[cfg(feature = "kvm")]
    Kvm(kvm_bindings::kvm_mp_state),
    #[cfg(all(feature = "mshv", target_arch = "x86_64"))]
    Mshv, /* MSHV does not supprt MpState yet */
}

#[derive(Debug, Clone, Copy)]
pub enum IoEventAddress {
    Pio(u64),
    Mmio(u64),
}

#[derive(Clone, serde::Serialize, serde::Deserialize)]
#[allow(clippy::large_enum_variant)]
pub enum CpuState {
    #[cfg(feature = "kvm")]
    Kvm(kvm::VcpuKvmState),
    #[cfg(all(feature = "mshv", target_arch = "x86_64"))]
    Mshv(mshv::VcpuMshvState),
}

#[derive(Debug, Clone, Copy, serde::Serialize, serde::Deserialize)]
#[cfg(target_arch = "x86_64")]
pub enum ClockData {
    #[cfg(feature = "kvm")]
    Kvm(kvm_bindings::kvm_clock_data),
    #[cfg(feature = "mshv")]
    Mshv, /* MSHV does not supprt ClockData yet */
}

#[cfg(target_arch = "x86_64")]
impl ClockData {
    pub fn reset_flags(&mut self) {
        match self {
            #[cfg(feature = "kvm")]
            ClockData::Kvm(s) => s.flags = 0,
            #[allow(unreachable_patterns)]
            _ => {}
        }
    }
}

#[derive(Copy, Clone)]
pub enum IrqRoutingEntry {
    #[cfg(feature = "kvm")]
    Kvm(kvm_bindings::kvm_irq_routing_entry),
    #[cfg(feature = "mshv")]
    Mshv(mshv_bindings::mshv_msi_routing_entry),
}

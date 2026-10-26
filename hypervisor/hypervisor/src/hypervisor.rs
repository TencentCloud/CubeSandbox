// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 OR BSD-3-Clause
//
// Copyright © 2020, Microsoft Corporation
//
// Copyright 2018-2019 CrowdStrike, Inc.
//
//
#[cfg(target_arch = "x86_64")]
use crate::arch::x86::CpuIdEntry;
#[cfg(feature = "tdx")]
use crate::kvm::TdxCapabilities;
use crate::vm::Vm;
use crate::HypervisorType;
use std::sync::Arc;
use thiserror::Error;

#[derive(Error, Debug)]
///
///
pub enum HypervisorError {
    ///
    /// Hypervisor availability check error
    ///
    #[error("Failed to check availability of the hypervisor: {0}")]
    HypervisorAvailableCheck(#[source] anyhow::Error),
    ///
    /// hypervisor creation error
    ///
    #[error("Failed to create the hypervisor: {0}")]
    HypervisorCreate(#[source] anyhow::Error),
    ///
    /// Vm creation failure
    ///
    #[error("Failed to create Vm: {0}")]
    VmCreate(#[source] anyhow::Error),
    ///
    /// Vm setup failure
    ///
    #[error("Failed to setup Vm: {0}")]
    VmSetup(#[source] anyhow::Error),
    ///
    /// API version error
    ///
    #[error("Failed to get API Version: {0}")]
    GetApiVersion(#[source] anyhow::Error),
    ///
    /// CpuId error
    ///
    #[error("Failed to get cpuid: {0}")]
    GetCpuId(#[source] anyhow::Error),
    ///
    /// Failed to retrieve list of MSRs.
    ///
    #[error("Failed to get the list of supported MSRs: {0}")]
    GetMsrList(#[source] anyhow::Error),
    ///
    /// API version is not compatible
    ///
    #[error("Incompatible API version")]
    IncompatibleApiVersion,
    ///
    /// Checking extensions failed
    ///
    #[error("Checking extensions:{0}")]
    CheckExtensions(#[source] anyhow::Error),
    ///
    /// Failed to retrieve TDX capabilities
    ///
    #[error("Failed to retrieve TDX capabilities:{0}")]
    TdxCapabilities(#[source] anyhow::Error),
    ///
    /// Failed to set partition property
    ///
    #[error("Failed to set partition property:{0}")]
    SetPartitionProperty(#[source] anyhow::Error),
}

///
/// Result type for returning from a function
///
pub type Result<T> = std::result::Result<T, HypervisorError>;

///
/// Trait to represent a Hypervisor
///
/// This crate provides a hypervisor-agnostic interfaces
///
pub trait Hypervisor: Send + Sync {
    ///
    /// Returns the type of the hypervisor
    ///
    fn hypervisor_type(&self) -> HypervisorType;
    ///
    /// Create a Vm using the underlying hypervisor
    /// Return a hypervisor-agnostic Vm trait object
    ///
    fn create_vm(&self) -> Result<Arc<dyn Vm>>;
    ///
    /// Create a Vm of a specific type using the underlying hypervisor
    /// Return a hypervisor-agnostic Vm trait object
    ///
    fn create_vm_with_type(&self, _vm_type: u64) -> Result<Arc<dyn Vm>> {
        unreachable!()
    }
    #[cfg(target_arch = "x86_64")]
    ///
    /// Get the supported CpuID
    ///
    fn get_cpuid(&self) -> Result<Vec<CpuIdEntry>>;
    ///
    /// Check particular extensions if any
    ///
    fn check_required_extensions(&self) -> Result<()> {
        Ok(())
    }
    #[cfg(target_arch = "aarch64")]
    ///
    /// Retrieve AArch64 host maximum IPA size supported by KVM
    ///
    fn get_host_ipa_limit(&self) -> i32;
    ///
    /// Retrieve TDX capabilities
    ///
    #[cfg(feature = "tdx")]
    fn tdx_capabilities(&self) -> Result<TdxCapabilities> {
        unimplemented!()
    }
    ///
    /// Get the number of supported hardware breakpoints
    ///
    fn get_guest_debug_hw_bps(&self) -> usize {
        unimplemented!()
    }
}

// Copyright 2022 Arm Limited (or its affiliates). All rights reserved.

use crate::{CpuState, GicState, HypervisorDeviceError, HypervisorVmError};
use std::any::Any;
use std::result;

/// Errors thrown while setting up the VGIC.
#[derive(Debug)]
pub enum Error {
    /// Error while calling KVM ioctl for setting up the global interrupt controller.
    CreateGic(HypervisorVmError),
    /// Error while setting device attributes for the GIC.
    SetDeviceAttribute(HypervisorDeviceError),
    /// Error while getting device attributes for the GIC.
    GetDeviceAttribute(HypervisorDeviceError),
}
pub type Result<T> = result::Result<T, Error>;

#[derive(Debug)]
pub struct VgicConfig {
    pub vcpu_count: u64,
    pub dist_addr: u64,
    pub dist_size: u64,
    pub redists_addr: u64,
    pub redists_size: u64,
    pub msi_addr: u64,
    pub msi_size: u64,
    pub nr_irqs: u32,
}

/// Hypervisor agnostic interface for a virtualized GIC
pub trait Vgic: Send + Sync {
    /// Returns the fdt compatibility property of the device
    fn fdt_compatibility(&self) -> &str;

    /// Returns the maint_irq fdt property of the device
    fn fdt_maint_irq(&self) -> u32;

    /// Returns an array with GIC device properties
    fn device_properties(&self) -> [u64; 4];

    /// Returns the number of vCPUs this GIC handles
    fn vcpu_count(&self) -> u64;

    /// Returns whether the GIC device is MSI compatible or not
    fn msi_compatible(&self) -> bool;

    /// Returns the MSI compatibility property of the device
    fn msi_compatibility(&self) -> &str;

    /// Returns the MSI reg property of the device
    fn msi_properties(&self) -> [u64; 2];

    /// Get the values of GICR_TYPER for each vCPU.
    fn set_gicr_typers(&mut self, vcpu_states: &[CpuState]);

    /// Downcast the trait object to its concrete type.
    fn as_any_concrete_mut(&mut self) -> &mut dyn Any;

    /// Save the state of GICv3ITS.
    fn state(&self) -> Result<GicState>;

    /// Restore the state of GICv3ITS.
    fn set_state(&mut self, state: &GicState) -> Result<()>;

    /// Saves GIC internal data tables into RAM.
    fn save_data_tables(&self) -> Result<()>;
}

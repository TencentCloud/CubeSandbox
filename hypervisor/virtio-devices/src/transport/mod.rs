// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

use vmm_sys_util::eventfd::EventFd;
mod pci_common_config;
mod pci_device;
pub use pci_common_config::VirtioPciCommonConfig;
pub use pci_device::{VirtioPciDevice, VirtioPciDeviceActivator};

pub trait VirtioTransport {
    fn ioeventfds(&self, base_addr: u64) -> Vec<(&EventFd, u64)>;
}

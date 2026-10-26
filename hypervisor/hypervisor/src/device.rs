// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 OR BSD-3-Clause
//
// Copyright © 2020, Microsoft Corporation
//
// Copyright 2018-2019 CrowdStrike, Inc.
//
// Copyright 2020, ARM Limited
//

use thiserror::Error;

#[derive(Error, Debug)]
///
/// Enum for device error
pub enum HypervisorDeviceError {
    ///
    /// Set device attribute error
    ///
    #[error("Failed to set device attribute: {0}")]
    SetDeviceAttribute(#[source] anyhow::Error),
    ///
    /// Get device attribute error
    ///
    #[error("Failed to get device attribute: {0}")]
    GetDeviceAttribute(#[source] anyhow::Error),
}

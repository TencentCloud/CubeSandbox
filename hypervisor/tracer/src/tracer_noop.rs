// Copyright © 2022 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

#[macro_export]
macro_rules! trace_scoped {
    ($event:expr) => {};
}

#[macro_export]
macro_rules! trace_point {
    ($event:expr) => {};
}

pub fn end() {}
pub fn start() {}

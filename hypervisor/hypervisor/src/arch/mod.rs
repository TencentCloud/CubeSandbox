// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE-BSD-3-Clause file.
//
// Copyright © 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0 OR BSD-3-Clause
//
// Copyright © 2020, Microsoft Corporation
//
// Copyright 2022 Arm Limited (or its affiliates). All rights reserved.
//

pub mod emulator;

#[cfg(target_arch = "x86_64")]
#[macro_use]
pub mod x86;

#[cfg(target_arch = "aarch64")]
pub mod aarch64;

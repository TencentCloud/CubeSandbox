// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Cube-specific (non-e2b-compatible) handlers.
//!
//! These endpoints are extensions that do **not** exist in the e2b API
//! surface. They are grouped under their own module and mounted behind the
//! `/cube` route prefix so the e2b-compatible surface stays clean and the two
//! contracts never get conflated.

pub mod exec_code;

pub use exec_code::exec_code;

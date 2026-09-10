// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! `/files` REST service facade.
//!
//! The public handlers remain at `rest::files::{download, upload}` while
//! download, upload and shared error/authentication logic live in separate
//! modules. The split is structural only; HTTP behavior is defined by the
//! existing handlers and tests.

mod download;
mod errors;
#[cfg(test)]
mod tests;
mod upload;

pub use download::download;
pub use upload::upload;

#[cfg(test)]
pub(crate) use download::modtime_of;
#[cfg(test)]
pub(crate) use upload::{entry_for, parse_boundary, spawn_upload_writer};

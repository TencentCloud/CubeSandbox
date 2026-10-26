// Copyright © 2022 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

#[cfg(feature = "tracing")]
#[macro_use]
extern crate log;

#[cfg(not(feature = "tracing"))]
mod tracer_noop;
#[cfg(not(feature = "tracing"))]
pub use tracer_noop::*;

#[cfg(feature = "tracing")]
mod tracer;
#[cfg(feature = "tracing")]
pub use tracer::*;

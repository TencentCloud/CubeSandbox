// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

#[cfg(panic = "abort")]
compile_error!("cube-envd requires panic=unwind so fatal request panics can fail closed");

mod cgroup;
pub mod cli;
#[cfg(feature = "test-support")]
#[doc(hidden)]
pub mod conformance;
pub mod error;
pub mod init;
pub mod process;
pub mod runtime;
pub mod server;
pub mod telemetry;
pub mod transport;

pub mod proto {
    connectrpc::include_generated!();
}

pub const COMPATIBILITY_VERSION: &str = "0.5.7";
pub const PRODUCT_VERSION: &str = env!("CUBE_ENVD_VERSION");
pub const REVISION: &str = env!("CUBE_ENVD_COMMIT");
pub const PROVIDER: &str = "cube";

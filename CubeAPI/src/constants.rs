// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! API-facing constants shared across sandbox responses.

/// Reported `envdVersion` for sandbox APIs (create, connect, list, get, resume, etc.).
pub const ENVD_VERSION: &str = "0.2.0";

/// E2B `envd` listens on this port inside every sandbox.
pub const ENVD_PORT: u32 = 49983;
pub const ENVD_PORT_STR: &str = "49983";

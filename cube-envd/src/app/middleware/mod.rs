// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! HTTP response adapters that exist for client compatibility rather than for
//! the protocol itself: the legacy Python SDK downgrade and the CORS
//! middleware upstream wraps its whole server in.

pub mod cors;
pub mod legacy;

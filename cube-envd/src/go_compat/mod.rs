// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Go compatibility helpers: byte-exact reproductions of Go runtime strings
//! that the wire contract depends on. Everything "copied from Go" lives here
//! (mirrors upstream's `internal/services/legacy/` precedent of keeping
//! compatibility shims in one place).

pub mod errno;

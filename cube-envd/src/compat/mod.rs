// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Go baseline vocabulary: byte-exact reproductions of the strings the Go envd
//! baseline puts on the wire.
//!
//! Invariant: this directory holds data tables and pure functions only — no
//! I/O, no state, no decisions. That is what makes the fidelity layer
//! auditable on its own.
//!
//! Source: `go_compat/`.

pub mod vocab;

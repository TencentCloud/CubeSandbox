// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! `process.Process` domain: the RPC surface, the process table, the child
//! execution engine and the per-command cgroup boundary.
//!
//! Contract: upstream `internal/services/process/` (start/connect/input/list/
//! signal/update + handler/), `internal/services/cgroups/` and the cgroup
//! placement of upstream `cmd/envd/main.go`.
//!
//! This file is the facade only: the eight RPC handlers live in `command`, the
//! attachment pump in `pump`, the post-spawn lifecycle in `supervisor`, the
//! termination marker in `metadata`, the wire shapes in `wire`, the child
//! machinery in `engine/`, and the resource boundary in `cgroup/`.

mod command;
mod metadata;
mod pump;
mod supervisor;

pub mod cgroup;
pub mod engine;
pub mod table;
pub mod wire;

pub use command::{
    close_stdin, connect, list, send_input, send_signal, start, stream_input_event, update,
};
pub use pump::{empty_stream_response, frame_stream_response, stream_error_response};

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! `process.Process` service facade.
//!
//! Public functions remain at `services::process::*`; implementation details
//! are grouped by command handling, stream delivery, supervision and
//! termination metadata.

mod command;
mod metadata;
mod stream;
mod supervisor;

pub use command::{
    close_stdin, connect, list, send_input, send_signal, start, stream_input_event, update,
};
pub use stream::{empty_stream_response, frame_stream_response, stream_error_response};

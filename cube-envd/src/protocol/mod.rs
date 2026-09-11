// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Connect wire layer: byte-exact framing and stream delivery, the timeout and
//! keepalive request headers, and the error envelope each surface answers with.
//!
//! Transport-agnostic: nothing here knows about a service or a domain. The
//! only downward dependency is `compat`, from which `error` borrows the Go
//! error-text table.
//!
//! One file, one question: `frames` codec, `stream` delivery, `keepalive`
//! cadence, `timeout` deadline, `error` error types and HTTP mapping.
//!
//! Source: `connect.rs` (frames/keepalive/timeout), `connect/stream.rs`,
//! `error.rs`.

pub mod error;
pub mod frames;
pub mod keepalive;
pub mod stream;
pub mod timeout;

pub use error::{ConnectCode, ConnectError, RestError};
pub use frames::{
    check_json_codec, decode_single_envelope, end_stream_error, end_stream_ok, message_frame,
    EnvelopeDecoder, MAX_ENVELOPE_SIZE, MAX_UNARY_BODY, STREAM_CONTENT_TYPE,
};
pub use keepalive::keepalive_interval_from_headers;
pub use timeout::timeout_from_headers;

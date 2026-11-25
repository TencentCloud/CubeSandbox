// Copyright 2026 Tencent Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//! General-purpose Network Block Device (NBD) client library.
//!
//! Provides:
//! - [`NbdUri`] / [`is_nbd_uri`] — NBD URI parser (`nbd://` and `nbd+unix://`, plus timeout/reconnect query params)
//! - [`NbdConn`] — standalone synchronous client (`read_at`, `write_at`, `flush`, `disconnect`)

pub mod conn;
pub mod handshake;
pub mod proto;
pub mod uri;

pub use crate::conn::{
    NbdConn, NbdConnectError, NbdSocketOptions, TcpKeepalive, DEFAULT_HANDSHAKE_TIMEOUT,
    MAX_TCP_KEEPALIVE_COUNT, MAX_TCP_KEEPALIVE_SECS,
};
pub use crate::handshake::{negotiate, NbdExportInfo, NbdHandshakeError};
pub use crate::proto::*;
pub use crate::uri::{is_nbd_uri, NbdAddress, NbdUri, NbdUriError};

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Shared helpers for talking to the in-guest envd daemon over the Connect-JSON
//! protocol.  This module is used by both the agenthub command proxy and the
//! Web Terminal bridge so that both features share the same wire-format
//! implementation and do not drift apart.

use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};

/// Default envd port inside the sandbox.
pub const ENVD_PORT: u16 = 49983;

/// Content-Type for Connect-JSON streaming RPCs.
pub const CONNECT_JSON: &str = "application/connect+json";

/// Connect-Protocol-Version header value.
pub const CONNECT_PROTOCOL_VERSION: &str = "1";

/// Connect end-of-stream flag (bit 1 of the frame flags byte).
pub const CONNECT_END_STREAM_FLAG: u8 = 0b10;

/// Connect compressed payload flag (bit 0 of the frame flags byte).  envd may
/// set this on trailer frames but never on data frames; we reject it if seen.
pub const CONNECT_COMPRESSED_FLAG: u8 = 0b01;

/// Wrap a JSON payload in the 5-byte Connect envelope:
///   [1 byte flags][4 bytes big-endian length][payload]
pub fn connect_envelope(payload: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(payload.len() + 5);
    out.push(0);
    out.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    out.extend_from_slice(payload);
    out
}

/// envd-facing Host header value used to route the request into the sandbox.
/// Format: `{port}-{sandbox_id}.{domain}`.
pub fn envd_host(port: u16, sandbox_id: &str, domain: &str) -> String {
    format!("{}-{}.{}", port, sandbox_id, domain)
}

/// Base URL of the sidecar/proxy that forwards requests into the sandbox.
/// Defaults to `http://127.0.0.1` and can be overridden with the
/// `AGENTHUB_SANDBOX_PROXY_URL` environment variable.
pub fn envd_proxy_url() -> String {
    std::env::var("AGENTHUB_SANDBOX_PROXY_URL")
        .unwrap_or_else(|_| "http://127.0.0.1".to_string())
        .trim_end_matches('/')
        .to_string()
}

/// Full URL for a `process.Process/{method}` RPC against the envd proxy.
pub fn envd_process_url(method: &str) -> String {
    format!("{}/process.Process/{}", envd_proxy_url(), method)
}

/// Build the `Authorization: Basic ...` header value used by envd.
///
/// An empty or missing password is treated as no password, matching the
/// default envd configuration and the Go/Python SDKs.
pub fn basic_auth_header(username: &str, password: Option<&str>) -> String {
    let password = password.unwrap_or("");
    let creds = BASE64.encode(format!("{}:{}", username, password));
    format!("Basic {}", creds)
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Minimal ConnectRPC server plumbing for the JSON codec.
//!
//! Every known client (repo Python/Node/Go SDKs and the official e2b SDK)
//! speaks Connect over JSON only, so this layer intentionally implements
//! just that:
//! - unary RPCs: `Content-Type: application/json`, plain JSON bodies both
//!   ways, errors as HTTP status + `{"code","message"}`;
//! - server-streaming RPCs: `Content-Type: application/connect+json`, both
//!   directions framed as `[flags:1B][len:u32 BE][payload]`, stream always
//!   terminated by an EndStream frame (flags bit 0x02) whose payload is `{}`
//!   on success or `{"error":{"code","message"}}` on failure — streaming
//!   errors never use HTTP status codes (baseline-verified).
//!
//! Binary protobuf codecs (`application/proto`, `application/connect+proto`)
//! are rejected with `unimplemented` — a declared MVP difference.

use bytes::{BufMut, Bytes, BytesMut};

use crate::error::{ConnectCode, ConnectError};

pub const END_STREAM_FLAG: u8 = 0x02;
pub const COMPRESSED_FLAG: u8 = 0x01;
/// Same cap the SDKs enforce on their side.
pub const MAX_ENVELOPE_SIZE: usize = 64 * 1024 * 1024;
pub const STREAM_CONTENT_TYPE: &str = "application/connect+json";

/// Encode one Connect streaming envelope.
pub fn encode_envelope(flags: u8, payload: &[u8]) -> Bytes {
    let mut buf = BytesMut::with_capacity(5 + payload.len());
    buf.put_u8(flags);
    buf.put_u32(payload.len() as u32);
    buf.put_slice(payload);
    buf.freeze()
}

pub fn message_frame(value: &serde_json::Value) -> Bytes {
    encode_envelope(0, value.to_string().as_bytes())
}

pub fn end_stream_ok() -> Bytes {
    encode_envelope(END_STREAM_FLAG, b"{}")
}

pub fn end_stream_error(err: &ConnectError) -> Bytes {
    let payload = serde_json::json!({
        "error": { "code": err.code.as_str(), "message": err.message }
    });
    encode_envelope(END_STREAM_FLAG, payload.to_string().as_bytes())
}

/// Decode the first envelope from a fully-buffered streaming request body.
///
/// Server-streaming RPCs carry exactly one request message, so only the
/// first envelope is decoded. Trailing bytes after it (a malformed client
/// sending multiple envelopes) are ignored rather than rejected — upstream
/// Go envd errors on that shape; accepting the leading message never
/// executes anything the client didn't ask for. A truncated or compressed
/// first envelope is still rejected loudly.
pub fn decode_single_envelope(body: &[u8]) -> Result<Vec<u8>, ConnectError> {
    if body.len() < 5 {
        return Err(ConnectError::new(
            ConnectCode::InvalidArgument,
            "truncated Connect envelope: missing 5-byte header",
        ));
    }
    let flags = body[0];
    if flags & COMPRESSED_FLAG != 0 {
        return Err(ConnectError::new(
            ConnectCode::Internal,
            "compressed Connect stream messages are not supported",
        ));
    }
    let size = u32::from_be_bytes([body[1], body[2], body[3], body[4]]) as usize;
    if size > MAX_ENVELOPE_SIZE {
        return Err(ConnectError::new(
            ConnectCode::InvalidArgument,
            format!("Connect stream message too large: {size} bytes"),
        ));
    }
    if body.len() < 5 + size {
        return Err(ConnectError::new(
            ConnectCode::InvalidArgument,
            format!(
                "truncated Connect envelope: declared {size} bytes, got {}",
                body.len() - 5
            ),
        ));
    }
    Ok(body[5..5 + size].to_vec())
}

/// Parse the `Connect-Timeout-Ms` request header.
pub fn timeout_from_headers(headers: &axum::http::HeaderMap) -> Option<std::time::Duration> {
    let raw = headers.get("connect-timeout-ms")?.to_str().ok()?;
    let ms: u64 = raw.trim().parse().ok()?;
    Some(std::time::Duration::from_millis(ms))
}

/// Reject binary-proto content types up front with a stable error.
pub fn check_json_codec(headers: &axum::http::HeaderMap) -> Result<(), ConnectError> {
    let ct = headers
        .get(axum::http::header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    if ct.contains("proto") {
        return Err(ConnectError::new(
            ConnectCode::Unimplemented,
            "binary protobuf codec is not supported by cube-envd; use the JSON codec (application/json or application/connect+json)",
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn envelope_roundtrip() {
        let frame = encode_envelope(0, br#"{"a":1}"#);
        assert_eq!(frame[0], 0);
        assert_eq!(
            u32::from_be_bytes([frame[1], frame[2], frame[3], frame[4]]),
            7
        );
        let payload = decode_single_envelope(&frame).unwrap();
        assert_eq!(payload, br#"{"a":1}"#);
    }

    #[test]
    fn end_stream_frames() {
        let ok = end_stream_ok();
        assert_eq!(ok[0], END_STREAM_FLAG);
        assert_eq!(&ok[5..], b"{}");

        let err = end_stream_error(&ConnectError::new(
            ConnectCode::DeadlineExceeded,
            "context deadline exceeded",
        ));
        assert_eq!(err[0], END_STREAM_FLAG);
        let v: serde_json::Value = serde_json::from_slice(&err[5..]).unwrap();
        assert_eq!(v["error"]["code"], "deadline_exceeded");
    }

    #[test]
    fn decode_rejects_compressed_and_truncated() {
        let compressed = encode_envelope(COMPRESSED_FLAG, b"x");
        assert!(decode_single_envelope(&compressed).is_err());
        assert!(decode_single_envelope(b"\x00\x00\x00").is_err());
        // Declared size larger than actual payload.
        let mut bad = encode_envelope(0, b"abc").to_vec();
        bad[4] = 200;
        assert!(decode_single_envelope(&bad).is_err());
    }

    #[test]
    fn timeout_header_parsing() {
        let mut headers = axum::http::HeaderMap::new();
        assert!(timeout_from_headers(&headers).is_none());
        headers.insert("connect-timeout-ms", "1500".parse().unwrap());
        assert_eq!(
            timeout_from_headers(&headers),
            Some(std::time::Duration::from_millis(1500))
        );
    }

    #[test]
    fn proto_codec_rejected() {
        let mut headers = axum::http::HeaderMap::new();
        headers.insert("content-type", "application/connect+proto".parse().unwrap());
        assert!(check_json_codec(&headers).is_err());
        headers.insert("content-type", "application/connect+json".parse().unwrap());
        assert!(check_json_codec(&headers).is_ok());
    }
}

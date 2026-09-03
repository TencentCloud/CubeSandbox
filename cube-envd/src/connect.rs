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

use bytes::{Buf, BufMut, Bytes, BytesMut};

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

/// Incremental decoder for Connect client-streaming request envelopes. It
/// keeps at most one incomplete frame between body chunks and enforces the
/// same per-message limit as server-streaming requests.
#[derive(Default)]
pub struct EnvelopeDecoder {
    buffered: BytesMut,
}

impl EnvelopeDecoder {
    pub fn push(&mut self, chunk: &[u8]) {
        self.buffered.extend_from_slice(chunk);
    }

    pub fn next_message(&mut self) -> Result<Option<Bytes>, ConnectError> {
        if self.buffered.len() < 5 {
            return Ok(None);
        }
        let flags = self.buffered[0];
        if flags & COMPRESSED_FLAG != 0 {
            return Err(ConnectError::new(
                ConnectCode::Internal,
                "compressed Connect stream messages are not supported",
            ));
        }
        if flags != 0 {
            return Err(ConnectError::new(
                ConnectCode::InvalidArgument,
                format!("unexpected Connect request envelope flags: 0x{flags:02x}"),
            ));
        }
        let size = u32::from_be_bytes([
            self.buffered[1],
            self.buffered[2],
            self.buffered[3],
            self.buffered[4],
        ]) as usize;
        if size > MAX_ENVELOPE_SIZE {
            return Err(ConnectError::new(
                ConnectCode::InvalidArgument,
                format!("Connect stream message too large: {size} bytes"),
            ));
        }
        if self.buffered.len() < 5 + size {
            return Ok(None);
        }

        let mut frame = self.buffered.split_to(5 + size);
        frame.advance(5);
        Ok(Some(frame.freeze()))
    }

    pub fn finish(self) -> Result<(), ConnectError> {
        if self.buffered.is_empty() {
            return Ok(());
        }
        if self.buffered.len() < 5 {
            return Err(ConnectError::new(
                ConnectCode::InvalidArgument,
                "truncated Connect envelope: missing 5-byte header",
            ));
        }
        let size = u32::from_be_bytes([
            self.buffered[1],
            self.buffered[2],
            self.buffered[3],
            self.buffered[4],
        ]) as usize;
        Err(ConnectError::new(
            ConnectCode::InvalidArgument,
            format!(
                "truncated Connect envelope: declared {size} bytes, got {}",
                self.buffered.len() - 5
            ),
        ))
    }
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

/// Default keepalive ping cadence for a quiet Start stream. cube-envd keeps
/// 30s rather than upstream's 90s: the LB in front of CubeProxy has an unknown
/// idle timeout (typically 60s), so 30s stays safely under any LB that is
/// >= 30s.
pub const DEFAULT_KEEPALIVE_INTERVAL: std::time::Duration = std::time::Duration::from_secs(30);

/// Parse the `Keepalive-Ping-Interval` request header (integer seconds).
///
/// Mirrors upstream `permissions.GetKeepAliveTicker`: the header tunes only the
/// cadence; keepalive is always on for a quiet Start stream and there is no
/// request field that gates it. An absent, non-numeric, non-positive, or
/// oversized value falls back to [`DEFAULT_KEEPALIVE_INTERVAL`].
///
/// The value is parsed as `u32` deliberately: an interval above `u32::MAX`
/// seconds (~136 years) is rejected rather than letting `Duration::from_secs`
/// overflow. Upstream feeds the header straight into `time.NewTicker`, which
/// panics for 0/negative/duration-overflowing values; cube-envd degrades to
/// the default instead of crashing.
pub fn keepalive_interval_from_headers(headers: &axum::http::HeaderMap) -> std::time::Duration {
    let secs = headers
        .get("keepalive-ping-interval")
        .and_then(|v| v.to_str().ok())
        .and_then(|s| s.trim().parse::<u32>().ok())
        .filter(|&s| s > 0);
    match secs {
        Some(s) => std::time::Duration::from_secs(u64::from(s)),
        None => DEFAULT_KEEPALIVE_INTERVAL,
    }
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
    fn incremental_decoder_handles_chunk_boundaries_and_multiple_frames() {
        let first = encode_envelope(0, br#"{"start":{}}"#);
        let second = encode_envelope(0, br#"{"keepalive":{}}"#);
        let joined = [first.as_ref(), second.as_ref()].concat();
        let mut decoder = EnvelopeDecoder::default();

        decoder.push(&joined[..3]);
        assert!(decoder.next_message().unwrap().is_none());
        decoder.push(&joined[3..first.len() + 2]);
        assert_eq!(
            decoder.next_message().unwrap().unwrap().as_ref(),
            br#"{"start":{}}"#
        );
        assert!(decoder.next_message().unwrap().is_none());
        decoder.push(&joined[first.len() + 2..]);
        assert_eq!(
            decoder.next_message().unwrap().unwrap().as_ref(),
            br#"{"keepalive":{}}"#
        );
        assert!(decoder.next_message().unwrap().is_none());
        decoder.finish().unwrap();
    }

    #[test]
    fn incremental_decoder_rejects_flags_and_truncated_tail() {
        let mut decoder = EnvelopeDecoder::default();
        decoder.push(&encode_envelope(END_STREAM_FLAG, b"{}"));
        assert_eq!(
            decoder.next_message().unwrap_err().code,
            ConnectCode::InvalidArgument
        );

        let frame = encode_envelope(0, b"abcdef");
        let mut decoder = EnvelopeDecoder::default();
        decoder.push(&frame[..frame.len() - 1]);
        assert!(decoder.next_message().unwrap().is_none());
        assert_eq!(
            decoder.finish().unwrap_err().code,
            ConnectCode::InvalidArgument
        );
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
    fn keepalive_interval_header_parsing() {
        let mut headers = axum::http::HeaderMap::new();
        // Absent -> default.
        assert_eq!(
            keepalive_interval_from_headers(&headers),
            DEFAULT_KEEPALIVE_INTERVAL
        );
        // Integer seconds override (case-insensitive header name).
        headers.insert("Keepalive-Ping-Interval", "90".parse().unwrap());
        assert_eq!(
            keepalive_interval_from_headers(&headers),
            std::time::Duration::from_secs(90)
        );
        // Non-numeric / zero / negative / oversized -> default.
        for bad in ["abc", "0", "-5", "", "4294967296", "18446744073709551616"] {
            headers.insert("keepalive-ping-interval", bad.parse().unwrap());
            assert_eq!(
                keepalive_interval_from_headers(&headers),
                DEFAULT_KEEPALIVE_INTERVAL,
                "value {bad:?} should fall back to default"
            );
        }
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

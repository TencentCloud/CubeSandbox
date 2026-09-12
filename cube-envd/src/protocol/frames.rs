// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Connect JSON codec: envelope framing, the EndStream trailer, the
//! incremental/single-envelope decoders, and the binary-proto rejection gate.
//!
//! Wire contract: `[flags:1B][len:u32 BE][payload]`; an EndStream frame
//! (flags bit 0x02) always terminates a stream, carrying `{}` on success or
//! `{"error":{"code","message"}}` on failure. Binary protobuf codecs
//! (`application/proto`, `application/connect+proto`) are rejected with
//! `unimplemented` — a declared difference.
//!
//! Source: `connect.rs` (framing half; split out by responsibility).

use bytes::{Buf, BufMut, Bytes, BytesMut};

use crate::protocol::error::{ConnectCode, ConnectError};

pub const END_STREAM_FLAG: u8 = 0x02;
pub const COMPRESSED_FLAG: u8 = 0x01;
/// Same cap the SDKs enforce on their side.
pub const MAX_ENVELOPE_SIZE: usize = 64 * 1024 * 1024;
/// Cap on a unary (non-streaming) request body. The streaming counterpart of
/// [`MAX_ENVELOPE_SIZE`]; both exist so a malformed or hostile client cannot
/// make the daemon buffer without bound.
///
/// It lives here rather than next to either reader so the two `app` modules
/// that enforce it (`handlers` unary RPCs, `lifecycle` `/init`) share it
/// without depending on each other.
pub const MAX_UNARY_BODY: usize = 4 * 1024 * 1024;
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
    fn proto_codec_rejected() {
        let mut headers = axum::http::HeaderMap::new();
        headers.insert("content-type", "application/connect+proto".parse().unwrap());
        assert!(check_json_codec(&headers).is_err());
        headers.insert("content-type", "application/connect+json".parse().unwrap());
        assert!(check_json_codec(&headers).is_ok());
    }
}

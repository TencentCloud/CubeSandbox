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
/// `[flags:1B][len:u32 BE]` — the prefix in front of every envelope payload.
pub const FRAME_HEADER_LEN: usize = 5;
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
    let mut buf = BytesMut::with_capacity(FRAME_HEADER_LEN + payload.len());
    buf.put_u8(flags);
    buf.put_u32(payload.len() as u32);
    buf.put_slice(payload);
    buf.freeze()
}

pub fn message_frame(value: &serde_json::Value) -> Bytes {
    encode_envelope(0, value.to_string().as_bytes())
}

/// Frame one JSON message without building a `Value` tree or an intermediate
/// string: the envelope header is written first and `serde_json` streams the
/// value directly behind it, so a large payload is copied once instead of
/// three times. Errors cannot be reported here (the caller has no way to
/// answer mid-stream), so a serialization failure ends the message short and
/// the trailer still follows.
pub fn json_message_frame<T: serde::Serialize>(value: &T) -> Bytes {
    let mut buf = Vec::with_capacity(FRAME_HEADER_LEN + 1024);
    buf.extend_from_slice(&[0; FRAME_HEADER_LEN]);
    match serde_json::to_writer(&mut buf, value) {
        Ok(()) => {
            let len = (buf.len() - FRAME_HEADER_LEN) as u32;
            buf[1..FRAME_HEADER_LEN].copy_from_slice(&len.to_be_bytes());
        }
        Err(e) => {
            tracing::warn!("message_frame: could not serialize the envelope: {e}");
            buf.truncate(FRAME_HEADER_LEN);
            buf[1..FRAME_HEADER_LEN].copy_from_slice(&0u32.to_be_bytes());
        }
    }
    Bytes::from(buf)
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

    /// `json_message_frame` frames the same JSON `message_frame(&to_value(..))`
    /// does, without the `Value` tree. Key *order* inside an object is not part
    /// of the Connect/JSON contract, and it is the one thing that differs: this
    /// path emits the serializer's declaration order, the `Value` path emits
    /// sorted keys. Everything a client observes must still match.
    #[test]
    fn json_message_frame_frames_the_same_json_as_the_value_path() {
        #[derive(serde::Serialize)]
        struct Probe {
            payload: String,
            empty: Option<String>,
        }
        for payload in [
            String::new(),
            "x".repeat(4096),
            "quote \" backslash \\ newline \n tab \t unicode \u{1f600}".into(),
        ] {
            let probe = Probe {
                payload,
                empty: None,
            };
            let actual = json_message_frame(&probe);
            assert_eq!(actual[0], 0, "a message frame is not an end-stream frame");
            let len = u32::from_be_bytes([actual[1], actual[2], actual[3], actual[4]]) as usize;
            assert_eq!(len, actual.len() - FRAME_HEADER_LEN, "length prefix");
            let payload = &actual[FRAME_HEADER_LEN..];
            let decoded: serde_json::Value = serde_json::from_slice(payload).unwrap();
            let expected = serde_json::to_value(&probe).unwrap();
            assert_eq!(decoded, expected, "same JSON value, whatever the key order");
            // Same payload bytes as serializing straight into a buffer, which is
            // what the header is glued in front of.
            assert_eq!(payload, serde_json::to_vec(&probe).unwrap());
            assert!(MAX_ENVELOPE_SIZE >= decoded.to_string().len());
        }
    }

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

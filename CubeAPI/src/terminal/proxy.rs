// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! envd Connect protocol proxy.
//!
//! This module implements the client side of the envd `process.Process/Connect`
//! streaming protocol, bridging raw WebSocket binary frames to/from the envd
//! HTTP streaming endpoint inside a sandbox container.
//!
//! ## Wire format
//!
//! All messages are framed with a 5-byte header:
//! ```text
//! [flags: u8][length: u32 big-endian][payload: length bytes]
//! ```
//!
//! Flag values:
//! - `0x00` – data frame (stdin from client, or JSON event from server)
//! - `0x01` – compressed (not supported; treated as error)
//! - `0x02` – end-of-stream
//!
//! Server → client payloads are JSON-encoded `processStartResponse` objects
//! with base64-encoded stdout/stderr/pty fields.
//!
//! Client → server payloads are raw bytes (terminal stdin).

use base64::{engine::general_purpose::STANDARD as BASE64, Engine};
use bytes::{BufMut, Bytes, BytesMut};
use serde::Deserialize;

/// Maximum allowed envelope payload size (64 MiB — matches Go SDK).
const MAX_ENVELOPE_SIZE: u32 = 64 * 1024 * 1024;

/// Maximum total frame buffer size before the connection is considered
/// malicious or broken (2× max envelope = one frame + partial header).
const MAX_BUFFER_SIZE: usize = 2 * MAX_ENVELOPE_SIZE as usize;

/// Connect envelope flag: regular data frame.
pub const FLAG_DATA: u8 = 0x00;
/// Connect envelope flag: compressed (unsupported).
pub const FLAG_COMPRESSED: u8 = 0x01;
/// Connect envelope flag: end-of-stream.
pub const FLAG_END_STREAM: u8 = 0x02;

/// A parsed envd stream event.
#[derive(Debug, Clone)]
pub enum EnvdEvent {
    /// stdout data (raw bytes, already base64-decoded).
    Stdout(Vec<u8>),
    /// stderr data (raw bytes, already base64-decoded).
    Stderr(Vec<u8>),
    /// PTY output (raw bytes).
    Pty(Vec<u8>),
    /// Process started with PID.
    Start { pid: i32 },
    /// Process ended with exit code.
    End { exit_code: i32 },
    /// Keepalive heartbeat.
    Keepalive,
}

/// Deserialization target for envd JSON events.
#[derive(Debug, Deserialize)]
struct ProcessStartResponse {
    event: Option<ProcessEvent>,
}

#[derive(Debug, Deserialize)]
struct ProcessEvent {
    start: Option<ProcessStartEvent>,
    data: Option<ProcessDataEvent>,
    end: Option<ProcessEndEvent>,
    keepalive: Option<serde_json::Value>,
}

#[derive(Debug, Deserialize)]
struct ProcessStartEvent {
    pid: i32,
}

#[derive(Debug, Deserialize)]
struct ProcessDataEvent {
    stdout: Option<String>,
    stderr: Option<String>,
    pty: Option<String>,
}

#[derive(Debug, Deserialize)]
struct ProcessEndEvent {
    #[serde(rename = "exitCode", alias = "exit_code")]
    exit_code: Option<i32>,
    error: Option<String>,
}

/// Encode raw stdin bytes into a Connect envelope.
pub fn encode_stdin_frame(data: &[u8]) -> Bytes {
    let mut buf = BytesMut::with_capacity(5 + data.len());
    buf.put_u8(FLAG_DATA);
    buf.put_u32(data.len() as u32);
    buf.put_slice(data);
    buf.freeze()
}

/// Encode an end-of-stream frame.
pub fn encode_end_stream_frame() -> Bytes {
    let mut buf = BytesMut::with_capacity(5);
    buf.put_u8(FLAG_END_STREAM);
    buf.put_u32(0);
    buf.freeze()
}

/// Encode a Connect envelope with a raw payload (used for custom messages).
pub fn encode_frame(flags: u8, payload: &[u8]) -> Bytes {
    let mut buf = BytesMut::with_capacity(5 + payload.len());
    buf.put_u8(flags);
    buf.put_u32(payload.len() as u32);
    buf.put_slice(payload);
    buf.freeze()
}

/// Read one Connect envelope from a byte stream.
///
/// Returns `(flags, payload)` on success, or an error if the frame is
/// malformed or too large.
pub fn decode_frame(data: &[u8]) -> Result<(u8, &[u8]), String> {
    if data.len() < 5 {
        return Err("incomplete frame header".to_string());
    }
    let flags = data[0];
    let length = u32::from_be_bytes([data[1], data[2], data[3], data[4]]);
    if length > MAX_ENVELOPE_SIZE {
        return Err(format!(
            "frame too large: {} bytes (max {})",
            length, MAX_ENVELOPE_SIZE
        ));
    }
    let payload_end = 5 + length as usize;
    if data.len() < payload_end {
        return Err(format!(
            "truncated frame: expected {} bytes, got {}",
            payload_end,
            data.len()
        ));
    }
    Ok((flags, &data[5..payload_end]))
}

/// Parse a server-sent JSON event payload into zero or more `EnvdEvent`s.
///
/// A single envelope may contain multiple data fields (stdout + stderr + pty
/// can appear together in one event).
pub fn parse_event(payload: &[u8]) -> Result<Vec<EnvdEvent>, String> {
    let response: ProcessStartResponse =
        serde_json::from_slice(payload).map_err(|e| format!("decode process event: {}", e))?;

    let event = match response.event {
        Some(e) => e,
        None => return Ok(Vec::new()),
    };

    let mut events = Vec::new();

    if let Some(start) = event.start {
        events.push(EnvdEvent::Start { pid: start.pid });
    }

    if let Some(data) = event.data {
        if let Some(stdout) = data.stdout {
            let raw = BASE64
                .decode(&stdout)
                .map_err(|e| format!("decode stdout base64: {}", e))?;
            events.push(EnvdEvent::Stdout(raw));
        }
        if let Some(stderr) = data.stderr {
            let raw = BASE64
                .decode(&stderr)
                .map_err(|e| format!("decode stderr base64: {}", e))?;
            events.push(EnvdEvent::Stderr(raw));
        }
        if let Some(pty) = data.pty {
            let raw = BASE64
                .decode(&pty)
                .map_err(|e| format!("decode pty base64: {}", e))?;
            events.push(EnvdEvent::Pty(raw));
        }
    }

    if let Some(end) = event.end {
        let exit_code = end.exit_code.unwrap_or(-1);
        events.push(EnvdEvent::End { exit_code });
    }

    if event.keepalive.is_some() {
        events.push(EnvdEvent::Keepalive);
    }

    Ok(events)
}

/// Build the JSON request body for `POST /process.Process/Connect`.
///
/// This tells envd to start an interactive bash session with a PTY.
pub fn build_connect_request(cmd: &str, args: &[String]) -> serde_json::Value {
    serde_json::json!({
        "process": {
            "cmd": cmd,
            "args": args,
        },
        "stdin": true
    })
}

/// A buffer that accumulates bytes and yields complete Connect frames.
#[derive(Default)]
pub struct FrameBuffer {
    buf: BytesMut,
}

impl FrameBuffer {
    pub fn new() -> Self {
        Self {
            buf: BytesMut::new(),
        }
    }

    /// Append received bytes to the internal buffer.
    /// Returns an error if the total buffer exceeds `MAX_BUFFER_SIZE`.
    pub fn extend(&mut self, data: &[u8]) -> Result<(), String> {
        if self.buf.len() + data.len() > MAX_BUFFER_SIZE {
            return Err(format!(
                "frame buffer overflow: {} + {} > {}",
                self.buf.len(),
                data.len(),
                MAX_BUFFER_SIZE
            ));
        }
        self.buf.put_slice(data);
        Ok(())
    }

    /// Try to extract one complete frame from the buffer.
    /// Returns `Some((flags, payload))` if a complete frame is available,
    /// or `None` if more data is needed.
    pub fn try_take_frame(&mut self) -> Result<Option<(u8, Bytes)>, String> {
        if self.buf.len() < 5 {
            return Ok(None);
        }
        let length = u32::from_be_bytes([self.buf[1], self.buf[2], self.buf[3], self.buf[4]]);
        if length > MAX_ENVELOPE_SIZE {
            return Err(format!(
                "frame too large: {} bytes (max {})",
                length, MAX_ENVELOPE_SIZE
            ));
        }
        let frame_len = 5 + length as usize;
        if self.buf.len() < frame_len {
            return Ok(None);
        }
        let frame = self.buf.split_to(frame_len);
        let flags = frame[0];
        let payload = frame.freeze().slice(5..);
        Ok(Some((flags, payload)))
    }

    pub fn is_empty(&self) -> bool {
        self.buf.is_empty()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_encode_stdin_frame() {
        let frame = encode_stdin_frame(b"hello");
        assert_eq!(frame.len(), 10); // 5 header + 5 data
        assert_eq!(frame[0], FLAG_DATA);
        assert_eq!(u32::from_be_bytes([frame[1], frame[2], frame[3], frame[4]]), 5);
        assert_eq!(&frame[5..], b"hello");
    }

    #[test]
    fn test_encode_end_stream_frame() {
        let frame = encode_end_stream_frame();
        assert_eq!(frame.len(), 5);
        assert_eq!(frame[0], FLAG_END_STREAM);
        assert_eq!(u32::from_be_bytes([frame[1], frame[2], frame[3], frame[4]]), 0);
    }

    #[test]
    fn test_decode_frame() {
        let frame = encode_stdin_frame(b"test");
        let (flags, payload) = decode_frame(&frame).unwrap();
        assert_eq!(flags, FLAG_DATA);
        assert_eq!(payload, b"test");
    }

    #[test]
    fn test_decode_incomplete_frame() {
        assert!(decode_frame(b"\x00\x00\x00\x00").is_err()); // only 4 bytes
    }

    #[test]
    fn test_decode_truncated_payload() {
        let mut buf = BytesMut::new();
        buf.put_u8(FLAG_DATA);
        buf.put_u32(100); // claim 100 bytes
        buf.put_slice(b"only 5"); // but only give 5
        assert!(decode_frame(&buf).is_err());
    }

    #[test]
    fn test_parse_event_start() {
        let json = r#"{"event":{"start":{"pid":42}}}"#;
        let events = parse_event(json.as_bytes()).unwrap();
        assert_eq!(events.len(), 1);
        match &events[0] {
            EnvdEvent::Start { pid } => assert_eq!(*pid, 42),
            _ => panic!("expected Start"),
        }
    }

    #[test]
    fn test_parse_event_data_stdout() {
        let encoded = BASE64.encode(b"hello world");
        let json = format!(r#"{{"event":{{"data":{{"stdout":"{}"}}}}}}"#, encoded);
        let events = parse_event(json.as_bytes()).unwrap();
        assert_eq!(events.len(), 1);
        match &events[0] {
            EnvdEvent::Stdout(data) => assert_eq!(data, b"hello world"),
            _ => panic!("expected Stdout"),
        }
    }

    #[test]
    fn test_parse_event_end() {
        let json = r#"{"event":{"end":{"exitCode":0}}}"#;
        let events = parse_event(json.as_bytes()).unwrap();
        assert_eq!(events.len(), 1);
        match &events[0] {
            EnvdEvent::End { exit_code } => assert_eq!(*exit_code, 0),
            _ => panic!("expected End"),
        }
    }

    #[test]
    fn test_parse_event_empty() {
        let json = r#"{}"#;
        let events = parse_event(json.as_bytes()).unwrap();
        assert!(events.is_empty());
    }

    #[test]
    fn test_frame_buffer_complete() {
        let mut buf = FrameBuffer::new();
        let frame = encode_stdin_frame(b"complete");
        buf.extend(&frame);
        let result = buf.try_take_frame().unwrap();
        assert!(result.is_some());
        let (flags, payload) = result.unwrap();
        assert_eq!(flags, FLAG_DATA);
        assert_eq!(&payload[..], b"complete");
        assert!(buf.is_empty());
    }

    #[test]
    fn test_frame_buffer_incomplete() {
        let mut buf = FrameBuffer::new();
        // Only send 3 bytes of header
        buf.extend(&[0x00, 0x00, 0x00]);
        let result = buf.try_take_frame().unwrap();
        assert!(result.is_none());
    }

    #[test]
    fn test_frame_buffer_partial_then_complete() {
        let mut buf = FrameBuffer::new();
        let frame = encode_stdin_frame(b"partial");
        // First half
        buf.extend(&frame[..7]);
        assert!(buf.try_take_frame().unwrap().is_none());
        // Rest
        buf.extend(&frame[7..]);
        let result = buf.try_take_frame().unwrap();
        assert!(result.is_some());
    }

    #[test]
    fn test_parse_event_stderr() {
        let encoded = BASE64.encode(b"error output");
        let json = format!(r#"{{"event":{{"data":{{"stderr":"{}"}}}}}}"#, encoded);
        let events = parse_event(json.as_bytes()).unwrap();
        assert_eq!(events.len(), 1);
        match &events[0] {
            EnvdEvent::Stderr(data) => assert_eq!(data, b"error output"),
            _ => panic!("expected Stderr"),
        }
    }

    #[test]
    fn test_parse_event_pty() {
        let encoded = BASE64.encode(b"pty data");
        let json = format!(r#"{{"event":{{"data":{{"pty":"{}"}}}}}}"#, encoded);
        let events = parse_event(json.as_bytes()).unwrap();
        assert_eq!(events.len(), 1);
        match &events[0] {
            EnvdEvent::Pty(data) => assert_eq!(data, b"pty data"),
            _ => panic!("expected Pty"),
        }
    }

    #[test]
    fn test_parse_event_keepalive() {
        let json = r#"{"event":{"keepalive":{}}}"#;
        let events = parse_event(json.as_bytes()).unwrap();
        assert_eq!(events.len(), 1);
        match &events[0] {
            EnvdEvent::Keepalive => {},
            _ => panic!("expected Keepalive"),
        }
    }

    #[test]
    fn test_parse_event_multiple_fields() {
        let stdout_b64 = BASE64.encode(b"hello");
        let stderr_b64 = BASE64.encode(b"error");
        let json = format!(
            r#"{{"event":{{"data":{{"stdout":"{}","stderr":"{}"}},"start":{{"pid":1}}}}}}"#,
            stdout_b64, stderr_b64
        );
        let events = parse_event(json.as_bytes()).unwrap();
        assert_eq!(events.len(), 3); // Start + Stdout + Stderr
        let has_start = events.iter().any(|e| matches!(e, EnvdEvent::Start { .. }));
        let has_stdout = events.iter().any(|e| matches!(e, EnvdEvent::Stdout(..)));
        let has_stderr = events.iter().any(|e| matches!(e, EnvdEvent::Stderr(..)));
        assert!(has_start);
        assert!(has_stdout);
        assert!(has_stderr);
    }

    #[test]
    fn test_build_connect_request() {
        let req = build_connect_request("/bin/bash", &["-l".to_string()]);
        let process = req.get("process").unwrap();
        assert_eq!(process["cmd"], "/bin/bash");
        assert_eq!(process["args"][0], "-l");
        assert_eq!(req["stdin"], true);
    }
}
use serde::Serialize;
use std::{fmt, io};
use tokio::io::{AsyncRead, AsyncReadExt};

pub const END_STREAM_FLAG: u8 = 0x02;
pub const COMPRESSED_FLAG: u8 = 0x01;
pub const MAX_PAYLOAD_SIZE: u32 = 64 * 1024 * 1024;

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct ConnectError {
    #[serde(skip_serializing_if = "String::is_empty")]
    pub code: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub message: String,
}

impl ConnectError {
    pub fn new(code: impl Into<String>, message: impl Into<String>) -> Self {
        Self {
            code: code.into(),
            message: message.into(),
        }
    }
}

#[derive(Debug)]
pub enum FrameError {
    Io(io::Error),
    Compressed,
    PayloadTooLarge(u32),
}

impl fmt::Display for FrameError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Io(error) => write!(formatter, "read Connect frame: {error}"),
            Self::Compressed => write!(formatter, "compressed Connect frames are unsupported"),
            Self::PayloadTooLarge(size) => write!(
                formatter,
                "Connect frame payload too large: {size} bytes (maximum {MAX_PAYLOAD_SIZE})"
            ),
        }
    }
}

impl std::error::Error for FrameError {}

impl From<io::Error> for FrameError {
    fn from(error: io::Error) -> Self {
        Self::Io(error)
    }
}

#[derive(Serialize)]
struct EndStream<'a> {
    #[serde(skip_serializing_if = "Option::is_none")]
    error: Option<&'a ConnectError>,
}

pub fn encode_stream_message(payload: &[u8]) -> Vec<u8> {
    encode_frame(0, payload)
}

pub fn encode_end_stream(error: Option<ConnectError>) -> Vec<u8> {
    let payload = serde_json::to_vec(&EndStream {
        error: error.as_ref(),
    })
    .expect("Connect end-stream JSON serialization cannot fail");
    encode_frame(END_STREAM_FLAG, &payload)
}

pub fn decode_frame(frame: &[u8]) -> Result<(u8, Vec<u8>), FrameError> {
    if frame.len() < 5 {
        return Err(FrameError::Io(io::Error::new(
            io::ErrorKind::UnexpectedEof,
            "Connect frame header is incomplete",
        )));
    }

    let flags = frame[0];
    if flags & COMPRESSED_FLAG != 0 {
        return Err(FrameError::Compressed);
    }

    let payload_size = u32::from_be_bytes(frame[1..5].try_into().expect("header is five bytes"));
    if payload_size > MAX_PAYLOAD_SIZE {
        return Err(FrameError::PayloadTooLarge(payload_size));
    }

    let expected_size = 5 + payload_size as usize;
    if frame.len() != expected_size {
        return Err(FrameError::Io(io::Error::new(
            io::ErrorKind::InvalidData,
            format!(
                "Connect frame size mismatch: header says {payload_size} bytes, body has {}",
                frame.len().saturating_sub(5)
            ),
        )));
    }

    Ok((flags, frame[5..].to_vec()))
}

fn encode_frame(flags: u8, payload: &[u8]) -> Vec<u8> {
    assert!(
        payload.len() <= u32::MAX as usize,
        "Connect frame payload exceeds the u32 length field"
    );

    let mut frame = Vec::with_capacity(5 + payload.len());
    frame.push(flags);
    frame.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    frame.extend_from_slice(payload);
    frame
}

pub async fn read_frame<R: AsyncRead + Unpin>(reader: &mut R) -> Result<(u8, Vec<u8>), FrameError> {
    let mut header = [0u8; 5];
    reader.read_exact(&mut header).await?;

    let flags = header[0];
    if flags & COMPRESSED_FLAG != 0 {
        return Err(FrameError::Compressed);
    }

    let payload_size = u32::from_be_bytes(header[1..5].try_into().expect("header is five bytes"));
    if payload_size > MAX_PAYLOAD_SIZE {
        return Err(FrameError::PayloadTooLarge(payload_size));
    }

    let mut payload = vec![0u8; payload_size as usize];
    reader.read_exact(&mut payload).await?;
    Ok((flags, payload))
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{AsyncWriteExt, DuplexStream};

    async fn read_from_frame(frame: Vec<u8>) -> Result<(u8, Vec<u8>), FrameError> {
        let (mut writer, mut reader): (DuplexStream, DuplexStream) = tokio::io::duplex(frame.len());
        writer.write_all(&frame).await.expect("write frame");
        writer.shutdown().await.expect("close frame writer");
        read_frame(&mut reader).await
    }

    #[tokio::test]
    async fn stream_message_round_trips_empty_payload() {
        let (flags, payload) = read_from_frame(encode_stream_message(&[])).await.unwrap();
        assert_eq!(flags, 0);
        assert!(payload.is_empty());
    }

    #[tokio::test]
    async fn stream_message_round_trips_payload() {
        let message = br#"{"hello":"world"}"#;
        let (flags, payload) = read_from_frame(encode_stream_message(message))
            .await
            .unwrap();
        assert_eq!(flags, 0);
        assert_eq!(payload, message);
    }

    #[test]
    fn end_stream_encodes_empty_object_without_error() {
        let frame = encode_end_stream(None);
        assert_eq!(frame[0], END_STREAM_FLAG);
        assert_eq!(&frame[5..], b"{}");
    }

    #[tokio::test]
    async fn end_stream_encodes_error() {
        let frame = encode_end_stream(Some(ConnectError::new("internal", "failed")));
        let (flags, payload) = read_from_frame(frame).await.unwrap();
        assert_eq!(flags, END_STREAM_FLAG);
        assert_eq!(
            payload,
            br#"{"error":{"code":"internal","message":"failed"}}"#
        );
    }

    #[tokio::test]
    async fn compressed_frames_are_rejected() {
        let error = read_from_frame(vec![COMPRESSED_FLAG, 0, 0, 0, 0])
            .await
            .unwrap_err();
        assert!(matches!(error, FrameError::Compressed));
    }

    #[tokio::test]
    async fn oversized_frames_are_rejected() {
        let size = MAX_PAYLOAD_SIZE + 1;
        let mut frame = vec![0];
        frame.extend_from_slice(&size.to_be_bytes());
        let error = read_from_frame(frame).await.unwrap_err();
        assert!(matches!(error, FrameError::PayloadTooLarge(value) if value == size));
    }

    #[test]
    fn encode_and_decode_frame_round_trip() {
        let frame = encode_frame(0, b"payload");
        let (flags, payload) = decode_frame(&frame).unwrap();
        assert_eq!(flags, 0);
        assert_eq!(payload, b"payload");
    }

    #[test]
    fn decode_frame_rejects_truncated_header() {
        let error = decode_frame(&[0, 0, 0]).unwrap_err();
        assert!(matches!(
            error,
            FrameError::Io(ref error) if error.kind() == std::io::ErrorKind::UnexpectedEof
        ));
    }

    #[test]
    fn decode_frame_rejects_size_mismatch() {
        let mut frame = vec![0u8];
        frame.extend_from_slice(&5u32.to_be_bytes());
        frame.extend_from_slice(b"ab");
        let error = decode_frame(&frame).unwrap_err();
        assert!(matches!(
            error,
            FrameError::Io(ref error) if error.kind() == std::io::ErrorKind::InvalidData
        ));
    }

    #[test]
    fn decode_frame_rejects_compressed_flag() {
        let mut frame = vec![COMPRESSED_FLAG];
        frame.extend_from_slice(&0u32.to_be_bytes());
        assert!(matches!(
            decode_frame(&frame).unwrap_err(),
            FrameError::Compressed
        ));
    }

    #[test]
    fn decode_frame_rejects_oversized_payload() {
        let mut frame = vec![0u8];
        frame.extend_from_slice(&(MAX_PAYLOAD_SIZE + 1).to_be_bytes());
        assert!(matches!(
            decode_frame(&frame).unwrap_err(),
            FrameError::PayloadTooLarge(_)
        ));
    }

    #[tokio::test]
    async fn read_frame_rejects_truncated_payload() {
        let mut frame = vec![0u8];
        frame.extend_from_slice(&4u32.to_be_bytes());
        frame.extend_from_slice(b"ab");
        let error = read_from_frame(frame).await.unwrap_err();
        assert!(matches!(
            error,
            FrameError::Io(ref error) if error.kind() == std::io::ErrorKind::UnexpectedEof
        ));
    }
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Shared Connect stream framing, independent of service events and state.
//!
//! This module owns the wire shapes (`message_frame`, `terminal_frame`,
//! `end_stream_*`) and the response constructor (`frame_stream_response`).
//! Queueing and backpressure live with the producer — per-process output in
//! `process::bus`, filesystem watch in `filesystem::watch` — and callers retain
//! ownership of service lifetimes and terminal-event policy.

use bytes::Bytes;

use crate::protocol;
use crate::protocol::ConnectError;

pub fn frame_stream_response(
    frames: impl futures::Stream<Item = Bytes> + Send + 'static,
) -> axum::response::Response {
    use futures::StreamExt;
    let body = axum::body::Body::from_stream(frames.map(Ok::<_, std::convert::Infallible>));
    axum::response::Response::builder()
        .status(axum::http::StatusCode::OK)
        .header(
            axum::http::header::CONTENT_TYPE,
            protocol::STREAM_CONTENT_TYPE,
        )
        .body(body)
        .expect("build stream response")
}

/// A streaming response that carries a single EndStream error frame.
pub fn stream_error_response(err: ConnectError) -> axum::response::Response {
    frame_stream_response(futures::stream::iter([protocol::end_stream_error(&err)]))
}

/// Successful response for a client-streaming RPC: one empty response message
/// followed by the mandatory EndStream envelope.
pub fn empty_stream_response() -> axum::response::Response {
    let message = protocol::message_frame(&serde_json::json!({}));
    let trailer = protocol::end_stream_ok();
    frame_stream_response(futures::stream::iter([terminal_frame(message, trailer)]))
}

/// Wrap an event and its EndStream trailer in a single queue item, so a sender
/// that reserves one slot can always deliver both.
pub(crate) fn terminal_frame(message: Bytes, trailer: Bytes) -> Bytes {
    let mut frames = Vec::with_capacity(message.len() + trailer.len());
    frames.extend_from_slice(&message);
    frames.extend_from_slice(&trailer);
    Bytes::from(frames)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn response_helpers_preserve_http_and_wire_contracts() {
        let error = ConnectError::new(crate::protocol::ConnectCode::Internal, "service failed");
        let response = stream_error_response(error.clone());
        assert_eq!(response.status(), axum::http::StatusCode::OK);
        assert_eq!(
            response.headers()[axum::http::header::CONTENT_TYPE],
            protocol::STREAM_CONTENT_TYPE
        );
        let body = axum::body::to_bytes(response.into_body(), 1024)
            .await
            .unwrap();
        assert_eq!(body, protocol::end_stream_error(&error));
        let response = empty_stream_response();
        let body = axum::body::to_bytes(response.into_body(), 1024)
            .await
            .unwrap();
        let message = protocol::message_frame(&serde_json::json!({}));
        assert_eq!(&body[..message.len()], message.as_ref());
        assert_eq!(&body[message.len()..], protocol::end_stream_ok().as_ref());
    }
}

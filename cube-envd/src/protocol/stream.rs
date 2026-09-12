// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Shared Connect stream delivery, independent of service events and state.
//!
//! One response queue slot is reserved for a terminal item. Producers never
//! await response capacity, and dropping the HTTP body wakes an idle driver.
//! Callers retain ownership of service lifetimes and terminal-event policy.

use bytes::Bytes;
use tokio::sync::broadcast::error::RecvError;
use tokio::sync::{broadcast, mpsc};

use crate::protocol;
use crate::protocol::{ConnectCode, ConnectError};

pub(crate) const RESPONSE_QUEUE_CAPACITY: usize = 65;

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

/// Construct the bounded queue shared by Connect response producers.
/// Data delivery must use try_send_data_frame to preserve the terminal slot.
pub(crate) fn response_channel() -> (mpsc::Sender<Bytes>, mpsc::Receiver<Bytes>) {
    mpsc::channel(RESPONSE_QUEUE_CAPACITY)
}

/// Keep a final message and its EndStream trailer in a single queue item.
pub(crate) fn terminal_frame(message: Bytes, trailer: Bytes) -> Bytes {
    let mut frames = Vec::with_capacity(message.len() + trailer.len());
    frames.extend_from_slice(&message);
    frames.extend_from_slice(&trailer);
    Bytes::from(frames)
}

/// A transport event; service-specific payloads and error policy stay opaque.
pub(crate) enum Delivery<T> {
    Event(Result<T, RecvError>),
    Disconnected,
    Keepalive,
    Deadline,
}

/// Select the next broadcast, disconnect or timer event without waiting for
/// HTTP queue capacity. The caller may disable its attachment deadline while
/// awaiting an already-announced terminal event. Returning Disconnected lets
/// the caller immediately drop its subscription and other attachment resources.
pub(crate) async fn next_delivery<T: Clone>(
    events: &mut broadcast::Receiver<T>,
    output: &mpsc::Sender<Bytes>,
    keepalive: &mut tokio::time::Interval,
    deadline: impl std::future::Future<Output = ()>,
    deadline_enabled: bool,
) -> Delivery<T> {
    tokio::select! {
        _ = output.closed() => Delivery::Disconnected,
        event = events.recv() => Delivery::Event(event),
        _ = keepalive.tick() => Delivery::Keepalive,
        _ = deadline, if deadline_enabled => Delivery::Deadline,
    }
}

/// Never await HTTP response capacity from a stream driver. One queue slot
/// is reserved for the terminal error: when ordinary output reaches that
/// boundary, close only this connection with an explicit resource_exhausted
/// EndStream frame while independent service lifecycle work continues.
pub(crate) fn try_send_data_frame(output: &mut Option<mpsc::Sender<Bytes>>, frame: Bytes) -> bool {
    let Some(tx) = output.as_ref() else {
        return false;
    };
    if tx.capacity() <= 1 {
        try_send_terminal_frame(
            output,
            protocol::end_stream_error(&ConnectError::new(
                ConnectCode::ResourceExhausted,
                "output consumer too slow: response queue full",
            )),
        );
        return false;
    }

    if tx.try_send(frame).is_err() {
        output.take();
        return false;
    }
    true
}

/// Queue a final EndStream-bearing frame in the reserved slot and drop the
/// producer. A closed receiver needs no trailer because the HTTP client is
/// already gone.
pub(crate) fn try_send_terminal_frame(
    output: &mut Option<mpsc::Sender<Bytes>>,
    frame: Bytes,
) -> bool {
    let sent = output.as_ref().is_some_and(|tx| tx.try_send(frame).is_ok());
    output.take();
    sent
}

#[cfg(test)]
mod tests {
    use super::*;

    fn quiet_interval() -> tokio::time::Interval {
        let mut interval = tokio::time::interval(std::time::Duration::from_secs(60));
        interval.reset();
        interval
    }

    #[tokio::test]
    async fn broadcast_delivery_is_independent_of_process_events() {
        let (publisher, mut events) = broadcast::channel(1);
        let (output, _body) = response_channel();
        let mut interval = quiet_interval();
        publisher.send("created").unwrap();
        publisher.send("modified").unwrap();
        assert!(matches!(
            next_delivery(
                &mut events,
                &output,
                &mut interval,
                std::future::pending(),
                true
            )
            .await,
            Delivery::Event(Err(RecvError::Lagged(1)))
        ));
        assert!(matches!(
            next_delivery(
                &mut events,
                &output,
                &mut interval,
                std::future::pending(),
                true
            )
            .await,
            Delivery::Event(Ok("modified"))
        ));
        drop(publisher);
        assert!(matches!(
            next_delivery(
                &mut events,
                &output,
                &mut interval,
                std::future::pending(),
                true
            )
            .await,
            Delivery::Event(Err(RecvError::Closed))
        ));
    }

    #[tokio::test]
    async fn body_disconnect_wakes_idle_driver_and_releases_subscription() {
        let (publisher, mut events) = broadcast::channel::<&str>(1);
        let (output, body) = response_channel();
        let driver = tokio::spawn(async move {
            let mut interval = quiet_interval();
            assert!(matches!(
                next_delivery(
                    &mut events,
                    &output,
                    &mut interval,
                    std::future::pending(),
                    true
                )
                .await,
                Delivery::Disconnected
            ));
        });
        drop(body);
        tokio::time::timeout(std::time::Duration::from_secs(1), driver)
            .await
            .expect("idle attachment retained after body drop")
            .unwrap();
        assert_eq!(publisher.receiver_count(), 0);
    }

    #[tokio::test]
    async fn delivery_preserves_keepalive_and_deadline_gating() {
        let (publisher, mut events) = broadcast::channel(1);
        let (output, _body) = response_channel();
        // An interval's initial tick is ready without waiting on wall-clock time.
        let mut interval = tokio::time::interval(std::time::Duration::from_secs(60));
        assert!(matches!(
            next_delivery(
                &mut events,
                &output,
                &mut interval,
                std::future::pending(),
                true
            )
            .await,
            Delivery::Keepalive
        ));
        interval.reset();
        assert!(matches!(
            next_delivery(
                &mut events,
                &output,
                &mut interval,
                std::future::ready(()),
                true
            )
            .await,
            Delivery::Deadline
        ));
        publisher.send("terminal").unwrap();
        assert!(matches!(
            next_delivery(
                &mut events,
                &output,
                &mut interval,
                std::future::ready(()),
                false
            )
            .await,
            Delivery::Event(Ok("terminal"))
        ));
    }

    #[tokio::test]
    async fn full_response_queue_reserves_an_explicit_error_trailer() {
        let (sender, mut body) = response_channel();
        let mut output = Some(sender);
        let frame = protocol::message_frame(&serde_json::json!({"change": "created"}));
        for _ in 0..RESPONSE_QUEUE_CAPACITY - 1 {
            assert!(try_send_data_frame(&mut output, frame.clone()));
        }
        assert!(!try_send_data_frame(&mut output, frame.clone()));
        assert!(output.is_none());
        for _ in 0..RESPONSE_QUEUE_CAPACITY - 1 {
            assert_eq!(body.recv().await.unwrap(), frame);
        }
        let trailer = body.recv().await.unwrap();
        assert_eq!(trailer[0], crate::protocol::frames::END_STREAM_FLAG);
        let payload: serde_json::Value = serde_json::from_slice(&trailer[5..]).unwrap();
        assert_eq!(payload["error"]["code"], "resource_exhausted");
        assert_eq!(
            payload["error"]["message"],
            "output consumer too slow: response queue full"
        );
        assert!(body.recv().await.is_none());
    }

    #[tokio::test]
    async fn final_message_and_trailer_fit_one_remaining_slot() {
        for error in [
            None,
            Some(ConnectError::new(
                ConnectCode::DeadlineExceeded,
                "context deadline exceeded",
            )),
        ] {
            let (sender, mut body) = mpsc::channel(2);
            let mut output = Some(sender);
            let first = protocol::message_frame(&serde_json::json!({"change": "created"}));
            assert!(try_send_data_frame(&mut output, first.clone()));
            let last = protocol::message_frame(&serde_json::json!({"complete": true}));
            let trailer = error
                .as_ref()
                .map(protocol::end_stream_error)
                .unwrap_or_else(protocol::end_stream_ok);
            assert!(try_send_terminal_frame(
                &mut output,
                terminal_frame(last.clone(), trailer.clone())
            ));
            assert_eq!(body.recv().await.unwrap(), first);
            let terminal = body.recv().await.unwrap();
            assert_eq!(&terminal[..last.len()], last.as_ref());
            assert_eq!(&terminal[last.len()..], trailer.as_ref());
            assert!(output.is_none());
            assert!(body.recv().await.is_none());
        }
    }

    #[tokio::test]
    async fn response_helpers_preserve_http_and_wire_contracts() {
        let error = ConnectError::new(ConnectCode::Internal, "service failed");
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

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Process event encoding and attachment termination policy.

use bytes::Bytes;
use tokio::sync::broadcast::error::RecvError;
use tokio::sync::{broadcast, mpsc};

use crate::process::engine;
use crate::process::wire::{Event, EventEnvelope, StartEvent};
use crate::protocol;
use crate::protocol::stream::{
    next_delivery, terminal_frame, try_send_data_frame, try_send_terminal_frame, Delivery,
};
use crate::protocol::{ConnectCode, ConnectError};

pub use crate::protocol::stream::{
    empty_stream_response, frame_stream_response, stream_error_response,
};

fn event_frame(event: Event) -> Bytes {
    let value =
        serde_json::to_value(EventEnvelope { event }).unwrap_or_else(|_| serde_json::json!({}));
    protocol::message_frame(&value)
}

/// Deliver process events without owning the supervised process lifetime.
pub(crate) async fn drive_stream(
    pid: u32,
    mut events: broadcast::Receiver<engine::PumpEvent>,
    tx: mpsc::Sender<Bytes>,
    keepalive_interval: std::time::Duration,
    stream_deadline: Option<std::time::Duration>,
) {
    // The producer is dropped immediately on backpressure or disconnect.
    // Process lifetime is owned by the separate supervisor, so this task
    // never needs to retain a dead HTTP client's broadcast subscription.
    let mut output = Some(tx);
    let mut deadline_seen = false;
    if !try_send_data_frame(&mut output, event_frame(Event::Start(StartEvent { pid }))) {
        return;
    }

    let mut keepalive = tokio::time::interval(keepalive_interval);
    keepalive.reset(); // first tick fires after one period, not immediately
    let deadline = async move {
        match stream_deadline {
            Some(deadline) => tokio::time::sleep(deadline).await,
            None => std::future::pending::<()>().await,
        }
    };
    tokio::pin!(deadline);

    loop {
        let close_signal = output.as_ref().cloned().expect("output sender is live");
        match next_delivery(
            &mut events,
            &close_signal,
            &mut keepalive,
            &mut deadline,
            !deadline_seen,
        )
        .await
        {
            Delivery::Disconnected => return,
            Delivery::Event(ev) => match ev {
                Ok(engine::PumpEvent::Data(d)) => {
                    keepalive.reset();
                    if !try_send_data_frame(&mut output, event_frame(Event::Data(d))) {
                        return;
                    }
                }
                Ok(engine::PumpEvent::End(end)) => {
                    // One queue slot carries both terminal envelopes. This
                    // prevents a nearly-full queue from exposing End without
                    // the required EndStream trailer.
                    let event = event_frame(Event::End(end));
                    let trailer = if deadline_seen {
                        protocol::end_stream_error(&ConnectError::new(
                            ConnectCode::DeadlineExceeded,
                            "context deadline exceeded",
                        ))
                    } else {
                        protocol::end_stream_ok()
                    };
                    try_send_terminal_frame(&mut output, terminal_frame(event, trailer));
                    return;
                }
                Ok(engine::PumpEvent::SpawnError(msg)) => {
                    try_send_terminal_frame(
                        &mut output,
                        protocol::end_stream_error(&ConnectError::new(ConnectCode::Internal, msg)),
                    );
                    return;
                }
                Ok(engine::PumpEvent::DeadlineExceeded) => {
                    // The supervisor has recorded the timeout and started
                    // killing the process. Keep this attachment alive until
                    // the pump publishes the real EndEvent, so clients get
                    // both the actual signal and `killedBy: "timeout"`.
                    deadline_seen = true;
                }
                Err(RecvError::Lagged(n)) => {
                    try_send_terminal_frame(
                        &mut output,
                        protocol::end_stream_error(&ConnectError::new(
                            ConnectCode::ResourceExhausted,
                            format!("output consumer too slow: {n} events dropped"),
                        )),
                    );
                    return;
                }
                Err(RecvError::Closed) => {
                    let (code, message) = if deadline_seen {
                        (ConnectCode::DeadlineExceeded, "context deadline exceeded")
                    } else {
                        (
                            ConnectCode::Internal,
                            "process output stream closed before a terminal event",
                        )
                    };
                    try_send_terminal_frame(
                        &mut output,
                        protocol::end_stream_error(&ConnectError::new(code, message)),
                    );
                    return;
                }
            },
            Delivery::Keepalive => {
                if !try_send_data_frame(
                    &mut output,
                    event_frame(Event::KeepAlive(serde_json::Map::new())),
                ) {
                    return;
                }
            }
            Delivery::Deadline => {
                // A Connect timeout bounds this attachment only. The process
                // belongs to its Start supervisor and must remain available
                // for List, input and a later Connect.
                try_send_terminal_frame(
                    &mut output,
                    protocol::end_stream_error(&ConnectError::new(
                        ConnectCode::DeadlineExceeded,
                        "context deadline exceeded",
                    )),
                );
                return;
            }
        }
    }
}

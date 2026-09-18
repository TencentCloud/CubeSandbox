// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Process event encoding and attachment termination policy.

use bytes::Bytes;

use crate::process::bus::{BusError, TerminalChannel};
use crate::process::engine;
use crate::process::wire::{EndEvent, Event, EventEnvelope, StartEvent};
use crate::protocol;
use crate::protocol::stream::terminal_frame;
use crate::protocol::{ConnectCode, ConnectError};

pub use crate::protocol::stream::{
    empty_stream_response, frame_stream_response, stream_error_response,
};

fn event_frame(event: Event) -> Bytes {
    protocol::json_message_frame(&EventEnvelope { event })
}

/// Queue a terminal frame in the connection's reserved slot. The reservation is
/// what makes this possible while every data slot is full, so it never waits.
fn emit_terminal_frame(body: &TerminalChannel<Bytes>, message: Bytes, trailer: Bytes) {
    if !body.send_terminal(terminal_frame(message, trailer)) {
        tracing::warn!("process stream: terminal frame already sent, dropping the duplicate");
    }
}

/// End the stream with an error and no `End` event.
fn emit_error(body: &TerminalChannel<Bytes>, code: ConnectCode, message: impl Into<String>) {
    if !body.send_terminal(protocol::end_stream_error(&ConnectError::new(
        code, message,
    ))) {
        tracing::warn!("process stream: terminal frame already sent, dropping the duplicate");
    }
}

/// The EndStream trailer for a process that finished.
///
/// A process killed by its deadline is recognised **out-of-band**, from the
/// decoration the supervisor already applied (`killed_by = "timeout"`), so the
/// trailer never depends on the best-effort `DeadlineExceeded` hint arriving.
fn end_trailer(end: &EndEvent) -> Bytes {
    if end.killed_by.as_deref() == Some("timeout") {
        protocol::end_stream_error(&ConnectError::new(
            ConnectCode::DeadlineExceeded,
            "context deadline exceeded",
        ))
    } else if end.output_truncated {
        // The drain grace stopped with bytes still buffered: the exit code is
        // real, but the output before this `End` is not complete, and reporting
        // that as a clean end is exactly the silent truncation this stream must
        // never produce.
        protocol::end_stream_error(&ConnectError::new(
            ConnectCode::ResourceExhausted,
            "output truncated: the drain grace expired with data still buffered",
        ))
    } else {
        protocol::end_stream_ok()
    }
}

/// Deliver process events for one attachment.
///
/// Everything a connection needs — the `Start` marker, data, keepalives and the
/// terminal frame with its trailer — is produced here, so the shared bus stays
/// unaware of per-connection framing. `body` holds one slot reserved for the
/// terminal frame, which guarantees an `End` even when the client is behind;
/// waiting for a data slot is what backpressures the child.
pub(crate) async fn drive_stream(
    pid: u32,
    mut events: crate::process::Subscription,
    body: TerminalChannel<Bytes>,
    keepalive_interval: std::time::Duration,
    stream_deadline: Option<std::time::Duration>,
) {
    let mut evicted = events.eviction();
    let mut keepalive = tokio::time::interval(keepalive_interval);
    keepalive.reset(); // first tick fires after one period, not immediately
    let deadline = async move {
        match stream_deadline {
            Some(deadline) => tokio::time::sleep(deadline).await,
            None => std::future::pending::<()>().await,
        }
    };
    tokio::pin!(deadline);

    let mut pending: Option<Bytes> = Some(event_frame(Event::Start(StartEvent { pid })));
    let mut deadline_seen = false;
    // The eviction latch closes when the bus goes away. That is not an
    // eviction: stop watching it and let `events.recv()` drain whatever is
    // already queued before it reports the close.
    let mut eviction_latch_closed = false;

    loop {
        let deadline_enabled = stream_deadline.is_some() && !deadline_seen;
        tokio::select! {
            biased;
            // The client is gone: nothing can be delivered, and dropping the
            // connection releases its reserved slot.
            _ = body.closed() => return,
            // A Connect deadline bounds *this attachment only*. The process
            // belongs to its Start supervisor and stays available for List,
            // input and a later Connect.
            _ = &mut deadline, if deadline_enabled => {
                emit_error(&body, ConnectCode::DeadlineExceeded, "context deadline exceeded");
                return;
            }
            // The subscriber made no progress for the eviction window. Report
            // it instead of letting it pin the pump — this arm is also what
            // abandons a blocked `reserve_data` below.
            latch = evicted.changed(), if !eviction_latch_closed => {
                if latch.is_ok() && *evicted.borrow() {
                    emit_error(
                        &body,
                        ConnectCode::ResourceExhausted,
                        "output consumer too slow: no progress",
                    );
                    return;
                }
                eviction_latch_closed = true;
                continue;
            }
            // Flattened on purpose: this arm only *reserves*, so the frame stays
            // in `pending` while it waits. A helper that moved the frame into
            // itself would lose it whenever another arm wins the select.
            permit = body.reserve_data(), if pending.is_some() => match permit {
                Ok(permit) => {
                    permit.send(pending.take().expect("arm gated on pending.is_some()"));
                }
                Err(_) => return,
            },
            event = events.recv(), if pending.is_none() => match event {
                Ok(engine::PumpEvent::Data(data)) => {
                    keepalive.reset();
                    pending = Some(event_frame(Event::Data(data)));
                }
                Ok(engine::PumpEvent::End(end)) => {
                    let trailer = end_trailer(&end);
                    emit_terminal_frame(&body, event_frame(Event::End(end)), trailer);
                    return;
                }
                Ok(engine::PumpEvent::SpawnError(message)) => {
                    emit_error(&body, ConnectCode::Internal, message);
                    return;
                }
                Ok(engine::PumpEvent::DeadlineExceeded) => {
                    // The supervisor recorded the timeout and started killing
                    // the process. Keep this attachment alive until the pump
                    // publishes the real EndEvent, so the client receives both
                    // the actual signal and `killedBy: "timeout"`.
                    deadline_seen = true;
                }
                Err(BusError::Evicted) => {
                    emit_error(&body, ConnectCode::ResourceExhausted, "output consumer too slow: no progress");
                    return;
                }
                // The bus went away without a terminal event. The process
                // table's cache usually still holds the real exit.
                Err(_) => {
                    match events.terminal_event() {
                        Some(engine::PumpEvent::End(end)) => {
                            let trailer = end_trailer(&end);
                            emit_terminal_frame(&body, event_frame(Event::End(end)), trailer);
                        }
                        Some(engine::PumpEvent::SpawnError(message)) => {
                            emit_error(&body, ConnectCode::Internal, message);
                        }
                        _ => emit_error(
                            &body,
                            ConnectCode::Internal,
                            "process output stream closed before a terminal event",
                        ),
                    }
                    return;
                }
            },
            // Only while idle: `biased` keeps queued data ahead of this arm, so
            // a keepalive never jumps in front of output.
            _ = keepalive.tick(), if pending.is_none() => {
                pending = Some(event_frame(Event::KeepAlive(serde_json::Map::new())));
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn end_event() -> EndEvent {
        EndEvent {
            exit_code: 0,
            exited: true,
            status: "exit status 0".into(),
            error: None,
            signal: None,
            oom_killed: None,
            killed_by: None,
            output_truncated: false,
        }
    }

    /// Abandoned output must not be reported as a clean end; a complete drain
    /// still must be.
    #[test]
    fn abandoned_output_is_never_reported_as_a_clean_end() {
        let trailer = end_trailer(&end_event());
        assert_eq!(trailer, protocol::end_stream_ok());

        let mut truncated = end_event();
        truncated.output_truncated = true;
        let trailer = end_trailer(&truncated);
        assert_ne!(trailer, protocol::end_stream_ok());
        let text = String::from_utf8_lossy(&trailer);
        assert!(text.contains("resource_exhausted"), "{text}");
        assert!(text.contains("truncated"), "{text}");
    }

    /// The deadline trailer keeps priority: a timed-out stream says so.
    #[test]
    fn a_timeout_still_reports_deadline_exceeded() {
        let mut end = end_event();
        end.killed_by = Some("timeout".into());
        end.output_truncated = true;
        let trailer = end_trailer(&end);
        let text = String::from_utf8_lossy(&trailer);
        assert!(text.contains("deadline_exceeded"), "{text}");
    }
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! How the stream lives and dies: the deadline / keepalive / disconnect
//! four-way select.
//!
//! Carries over the streaming pump of `services/watch.rs`. Contract: upstream
//! `watch.go:66-90`.

use std::time::Duration;

use tokio::io::unix::AsyncFd;

use crate::filesystem::wire::{StartEvent, WatchDirResponse};
use crate::protocol;
use crate::protocol::{ConnectCode, ConnectError};

use super::inotify::Inotify;
use super::tree::{drain_events, WatchState};

fn frame_of(resp: &WatchDirResponse) -> bytes::Bytes {
    match serde_json::to_value(resp) {
        Ok(v) => protocol::message_frame(&v),
        Err(e) => protocol::end_stream_error(&ConnectError::new(
            ConnectCode::Internal,
            format!("serialize response: {e}"),
        )),
    }
}

fn fail_stream(tx: &tokio::sync::mpsc::Sender<bytes::Bytes>, e: &ConnectError) {
    let _ = tx.try_send(protocol::end_stream_error(e));
}

pub(super) fn internal(msg: impl Into<String>) -> ConnectError {
    ConnectError::new(ConnectCode::Internal, msg)
}

pub(super) async fn run_stream(
    ino: Inotify,
    mut st: WatchState,
    keepalive: Duration,
    deadline: Option<Duration>,
    tx: tokio::sync::mpsc::Sender<bytes::Bytes>,
) {
    // Start frame first (watch.go:66-73). A failed send means the client is
    // already gone.
    if tx
        .send(frame_of(&WatchDirResponse::Start(StartEvent {})))
        .await
        .is_err()
    {
        return;
    }
    let afd = match AsyncFd::new(ino) {
        Ok(a) => a,
        Err(e) => {
            fail_stream(&tx, &internal(format!("watcher error: {e}")));
            return;
        }
    };
    // First tick after the interval, like time.NewTicker (not immediately).
    let mut interval = tokio::time::interval_at(tokio::time::Instant::now() + keepalive, keepalive);
    // Connect-Timeout-Ms bounds the whole stream: on expiry upstream returns
    // ctx.Err() (`watch.go:89-90`), which connect renders as
    // deadline_exceeded "context deadline exceeded" — an EndStream error
    // frame that also releases the inotify fd.
    let has_deadline = deadline.is_some();
    let inner: futures::future::OptionFuture<tokio::time::Sleep> =
        deadline.map(tokio::time::sleep).into();
    let mut deadline_fut = Box::pin(inner);
    loop {
        tokio::select! {
            // Client disconnect → immediate teardown; dropping `afd` closes
            // the inotify fd (the `defer w.Close()` + ctx.Done() pair).
            _ = tx.closed() => break,
            _ = deadline_fut.as_mut(), if has_deadline => {
                fail_stream(
                    &tx,
                    &ConnectError::new(
                        ConnectCode::DeadlineExceeded,
                        "context deadline exceeded",
                    ),
                );
                break;
            }
            _ = interval.tick() => {
                if tx
                    .send(frame_of(&WatchDirResponse::KeepAlive(Default::default())))
                    .await
                    .is_err()
                {
                    break;
                }
            }
            ready = afd.readable() => {
                let mut guard = match ready {
                    Ok(g) => g,
                    Err(e) => {
                        fail_stream(&tx, &internal(format!("watcher error: {e}")));
                        break;
                    }
                };
                guard.clear_ready();
                match drain_events(afd.get_ref(), &mut st) {
                    Ok(events) => {
                        for ev in events {
                            if tx
                                .send(frame_of(&WatchDirResponse::Filesystem(ev)))
                                .await
                                .is_err()
                            {
                                return;
                            }
                            // upstream resets the keepalive after every op
                            // (watch.go:155)
                            interval.reset();
                        }
                    }
                    Err(e) => {
                        fail_stream(&tx, &e);
                        break;
                    }
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // ---- streaming teardown ----

    #[tokio::test]
    async fn stream_ends_when_client_disconnects() {
        let dir = tempfile::tempdir().unwrap();
        let (ino, st) = WatchState::new(dir.path().to_path_buf(), false).unwrap();
        let (tx, mut rx) = tokio::sync::mpsc::channel::<bytes::Bytes>(8);
        let jh = tokio::spawn(run_stream(
            ino,
            st,
            Duration::from_millis(50),
            None,
            tx.clone(),
        ));
        // First frame must be the Start event.
        let first = rx.recv().await.unwrap();
        let text = std::str::from_utf8(&first[5..]).unwrap(); // skip 1+4 header
        assert!(text.contains("\"start\""), "{text}");
        // Client hangs up → the pump must exit promptly (fd closed by Drop).
        drop(rx);
        let _ = tx.closed().await;
        tokio::time::timeout(Duration::from_secs(2), jh)
            .await
            .expect("pump did not exit after disconnect")
            .unwrap();
    }
    // ---- keepalive reset: each event pushes the next ping a full period ----

    #[tokio::test]
    async fn keepalive_resets_after_every_event() {
        use std::time::Instant;
        let dir = tempfile::tempdir().unwrap();
        // Pre-create the file so the single in-sandbox write yields exactly
        // one IN_MODIFY (a fresh create would emit CREATE + MODIFY).
        std::fs::write(dir.path().join("seed"), b"").unwrap();
        let (ino, st) = WatchState::new(dir.path().to_path_buf(), false).unwrap();
        let (tx, mut rx) = tokio::sync::mpsc::channel::<bytes::Bytes>(64);
        let period = Duration::from_millis(400);
        let jh = tokio::spawn(run_stream(ino, st, period, None, tx));

        // Start frame; record the mutation moment.
        let _start = rx.recv().await.unwrap();
        tokio::time::sleep(Duration::from_millis(200)).await;
        let t_event = Instant::now();
        std::fs::write(dir.path().join("seed"), b"x").unwrap();

        // Collect every frame for 900ms after the event, recording keepalive
        // arrival offsets. The event resets the ticker, so the next ping must
        // land a full period (400ms) after the event — not at the stale
        // schedule (200ms after).
        let mut ka_offsets_ms: Vec<u128> = Vec::new();
        let mut got_event = false;
        loop {
            let remaining = Duration::from_millis(900).saturating_sub(t_event.elapsed());
            if remaining.is_zero() {
                break;
            }
            match tokio::time::timeout(remaining, rx.recv()).await {
                Ok(Some(frame)) => {
                    let payload = std::str::from_utf8(&frame[5..]).unwrap_or("");
                    let elapsed = t_event.elapsed().as_millis();
                    if payload.contains("\"filesystem\"") {
                        got_event = true;
                    } else if payload.contains("\"keepalive\"") {
                        ka_offsets_ms.push(elapsed);
                    }
                }
                _ => break,
            }
        }
        assert!(got_event, "filesystem event frame never arrived");
        jh.abort();

        // Without a reset the ping would fire 200ms after the event (the
        // original schedule); with it, a full period later (400ms). The gap
        // between the two hypotheses is 200ms — wide enough for CI.
        assert!(
            ka_offsets_ms.first().is_none_or(|ms| *ms >= 300),
            "keepalive arrived {}ms after the event — reset missing",
            ka_offsets_ms.first().unwrap_or(&0)
        );
        assert!(
            ka_offsets_ms.first().is_some_and(|ms| *ms <= 700),
            "no keepalive within 700ms after the event — ticker died: {ka_offsets_ms:?}"
        );
    }
    // ---- PR #16 review P2: Connect-Timeout-Ms bounds the stream ----

    #[tokio::test]
    async fn connect_timeout_ends_the_watch_stream() {
        let dir = tempfile::tempdir().unwrap();
        let (ino, st) = WatchState::new(dir.path().to_path_buf(), false).unwrap();
        let (tx, mut rx) = tokio::sync::mpsc::channel::<bytes::Bytes>(8);
        let jh = tokio::spawn(run_stream(
            ino,
            st,
            Duration::from_secs(30),
            Some(Duration::from_millis(150)),
            tx,
        ));
        let _start = rx.recv().await.unwrap();
        // On expiry upstream returns ctx.Err(): deadline_exceeded
        // "context deadline exceeded" as an EndStream error frame.
        let frame = tokio::time::timeout(Duration::from_secs(1), rx.recv())
            .await
            .expect("no deadline frame within 1s")
            .unwrap();
        let payload = std::str::from_utf8(&frame[5..]).unwrap_or("");
        assert!(payload.contains("deadline_exceeded"), "{payload}");
        assert!(payload.contains("context deadline exceeded"), "{payload}");
        assert_ne!(frame[0] & 0x02, 0, "EndStream flag must be set");
        // The stream is over.
        assert!(rx.recv().await.is_none());
        jh.await.unwrap();
    }
}

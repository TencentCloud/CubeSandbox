// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use std::sync::{Arc, Mutex};
use tokio::io::AsyncReadExt;
use tokio::sync::{mpsc, oneshot};

use crate::error::DomainError;
use crate::proto::process::{process_event, ProcessEvent};

// The upstream multiplexer buffers 64 source events, then waits for each
// subscriber. Slow consumers apply backpressure without losing their stream.
const CHUNK: usize = 32 * 1024;
const QUEUE: usize = 64;
type Terminal = Result<process_event::EndEvent, DomainError>;

pub(crate) enum Readers {
    Pipes(
        crate::process::linux::ProcessFd<tokio::net::unix::pipe::Receiver>,
        crate::process::linux::ProcessFd<tokio::net::unix::pipe::Receiver>,
    ),
    Pty(Arc<crate::process::linux::Pty>),
}

pub(crate) struct Output {
    subscribers: Mutex<Vec<Subscriber>>,
    cancelled: tokio::sync::watch::Sender<bool>,
}
impl Default for Output {
    fn default() -> Self {
        Self {
            subscribers: Mutex::new(Vec::new()),
            cancelled: tokio::sync::watch::channel(false).0,
        }
    }
}
struct Subscriber {
    data: mpsc::Sender<ProcessEvent>,
    terminal: oneshot::Sender<Terminal>,
}
#[derive(Debug)]
pub struct Subscription {
    pub pid: u32,
    data: mpsc::Receiver<ProcessEvent>,
    terminal: oneshot::Receiver<Terminal>,
}
impl Output {
    pub fn subscribe(&self, pid: u32) -> Result<Subscription, DomainError> {
        let mut subscribers = self.subscribers.lock().map_err(|_| DomainError::Internal)?;
        subscribers.retain(|subscriber| !subscriber.data.is_closed());
        let (send, data) = mpsc::channel(1);
        let (terminal, receive) = oneshot::channel();
        subscribers.push(Subscriber {
            data: send,
            terminal,
        });
        Ok(Subscription {
            pid,
            data,
            terminal: receive,
        })
    }
    async fn send(&self, event: ProcessEvent) {
        let subscribers: Vec<_> = {
            let mut subscribers = self.subscribers.lock().expect("output lock");
            subscribers.retain(|subscriber| !subscriber.data.is_closed());
            subscribers
                .iter()
                .map(|subscriber| subscriber.data.clone())
                .collect()
        };
        for subscriber in subscribers {
            let _ = subscriber.send(event.clone()).await;
        }
    }
    pub fn cancel(&self) {
        self.cancelled.send_replace(true);
    }
    pub fn finish(&self, terminal: Terminal) {
        for subscriber in self.subscribers.lock().expect("output lock").drain(..) {
            let _ = subscriber.terminal.send(terminal.clone());
        }
    }
    pub async fn read(self: &Arc<Self>, readers: Readers) -> Result<(), DomainError> {
        let (source, mut events) = mpsc::channel(QUEUE);
        let forwarding = async {
            while let Some(event) = events.recv().await {
                self.send(event).await;
            }
        };
        let (result, ()) = tokio::join!(self.read_inner(readers, source), forwarding);
        result
    }
    pub async fn cancelled(&self) {
        let mut cancelled = self.cancelled.subscribe();
        let _ = cancelled.wait_for(|cancelled| *cancelled).await;
    }
    async fn read_inner(
        &self,
        readers: Readers,
        source: mpsc::Sender<ProcessEvent>,
    ) -> Result<(), DomainError> {
        match readers {
            Readers::Pipes(stdout, stderr) => self.read_pipes(stdout, stderr, source).await,
            Readers::Pty(pty) => {
                let mut buffer = [0; CHUNK];
                loop {
                    let count = pty
                        .read(&mut buffer)
                        .await
                        .map_err(|_| DomainError::Internal)?;
                    if count == 0 {
                        return Ok(());
                    }
                    source
                        .send(ProcessEvent {
                            event: Some(process_event::Event::Data(Box::new(
                                process_event::DataEvent {
                                    output: Some(process_event::data_event::Output::Pty(
                                        buffer[..count].to_vec(),
                                    )),
                                    ..Default::default()
                                },
                            ))),
                            ..Default::default()
                        })
                        .await
                        .map_err(|_| DomainError::Internal)?;
                }
            }
        }
    }
    async fn read_pipes(
        &self,
        mut stdout: crate::process::linux::ProcessFd<tokio::net::unix::pipe::Receiver>,
        mut stderr: crate::process::linux::ProcessFd<tokio::net::unix::pipe::Receiver>,
        source: mpsc::Sender<ProcessEvent>,
    ) -> Result<(), DomainError> {
        let (mut out, mut err) = ([0; CHUNK], [0; CHUNK]);
        let (mut out_open, mut err_open) = (true, true);
        while out_open || err_open {
            let (result, buffer, is_stdout) = tokio::select! {
                result = stdout.read(&mut out), if out_open => (result, &out, true),
                result = stderr.read(&mut err), if err_open => (result, &err, false),
            };
            let count = result.map_err(|_| DomainError::Internal)?;
            if count == 0 {
                if is_stdout {
                    out_open = false;
                } else {
                    err_open = false;
                }
                continue;
            }
            let bytes = buffer[..count].to_vec();
            let output = if is_stdout {
                process_event::data_event::Output::Stdout(bytes)
            } else {
                process_event::data_event::Output::Stderr(bytes)
            };
            source
                .send(ProcessEvent {
                    event: Some(process_event::Event::Data(Box::new(
                        process_event::DataEvent {
                            output: Some(output),
                            ..Default::default()
                        },
                    ))),
                    ..Default::default()
                })
                .await
                .map_err(|_| DomainError::Internal)?;
        }
        Ok(())
    }
}
impl Subscription {
    pub fn stream(self, interval: std::time::Duration) -> connectrpc::ServiceStream<ProcessEvent> {
        let first = ProcessEvent {
            event: Some(process_event::Event::Start(Box::new(
                process_event::StartEvent {
                    pid: self.pid,
                    ..Default::default()
                },
            ))),
            ..Default::default()
        };
        use futures::StreamExt;
        let events = futures::stream::unfold(
            Some((self, tokio::time::Instant::now() + interval)),
            move |state| async move {
                let (mut subscription, deadline) = state?;
                let event = tokio::select! {
                    biased;
                    event = subscription.data.recv() => event,
                    _ = tokio::time::sleep_until(deadline) => Some(ProcessEvent { event: Some(process_event::Event::Keepalive(Box::default())), ..Default::default() }),
                };
                if let Some(event) = event {
                    return Some((
                        Ok(event),
                        Some((subscription, tokio::time::Instant::now() + interval)),
                    ));
                }
                let terminal = subscription
                    .terminal
                    .await
                    .unwrap_or(Err(DomainError::Internal));
                let event = terminal
                    .map(|end| ProcessEvent {
                        event: Some(process_event::Event::End(Box::new(end))),
                        ..Default::default()
                    })
                    .map_err(|error| {
                        connectrpc::ConnectError::new(error.connect_code(), error.public_message())
                    });
                Some((event, None))
            },
        );
        Box::pin(futures::stream::once(async { Ok(first) }).chain(events))
    }
}

/// Header values are seconds; bound multiplication to a signed nanosecond
/// duration, as used by the oracle, before constructing a timer.
pub(crate) fn keepalive(headers: &http::HeaderMap) -> Result<std::time::Duration, DomainError> {
    let text = headers
        .get("keepalive-ping-interval")
        .and_then(|value| value.to_str().ok())
        .unwrap_or("");
    let digits = text.strip_prefix(['+', '-']).unwrap_or(text);
    if digits.is_empty() || !digits.bytes().all(|byte| byte.is_ascii_digit()) {
        return Ok(std::time::Duration::from_secs(90));
    }
    let seconds = text
        .parse::<i64>()
        .ok()
        .filter(|seconds| *seconds > 0)
        .and_then(|seconds| seconds.checked_mul(1_000_000_000))
        .ok_or_else(|| DomainError::InvalidArgument("invalid keepalive interval".into()))?;
    Ok(std::time::Duration::from_nanos(seconds as u64))
}

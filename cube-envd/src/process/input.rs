// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use crate::error::DomainError;
use std::sync::{Arc, Mutex};
use tokio::io::AsyncWriteExt;
use tokio::sync::{mpsc, oneshot};

// Like upstream's serialized pipe writes, pending requests wait for the writer
// without a daemon-defined payload-size or queue-capacity rejection.
type Reply = oneshot::Sender<Result<(), DomainError>>;
type Pending = (Option<Vec<u8>>, Reply);
pub(crate) enum Target {
    Pipe(crate::process::linux::ProcessFd<tokio::net::unix::pipe::Sender>),
    Pty(std::sync::Arc<crate::process::linux::Pty>),
}
impl Target {
    async fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        match self {
            Self::Pipe(pipe) => pipe.write(bytes).await,
            Self::Pty(pty) => pty.write(bytes).await,
        }
    }
}
#[derive(Clone)]
pub(crate) struct Input {
    queue: mpsc::UnboundedSender<Pending>,
    state: Arc<Mutex<InputState>>,
}
pub(crate) struct InputTarget {
    pub input: Input,
    pub pty: bool,
}
impl InputTarget {
    pub async fn send(
        &self,
        input: Option<crate::proto::process::ProcessInput>,
    ) -> Result<(), DomainError> {
        use crate::proto::process::process_input::Input;
        let bytes = match input.and_then(|input| input.input) {
            Some(Input::Stdin(bytes)) if !self.pty => bytes,
            Some(Input::Pty(bytes)) if self.pty => bytes,
            Some(Input::Stdin(_)) => return Err(DomainError::FailedPrecondition("error writing to stdin: tty assigned to process — input should be written to the pty, not the stdin".into())),
            Some(Input::Pty(_)) => return Err(DomainError::FailedPrecondition("error writing to tty: tty not assigned to process — input should be written to the stdin, not the tty".into())),
            None => return Err(DomainError::Unimplemented("invalid input type <nil>".into())),
        };
        self.input
            .accept(Some(bytes))
            .await
            .unwrap_or_else(|_| Err(self.input.closed_write()))
    }
}
pub(crate) struct Writer {
    pid: u32,
    queue: mpsc::UnboundedReceiver<Pending>,
    target: Option<Target>,
    state: Arc<Mutex<InputState>>,
    active: Option<Reply>,
}
impl Input {
    pub fn new(target: Option<Target>, pid: u32) -> (Self, Writer) {
        let (send, queue) = mpsc::unbounded_channel();
        let state = Arc::new(Mutex::new(InputState {
            enabled: target.is_some(),
            pid,
            pty: matches!(target, Some(Target::Pty(_))),
        }));
        (
            Self {
                queue: send,
                state: state.clone(),
            },
            Writer {
                pid,
                queue,
                target,
                state,
                active: None,
            },
        )
    }
    pub fn accept(&self, bytes: Option<Vec<u8>>) -> oneshot::Receiver<Result<(), DomainError>> {
        let (reply, receive) = oneshot::channel();
        let mut state = self.state.lock().expect("input state");
        if let Err(error) = self.queue.send((bytes, reply)) {
            let (bytes, reply) = error.0;
            let result = state.after_exit(bytes.is_some());
            let _ = reply.send(result);
        }
        receive
    }
    pub async fn close(&self) -> Result<(), DomainError> {
        self.accept(None)
            .await
            .unwrap_or(Err(DomainError::Internal))
    }
    fn closed_write(&self) -> DomainError {
        self.state.lock().expect("input state").write_error()
    }
}
struct InputState {
    enabled: bool,
    pid: u32,
    pty: bool,
}
impl InputState {
    fn write_error(&self) -> DomainError {
        if !self.enabled {
            return DomainError::FailedPrecondition(
                "error writing to stdin: stdin not enabled or closed".into(),
            );
        }
        let (kind, file) = if self.pty {
            ("tty", "/dev/ptmx")
        } else {
            ("stdin", "|1")
        };
        DomainError::InternalMessage(format!("error writing to {kind}: error writing to {kind} of process '{}': write {file}: file already closed", self.pid))
    }
    fn after_exit(&mut self, writing: bool) -> Result<(), DomainError> {
        if writing {
            return Err(self.write_error());
        }
        if std::mem::replace(&mut self.enabled, false) {
            Err(DomainError::UnknownMessage(
                "error closing stdin: close |1: file already closed".into(),
            ))
        } else {
            Ok(())
        }
    }
}
impl Drop for Writer {
    fn drop(&mut self) {
        // Complete accepted operations in writer order, even when their callers
        // disconnected. New post-exit operations wait for this state transition.
        let mut state = self.state.lock().expect("input state");
        self.queue.close();
        if let Some(reply) = self.active.take() {
            let _ = reply.send(Err(state.write_error()));
        }
        while let Ok((bytes, reply)) = self.queue.try_recv() {
            let _ = reply.send(state.after_exit(bytes.is_some()));
        }
    }
}

impl Writer {
    pub async fn run(mut self) {
        while let Some((bytes, reply)) = self.queue.recv().await {
            self.active = Some(reply);
            let result = match bytes {
                None => {
                    self.state.lock().expect("input state").enabled = false;
                    self.target.take();
                    Ok(())
                }
                Some(bytes) => self.write(&bytes).await,
            };
            let _ = self.active.take().unwrap().send(result);
        }
    }
    async fn write(&mut self, bytes: &[u8]) -> Result<(), DomainError> {
        let Some(target) = self.target.as_mut() else {
            return Err(DomainError::FailedPrecondition(
                "error writing to stdin: stdin not enabled or closed".into(),
            ));
        };
        let mut written = 0;
        while written < bytes.len() {
            // Once decoded and accepted, an input follows the process lifetime,
            // not the response connection. Wait/reap owns cancellation.
            let result = target.write(&bytes[written..]).await;
            match result {
                Ok(count) if count > 0 => written += count,
                Err(error) if error.kind() == std::io::ErrorKind::Interrupted => continue,
                result => {
                    let (kind, file) = match target {
                        Target::Pipe(_) => ("stdin", "|1"),
                        Target::Pty(_) => ("tty", "/dev/ptmx"),
                    };
                    let detail = match result {
                        Err(error) => error
                            .to_string()
                            .split(" (os error ")
                            .next()
                            .unwrap_or("I/O error")
                            .to_lowercase(),
                        _ => "short write".to_owned(),
                    };
                    // A failed write does not close the parent's descriptor.
                    // Subsequent writes observe the same kernel pipe state.
                    return Err(DomainError::InternalMessage(format!(
                        "error writing to {kind}: error writing to {kind} of process '{}': write {file}: {detail}",
                        self.pid,
                    )));
                }
            }
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn exit_drains_disconnected_close_in_accepted_order() {
        let (input, writer) = Input::new(None, 7);
        input.state.lock().unwrap().enabled = true;
        let before = input.accept(Some(vec![1]));
        let close = input.accept(None);
        drop(close);
        let after = input.accept(Some(vec![2]));
        drop(writer);
        assert_eq!(before.await.unwrap(), Err(DomainError::InternalMessage(
            "error writing to stdin: error writing to stdin of process '7': write |1: file already closed".into()
        )));
        let closed = Err(DomainError::FailedPrecondition(
            "error writing to stdin: stdin not enabled or closed".into(),
        ));
        assert_eq!(after.await.unwrap(), closed);
        assert_eq!(input.accept(Some(vec![3])).await.unwrap(), closed);
        assert_eq!(input.close().await, Ok(()));
    }
}

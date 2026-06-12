// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use super::ContainerState;
use crate::log::Log;
use crate::{debugf, errf, infof};
use oci_spec::runtime::Process;
use protoc::{agent, agent_ttrpc};
use tokio::fs::OpenOptions;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use ttrpc::context;

const LOG_FILE_MODE: u32 = 0o600;

#[derive(Clone, Default)]
pub struct Exec {
    pub container_id: String,
    pub id: String,
    pub tty: Tty,
    pub proc: Process,
    pub state: Option<ContainerState>,
}

#[derive(Clone, Default)]
pub struct Tty {
    pub stdin: String,
    pub stdout: String,
    pub stderr: String,
    pub height: u32,
    pub width: u32,
    pub terminal: bool,
}

impl Exec {
    /// Spawn stdout/stderr forwarding sub-tasks for exec processes.
    /// Returns immediately; the spawned tasks run until the stream closes.
    pub async fn forward_std(
        &self,
        state: ContainerState,
        client: agent_ttrpc::AgentServiceClient,
        log: Log,
    ) {
        let state_in = state.clone();
        let client_in = client.clone();
        let log_in = log.clone();

        let exec_in = self.clone();
        // exec processes have no external cancel; keep the sender alive inside
        // each spawned task so the watch receiver is not immediately closed.
        let (cancel_tx_in, cancel_rx_in) = tokio::sync::watch::channel(false);
        tokio::spawn(async move {
            let _keep_alive = cancel_tx_in;
            exec_in
                .forward_stdin(state_in, client_in, log_in, cancel_rx_in)
                .await;
        });

        let state_out = state.clone();
        let client_out = client.clone();
        let log_out = log.clone();
        let exec_out = self.clone();
        let (cancel_tx_out, cancel_rx_out) = tokio::sync::watch::channel(false);
        tokio::spawn(async move {
            let _keep_alive = cancel_tx_out;
            exec_out
                .forward_stdout(state_out, client_out, log_out, cancel_rx_out)
                .await;
        });

        let exec = self.clone();
        let (cancel_tx_err, cancel_rx_err) = tokio::sync::watch::channel(false);
        tokio::spawn(async move {
            let _keep_alive = cancel_tx_err;
            exec.forward_stderr(state, client, log, cancel_rx_err).await;
        });
    }

    /// Spawn stdout+stderr forwarding tasks for the main container log and
    /// return a single JoinHandle that, when awaited, confirms both sub-tasks
    /// have exited.  The cancel_rx watch receiver is cloned into each sub-task;
    /// when the sender sends `true` both loops exit at their next select! point.
    pub async fn start_log_forward(
        &self,
        client: agent_ttrpc::AgentServiceClient,
        log: Log,
        cancel_rx: tokio::sync::watch::Receiver<bool>,
    ) -> tokio::task::JoinHandle<()> {
        // ContainerState is required for the forwarding loops (they select on
        // the process exit / VM-pause channel).  If it is somehow absent we
        // cannot forward; return a no-op task.
        let (state_out, state_err) = match self.state.clone() {
            Some(s) => (s.clone(), s),
            None => {
                return tokio::spawn(async {});
            }
        };

        let exec_out = self.clone();
        let client_out = client.clone();
        let log_out = log.clone();
        let cancel_out = cancel_rx.clone();

        let exec_err = self.clone();
        let cancel_err = cancel_rx;

        tokio::spawn(async move {
            let h_out = tokio::spawn(async move {
                exec_out
                    .forward_stdout(state_out, client_out, log_out, cancel_out)
                    .await;
            });
            let h_err = tokio::spawn(async move {
                exec_err
                    .forward_stderr(state_err, client, log, cancel_err)
                    .await;
            });
            // Await both sub-tasks so the outer JoinHandle represents true
            // completion: when the caller awaits this handle it knows both
            // vsock reads have finished.
            let _ = tokio::join!(h_out, h_err);
        })
    }

    pub async fn forward_stdin(
        &self,
        _state: ContainerState,
        client: agent_ttrpc::AgentServiceClient,
        log: Log,
        mut cancel: tokio::sync::watch::Receiver<bool>,
    ) {
        infof!(log, "forward stdin start");
        if self.tty.stdin.is_empty() {
            infof!(log, "exec:{} stdin is empty", self.id.clone());
            return;
        }
        let mut file = match OpenOptions::new()
            .read(true)
            .write(false)
            .open(self.tty.stdin.clone())
            .await
        {
            Ok(file) => file,
            Err(e) => {
                errf!(
                    log,
                    "exec:{}, open stdin file:{} failed:{}",
                    self.id.clone(),
                    self.tty.stdin.clone(),
                    e
                );
                return;
            }
        };

        let mut buf = [0; 4096];
        let mut req = agent::WriteStreamRequest {
            container_id: self.container_id.clone(),
            exec_id: self.id.clone(),
            ..Default::default()
        };
        let ctx = context::Context::default();

        loop {
            let res = tokio::select! {
                _ = cancel.changed() => {
                    if *cancel.borrow() {
                        infof!(log, "exec:{} forward stdin cancelled", self.id.clone());
                        return;
                    }
                    continue;
                }
                // Block until user input or FIFO close; no timeout on idle.
                res = file.read(&mut buf) => res,
            };
            if let Err(e) = res {
                infof!(
                    log,
                    "exec:{}, read fifo:{} failed:{}",
                    self.id.clone(),
                    self.tty.stdin.clone(),
                    e
                );
                return;
            }

            let n = res.unwrap();
            if n == 0 {
                infof!(log, "stdin closed");
                return;
            }
            let mut offset = 0;

            while offset < n {
                req.data = buf[offset..n].to_vec();
                let size = match client.write_stdin(ctx.clone(), &req).await {
                    Err(e) => {
                        debugf!(
                            log,
                            "exec:{}, write process stdin failed:{}",
                            self.id.clone(),
                            e
                        );
                        return;
                    }
                    Ok(rsp) => rsp.len,
                };
                if size == 0 {
                    infof!(
                        log,
                        "exec:{}, write process stdin failed: write size is 0",
                        self.id.clone()
                    );
                    return;
                }
                offset += size as usize;
            }
        }
    }

    pub async fn forward_stdout(
        &self,
        _state: ContainerState,
        client: agent_ttrpc::AgentServiceClient,
        log: Log,
        mut cancel: tokio::sync::watch::Receiver<bool>,
    ) {
        infof!(log, "forward stdout start");
        if self.tty.stdout.is_empty() {
            infof!(log, "exec:{} stdout is empty", self.id.clone());
            return;
        }

        let mut file = match tokio::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .mode(LOG_FILE_MODE)
            .open(self.tty.stdout.clone())
            .await
        {
            Ok(file) => file,
            Err(e) => {
                errf!(
                    log,
                    "exec:{}, open stdout file:{} failed:{}",
                    self.id.clone(),
                    self.tty.stdout.clone(),
                    e
                );
                return;
            }
        };

        let req = agent::ReadStreamRequest {
            container_id: self.container_id.clone(),
            exec_id: self.id.clone(),
            len: 4096,
            ..Default::default()
        };
        // No RPC timeout: log/exec stdout reads may idle for long periods between
        // lines; exit is driven by cancel (pause/snapshot) or vsock/RPC errors.
        let ctx = context::Context::default();
        loop {
            tokio::select! {
                // Cancel signal: pause / snapshot / destroy path.
                // watch::changed() resolves as soon as the sender sends true.
                _ = cancel.changed() => {
                    if *cancel.borrow() {
                        infof!(log, "exec:{} forward stdout cancelled", self.id.clone());
                        return;
                    }
                }
                res = client.read_stdout(ctx.clone(), &req) => {
                    match res {
                        Err(e) => {
                            debugf!(
                                log,
                                "exec:{}, read process stdout failed:{}",
                                self.id.clone(),
                                e
                            );
                            return;
                        }
                        Ok(rsp) => {
                            if let Err(e) = file.write_all(&rsp.data).await {
                                infof!(
                                    log,
                                    "exec:{}, write process stdout failed:{}",
                                    self.id.clone(),
                                    e
                                );
                                return;
                            }
                        }
                    }
                }
            }
        }
    }

    pub async fn forward_stderr(
        &self,
        _state: ContainerState,
        client: agent_ttrpc::AgentServiceClient,
        log: Log,
        mut cancel: tokio::sync::watch::Receiver<bool>,
    ) {
        infof!(log, "forward stderr start");
        if self.tty.stderr.is_empty() {
            infof!(log, "exec:{} stderr is empty", self.id.clone());
            return;
        }

        let mut file = match tokio::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .mode(LOG_FILE_MODE)
            .open(self.tty.stderr.clone())
            .await
        {
            Ok(file) => file,
            Err(e) => {
                errf!(
                    log,
                    "exec:{}, open stderr file:{} failed:{}",
                    self.id.clone(),
                    self.tty.stderr.clone(),
                    e
                );
                return;
            }
        };

        let req = agent::ReadStreamRequest {
            container_id: self.container_id.clone(),
            exec_id: self.id.clone(),
            len: 4096,
            ..Default::default()
        };
        let ctx = context::Context::default();
        loop {
            tokio::select! {
                _ = cancel.changed() => {
                    if *cancel.borrow() {
                        infof!(log, "exec:{} forward stderr cancelled", self.id.clone());
                        return;
                    }
                }
                res = client.read_stderr(ctx.clone(), &req) => {
                    match res {
                        Err(e) => {
                            debugf!(
                                log,
                                "exec:{}, read process stderr failed:{}",
                                self.id.clone(),
                                e
                            );
                            return;
                        }
                        Ok(rsp) => {
                            if let Err(e) = file.write_all(&rsp.data).await {
                                infof!(
                                    log,
                                    "exec:{}, write process stderr failed:{}",
                                    self.id.clone(),
                                    e
                                );
                                return;
                            }
                        }
                    }
                }
            }
        }
    }
}

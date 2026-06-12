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
    /// Init container log forwarding uses an empty exec_id when talking to the agent.
    fn is_init_log(&self) -> bool {
        self.id.is_empty()
    }

    /// Exec I/O relay (unchanged from pre-log-forwarding).
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
        tokio::spawn(async move {
            exec_in.forward_stdin(state_in, client_in, log_in).await;
        });

        let state_out = state.clone();
        let client_out = client.clone();
        let log_out = log.clone();
        let exec_out = self.clone();
        tokio::spawn(async move {
            exec_out
                .forward_stdout(state_out, client_out, log_out)
                .await;
        });

        let exec = self.clone();
        tokio::spawn(async move {
            exec.forward_stderr(state, client, log).await;
        });
    }

    /// Init-process log forwarding only (exec_id is empty).
    pub async fn start_log_forward(
        &self,
        client: agent_ttrpc::AgentServiceClient,
        log: Log,
        cancel_rx: tokio::sync::watch::Receiver<bool>,
    ) -> tokio::task::JoinHandle<()> {
        debug_assert!(
            self.is_init_log(),
            "start_log_forward requires empty exec_id"
        );

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
                    .forward_init_log_stdout(state_out, client_out, log_out, cancel_out)
                    .await;
            });
            let h_err = tokio::spawn(async move {
                exec_err
                    .forward_init_log_stderr(state_err, client, log, cancel_err)
                    .await;
            });
            let _ = tokio::join!(h_out, h_err);
        })
    }

    pub async fn forward_stdin(
        &self,
        _state: ContainerState,
        client: agent_ttrpc::AgentServiceClient,
        log: Log,
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
            let res = file.read(&mut buf).await;
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

    /// Exec stdout relay (pre-log-forwarding behaviour).
    pub async fn forward_stdout(
        &self,
        _state: ContainerState,
        client: agent_ttrpc::AgentServiceClient,
        log: Log,
    ) {
        infof!(log, "forward stdout start");
        if self.tty.stdout.is_empty() {
            infof!(log, "exec:{} stdout is empty", self.id.clone());
            return;
        }

        let mut file = match OpenOptions::new()
            .write(true)
            .open(self.tty.stdout.clone())
            .await
        {
            Ok(file) => file,
            Err(e) => {
                errf!(
                    log,
                    "exec:{}, open stdout fifo:{} failed:{}",
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
        let ctx = context::Context::default();

        loop {
            let res = client.read_stdout(ctx.clone(), &req).await;
            if let Err(e) = res {
                debugf!(
                    log,
                    "exec:{}, read process stdout failed:{}",
                    self.id.clone(),
                    e
                );
                return;
            }

            let rsp = res.unwrap().data;
            if let Err(e) = file.write_all(&rsp).await {
                infof!(
                    log,
                    "exec:{}, write process stdout failed:{}",
                    self.id.clone(),
                    e
                );
            }
        }
    }

    /// Exec stderr relay (pre-log-forwarding behaviour).
    pub async fn forward_stderr(
        &self,
        _state: ContainerState,
        client: agent_ttrpc::AgentServiceClient,
        log: Log,
    ) {
        infof!(log, "forward stderr start");
        if self.tty.stderr.is_empty() {
            infof!(log, "exec:{} stderr is empty", self.id.clone());
            return;
        }

        let mut file = match OpenOptions::new()
            .write(true)
            .open(self.tty.stderr.clone())
            .await
        {
            Ok(file) => file,
            Err(e) => {
                errf!(
                    log,
                    "exec:{}, open stderr fifo:{} failed:{}",
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
            let res = client.read_stderr(ctx.clone(), &req).await;
            if let Err(e) = res {
                debugf!(
                    log,
                    "exec:{}, read process stderr failed:{}",
                    self.id.clone(),
                    e
                );
                return;
            }

            let rsp = res.unwrap().data;
            if let Err(e) = file.write_all(&rsp).await {
                infof!(
                    log,
                    "exec:{}, write process stderr failed:{}",
                    self.id.clone(),
                    e
                );
            }
        }
    }

    /// Init container log: agent read_stdout with exec_id="" → append to log file.
    async fn forward_init_log_stdout(
        &self,
        _state: ContainerState,
        client: agent_ttrpc::AgentServiceClient,
        log: Log,
        mut cancel: tokio::sync::watch::Receiver<bool>,
    ) {
        infof!(log, "forward init log stdout start");
        if self.tty.stdout.is_empty() {
            return;
        }

        let mut file = match OpenOptions::new()
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
                    "init log: open stdout file:{} failed:{}",
                    self.tty.stdout.clone(),
                    e
                );
                return;
            }
        };

        let req = agent::ReadStreamRequest {
            container_id: self.container_id.clone(),
            exec_id: String::new(),
            len: 4096,
            ..Default::default()
        };
        let ctx = context::Context::default();

        loop {
            tokio::select! {
                _ = cancel.changed() => {
                    if *cancel.borrow() {
                        infof!(log, "init log forward stdout cancelled");
                        return;
                    }
                }
                res = client.read_stdout(ctx.clone(), &req) => {
                    match res {
                        Err(e) => {
                            debugf!(log, "init log: read stdout failed:{}", e);
                            return;
                        }
                        Ok(rsp) => {
                            if let Err(e) = file.write_all(&rsp.data).await {
                                infof!(log, "init log: write stdout failed:{}", e);
                                return;
                            }
                        }
                    }
                }
            }
        }
    }

    /// Init container log: agent read_stderr with exec_id="" → append to log file.
    async fn forward_init_log_stderr(
        &self,
        _state: ContainerState,
        client: agent_ttrpc::AgentServiceClient,
        log: Log,
        mut cancel: tokio::sync::watch::Receiver<bool>,
    ) {
        infof!(log, "forward init log stderr start");
        if self.tty.stderr.is_empty() {
            return;
        }

        let mut file = match OpenOptions::new()
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
                    "init log: open stderr file:{} failed:{}",
                    self.tty.stderr.clone(),
                    e
                );
                return;
            }
        };

        let req = agent::ReadStreamRequest {
            container_id: self.container_id.clone(),
            exec_id: String::new(),
            len: 4096,
            ..Default::default()
        };
        let ctx = context::Context::default();

        loop {
            tokio::select! {
                _ = cancel.changed() => {
                    if *cancel.borrow() {
                        infof!(log, "init log forward stderr cancelled");
                        return;
                    }
                }
                res = client.read_stderr(ctx.clone(), &req) => {
                    match res {
                        Err(e) => {
                            debugf!(log, "init log: read stderr failed:{}", e);
                            return;
                        }
                        Ok(rsp) => {
                            if let Err(e) = file.write_all(&rsp.data).await {
                                infof!(log, "init log: write stderr failed:{}", e);
                                return;
                            }
                        }
                    }
                }
            }
        }
    }
}

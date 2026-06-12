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
    /// True when forwarding the init container log (exec_id is empty).
    fn is_init_log(&self) -> bool {
        self.id.is_empty()
    }

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
        tokio::spawn(async move {
            exec_in
                .forward_stdin(state_in, client_in, log_in)
                .await;
        });

        let state_out = state.clone();
        let client_out = client.clone();
        let log_out = log.clone();
        let exec_out = self.clone();
        tokio::spawn(async move {
            exec_out
                .forward_stdout(state_out, client_out, log_out, None)
                .await;
        });

        let exec = self.clone();
        tokio::spawn(async move {
            exec.forward_stderr(state, client, log, None).await;
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
                    .forward_stdout(state_out, client_out, log_out, Some(cancel_out))
                    .await;
            });
            let h_err = tokio::spawn(async move {
                exec_err
                    .forward_stderr(state_err, client, log, Some(cancel_err))
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

    async fn open_stdout_sink(
        &self,
        log: &Log,
    ) -> Result<tokio::fs::File, ()> {
        if self.is_init_log() {
            match tokio::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .mode(LOG_FILE_MODE)
                .open(self.tty.stdout.clone())
                .await
            {
                Ok(file) => Ok(file),
                Err(e) => {
                    errf!(
                        log,
                        "exec:{}, open stdout log file:{} failed:{}",
                        self.id.clone(),
                        self.tty.stdout.clone(),
                        e
                    );
                    Err(())
                }
            }
        } else {
            match OpenOptions::new()
                .write(true)
                .open(self.tty.stdout.clone())
                .await
            {
                Ok(file) => Ok(file),
                Err(e) => {
                    errf!(
                        log,
                        "exec:{}, open stdout fifo:{} failed:{}",
                        self.id.clone(),
                        self.tty.stdout.clone(),
                        e
                    );
                    Err(())
                }
            }
        }
    }

    async fn open_stderr_sink(
        &self,
        log: &Log,
    ) -> Result<tokio::fs::File, ()> {
        if self.is_init_log() {
            match tokio::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .mode(LOG_FILE_MODE)
                .open(self.tty.stderr.clone())
                .await
            {
                Ok(file) => Ok(file),
                Err(e) => {
                    errf!(
                        log,
                        "exec:{}, open stderr log file:{} failed:{}",
                        self.id.clone(),
                        self.tty.stderr.clone(),
                        e
                    );
                    Err(())
                }
            }
        } else {
            match OpenOptions::new()
                .write(true)
                .open(self.tty.stderr.clone())
                .await
            {
                Ok(file) => Ok(file),
                Err(e) => {
                    errf!(
                        log,
                        "exec:{}, open stderr fifo:{} failed:{}",
                        self.id.clone(),
                        self.tty.stderr.clone(),
                        e
                    );
                    Err(())
                }
            }
        }
    }

    pub async fn forward_stdout(
        &self,
        _state: ContainerState,
        client: agent_ttrpc::AgentServiceClient,
        log: Log,
        cancel: Option<tokio::sync::watch::Receiver<bool>>,
    ) {
        infof!(log, "forward stdout start");
        if self.tty.stdout.is_empty() {
            infof!(log, "exec:{} stdout is empty", self.id.clone());
            return;
        }

        let mut file = match self.open_stdout_sink(&log).await {
            Ok(file) => file,
            Err(()) => return,
        };

        let req = agent::ReadStreamRequest {
            container_id: self.container_id.clone(),
            exec_id: self.id.clone(),
            len: 4096,
            ..Default::default()
        };
        let ctx = context::Context::default();

        if let Some(mut cancel) = cancel {
            loop {
                tokio::select! {
                    _ = cancel.changed() => {
                        if *cancel.borrow() {
                            infof!(log, "exec:{} forward stdout cancelled", self.id.clone());
                            return;
                        }
                    }
                    res = client.read_stdout(ctx.clone(), &req) => {
                        if !Self::handle_read_stdout(&log, &self.id, &mut file, res).await {
                            return;
                        }
                    }
                }
            }
        } else {
            loop {
                let res = client.read_stdout(ctx.clone(), &req).await;
                if !Self::handle_read_stdout(&log, &self.id, &mut file, res).await {
                    return;
                }
            }
        }
    }

    pub async fn forward_stderr(
        &self,
        _state: ContainerState,
        client: agent_ttrpc::AgentServiceClient,
        log: Log,
        cancel: Option<tokio::sync::watch::Receiver<bool>>,
    ) {
        infof!(log, "forward stderr start");
        if self.tty.stderr.is_empty() {
            infof!(log, "exec:{} stderr is empty", self.id.clone());
            return;
        }

        let mut file = match self.open_stderr_sink(&log).await {
            Ok(file) => file,
            Err(()) => return,
        };

        let req = agent::ReadStreamRequest {
            container_id: self.container_id.clone(),
            exec_id: self.id.clone(),
            len: 4096,
            ..Default::default()
        };
        let ctx = context::Context::default();

        if let Some(mut cancel) = cancel {
            loop {
                tokio::select! {
                    _ = cancel.changed() => {
                        if *cancel.borrow() {
                            infof!(log, "exec:{} forward stderr cancelled", self.id.clone());
                            return;
                        }
                    }
                    res = client.read_stderr(ctx.clone(), &req) => {
                        if !Self::handle_read_stderr(&log, &self.id, &mut file, res).await {
                            return;
                        }
                    }
                }
            }
        } else {
            loop {
                let res = client.read_stderr(ctx.clone(), &req).await;
                if !Self::handle_read_stderr(&log, &self.id, &mut file, res).await {
                    return;
                }
            }
        }
    }

    async fn handle_read_stdout(
        log: &Log,
        exec_id: &str,
        file: &mut tokio::fs::File,
        res: Result<agent::ReadStreamResponse, ttrpc::Error>,
    ) -> bool {
        match res {
            Err(e) => {
                debugf!(log, "exec:{}, read process stdout failed:{}", exec_id, e);
                false
            }
            Ok(rsp) => {
                if let Err(e) = file.write_all(&rsp.data).await {
                    infof!(
                        log,
                        "exec:{}, write process stdout failed:{}",
                        exec_id, e
                    );
                    if exec_id.is_empty() {
                        return false;
                    }
                }
                true
            }
        }
    }

    async fn handle_read_stderr(
        log: &Log,
        exec_id: &str,
        file: &mut tokio::fs::File,
        res: Result<agent::ReadStreamResponse, ttrpc::Error>,
    ) -> bool {
        match res {
            Err(e) => {
                debugf!(log, "exec:{}, read process stderr failed:{}", exec_id, e);
                false
            }
            Ok(rsp) => {
                if let Err(e) = file.write_all(&rsp.data).await {
                    infof!(
                        log,
                        "exec:{}, write process stderr failed:{}",
                        exec_id, e
                    );
                    if exec_id.is_empty() {
                        return false;
                    }
                }
                true
            }
        }
    }
}

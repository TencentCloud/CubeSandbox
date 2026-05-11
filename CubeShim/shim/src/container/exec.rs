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
use ttrpc::context::{self, Context};

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
        let ctx = context::with_timeout(1000 * 1000 * 1000 * 3);

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
            .read(false)
            .write(true)
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

        //let mut buf = [0; 4096];
        let req = agent::ReadStreamRequest {
            container_id: self.container_id.clone(),
            exec_id: self.id.clone(),
            len: 4096,
            ..Default::default()
        };
        let ctx = Context::default();
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
            .read(false)
            .write(true)
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

        //let mut buf = [0; 4096];
        let req = agent::ReadStreamRequest {
            container_id: self.container_id.clone(),
            exec_id: self.id.clone(),
            len: 4096,
            ..Default::default()
        };
        let ctx = Context::default();
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
}

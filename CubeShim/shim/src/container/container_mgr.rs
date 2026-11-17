// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::sync::Arc;

use chrono::{DateTime, Utc};
use containerd_shim::event::Event;
use containerd_shim::protos::events::task::TaskExit;
use containerd_shim::protos::protobuf::MessageDyn;
use protoc::agent::WaitProcessResponse;
use protoc::{agent, agent_ttrpc};
use tokio::sync::{mpsc::channel, mpsc::Receiver, mpsc::Sender, Mutex};
use ttrpc::context;

use crate::log::Log;
use crate::{errf, infof};

const EXIT_CODE_255: u32 = 255;

#[derive(Clone, PartialEq, Default, Debug)]
pub enum TaskState {
    #[default]
    UNKNOWN = 0,
    CREATED = 1,
    RUNNING = 2,
    STOPPED = 3,
    //not support
    PAUSED = 4,
    //not support
    PAUSING = 5,
}

#[derive(Clone, Default)]
pub struct ContainerInfo {
    pub id: String,
    pub bundle: String,
    pub stdin: String,
    pub stdout: String,
    pub stderr: String,
    pub terminal: bool,
    pub state: TaskState,
    pub exit_code: u32,
    pub exit_tm: Option<DateTime<Utc>>,
}

#[derive(Clone)]
pub struct ContainerState {
    exit_info: Arc<Mutex<(u32, DateTime<Utc>)>>,
    tx: Arc<Mutex<Sender<()>>>,
    rx: Arc<Mutex<Receiver<()>>>,
    log: Log,
    vm_tx: Arc<Mutex<Sender<TaskState>>>,
    vm_rx: Arc<Mutex<Receiver<TaskState>>>,
    state: Arc<Mutex<TaskState>>,
    wait_handle: Option<Arc<tokio::task::JoinHandle<()>>>,
}

impl ContainerState {
    pub fn new(log: Log) -> Self {
        let (tx, rx) = channel::<()>(1);
        let (vm_tx, vm_rx) = channel::<TaskState>(1024);
        ContainerState {
            exit_info: Arc::new(Mutex::new((0, Utc::now()))),
            tx: Arc::new(Mutex::new(tx)),
            rx: Arc::new(Mutex::new(rx)),
            log,
            vm_tx: Arc::new(Mutex::new(vm_tx)),
            vm_rx: Arc::new(Mutex::new(vm_rx)),
            state: Arc::new(Mutex::new(TaskState::CREATED)),
            wait_handle: None,
        }
    }

    pub async fn notify_vm_shutdown(&self) {
        let tx = self.vm_tx.lock().await;
        let _ = tx.try_send(TaskState::STOPPED);
    }

    pub async fn notify_vm_pause(&self) {
        let tx = self.vm_tx.lock().await;
        let _ = tx.try_send(TaskState::PAUSED);
        if let Some(handle) = self.wait_handle.as_ref() {
            handle.abort()
        }
    }

    pub async fn wait_process(
        &mut self,
        client: agent_ttrpc::AgentServiceClient,
        cid: String,
        real_cid: String,
        exec_id: String,
        tx_containerd: Sender<(String, Box<dyn MessageDyn>)>,
    ) {
        let mut state = self.clone();
        let handle = tokio::spawn(async move {
            state
                .wait_process_task(client, cid, real_cid, exec_id, tx_containerd)
                .await;
        });
        self.wait_handle = Some(Arc::new(handle));
    }

    async fn wait_process_task(
        &mut self,
        client: agent_ttrpc::AgentServiceClient,
        cid: String,
        real_cid: String,
        exec_id: String,
        tx_containerd: Sender<(String, Box<dyn MessageDyn>)>,
    ) {
        let tx = self.tx.lock().await;
        let mut vm_rx = self.vm_rx.lock().await;
        {
            let mut state = self.state.lock().await;
            *state = TaskState::RUNNING;
        }
        infof!(self.log, "wait process start");

        let req = agent::WaitProcessRequest {
            container_id: cid.clone(),
            exec_id: exec_id.clone(),
            ..Default::default()
        };

        let ctx = context::Context::default();
        let rsp = tokio::select! {
            r = client.wait_process(ctx, &req) => {
                r
            },
            state = vm_rx.recv() => {
                infof!(self.log, "wait process {:?}", state.clone());
                match state {
                    Some(TaskState::STOPPED) => {
                        Ok(WaitProcessResponse {
                            status: -1,
                            ..Default::default()
                        })
                    }
                    Some(TaskState::PAUSED) => {
                        return;
                    }
                    _ => {
                        errf!(self.log, "not support state:{:?}", state.clone());
                        return;
                    }
                }
            }

        };

        let mut exit_info = self.exit_info.lock().await;
        exit_info.1 = Utc::now();
        let _exit_code = 0;
        match rsp {
            Ok(r) => {
                exit_info.0 = r.status as u32;
                infof!(
                    self.log,
                    "wait container:{} real:{} execid{} finish, exit code:{}",
                    cid.clone(),
                    real_cid.clone(),
                    exec_id.clone(),
                    r.status
                );
            }
            Err(e) => {
                exit_info.0 = EXIT_CODE_255;
                errf!(
                    self.log,
                    "wait container:{} real:{} execid{} error:{}",
                    cid.clone(),
                    real_cid.clone(),
                    exec_id.clone(),
                    e
                );
            }
        }
        let e_tm = protobuf::well_known_types::timestamp::Timestamp {
            seconds: (exit_info.1).timestamp(),
            ..Default::default()
        };

        //change state
        {
            let mut state = self.state.lock().await;
            *state = TaskState::STOPPED;
        }
        //notify waiter
        let _ = tx.try_send(());

        //notify containerd
        let mut event = TaskExit {
            container_id: real_cid.clone(),
            id: exec_id,
            pid: std::process::id(),
            exit_status: exit_info.0,
            exited_at: Some(e_tm).into(),
            ..Default::default()
        };
        if event.id.is_empty() {
            event.id = real_cid.clone()
        }
        let topic = event.topic();
        let _ = tx_containerd.try_send((topic, Box::new(event)));
    }

    pub async fn wait_exit_info(&self) -> (u32, DateTime<Utc>) {
        {
            let mut rx = self.rx.lock().await;
            if (rx.recv().await).is_some() {
                let tx = self.tx.lock().await;
                let _ = tx.try_send(());
            }
        }

        let info = self.exit_info.lock().await;
        (info.0, info.1)
    }

    pub async fn set_container_stoped(&self) {
        //set exit info
        {
            let mut exit_info = self.exit_info.lock().await;
            exit_info.0 = 0;
            exit_info.1 = Utc::now();
        }

        //change state
        {
            let mut state = self.state.lock().await;
            *state = TaskState::STOPPED;
        }
        let tx = self.tx.lock().await;
        //notify waiter
        let _ = tx.try_send(());
    }

    pub async fn get_exit_info(&self) -> (u32, DateTime<Utc>) {
        let info = self.exit_info.lock().await;
        (info.0, info.1)
    }

    pub async fn is_running(&self) -> bool {
        let state = self.state.lock().await;
        *state == TaskState::RUNNING
    }

    pub async fn is_created(&self) -> bool {
        let state = self.state.lock().await;
        *state == TaskState::CREATED
    }

    pub async fn state(&self) -> TaskState {
        let state = self.state.lock().await;

        (*state).clone()
    }
}

#[cfg(test)]
mod pause_vm_forbidding_tests {
    use super::*;
    use crate::container::{exec::Exec, Container, LogForwardHandle};
    use crate::sandbox::config::Config;
    use oci_spec::runtime::Spec;
    use std::collections::HashMap;

    async fn container_with_execs(states: &[TaskState]) -> Container {
        let mut execs = HashMap::new();
        for (index, task_state) in states.iter().enumerate() {
            let state = ContainerState::new(Log::default());
            if *task_state == TaskState::STOPPED {
                state.set_container_stoped().await;
            } else {
                *state.state.lock().await = task_state.clone();
            }
            let id = format!("exec-{index}");
            execs.insert(
                id.clone(),
                Exec {
                    id,
                    state: Some(state),
                    ..Default::default()
                },
            );
        }
        let (tx_containerd, _) = channel(1);
        Container {
            sandbox_id: "sandbox".into(),
            id: "container".into(),
            real_id: "container".into(),
            spec: Spec::default(),
            client: None,
            ctx: context::Context::default(),
            log: Log::default(),
            sb_conf: Config::default(),
            info: ContainerInfo::default(),
            state: None,
            tx_containerd,
            execs: Arc::new(Mutex::new(execs)),
            app_snapshot: false,
            log_forward: LogForwardHandle::new(),
        }
    }

    #[tokio::test]
    async fn no_execs_allow_pause() {
        let container = container_with_execs(&[]).await;
        assert!(!container.pause_vm_forbidding().await);
    }

    #[tokio::test]
    async fn stopped_exec_allows_pause_without_losing_exit_info() {
        let mut container = container_with_execs(&[TaskState::STOPPED]).await;

        assert!(!container.pause_vm_forbidding().await);

        let exec_id = "exec-0".to_string();
        let info = container.get_container_info(&exec_id).await.unwrap();
        assert_eq!(info.state, TaskState::STOPPED);
        assert_eq!(info.exit_code, 0);
        let (code, _) = tokio::time::timeout(
            std::time::Duration::from_secs(1),
            container.wait_container(&exec_id),
        )
        .await
        .unwrap()
        .unwrap();
        assert_eq!(code, 0);
    }

    #[tokio::test]
    async fn running_exec_forbids_pause() {
        let container = container_with_execs(&[TaskState::RUNNING]).await;
        assert!(container.pause_vm_forbidding().await);
    }

    #[tokio::test]
    async fn running_exec_among_stopped_execs_forbids_pause() {
        let container = container_with_execs(&[TaskState::STOPPED, TaskState::RUNNING]).await;
        assert!(container.pause_vm_forbidding().await);
    }

    #[tokio::test]
    async fn unstarted_exec_forbids_pause() {
        for state in [TaskState::UNKNOWN, TaskState::CREATED] {
            let container = container_with_execs(&[state]).await;
            assert!(container.pause_vm_forbidding().await);
        }
    }
}

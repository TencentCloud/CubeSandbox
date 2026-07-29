// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::common::utils::Utils;

use crate::container::{container_mgr::ContainerInfo, exec::Tty};
use crate::log::{stat_defer, Log, LogLevel};
use crate::sandbox::sb;
use crate::service::update_ext;
use crate::{debugf, errf, infof, warnf};
use async_trait::async_trait;

use containerd_shim::event::Event;

use containerd_shim::{
    asynchronous::{publisher::RemotePublisher, ExitSignal},
    protos::events::task::{
        TaskCreate, TaskDelete, TaskExecAdded, TaskExecStarted, TaskIO, TaskStart,
    },
    protos::{
        api, protobuf::MessageDyn, shim_async::Task, ttrpc::r#async::TtrpcContext,
        ttrpc::Error::Others, types::task,
    },
    Context, Error, TtrpcResult,
};
use protobuf::Enum;
use std::sync::Arc;
use std::time::Instant;
use tokio::sync::mpsc::{channel, Receiver, Sender};
use tokio::sync::Mutex;
const MODULE: &str = "Shim";
const INTERNAL_PROBE_EXEC_ID_PREFIX: &str = "cubesandbox-internal-probe-";

#[derive(Clone)]
pub struct TaskService {
    //id: String,
    //ns: String,
    sandbox: Arc<Mutex<sb::SandBox>>,
    log: Log,
    //debug: bool,
    exit: Arc<ExitSignal>,
    tx_containerd: Sender<(String, Box<dyn MessageDyn>)>,
}

impl TaskService {
    pub async fn new(
        id: String,
        ns: String,
        debug: bool,
        exit: Arc<ExitSignal>,
        publisher: RemotePublisher,
    ) -> Self {
        let mut level = LogLevel::Info;
        if debug {
            level = LogLevel::Debug;
        }

        let log = Log::new(id.clone(), MODULE.to_string(), level);

        let (tx, rx) = channel::<(String, Box<dyn MessageDyn>)>(128);

        forward_event(rx, publisher, ns.clone(), log.clone()).await;

        let sb = sb::SandBox::new(id.clone(), log.clone(), debug, tx.clone());
        TaskService {
            //id,
            //ns,
            sandbox: Arc::new(Mutex::new(sb)),
            log,
            //debug: debug,
            exit,
            tx_containerd: tx,
        }
    }

    async fn tx_event(&self, topic: String, event: Box<dyn MessageDyn>) {
        self.tx_containerd
            .try_send((topic.clone(), event))
            .unwrap_or_else(|e| warnf!(self.log, "tx event:{} to publisher failed:{}", topic, e));
    }
}

#[async_trait]
impl Task for TaskService {
    async fn create(
        &self,
        _ctx: &TtrpcContext,
        req: api::CreateTaskRequest,
    ) -> TtrpcResult<api::CreateTaskResponse> {
        infof!(self.log, "create req start");
        let start = Instant::now();
        let mut stat = stat_defer::StatDefer::new(
            req.id.clone(),
            stat_defer::CALLEE_SHIM.to_string(),
            stat_defer::ACT_CREATE.to_string(),
            stat_defer::CALLEE_ACT_CREATE_POD_CONTAINER.to_string(),
            self.log.clone(),
        );

        let bundle = req.bundle.as_str();

        let spec = Utils::load_spec(bundle).map_err(|e| {
            errf!(self.log, "Load spec failed:{}", e.clone());
            Others(format!("Load spec failed:{}", e))
        })?;

        infof!(
            self.log,
            "load spec finish at:{}",
            start.elapsed().as_millis()
        );

        let mut sb = self.sandbox.lock().await;
        if sb.paused().await {
            errf!(self.log, "sandbox not in normal state");
            return Err(Others(format!("sandbox not in normal state")));
        }
        if !sb.inited() {
            stat.set_callee_act(stat_defer::CALLEE_ACT_CREATE_POD_SANDBOX.to_string());
            infof!(self.log, "shim pid {}", std::process::id());
            if let Err(e) = Utils::record_pid() {
                errf!(self.log, "Create pid file failed:{}", e);
                return Err(Others(format!("Create pid file failed:{}", e)));
            }
            sb.init(spec.clone()).map_err(|e| {
                errf!(self.log, "Init sandbox config failed:{}", e.clone());
                Error::Other(format!("Init sandbox config failed:{}", e))
            })?;

            sb.create_sandbox().await.map_err(|e| {
                errf!(self.log, "Create sandbox failed:{}", e.clone());
                Error::Other(format!("Create sandbox failed:{}", e))
            })?;
        }

        infof!(
            self.log,
            "start sandbox finish at:{}",
            start.elapsed().as_millis()
        );
        let info = ContainerInfo {
            id: req.id.clone(),
            bundle: req.bundle.clone(),
            stdout: req.stdout.clone(),
            stderr: req.stderr.clone(),
            terminal: req.terminal,
            ..Default::default()
        };
        sb.create_container(req.id.clone(), spec, info)
            .await
            .map_err(|e| {
                errf!(self.log, "Create container failed:{}", e.clone());
                Error::Other(format!("Create container failed:{}", e))
            })?;
        infof!(
            self.log,
            "start container finish at:{}",
            start.elapsed().as_millis()
        );

        let io = TaskIO {
            stdin: req.stdin.clone(),
            stdout: req.stdout.clone(),
            stderr: req.stderr.clone(),
            terminal: req.terminal,
            ..Default::default()
        };
        let event = TaskCreate {
            container_id: req.id.clone(),
            bundle: req.bundle.clone(),
            rootfs: req.rootfs.clone(),
            checkpoint: req.checkpoint.clone(),
            pid: sb.pid(),
            io: Some(io).into(),
            ..Default::default()
        };
        let topic = event.topic();
        self.tx_event(topic, Box::new(event)).await;
        stat.set_ok();
        infof!(self.log, "create req finish");
        Ok(api::CreateTaskResponse {
            pid: sb.pid(),
            ..Default::default()
        })
    }
    async fn start(
        &self,
        _ctx: &TtrpcContext,
        req: api::StartRequest,
    ) -> TtrpcResult<api::StartResponse> {
        infof!(
            self.log,
            "start request, id:{}, execid:{}",
            req.id(),
            req.exec_id()
        );
        let mut sb = self.sandbox.lock().await;
        if sb.paused().await {
            errf!(self.log, "sandbox not in normal state");
            return Err(Others(format!("sandbox not in normal state")));
        }
        if req.exec_id().is_empty() {
            sb.start_container(&req.id).await.map_err(|e| {
                errf!(self.log, "Start container failed:{}", e);
                e
            })?;

            let event = TaskStart {
                container_id: req.id.clone(),
                pid: sb.pid(),
                ..Default::default()
            };
            let topic = event.topic();
            self.tx_event(topic, Box::new(event)).await;
        } else {
            sb.start_exec(&req.id, &req.exec_id).await.map_err(|e| {
                errf!(self.log, "Start exec failed:{}", e);
                e
            })?;

            let event = TaskExecStarted {
                container_id: req.id.clone(),
                exec_id: req.exec_id.clone(),
                pid: sb.pid(),
                ..Default::default()
            };
            let topic = event.topic();
            self.tx_event(topic, Box::new(event)).await;
        }
        infof!(self.log, "start req finish");
        Ok(api::StartResponse {
            pid: sb.pid(),
            ..Default::default()
        })
    }

    async fn wait(
        &self,
        _ctx: &TtrpcContext,
        req: api::WaitRequest,
    ) -> TtrpcResult<api::WaitResponse> {
        infof!(
            self.log,
            "wait req start, id:{}, execid:{}",
            req.id(),
            req.exec_id()
        );

        let sb = {
            let sb = self.sandbox.lock().await;
            sb.clone()
        };
        if sb.paused().await {
            errf!(self.log, "sandbox not in normal state");
            return Err(Others(format!("sandbox not in normal state")));
        }

        let (code, tm) = sb
            .wait_container(&req.id, &req.exec_id)
            .await
            .map_err(|e| {
                errf!(self.log, "wait failed:{}", e);
                e
            })?;
        let e_tm: protobuf::well_known_types::timestamp::Timestamp =
            protobuf::well_known_types::timestamp::Timestamp {
                seconds: tm.timestamp(),
                ..Default::default()
            };
        infof!(self.log, "wait req finish");
        Ok(api::WaitResponse {
            exit_status: code,
            exited_at: Some(e_tm).into(),
            ..Default::default()
        })
    }

    async fn delete(
        &self,
        _ctx: &TtrpcContext,
        req: api::DeleteRequest,
    ) -> TtrpcResult<api::DeleteResponse> {
        infof!(
            self.log,
            "delete req start, id:{}, execid:{}",
            req.id(),
            req.exec_id()
        );
        let mut stat = stat_defer::StatDefer::new(
            req.id.clone(),
            stat_defer::CALLEE_SHIM.to_string(),
            stat_defer::ACT_DELETE.to_string(),
            stat_defer::CALLEE_ACT_DEL_CONTAINER.to_string(),
            self.log.clone(),
        );
        let mut sb = self.sandbox.lock().await;
        if sb.paused().await {
            errf!(self.log, "sandbox not in normal state");
            return Err(Others(format!("sandbox not in normal state")));
        }
        let (exit_code, exit_tm) = {
            if req.exec_id.is_empty() {
                match sb.delete_container(&req.id).await {
                    Err(e) => {
                        errf!(self.log, "delete container failed:{}", e);
                        return Err(e.into());
                    }
                    Ok((code, tm)) => {
                        let e_tm = protobuf::well_known_types::timestamp::Timestamp {
                            seconds: tm.timestamp(),
                            ..Default::default()
                        };
                        let event = TaskDelete {
                            container_id: req.id.clone(),
                            pid: sb.pid(),
                            exit_status: code,
                            exited_at: Some(e_tm.clone()).into(),
                            ..Default::default()
                        };
                        let topic = event.topic();
                        self.tx_event(topic, Box::new(event)).await;
                        (code, e_tm)
                    }
                }
            } else {
                match sb.delete_exec(&req.id, &req.exec_id).await {
                    Err(e) => {
                        errf!(self.log, "delete exec failed:{}", e);
                        return Err(e.into());
                    }

                    Ok((code, tm)) => {
                        let e_tm = protobuf::well_known_types::timestamp::Timestamp {
                            seconds: tm.timestamp(),
                            ..Default::default()
                        };
                        (code, e_tm)
                    }
                }
            }
        };
        stat.set_ok();
        infof!(self.log, "delete req finish");

        Ok(api::DeleteResponse {
            pid: sb.pid(),
            exit_status: exit_code,
            exited_at: Some(exit_tm).into(),
            ..Default::default()
        })
    }

    async fn kill(&self, _ctx: &TtrpcContext, req: api::KillRequest) -> TtrpcResult<api::Empty> {
        infof!(
            self.log,
            "kill req start, id:{} execid:{}",
            req.id(),
            req.exec_id()
        );
        let mut exec_id = req.exec_id.clone();
        if req.all {
            exec_id = "".to_string();
        }
        let sb = self.sandbox.lock().await;
        if sb.paused().await {
            errf!(self.log, "sandbox not in normal state");
            return Err(Others(format!("sandbox not in normal state")));
        }
        sb.kill_container(&req.id, &exec_id, req.signal())
            .await
            .map_err(|e| {
                errf!(self.log, "Kill container failed:{}", e);
                e
            })?;

        infof!(self.log, "kill req finish");
        Ok(api::Empty::default())
    }

    async fn update(
        &self,
        _ctx: &TtrpcContext,
        req: api::UpdateTaskRequest,
    ) -> TtrpcResult<api::Empty> {
        infof!(self.log, "update req start, id:{}", &req.id);
        let mut sb = self.sandbox.lock().await;
        if sb.paused().await {
            errf!(self.log, "sandbox not in normal state");
            return Err(Others(format!("sandbox not in normal state")));
        }
        if let Some(resource) = req.resources.as_ref() {
            let res = Utils::get_oci_res(resource.value.as_slice())
                .map_err(|e| Error::Other(format!("Invalid format process config:{}", e)))?;
            sb.update_container(&req.id, &res).await.map_err(|e| {
                errf!(self.log, "update container failed:{}", e);
                e
            })?;
        }

        sb.update_sandbox(&req.annotations).await.map_err(|e| {
            errf!(self.log, "update sandbox failed:{}", e.clone());
            Error::Other(format!("update sandbox failed:{}", e))
        })?;

        update_ext::update_route(&mut sb, &req.annotations, &self.log)
            .await
            .map_err(|e| {
                errf!(self.log, "update sandbox failed:{}", e.clone());
                Error::Other(format!("update sandbox failed:{}", e))
            })?;

        infof!(self.log, "update req finish");
        Ok(api::Empty::default())
    }

    async fn connect(
        &self,
        _ctx: &TtrpcContext,
        _req: api::ConnectRequest,
    ) -> TtrpcResult<api::ConnectResponse> {
        debugf!(self.log, "connect request");
        let pid = std::process::id();
        Ok(api::ConnectResponse {
            shim_pid: pid,
            task_pid: pid,
            ..Default::default()
        })
    }

    async fn shutdown(
        &self,
        _ctx: &TtrpcContext,
        _req: api::ShutdownRequest,
    ) -> TtrpcResult<api::Empty> {
        infof!(self.log, "shutdown req start");

        let mut sb = self.sandbox.lock().await;
        if sb.paused().await {
            errf!(self.log, "sandbox not in normal state");
            return Err(Others(format!("sandbox not in normal state")));
        }
        if !sb.is_empty().await {
            infof!(
                self.log,
                "sandbox not empty, do nothing, shutdown req finish"
            );
            return Ok(api::Empty::default());
        }
        if let Err(e) = sb.destroy_sandbox().await {
            errf!(self.log, "shutdown failed:{}", e)
        } else {
            infof!(self.log, "shutdown req finish");
        }
        self.exit.signal();
        Ok(api::Empty::default())
    }

    async fn state(
        &self,
        _ctx: &TtrpcContext,
        req: api::StateRequest,
    ) -> TtrpcResult<api::StateResponse> {
        let sb = self.sandbox.lock().await;
        match sb.get_container_info(&req.id, &req.exec_id).await {
            Ok(c) => {
                let state = protobuf::EnumOrUnknown::new(
                    task::Status::from_i32(c.state as i32).unwrap_or(task::Status::default()),
                );
                let mut rsp = api::StateResponse {
                    id: req.id.clone(),
                    exec_id: req.exec_id.clone(),
                    status: state,
                    bundle: c.bundle,
                    pid: sb.pid(),
                    stdout: c.stdout,
                    stderr: c.stderr,
                    terminal: c.terminal,
                    exit_status: c.exit_code,
                    ..Default::default()
                };

                if let Some(exit_tm) = c.exit_tm {
                    rsp.exited_at = Some(protobuf::well_known_types::timestamp::Timestamp {
                        seconds: exit_tm.timestamp(),
                        ..Default::default()
                    })
                    .into();
                }
                if sb.paused().await {
                    rsp.status = protobuf::EnumOrUnknown::new(task::Status::PAUSED);
                }
                return Ok(rsp);
            }
            Err(e) => {
                errf!(self.log, "state request error:{}", e);
                Err(e.into())
            }
        }
    }

    async fn exec(
        &self,
        _ctx: &TtrpcContext,
        req: api::ExecProcessRequest,
    ) -> TtrpcResult<api::Empty> {
        infof!(
            self.log,
            "exec req start, id:{}, execid:{}",
            req.id(),
            req.exec_id()
        );
        let sb = self.sandbox.lock().await;
        if sb.app_snapshot_create() && !is_internal_probe_exec_id(req.exec_id()) {
            infof!(self.log, "exec disabled while app snapshotting");
            return Err(Others("exec disabled while app snapshotting".to_string()));
        }
        if sb.paused().await {
            errf!(self.log, "sandbox not in normal state");
            return Err(Others(format!("sandbox not in normal state")));
        }
        let proc = match req.spec.as_ref() {
            Some(v) => Utils::get_oci_proc(v.value.as_slice()).map_err(|e| {
                errf!(self.log, "Invalid format process config:{}", e.clone());
                Error::Other(format!("Invalid format process config:{}", e))
            })?,
            None => {
                return Err(Others("Not found process config".to_string()));
            }
        };

        let tty = Tty {
            stdout: req.stdout.clone(),
            stderr: req.stderr.clone(),
            stdin: req.stdin.clone(),
            terminal: req.terminal,
            ..Default::default()
        };

        sb.exec_container(&req.id, &req.exec_id, tty, proc)
            .await
            .map_err(|e| {
                errf!(self.log, "Exec container:{} failed:{}", req.id.clone(), e);
                e
            })?;

        let event = TaskExecAdded {
            container_id: req.id.clone(),
            exec_id: req.exec_id.clone(),
            ..Default::default()
        };
        let topic = event.topic();
        self.tx_event(topic, Box::new(event)).await;
        infof!(self.log, "exec req finish");
        Ok(api::Empty::default())
    }

    async fn resize_pty(
        &self,
        _ctx: &TtrpcContext,
        _req: api::ResizePtyRequest,
    ) -> TtrpcResult<api::Empty> {
        Ok(api::Empty::default())
    }

    async fn pids(
        &self,
        _ctx: &TtrpcContext,
        _req: api::PidsRequest,
    ) -> TtrpcResult<api::PidsResponse> {
        let sb = self.sandbox.lock().await;
        let rsp = api::PidsResponse {
            processes: vec![task::ProcessInfo {
                pid: sb.pid(),
                ..Default::default()
            }],
            ..Default::default()
        };
        Ok(rsp)
    }

    async fn pause(&self, ctx: &TtrpcContext, _req: api::PauseRequest) -> TtrpcResult<api::Empty> {
        infof!(self.log, "pause req start");
        if ctx.metadata.get("pod_scope").is_none() {
            return Err(Others(
                "current pause operations are only supported at the pod level".to_string(),
            ));
        }
        let mut sb: tokio::sync::MutexGuard<'_, sb::SandBox> = self.sandbox.lock().await;
        if !sb.normal().await {
            errf!(self.log, "sandbox not in normal state");
            return Err(Others(format!("sandbox not in normal state")));
        }

        sb.pause_vm().await.map_err(|e| {
            errf!(self.log, "pause vm failed:{}", e);
            Error::Other(format!("Pause vm failed:{}", e))
        })?;

        infof!(self.log, "pause req finish");
        Ok(api::Empty::default())
    }

    async fn resume(
        &self,
        ctx: &TtrpcContext,
        _req: api::ResumeRequest,
    ) -> TtrpcResult<api::Empty> {
        infof!(self.log, "resume req start");
        if ctx.metadata.get("pod_scope").is_none() {
            return Err(Others(
                "current resume operations are only supported at the pod level.".to_string(),
            ));
        }
        let mut sb: tokio::sync::MutexGuard<'_, sb::SandBox> = self.sandbox.lock().await;
        if !sb.paused().await {
            errf!(self.log, "sandbox not in paused state");
            return Err(Others(format!("sandbox not in paused state")));
        }
        sb.resume_vm().await.map_err(|e| {
            errf!(self.log, "resume vm failed:{}", e);
            Error::Other(format!("Resume vm failed:{}", e))
        })?;
        infof!(self.log, "resume req finish");
        Ok(api::Empty::default())
    }
}

fn is_internal_probe_exec_id(exec_id: &str) -> bool {
    exec_id.starts_with(INTERNAL_PROBE_EXEC_ID_PREFIX)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn internal_probe_exec_requires_exec_id_prefix() {
        assert!(is_internal_probe_exec_id(
            "cubesandbox-internal-probe-4e7d6a"
        ));
        assert!(!is_internal_probe_exec_id("internal-probe-4e7d6a"));
        assert!(!is_internal_probe_exec_id(
            "user-cubesandbox-internal-probe-4e7d6a"
        ));
    }
}

async fn forward_event(
    mut rx: Receiver<(String, Box<dyn MessageDyn>)>,
    publisher: RemotePublisher,
    ns: String,
    log: Log,
) {
    tokio::spawn(async move {
        let mut publisher = publisher;
        const TTRPC_ADDRESS: &str = "TTRPC_ADDRESS";
        while let Some((topic, evt)) = rx.recv().await {
            let ret = publisher
                .publish(Context::default(), &topic, &ns, evt.clone())
                .await;

            if let Err(e) = ret {
                warnf!(log, "publish {} to containerd failed: {}", topic, e);
                if let Ok(ttrpc_address) = std::env::var(TTRPC_ADDRESS) {
                    match RemotePublisher::new(ttrpc_address).await {
                        Ok(p) => {
                            publisher = p;
                            publisher
                                .publish(Context::default(), &topic, &ns, evt)
                                .await
                                .unwrap_or_else(|e| {
                                    warnf!(log, "publish {} to containerd failed: {}", topic, e)
                                });
                        }
                        Err(e) => warnf!(log, "RemotePublisher reconnect failed:{}", e),
                    }
                } else {
                    warnf!(log, "not found env {} can't reconnect", TTRPC_ADDRESS);
                }
            }
        }
    });
}

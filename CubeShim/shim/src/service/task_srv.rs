// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::sync::Arc;
use std::time::Instant;

use async_trait::async_trait;
use containerd_shim::event::Event;
use containerd_shim::{
    asynchronous::{publisher::RemotePublisher, ExitSignal},
    protos::events::task::{
        TaskCreate, TaskDelete, TaskExecAdded, TaskExecStarted, TaskIO, TaskStart,
    },
    protos::{
        api,
        cgroups::metrics::{CPUStat, CPUUsage, MemoryEntry, MemoryStat, Metrics, Throttle},
        protobuf::MessageDyn,
        shim_async::Task,
        ttrpc::r#async::TtrpcContext,
        ttrpc::Error::Others,
        types::task,
    },
    Context, Error, TtrpcResult,
};
use protobuf::{Enum, Message};
use tokio::sync::mpsc::{channel, Receiver, Sender};
use tokio::sync::Mutex;

use crate::common::utils::Utils;
use crate::container::{container_mgr::ContainerInfo, exec::Tty};
use crate::log::{stat_defer, Log, LogLevel};
use crate::sandbox::sb;
use crate::service::update_ext;
use crate::{debugf, errf, infof, warnf};
const MODULE: &str = "Shim";
const INTERNAL_PROBE_EXEC_ID_PREFIX: &str = "cubesandbox-internal-probe-";
const CGROUP_V1_METRICS_TYPE_URL: &str = "io.containerd.cgroups.v1.Metrics";
const RESOURCE_METRICS_VERSION_V1: u32 = 1;

fn normalize_guest_stats(stats: &protoc::agent::StatsContainerResponse) -> Result<Metrics, String> {
    let version = stats.get_resource_metrics_version();
    if version != RESOURCE_METRICS_VERSION_V1 {
        return Err(format!(
            "guest does not support resource metrics version {} (reported version {})",
            RESOURCE_METRICS_VERSION_V1, version
        ));
    }
    if !stats.has_cgroup_stats() {
        return Err("guest response is missing cgroup stats".to_string());
    }
    let guest = stats.get_cgroup_stats();
    if !guest.has_cpu_stats() || !guest.has_memory_stats() {
        return Err("guest response is missing CPU or memory stats".to_string());
    }
    let guest_cpu = guest.get_cpu_stats();
    if !guest_cpu.has_cpu_usage() || !guest_cpu.has_throttling_data() {
        return Err("guest response is missing CPU usage or throttling stats".to_string());
    }
    if !guest.get_memory_stats().has_usage() {
        return Err("guest response is missing memory usage stats".to_string());
    }
    let guest_usage = guest_cpu.get_cpu_usage();
    let guest_throttling = guest_cpu.get_throttling_data();
    let guest_memory = guest.get_memory_stats().get_usage();

    let cpu = CPUStat {
        usage: Some(CPUUsage {
            total: guest_usage.get_total_usage(),
            kernel: guest_usage.get_usage_in_kernelmode(),
            user: guest_usage.get_usage_in_usermode(),
            per_cpu: guest_usage.get_percpu_usage().to_vec(),
            ..Default::default()
        })
        .into(),
        throttling: Some(Throttle {
            periods: guest_throttling.get_periods(),
            throttled_periods: guest_throttling.get_throttled_periods(),
            throttled_time: guest_throttling.get_throttled_time(),
            ..Default::default()
        })
        .into(),
        ..Default::default()
    };
    let memory = MemoryStat {
        usage: Some(MemoryEntry {
            usage: guest_memory.get_usage(),
            max: guest_memory.get_max_usage(),
            failcnt: guest_memory.get_failcnt(),
            limit: guest_memory.get_limit(),
            ..Default::default()
        })
        .into(),
        ..Default::default()
    };

    Ok(Metrics {
        cpu: Some(cpu).into(),
        memory: Some(memory).into(),
        ..Default::default()
    })
}

fn encode_guest_stats(
    stats: &protoc::agent::StatsContainerResponse,
) -> Result<protobuf::well_known_types::any::Any, String> {
    let metrics = normalize_guest_stats(stats)?;
    let value = metrics
        .write_to_bytes()
        .map_err(|e| format!("encode cgroup metrics failed: {}", e))?;
    Ok(protobuf::well_known_types::any::Any {
        type_url: CGROUP_V1_METRICS_TYPE_URL.to_string(),
        value,
        ..Default::default()
    })
}

#[cfg(test)]
mod stats_tests {
    use super::{encode_guest_stats, normalize_guest_stats, CGROUP_V1_METRICS_TYPE_URL};
    use protoc::agent::{
        CgroupStats, CpuStats, CpuUsage as GuestCpuUsage, MemoryData, MemoryStats,
        StatsContainerResponse, ThrottlingData,
    };

    fn complete_guest_stats_response() -> StatsContainerResponse {
        let mut response = StatsContainerResponse::new();
        let mut cgroup = CgroupStats::new();
        let mut cpu = CpuStats::new();
        cpu.set_cpu_usage(GuestCpuUsage {
            // cube-agent normalizes cgroup v2 microsecond counters to
            // nanoseconds before returning StatsContainerResponse.
            total_usage: 101_000,
            usage_in_kernelmode: 31_000,
            usage_in_usermode: 70_000,
            percpu_usage: vec![40_000, 61_000],
            ..Default::default()
        });
        cpu.set_throttling_data(ThrottlingData {
            periods: 9,
            throttled_periods: 3,
            throttled_time: 17,
            ..Default::default()
        });
        cgroup.set_cpu_stats(cpu);

        let mut memory = MemoryStats::new();
        memory.set_usage(MemoryData {
            usage: 4096,
            max_usage: 8192,
            failcnt: 2,
            limit: 16384,
            ..Default::default()
        });
        cgroup.set_memory_stats(memory);
        response.set_cgroup_stats(cgroup);
        response.set_resource_metrics_version(1);
        response
    }

    #[test]
    fn normalizes_guest_cpu_and_memory_into_containerd_metrics() {
        let response = complete_guest_stats_response();

        let metrics = normalize_guest_stats(&response).unwrap();
        assert_eq!(metrics.cpu().usage().total, 101_000);
        assert_eq!(metrics.cpu().usage().kernel, 31_000);
        assert_eq!(metrics.cpu().usage().user, 70_000);
        assert_eq!(metrics.cpu().throttling().periods, 9);
        assert_eq!(metrics.cpu().throttling().throttled_periods, 3);
        assert_eq!(metrics.cpu().throttling().throttled_time, 17);
        assert_eq!(metrics.memory().usage().usage, 4096);
        assert_eq!(metrics.memory().usage().max, 8192);
        assert_eq!(metrics.memory().usage().failcnt, 2);
        assert_eq!(metrics.memory().usage().limit, 16384);

        let encoded = encode_guest_stats(&response).unwrap();
        assert_eq!(encoded.type_url, CGROUP_V1_METRICS_TYPE_URL);
        assert!(!encoded.value.is_empty());
    }

    #[test]
    fn rejects_incomplete_guest_cpu_and_memory_stats() {
        let mut missing_cpu_stats = complete_guest_stats_response();
        missing_cpu_stats.mut_cgroup_stats().clear_cpu_stats();

        let mut missing_memory_stats = complete_guest_stats_response();
        missing_memory_stats.mut_cgroup_stats().clear_memory_stats();

        let mut missing_cpu_usage = complete_guest_stats_response();
        missing_cpu_usage
            .mut_cgroup_stats()
            .mut_cpu_stats()
            .clear_cpu_usage();

        let mut missing_throttling = complete_guest_stats_response();
        missing_throttling
            .mut_cgroup_stats()
            .mut_cpu_stats()
            .clear_throttling_data();

        let mut missing_memory_usage = complete_guest_stats_response();
        missing_memory_usage
            .mut_cgroup_stats()
            .mut_memory_stats()
            .clear_usage();

        for (name, response, expected) in [
            (
                "cpu stats",
                missing_cpu_stats,
                "guest response is missing CPU or memory stats",
            ),
            (
                "memory stats",
                missing_memory_stats,
                "guest response is missing CPU or memory stats",
            ),
            (
                "CPU usage",
                missing_cpu_usage,
                "guest response is missing CPU usage or throttling stats",
            ),
            (
                "CPU throttling",
                missing_throttling,
                "guest response is missing CPU usage or throttling stats",
            ),
            (
                "memory usage",
                missing_memory_usage,
                "guest response is missing memory usage stats",
            ),
        ] {
            assert_eq!(
                normalize_guest_stats(&response).unwrap_err(),
                expected,
                "{name}"
            );
        }
    }

    #[test]
    fn rejects_missing_guest_cgroup_stats() {
        let mut response = StatsContainerResponse::new();
        response.set_resource_metrics_version(1);
        let err = normalize_guest_stats(&response).unwrap_err();
        assert_eq!(err, "guest response is missing cgroup stats");
    }

    #[test]
    fn rejects_guest_without_resource_metrics_capability() {
        let err = normalize_guest_stats(&StatsContainerResponse::new()).unwrap_err();
        assert_eq!(
            err,
            "guest does not support resource metrics version 1 (reported version 0)"
        );
    }

    #[test]
    fn rejects_unknown_resource_metrics_capability_version() {
        let mut response = StatsContainerResponse::new();
        response.set_resource_metrics_version(2);
        let err = normalize_guest_stats(&response).unwrap_err();
        assert_eq!(
            err,
            "guest does not support resource metrics version 1 (reported version 2)"
        );
    }
}

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
            stdin: req.stdin.clone(),
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
        let start_at = Instant::now();
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
        infof!(
            self.log,
            "start req finish, id:{}, execid:{}, cost_ms:{}",
            req.id(),
            req.exec_id(),
            start_at.elapsed().as_millis()
        );
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
        let start_at = Instant::now();
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
        infof!(
            self.log,
            "wait req finish, id:{}, execid:{}, exit_status:{}, cost_ms:{}",
            req.id(),
            req.exec_id(),
            code,
            start_at.elapsed().as_millis()
        );
        Ok(api::WaitResponse {
            exit_status: code,
            exited_at: Some(e_tm).into(),
            ..Default::default()
        })
    }

    async fn stats(
        &self,
        _ctx: &TtrpcContext,
        req: api::StatsRequest,
    ) -> TtrpcResult<api::StatsResponse> {
        if req.id.is_empty() {
            return Err(Error::InvalidArgument("stats request id is empty".to_string()).into());
        }

        let sb = {
            let sb = self.sandbox.lock().await;
            sb.clone()
        };
        let guest_stats = sb.stats_container(&req.id).await.map_err(|e| {
            errf!(self.log, "StatsContainer failed for {}: {}", req.id, e);
            e
        })?;
        let stats = encode_guest_stats(&guest_stats).map_err(Error::FailedPreconditionError)?;

        Ok(api::StatsResponse {
            stats: Some(stats).into(),
            ..Default::default()
        })
    }

    async fn delete(
        &self,
        _ctx: &TtrpcContext,
        req: api::DeleteRequest,
    ) -> TtrpcResult<api::DeleteResponse> {
        let start_at = Instant::now();
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
        infof!(
            self.log,
            "delete req finish, id:{}, execid:{}, exit_status:{}, cost_ms:{}",
            req.id(),
            req.exec_id(),
            exit_code,
            start_at.elapsed().as_millis()
        );

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
        let start_at = Instant::now();
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
        infof!(
            self.log,
            "exec req finish, id:{}, execid:{}, cost_ms:{}",
            req.id(),
            req.exec_id(),
            start_at.elapsed().as_millis()
        );
        Ok(api::Empty::default())
    }

    async fn close_io(
        &self,
        _ctx: &TtrpcContext,
        req: api::CloseIORequest,
    ) -> TtrpcResult<api::Empty> {
        let start_at = Instant::now();
        infof!(
            self.log,
            "close_io req start, id:{}, execid:{}",
            req.id(),
            req.exec_id()
        );

        if !req.stdin {
            infof!(
                self.log,
                "close_io req finish, id:{}, execid:{}, stdin:false, cost_ms:{}",
                req.id(),
                req.exec_id(),
                start_at.elapsed().as_millis()
            );
            return Ok(api::Empty::default());
        }

        let sb = self.sandbox.lock().await;
        if sb.paused().await {
            errf!(self.log, "sandbox not in normal state");
            return Err(Others(format!("sandbox not in normal state")));
        }

        sb.close_io(&req.id, &req.exec_id).await.map_err(|e| {
            errf!(self.log, "close_io failed:{}", e);
            e
        })?;

        infof!(
            self.log,
            "close_io req finish, id:{}, execid:{}, stdin:true, cost_ms:{}",
            req.id(),
            req.exec_id(),
            start_at.elapsed().as_millis()
        );
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

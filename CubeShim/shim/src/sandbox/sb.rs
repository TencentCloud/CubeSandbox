// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use super::config::{Fs, ANNO_VMM_FS, VIRTIO_FS_ID, VIRTIO_FS_TAG};
use crate::common::types::PropagationMount;
use crate::common::utils::{self, AsyncUtils, CPath, Utils};
use crate::common::{
    CResult, ANNO_PROPAGATION_MNTS, CUBE_BIND_SHARE_GUEST_BASE_DIR, CUBE_BIND_SHARE_TYPE,
    GUEST_VIRTIOFS_MNT_PATH_DEPRECATED, PAUSE_VM_SNAPSHOT_BASE,
};
use crate::container::container_mgr::ContainerInfo;
use crate::container::{exec::Tty, Container, GUEST_DEV_SHM};
use crate::hypervisor::config::{HypConfig, VmConfig};
use crate::hypervisor::cube_hypervisor as CH;
use crate::hypervisor::snapshot::{enable_snapshot, SnapshotInfo};
use crate::log::{stat_defer, Log};
use crate::sandbox::config;
use crate::{debugf, errf, infof, warnf};
use chrono::{DateTime, Utc};
use containerd_shim::event::Event;
use containerd_shim::protos::events::task::TaskOOM;
use containerd_shim::protos::protobuf::MessageDyn;
use containerd_shim::{Error, Result};
use cube_hypervisor::config::RestoreConfig;
use cube_hypervisor::vm_config::{DeviceConfig, FsConfig};
use cube_hypervisor::VmRemoveDeviceData;
use oci_spec::runtime::{LinuxResources, Process, Spec};
use protoc::{agent, agent_ttrpc, health, health_ttrpc};
use std::collections::{HashMap, HashSet};
use std::fs as stdfs;
use std::net::IpAddr;
use std::time::Instant;
use ttrpc::context::{self, Context};
use ttrpc::r#async::Client;

use std::path::PathBuf;
use std::sync::Arc;

use tokio::sync::mpsc::{channel, Sender};
use tokio::sync::Mutex;
use tokio::time::{sleep, Duration};

use super::device;
use super::disk::Disk;
use super::pmem::{self, Pmem};
//use tokio_uring::fs::UnixStream;

const ANNO_SANDBOX_DNS: &str = "cube.sandbox.dns";

#[derive(PartialEq, Eq)]
enum SandBoxState {
    Normal,
    Paused,
    Exited,
}

#[derive(Clone)]
pub struct SandBox {
    id: String,
    conn: Option<Arc<Mutex<Client>>>,
    pub(super) client: Option<Arc<Mutex<agent_ttrpc::AgentServiceClient>>>,
    spec: Spec,
    conf: config::Config,
    pub(super) ctx: Context,
    ch: Option<Arc<Mutex<CH::CubeHypervisor>>>,
    containers: Arc<Mutex<HashMap<String, Container>>>,
    pub(super) log: Log,
    inited: bool,
    pid: u32,
    tx_containerd: Sender<(String, Box<dyn MessageDyn>)>,
    debug: bool,
    state: Arc<Mutex<SandBoxState>>,
    tx_monitor_exited: Option<Sender<()>>,
    monitor_handle: Option<Arc<tokio::task::JoinHandle<()>>>,
    tx_oom_exited: Option<Sender<()>>,
    oom_handle: Option<Arc<tokio::task::JoinHandle<()>>>,
}

impl SandBox {
    pub fn new(
        id: String,
        log: Log,
        debug: bool,
        tx_containerd: Sender<(String, Box<dyn MessageDyn>)>,
    ) -> Self {
        let mut hyp_config = HypConfig {
            debug,
            log_level: log::LevelFilter::Info,
            sandbox_id: id.clone(),
            ch_http_api: Some(Utils::chapi_path(id.as_str())),
        };

        if debug {
            hyp_config.log_level = log::LevelFilter::Debug;
        }
        let ch: CH::CubeHypervisor = CH::CubeHypervisor::new(hyp_config, log.clone());

        SandBox {
            id,
            conn: None,
            client: None,
            spec: Spec::default(),
            conf: config::Config::default(),
            ctx: context::with_timeout(1000 * 1000 * 1000 * 3),
            ch: Some(Arc::new(Mutex::new(ch))),
            containers: Arc::new(Mutex::new(HashMap::new())),
            log,
            inited: false,
            pid: std::process::id(),
            tx_containerd,
            debug,
            state: Arc::new(Mutex::new(SandBoxState::Normal)),
            tx_monitor_exited: None,
            monitor_handle: None,
            tx_oom_exited: None,
            oom_handle: None,
        }
    }

    pub fn app_snapshot_create(&self) -> bool {
        self.conf.app_snapshot_create
    }

    pub fn app_snapshot_restore(&self) -> bool {
        self.conf.app_snapshot_restore
    }

    pub fn normal_create(&self) -> bool {
        !(self.conf.app_snapshot_create || self.conf.app_snapshot_restore)
    }

    pub fn inited(&self) -> bool {
        self.inited
    }

    pub fn init(&mut self, spec: Spec) -> CResult<()> {
        self.spec = spec;
        self.conf = config::Config::new(self.spec.annotations())?;
        let mut vm_dir = PathBuf::from(utils::VM_PATH);
        vm_dir.push(self.id.clone());
        stdfs::create_dir_all(vm_dir).map_err(|e| format!("Mkdir vm run dir failed:{}", e))?;
        self.inited = true;
        Ok(())
    }

    pub fn deinit(&mut self, _spec: Spec) {
        if let Err(e) = Utils::clean_sandbox_resource(&self.id) {
            warnf!(self.log, "clean resource failed:{}", e)
        }
    }

    fn new_create_stat(&self, callee_act: String) -> stat_defer::StatDefer {
        stat_defer::StatDefer::new(
            self.id.clone(),
            stat_defer::CALLEE_AGENT.to_string(),
            stat_defer::ACT_CREATE.to_string(),
            callee_act,
            self.log.clone(),
        )
    }

    async fn connect_agent(&mut self) -> CResult<()> {
        let conn = AsyncUtils::connect_agent(&self.id).await?;
        let client = agent_ttrpc::AgentServiceClient::new(conn.clone());
        self.conn = Some(Arc::new(Mutex::new(conn)));
        self.client = Some(Arc::new(Mutex::new(client)));

        Ok(())
    }

    async fn pause_vm_forbidding(&self) -> bool {
        let containers = self.containers.lock().await;
        for (_, c) in containers.iter() {
            if c.pause_vm_forbidding().await {
                return true;
            }
        }
        false
    }
    async fn disconnect_agent(&mut self) -> CResult<()> {
        //stop monitor

        if let Some(tx) = self.tx_monitor_exited.as_ref() {
            let _ = tx.try_send(());
        }

        if let Some(handle) = self.monitor_handle.take() {
            handle.abort();
        }

        //stop watch oom event
        if let Some(tx) = self.tx_oom_exited.as_ref() {
            let _ = tx.try_send(());
        }

        if let Some(handle) = self.oom_handle.take() {
            handle.abort();
        }

        let mut containers = self.containers.lock().await;
        for (_, c) in containers.iter_mut() {
            c.unset_client().await;
        }
        tokio::time::sleep(Duration::from_millis(1000)).await;
        if let Some(client) = self.client.take() {
            if Arc::strong_count(&client) != 1 {
                errf!(
                    self.log,
                    "client ref count is {}",
                    Arc::strong_count(&client)
                );
                //return Err(format!("disconnect_agent: client ref count is not 1").into());
            }
            drop(client)
        }

        if let Some(conn) = self.conn.take() {
            if Arc::strong_count(&conn) != 1 {
                errf!(self.log, "conn ref count is {}", Arc::strong_count(&conn));
                //return Err(format!("disconnect_agent: conn ref count is not 1").into());
                drop(conn)
            }
        }
        Ok(())
    }

    fn get_storages(&mut self) -> CResult<Vec<agent::Storage>> {
        let mut storages = Vec::new();

        if self.normal_create() {
            //default config
            if self.conf.fs.is_some() {
                let virtiofs = agent::Storage {
                    driver: "virtio-fs".to_string(),
                    source: "cubeShared".to_string(),
                    fstype: "virtiofs".to_string(),
                    options: vec!["ro".to_string()].into(),
                    mount_point: GUEST_VIRTIOFS_MNT_PATH_DEPRECATED.to_string(),
                    ..Default::default()
                };
                storages.push(virtiofs);
            }
        }
        if !self.app_snapshot_create() {
            for fs in self.conf.virtiofs.iter() {
                debugf!(self.log, "add virtiofs: {:?}", fs.id.clone());
                let mut virtiofs = agent::Storage {
                    driver: "virtio-fs".to_string(),
                    source: fs.id.clone(),
                    fstype: "virtiofs".to_string(),
                    options: vec![].into(),
                    ..Default::default()
                };
                if fs.propagation_mount_name.is_empty() {
                    virtiofs.mount_point = Utils::virtiofs_guest_base(fs.id.clone());
                } else {
                    let mut cp = CPath::new(CUBE_BIND_SHARE_GUEST_BASE_DIR);
                    cp.join(fs.propagation_mount_name.as_str());
                    virtiofs.mount_point = cp.to_str().unwrap_or_default().to_string();
                }
                storages.push(virtiofs);
            }
        }

        let anno = self.spec.annotations().as_ref().unwrap();
        if let Some(mounts_str) = anno.get(ANNO_PROPAGATION_MNTS) {
            let mounts = Utils::anno_to_obj::<Vec<PropagationMount>>(mounts_str)?;
            for mnt in mounts {
                let mut mpath = CPath::new(CUBE_BIND_SHARE_GUEST_BASE_DIR);
                mpath.join(mnt.name.as_str());
                let s = agent::Storage {
                    driver: CUBE_BIND_SHARE_TYPE.to_string(),
                    mount_point: mpath.to_str().unwrap_or_default().to_string(),
                    ..Default::default()
                };
                storages.push(s);
            }
        }

        let shm = agent::Storage {
            driver: "ephemeral".to_string(),
            source: "shm".to_string(),
            fstype: "tmpfs".to_string(),
            options: vec![
                "noexec".to_string(),
                "nosuid".to_string(),
                "nodev".to_string(),
                "mode=1777".to_string(),
                "size=67108864".to_string(),
            ]
            .into(),
            mount_point: GUEST_DEV_SHM.to_string(),
            ..Default::default()
        };
        storages.push(shm);

        //pmem
        for (i, p) in self.conf.pmem.iter().enumerate() {
            if p.placeholder {
                continue;
            }
            //let dev_path = p.guest_device_path(i);
            //let g_mount_point = p.guest_mount_point(i);
            let ps = agent::Storage {
                driver: Pmem::driver(),
                source: Pmem::guest_device_path(i as u32),
                fstype: p.fs_type.clone(),
                mount_point: Pmem::guest_mount_point(i as u32),
                options: vec!["ro".to_string(), "dax".to_string()].into(),
                ..Default::default()
            };

            infof!(self.log, "found pmem:{p:?}");
            storages.push(ps.clone());

           
        }

        //disk
        for (i, d) in self.conf.disk.iter().enumerate() {
            let ds = agent::Storage {
                driver: Disk::driver(),
                source: Disk::guest_device_path(i as u32),
                fstype: d.fs_type.clone(),
                mount_point: Disk::guest_mount_point(i as u32),
                options: d.opt().into(),
                driver_options: d.driver_opt().into(),
                ..Default::default()
            };
            storages.push(ds);
        }

        //vfio disk
        for d in self.conf.vfio_disks.iter() {
            if d.platform {
                let ds = agent::Storage {
                    driver: Disk::driver(),
                    source: d.guest_pci.clone(),
                    fstype: d.fs_type.clone(),
                    mount_point: d.get_mount_point(),
                    need_format: d.need_format,
                    need_resize: d.need_resize,
                    driver_options: d.driver_opt().into(),
                    ..Default::default()
                };
                storages.push(ds);
            }
        }
        Ok(storages)
    }

    fn get_dns(&self) -> CResult<Vec<String>> {
        let anno = match self.spec.annotations().as_ref() {
            Some(anno) => anno,
            None => return Ok(Vec::new()),
        };

        let dns = match anno.get(ANNO_SANDBOX_DNS) {
            Some(dns) => Utils::anno_to_obj::<Vec<String>>(dns)?,
            None => Vec::new(),
        };

        dns.into_iter()
            .map(|entry| normalize_dns_for_agent(&entry))
            .collect()
    }
    async fn reset_guest(&mut self) -> CResult<()> {
        if self.client.is_none() {
            errf!(self.log, "client is None in reset_guest");
            return Err(format!("client is None"));
        }
        let client = self.client.as_ref().unwrap().lock().await;
        let mut stat = self.new_create_stat(stat_defer::CALLEE_ACT_RESET_VM.to_string());
        let tm = Utc::now();

        let req = agent::SetGuestDateTimeRequest {
            Sec: tm.timestamp(),
            Usec: tm.timestamp_subsec_micros() as i64,
            ..Default::default()
        };

        client
            .set_guest_date_time(self.ctx.clone(), &req)
            .await
            .map_err(|e| format!("reset guest time failed:{}", e))?;

        let rng = Utils::get_rng()?;
        let req = agent::ReseedRandomDevRequest {
            data: rng,
            ..Default::default()
        };

        client
            .reseed_random_dev(self.ctx.clone(), &req)
            .await
            .map_err(|e| format!("reset reseed random dev failed:{}", e))?;
        stat.set_ok();
        Ok(())
    }

    pub async fn create_sandbox(&mut self) -> CResult<()> {
        let snapshot = self.start_vm().await?;

        //todo: app snapshot
        if self.conf.notify_snapshot_ret {
            if let Err(e) = AsyncUtils::notify_snapshot_ret(&self.id, snapshot).await {
                warnf!(self.log, "notify restore ret to oss failed:{}", e);
            }
        }

        self.connect_agent().await?;

        infof!(self.log, "agent is ready");

        if snapshot {
            self.reset_guest().await?;
        }

        //add vfio device
        if !self.app_snapshot_restore() {
            self.add_device().await?;
        }

        let storages = self.get_storages()?;
        let dns = self.get_dns()?;
        let mut stat = self.new_create_stat(stat_defer::CALLEE_ACT_CREATE_SANDBOX.to_string());
        let mut req = agent::CreateSandboxRequest {
            //hostname: self.id.clone(),
            hostname: self.id.chars().take(8).collect::<String>(),
            dns: dns.into(),
            storages: storages.into(),
            sandbox_pidns: false,
            sandbox_id: self.id.clone(),
            interfaces: self.conf.net.get_pb_interfaces().into(),
            routes: self.conf.net.get_pb_routes().into(),
            ARPNeighbors: self.conf.net.get_pb_arps().into(),
            cube_vip: self.conf.vips.clone(),
            ..Default::default()
        };

        if snapshot {
            req.cube_preserve_mem_m = self.conf.vm_res.preserve_memory as u32;
        }

        let mut ctx = self.ctx.clone();

        ctx.timeout_nano = 25 * 1000 * 1000 * 1000;
        if self.app_snapshot_create() {
            req.start_mode = protoc::agent::StartMode::SNAPSHOT;
        }

        if self.app_snapshot_restore() {
            req.start_mode = protoc::agent::StartMode::RESTORE;
        }

        {
            if self.client.is_none() {
                errf!(self.log, "client is None in create_sandbox");
                return Err(format!("client is None"));
            }
            let client = self.client.as_ref().unwrap().lock().await;

            client
                .create_sandbox(ctx, &req)
                .await
                .map_err(|e| format!("create sandbox failed:{}", e))?;
        }

        if !self.conf.app_snapshot_create {
            //watch oom
            let (sender, handle) = self.watch_oom().await?;
            self.tx_oom_exited = Some(sender);
            self.oom_handle = Some(Arc::new(handle));

            //monitor guest
            let (sender, handle) = self.monitor_vm(false).await?;
            self.tx_monitor_exited = Some(sender);
            self.monitor_handle = Some(Arc::new(handle));
        }
        stat.set_ok();
        Ok(())
    }

    //this function must be called after CreateSandbox
    async fn monitor_vm(
        &self,
        check_agent: bool,
    ) -> CResult<(Sender<()>, tokio::task::JoinHandle<()>)> {
        let mut arc_ch: Option<Arc<Mutex<CH::CubeHypervisor>>> = self.ch.clone();
        if arc_ch.is_none() {
            panic!("BUG: ch is None");
        }
        let (tx, mut rx) = channel::<()>(1);
        let arc_conainers = self.containers.clone();
        let arc_state = self.state.clone();
        let conn = AsyncUtils::connect_agent(&self.id).await?;
        let client = health_ttrpc::HealthClient::new(conn);
        let log = self.log.clone();
        let handle = tokio::spawn(async move {
            let ctx = context::with_timeout(1000 * 1000 * 1000 * 5);
            let mut aborted = false;
            let interval = 60;
            let mut counter = 0;
            loop {
                {
                    counter += 1;
                    let ch = arc_ch.as_mut().unwrap().lock().await;
                    let req = health::CheckRequest {
                        ..Default::default()
                    };

                    if let Ok(ev) = ch.try_wait_notify() {
                        if ev != CH::NotifyEvent::VmShutdown {
                            warnf!(log, "recv event:{:?}", ev);
                        } else {
                            aborted = true;
                        }
                    }

                    //no event from ch and it's time to ping agent.
                    if check_agent && counter > interval && !aborted {
                        counter = 0;

                        if let Err(e) = client.check(ctx.clone(), &req).await {
                            infof!(log, "check agent failed:{}", e);
                            aborted = true;
                        }
                    }

                    if aborted {
                        {
                            let mut state = arc_state.lock().await;
                            *state = SandBoxState::Exited;
                        }

                        let containers = arc_conainers.lock().await;
                        for (_, container) in containers.iter() {
                            container.notify_vm_shutdown().await;
                        }
                        break;
                    }
                }
                if let Ok(_) = rx.try_recv() {
                    return;
                }
                sleep(Duration::from_millis(1000)).await;
            }
        });

        Ok((tx, handle))
    }

    pub async fn watch_oom(&self) -> CResult<(Sender<()>, tokio::task::JoinHandle<()>)> {
        if self.client.is_none() {
            errf!(self.log, "client is None in watch_oom");
            return Err(format!("client is None"));
        }
        let client = self.client.as_ref().unwrap().lock().await.clone();
        let tx_containerd = self.tx_containerd.clone();
        let log = self.log.clone();
        let containers = self.containers.clone();
        let (tx, mut rx) = channel::<()>(1);
        let handle = tokio::spawn(async move {
            let req = agent::GetOOMEventRequest::default();
            loop {
                tokio::select! {
                    _ = rx.recv() => {
                        return;
                    }
                    res = client.get_oom_event(context::Context::default(), &req) => {
                        match res {
                            Ok(rsp) => {
                        let real_id = {
                            let mut id = rsp.container_id.clone();
                            let cs = containers.lock().await;
                            for (real_id, c) in cs.iter() {
                                if c.get_id() == rsp.container_id {
                                    id = real_id.clone();
                                    break;
                                }
                            }
                            id
                        };

                                let event = TaskOOM {
                                    container_id: real_id,
                                    ..Default::default()
                                };
                                let topic = event.topic();
                                let _ = tx_containerd.try_send((topic, Box::new(event)));
                            }
                            Err(e) => {
                                errf!(log, "watch oom failed:{}", e);
                                break;
                            }
                        }
                    }
                }
            }
        });
        Ok((tx, handle))
    }

    pub async fn is_empty(&self) -> bool {
        let containers = self.containers.lock().await;
        containers.is_empty()
    }

    pub async fn destroy_sandbox(&mut self) -> CResult<()> {
        infof!(self.log, "destroy sandbox start");
        let req = agent::DestroySandboxRequest {
            ..Default::default()
        };

        if self.ch.is_none() {
            infof!(self.log, "ch instance is None");
            return Ok(());
        }
        let mut ch = self.ch.as_mut().unwrap().lock().await;
        //In the context of this 'ch' lock, check whether the VmShutdown event has been received
        {
            let state = self.state.lock().await;
            if *state == SandBoxState::Exited {
                infof!(self.log, "vm has exited");
                return Ok(());
            }
        }
        if self.client.is_none() {
            infof!(self.log, "client is None");
            return Ok(());
        }
        let client = self.client.as_ref().unwrap().lock().await;

        if let Err(e) = client
            .destroy_sandbox(context::with_timeout(1000 * 1000 * 200), &req)
            .await
        {
            //perhaps the VM has already shutdown.(eg:panic/cube-agent exited/...)
            warnf!(self.log, "destroy sandbox failed:{}, but nothing to do", e)
        }

        infof!(self.log, "wait vm shutdown");

        //wait for the vm shutdown gracefully
        loop {
            match ch.wait_notify(Duration::from_millis(1000)).await {
                Ok(ev) => {
                    if CH::NotifyEvent::VmShutdown != ev {
                        warnf!(
                            self.log,
                            "Not an expected event, expected:{:?}, actual:{:?}",
                            CH::NotifyEvent::VmShutdown,
                            ev
                        );
                        continue;
                    }
                    break;
                }

                //we are in the process of destruction, so the results here are not important.
                Err(e) => {
                    warnf!(
                        self.log,
                        "wait vm shutdown event failed:{}, but nothing to do",
                        e
                    );
                    break;
                }
            }
        }
        infof!(self.log, "wait ch exit");
        if let Err(e) = ch.join().await {
            warnf!(self.log, "join ch failed:{}, but nothing to do", e);
        }
        infof!(self.log, "destroy sandbox finish");
        Ok(())
    }

    pub async fn prepare_resource(&mut self) -> CResult<VmConfig> {
        let mut vc = VmConfig::default();
        vc.set_kernel(self.conf.kernel.clone())
            .set_vcpus(self.conf.vm_res.cpu)
            .set_memory(self.conf.vm_res.memory, false)
            .add_nets(&self.conf.net)?
            .add_disks(&self.conf.disk)
            .add_virtiofs(&self.conf.virtiofs)
            .add_vsock(self.id.clone());

        if let Some(fs) = self.conf.fs.as_ref() {
            vc.add_fs(fs);
        }
        if self.conf.app_snapshot_create {
            vc.set_memory(self.conf.vm_res.memory, true);
        }

        if self.debug {
            vc.add_cmdline("agent.log=debug".to_string());
        } else {
            vc.add_cmdline("quiet".to_string());
        }
        vc.add_cmdline("highres=off".to_string());
        vc.add_cmdline("clocksource=kvm-clock".to_string());

        // 添加外部传入的 pmem
        vc.add_pmems(&self.conf.pmem);

        // 检查额外内核参数是否与已有参数冲突
        if !self.conf.extra_kernel_params.is_empty() {
            let conflicts = vc.check_cmdline_conflicts(&self.conf.extra_kernel_params);
            if !conflicts.is_empty() {
                return Err(format!(
                    "kernel parameter conflicts detected, cannot create container: {}",
                    conflicts.join("; ")
                ));
            }
        }

        for param in self.conf.extra_kernel_params.iter() {
            vc.add_cmdline(param.clone());
        }

        Ok(vc)
    }

    pub async fn recycle_resource(&mut self) -> CResult<()> {
        Ok(())
    }

    fn by_snapshot(&self) -> bool {
        let anno = self.spec.annotations().as_ref().unwrap();
        if anno.contains_key(config::ANNO_SNAPSHOT_DISABLE) {
            return false;
        }

        if let Some(proc) = self.spec.process() {
            if proc.selinux_label().is_some() && !proc.selinux_label().clone().unwrap().is_empty() {
                return false;
            }
        }
        if !enable_snapshot() {
            return false;
        }

        !self.conf.app_snapshot_create
    }
    async fn start_vm(&mut self) -> CResult<bool> {
        infof!(self.log, "start vm start");
        {
            let mut ch = self.ch.as_mut().unwrap().lock().await;
            ch.launch_vmm().await?;
        }
        let mut snapshot = false;

        if self.by_snapshot() {
            match self.restore_vm().await {
                Ok(_) => {
                    snapshot = true;
                    if self.conf.app_snapshot_restore {
                        return Ok(snapshot);
                    }
                }
                Err(e) => {
                    errf!(self.log, "restore vm failed:{}", e);
                    if self.conf.app_snapshot_restore {
                        return Err(format!("app snapshot restore vm failed:{}", e));
                    }
                }
            }
        }

        if !snapshot {
            self.boot_vm().await?;
        }

        {
            let ch = self.ch.as_mut().unwrap().lock().await;
            let start = Instant::now();
            let ev = ch
                .wait_notify(Duration::from_nanos(1000 * 1000 * 1000 * 60 as u64))
                .await?;

            if CH::NotifyEvent::VsockServerReady != ev {
                return Err(format!(
                    "Not an expected event, expected:{:?}, actual:{:?}",
                    CH::NotifyEvent::VsockServerReady,
                    ev
                ));
            }
            let duration = start.elapsed().as_millis();
            infof!(self.log, "vm ready, vsock is listening, cost:{}", duration);
        }
        Ok(snapshot)
    }

    async fn boot_vm(&mut self) -> CResult<()> {
        let config = self.prepare_resource().await?;
        let mut ch = self.ch.as_mut().unwrap().lock().await;
        ch.create_vm(&config).await?;
        ch.boot_vm().await?;
        Ok(())
    }

    async fn restore_vm(&mut self) -> CResult<()> {
        let ss_file = SnapshotInfo::load(
            self.conf.snapshot_base.as_str(),
            self.conf.vm_res.cpu,
            self.conf.vm_res.snap_memory,
        )?;

        let mut ss_req = SnapshotInfo::new(self.conf.vm_res.cpu, self.conf.vm_res.snap_memory);
        ss_req.set_image_version()?;
        ss_req.set_kernel_version(self.conf.kernel.as_str())?;
        ss_req.set_disks(&self.conf.disk);

        let align_pmem = ss_file.align_pmems(&self.conf.pmem);
        ss_req.set_pmems(&align_pmem);

        //ss_file must be treated as a self object.
        ss_file
            .eq(&ss_req)
            .map_err(|e| format!("snapshot metadata not match:{}", e))?;

        let snapshot = Utils::get_snapshot_dir(
            self.conf.snapshot_base.as_str(),
            self.conf.vm_res.cpu,
            self.conf.vm_res.snap_memory,
        );
        infof!(self.log, "snapshot dir:{}", snapshot.clone());
        let mut fss = vec![];
        if let Some(fs) = self.conf.fs.as_ref() {
            let f = Utils::restore_fs_configs(fs);
            fss.push(f);
        }
        fss.extend(Utils::restore_virtiofs_configs(&self.conf.virtiofs));
        let nets = Utils::restore_nets_config(&self.conf.net.interfaces)?;
        let disks = Utils::restore_disks_config(&self.conf.disk);
        let pmems = Utils::restore_pmems_config(&self.conf.pmem);
        let vsock = Utils::gen_vsock_config(&self.id);

        let ch = self.ch.as_mut().unwrap().lock().await;
        let config = RestoreConfig {
            source_url: PathBuf::from(snapshot),
            fs: Some(fss),
            net: Some(nets),
            disks: Some(disks),
            pmem: Some(pmems),
            vsock: Some(vsock),
            ..Default::default()
        };

        ch.restore_vm(config).await?;
        /*
        let ev = ch
            .wait_notify(Duration::from_nanos(self.ctx.timeout_nano as u64))
            .await?;
        if CH::NotifyEvent::RestoreReady != ev {
            return Err(format!(
                "Not an expected event, expected:{:?}, actual:{:?}",
                CH::NotifyEvent::RestoreReady,
                ev
            ));
        }*/

        //update pmem seq, must occur after successful restore
        self.conf.pmem = align_pmem;
        let mut pmem_path_map = HashMap::new();
        for (i, p) in self.conf.pmem.iter().enumerate() {
            pmem_path_map.insert(p.file.clone(), i as u32);
        }
        self.conf.pmem_path_map = pmem_path_map;
        Ok(())
    }

    pub async fn create_container(
        &mut self,
        id: String,
        spec: Spec,
        info: ContainerInfo,
    ) -> CResult<()> {
        let mut containers = self.containers.lock().await;
        if containers.contains_key(&id) {
            return Err(format!("container {} already exists", id.clone()));
        }
        let client = self.client.as_ref().unwrap();
        let mut c: Container = Container::new(
            self.id.clone(),
            id.clone(),
            spec,
            client.clone(),
            self.log.clone(),
            self.conf.clone(),
            info,
            self.tx_containerd.clone(),
            self.app_snapshot_create(),
        )?;
        c.create_container().await?;
        containers.insert(id, c);

        Ok(())
    }

    pub async fn start_container(&mut self, id: &String) -> Result<()> {
        let mut containers = self.containers.lock().await;
        let container = match containers.get_mut(id) {
            Some(c) => c,
            None => return Err(Error::NotFoundError(format!("not found container:{}", id))),
        };
        container
            .start_container()
            .await
            .map_err(|e| Error::Other(e.to_string()))?;
        Ok(())
    }

    pub async fn kill_container(&self, id: &String, exec_id: &String, sig: u32) -> Result<()> {
        let mut containers = self.containers.lock().await;
        let container = containers.get_mut(id);
        if container.is_none() {
            return Err(Error::NotFoundError(format!("not found container:{}", id)));
        }

        container.unwrap().signal_container(exec_id, sig).await
    }

    pub async fn delete_container(&mut self, id: &String) -> Result<(u32, DateTime<Utc>)> {
        let mut container = {
            let mut containers = self.containers.lock().await;
            match containers.get_mut(id) {
                Some(c) => c.clone(),
                None => return Err(Error::NotFoundError(format!("not found container:{}", id))),
            }
        };
        let (code, tm) = container
            .destroy_container()
            .await
            .map_err(|e| Error::Other(format!("{}", e)))?;

        let mut containers = self.containers.lock().await;
        if containers.remove(id).is_none() {
            warnf!(self.log, "remove container:{} failed from map", id);
        }
        Ok((code, tm))
    }

    pub fn pid(&self) -> u32 {
        self.pid
    }

    pub async fn get_container_info(&self, id: &String, exec_id: &String) -> Result<ContainerInfo> {
        let containers = self.containers.lock().await;
        let ci = match containers.get(id) {
            Some(c) => c.get_container_info(exec_id).await,
            None => return Err(Error::NotFoundError(format!("not found container:{}", id))),
        };
        ci
    }

    pub async fn wait_container(&self, id: &String, exec_id: &str) -> Result<(u32, DateTime<Utc>)> {
        let mut cid = exec_id.to_owned();
        if cid.is_empty() {
            cid = id.clone();
        }
        let mut container = {
            let mut containers = self.containers.lock().await;
            match containers.get_mut(id) {
                Some(c) => c.clone(),
                None => return Err(Error::NotFoundError(format!("not found container:{}", id))),
            }
        };
        container.wait_container(&cid).await
    }

    pub async fn exec_container(
        &self,
        id: &String,
        exec_id: &String,
        tty: Tty,
        proc: Process,
    ) -> Result<()> {
        let mut containers = self.containers.lock().await;
        if let Some(c) = containers.get_mut(id) {
            return c
                .create_exec(exec_id, tty, proc)
                .await
                .map_err(|e| Error::Other(e.to_string()));
        }
        Err(Error::NotFoundError(format!("not found container:{}", id)))
    }

    pub async fn start_exec(&self, id: &String, exec_id: &String) -> Result<()> {
        let mut containers = self.containers.lock().await;
        if let Some(c) = containers.get_mut(id) {
            return c.start_exec(exec_id).await;
        }
        Err(Error::NotFoundError(format!("not found container:{}", id)))
    }

    pub async fn delete_exec(
        &mut self,
        id: &String,
        exec_id: &String,
    ) -> Result<(u32, DateTime<Utc>)> {
        let mut containers = self.containers.lock().await;
        if let Some(c) = containers.get_mut(id) {
            return c
                .destroy_exec(exec_id)
                .await
                .map_err(|e| Error::Other(e.to_string()));
        }
        Err(Error::NotFoundError(format!("not found container:{}", id)))
    }
    pub async fn update_container(&mut self, id: &String, res: &LinuxResources) -> Result<()> {
        let mut containers = self.containers.lock().await;
        if let Some(c) = containers.get_mut(id) {
            return c.update(res).await.map_err(|e| Error::Other(e.to_string()));
        }
        Err(Error::NotFoundError(format!("not found container:{}", id)))
    }

    pub async fn update_sandbox(&mut self, annotation: &HashMap<String, String>) -> CResult<()> {
        let ch = self.ch.as_ref().unwrap().lock().await;

        //set fs
        let anno = annotation.get(ANNO_VMM_FS);
        if let Some(anno) = anno {
            let fs = Utils::anno_to_obj::<Fs>(anno)?;

            let mut fs_config = FsConfig {
                id: Some(VIRTIO_FS_ID.to_string()),
                tag: VIRTIO_FS_TAG.to_string(),
                backendfs_config: fs.backendfs_config.clone(),
                ..Default::default()
            };
            //shit:merge the two allow dirs
            //todo:test
            if fs_config.backendfs_config.is_some()
                && fs_config
                    .backendfs_config
                    .as_ref()
                    .unwrap()
                    .allowed_dirs
                    .is_none()
            {
                let mut merge = HashSet::new();
                merge.extend(
                    fs_config
                        .backendfs_config
                        .as_ref()
                        .unwrap()
                        .allowed_dirs
                        .clone()
                        .unwrap(),
                );
                if self.conf.fs.is_some()
                    && self.conf.fs.as_mut().unwrap().backendfs_config.is_some()
                    && self
                        .conf
                        .fs
                        .as_mut()
                        .unwrap()
                        .backendfs_config
                        .as_ref()
                        .unwrap()
                        .allowed_dirs
                        .is_some()
                {
                    merge.extend(
                        self.conf
                            .fs
                            .as_mut()
                            .unwrap()
                            .backendfs_config
                            .as_ref()
                            .unwrap()
                            .allowed_dirs
                            .clone()
                            .unwrap(),
                    );
                }
                fs_config.backendfs_config.as_mut().unwrap().allowed_dirs =
                    Some(merge.into_iter().collect());
            }
            ch.set_fs(fs_config).await?;
        }

        //add disk
        if let Some(anno) = annotation.get(device::ANNO_VFIO_DISK) {
            let devs = Utils::anno_to_obj::<Vec<device::Device>>(anno)?;
            for dev in &devs {
                let config = DeviceConfig {
                    path: PathBuf::from(dev.sysfs_dev.clone()),
                    id: Some(dev.id.clone()),
                    ..Default::default()
                };
                ch.add_dev(config).await?;
            }
        }

        //remove disk
        if let Some(anno) = annotation.get(device::ANNO_VFIO_DISK_RM) {
            let devs = Utils::anno_to_obj::<Vec<device::Device>>(anno)?;
            for dev in &devs {
                let config = VmRemoveDeviceData { id: dev.id.clone() };
                ch.remove_dev(config).await?;
            }
        }

        Ok(())
    }

    async fn add_device(&mut self) -> CResult<()> {
        let ch = self.ch.as_mut().unwrap().lock().await;

        for dev in &self.conf.vfio_nets {
            let config = DeviceConfig {
                path: PathBuf::from(dev.sysfs_dev.clone()),
                id: Some(dev.id.clone()),
                ..Default::default()
            };
            ch.add_dev(config).await?;
        }

        for dev in &mut self.conf.vfio_disks {
            let config = DeviceConfig {
                path: PathBuf::from(dev.sysfs_dev.clone()),
                id: Some(dev.id.clone()),
                ..Default::default()
            };
            dev.guest_pci = ch.add_dev(config).await?;
        }

        Ok(())
    }

    pub async fn create_snapshot(&self, snapshot_path: &str) -> CResult<()> {
        let ch = self.ch.as_ref().unwrap().lock().await;
        ch.pause_vm().await?;
        ch.snapshot_vm(format!("file://{}", snapshot_path).as_str())
            .await?;
        ch.resume_vm().await
    }
    pub async fn paused(&self) -> bool {
        let state = self.state.lock().await;
        *state == SandBoxState::Paused
    }

    pub async fn normal(&self) -> bool {
        let state = self.state.lock().await;
        *state == SandBoxState::Normal
    }

    pub async fn pause_vm(&mut self) -> CResult<()> {
        {
            let mut state = self.state.lock().await;
            if *state != SandBoxState::Normal {
                return Err(format!("sandbox not running").into());
            };

            if self.pause_vm_forbidding().await {
                return Err(format!("sandbox pause forbidding, terminate exec tasks first").into());
            }
            *state = SandBoxState::Paused;
        }

        self.disconnect_agent().await?;

        let ch = self.ch.as_mut().unwrap().lock().await;

        let snapshot_path = format!("{}/{}", PAUSE_VM_SNAPSHOT_BASE, self.id);
        let _ = std::fs::remove_dir_all(snapshot_path.clone());
        if let Err(e) = std::fs::create_dir_all(snapshot_path.clone()) {
            return Err(format!("mkdir snapshot dir failed:{}", e).into());
        }

        ch.pause_vm_cube(format!("file://{}", snapshot_path).as_str())
            .await?;

        //vmshutdown event
        let _ = ch
            .wait_notify(Duration::from_nanos(self.ctx.timeout_nano as u64))
            .await?;

        Ok(())
    }

    pub async fn resume_vm(&mut self) -> CResult<()> {
        //modify state
        {
            let mut state = self.state.lock().await;
            if *state != SandBoxState::Paused {
                return Err(format!("sandbox not paused").into());
            };
            *state = SandBoxState::Normal;
        }
        //resume vm
        {
            let ch = self.ch.as_mut().unwrap().lock().await;
            let resume_path = format!("{}/{}", PAUSE_VM_SNAPSHOT_BASE, self.id);
            ch.resume_vm_cube(format!("file://{}", resume_path).as_str())
                .await?;
        }

        self.connect_agent().await?;

        if self.client.is_none() {
            errf!(self.log, "client is None in resume_vm");
            return Err(format!("client is None"));
        }

        self.reset_guest().await?;

        let client = self.client.as_ref().unwrap();

        let mut containers = self.containers.lock().await;
        for (_, c) in containers.iter_mut() {
            c.set_client(client.clone()).await;
        }

        let (sender, handle) = self.watch_oom().await?;
        self.tx_oom_exited = Some(sender);
        self.oom_handle = Some(Arc::new(handle));

        //monitor guest
        let (sender, handle) = self.monitor_vm(false).await?;
        self.tx_monitor_exited = Some(sender);
        self.monitor_handle = Some(Arc::new(handle));

        Ok(())
    }
}

fn normalize_dns_for_agent(entry: &str) -> CResult<String> {
    let trimmed = entry.trim();
    if trimmed.is_empty() {
        return Err("dns entry is empty".to_string());
    }

    if let Some(ip) = trimmed.strip_prefix("nameserver ") {
        let ip = ip.trim();
        ip.parse::<IpAddr>()
            .map_err(|_| format!("invalid dns ip {}", entry))?;
        return Ok(format!("nameserver {}", ip));
    }

    let ip = trimmed
        .parse::<IpAddr>()
        .map_err(|_| format!("invalid dns ip {}", entry))?;
    Ok(format!("nameserver {}", ip))
}

#[cfg(test)]
mod tests {
    use protobuf::MessageDyn;
    use std::collections::HashSet;
    use tokio::sync::mpsc::channel;

    use crate::common::PRODUCT_CUBEBOX;

    use super::normalize_dns_for_agent;
    use super::Log;
    use super::SandBox;

    #[tokio::test]
    async fn test_sandbox_prepare_resource() {
        let log = Log::default();
        let (tx, _) = channel::<(String, Box<dyn MessageDyn>)>(128);
        let mut sb = SandBox::new("ut".to_string(), log, false, tx);
        sb.conf.kernel = "ut_kernel".to_string();
        sb.conf.vm_res.cpu = 999;
        sb.conf.vm_res.memory = 999;
        sb.conf.product = PRODUCT_CUBEBOX.to_string();
        sb.conf.extra_kernel_params = vec![
            "custom.param=42".to_string(),
            "another.param=foo".to_string(),
        ];

        let vmconfig = sb.prepare_resource().await;
        assert!(vmconfig.is_ok());
        let vm_config = vmconfig.unwrap();

        assert_eq!(vm_config.vcpus, 999);
        assert_eq!(vm_config.memory_size, 999);
        assert_eq!(vm_config.kernel, "ut_kernel".to_string());
        assert!(vm_config.cmdlines.contains(&"custom.param=42".to_string()));
        assert!(vm_config
            .cmdlines
            .contains(&"another.param=foo".to_string()));

        let mut set_expect: HashSet<String> = vec!["highres=off", "clocksource=kvm-clock"]
            .into_iter()
            .map(|s| s.to_string())
            .collect();
        let set_unexpect: HashSet<String> = vec!["clocksource=tsc", "tsc=reliable"]
            .into_iter()
            .map(|s| s.to_string())
            .collect();

        for cmd in vm_config.cmdlines.iter() {
            assert!(!set_unexpect.contains(cmd), "unexpect {} in cmdlines", cmd);
            if set_expect.contains(cmd) {
                set_expect.remove(cmd);
            }
        }
        assert!(
            set_expect.is_empty(),
            "missing expected cmdlines: {:?}",
            set_expect
        );
    }

    #[test]
    fn test_normalize_dns_for_agent_accepts_raw_ip() {
        let got = normalize_dns_for_agent(" 119.29.29.29 ").unwrap();
        assert_eq!(got, "nameserver 119.29.29.29");
    }

    #[test]
    fn test_normalize_dns_for_agent_accepts_prefixed_entry() {
        let got = normalize_dns_for_agent(" nameserver 8.8.8.8 ").unwrap();
        assert_eq!(got, "nameserver 8.8.8.8");
    }
}

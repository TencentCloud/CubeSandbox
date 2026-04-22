// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

pub mod container_mgr;
pub mod exec;
pub mod rootfs;
use crate::common::types::PropagationContainerMount;
use crate::common::utils::{AsyncUtils, CPath, Utils};
use crate::common::{
    self, CResult, ANNO_PROPAGATION_CONTAINER_MNTS, CUBE_BIND_SHARE_GUEST_BASE_DIR,
    CUBE_BIND_SHARE_TYPE, MOUNT_TYPE_BIND, MOUNT_TYPE_RBIND,
};
use crate::container::rootfs::ANNO_CONTAINER_CUSTOM_FILE;
use crate::log::{stat_defer, stat_defer::StatDefer, Log};
use crate::sandbox::config::{Config, ANNO_APP_SNAPSHOT_CREATE};
use crate::{infof, warnf};
use agent::CustomFile;
use chrono::{DateTime, Utc};
use container_mgr::{ContainerInfo, ContainerState, TaskState};
use containerd_shim::protos::protobuf::MessageDyn;
use containerd_shim::{Error, Result};
use exec::{Exec, Tty};
use oci_spec::runtime::{LinuxResources, Mount, Process, Spec};

use protoc::{agent, agent_ttrpc, oci};
use tokio::sync::mpsc::Sender;

use serde_json;
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;

use tokio::sync::Mutex;
use ttrpc::context::{self, Context};

pub const GUEST_DEV_SHM: &str = "/run/cube-containers/sandbox/shm";
pub const ANNO_APP_SNAPSHOT_CONTAINER_ID: &str = "cube.appsnapshot.container.id";
#[derive(Clone)]
pub struct Container {
    sandbox_id: String,
    id: String,
    real_id: String,
    spec: Spec,
    client: Option<Arc<Mutex<agent_ttrpc::AgentServiceClient>>>,
    ctx: Context,
    log: Log,
    sb_conf: Config,
    info: ContainerInfo,
    state: Option<ContainerState>,
    tx_containerd: Sender<(String, Box<dyn MessageDyn>)>,
    execs: Arc<Mutex<HashMap<String, Exec>>>,
    app_snapshot: bool,
}

impl Container {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        sandbox_id: String,
        real_id: String,
        spec: Spec,
        client: Arc<Mutex<agent_ttrpc::AgentServiceClient>>,
        log: Log,
        sb_conf: Config,
        info: ContainerInfo,
        tx_containerd: Sender<(String, Box<dyn MessageDyn>)>,
        app_snapshot: bool,
    ) -> CResult<Self> {
        let mut id = real_id.clone();
        if let Some(annos) = spec.annotations().as_ref() {
            if let Some(cid) = annos.get(ANNO_APP_SNAPSHOT_CONTAINER_ID) {
                id = cid.clone();
                if sb_conf.app_snapshot_create {
                    return Err(format!(
                        "{} conflicts with {}",
                        ANNO_APP_SNAPSHOT_CONTAINER_ID, ANNO_APP_SNAPSHOT_CREATE
                    ));
                }
            }
        }
        let c = Container {
            sandbox_id,
            id,
            real_id,
            spec,
            client: Some(client),
            log,
            sb_conf,
            info,
            ctx: context::with_timeout(1000 * 1000 * 1000 * 10),
            state: None,
            execs: Arc::new(Mutex::new(HashMap::new())),
            tx_containerd,
            app_snapshot,
        };
        Ok(c)
    }

    pub async fn pause_vm_forbidding(&self) -> bool {
        let execs = self.execs.lock().await;
        if execs.is_empty() {
            return false;
        }
        true
    }

    pub async fn set_client(&mut self, client: Arc<Mutex<agent_ttrpc::AgentServiceClient>>) {
        self.client = Some(client.clone());
        self.state = Some(ContainerState::new(self.log.clone()));
        let cli = self.client.as_ref().unwrap().lock().await;

        let state = self.state.as_mut().unwrap();
        let client_wait = cli.clone();
        let cid = self.id.clone();
        let real_id = self.real_id.clone();
        let tx_containerd = self.tx_containerd.clone();

        state
            .wait_process(client_wait, cid, real_id, String::new(), tx_containerd)
            .await;
    }

    pub async fn unset_client(&mut self) {
        //terminate the wait req
        if self.state.is_some() {
            self.state.as_ref().unwrap().notify_vm_pause().await;
            self.state = None;
        }
        self.client = None;
    }

    fn get_storages(&mut self) -> CResult<Vec<agent::Storage>> {
        let mut storages = Vec::new();
        let spec = self.spec.clone();
        let mounts = self.spec.mounts_mut().as_mut().unwrap();
        //bind-share
        for m in mounts.iter_mut() {
            if let Some(t) = m.typ() {
                if t == CUBE_BIND_SHARE_TYPE {
                    let mut source = CPath::new(CUBE_BIND_SHARE_GUEST_BASE_DIR);

                    source.join(
                        m.source()
                            .clone()
                            .unwrap_or(PathBuf::new())
                            .to_str()
                            .unwrap_or(""),
                    );
                    m.set_source(Some(source.to_path_buf()));
                    m.set_typ(Some(common::MOUNT_TYPE_BIND.to_string()));

                    let s = agent::Storage {
                        driver: CUBE_BIND_SHARE_TYPE.to_string(),
                        mount_point: source.to_str().unwrap_or("").to_string(),
                        ..Default::default()
                    };
                    storages.push(s);
                    continue;
                }
            }

            if let Some(src) = m.source() {
                if let Some(i) = self.sb_conf.disk_path_map.get(src.to_str().unwrap()) {
                    let index = *i as usize;
                    let disk = self.sb_conf.disk.get(index).unwrap_or_else(|| {
                        panic!("BUG: sandbox.conf.disk, invalid index:{}", index)
                    });
                    let src = disk.guest_bind_source(*i, m.options());
                    m.set_typ(Some(common::MOUNT_TYPE_BIND.to_string()));
                    m.set_source(Some(PathBuf::from(src)));
                    continue;
                }

                if let Some(i) = self.sb_conf.pmem_path_map.get(src.to_str().unwrap()) {
                    let index = *i as usize;
                    let pmem = self.sb_conf.pmem.get(index).unwrap_or_else(|| {
                        panic!("BUG: sandbox.conf.pmem, invalid index:{}", index)
                    });
                    let src = pmem.guest_bind_source(*i);
                    m.set_typ(Some(common::MOUNT_TYPE_BIND.to_string()));
                    m.set_source(Some(PathBuf::from(src)));
                    continue;
                }

                if let Some(i) = self.sb_conf.vfio_disk_path_map.get(src.to_str().unwrap()) {
                    let index = *i as usize;
                    let vfio_disk = self.sb_conf.vfio_disks.get(index).unwrap_or_else(|| {
                        panic!("BUG: sandbox.conf.vfio_disks, invalid index:{}", index)
                    });
                    let src = vfio_disk.guest_pci_source(m.options());
                    m.set_typ(Some(common::MOUNT_TYPE_BIND.to_string()));
                    m.set_source(Some(PathBuf::from(src)));
                    continue;
                }
            }
        }

        let anno = spec.annotations().as_ref().unwrap();
        if let Some(mount_str) = anno.get(ANNO_PROPAGATION_CONTAINER_MNTS) {
            let pmounts = Utils::anno_to_obj::<Vec<PropagationContainerMount>>(mount_str)?;
            for mnt in pmounts {
                let mut m = Mount::default();
                m.set_typ(Some(common::MOUNT_TYPE_BIND.to_string()));
                m.set_destination(PathBuf::from(mnt.container_dir.clone()));
                let mut mpath = CPath::new(CUBE_BIND_SHARE_GUEST_BASE_DIR);
                mpath.join(mnt.name.as_str());
                m.set_source(Some(mpath.to_path_buf()));
                //propagation-mnt: Tell the agent not to overwrite the mountpoint if the path already exists
                m.set_options(Some(vec![
                    "propagation-mnt".to_string(),
                    "bind".to_string(),
                    "rslave".to_string(),
                ]));
                mounts.push(m);
            }
        }

        Ok(storages)
    }

    fn get_pb_spec(&mut self) -> CResult<oci::Spec> {
        let json_str = serde_json::to_string(&self.spec)
            .map_err(|e| format!("serialize spec failed:{}", e))?;

        let mut spec: oci::Spec = serde_json::from_str(&json_str)
            .map_err(|e| format!("deserialize spec failed:{}", e))?;

        let proc = spec.mut_process();
        proc.set_noNewPrivileges(false);
        proc.set_selinuxLabel(String::new());

        let res = spec.mut_linux().mut_resources();
        res.clear_devices();
        res.clear_pids();
        res.clear_blockIO();

        res.mut_cpu().clear_cpus();
        res.mut_cpu().clear_mems();

        let mut nss = Vec::new();
        for ns in spec.get_linux().get_namespaces() {
            if ns.field_type == common::NS_CGROUP
                || ns.field_type == common::NS_NET
                || ns.field_type == common::NS_PID
            {
                continue;
            }
            let mut n = ns.clone();
            n.set_path(String::new());
            nss.push(n);
        }
        spec.mut_linux().set_namespaces(nss.into());

        //rootfs is writeable
        let anno = spec.mut_annotations();
        if let Some(path) = anno.get(common::ANNO_ROOTFS_WLAYER_PATH) {
            let subdir = anno.get(common::ANNO_ROOTFS_WLAYER_PATH_SUBDIR);
            if let Some(i) = self.sb_conf.disk_path_map.get(path) {
                let index = *i as usize;
                let disk =
                    self.sb_conf.disk.get(index).unwrap_or_else(|| {
                        panic!("BUG: sandbox.conf.disk, invalid index:{}", index)
                    });

                let src = if let Some(subdir) = subdir {
                    disk.guest_bind_source_with_subdir(*i, &None, subdir.clone())
                } else {
                    disk.guest_bind_source(*i, &None)
                };
                anno.insert(common::ANNO_ROOTFS_WLAYER_PATH.to_string(), src);
            } else {
                // cbs系统盘
                if let Some(i) = self.sb_conf.vfio_disk_path_map.get(path).cloned() {
                    let disk = self.sb_conf.vfio_disks.get(i as usize).unwrap_or_else(|| {
                        panic!("BUG: sandbox.conf.vfio_disks, invalid index:{}", i)
                    });
                    if disk.platform {
                        let src = if let Some(subdir) = subdir {
                            disk.guest_pci_source_with_subdir(&None, subdir.clone())
                        } else {
                            disk.guest_pci_source(&None)
                        };

                        anno.insert(common::ANNO_ROOTFS_WLAYER_PATH.to_string(), src);
                    }
                }
            }
        }

        //rootfs by pmem
        if let Some(pmem_rootfs) = anno.get(rootfs::ANNOTATION_K_ROOTFS_INFO) {
            let mut rootfs = rootfs::RootfsInfo::new(pmem_rootfs)?;

            if self.app_snapshot {
                if rootfs.overlay_info.is_some() || rootfs.mounts.is_some() {
                    rootfs.overlay_info = None;
                    rootfs.mounts = None;
                    //return Err("Overlay info and mounts is not supported in app snapshot".to_string());
                }
            }

            if let Some(pmem_file) = rootfs.pmem_file.clone() {
                if let Some(i) = self.sb_conf.pmem_path_map.get(&pmem_file) {
                    let index = *i as usize;
                    let pmem = self.sb_conf.pmem.get(index).unwrap_or_else(|| {
                        panic!("BUG: sandbox.conf.pmem, invalid index:{}", index)
                    });
                    let src = pmem.guest_bind_source(*i);
                    rootfs.pmem_file = Some(src);
                }
            } else if let Some(ero_image) = rootfs.ero_image.as_mut() {
                if let Some(i) = self.sb_conf.disk_path_map.get(&ero_image.path) {
                    let index = *i as usize;
                    let disk = self.sb_conf.disk.get(index).unwrap_or_else(|| {
                        panic!("BUG: sandbox.conf.pmem, invalid index:{}", index)
                    });
                    ero_image.path = disk.guest_bind_source(*i, &None);
                }
            }
            rootfs.fix_virtiofs();
            let rootfs_str = serde_json::to_string(&rootfs)
                .map_err(|e| format!("Serialize rootfs failed:{}", e))?;
            anno.insert(rootfs::ANNOTATION_K_ROOTFS_INFO.to_string(), rootfs_str);
        }

        for m in spec.mut_mounts().iter_mut() {
            if m.get_destination() == "/dev/shm" {
                m.set_source(GUEST_DEV_SHM.to_string());
                m.set_field_type(MOUNT_TYPE_BIND.to_string());
                m.set_options(vec![MOUNT_TYPE_RBIND.to_string()].into());
                break;
            }
        }
        Ok(spec)
    }

    fn get_custom_files(&mut self) -> CResult<Vec<CustomFile>> {
        if self.spec.annotations().is_none() {
            return Ok(Vec::<CustomFile>::new());
        }

        let data = self
            .spec
            .annotations()
            .as_ref()
            .unwrap()
            .get(ANNO_CONTAINER_CUSTOM_FILE);
        if data.is_none() {
            return Ok(Vec::<CustomFile>::new());
        }

        let data = data.unwrap();
        let files = serde_json::from_str::<Vec<CustomFile>>(data)
            .map_err(|e| format!("deserialize custom file failed:{}", e))?;

        Ok(files)
    }

    fn new_stat(&self, callee_act: String) -> StatDefer {
        stat_defer::StatDefer::new(
            self.real_id.clone(),
            stat_defer::CALLEE_AGENT.to_string(),
            stat_defer::ACT_CREATE.to_string(),
            callee_act,
            self.log.clone(),
        )
    }

    fn is_cold_start(&self) -> bool {
        self.id == self.real_id
    }

    pub async fn create_container(&mut self) -> CResult<()> {
        let mut stat = self.new_stat(stat_defer::CALLEE_ACT_CREATE_CONTAINER.to_string());
        let req = agent::CreateContainerRequest {
            container_id: self.id.clone(),
            exec_id: self.id.clone(),
            storages: self.get_storages()?.into(),
            OCI: Some(self.get_pb_spec()?).into(),
            custom_files: self.get_custom_files()?.into(),
            ..Default::default()
        };

        let client = self.client.as_ref().unwrap().lock().await;

        client
            .create_container(self.ctx.clone(), &req)
            .await
            .map_err(|e: ttrpc::Error| format!("create container failed:{}", e))?;

        self.state = Some(ContainerState::new(self.log.clone()));
        stat.set_ok();
        Ok(())
    }

    pub async fn start_container(&mut self) -> CResult<()> {
        let client = self.client.as_ref().unwrap().lock().await;
        if self.is_cold_start() {
            let req = agent::StartContainerRequest {
                container_id: self.id.clone(),
                ..Default::default()
            };
            client
                .start_container(self.ctx.clone(), &req)
                .await
                .map_err(|e| format!("start container failed:{}", e))?;
        }
        if !self.sb_conf.app_snapshot_create {
            if self.state.is_none() {
                return Err(format!("BUG: start container failed, state is none"));
            }
            let state = self.state.as_mut().unwrap();
            let client_wait = client.clone();
            let cid = self.id.clone();
            let real_id = self.real_id.clone();
            let tx_containerd = self.tx_containerd.clone();
            state
                .wait_process(client_wait, cid, real_id, String::new(), tx_containerd)
                .await;
        }

        Ok(())
    }

    async fn do_signal_container(&mut self, exec_id: &String, sig: u32) -> CResult<()> {
        infof!(
            self.log,
            "signal {} to container:{}, exec:{}",
            sig,
            &self.real_id,
            exec_id
        );
        let req = agent::SignalProcessRequest {
            container_id: self.id.clone(),
            exec_id: exec_id.clone(),
            signal: sig,
            ..Default::default()
        };
        let client = self.client.as_ref().unwrap().lock().await;

        if let Err(e) = client
            .signal_process(self.ctx.clone(), &req)
            .await
            .map_err(|e| {
                format!(
                    "signal process failed:{}, execid:{}, sig:{}",
                    e,
                    exec_id.to_owned(),
                    sig
                )
            })
        {
            //forcibly change the result of the kill request to success,
            //so that the cubelet can successfully complete the destruction work.
            if sig != (libc::SIGKILL as u32) && sig != (libc::SIGTERM as u32) {
                return Err(e);
            }
        }

        Ok(())
    }

    pub async fn signal_container(&mut self, exec_id: &String, sig: u32) -> Result<()> {
        {
            let state = self.state.as_ref().unwrap();
            if !state.is_running().await {
                if sig == (libc::SIGKILL as u32) || sig == (libc::SIGTERM as u32) {
                    //stop the container to unblock the hanging 'wait' call
                    if self.sb_conf.app_snapshot_create {
                        state.set_container_stopped().await;
                    }
                    return Ok(());
                }
                infof!(
                    self.log,
                    "container:{} has exited, can't be killed",
                    &self.real_id
                );
                return Err(Error::Other(format!(
                    "container:{} has exited",
                    &self.real_id
                )));
            }
        }

        if !exec_id.is_empty() {
            let exec = {
                let execs = self.execs.lock().await;
                let exec = execs.get(exec_id);
                if exec.is_none() {
                    warnf!(
                        self.log,
                        "not found exec:{} in container:{}, can't be signaled",
                        exec_id,
                        &self.real_id
                    );
                    return Err(Error::NotFoundError(format!(
                        "not found exec:{} in container:{}, can't be signaled",
                        exec_id, &self.real_id
                    )));
                }
                exec.cloned().unwrap()
            };

            if !exec.state.as_ref().unwrap().is_running().await {
                if sig == (libc::SIGKILL as u32) || sig == (libc::SIGTERM as u32) {
                    return Ok(());
                }
                infof!(self.log, "exec:{} has exited, can't be killed", &exec_id);
                return Err(Error::Other(format!(
                    "container:{} exec:{} has exited, can't be killed",
                    &self.real_id, exec_id
                )));
            }
        }

        self.do_signal_container(exec_id, sig)
            .await
            .map_err(|e| Error::Other(e.to_string()))?;

        Ok(())
    }

    pub async fn destroy_container(&mut self) -> Result<(u32, DateTime<Utc>)> {
        //kill
        self.signal_container(&"".to_string(), libc::SIGKILL as u32)
            .await?;

        //remove
        //todo:remove container in guest
        //be lazy here
        Ok(self.state.as_ref().unwrap().get_exit_info().await)
    }

    pub async fn get_container_info(&self, exec_id: &String) -> Result<ContainerInfo> {
        if exec_id.is_empty() {
            let mut info = self.info.clone();
            if self.state.is_some() {
                let task_state = self.state.as_ref().unwrap();
                info.state = task_state.state().await;
                if info.state == TaskState::STOPPED {
                    let (code, tm) = task_state.get_exit_info().await;
                    info.exit_code = code;
                    info.exit_tm = Some(tm);
                }
            }
            return Ok(info);
        }
        let execs = self.execs.lock().await;
        let exec = match execs.get(exec_id) {
            Some(e) => e,
            None => {
                return Err(Error::NotFoundError(format!(
                    "Exec id:{} not found, container:{}",
                    exec_id, &self.real_id
                )))
            }
        };

        let mut ci = ContainerInfo {
            id: exec.id.clone(),
            bundle: self.info.bundle.clone(),
            stdout: exec.tty.stdout.clone(),
            stderr: exec.tty.stderr.clone(),
            terminal: exec.tty.terminal,
            ..Default::default()
        };

        let task_state = exec.state.as_ref().unwrap();
        ci.state = task_state.state().await;
        if ci.state == TaskState::STOPPED {
            let (code, tm) = task_state.get_exit_info().await;
            ci.exit_code = code;
            ci.exit_tm = Some(tm);
        }
        Ok(ci)
    }

    pub async fn wait_container(&mut self, exec_id: &String) -> Result<(u32, DateTime<Utc>)> {
        if *exec_id == self.real_id {
            if self.state.is_none() {
                return Err(Error::Other(
                    "BUG: start container failed, state is none".to_string(),
                ));
            }
            let (code, tm) = self.state.as_ref().unwrap().wait_exit_info().await;
            return Ok((code, tm));
        }

        let exec = {
            let execs = self.execs.lock().await;
            let exec = match execs.get(exec_id) {
                Some(e) => e,
                None => {
                    return Err(Error::NotFoundError(format!(
                        "Exec id:{} not found, container:{}",
                        exec_id, &self.real_id
                    )))
                }
            };
            exec.clone()
        };

        let (code, tm) = exec.state.as_ref().unwrap().wait_exit_info().await;
        Ok((code, tm))
    }

    pub async fn create_exec(&mut self, exec_id: &String, tty: Tty, proc: Process) -> CResult<()> {
        let mut execs = self.execs.lock().await;
        if execs.contains_key(exec_id) {
            return Err(format!(
                "Exec id:{} has exists, container:{}",
                exec_id, &self.real_id
            ));
        }
        let _cs = ContainerState::new(self.log.clone());

        let exec = Exec {
            container_id: self.id.clone(),
            id: exec_id.clone(),
            tty,
            proc,
            state: Some(ContainerState::new(self.log.clone())),
        };
        execs.insert(exec.id.clone(), exec);
        Ok(())
    }

    pub async fn start_exec(&mut self, exec_id: &String) -> Result<()> {
        let exec = {
            let execs = self.execs.lock().await;
            let exec = match execs.get(exec_id) {
                Some(e) => e,
                None => {
                    return Err(Error::NotFoundError(format!(
                        "Exec id:{} not found, container:{}",
                        exec_id, &self.real_id
                    )))
                }
            };
            exec.clone()
        };

        let mut proc = oci::Process::new();
        proc.set_terminal(exec.tty.terminal);
        let user = oci::User {
            uid: exec.proc.user().uid(),
            gid: exec.proc.user().gid(),
            additionalGids: exec
                .proc
                .user()
                .additional_gids()
                .clone()
                .unwrap_or(Vec::new())
                .clone(),
            username: exec
                .proc
                .user()
                .username()
                .clone()
                .unwrap_or(String::new())
                .clone(),
            ..Default::default()
        };
        proc.set_user(user);
        proc.set_args(exec.proc.args().clone().unwrap_or(Vec::new()).into());
        proc.set_env(exec.proc.env().clone().unwrap_or(Vec::new()).into());
        proc.set_cwd(exec.proc.cwd().clone().to_str().unwrap_or("").to_string());

        let mut req = agent::ExecProcessRequest {
            container_id: self.id.clone(),
            exec_id: exec_id.clone(),
            process: Some(proc).into(),
            ..Default::default()
        };

        let runtime_prefix = "runtime:unix://";
        if exec.tty.stdin.starts_with(runtime_prefix) {
            req.runtime_unix_addr = exec
                .tty
                .stdin
                .strip_prefix(runtime_prefix)
                .unwrap_or("")
                .to_string();
        }
        let client = self.client.as_ref().unwrap().lock().await;

        let _ = client
            .exec_process(self.ctx.clone(), &req)
            .await
            .map_err(|e| Error::Other(format!("start execid:{} failed:{}", exec_id, e)))?;
        let mut state = exec.state.clone().unwrap();
        let client_wait = client.clone();
        let cid = self.id.clone();
        let real_id = self.real_id.clone();
        let exec_id = exec_id.clone();
        let tx_containerd = self.tx_containerd.clone();

        state
            .wait_process(client_wait, cid, real_id, exec_id, tx_containerd)
            .await;

        let conn = AsyncUtils::connect_agent(&self.sandbox_id)
            .await
            .map_err(|e| Error::Other(e.to_string()))?;
        let std_client = agent_ttrpc::AgentServiceClient::new(conn);
        exec.forward_std(exec.state.clone().unwrap(), std_client, self.log.clone())
            .await;

        Ok(())
    }

    pub async fn destroy_exec(&mut self, exec_id: &String) -> CResult<(u32, DateTime<Utc>)> {
        let mut exit_code = 255;
        let mut exit_tm = Utc::now();
        //delete exec
        let exec = {
            let execs = self.execs.lock().await;
            let exec = execs.get(exec_id);
            if exec.is_none() {
                warnf!(
                    self.log,
                    "destroy exec:not found exec:{} in container:{}",
                    exec_id,
                    &self.real_id
                );
                return Ok((exit_code, exit_tm));
            }
            exec.cloned().unwrap()
        };

        if exec.state.as_ref().unwrap().is_running().await {
            self.do_signal_container(exec_id, libc::SIGKILL as u32)
                .await?;
        }

        (exit_code, exit_tm) = exec.state.as_ref().unwrap().get_exit_info().await;

        let mut execs = self.execs.lock().await;
        let _ = execs.remove(exec_id);

        Ok((exit_code, exit_tm))
    }

    pub async fn update(&mut self, res: &LinuxResources) -> CResult<()> {
        let mut pb_res = oci::LinuxResources::default();

        if let Some(c) = res.cpu() {
            let cpu = pb_res.mut_cpu();

            if let Some(v) = c.shares() {
                cpu.set_shares(v);
            }

            if let Some(v) = c.quota() {
                cpu.set_quota(v);
            }

            if let Some(v) = c.period() {
                cpu.set_period(v);
            }

            if let Some(v) = c.cpus() {
                cpu.set_cpus(v.clone());
            }
        }

        if let Some(mem) = res.memory() {
            if let Some(limit) = mem.limit() {
                pb_res.mut_memory().set_limit(limit);
            }
        }

        let req = agent::UpdateContainerRequest {
            container_id: self.id.clone(),
            resources: Some(pb_res).into(),
            ..Default::default()
        };
        let client = self.client.as_ref().unwrap().lock().await;

        let _ = client
            .update_container(self.ctx.clone(), &req)
            .await
            .map_err(|e| format!("update container:{} failed:{}", &self.real_id, e))?;
        Ok(())
    }

    pub async fn notify_vm_shutdown(&self) {
        if let Some(state) = &self.state {
            state.notify_vm_shutdown().await;
        }

        let execs = self.execs.lock().await;
        for (_, exec) in execs.iter() {
            if let Some(state) = &exec.state {
                state.notify_vm_shutdown().await;
            }
        }
    }

    pub fn get_id(&self) -> String {
        self.id.clone()
    }
}

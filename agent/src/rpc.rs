// Copyright (c) 2019 Ant Financial
//
// SPDX-License-Identifier: Apache-2.0
//

use anyhow::{anyhow, Context, Result};
use async_trait::async_trait;
use cgroups::freezer::FreezerState;
use cube::rootfs::ANNO_PROPAGATION_CONTAINER_UMNTS;
use cube::rootfs::ANNO_PROPAGATION_EXEC_MNTS;
use cube::utils::ANNO_APP_SNAPSHOT_CONTAINER_ID;
use oci::{LinuxNamespace, Mount, Root, Spec};
use protobuf::MessageDyn;
use protobuf::MessageField;

use protocols::agent::{
    self, AddSwapRequest, AgentDetails, CopyFileRequest, GetIPTablesRequest, GetIPTablesResponse,
    GuestDetailsResponse, Interfaces, Metrics, OOMEvent, ReadStreamResponse, Routes,
    SetIPTablesRequest, SetIPTablesResponse, StatsContainerResponse, VolumeStatsRequest,
    WaitProcessResponse, WriteStreamResponse,
};
use protocols::csi::{volume_usage, VolumeCondition, VolumeStatsResponse, VolumeUsage};
use protocols::empty::Empty;
use protocols::health::health_check_response::ServingStatus;
use protocols::health::{HealthCheckResponse, VersionCheckResponse};
use protocols::types::Interface;
use rustjail::cgroups::notifier;
use rustjail::container::{
    start_exec_process, BaseContainer, Container, LinuxContainer, EXEC_FIFO_FILENAME,
};
use rustjail::process::Process;
use rustjail::specconv::CreateOpts;
use rustjail::{pipestream::PipeStream, process::StreamType};
use std::ffi::CString;
use std::fmt;
use std::fs::create_dir_all;
use std::io;
use std::path::Path;
use std::sync::Arc;
use tokio::io::{AsyncReadExt, AsyncWriteExt, ReadHalf};
use tokio::sync::Mutex;
use ttrpc::{
    self,
    error::get_rpc_status,
    r#async::{Server as TtrpcServer, TtrpcContext},
};

use nix::errno::Errno;
use nix::mount::MsFlags;
use nix::sys::{stat, statfs};
use nix::unistd::{self, Pid};
use rustjail::cgroups::Manager;
use rustjail::process::ProcessOperations;

use crate::device::{
    add_devices, get_virtio_blk_pci_device_name, update_device_cgroup, update_env_pci,
    wait_for_pci_net,
};
use crate::linux_abi::*;
use crate::metrics::get_metrics;
use crate::mount::add_virtiofs_storages;
use crate::mount::{add_storages, baremount, STORAGE_HANDLER_LIST};
use crate::namespace::{NSTYPEIPC, NSTYPEPID, NSTYPEUTS};
use crate::network::setup_guest_dns;
use crate::pci;
use crate::random;
use crate::sandbox::Sandbox;
use crate::time::start_time_sync_task;
use crate::version::{AGENT_VERSION, API_VERSION};
use crate::AGENT_CONFIG;
use cube::rootfs;

use crate::trace_rpc_call;
use crate::tracer::extract_carrier_from_ttrpc;
use opentelemetry::global;
use tracing::span;
use tracing_opentelemetry::OpenTelemetrySpanExt;

use tracing::instrument;

use libc::{self, c_char, c_ushort, pid_t, winsize, TIOCSWINSZ};
use std::fs;
use std::os::unix::prelude::PermissionsExt;
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use base64::engine::general_purpose::STANDARD;
use base64::Engine;
use nix::unistd::{Gid, Uid};
use std::fs::{File, OpenOptions};
use std::io::{BufRead, BufReader, Write};
use std::os::unix::fs::FileExt;
use std::path::PathBuf;
use std::str::FromStr;
const CONTAINER_BASE: &str = "/run/cube-containers";
const RUNTIME_SHARE: &str = "/run/share_runtime/";

const IPTABLES_SAVE: &str = "/sbin/iptables-save";
const IPTABLES_RESTORE: &str = "/sbin/iptables-restore";
const IP6TABLES_SAVE: &str = "/sbin/ip6tables-save";
const IP6TABLES_RESTORE: &str = "/sbin/ip6tables-restore";

const ERR_CANNOT_GET_WRITER: &str = "Cannot get writer";
const ERR_INVALID_BLOCK_SIZE: &str = "Invalid block size";
const ERR_NO_LINUX_FIELD: &str = "Spec does not contain linux field";
const ERR_NO_SANDBOX_PIDNS: &str = "Sandbox does not have sandbox_pidns";

// IPTABLES_RESTORE_WAIT_SEC is the timeout value provided to iptables-restore --wait. Since we
// don't expect other writers to iptables, we don't expect contention for grabbing the iptables
// filesystem lock. Based on this, 5 seconds seems a reasonable timeout period in case the lock is
// not available.
const IPTABLES_RESTORE_WAIT_SEC: u64 = 5;

const ANNOTATION_K_ROOTFS_WL_PATH: &str = "cube.rootfs.wlayer.path";
const PROC_PATH_NFS_CLIENT_IDENT: &str = "/sys/fs/nfs/net/nfs_client/identifier";
const CONTAINER_CUSTOM_FILE_BASE: &str = "/run/custom_file";

// Convenience macro to obtain the scope logger
macro_rules! sl {
    () => {
        slog_scope::logger()
    };
}

// Convenience macro to wrap an error and response to ttrpc client
macro_rules! ttrpc_error {
    ($code:path, $err:expr $(,)?) => {
        get_rpc_status($code, format!("{:?}", $err))
    };
}

macro_rules! is_allowed {
    ($req:ident) => {
        if !AGENT_CONFIG
            .read()
            .await
            .is_allowed_endpoint($req.descriptor_dyn().name())
        {
            return Err(ttrpc_error!(
                ttrpc::Code::UNIMPLEMENTED,
                format!("{} is blocked", $req.descriptor_dyn().name()),
            ));
        }
    };
}

#[derive(Clone, Debug)]
pub struct AgentService {
    sandbox: Arc<Mutex<Sandbox>>,
}

impl AgentService {
    #[instrument]
    async fn do_create_container(
        &self,
        req: protocols::agent::CreateContainerRequest,
    ) -> Result<()> {
        let mut start = Instant::now();
        let cid = req.container_id.clone();
        info!(sl!(), "[cube-strace]recv create container");

        let mut oci_spec = req.OCI.clone();
        let use_sandbox_pidns = req.sandbox_pidns();

        let sandbox;
        let mut s;
        let mut oci = match oci_spec.as_mut() {
            Some(spec) => rustjail::grpc_to_oci(spec),
            None => {
                error!(sl!(), "no oci spec in the create container request!");
                return Err(anyhow!(nix::Error::EINVAL));
            }
        };
        let anno = oci.annotations.clone();
        if let Some(id) = anno.get(ANNO_APP_SNAPSHOT_CONTAINER_ID) {
            info!(sl!(), "create container by restore");
            let pid = {
                let sandbox = self.sandbox.clone();
                let mut s: tokio::sync::MutexGuard<'_, Sandbox> = sandbox.lock().await;
                let process = s.find_container_process(id, &"")?;
                process.pid
            };

            debug!(sl!(), "container pid:{}", pid);
            start_exec_process(
                pid,
                anno.get(ANNO_PROPAGATION_EXEC_MNTS),
                anno.get(ANNO_PROPAGATION_CONTAINER_UMNTS),
            )
            .await
            .map_err(|e| anyhow!(format!("Exec mount failed:{}", e.to_string())))?;
            return Ok(());
        }

        // Some devices need some extra processing (the ones invoked with
        // --device for instance), and that's what this call is doing. It
        // updates the devices listed in the OCI spec, so that they actually
        // match real devices inside the VM. This step is necessary since we
        // cannot predict everything from the caller.
        add_devices(&req.devices.to_vec(), &mut oci, &self.sandbox).await?;
        let duration_add_devices = start.elapsed().as_millis();
        start = Instant::now();
        // Both rootfs and volumes (invoked with --volume for instance) will
        // be processed the same way. The idea is to always mount any provided
        // storage to the specified MountPoint, so that it will match what's
        // inside oci.Mounts.
        // After all those storages have been processed, no matter the order
        // here, the agent will rely on rustjail (using the oci.Mounts
        // list) to bind mount all of them inside the container.
        let m = add_storages(
            sl!(),
            req.storages.to_vec(),
            self.sandbox.clone(),
            Some(req.container_id.clone()),
        )
        .await?;

        {
            sandbox = self.sandbox.clone();
            s = sandbox.lock().await;
            s.container_mounts.insert(cid.clone(), m);
        }

        let duration_add_storage = start.elapsed().as_millis();
        start = Instant::now();
        update_container_namespaces(&s, &mut oci, use_sandbox_pidns)?;

        // Add the root partition to the device cgroup to prevent access
        update_device_cgroup(&mut oci)?;

        // Append guest hooks
        append_guest_hooks(&s, &mut oci)?;

        // write spec to bundle path, hooks might
        // read ocispec
        let olddir = setup_bundle(&cid, &mut oci, req.custom_files.to_vec())?;
        // restore the cwd for kata-agent process.
        defer!(unistd::chdir(&olddir).unwrap());
        let opts = CreateOpts {
            cgroup_name: "".to_string(),
            use_systemd_cgroup: false,
            no_pivot_root: s.no_pivot_root,
            no_new_keyring: false,
            spec: Some(oci.clone()),
            rootless_euid: false,
            rootless_cgroup: false,
        };
        let duration_setup_bundle = start.elapsed().as_millis();
        start = Instant::now();
        let mut ctr: LinuxContainer =
            LinuxContainer::new(cid.as_str(), CONTAINER_BASE, opts, &sl!())?;

        let pipe_size = AGENT_CONFIG.read().await.container_pipe_size;

        let mut p = if let Some(p) = oci.process {
            Process::new(&sl!(), &p, cid.as_str(), true, pipe_size)?
        } else {
            info!(sl!(), "no process configurations!");
            return Err(anyhow!(nix::Error::EINVAL));
        };
        let duration_init_container = start.elapsed().as_millis();
        start = Instant::now();
        p.open_io(&sl!(), None).map_err(|e| anyhow!(e))?;
        ctr.start(p).await?;
        s.update_shared_pidns(&ctr)?;
        s.add_container(ctr);
        let duration_start_container = start.elapsed().as_millis();
        info!(sl!(), "created container!, add_devices: {}ms, add storage:{}ms, setup bundle:{}ms, init container:{}ms, start container:{}ms",
            duration_add_devices,  duration_add_storage, duration_setup_bundle, duration_init_container, duration_start_container);
        start_time_sync_task().await;
        Ok(())
    }

    #[instrument]
    async fn do_start_container(&self, req: protocols::agent::StartContainerRequest) -> Result<()> {
        let cid = req.container_id;

        let sandbox = self.sandbox.clone();
        let mut s = sandbox.lock().await;
        let sid = s.id.clone();

        let ctr = s
            .get_container(&cid)
            .ok_or_else(|| anyhow!("Invalid container id"))?;

        ctr.exec().await?;

        if sid == cid {
            return Ok(());
        }

        // start oom event loop
        if let Some(ref ctr) = ctr.cgroup_manager {
            let cg_path = ctr.get_cg_path("memory");

            if let Some(cg_path) = cg_path {
                let rx = notifier::notify_oom(cid.as_str(), cg_path.to_string()).await?;

                s.run_oom_event_monitor(rx, cid.clone()).await;
            }
        }

        Ok(())
    }

    #[instrument]
    async fn do_remove_container(
        &self,
        req: protocols::agent::RemoveContainerRequest,
    ) -> Result<()> {
        let cid = req.container_id.clone();
        let mut cmounts: Vec<String> = vec![];

        let mut remove_container_resources = |sandbox: &mut Sandbox| -> Result<()> {
            // Find the sandbox storage used by this container
            let mounts = sandbox.container_mounts.get(&cid);
            if let Some(mounts) = mounts {
                for m in mounts.iter() {
                    if sandbox.storages.get(m).is_some() {
                        cmounts.push(m.to_string());
                    }
                }
            }

            for m in cmounts.iter() {
                sandbox.unset_and_remove_sandbox_storage(m)?;
            }

            sandbox.container_mounts.remove(cid.as_str());
            sandbox.containers.remove(cid.as_str());
            Ok(())
        };

        if req.timeout == 0 {
            let s = Arc::clone(&self.sandbox);
            let mut sandbox = s.lock().await;

            sandbox.bind_watcher.remove_container(&cid).await;

            sandbox
                .get_container(&cid)
                .ok_or_else(|| anyhow!("Invalid container id"))?
                .destroy()
                .await?;

            remove_container_resources(&mut sandbox)?;

            return Ok(());
        }

        // timeout != 0
        let s = self.sandbox.clone();
        let cid2 = cid.clone();
        let (tx, rx) = tokio::sync::oneshot::channel::<i32>();

        let handle = tokio::spawn(async move {
            let mut sandbox = s.lock().await;
            if let Some(ctr) = sandbox.get_container(&cid2) {
                ctr.destroy().await.unwrap();
                sandbox.bind_watcher.remove_container(&cid2).await;
                tx.send(1).unwrap();
            };
        });

        if tokio::time::timeout(Duration::from_secs(req.timeout.into()), rx)
            .await
            .is_err()
        {
            return Err(anyhow!(nix::Error::ETIME));
        }

        if handle.await.is_err() {
            return Err(anyhow!(nix::Error::UnknownErrno));
        }

        let s = self.sandbox.clone();
        let mut sandbox = s.lock().await;

        remove_container_resources(&mut sandbox)?;

        Ok(())
    }

    #[instrument]
    async fn do_exec_process(&self, req: protocols::agent::ExecProcessRequest) -> Result<()> {
        let cid = req.container_id.clone();
        let exec_id = req.exec_id.clone();

        info!(sl!(), "do_exec_process cid: {} eid: {}", cid, exec_id);

        let s = self.sandbox.clone();
        let mut sandbox = s.lock().await;

        let mut process = req
            .process
            .into_option()
            .ok_or_else(|| anyhow!(nix::Error::EINVAL))?;

        // Apply any necessary corrections for PCI addresses
        update_env_pci(&mut process.Env, &sandbox.pcimap)?;

        let pipe_size = AGENT_CONFIG.read().await.container_pipe_size;
        let ocip = rustjail::process_grpc_to_oci(&process);
        let mut p = Process::new(&sl!(), &ocip, exec_id.as_str(), false, pipe_size)?;
        let ctr = sandbox
            .get_container(&cid)
            .ok_or_else(|| anyhow!("Invalid container id"))?;

        if req.runtime_unix_addr.is_empty() {
            p.open_io(&sl!(), None).map_err(|e| anyhow!(e))?;
        } else {
            p.open_io(&sl!(), Some(&req.runtime_unix_addr))
                .map_err(|e| anyhow!(e))?;
        }

        ctr.run(p).await?;
        info!(sl!(), "finish exec run");

        Ok(())
    }

    #[instrument]
    async fn do_signal_process(&self, req: protocols::agent::SignalProcessRequest) -> Result<()> {
        let cid = req.container_id.clone();
        let eid = req.exec_id.clone();
        let s = self.sandbox.clone();

        info!(
            sl!(),
            "signal process";
            "container-id" => cid.clone(),
            "exec-id" => eid.clone(),
        );

        let mut sig: libc::c_int = req.signal as libc::c_int;
        {
            let mut sandbox = s.lock().await;
            let p = sandbox.find_container_process(cid.as_str(), eid.as_str())?;
            // For container initProcess, if it hasn't installed handler for "SIGTERM" signal,
            // it will ignore the "SIGTERM" signal sent to it, thus send it "SIGKILL" signal
            // instead of "SIGTERM" to terminate it.
            let proc_status_file = format!("/proc/{}/status", p.pid);
            if p.init && sig == libc::SIGTERM && !is_signal_handled(&proc_status_file, sig as u32) {
                sig = libc::SIGKILL;
            }
            p.signal(sig)?;

            if p.init && sig == libc::SIGKILL {
                let ctr = sandbox
                    .get_container(&cid)
                    .ok_or_else(|| anyhow!("Invalid container id"))?;
                let fifo_file = format!("{}/{}", &ctr.root, EXEC_FIFO_FILENAME);
                unistd::unlink(fifo_file.as_str())?;
            }
        }

        if eid.is_empty() {
            // eid is empty, signal all the remaining processes in the container cgroup
            info!(
                sl!(),
                "signal all the remaining processes";
                "container-id" => cid.clone(),
                "exec-id" => eid.clone(),
            );

            if let Err(err) = self.freeze_cgroup(&cid, FreezerState::Frozen).await {
                warn!(
                    sl!(),
                    "freeze cgroup failed";
                    "container-id" => cid.clone(),
                    "exec-id" => eid.clone(),
                    "error" => format!("{:?}", err),
                );
            }

            let pids = self.get_pids(&cid).await?;
            for pid in pids.iter() {
                let res = unsafe { libc::kill(*pid, sig) };
                if let Err(err) = Errno::result(res).map(drop) {
                    warn!(
                        sl!(),
                        "signal failed";
                        "container-id" => cid.clone(),
                        "exec-id" => eid.clone(),
                        "pid" => pid,
                        "error" => format!("{:?}", err),
                    );
                }
            }
            if let Err(err) = self.freeze_cgroup(&cid, FreezerState::Thawed).await {
                warn!(
                    sl!(),
                    "unfreeze cgroup failed";
                    "container-id" => cid.clone(),
                    "exec-id" => eid.clone(),
                    "error" => format!("{:?}", err),
                );
            }
        }
        Ok(())
    }

    async fn freeze_cgroup(&self, cid: &str, state: FreezerState) -> Result<()> {
        let s = self.sandbox.clone();
        let mut sandbox = s.lock().await;
        let ctr = sandbox
            .get_container(cid)
            .ok_or_else(|| anyhow!("Invalid container id {}", cid))?;
        let cm = ctr
            .cgroup_manager
            .as_ref()
            .ok_or_else(|| anyhow!("cgroup manager not exist"))?;
        cm.freeze(state)?;
        Ok(())
    }

    async fn get_pids(&self, cid: &str) -> Result<Vec<i32>> {
        let s = self.sandbox.clone();
        let mut sandbox = s.lock().await;
        let ctr = sandbox
            .get_container(cid)
            .ok_or_else(|| anyhow!("Invalid container id {}", cid))?;
        let cm = ctr
            .cgroup_manager
            .as_ref()
            .ok_or_else(|| anyhow!("cgroup manager not exist"))?;
        let pids = cm.get_pids()?;
        Ok(pids)
    }

    #[instrument]
    async fn do_wait_process(
        &self,
        req: protocols::agent::WaitProcessRequest,
    ) -> Result<protocols::agent::WaitProcessResponse> {
        let cid = req.container_id.clone();
        let eid = req.exec_id;
        let s = self.sandbox.clone();
        let mut resp = WaitProcessResponse::new();
        let pid: pid_t;

        let (exit_send, mut exit_recv) = tokio::sync::mpsc::channel(100);

        info!(
            sl!(),
            "wait process";
            "container-id" => cid.clone(),
            "exec-id" => eid.clone()
        );

        let exit_rx = {
            let mut sandbox = s.lock().await;
            let p = sandbox.find_container_process(cid.as_str(), eid.as_str())?;

            p.exit_watchers.push(exit_send);
            pid = p.pid;

            p.exit_rx.clone()
        };

        if let Some(mut exit_rx) = exit_rx {
            info!(sl!(), "cid {} eid {} waiting for exit signal", &cid, &eid);
            while exit_rx.changed().await.is_ok() {}
            info!(sl!(), "cid {} eid {} received exit signal", &cid, &eid);
        }

        let mut sandbox = s.lock().await;
        let ctr = sandbox
            .get_container(&cid)
            .ok_or_else(|| anyhow!("Invalid container id"))?;

        let p = match ctr.processes.get_mut(&pid) {
            Some(p) => p,
            None => {
                // Lost race, pick up exit code from channel
                resp.status = exit_recv
                    .recv()
                    .await
                    .ok_or_else(|| anyhow!("Failed to receive exit code"))?;

                return Ok(resp);
            }
        };

        // need to close all fd
        // ignore errors for some fd might be closed by stream
        p.cleanup_process_stream();

        resp.status = p.exit_code;
        // broadcast exit code to all parallel watchers
        for s in p.exit_watchers.iter_mut() {
            // Just ignore errors in case any watcher quits unexpectedly
            let _ = s.send(p.exit_code).await;
        }

        ctr.processes.remove(&pid);

        Ok(resp)
    }

    async fn do_write_stream(
        &self,
        req: protocols::agent::WriteStreamRequest,
    ) -> Result<protocols::agent::WriteStreamResponse> {
        let cid = req.container_id.clone();
        let eid = req.exec_id.clone();

        let writer = {
            let s = self.sandbox.clone();
            let mut sandbox = s.lock().await;
            let p = sandbox.find_container_process(cid.as_str(), eid.as_str())?;

            // use ptmx io
            if p.term_master.is_some() {
                p.get_writer(StreamType::TermMaster)
            } else {
                // use piped io
                p.get_writer(StreamType::ParentStdin)
            }
        };

        let writer = writer.ok_or_else(|| anyhow!(ERR_CANNOT_GET_WRITER))?;
        writer.lock().await.write_all(req.data.as_slice()).await?;

        let mut resp = WriteStreamResponse::new();
        resp.set_len(req.data.len() as u32);

        Ok(resp)
    }

    async fn do_read_stream(
        &self,
        req: protocols::agent::ReadStreamRequest,
        stdout: bool,
    ) -> Result<protocols::agent::ReadStreamResponse> {
        let cid = req.container_id;
        let eid = req.exec_id;

        let mut term_exit_notifier = Arc::new(tokio::sync::Notify::new());
        let reader = {
            let s = self.sandbox.clone();
            let mut sandbox = s.lock().await;

            let p = sandbox.find_container_process(cid.as_str(), eid.as_str())?;

            if p.term_master.is_some() {
                term_exit_notifier = p.term_exit_notifier.clone();
                p.get_reader(StreamType::TermMaster)
            } else if stdout {
                if p.parent_stdout.is_some() {
                    p.get_reader(StreamType::ParentStdout)
                } else {
                    None
                }
            } else {
                p.get_reader(StreamType::ParentStderr)
            }
        };

        if reader.is_none() {
            return Err(anyhow!(nix::Error::EINVAL));
        }

        let reader = reader.ok_or_else(|| anyhow!("cannot get stream reader"))?;

        tokio::select! {
            _ = term_exit_notifier.notified() => {
                Err(anyhow!("eof"))
            }
            v = read_stream(reader, req.len as usize)  => {
                let vector = v?;
                let mut resp = ReadStreamResponse::new();
                resp.set_data(vector);

                Ok(resp)
            }
        }
    }
}

#[async_trait]
impl protocols::agent_ttrpc::AgentService for AgentService {
    async fn create_container(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::CreateContainerRequest,
    ) -> ttrpc::Result<Empty> {
        trace_rpc_call!(ctx, "create_container", req);
        is_allowed!(req);
        match self.do_create_container(req).await {
            Err(e) => Err(ttrpc_error!(ttrpc::Code::INTERNAL, e)),
            Ok(_) => Ok(Empty::new()),
        }
    }

    async fn start_container(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::StartContainerRequest,
    ) -> ttrpc::Result<Empty> {
        trace_rpc_call!(ctx, "start_container", req);
        is_allowed!(req);
        match self.do_start_container(req).await {
            Err(e) => Err(ttrpc_error!(ttrpc::Code::INTERNAL, e)),
            Ok(_) => Ok(Empty::new()),
        }
    }

    async fn remove_container(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::RemoveContainerRequest,
    ) -> ttrpc::Result<Empty> {
        trace_rpc_call!(ctx, "remove_container", req);
        is_allowed!(req);

        match self.do_remove_container(req).await {
            Err(e) => Err(ttrpc_error!(ttrpc::Code::INTERNAL, e)),
            Ok(_) => Ok(Empty::new()),
        }
    }

    async fn exec_process(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::ExecProcessRequest,
    ) -> ttrpc::Result<Empty> {
        trace_rpc_call!(ctx, "exec_process", req);
        is_allowed!(req);
        match self.do_exec_process(req).await {
            Err(e) => Err(ttrpc_error!(ttrpc::Code::INTERNAL, e)),
            Ok(_) => Ok(Empty::new()),
        }
    }

    async fn signal_process(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::SignalProcessRequest,
    ) -> ttrpc::Result<Empty> {
        trace_rpc_call!(ctx, "signal_process", req);
        is_allowed!(req);
        match self.do_signal_process(req).await {
            Err(e) => Err(ttrpc_error!(ttrpc::Code::INTERNAL, e)),
            Ok(_) => Ok(Empty::new()),
        }
    }

    async fn wait_process(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::WaitProcessRequest,
    ) -> ttrpc::Result<WaitProcessResponse> {
        trace_rpc_call!(ctx, "wait_process", req);
        is_allowed!(req);
        self.do_wait_process(req)
            .await
            .map_err(|e| ttrpc_error!(ttrpc::Code::INTERNAL, e))
    }

    async fn update_container(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::UpdateContainerRequest,
    ) -> ttrpc::Result<Empty> {
        trace_rpc_call!(ctx, "update_container", req);
        is_allowed!(req);
        let cid = req.container_id.clone();
        let res = req.resources;
        let s = Arc::clone(&self.sandbox);
        let mut sandbox = s.lock().await;

        let ctr = sandbox.get_container(&cid).ok_or_else(|| {
            ttrpc_error!(
                ttrpc::Code::INVALID_ARGUMENT,
                "invalid container id".to_string(),
            )
        })?;

        let resp = Empty::new();

        if let Some(res) = res.as_ref() {
            let oci_res = rustjail::resources_grpc_to_oci(res);
            match ctr.set(oci_res) {
                Err(e) => {
                    return Err(ttrpc_error!(ttrpc::Code::INTERNAL, e));
                }

                Ok(_) => return Ok(resp),
            }
        }

        Ok(resp)
    }

    async fn stats_container(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::StatsContainerRequest,
    ) -> ttrpc::Result<StatsContainerResponse> {
        trace_rpc_call!(ctx, "stats_container", req);
        is_allowed!(req);
        let cid = req.container_id;
        let s = Arc::clone(&self.sandbox);
        let mut sandbox = s.lock().await;

        let ctr = sandbox.get_container(&cid).ok_or_else(|| {
            ttrpc_error!(
                ttrpc::Code::INVALID_ARGUMENT,
                "invalid container id".to_string(),
            )
        })?;

        ctr.stats()
            .map_err(|e| ttrpc_error!(ttrpc::Code::INTERNAL, e))
    }

    async fn pause_container(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::PauseContainerRequest,
    ) -> ttrpc::Result<protocols::empty::Empty> {
        trace_rpc_call!(ctx, "pause_container", req);
        is_allowed!(req);
        let cid = req.container_id();
        let s = Arc::clone(&self.sandbox);
        let mut sandbox = s.lock().await;

        let ctr = sandbox.get_container(cid).ok_or_else(|| {
            ttrpc_error!(
                ttrpc::Code::INVALID_ARGUMENT,
                "invalid container id".to_string(),
            )
        })?;

        ctr.pause()
            .map_err(|e| ttrpc_error!(ttrpc::Code::INTERNAL, e))?;

        Ok(Empty::new())
    }

    async fn resume_container(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::ResumeContainerRequest,
    ) -> ttrpc::Result<protocols::empty::Empty> {
        trace_rpc_call!(ctx, "resume_container", req);
        is_allowed!(req);
        let cid = req.container_id();
        let s = Arc::clone(&self.sandbox);
        let mut sandbox = s.lock().await;

        let ctr = sandbox.get_container(cid).ok_or_else(|| {
            ttrpc_error!(
                ttrpc::Code::INVALID_ARGUMENT,
                "invalid container id".to_string(),
            )
        })?;

        ctr.resume()
            .map_err(|e| ttrpc_error!(ttrpc::Code::INTERNAL, e))?;

        Ok(Empty::new())
    }

    async fn write_stdin(
        &self,
        _ctx: &TtrpcContext,
        req: protocols::agent::WriteStreamRequest,
    ) -> ttrpc::Result<WriteStreamResponse> {
        is_allowed!(req);
        self.do_write_stream(req)
            .await
            .map_err(|e| ttrpc_error!(ttrpc::Code::INTERNAL, e))
    }

    async fn read_stdout(
        &self,
        _ctx: &TtrpcContext,
        req: protocols::agent::ReadStreamRequest,
    ) -> ttrpc::Result<ReadStreamResponse> {
        is_allowed!(req);
        self.do_read_stream(req, true)
            .await
            .map_err(|e| ttrpc_error!(ttrpc::Code::INTERNAL, e))
    }

    async fn read_stderr(
        &self,
        _ctx: &TtrpcContext,
        req: protocols::agent::ReadStreamRequest,
    ) -> ttrpc::Result<ReadStreamResponse> {
        is_allowed!(req);
        self.do_read_stream(req, false)
            .await
            .map_err(|e| ttrpc_error!(ttrpc::Code::INTERNAL, e))
    }

    async fn close_stdin(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::CloseStdinRequest,
    ) -> ttrpc::Result<Empty> {
        trace_rpc_call!(ctx, "close_stdin", req);
        is_allowed!(req);

        let cid = req.container_id.clone();
        let eid = req.exec_id;
        let s = Arc::clone(&self.sandbox);
        let mut sandbox = s.lock().await;

        let p = sandbox
            .find_container_process(cid.as_str(), eid.as_str())
            .map_err(|e| {
                ttrpc_error!(
                    ttrpc::Code::INVALID_ARGUMENT,
                    format!("invalid argument: {:?}", e),
                )
            })?;

        p.close_stdin();

        Ok(Empty::new())
    }

    async fn tty_win_resize(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::TtyWinResizeRequest,
    ) -> ttrpc::Result<Empty> {
        trace_rpc_call!(ctx, "tty_win_resize", req);
        is_allowed!(req);

        let cid = req.container_id.clone();
        let eid = req.exec_id.clone();
        let s = Arc::clone(&self.sandbox);
        let mut sandbox = s.lock().await;
        let p = sandbox
            .find_container_process(cid.as_str(), eid.as_str())
            .map_err(|e| {
                ttrpc_error!(
                    ttrpc::Code::UNAVAILABLE,
                    format!("invalid argument: {:?}", e),
                )
            })?;

        if let Some(fd) = p.term_master {
            unsafe {
                let win = winsize {
                    ws_row: req.row as c_ushort,
                    ws_col: req.column as c_ushort,
                    ws_xpixel: 0,
                    ws_ypixel: 0,
                };

                let err = libc::ioctl(fd, TIOCSWINSZ, &win);
                Errno::result(err).map(drop).map_err(|e| {
                    ttrpc_error!(ttrpc::Code::INTERNAL, format!("ioctl error: {:?}", e))
                })?;
            }
        } else {
            return Err(ttrpc_error!(ttrpc::Code::UNAVAILABLE, "no tty".to_string()));
        }

        Ok(Empty::new())
    }

    async fn update_interface(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::UpdateInterfaceRequest,
    ) -> ttrpc::Result<Interface> {
        trace_rpc_call!(ctx, "update_interface", req);
        is_allowed!(req);

        let interface = req.interface.into_option().ok_or_else(|| {
            ttrpc_error!(
                ttrpc::Code::INVALID_ARGUMENT,
                "empty update interface request".to_string(),
            )
        })?;

        self.sandbox
            .lock()
            .await
            .rtnl
            .update_interface(&interface)
            .await
            .map_err(|e| {
                ttrpc_error!(ttrpc::Code::INTERNAL, format!("update interface: {:?}", e))
            })?;

        Ok(interface)
    }

    async fn update_routes(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::UpdateRoutesRequest,
    ) -> ttrpc::Result<Routes> {
        trace_rpc_call!(ctx, "update_routes", req);
        is_allowed!(req);

        let new_routes = req
            .routes
            .into_option()
            .map(|r| r.Routes.to_vec())
            .ok_or_else(|| {
                ttrpc_error!(
                    ttrpc::Code::INVALID_ARGUMENT,
                    "empty update routes request".to_string(),
                )
            })?;

        let mut sandbox = self.sandbox.lock().await;

        sandbox.rtnl.update_routes(new_routes).await.map_err(|e| {
            ttrpc_error!(
                ttrpc::Code::INTERNAL,
                format!("Failed to update routes: {:?}", e),
            )
        })?;

        let list = sandbox.rtnl.list_routes().await.map_err(|e| {
            ttrpc_error!(
                ttrpc::Code::INTERNAL,
                format!("Failed to list routes after update: {:?}", e),
            )
        })?;

        Ok(protocols::agent::Routes {
            Routes: list,
            ..Default::default()
        })
    }

    async fn get_ip_tables(
        &self,
        ctx: &TtrpcContext,
        req: GetIPTablesRequest,
    ) -> ttrpc::Result<GetIPTablesResponse> {
        trace_rpc_call!(ctx, "get_iptables", req);
        is_allowed!(req);

        info!(sl!(), "get_ip_tables: request received");

        let cmd = if req.is_ipv6 {
            IP6TABLES_SAVE
        } else {
            IPTABLES_SAVE
        }
        .to_string();

        match Command::new(cmd.clone()).output() {
            Ok(output) => Ok(GetIPTablesResponse {
                data: output.stdout,
                ..Default::default()
            }),
            Err(e) => {
                warn!(sl!(), "failed to run {}: {:?}", cmd, e.kind());
                return Err(ttrpc_error!(ttrpc::Code::INTERNAL, e));
            }
        }
    }

    async fn set_ip_tables(
        &self,
        ctx: &TtrpcContext,
        req: SetIPTablesRequest,
    ) -> ttrpc::Result<SetIPTablesResponse> {
        trace_rpc_call!(ctx, "set_iptables", req);
        is_allowed!(req);

        info!(sl!(), "set_ip_tables request received");

        let cmd = if req.is_ipv6 {
            IP6TABLES_RESTORE
        } else {
            IPTABLES_RESTORE
        }
        .to_string();

        let mut child = match Command::new(cmd.clone())
            .arg("--wait")
            .arg(IPTABLES_RESTORE_WAIT_SEC.to_string())
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
        {
            Ok(child) => child,
            Err(e) => {
                warn!(sl!(), "failure to spawn {}: {:?}", cmd, e.kind());
                return Err(ttrpc_error!(ttrpc::Code::INTERNAL, e));
            }
        };

        let mut stdin = match child.stdin.take() {
            Some(si) => si,
            None => {
                println!("failed to get stdin from child");
                return Err(ttrpc_error!(
                    ttrpc::Code::INTERNAL,
                    "failed to take stdin from child".to_string()
                ));
            }
        };

        let (tx, rx) = tokio::sync::oneshot::channel::<i32>();
        let handle = tokio::spawn(async move {
            let _ = match stdin.write_all(&req.data) {
                Ok(o) => o,
                Err(e) => {
                    warn!(sl!(), "error writing stdin: {:?}", e.kind());
                    return;
                }
            };
            if tx.send(1).is_err() {
                warn!(sl!(), "stdin writer thread receiver dropped");
            };
        });

        if tokio::time::timeout(Duration::from_secs(IPTABLES_RESTORE_WAIT_SEC), rx)
            .await
            .is_err()
        {
            return Err(ttrpc_error!(
                ttrpc::Code::INTERNAL,
                "timeout waiting for stdin writer to complete".to_string()
            ));
        }

        if handle.await.is_err() {
            return Err(ttrpc_error!(
                ttrpc::Code::INTERNAL,
                "stdin writer thread failure".to_string()
            ));
        }

        let output = match child.wait_with_output() {
            Ok(o) => o,
            Err(e) => {
                warn!(
                    sl!(),
                    "failure waiting for spawned {} to complete: {:?}",
                    cmd,
                    e.kind()
                );
                return Err(ttrpc_error!(ttrpc::Code::INTERNAL, e));
            }
        };

        if !output.status.success() {
            warn!(sl!(), "{} failed: {:?}", cmd, output.stderr);
            return Err(ttrpc_error!(
                ttrpc::Code::INTERNAL,
                format!(
                    "{} failed: {:?}",
                    cmd,
                    String::from_utf8_lossy(&output.stderr)
                )
            ));
        }

        Ok(SetIPTablesResponse {
            data: output.stdout,
            ..Default::default()
        })
    }

    async fn list_interfaces(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::ListInterfacesRequest,
    ) -> ttrpc::Result<Interfaces> {
        trace_rpc_call!(ctx, "list_interfaces", req);
        is_allowed!(req);

        let list = self
            .sandbox
            .lock()
            .await
            .rtnl
            .list_interfaces()
            .await
            .map_err(|e| {
                ttrpc_error!(
                    ttrpc::Code::INTERNAL,
                    format!("Failed to list interfaces: {:?}", e),
                )
            })?;

        Ok(protocols::agent::Interfaces {
            Interfaces: list,
            ..Default::default()
        })
    }

    async fn list_routes(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::ListRoutesRequest,
    ) -> ttrpc::Result<Routes> {
        trace_rpc_call!(ctx, "list_routes", req);
        is_allowed!(req);

        let list = self
            .sandbox
            .lock()
            .await
            .rtnl
            .list_routes()
            .await
            .map_err(|e| ttrpc_error!(ttrpc::Code::INTERNAL, format!("list routes: {:?}", e)))?;

        Ok(protocols::agent::Routes {
            Routes: list,
            ..Default::default()
        })
    }

    async fn create_sandbox(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::CreateSandboxRequest,
    ) -> ttrpc::Result<Empty> {
        trace_rpc_call!(ctx, "create_sandbox", req);
        is_allowed!(req);
        info!(sl!(), "receive create sandbox");
        let mut start = Instant::now();

        if req.start_mode == protocols::agent::StartMode::RESTORE.into() {
            match add_virtiofs_storages(sl!(), req.storages.to_vec()).await {
                Ok(_) => {}
                Err(e) => {
                    error!(sl!(), "add storages failed:{:?}", e);
                    return Err(ttrpc_error!(
                        ttrpc::Code::INTERNAL,
                        format!("add storages failed:{:?}", e)
                    ));
                }
            };
            let duration_storage = start.elapsed().as_millis();
            info!(sl!(), "create sandbox!, add storage:{}ms", duration_storage);
            return Ok(Empty::new());
        }

        if req.cube_mvm_monitor {
            do_enable_cube_mvm_monitor()
                .await
                .map_err(|e| {
                    error!(sl!(), "enable cube mvm monitor failed, {:}", e);
                })
                .ok();
        }

        {
            let interfaces = req.interfaces.to_vec();
            for i in interfaces {
                if !i.pciPath.is_empty() {
                    let sandbox = self.sandbox.clone();
                    let pcipath = pci::Path::from_str(i.pciPath.as_str()).map_err(|e| {
                        ttrpc_error!(
                            ttrpc::Code::INTERNAL,
                            format!("pci::Path::from_str failed, pciPath:{},{}", i.pciPath, e)
                        )
                    })?;
                    let addr = wait_for_pci_net(&sandbox, &pcipath).await.map_err(|e| {
                        ttrpc_error!(
                            ttrpc::Code::INTERNAL,
                            format!("Failed to wait pci: {:?}", e)
                        )
                    })?;
                    info!(sl!(), "wait a pci:{:}", addr)
                }
                self.sandbox
                    .lock()
                    .await
                    .rtnl
                    .update_interface(&i)
                    .await
                    .map_err(|e| {
                        ttrpc_error!(
                            ttrpc::Code::INTERNAL,
                            format!("Failed to update interface: {:?}", e)
                        )
                    })?;
            }
        }

        {
            let routes = req.routes.to_vec();
            self.sandbox
                .lock()
                .await
                .rtnl
                .update_routes(routes)
                .await
                .map_err(|e| {
                    ttrpc_error!(
                        ttrpc::Code::INTERNAL,
                        format!("Failed to update routes: {:?}", e),
                    )
                })?;
        }

        {
            let arps = req.ARPNeighbors.to_vec();
            self.sandbox
                .lock()
                .await
                .rtnl
                .add_arp_neighbors(arps)
                .await
                .map_err(|e| {
                    ttrpc_error!(
                        ttrpc::Code::INTERNAL,
                        format!("Failed to add ARP neighbours: {:?}", e),
                    )
                })?;
        }
        let duration_net = start.elapsed().as_millis();

        {
            let sandbox = self.sandbox.clone();
            let mut s = sandbox.lock().await;

            let _ = fs::remove_dir_all(CONTAINER_BASE);
            let _ = fs::create_dir_all(CONTAINER_BASE);
            let _ = fs::create_dir_all(RUNTIME_SHARE);

            s.hostname = req.hostname.clone();
            s.running = true;

            if !req.sandbox_id.is_empty() {
                s.id = req.sandbox_id.clone();
            }

            s.setup_shared_namespaces().await.map_err(|e| {
                ttrpc_error!(
                    ttrpc::Code::INTERNAL,
                    format!("setup shared namespaces failed:{:?}", e)
                )
            })?;
        }
        debug!(sl!(), "add storage:{:?}", req.storages.to_vec());
        start = Instant::now();
        match add_storages(sl!(), req.storages.to_vec(), self.sandbox.clone(), None).await {
            Ok(m) => {
                let sandbox = self.sandbox.clone();
                let mut s = sandbox.lock().await;
                s.mounts = m
            }
            Err(e) => {
                return Err(ttrpc_error!(
                    ttrpc::Code::INTERNAL,
                    format!("add storages failed:{:?}", e)
                ))
            }
        };
        let duration_storage = start.elapsed().as_millis();
        start = Instant::now();

        match std::fs::write(PROC_PATH_NFS_CLIENT_IDENT, req.sandbox_id) {
            Ok(_) => {}
            Err(e) => error!(sl!(), "config nfs client identifier failed:{:}", e),
        }
        let duration_proc = start.elapsed().as_millis();
        match setup_guest_dns(sl!(), req.dns.to_vec()) {
            Ok(_) => {
                let sandbox = self.sandbox.clone();
                let mut s = sandbox.lock().await;
                let _dns = req
                    .dns
                    .to_vec()
                    .iter()
                    .map(|dns| s.network.set_dns(dns.to_string()));
            }
            Err(e) => {
                return Err(ttrpc_error!(
                    ttrpc::Code::INTERNAL,
                    format!("setup dns failed:{:?}", e)
                ))
            }
        };

        info!(
            sl!(),
            "create sandbox!, config net:{}ms, add storage:{}ms, write proc:{}ms",
            duration_net,
            duration_storage,
            duration_proc,
        );

        Ok(Empty::new())
    }

    async fn destroy_sandbox(
        &self,
        _: &TtrpcContext,
        _: protocols::agent::DestroySandboxRequest,
    ) -> ttrpc::Result<Empty> {
        info!(sl!(), "receive destroy sandbox");

        let s = Arc::clone(&self.sandbox);
        let mut sandbox = s.lock().await;

        sandbox
            .sender
            .take()
            .ok_or_else(|| {
                ttrpc_error!(
                    ttrpc::Code::INTERNAL,
                    "failed to get sandbox sender channel".to_string(),
                )
            })?
            .send(1)
            .map_err(|e| ttrpc_error!(ttrpc::Code::INTERNAL, e))?;

        Ok(Empty::new())
    }

    async fn add_arp_neighbors(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::AddARPNeighborsRequest,
    ) -> ttrpc::Result<Empty> {
        trace_rpc_call!(ctx, "add_arp_neighbors", req);
        is_allowed!(req);

        let neighs = req
            .neighbors
            .into_option()
            .map(|n| n.ARPNeighbors.to_vec())
            .ok_or_else(|| {
                ttrpc_error!(
                    ttrpc::Code::INVALID_ARGUMENT,
                    "empty add arp neighbours request".to_string(),
                )
            })?;

        self.sandbox
            .lock()
            .await
            .rtnl
            .add_arp_neighbors(neighs)
            .await
            .map_err(|e| {
                ttrpc_error!(
                    ttrpc::Code::INTERNAL,
                    format!("Failed to add ARP neighbours: {:?}", e),
                )
            })?;

        Ok(Empty::new())
    }

    async fn online_cpu_mem(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::OnlineCPUMemRequest,
    ) -> ttrpc::Result<Empty> {
        is_allowed!(req);
        let s = Arc::clone(&self.sandbox);
        let sandbox = s.lock().await;
        trace_rpc_call!(ctx, "online_cpu_mem", req);

        sandbox
            .online_cpu_memory(&req)
            .map_err(|e| ttrpc_error!(ttrpc::Code::INTERNAL, e))?;

        Ok(Empty::new())
    }

    async fn reseed_random_dev(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::ReseedRandomDevRequest,
    ) -> ttrpc::Result<Empty> {
        trace_rpc_call!(ctx, "reseed_random_dev", req);
        is_allowed!(req);

        random::reseed_rng(req.data.as_slice())
            .map_err(|e| ttrpc_error!(ttrpc::Code::INTERNAL, e))?;

        Ok(Empty::new())
    }

    async fn get_guest_details(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::GuestDetailsRequest,
    ) -> ttrpc::Result<GuestDetailsResponse> {
        trace_rpc_call!(ctx, "get_guest_details", req);
        is_allowed!(req);

        debug!(sl!(), "get guest details!");
        let mut resp = GuestDetailsResponse::new();
        // to get memory block size
        match get_memory_info(
            req.mem_block_size,
            req.mem_hotplug_probe,
            SYSFS_MEMORY_BLOCK_SIZE_PATH,
            SYSFS_MEMORY_HOTPLUG_PROBE_PATH,
        ) {
            Ok((u, v)) => {
                resp.mem_block_size_bytes = u;
                resp.support_mem_hotplug_probe = v;
            }
            Err(e) => {
                info!(sl!(), "fail to get memory info!");
                return Err(ttrpc_error!(ttrpc::Code::INTERNAL, e));
            }
        }

        // to get agent details
        let detail = get_agent_details();
        resp.agent_details = MessageField::some(detail);

        Ok(resp)
    }

    async fn mem_hotplug_by_probe(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::MemHotplugByProbeRequest,
    ) -> ttrpc::Result<Empty> {
        trace_rpc_call!(ctx, "mem_hotplug_by_probe", req);
        is_allowed!(req);

        do_mem_hotplug_by_probe(&req.memHotplugProbeAddr)
            .map_err(|e| ttrpc_error!(ttrpc::Code::INTERNAL, e))?;

        Ok(Empty::new())
    }

    async fn set_guest_date_time(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::SetGuestDateTimeRequest,
    ) -> ttrpc::Result<Empty> {
        trace_rpc_call!(ctx, "set_guest_date_time", req);
        is_allowed!(req);

        do_set_guest_date_time(req.Sec, req.Usec)
            .map_err(|e| ttrpc_error!(ttrpc::Code::INTERNAL, e))?;

        Ok(Empty::new())
    }

    async fn copy_file(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::CopyFileRequest,
    ) -> ttrpc::Result<Empty> {
        trace_rpc_call!(ctx, "copy_file", req);
        is_allowed!(req);

        do_copy_file(&req).map_err(|e| ttrpc_error!(ttrpc::Code::INTERNAL, e))?;

        Ok(Empty::new())
    }

    async fn get_metrics(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::GetMetricsRequest,
    ) -> ttrpc::Result<Metrics> {
        trace_rpc_call!(ctx, "get_metrics", req);
        is_allowed!(req);

        match get_metrics(&req) {
            Err(e) => Err(ttrpc_error!(ttrpc::Code::INTERNAL, e)),
            Ok(s) => {
                let mut metrics = Metrics::new();
                metrics.set_metrics(s);
                Ok(metrics)
            }
        }
    }

    async fn get_oom_event(
        &self,
        _ctx: &TtrpcContext,
        req: protocols::agent::GetOOMEventRequest,
    ) -> ttrpc::Result<OOMEvent> {
        is_allowed!(req);
        let mut rx = {
            let sandbox = self.sandbox.clone();
            let s = sandbox.lock().await;
            if s.event_tx.is_none() {
                return Err(ttrpc_error!(ttrpc::Code::INTERNAL, ""));
            }

            let rx = s.event_tx.as_ref().unwrap().subscribe();
            rx
        };

        if let Ok(container_id) = rx.recv().await {
            info!(sl!(), "get_oom_event return {}", &container_id);

            let mut resp = OOMEvent::new();
            resp.container_id = container_id;

            return Ok(resp);
        }

        Err(ttrpc_error!(ttrpc::Code::INTERNAL, ""))
    }

    async fn get_volume_stats(
        &self,
        ctx: &TtrpcContext,
        req: VolumeStatsRequest,
    ) -> ttrpc::Result<VolumeStatsResponse> {
        trace_rpc_call!(ctx, "get_volume_stats", req);
        is_allowed!(req);

        info!(sl!(), "get volume stats!");
        let mut resp = VolumeStatsResponse::new();

        let mut condition = VolumeCondition::new();

        match File::open(&req.volume_guest_path) {
            Ok(_) => {
                condition.abnormal = false;
                condition.message = String::from("OK");
            }
            Err(e) => {
                info!(sl!(), "failed to open the volume");
                return Err(ttrpc_error!(ttrpc::Code::INTERNAL, e));
            }
        };

        let mut usage_vec = Vec::new();

        // to get volume capacity stats
        get_volume_capacity_stats(&req.volume_guest_path)
            .map(|u| usage_vec.push(u))
            .map_err(|e| ttrpc_error!(ttrpc::Code::INTERNAL, e))?;

        // to get volume inode stats
        get_volume_inode_stats(&req.volume_guest_path)
            .map(|u| usage_vec.push(u))
            .map_err(|e| ttrpc_error!(ttrpc::Code::INTERNAL, e))?;

        resp.usage = usage_vec;
        resp.volume_condition = MessageField::some(condition);
        Ok(resp)
    }

    async fn add_swap(
        &self,
        ctx: &TtrpcContext,
        req: protocols::agent::AddSwapRequest,
    ) -> ttrpc::Result<Empty> {
        trace_rpc_call!(ctx, "add_swap", req);
        is_allowed!(req);

        do_add_swap(&self.sandbox, &req)
            .await
            .map_err(|e| ttrpc_error!(ttrpc::Code::INTERNAL, e))?;

        Ok(Empty::new())
    }
}

#[derive(Clone)]
struct HealthService;

#[async_trait]
impl protocols::health_ttrpc::Health for HealthService {
    async fn check(
        &self,
        _ctx: &TtrpcContext,
        _req: protocols::health::CheckRequest,
    ) -> ttrpc::Result<HealthCheckResponse> {
        let mut resp = HealthCheckResponse::new();
        resp.set_status(ServingStatus::SERVING);

        Ok(resp)
    }

    async fn version(
        &self,
        _ctx: &TtrpcContext,
        req: protocols::health::CheckRequest,
    ) -> ttrpc::Result<VersionCheckResponse> {
        info!(sl!(), "version {:?}", req);
        let mut rep = protocols::health::VersionCheckResponse::new();
        rep.agent_version = AGENT_VERSION.to_string();
        rep.grpc_version = API_VERSION.to_string();

        Ok(rep)
    }
}

fn get_memory_info(
    block_size: bool,
    hotplug: bool,
    block_size_path: &str,
    hotplug_probe_path: &str,
) -> Result<(u64, bool)> {
    let mut size: u64 = 0;
    let mut plug: bool = false;
    if block_size {
        match fs::read_to_string(block_size_path) {
            Ok(v) => {
                if v.is_empty() {
                    warn!(sl!(), "file {} is empty", block_size_path);
                    return Err(anyhow!(ERR_INVALID_BLOCK_SIZE));
                }

                size = u64::from_str_radix(v.trim(), 16).map_err(|_| {
                    warn!(sl!(), "failed to parse the str {} to hex", size);
                    anyhow!(ERR_INVALID_BLOCK_SIZE)
                })?;
            }
            Err(e) => {
                warn!(sl!(), "memory block size error: {:?}", e.kind());
                if e.kind() != std::io::ErrorKind::NotFound {
                    return Err(anyhow!(e));
                }
            }
        }
    }

    if hotplug {
        match stat::stat(hotplug_probe_path) {
            Ok(_) => plug = true,
            Err(e) => {
                debug!(sl!(), "hotplug memory error: {:?}", e);
                match e {
                    nix::Error::ENOENT => plug = false,
                    _ => return Err(anyhow!(e)),
                }
            }
        }
    }

    Ok((size, plug))
}

fn get_volume_capacity_stats(path: &str) -> Result<VolumeUsage> {
    let mut usage = VolumeUsage::new();

    let stat = statfs::statfs(path)?;
    let block_size = stat.block_size() as u64;
    usage.total = stat.blocks() * block_size;
    usage.available = stat.blocks_free() * block_size;
    usage.used = usage.total - usage.available;
    usage.unit = volume_usage::Unit::BYTES.into();

    Ok(usage)
}

fn get_volume_inode_stats(path: &str) -> Result<VolumeUsage> {
    let mut usage = VolumeUsage::new();

    let stat = statfs::statfs(path)?;
    usage.total = stat.files();
    usage.available = stat.files_free();
    usage.used = usage.total - usage.available;
    usage.unit = volume_usage::Unit::INODES.into();

    Ok(usage)
}

pub fn have_seccomp() -> bool {
    if cfg!(feature = "seccomp") {
        return true;
    }

    false
}

fn get_agent_details() -> AgentDetails {
    let mut detail = AgentDetails::new();

    detail.set_version(AGENT_VERSION.to_string());
    detail.set_supports_seccomp(have_seccomp());
    detail.init_daemon = unistd::getpid() == Pid::from_raw(1);

    detail.device_handlers = Vec::new();
    detail.storage_handlers = STORAGE_HANDLER_LIST
        .to_vec()
        .iter()
        .map(|x| x.to_string())
        .collect();

    detail
}

async fn read_stream(reader: Arc<Mutex<ReadHalf<PipeStream>>>, l: usize) -> Result<Vec<u8>> {
    let mut content = vec![0u8; l];

    let mut reader = reader.lock().await;
    let len = reader.read(&mut content).await?;
    content.resize(len, 0);

    if len == 0 {
        return Err(anyhow!("read meet eof"));
    }

    Ok(content)
}

pub fn start(s: Arc<Mutex<Sandbox>>, server_address: &str) -> Result<TtrpcServer> {
    let agent_worker = Arc::new(AgentService { sandbox: s });

    let health_worker = Arc::new(HealthService {});

    let aservice = protocols::agent_ttrpc::create_agent_service(agent_worker);

    let hservice = protocols::health_ttrpc::create_health(health_worker);

    let server = TtrpcServer::new()
        .bind(server_address)?
        .register_service(aservice)
        .register_service(hservice);
    println!(
        "ttRPC server started at:{}",
        moniclock::Clock::new().elapsed().as_millis()
    );

    let port: u16 = 0x680;
    let data: u8 = 0x8;
    unsafe {
        libc::ioperm(port as u64, 5, 1);
    }
    let mut ioport = x86_64::instructions::port::Port::new(port);

    unsafe {
        ioport.write(data);
    }

    Ok(server)
}

// This function updates the container namespaces configuration based on the
// sandbox information. When the sandbox is created, it can be setup in a way
// that all containers will share some specific namespaces. This is the agent
// responsibility to create those namespaces so that they can be shared across
// several containers.
// If the sandbox has not been setup to share namespaces, then we assume all
// containers will be started in their own new namespace.
// The value of a.sandbox.sharedPidNs.path will always override the namespace
// path set by the spec, since we will always ignore it. Indeed, it makes no
// sense to rely on the namespace path provided by the host since namespaces
// are different inside the guest.
fn update_container_namespaces(
    sandbox: &Sandbox,
    spec: &mut Spec,
    sandbox_pidns: bool,
) -> Result<()> {
    let linux = spec
        .linux
        .as_mut()
        .ok_or_else(|| anyhow!(ERR_NO_LINUX_FIELD))?;

    let namespaces = linux.namespaces.as_mut_slice();
    for namespace in namespaces.iter_mut() {
        if namespace.r#type == NSTYPEIPC {
            namespace.path = sandbox.shared_ipcns.path.clone();
            continue;
        }
        if namespace.r#type == NSTYPEUTS {
            namespace.path = sandbox.shared_utsns.path.clone();
            continue;
        }
    }
    // update pid namespace
    let mut pid_ns = LinuxNamespace {
        r#type: NSTYPEPID.to_string(),
        ..Default::default()
    };

    // Use shared pid ns if useSandboxPidns has been set in either
    // the create_sandbox request or create_container request.
    // Else set this to empty string so that a new pid namespace is
    // created for the container.
    if sandbox_pidns {
        if let Some(ref pidns) = &sandbox.sandbox_pidns {
            pid_ns.path = String::from(pidns.path.as_str());
        } else {
            return Err(anyhow!(ERR_NO_SANDBOX_PIDNS));
        }
    }

    linux.namespaces.push(pid_ns);
    Ok(())
}

fn append_guest_hooks(s: &Sandbox, oci: &mut Spec) -> Result<()> {
    if let Some(ref guest_hooks) = s.hooks {
        let mut hooks = oci.hooks.take().unwrap_or_default();
        hooks.prestart.append(&mut guest_hooks.prestart.clone());
        hooks.poststart.append(&mut guest_hooks.poststart.clone());
        hooks.poststop.append(&mut guest_hooks.poststop.clone());
        oci.hooks = Some(hooks);
    }

    Ok(())
}

// Check if the container process installed the
// handler for specific signal.
fn is_signal_handled(proc_status_file: &str, signum: u32) -> bool {
    let shift_count: u64 = if signum == 0 {
        // signum 0 is used to check for process liveness.
        // Since that signal is not part of the mask in the file, we only need
        // to know if the file (and therefore) process exists to handle
        // that signal.
        return fs::metadata(proc_status_file).is_ok();
    } else if signum > 64 {
        // Ensure invalid signum won't break bit shift logic
        warn!(sl!(), "received invalid signum {}", signum);
        return false;
    } else {
        (signum - 1).into()
    };

    // Open the file in read-only mode (ignoring errors).
    let file = match File::open(proc_status_file) {
        Ok(f) => f,
        Err(_) => {
            warn!(sl!(), "failed to open file {}", proc_status_file);
            return false;
        }
    };

    let sig_mask: u64 = 1 << shift_count;
    let reader = BufReader::new(file);

    // read lines start with SigBlk/SigIgn/SigCgt and check any match the signal mask
    reader
        .lines()
        .flatten()
        .filter(|line| {
            line.starts_with("SigBlk:")
                || line.starts_with("SigIgn:")
                || line.starts_with("SigCgt:")
        })
        .any(|line| {
            let mask_vec: Vec<&str> = line.split(':').collect();
            if mask_vec.len() == 2 {
                let sig_str = mask_vec[1].trim();
                if let Ok(sig) = u64::from_str_radix(sig_str, 16) {
                    return sig & sig_mask == sig_mask;
                }
            }
            false
        })
}

fn do_mem_hotplug_by_probe(addrs: &[u64]) -> Result<()> {
    for addr in addrs.iter() {
        fs::write(SYSFS_MEMORY_HOTPLUG_PROBE_PATH, format!("{:#X}", *addr))?;
    }
    Ok(())
}

fn do_set_guest_date_time(sec: i64, usec: i64) -> Result<()> {
    let tv = libc::timeval {
        tv_sec: sec,
        tv_usec: usec,
    };

    let ret = unsafe {
        libc::settimeofday(
            &tv as *const libc::timeval,
            std::ptr::null::<libc::timezone>(),
        )
    };

    Errno::result(ret).map(drop)?;

    Ok(())
}

fn do_copy_file(req: &CopyFileRequest) -> Result<()> {
    let path = PathBuf::from(req.path.as_str());

    if !path.starts_with(CONTAINER_BASE) {
        return Err(anyhow!(nix::Error::EINVAL));
    }

    let parent = path.parent();

    let dir = if let Some(parent) = parent {
        parent.to_path_buf()
    } else {
        PathBuf::from("/")
    };

    fs::create_dir_all(&dir).or_else(|e| {
        if e.kind() != std::io::ErrorKind::AlreadyExists {
            return Err(e);
        }

        Ok(())
    })?;

    std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(req.dir_mode))?;

    let mut tmpfile = path.clone();
    tmpfile.set_extension("tmp");

    let file = OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(false)
        .open(&tmpfile)?;

    file.write_all_at(req.data.as_slice(), req.offset as u64)?;
    let st = stat::stat(&tmpfile)?;

    if st.st_size != req.file_size {
        return Ok(());
    }

    file.set_permissions(std::fs::Permissions::from_mode(req.file_mode))?;

    unistd::chown(
        &tmpfile,
        Some(Uid::from_raw(req.uid as u32)),
        Some(Gid::from_raw(req.gid as u32)),
    )?;

    fs::rename(tmpfile, path)?;

    Ok(())
}

async fn do_add_swap(sandbox: &Arc<Mutex<Sandbox>>, req: &AddSwapRequest) -> Result<()> {
    let mut slots = Vec::new();
    for slot in &req.PCIPath {
        slots.push(pci::SlotFn::new(*slot, 0)?);
    }
    let pcipath = pci::Path::new(slots)?;
    let dev_name = get_virtio_blk_pci_device_name(sandbox, &pcipath).await?;

    let c_str = CString::new(dev_name)?;
    let ret = unsafe { libc::swapon(c_str.as_ptr() as *const c_char, 0) };
    if ret != 0 {
        return Err(anyhow!(
            "libc::swapon get error {}",
            io::Error::last_os_error()
        ));
    }

    Ok(())
}

async fn do_enable_cube_mvm_monitor() -> Result<()> {
    const PATH: &str = "/sys/kernel/cube_mon/enabled";
    let mut file = File::options().read(true).write(true).open(PATH)?;
    file.write_all(b"1")?;
    Ok(())
}

// Setup container bundle under CONTAINER_BASE, which is cleaned up
// before removing a container.
// - bundle path is /<CONTAINER_BASE>/<cid>/
// - config.json at /<CONTAINER_BASE>/<cid>/config.json
// - container rootfs bind mounted at /<CONTAINER_BASE>/<cid>/rootfs
// - modify container spec root to point to /<CONTAINER_BASE>/<cid>/rootfs
pub fn setup_bundle(
    cid: &str,
    spec: &mut Spec,
    cust_files: Vec<agent::CustomFile>,
) -> Result<PathBuf> {
    let lowerdir;
    if let Some(ri_str) = spec.annotations.get(rootfs::ANNOTATION_K_ROOTFS_INFO) {
        info!(sl!(), "annotation rootfs");
        let ri = rootfs::RootfsInfo::new(ri_str).map_err(|e| anyhow!("{}", e))?;

        if ri.pmem_file.is_some() {
            lowerdir = PathBuf::from(ri.pmem_file.unwrap().clone())
                .to_str()
                .unwrap()
                .to_string();
        } else if ri.ero_image.is_some() {
            info!(sl!(), "ero image");
            let mut lower_dirs: Vec<String> = Vec::new();
            let ero_image = ri.ero_image.unwrap();
            for lower in ero_image.lower_dir.iter() {
                let mut dir = PathBuf::from(ero_image.path.clone());
                let low = lower.trim_start_matches('/');
                dir.push(low);
                lower_dirs.push(dir.to_str().unwrap().to_string());
            }
            lowerdir = lower_dirs.join(":");
        } else {
            let mut lower_dirs: Vec<String> = Vec::new();

            if ri.overlay_info.is_none() {
                return Err(anyhow!(format!("overlay info is none")));
            }
            for d in ri.overlay_info.unwrap().virtiofs_lower_dir.iter() {
                lower_dirs.push(d.clone());
            }
            lowerdir = lower_dirs.join(":");
        }
    } else {
        let spec_root = if let Some(sr) = &spec.root {
            sr
        } else {
            return Err(anyhow!(nix::Error::EINVAL));
        };
        lowerdir = spec_root.path.clone();
    }

    let bundle_path = Path::new(CONTAINER_BASE).join(cid);
    let config_path = bundle_path.join("config.json");
    let rootfs_path = bundle_path.join("rootfs");
    let overlay_path = bundle_path.join("overlay");
    let mut work_dir = overlay_path.join("work");
    let mut upper_dir = overlay_path.join("upper");
    let mut opt = fmt::format(format_args!(
        "workdir={},upperdir={},lowerdir={}",
        work_dir.to_str().unwrap(),
        upper_dir.to_str().unwrap(),
        lowerdir,
    ));

    fs::create_dir_all(&rootfs_path)?;
    let mut read_only = true;
    if let Some(wl_path) = spec.annotations.get(ANNOTATION_K_ROOTFS_WL_PATH) {
        read_only = false;
        let blk_path = Path::new(wl_path);
        work_dir = blk_path.join("work");
        if let Ok(_) = fs::metadata(work_dir.clone()) {
            warn!(sl!(), "work exists in blk");
        }

        fs::create_dir_all(&work_dir)
            .map_err(|e| anyhow!(e).context("Failed to create work dir for overlayfs"))?;
        upper_dir = blk_path.join("upper");
        if let Ok(_) = fs::metadata(upper_dir.clone()) {
            warn!(sl!(), "upper exists in blk");
        }
        fs::create_dir_all(&upper_dir)
            .map_err(|e| anyhow!(e).context("Failed to create upper dir for overlayfs"))?;
        opt = fmt::format(format_args!(
            "workdir={},upperdir={},lowerdir={}",
            work_dir.to_str().unwrap(),
            upper_dir.to_str().unwrap(),
            lowerdir,
        ));
    } else {
        fs::create_dir_all(&work_dir)
            .map_err(|e| anyhow!(e).context("Failed to create work dir for overlayfs"))?;
        fs::create_dir_all(&upper_dir)
            .map_err(|e| anyhow!(e).context("Failed to create upper dir for overlayfs"))?;
    }
    baremount(
        Path::new("overlay2"),
        &rootfs_path,
        "overlay",
        MsFlags::empty(),
        opt.as_str(),
        &sl!(),
    )
    .map_err(|e| {
        anyhow!(e).context(fmt::format(format_args!(
            "dst:{} opt:{}",
            rootfs_path.to_str().unwrap().to_string(),
            opt
        )))
    })?;
    mount_custom_file(cid, spec, cust_files)?;
    let rootfs_path_name = rootfs_path
        .to_str()
        .ok_or_else(|| anyhow!("failed to convert rootfs to unicode"))?
        .to_string();

    spec.root = Some(Root {
        path: rootfs_path_name,
        readonly: read_only,
    });

    let _ = spec.save(
        config_path
            .to_str()
            .ok_or_else(|| anyhow!("cannot convert path to unicode"))?,
    );

    let olddir = unistd::getcwd().context("cannot getcwd")?;
    unistd::chdir(
        bundle_path
            .to_str()
            .ok_or_else(|| anyhow!("cannot convert bundle path to unicode"))?,
    )?;

    Ok(olddir)
}

pub fn mount_custom_file(
    cid: &str,
    spec: &mut Spec,
    cust_files: Vec<agent::CustomFile>,
) -> Result<()> {
    let mut p = PathBuf::from(CONTAINER_CUSTOM_FILE_BASE);
    p.push(cid);

    let _ = create_dir_all(p.clone())
        .map_err(|e| anyhow!("create container custom dir failed:{:}", e))?;

    for cust_file in cust_files {
        let f = cust_file.path.trim_start_matches('/');
        let file_p = p.join(f);

        match file_p.parent() {
            Some(dir) => {
                let _ = create_dir_all(dir).map_err(|e| {
                    anyhow!(
                        "create container custom dir {} failed:{:}",
                        dir.display(),
                        e
                    )
                })?;
            }
            None => {
                return Err(anyhow!("can't get parent dir:{:}", file_p.display()));
            }
        }

        let mut file = File::create(file_p.clone())?;
        let decoded_data = STANDARD.decode(cust_file.content)?;
        file.write_all(&decoded_data)?;

        let m = Mount {
            destination: cust_file.path.clone(),
            source: file_p.as_os_str().to_str().unwrap().to_string(),
            options: vec!["bind".to_string(), "ro".to_string()],
            r#type: "bind".to_string(),
        };
        spec.mounts.push(m);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        assert_result, namespace::Namespace, protocols::agent_ttrpc::AgentService as _,
        skip_if_not_root,
    };
    use nix::mount;
    use nix::sched::{unshare, CloneFlags};
    use oci::{Hook, Hooks, Linux, LinuxNamespace};
    use tempfile::{tempdir, TempDir};
    use ttrpc::{r#async::TtrpcContext, MessageHeader};

    fn mk_ttrpc_context() -> TtrpcContext {
        TtrpcContext {
            fd: -1,
            mh: MessageHeader::default(),
            metadata: std::collections::HashMap::new(),
            timeout_nano: 0,
        }
    }

    fn create_dummy_opts() -> CreateOpts {
        let root = Root {
            path: String::from("/"),
            ..Default::default()
        };

        let spec = Spec {
            linux: Some(oci::Linux::default()),
            root: Some(root),
            ..Default::default()
        };

        CreateOpts {
            cgroup_name: "".to_string(),
            use_systemd_cgroup: false,
            no_pivot_root: false,
            no_new_keyring: false,
            spec: Some(spec),
            rootless_euid: false,
            rootless_cgroup: false,
        }
    }

    fn create_linuxcontainer() -> (LinuxContainer, TempDir) {
        let dir = tempdir().expect("failed to make tempdir");

        (
            LinuxContainer::new(
                "some_id",
                dir.path().join("rootfs").to_str().unwrap(),
                create_dummy_opts(),
                &slog_scope::logger(),
            )
            .unwrap(),
            dir,
        )
    }

    #[tokio::test]
    async fn test_append_guest_hooks() {
        let logger = slog::Logger::root(slog::Discard, o!());
        let mut s = Sandbox::new(&logger).unwrap();
        s.hooks = Some(Hooks {
            prestart: vec![Hook {
                path: "foo".to_string(),
                ..Default::default()
            }],
            ..Default::default()
        });
        let mut oci = Spec {
            ..Default::default()
        };
        append_guest_hooks(&s, &mut oci).unwrap();
        assert_eq!(s.hooks, oci.hooks);
    }

    #[tokio::test]
    async fn test_update_interface() {
        let logger = slog::Logger::root(slog::Discard, o!());
        let sandbox = Sandbox::new(&logger).unwrap();

        let agent_service = Box::new(AgentService {
            sandbox: Arc::new(Mutex::new(sandbox)),
        });

        let req = protocols::agent::UpdateInterfaceRequest::default();
        let ctx = mk_ttrpc_context();

        let result = agent_service.update_interface(&ctx, req).await;

        assert!(result.is_err(), "expected update interface to fail");
    }

    #[tokio::test]
    async fn test_update_routes() {
        let logger = slog::Logger::root(slog::Discard, o!());
        let sandbox = Sandbox::new(&logger).unwrap();

        let agent_service = Box::new(AgentService {
            sandbox: Arc::new(Mutex::new(sandbox)),
        });

        let req = protocols::agent::UpdateRoutesRequest::default();
        let ctx = mk_ttrpc_context();

        let result = agent_service.update_routes(&ctx, req).await;

        assert!(result.is_err(), "expected update routes to fail");
    }

    #[tokio::test]
    async fn test_add_arp_neighbors() {
        let logger = slog::Logger::root(slog::Discard, o!());
        let sandbox = Sandbox::new(&logger).unwrap();

        let agent_service = Box::new(AgentService {
            sandbox: Arc::new(Mutex::new(sandbox)),
        });

        let req = protocols::agent::AddARPNeighborsRequest::default();
        let ctx = mk_ttrpc_context();

        let result = agent_service.add_arp_neighbors(&ctx, req).await;

        assert!(result.is_err(), "expected add arp neighbors to fail");
    }

    #[tokio::test]
    async fn test_do_write_stream() {
        #[derive(Debug)]
        struct TestData<'a> {
            create_container: bool,
            has_fd: bool,
            has_tty: bool,
            break_pipe: bool,

            container_id: &'a str,
            exec_id: &'a str,
            data: Vec<u8>,
            result: Result<protocols::agent::WriteStreamResponse>,
        }

        impl Default for TestData<'_> {
            fn default() -> Self {
                TestData {
                    create_container: true,
                    has_fd: true,
                    has_tty: true,
                    break_pipe: false,

                    container_id: "1",
                    exec_id: "2",
                    data: vec![1, 2, 3],
                    result: Ok(WriteStreamResponse {
                        len: 3,
                        ..WriteStreamResponse::default()
                    }),
                }
            }
        }

        let tests = &[
            TestData {
                ..Default::default()
            },
            TestData {
                has_tty: false,
                ..Default::default()
            },
            TestData {
                break_pipe: true,
                result: Err(anyhow!(std::io::Error::from_raw_os_error(libc::EPIPE))),
                ..Default::default()
            },
            TestData {
                create_container: false,
                result: Err(anyhow!(crate::sandbox::ERR_INVALID_CONTAINER_ID)),
                ..Default::default()
            },
            TestData {
                container_id: "8181",
                result: Err(anyhow!(crate::sandbox::ERR_INVALID_CONTAINER_ID)),
                ..Default::default()
            },
            TestData {
                data: vec![],
                result: Ok(WriteStreamResponse {
                    len: 0,
                    ..WriteStreamResponse::default()
                }),
                ..Default::default()
            },
            TestData {
                has_fd: false,
                result: Err(anyhow!(ERR_CANNOT_GET_WRITER)),
                ..Default::default()
            },
        ];

        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);

            let logger = slog::Logger::root(slog::Discard, o!());
            let mut sandbox = Sandbox::new(&logger).unwrap();

            let (rfd, wfd) = unistd::pipe().unwrap();
            if d.break_pipe {
                unistd::close(rfd).unwrap();
            }

            if d.create_container {
                let (mut linux_container, _root) = create_linuxcontainer();
                let exec_process_id = 2;

                linux_container.id = "1".to_string();

                let mut exec_process = Process::new(
                    &logger,
                    &oci::Process::default(),
                    &exec_process_id.to_string(),
                    false,
                    1,
                )
                .unwrap();

                let fd = {
                    if d.has_fd {
                        Some(wfd)
                    } else {
                        None
                    }
                };

                if d.has_tty {
                    exec_process.parent_stdin = None;
                    exec_process.term_master = fd;
                } else {
                    exec_process.parent_stdin = fd;
                    exec_process.term_master = None;
                }
                linux_container
                    .processes
                    .insert(exec_process_id, exec_process);

                sandbox.add_container(linux_container);
            }

            let agent_service = Box::new(AgentService {
                sandbox: Arc::new(Mutex::new(sandbox)),
            });

            let result = agent_service
                .do_write_stream(protocols::agent::WriteStreamRequest {
                    container_id: d.container_id.to_string(),
                    exec_id: d.exec_id.to_string(),
                    data: d.data.clone(),
                    ..Default::default()
                })
                .await;

            if !d.break_pipe {
                unistd::close(rfd).unwrap();
            }
            unistd::close(wfd).unwrap();

            let msg = format!("{}, result: {:?}", msg, result);
            assert_result!(d.result, result, msg);
        }
    }

    #[tokio::test]
    async fn test_update_container_namespaces() {
        #[derive(Debug)]
        struct TestData<'a> {
            has_linux_in_spec: bool,
            sandbox_pidns_path: Option<&'a str>,

            namespaces: Vec<LinuxNamespace>,
            use_sandbox_pidns: bool,
            result: Result<()>,
            expected_namespaces: Vec<LinuxNamespace>,
        }

        impl Default for TestData<'_> {
            fn default() -> Self {
                TestData {
                    has_linux_in_spec: true,
                    sandbox_pidns_path: Some("sharedpidns"),
                    namespaces: vec![
                        LinuxNamespace {
                            r#type: NSTYPEIPC.to_string(),
                            path: "ipcpath".to_string(),
                        },
                        LinuxNamespace {
                            r#type: NSTYPEUTS.to_string(),
                            path: "utspath".to_string(),
                        },
                    ],
                    use_sandbox_pidns: false,
                    result: Ok(()),
                    expected_namespaces: vec![
                        LinuxNamespace {
                            r#type: NSTYPEIPC.to_string(),
                            path: "".to_string(),
                        },
                        LinuxNamespace {
                            r#type: NSTYPEUTS.to_string(),
                            path: "".to_string(),
                        },
                        LinuxNamespace {
                            r#type: NSTYPEPID.to_string(),
                            path: "".to_string(),
                        },
                    ],
                }
            }
        }

        let tests = &[
            TestData {
                ..Default::default()
            },
            TestData {
                use_sandbox_pidns: true,
                expected_namespaces: vec![
                    LinuxNamespace {
                        r#type: NSTYPEIPC.to_string(),
                        path: "".to_string(),
                    },
                    LinuxNamespace {
                        r#type: NSTYPEUTS.to_string(),
                        path: "".to_string(),
                    },
                    LinuxNamespace {
                        r#type: NSTYPEPID.to_string(),
                        path: "sharedpidns".to_string(),
                    },
                ],
                ..Default::default()
            },
            TestData {
                namespaces: vec![],
                use_sandbox_pidns: true,
                expected_namespaces: vec![LinuxNamespace {
                    r#type: NSTYPEPID.to_string(),
                    path: "sharedpidns".to_string(),
                }],
                ..Default::default()
            },
            TestData {
                namespaces: vec![],
                use_sandbox_pidns: false,
                expected_namespaces: vec![LinuxNamespace {
                    r#type: NSTYPEPID.to_string(),
                    path: "".to_string(),
                }],
                ..Default::default()
            },
            TestData {
                namespaces: vec![],
                sandbox_pidns_path: None,
                use_sandbox_pidns: true,
                result: Err(anyhow!(ERR_NO_SANDBOX_PIDNS)),
                expected_namespaces: vec![],
                ..Default::default()
            },
            TestData {
                has_linux_in_spec: false,
                result: Err(anyhow!(ERR_NO_LINUX_FIELD)),
                ..Default::default()
            },
        ];

        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);

            let logger = slog::Logger::root(slog::Discard, o!());
            let mut sandbox = Sandbox::new(&logger).unwrap();
            if let Some(pidns_path) = d.sandbox_pidns_path {
                let mut sandbox_pidns = Namespace::new(&logger);
                sandbox_pidns.path = pidns_path.to_string();
                sandbox.sandbox_pidns = Some(sandbox_pidns);
            }

            let mut oci = Spec::default();
            if d.has_linux_in_spec {
                oci.linux = Some(Linux {
                    namespaces: d.namespaces.clone(),
                    ..Default::default()
                });
            }

            let result = update_container_namespaces(&sandbox, &mut oci, d.use_sandbox_pidns);

            let msg = format!("{}, result: {:?}", msg, result);

            assert_result!(d.result, result, msg);
            if let Some(linux) = oci.linux {
                assert_eq!(d.expected_namespaces, linux.namespaces, "{}", msg);
            }
        }
    }

    #[tokio::test]
    async fn test_get_memory_info() {
        #[derive(Debug)]
        struct TestData<'a> {
            // if None is provided, no file will be generated, else the data in the Option will populate the file
            block_size_data: Option<&'a str>,

            hotplug_probe_data: bool,
            get_block_size: bool,
            get_hotplug: bool,
            result: Result<(u64, bool)>,
        }

        let tests = &[
            TestData {
                block_size_data: Some("10000000"),
                hotplug_probe_data: true,
                get_block_size: true,
                get_hotplug: true,
                result: Ok((268435456, true)),
            },
            TestData {
                block_size_data: Some("100"),
                hotplug_probe_data: false,
                get_block_size: true,
                get_hotplug: true,
                result: Ok((256, false)),
            },
            TestData {
                block_size_data: None,
                hotplug_probe_data: false,
                get_block_size: true,
                get_hotplug: true,
                result: Ok((0, false)),
            },
            TestData {
                block_size_data: Some(""),
                hotplug_probe_data: false,
                get_block_size: true,
                get_hotplug: false,
                result: Err(anyhow!(ERR_INVALID_BLOCK_SIZE)),
            },
            TestData {
                block_size_data: Some("-1"),
                hotplug_probe_data: false,
                get_block_size: true,
                get_hotplug: false,
                result: Err(anyhow!(ERR_INVALID_BLOCK_SIZE)),
            },
            TestData {
                block_size_data: Some("    "),
                hotplug_probe_data: false,
                get_block_size: true,
                get_hotplug: false,
                result: Err(anyhow!(ERR_INVALID_BLOCK_SIZE)),
            },
            TestData {
                block_size_data: Some("some data"),
                hotplug_probe_data: false,
                get_block_size: true,
                get_hotplug: false,
                result: Err(anyhow!(ERR_INVALID_BLOCK_SIZE)),
            },
            TestData {
                block_size_data: Some("some data"),
                hotplug_probe_data: true,
                get_block_size: false,
                get_hotplug: false,
                result: Ok((0, false)),
            },
            TestData {
                block_size_data: Some("100"),
                hotplug_probe_data: true,
                get_block_size: false,
                get_hotplug: false,
                result: Ok((0, false)),
            },
            TestData {
                block_size_data: Some("100"),
                hotplug_probe_data: true,
                get_block_size: false,
                get_hotplug: true,
                result: Ok((0, true)),
            },
        ];

        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);

            let dir = tempdir().expect("failed to make tempdir");
            let block_size_path = dir.path().join("block_size_bytes");
            let hotplug_probe_path = dir.path().join("probe");

            if let Some(block_size_data) = d.block_size_data {
                fs::write(&block_size_path, block_size_data).unwrap();
            }
            if d.hotplug_probe_data {
                fs::write(&hotplug_probe_path, []).unwrap();
            }

            let result = get_memory_info(
                d.get_block_size,
                d.get_hotplug,
                block_size_path.to_str().unwrap(),
                hotplug_probe_path.to_str().unwrap(),
            );

            let msg = format!("{}, result: {:?}", msg, result);

            assert_result!(d.result, result, msg);
        }
    }

    #[tokio::test]
    async fn test_is_signal_handled() {
        #[derive(Debug)]
        struct TestData<'a> {
            status_file_data: Option<&'a str>,
            signum: u32,
            result: bool,
        }

        let tests = &[
            TestData {
                status_file_data: Some(
                    r#"
SigBlk:0000000000010000
SigCgt:0000000000000001
OtherField:other
                "#,
                ),
                signum: 1,
                result: true,
            },
            TestData {
                status_file_data: Some("SigCgt:000000004b813efb"),
                signum: 4,
                result: true,
            },
            TestData {
                status_file_data: Some("SigCgt:\t000000004b813efb"),
                signum: 4,
                result: true,
            },
            TestData {
                status_file_data: Some("SigCgt: 000000004b813efb"),
                signum: 4,
                result: true,
            },
            TestData {
                status_file_data: Some("SigCgt:000000004b813efb "),
                signum: 4,
                result: true,
            },
            TestData {
                status_file_data: Some("SigCgt:\t000000004b813efb "),
                signum: 4,
                result: true,
            },
            TestData {
                status_file_data: Some("SigCgt:000000004b813efb"),
                signum: 3,
                result: false,
            },
            TestData {
                status_file_data: Some("SigCgt:000000004b813efb"),
                signum: 65,
                result: false,
            },
            TestData {
                status_file_data: Some("SigCgt:000000004b813efb"),
                signum: 0,
                result: true,
            },
            TestData {
                status_file_data: Some("SigCgt:ZZZZZZZZ"),
                signum: 1,
                result: false,
            },
            TestData {
                status_file_data: Some("SigCgt:-1"),
                signum: 1,
                result: false,
            },
            TestData {
                status_file_data: Some("SigCgt"),
                signum: 1,
                result: false,
            },
            TestData {
                status_file_data: Some("any data"),
                signum: 0,
                result: true,
            },
            TestData {
                status_file_data: Some("SigBlk:0000000000000001"),
                signum: 1,
                result: true,
            },
            TestData {
                status_file_data: Some("SigIgn:0000000000000001"),
                signum: 1,
                result: true,
            },
            TestData {
                status_file_data: None,
                signum: 1,
                result: false,
            },
            TestData {
                status_file_data: None,
                signum: 0,
                result: false,
            },
        ];

        for (i, d) in tests.iter().enumerate() {
            let msg = format!("test[{}]: {:?}", i, d);

            let dir = tempdir().expect("failed to make tempdir");
            let proc_status_file_path = dir.path().join("status");

            if let Some(file_data) = d.status_file_data {
                fs::write(&proc_status_file_path, file_data).unwrap();
            }

            let result = is_signal_handled(proc_status_file_path.to_str().unwrap(), d.signum);

            let msg = format!("{}, result: {:?}", msg, result);

            assert_eq!(d.result, result, "{}", msg);
        }
    }

    #[tokio::test]
    async fn test_volume_capacity_stats() {
        skip_if_not_root!();

        // Verify error if path does not exist
        assert!(get_volume_capacity_stats("/does-not-exist").is_err());

        // Create a new tmpfs mount, and verify the initial values
        let mount_dir = tempfile::tempdir().unwrap();
        mount::mount(
            Some("tmpfs"),
            mount_dir.path().to_str().unwrap(),
            Some("tmpfs"),
            mount::MsFlags::empty(),
            None::<&str>,
        )
        .unwrap();
        let mut stats = get_volume_capacity_stats(mount_dir.path().to_str().unwrap()).unwrap();
        assert_eq!(stats.used, 0);
        assert_ne!(stats.available, 0);
        let available = stats.available;

        // Verify that writing a file will result in increased utilization
        fs::write(mount_dir.path().join("file.dat"), "foobar").unwrap();
        stats = get_volume_capacity_stats(mount_dir.path().to_str().unwrap()).unwrap();

        assert_eq!(stats.used, 4 * 1024);
        assert_eq!(stats.available, available - 4 * 1024);
    }

    #[tokio::test]
    async fn test_get_volume_inode_stats() {
        skip_if_not_root!();

        // Verify error if path does not exist
        assert!(get_volume_inode_stats("/does-not-exist").is_err());

        // Create a new tmpfs mount, and verify the initial values
        let mount_dir = tempfile::tempdir().unwrap();
        mount::mount(
            Some("tmpfs"),
            mount_dir.path().to_str().unwrap(),
            Some("tmpfs"),
            mount::MsFlags::empty(),
            None::<&str>,
        )
        .unwrap();
        let mut stats = get_volume_inode_stats(mount_dir.path().to_str().unwrap()).unwrap();
        assert_eq!(stats.used, 1);
        assert_ne!(stats.available, 0);
        let available = stats.available;

        // Verify that creating a directory and writing a file will result in increased utilization
        let dir = mount_dir.path().join("foobar");
        fs::create_dir_all(&dir).unwrap();
        fs::write(dir.as_path().join("file.dat"), "foobar").unwrap();
        stats = get_volume_inode_stats(mount_dir.path().to_str().unwrap()).unwrap();

        assert_eq!(stats.used, 3);
        assert_eq!(stats.available, available - 2);
    }

    #[tokio::test]
    async fn test_ip_tables() {
        skip_if_not_root!();

        let logger = slog::Logger::root(slog::Discard, o!());
        let sandbox = Sandbox::new(&logger).unwrap();
        let agent_service = Box::new(AgentService {
            sandbox: Arc::new(Mutex::new(sandbox)),
        });

        let ctx = mk_ttrpc_context();

        // Move to a new netns in order to ensure we don't trash the hosts' iptables
        unshare(CloneFlags::CLONE_NEWNET).unwrap();

        // Get initial iptables, we expect to be empty:
        let result = agent_service
            .get_ip_tables(
                &ctx,
                GetIPTablesRequest {
                    is_ipv6: false,
                    ..Default::default()
                },
            )
            .await;
        assert!(result.is_ok(), "get ip tables should succeed");
        assert_eq!(
            result.unwrap().data.len(),
            0,
            "ip tables should be empty initially"
        );

        // Initial ip6 ip tables should also be empty:
        let result = agent_service
            .get_ip_tables(
                &ctx,
                GetIPTablesRequest {
                    is_ipv6: true,
                    ..Default::default()
                },
            )
            .await;
        assert!(result.is_ok(), "get ip6 tables should succeed");
        assert_eq!(
            result.unwrap().data.len(),
            0,
            "ip tables should be empty initially"
        );

        // Verify that attempting to write 'empty' iptables results in no error:
        let empty_rules = "";
        let result = agent_service
            .set_ip_tables(
                &ctx,
                SetIPTablesRequest {
                    is_ipv6: false,
                    data: empty_rules.as_bytes().to_vec(),
                    ..Default::default()
                },
            )
            .await;
        assert!(result.is_ok(), "set ip tables with no data should succeed");

        // Verify that attempting to write "garbage" iptables results in an error:
        let garbage_rules = r#"
this
is
just garbage
"#;
        let result = agent_service
            .set_ip_tables(
                &ctx,
                SetIPTablesRequest {
                    is_ipv6: false,
                    data: garbage_rules.as_bytes().to_vec(),
                    ..Default::default()
                },
            )
            .await;
        assert!(result.is_err(), "set iptables with garbage should fail");

        // Verify setup of valid iptables:Setup  valid set of iptables:
        let valid_rules = r#"
*nat
-A PREROUTING -d 192.168.103.153/32 -j DNAT --to-destination 192.168.188.153

COMMIT

"#;
        let result = agent_service
            .set_ip_tables(
                &ctx,
                SetIPTablesRequest {
                    is_ipv6: false,
                    data: valid_rules.as_bytes().to_vec(),
                    ..Default::default()
                },
            )
            .await;
        assert!(result.is_ok(), "set ip tables should succeed");

        let result = agent_service
            .get_ip_tables(
                &ctx,
                GetIPTablesRequest {
                    is_ipv6: false,
                    ..Default::default()
                },
            )
            .await
            .unwrap();
        assert!(!result.data.is_empty(), "we should have non-zero output:");
        assert!(
            std::str::from_utf8(&*result.data).unwrap().contains(
                "PREROUTING -d 192.168.103.153/32 -j DNAT --to-destination 192.168.188.153"
            ),
            "We should see the resulting rule"
        );

        // Verify setup of valid ip6tables:
        let valid_ipv6_rules = r#"
*filter
-A INPUT -s 2001:db8:100::1/128 -i sit+ -p tcp -m tcp --sport 512:65535

COMMIT

"#;
        let result = agent_service
            .set_ip_tables(
                &ctx,
                SetIPTablesRequest {
                    is_ipv6: true,
                    data: valid_ipv6_rules.as_bytes().to_vec(),
                    ..Default::default()
                },
            )
            .await;
        assert!(result.is_ok(), "set ip6 tables should succeed");

        let result = agent_service
            .get_ip_tables(
                &ctx,
                GetIPTablesRequest {
                    is_ipv6: true,
                    ..Default::default()
                },
            )
            .await
            .unwrap();
        assert!(!result.data.is_empty(), "we should have non-zero output:");
        assert!(
            std::str::from_utf8(&*result.data)
                .unwrap()
                .contains("INPUT -s 2001:db8:100::1/128 -i sit+ -p tcp -m tcp --sport 512:65535"),
            "We should see the resulting rule"
        );
    }
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::common::CResult;
use crate::hypervisor::config::{HypConfig, VmConfig};
use crate::infof;
use crate::log::stat_defer::StatDefer;
use crate::log::{stat_defer, Log};
use cube_hypervisor::config::RestoreConfig;
use cube_hypervisor::vm_config::{DeviceConfig, FsConfig};
use cube_hypervisor::{
    self, config, vmm_config, ApiRequest, ApiResponsePayload, SnapshotConfig, SnapshotType,
    VmRemoveDeviceData,
};
use std::sync::mpsc::{channel, Receiver};
use std::time::Duration;
use std::{fmt, sync::Arc};
use tokio::sync::Mutex;

pub use cube_hypervisor::NotifyEvent;

use super::config::PciDeviceInfo;

const CALLE_ACTION_ADD_DEV_PRE: &str = "AddDevice";

#[derive(Debug, PartialEq, Eq, Clone)]
enum HypStatus {
    Init,
    Launched,
    Running,
}
impl fmt::Display for HypStatus {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        match *self {
            HypStatus::Init => write!(f, "init"),
            HypStatus::Launched => write!(f, "launched"),
            HypStatus::Running => write!(f, "running"),
        }
    }
}

#[derive(Clone)]
pub struct CubeHypervisor {
    status: HypStatus,
    config: HypConfig,
    ch: Option<Arc<Mutex<cube_hypervisor::VmmInstance>>>,
    ev_receiver: Option<Arc<Mutex<Receiver<NotifyEvent>>>>,
    log: Log,
}

impl CubeHypervisor {
    pub fn new(config: HypConfig, log: Log) -> Self {
        CubeHypervisor {
            status: HypStatus::Init,
            ch: None,
            config: config.clone(),
            ev_receiver: None,
            log,
        }
    }

    fn status_err(&self, s: String) -> String {
        format!("{}, status: {}", s, self.status)
    }
    fn new_stat(&self, callee_act: String) -> StatDefer {
        stat_defer::StatDefer::new(
            self.config.sandbox_id.clone(),
            stat_defer::CALLEE_CH.to_string(),
            stat_defer::ACT_CREATE.to_string(),
            callee_act,
            self.log.clone(),
        )
    }
    pub async fn launch_vmm(&mut self) -> CResult<()> {
        if let Some(_ch) = &self.ch {
            return Err(self.status_err("oops: ch is not None".to_string()));
        }
        cube_hypervisor::set_runtime_seccomp_rules(vec![
            #[cfg(target_arch = "x86_64")]
            (libc::SYS_mkdir, vec![]),
            #[cfg(target_arch = "aarch64")]
            (libc::SYS_mkdirat, vec![]),
            (libc::SYS_getsockopt, vec![]),
            (libc::SYS_setsockopt, vec![]),
            (libc::SYS_faccessat2, vec![]),
        ]);
        let mut vmm_config = self.config.to_vmm_config();
        let (sender, receiver) = channel::<NotifyEvent>();
        let notifier = vmm_config::EventNotifyConfig { notifier: sender };
        vmm_config.event_notifier = Some(notifier);
        self.ev_receiver = Some(Arc::new(Mutex::new(receiver)));

        let mut stat = self.new_stat(stat_defer::CALLEE_ACT_LAUNCH_VMM.to_string());

        let ch: cube_hypervisor::VmmInstance = cube_hypervisor::VmmInstance::new(vmm_config)
            .map_err(|e| self.status_err(format!("New vmm instance failed:{}", e)))?;
        self.ch = Some(Arc::new(Mutex::new(ch)));
        self.status = HypStatus::Launched;
        stat.set_ok();
        Ok(())
    }

    pub async fn ping_vmm(&self, _timeout_ms: u64) -> CResult<()> {
        let ch = self.ch.as_ref().unwrap().lock().await;
        let _ = ch
            .send_request(ApiRequest::VmmPing)
            .map_err(|e| format!("Ping Vmm failed:{}", e))?
            .map_err(|e| format!("Ping Vmm failed:{}", e))?;
        Ok(())
    }

    pub async fn create_vm(&self, config: &VmConfig) -> CResult<()> {
        let ch = self.ch.as_ref().unwrap().lock().await;
        let vm_config = config.to_vm_config();
        let b_vm_config = Box::new(vm_config);
        let mut stat = self.new_stat(stat_defer::CALLEE_ACT_CREATE_VM.to_string());
        let _ = ch
            .send_request(ApiRequest::VmCreate(b_vm_config))
            .map_err(|e| self.status_err(format!("Create vm failed:{}", e)))?
            .map_err(|e| self.status_err(format!("Create vm failed:{}", e)))?;
        stat.set_ok();
        Ok(())
    }

    pub async fn boot_vm(&mut self) -> CResult<()> {
        let ch = self.ch.as_ref().unwrap().lock().await;
        let mut stat = self.new_stat(stat_defer::CALLEE_ACT_BOOT_VM.to_string());
        let _ = ch
            .send_request(ApiRequest::VmBoot)
            .map_err(|e| self.status_err(format!("Boot vm failed:{}", e)))?
            .map_err(|e| self.status_err(format!("Boot vm failed:{}", e)))?;
        self.status = HypStatus::Running;
        stat.set_ok();
        Ok(())
    }

    pub async fn snapshot_vm(&self, path: &str, snapshot_type: SnapshotType) -> CResult<()> {
        let ch = self.ch.as_ref().unwrap().lock().await;
        let snap_config = Arc::new(SnapshotConfig {
            destination_url: path.to_string(),
            snapshot_type,
            ..Default::default()
        });
        let _ = ch
            .send_request(ApiRequest::VmSnapshot(snap_config))
            .map_err(|e| self.status_err(format!("Snapshot vm failed:{}", e)))?
            .map_err(|e| self.status_err(format!("Snapshot vm failed:{}", e)))?;

        Ok(())
    }

    pub async fn pause_vm(&self) -> CResult<()> {
        let ch = self.ch.as_ref().unwrap().lock().await;
        let _ = ch
            .send_request(ApiRequest::VmPause)
            .map_err(|e| self.status_err(format!("Pause vm failed:{}", e)))?
            .map_err(|e| self.status_err(format!("Pause vm failed:{}", e)))?;

        Ok(())
    }

    pub async fn resume_vm(&self) -> CResult<()> {
        let ch = self.ch.as_ref().unwrap().lock().await;
        let _ = ch
            .send_request(ApiRequest::VmResume)
            .map_err(|e| self.status_err(format!("Resume vm failed:{}", e)))?
            .map_err(|e| self.status_err(format!("Resume vm failed:{}", e)))?;

        Ok(())
    }

    pub async fn restore_vm(&self, config: config::RestoreConfig) -> CResult<()> {
        let ch = self.ch.as_ref().unwrap().lock().await;
        let mut stat = self.new_stat(stat_defer::CALLEE_ACT_RESTORE_VM.to_string());
        let restore_config = Arc::new(config);
        let _ = ch
            .send_request(ApiRequest::VmRestore(restore_config))
            .map_err(|e| self.status_err(format!("Restore vm failed:{}", e)))?
            .map_err(|e| self.status_err(format!("Restore vm failed:{}", e)))?;
        stat.set_ok();
        Ok(())
    }

    pub async fn set_fs(&self, config: FsConfig) -> CResult<()> {
        infof!(self.log, "update fs allow dir start");
        let ch = self.ch.as_ref().unwrap().lock().await;
        let fs_config = Arc::new(config);
        let _ = ch
            .send_request(ApiRequest::VmSetFs(fs_config))
            .map_err(|e| self.status_err(format!("Setfs vm failed:{}", e)))?
            .map_err(|e| self.status_err(format!("Setfs vm failed:{}", e)))?;
        infof!(self.log, "update fs allow dir finish");
        Ok(())
    }

    pub async fn add_dev(&self, config: DeviceConfig) -> CResult<String> {
        let id = config.id.clone().unwrap_or("".to_string());
        let id_pre = if let Some(id_pre) = id.split_once("-") {
            id_pre.0.to_string()
        } else {
            "".to_string()
        };
        let act = format!("{}-{}", CALLE_ACTION_ADD_DEV_PRE, id_pre);
        let mut stat = self.new_stat(act);
        let ch = self.ch.as_ref().unwrap().lock().await;
        let dev_config = Arc::new(config);
        let rsp = ch
            .send_request(ApiRequest::VmAddDevice(dev_config))
            .map_err(|e| self.status_err(format!("Add device failed:{}", e)))?
            .map_err(|e| self.status_err(format!("Add device failed:{}", e)))?;

        match rsp {
            ApiResponsePayload::VmAction(Some(payload)) => {
                let ret = serde_json::from_slice::<PciDeviceInfo>(&payload).unwrap();
                return Ok(ret.bdf.to_string());
            }
            _ => {}
        }
        stat.set_ok();
        Ok("".to_string())
    }

    pub async fn remove_dev(&self, config: VmRemoveDeviceData) -> CResult<()> {
        infof!(self.log, "remove dev:{} start", config.id.clone());
        let ch = self.ch.as_ref().unwrap().lock().await;
        let dev_config = Arc::new(config);
        let _ = ch
            .send_request(ApiRequest::VmRemoveDevice(dev_config))
            .map_err(|e| self.status_err(format!("Rm device failed:{}", e)))?
            .map_err(|e| self.status_err(format!("Rm device failed:{}", e)))?;
        infof!(self.log, "remove dev finish");
        Ok(())
    }

    pub async fn stop_vm(&self) -> CResult<()> {
        Ok(())
    }

    /// Delete the current VM (cube-hypervisor `VmDelete`).
    /// The VMM shuts down the VM if still running, then destroys the VM object.
    /// After this call the hypervisor process is still alive and can host a new VM
    /// (e.g. restored from a snapshot via `resume_vm_cube_with_config`).
    pub async fn delete_vm(&self) -> CResult<()> {
        let ch = self.ch.as_ref().unwrap().lock().await;
        let _ = ch
            .send_request(ApiRequest::VmDelete)
            .map_err(|e| self.status_err(format!("Delete vm failed:{}", e)))?
            .map_err(|e| self.status_err(format!("Delete vm failed:{}", e)))?;
        Ok(())
    }

    pub async fn wait_notify(&self, timeout: Duration) -> CResult<NotifyEvent> {
        if let Some(recv) = &self.ev_receiver {
            let rx = recv.lock().await;
            return tokio::task::block_in_place(move || match rx.recv_timeout(timeout) {
                Ok(ev) => Ok(ev),
                Err(_e) => Err(format!(
                    "Receive event timeout after {}ms",
                    timeout.as_millis()
                )),
            });
        }
        Err("Receiver is uninitialized".to_string())
    }

    pub fn try_wait_notify(&self) -> CResult<NotifyEvent> {
        if let Some(recv) = &self.ev_receiver {
            let rx = {
                if let Ok(rx) = recv.try_lock() {
                    rx
                } else {
                    return Err("Receiver is busying".to_string());
                }
            };
            return match rx.try_recv() {
                Ok(ev) => Ok(ev),
                Err(e) => Err(format!("Receive event failed:{}", e)),
            };
        }
        Err("Receiver is uninitialized".to_string())
    }

    pub async fn join(&mut self) -> CResult<()> {
        let mut ch = self.ch.as_mut().unwrap().lock().await;
        ch.join().map_err(|e| format!("join ch failed:{}", e))
    }

    pub async fn pause_vm_cube(&self, path: &str) -> CResult<()> {
        let snap_config = Arc::new(SnapshotConfig {
            destination_url: path.to_string(),
            ..Default::default()
        });
        let ch = self.ch.as_ref().unwrap().lock().await;
        let _ = ch
            .send_request(ApiRequest::VmPauseToSnapshot(snap_config))
            .map_err(|e| self.status_err(format!("pause vm to snapshot failed:{}", e)))?
            .map_err(|e| self.status_err(format!("pause vm to snapshot failed:{}", e)))?;

        Ok(())
    }

    pub async fn resume_vm_cube(&self, path: &str) -> CResult<()> {
        let restore_config = Arc::new(RestoreConfig {
            source_url: path.into(),
            ..Default::default()
        });
        let ch = self.ch.as_ref().unwrap().lock().await;
        let _ = ch
            .send_request(ApiRequest::VmResumeFromSnapshot(restore_config))
            .map_err(|e| self.status_err(format!("resume vm from snapshot failed:{}", e)))?
            .map_err(|e| self.status_err(format!("resume vm from snapshot failed:{}", e)))?;

        Ok(())
    }

    pub async fn resume_vm_cube_with_config(&self, config: RestoreConfig) -> CResult<()> {
        let restore_config = Arc::new(config);
        let ch = self.ch.as_ref().unwrap().lock().await;
        let _ = ch
            .send_request(ApiRequest::VmResumeFromSnapshot(restore_config))
            .map_err(|e| self.status_err(format!("resume vm from snapshot failed:{}", e)))?
            .map_err(|e| self.status_err(format!("resume vm from snapshot failed:{}", e)))?;

        Ok(())
    }
}

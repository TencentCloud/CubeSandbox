// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::common::utils::Utils;
use crate::common::utils::VM_PATH;
use crate::common::CResult;
use crate::hypervisor::config::VmConfig;
use crate::hypervisor::snapshot::{self, SnapshotInfo};
use crate::sandbox::config::{Fs, VmResource, SHARE_CACHE_NEVER};
use crate::sandbox::disk::Disk;

use crate::sandbox::net::Interface;
use crate::sandbox::net::Net;

use crate::sandbox::pmem::Pmem;

use cube_hypervisor;
use cube_hypervisor::config::BackendFsConfig;

use cube_hypervisor::vmm_config;
use cube_hypervisor::ApiRequest;
use cube_hypervisor::VmSnapshotConfig;
use cube_hypervisor::{NotifyEvent, VmmInstance};
use http_body_util::{BodyExt, Full};
use hyper::body::Bytes;
use hyper::client;
use hyper_util::rt::TokioIo;
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::mpsc::{channel, Receiver};
use std::sync::Arc;
use std::thread;
use std::time::Duration;
pub mod cmd;

pub const SUB_CMD: &str = "snapshot";
pub const P_PATH: &str = "path";
pub const P_DISK: &str = "disk";
pub const P_PMEM: &str = "pmem";
pub const P_KERNEL: &str = "kernel";
pub const P_NO_TAP: &str = "notap";
pub const P_RES: &str = "resource";
pub const P_FORCE: &str = "force";
pub const CUBE_SYS_PATH: &str = "/usr/local/services/cubetoolbox/";
pub const FS_SHARE_DIR: &str = "/tmp/snapshot-share-dir";

#[derive(Debug)]
pub struct FilePtr {
    path: String,
}

impl FilePtr {
    pub fn new(path: &str) -> CResult<Self> {
        let p = Path::new(path);
        if !p.exists() {
            fs::create_dir_all(p).map_err(|e| format!("create dir {} failed:{}", path, e))?;
        }
        Ok(FilePtr {
            path: path.to_string(),
        })
    }
}

impl Drop for FilePtr {
    fn drop(&mut self) {
        let p = Path::new(self.path.as_str());
        if p.exists() {
            if let Err(e) = fs::remove_dir_all(p) {
                println!("rm {} failed:{}", self.path, e)
            }
        }
    }
}

#[derive(Default)]
pub struct Snapshot {
    id: String,
    path: String,
    kernel: String,
    tap: bool,
    force: bool,
    ch_rx: Option<Receiver<NotifyEvent>>,
    ch: Option<VmmInstance>,
    res: VmResource,
    disk: Vec<Disk>,
    pmem: Vec<Pmem>,
    sharefs_ptr: Option<FilePtr>,
    vsock_ptr: Option<FilePtr>,
    app_snapshot: bool,
}

impl Snapshot {
    pub fn new() -> Self {
        Snapshot::default()
    }

    pub(self) async fn handle(&mut self) -> CResult<()> {
        self.check_path()?;

        if self.app_snapshot {
            self.do_app_snapshot().await?;
            return Ok(());
        }
        self.do_snapshot()
    }

    fn do_snapshot(&mut self) -> CResult<()> {
        self.launch_vmm()?;
        self.boot_vm()?;
        self.wait_vm_ready()?;
        self.create_snapshot()?;
        self.store_metadata()
    }

    async fn do_app_snapshot(&mut self) -> CResult<()> {
        self.api_pause_vm().await?;
        self.api_snapshot_vm().await?;
        self.api_resume_vm().await?;
        self.store_metadata()
    }

    async fn api_pause_vm(&self) -> CResult<()> {
        self.request_ch("/api/v1/vm.pause", "".to_string())
            .await
            .map_err(|e| format!("pause vm failed:{}", e))?;
        Ok(())
    }

    async fn api_resume_vm(&self) -> CResult<()> {
        self.request_ch("/api/v1/vm.resume", "".to_string())
            .await
            .map_err(|e| format!("pause vm failed:{}", e))?;
        Ok(())
    }

    async fn api_snapshot_vm(&self) -> CResult<()> {
        let mut snapshot_path = PathBuf::from(self.path.clone());
        snapshot_path.push("snapshot");

        fs::create_dir_all(snapshot_path.as_path()).map_err(|e| {
            format!(
                "Failed to create path:{}, err:{}",
                snapshot_path.display(),
                e
            )
        })?;

        let config = VmSnapshotConfig {
            destination_url: format!("file://{}", snapshot_path.to_str().unwrap()),
        };
        let data =
            serde_json::to_string(&config).map_err(|e| format!("serialize config failed:{}", e))?;

        self.request_ch("/api/v1/vm.snapshot", data)
            .await
            .map_err(|e| format!("pause vm failed:{}", e))?;
        Ok(())
    }

    async fn request_ch(&self, path: &str, data: String) -> CResult<Bytes> {
        let address = Utils::chapi_path(self.id.as_str());

        let stream = tokio::net::UnixStream::connect(address.as_str())
            .await
            .map_err(|e| format!("connect {} failed:{:?}", address, e))?;
        let io = TokioIo::new(stream);
        let (mut sender, conn) = client::conn::http1::Builder::new()
            .preserve_header_case(true)
            .title_case_headers(true)
            .handshake(io)
            .await
            .map_err(|e| format!("handshake failed:{}", e.to_string()))?;
        tokio::task::spawn(async move {
            if let Err(err) = conn.await {
                println!("Connection failed: {:?}", err);
            }
        });
        let request = hyper::Request::builder()
            .method("PUT")
            .uri(path)
            .header("Host", "localhost")
            .header("Accept", "*/*")
            .header("Content-Type", "application/json")
            .body(Full::new(Bytes::from(data)))
            .map_err(|e| format!("build request failed:{}", e.to_string()))?;

        let response = sender
            .send_request(request)
            .await
            .map_err(|e| format!("send request failed:{}", e.to_string()))?;
        let status = response.status();
        if !status.is_success() {
            return Err(format!("HTTP request failed with status: {}", status).into());
        }
        let body_bytes = response
            .into_body()
            .collect()
            .await
            .map_err(|e| format!("collect body failed:{}", e.to_string()))?
            .to_bytes();
        Ok(body_bytes)
    }

    fn launch_vmm(&mut self) -> CResult<()> {
        //launch
        cube_hypervisor::set_runtime_seccomp_rules(vec![
            (libc::SYS_mkdir, vec![]),
            (libc::SYS_getsockopt, vec![]),
            (libc::SYS_setsockopt, vec![]),
        ]);
        let mut vmm_config = vmm_config::VmmConfig {
            sandbox_id: self.id.clone(),
            ..Default::default()
        };
        let (sender, receiver) = channel::<NotifyEvent>();
        let notifier = vmm_config::EventNotifyConfig { notifier: sender };
        vmm_config.event_notifier = Some(notifier);
        self.ch_rx = Some(receiver);

        let ch = cube_hypervisor::VmmInstance::new(vmm_config)
            .map_err(|e| format!("New vmm instance failed:{}", e))?;
        self.ch = Some(ch);
        Ok(())
    }

    fn boot_vm(&mut self) -> CResult<()> {
        let mut vm_config = VmConfig::default();
        vm_config
            .set_kernel(self.kernel.clone())
            .set_vcpus(self.res.cpu)
            .set_memory(self.res.memory, true);

        if self.tap {
            let net = Net {
                interfaces: vec![Interface {
                    mac: "20:90:6f:fc:fc:fc".to_string(),
                    ..Default::default()
                }],
                ..Default::default()
            };
            let _ = vm_config.add_nets(&net)?;

            //don't disable highres in eks, temporarily use tap to identify this situation
            vm_config.add_cmdline("highres=off".to_string());
            vm_config.add_cmdline("clocksource=kvm-clock".to_string());
        } else {
            vm_config.add_cmdline("clocksource=tsc".to_string());
            vm_config.add_cmdline("tsc=reliable".to_string());
        }

        let sharefs_ptr = FilePtr::new(FS_SHARE_DIR)?;
        self.sharefs_ptr = Some(sharefs_ptr);

        let mut bfs_conf = BackendFsConfig::default();
        bfs_conf.shared_dir = FS_SHARE_DIR.to_string();
        bfs_conf.cache = SHARE_CACHE_NEVER;
        let fs = Fs {
            backendfs_config: Some(bfs_conf),
            rate_limiter_config: None,
        };

        if let Some(p) = self.pmem.get(0) {
            if p.id == "pmem-agent" {
                self.pmem.remove(0);
            }
        }

        vm_config
            .add_disks(&self.disk)
            .add_pmems(&self.pmem)
            .add_fs(&fs);

        let mut vm_dir = PathBuf::from(VM_PATH);
        vm_dir.push(self.id.clone());
        let vsock_ptr = FilePtr::new(vm_dir.clone().to_str().unwrap())?;
        self.vsock_ptr = Some(vsock_ptr);
        vm_config.add_vsock(self.id.clone());
        vm_config.add_cmdline("quiet".to_string());
        vm_config.add_cmdline("snapshot-mode".to_string());
        if self.res.preserve_memory > 0 {
            vm_config.add_cmdline(format!("cubemem_pages_nr={}", self.res.preserve_memory));
            vm_config.add_cmdline(format!(
                "cube_reserve_nr_pages_order8={}",
                self.res.preserve_memory
            ));
        }

        let b_vm_config = Box::new(vm_config.to_vm_config());

        let _ = self
            .ch
            .as_ref()
            .unwrap()
            .send_request(ApiRequest::VmCreate(b_vm_config))
            .map_err(|e| format!("Create vm failed:{}", e))?
            .map_err(|e| format!("Create vm failed:{}", e))?;

        let _ = self
            .ch
            .as_ref()
            .unwrap()
            .send_request(ApiRequest::VmBoot)
            .map_err(|e| format!("Boot vm failed:{}", e))?
            .map_err(|e| format!("Boot vm failed:{}", e))?;
        Ok(())
    }

    fn wait_vm_ready(&self) -> CResult<()> {
        let ev = self
            .ch_rx
            .as_ref()
            .unwrap()
            .recv_timeout(Duration::from_nanos(1000 * 1000 * 1000 * 3));
        if let Err(e) = ev {
            return Err(format!("Wait vm ready err:{}", e));
        }
        let ev = ev.unwrap();
        if ev != NotifyEvent::SysStart {
            return Err(format!(
                "Not an expected event, expected:{:?}, actual:{:?}",
                NotifyEvent::SysStart,
                ev
            ));
        }
        thread::sleep(Duration::from_secs(3));
        Ok(())
    }

    fn create_snapshot(&self) -> CResult<()> {
        let _ = self
            .ch
            .as_ref()
            .unwrap()
            .send_request(ApiRequest::VmPause)
            .map_err(|e| format!("Pause vm failed:{}", e))?
            .map_err(|e| format!("Pause vm failed:{}", e))?;
        let mut snapshot_path = PathBuf::from(self.path.clone());
        snapshot_path.push("snapshot");

        fs::create_dir_all(snapshot_path.as_path()).map_err(|e| {
            format!(
                "Failed to create path:{}, err:{}",
                snapshot_path.display(),
                e
            )
        })?;

        let config = VmSnapshotConfig {
            destination_url: format!("file://{}", snapshot_path.to_str().unwrap()),
        };
        let _ = self
            .ch
            .as_ref()
            .unwrap()
            .send_request(ApiRequest::VmSnapshot(Arc::new(config)))
            .map_err(|e| format!("Snapshot vm failed:{}", e))?
            .map_err(|e| format!("Snapshot vm failed:{}", e))?;
        Ok(())
    }

    fn store_metadata(&self) -> CResult<()> {
        let mut snap_info = SnapshotInfo::new(self.res.cpu, self.res.memory);
        snap_info.image_version = Utils::get_image_version()?;
        snap_info.kernel_version = Utils::get_kernel_version(self.kernel.as_str())?;
        for d in &self.disk {
            let disk = snapshot::Disk {
                fs_type: d.fs_type.clone(),
                size: d.size,
            };
            snap_info.vm_res.disks.push(disk);
        }

        for p in &self.pmem {
            let pmem = snapshot::Pmem {
                id: p.id.clone(),
                fs_type: p.fs_type.clone(),
                size: p.size,
                ..Default::default()
            };
            snap_info.vm_res.pmems.push(pmem);
        }
        let mut metadata = PathBuf::from(self.path.clone());
        metadata.push("metadata.json");
        snap_info.store(metadata.as_path())
    }

    fn check_path(&self) -> CResult<()> {
        if self.path.starts_with(CUBE_SYS_PATH) && !self.force {
            return Err(format!(
                "Can't create snapshot in cube sys path:[{}] directly",
                CUBE_SYS_PATH
            ));
        }
        let path = Path::new(self.path.as_str());
        if path.exists() {
            if self.force {
                fs::remove_dir_all(path)
                    .map_err(|e| format!("Failed to clean path:{}, err:{}", CUBE_SYS_PATH, e))?;
            } else {
                return Err(format!("Path:{} exist", &self.path));
            }
        }
        fs::create_dir_all(path)
            .map_err(|e| format!("Failed to create path:{}, err:{}", CUBE_SYS_PATH, e))?;

        Ok(())
    }
}

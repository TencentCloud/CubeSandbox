// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use libc::pid_t;
use nix::mount::{mount, umount2, MntFlags, MsFlags};
use serde::{Deserialize, Serialize};
use serde_json::from_str;
use std::fs::OpenOptions;
use std::path::Path;
use std::result::Result;
use std::string::String;
use std::{fs, fs::File, os::fd::AsRawFd};
pub const ANNOTATION_K_ROOTFS_INFO: &str = "cube.rootfs.info";
pub const ANNO_CONTAINER_CUSTOM_FILE: &str = "cube.container.custom.file";
pub const ANNO_PROPAGATION_EXEC_MNTS: &str = "cube.propagation.exec.mounts";
pub const ANNO_PROPAGATION_CONTAINER_UMNTS: &str = "cube.propagation.container.umounts";
pub const ENV_CONTAINER_PID: &str = "container.pid";

#[derive(Serialize, Deserialize, Debug)]
pub struct OverlayInfo {
    pub virtiofs_lower_dir: Vec<String>,
}

#[derive(Serialize, Deserialize, Debug)]
pub struct MountInfo {
    pub virtiofs_source: String,
    pub container_dest: String,
    pub r#type: String,
    pub options: Vec<String>,
}

#[derive(Serialize, Deserialize, Debug)]
pub struct EroImage {
    pub path: String,
    pub lower_dir: Vec<String>,
}

#[derive(Serialize, Deserialize, Debug)]
pub struct RootfsInfo {
    pub rootfs: Option<String>,
    pub pmem_file: Option<String>,
    pub overlay_info: Option<OverlayInfo>,
    pub mounts: Option<Vec<MountInfo>>,
    pub ero_image: Option<EroImage>,
}

#[derive(Serialize, Deserialize, Debug)]
pub struct ExecMount {
    pub source: String,
    pub dest: String,
    //pub r#type: String,
    //pub options: Vec<String>,
}

impl RootfsInfo {
    pub fn new(s: &String) -> Result<Self, String> {
        match from_str(s).map_err(|e| format!("new RootfsInfo failed:{}", e.to_string())) {
            Ok(oi) => Ok(oi),
            Err(e) => Err(e),
        }
    }
}

#[derive(Serialize, Deserialize, Debug)]
pub struct CustomFile {
    pub path: String,
    pub content: String,
}

#[derive(Clone, Debug, Serialize, Deserialize, Default)]
pub struct PropagationContainerUmount {
    pub name: String,
    pub container_dir: String,
}

pub fn exit_proc_failed(msg: String) {
    println!("{}", msg);
    std::process::exit(1);
}

pub fn do_exec_mount() {
    println!("exec process start");
    let ev_pid = std::env::var(ENV_CONTAINER_PID);
    if ev_pid.is_err() {
        exit_proc_failed(format!("Not found env: {}", ENV_CONTAINER_PID));
    }
    let pid: pid_t = ev_pid.unwrap().parse().expect("invalid pid");

    let exec_mnts = {
        match std::env::var(ANNO_PROPAGATION_EXEC_MNTS) {
            Ok(val) => {
                let exec_mnts = serde_json::from_str::<Vec<ExecMount>>(val.as_str());
                if let Err(e) = exec_mnts.as_ref() {
                    exit_proc_failed(format!("Deserialize ExecMount failed:{}", e.to_string()));
                }
                exec_mnts.unwrap()
            }
            Err(_) => {
                let exec_mnts = vec![];
                exec_mnts
            }
        }
    };

    let propa_umnts = {
        match std::env::var(ANNO_PROPAGATION_CONTAINER_UMNTS) {
            Ok(val) => {
                let propa_umnts =
                    serde_json::from_str::<Vec<PropagationContainerUmount>>(val.as_str());
                if let Err(e) = propa_umnts.as_ref() {
                    exit_proc_failed(format!(
                        "Deserialize PropagationContainerUmount failed:{}",
                        e.to_string()
                    ));
                }
                propa_umnts.unwrap()
            }
            Err(_) => {
                let propa_umnts = vec![];
                propa_umnts
            }
        }
    };

    let mnt_ns_path = format!("/proc/{}/ns/mnt", pid);
    let file = File::open(mnt_ns_path.clone());
    if let Err(e) = file.as_ref() {
        exit_proc_failed(format!("open {} failed:{}", mnt_ns_path, e));
    }
    let ret = unsafe { libc::setns(file.unwrap().as_raw_fd(), libc::CLONE_NEWNS) };
    if ret < 0 {
        exit_proc_failed(format!("setns failed:{}", std::io::Error::last_os_error()));
    }
    for m in exec_mnts {
        println!("start exec mount {}", m.source.as_str());
        let dest = Path::new(m.dest.as_str());
        if !dest.exists() {
            let src = Path::new(m.source.as_str());
            if !src.exists() {
                exit_proc_failed(format!("the source dir {} not exists", m.source));
            }

            if src.is_dir() {
                if let Err(e) = fs::create_dir_all(dest) {
                    exit_proc_failed(format!("create dir {} failed:{}", m.dest, e.to_string()));
                }
            } else {
                let parent = dest.parent();
                if parent.is_none() {
                    exit_proc_failed(format!("parent dir {} invalid", m.dest));
                }
                if let Err(e) = fs::create_dir_all(parent.unwrap()) {
                    exit_proc_failed(format!(
                        "create dir {:?} failed:{}",
                        parent.unwrap(),
                        e.to_string()
                    ));
                }

                if let Err(e) = OpenOptions::new().write(true).create(true).open(dest) {
                    exit_proc_failed(format!(
                        "create file {} failed:{}",
                        dest.display(),
                        e.to_string()
                    ));
                }
            }
        }
        if let Err(e) = mount(
            Some(m.source.as_str()),
            m.dest.as_str(),
            None::<&str>,
            MsFlags::MS_BIND | MsFlags::MS_REC,
            None::<&str>,
        ) {
            exit_proc_failed(format!(
                "mount {} to {} failed:{}",
                m.source.as_str(),
                m.dest.as_str(),
                e.to_string()
            ));
        }
        println!("exec mount {} finish", m.source.as_str());
    }

    for m in propa_umnts {
        println!("start umount {}", m.container_dir.as_str());
        if let Err(e) = umount2(m.container_dir.as_str(), MntFlags::MNT_DETACH) {
            println!(
                "first umount propagation mnt:{} failed:{}",
                m.container_dir.as_str(),
                e
            );
        }

        if let Err(e) = umount2(m.container_dir.as_str(), MntFlags::MNT_DETACH) {
            println!(
                "second umount propagation mnt:{} failed:{}",
                m.container_dir.as_str(),
                e
            );
        }

        if let Err(e) = fs::remove_dir_all(m.container_dir.as_str()) {
            println!("rm dir:{} failed:{}", m.container_dir.as_str(), e);
        }
        println!("umount {} finish", m.container_dir.as_str());
    }
}

#[cfg(test)]
mod tests {
    use crate::rootfs::RootfsInfo;

    #[test]
    fn test_rootfsinfo_new() {
        let info =
            r#"{"pmem_file": "/pmem_file", "rootfs": "/rootfs", "overlay_info": {"virtiofs_lower_dir":["123", "456"]}}"#
                .to_string();
        let rootfs_info = RootfsInfo::new(&info);
        assert!(rootfs_info.is_ok());
        assert!(rootfs_info.unwrap().mounts.is_none());
        let rootfs_info1 = RootfsInfo::new(&info);
        assert!(rootfs_info1.unwrap().overlay_info.is_some());
    }
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

pub mod types;
pub mod utils;
pub type CResult<T> = Result<T, String>;
pub const PRODUCT_CUBEBOX: &str = "cubebox";

pub const NS_NET: &str = "network";
pub const NS_CGROUP: &str = "cgroup";
pub const NS_PID: &str = "pid";

pub const MOUNT_TYPE_BIND: &str = "bind";
pub const MOUNT_TYPE_RBIND: &str = "rbind";

pub const ANNO_ROOTFS_WLAYER_PATH: &str = "cube.rootfs.wlayer.path";
pub const ANNO_ROOTFS_WLAYER_PATH_SUBDIR: &str = "cube.rootfs.wlayer.subdir";

pub const SHIM_VERSION: &str = env!("CUBE_VERSION");
pub const SHIM_COMMIT: &str = env!("CUBE_COMMIT");
pub const SHIM_BUILD_TIME: &str = env!("CUBE_BUILD_TIME");

pub const ANNO_UPDATE_EXT_ACT: &str = "cube.shimapi.update.action";
pub const ANNO_UPDATE_EXT_PARAM: &str = "cube.shimapi.update.param";

pub const PAUSE_VM_SNAPSHOT_BASE: &str = "/data/cubelet/root/pausevm";

pub const GUEST_PROPAGATION_DIR: &str = "/run/propagation";
pub const ANNO_PROPAGATION_MNTS: &str = "cube.propagation.mounts";
pub const ANNO_PROPAGATION_CONTAINER_MNTS: &str = "cube.propagation.container.mounts";

/// Container-level annotation injected by the shim into OCI spec annotations.
/// When present and equal to "true", the agent will create stdout/stderr pipes
/// in open_io() so that log forwarding can work.  Absent when the old shim is
/// used, allowing the agent to stay backward-compatible.
pub const ANNO_CONTAINER_LOG_FORWARDING: &str = "cube.container.log_forwarding";

pub const CUBE_BIND_SHARE_TYPE: &str = "bind-share";
pub const CUBE_BIND_SHARE_GUEST_BASE_DIR: &str = "/run/cube-bind-share/";
pub const GUEST_VIRTIOFS_MNT_PATH: &str = "/run/virtiofs";
pub const GUEST_VIRTIOFS_MNT_PATH_DEPRECATED: &str = "/run/cube-containers/shared/containers";

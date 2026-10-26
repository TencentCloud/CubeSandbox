// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::time::Instant;

use super::{Log, StatRet};

pub const ACT_CREATE: &str = "Create";
pub const ACT_DELETE: &str = "Delete";
pub const ACT_START: &str = "Start";
pub const ACT_WAIT: &str = "Wait";
/*
    CalleeActionCreatePodSandbox   = "CreatePodSandbox"
    CalleeActionCreatePodContainer = "CreatePodContainer"
    CalleeActionStartVm            = "StartVm"
    CalleeActionBootVmm            = "BootVm"
    CalleeActionCreateVm           = "CreateVm"
    CalleeActionRestoreVm          = "RestoreVm"
    CalleeActionLaunchVmm          = "LaunchVmm"
    CalleeActionCreateSandbox      = "CreateSandbox"
    CalleeActionCreateContainer    = "CreateContainer"

    CalleeActionFsSharePrepare = "FsSharePrepare"
    CalleeActionCubeHCreate    = "CubeHCreate"
    CalleeActionCubeHLaunch    = "CubeHLaunch"
    CalleeActionHotplugDisk    = "HotplugDisk"
    CalleeActionHotplugNet     = "HotplugNet"

    CalleeActionStart    = "Start"
    CalleeActionDelete   = "Delete"
    CalleeActionKill     = "Kill"
    CalleeActionWait     = "Wait"
    CalleeActionShutDown = "Shutdown"
*/
pub const CALLEE_ACT_CREATE_POD_SANDBOX: &str = "CreatePodSandbox";
pub const CALLEE_ACT_CREATE_POD_CONTAINER: &str = "CreatePodContainer";
pub const CALLEE_ACT_LAUNCH_VMM: &str = "LaunchVmm";
pub const CALLEE_ACT_BOOT_VM: &str = "BootVm";
pub const CALLEE_ACT_CREATE_VM: &str = "CreateVm";
pub const CALLEE_ACT_RESTORE_VM: &str = "RestoreVm";
pub const CALLEE_ACT_CREATE_SANDBOX: &str = "CreateSandbox";
pub const CALLEE_ACT_CREATE_CONTAINER: &str = "CreateContainer";

pub const CALLEE_ACT_RESET_VM: &str = "ResetVm";

pub const CALLEE_ACT_DEL_CONTAINER: &str = "DeleteContainer";

pub const CALLEE_ACT_AGENT: &str = "Agent";

pub const CALLEE_SHIM: &str = "Shim";
pub const CALLEE_CH: &str = "Ch";
pub const CALLEE_AGENT: &str = "Agent";

pub struct StatDefer {
    cid: String,
    callee: String,
    action: String,
    callee_act: String,
    start: Instant,
    ret: StatRet,
    log: Log,
    loged: bool,
}
/*
       container_id: String,
       caller: String,
       callee: String,
       action: String,
       callee_action: String,
       ret: StatRet,
       cost: u64,
*/
impl StatDefer {
    pub fn new(cid: String, callee: String, action: String, callee_act: String, log: Log) -> Self {
        StatDefer {
            cid,
            callee,
            action,
            callee_act,
            start: Instant::now(),
            ret: StatRet::Err,
            log,
            loged: false,
        }
    }

    pub fn set_ok(&mut self) {
        self.ret = StatRet::Ok;
    }

    pub fn set_callee_act(&mut self, act: String) {
        self.callee_act = act;
    }

    pub fn stat(&mut self) {
        let duration = self.start.elapsed().as_millis();
        self.log.stat(
            self.cid.clone(),
            self.callee.clone(),
            self.action.clone(),
            self.callee_act.clone(),
            self.ret.clone(),
            duration,
        );
        self.loged = true
    }
}

impl Drop for StatDefer {
    fn drop(&mut self) {
        if !self.loged {
            self.stat();
        }
    }
}

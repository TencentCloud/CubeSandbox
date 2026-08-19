// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Guest PID 1: mount cube-agent.ext4 from /dev/pmem1 and exec cube-agent.
//!
//! Topology (fixed):
//!   /dev/pmem0 = Guest OS image (this binary as /sbin/init)
//!   /dev/pmem1 = cube-agent.ext4 → mounted at /run/support (ext4,ro,dax)
//!
//! Open-source guest-init intentionally does **not** perform SysCtrl
//! snapshot handshake (write SYS_START / poll SYS_RESTORE). Product
//! templates use APP snapshot on a running VM; cold boot never needs it.
//! Agent still signals VsockServerReady via SysCtrl after exec.

use anyhow::{anyhow, Result};
use nix::mount::{mount, MsFlags};
use nix::unistd;
use std::ffi::CString;
use std::fs;
use std::path::Path;
use std::process;
use std::time::Instant;

mod init_env;

use crate::init_env::init;

const PMEM_DEV: &str = "/dev/pmem1";
const PMEM_MP: &str = "/run/support/";
const CUBE_AGENT: &str = "/run/support/cube-agent";

fn main() {
    let start = Instant::now();
    println!("init start at:{}", start.elapsed().as_millis());

    if process::id() != 1 {
        panic!("cube init must be started as pid 1");
    }

    if let Err(e) = init(start) {
        panic!("{}", e);
    }

    if let Err(e) = mount_pmem() {
        panic!("{}", e);
    }
    println!("mount pmem finish at:{}", start.elapsed().as_millis());
    start_agent();
}

fn mount_pmem() -> Result<()> {
    let source = Path::new(PMEM_DEV);
    let target = Path::new(PMEM_MP);
    let m_type = Some("ext4");
    let flags = MsFlags::MS_RDONLY;

    fs::create_dir_all(target).map_err(|e| anyhow!("mkdir {} failed:{}", PMEM_MP, e))?;

    mount(Some(source), target, m_type, flags, Some("dax"))
        .map_err(|e| anyhow!("mount pmem failed:{}", e))?;

    Ok(())
}

fn agent_argv() -> [CString; 1] {
    [CString::new(CUBE_AGENT).expect("new cmd failed")]
}

fn start_agent() -> ! {
    let args = agent_argv();
    let err = unistd::execvp(args[0].as_c_str(), &args).unwrap_err();
    panic!("exec agent failed:{}", err);
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn agent_argv_carries_program_name() {
        let argv = agent_argv();
        assert_eq!(argv.len(), 1);
        assert_eq!(argv[0].to_str().unwrap(), CUBE_AGENT);
        assert!(!argv[0].as_bytes().is_empty());
    }

    #[test]
    fn agent_exec_path_constant() {
        assert_eq!(CUBE_AGENT, "/run/support/cube-agent");
        assert_eq!(PMEM_DEV, "/dev/pmem1");
        assert_eq!(PMEM_MP, "/run/support/");
    }
}

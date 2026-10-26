// Copyright (c) 2019 Ant Financial
//
// SPDX-License-Identifier: Apache-2.0
//

// looks like we can use caps to manipulate capabilities
// conveniently, use caps to do it directly.. maybe

use crate::log_child;
use crate::sync::write_count;
use anyhow::{anyhow, Result};
use caps::{self, runtime, CapSet, Capability, CapsHashSet};
use oci::LinuxCapabilities;
use std::os::unix::io::RawFd;
use std::str::FromStr;

fn to_capshashset(cfd_log: RawFd, caps: &[String]) -> CapsHashSet {
    let mut r = CapsHashSet::new();

    for cap in caps.iter() {
        match Capability::from_str(cap) {
            Err(_) => {
                log_child!(cfd_log, "{} is not a cap", cap);
                continue;
            }
            Ok(c) => r.insert(c),
        };
    }

    r
}

pub fn get_all_caps() -> CapsHashSet {
    let mut caps_set =
        runtime::procfs_all_supported(None).unwrap_or_else(|_| runtime::thread_all_supported());
    if caps_set.is_empty() {
        caps_set = caps::all();
    }
    caps_set
}

pub fn reset_effective() -> Result<()> {
    let all = get_all_caps();
    caps::set(None, CapSet::Effective, &all).map_err(|e| anyhow!(e.to_string()))?;
    Ok(())
}

pub fn drop_privileges(cfd_log: RawFd, caps: &LinuxCapabilities) -> Result<()> {
    let all = get_all_caps();

    for c in all.difference(&to_capshashset(cfd_log, caps.bounding.as_ref())) {
        caps::drop(None, CapSet::Bounding, *c).map_err(|e| anyhow!(e.to_string()))?;
    }

    caps::set(
        None,
        CapSet::Effective,
        &to_capshashset(cfd_log, caps.effective.as_ref()),
    )
    .map_err(|e| anyhow!(e.to_string()))?;
    caps::set(
        None,
        CapSet::Permitted,
        &to_capshashset(cfd_log, caps.permitted.as_ref()),
    )
    .map_err(|e| anyhow!(e.to_string()))?;
    caps::set(
        None,
        CapSet::Inheritable,
        &to_capshashset(cfd_log, caps.inheritable.as_ref()),
    )
    .map_err(|e| anyhow!(e.to_string()))?;

    let _ = caps::set(
        None,
        CapSet::Ambient,
        &to_capshashset(cfd_log, caps.ambient.as_ref()),
    )
    .map_err(|_| log_child!(cfd_log, "failed to set ambient capability"));

    Ok(())
}

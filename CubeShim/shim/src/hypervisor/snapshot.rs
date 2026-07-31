// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::common::utils::Utils;
use crate::common::CResult;

use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::fs;
use std::io::{BufReader, Write};
use std::path::Path;

use crate::sandbox::disk::Disk as SbDisk;
use crate::sandbox::pmem::Pmem as SbPmem;
use cube_hypervisor::SNAPSHOT_VERSION;

use serde_json;
pub fn enable_snapshot() -> bool {
    let p = Path::new("/data/cube-shim/snapshot");
    if let Ok(_stat) = fs::metadata(p) {
        return true;
    }
    false
}

#[derive(Clone, Default, Serialize, Deserialize)]
pub struct SnapshotInfo {
    pub kernel_version: String,
    pub image_version: String,
    pub ch_version: String,
    /// Agent plane-file identity (`cube-agent/version` next to cube-agent.ext4).
    /// Optional for serde backward compat with old templates; restore only fails
    /// when both sides are `Some` and unequal (missing/`None` is allowed).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub agent_version: Option<String>,
    pub vm_res: VmRes,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub memory_vol_url: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub app_snapshot_container_id: Option<String>,
}

#[derive(Clone, Default, Serialize, Deserialize)]
pub struct VmRes {
    pub cpu: u32,
    pub cpu_max: u32,
    pub memory: u64,
    pub disks: Vec<Disk>,
    pub pmems: Vec<Pmem>,
}

#[derive(Clone, Default, Serialize, Deserialize)]
pub struct Disk {
    pub fs_type: String,
    pub size: u64,
}

#[derive(Clone, Default, Serialize, Deserialize, PartialEq, Eq, Debug)]
pub struct Pmem {
    pub id: String,
    pub fs_type: String,
    pub size: Option<u64>,
    pub placeholder: bool,
}

impl SnapshotInfo {
    pub fn load(base: &str, cpu: u32, memory: u64) -> CResult<Self> {
        let pfile = Utils::get_snapshot_metadata_file(base, cpu, memory);
        let file = fs::File::open(pfile.clone())
            .map_err(|e| format!("load snapshot failed:{} file:{}", e, pfile))?;
        let reader = BufReader::new(file);
        let ss: SnapshotInfo = serde_json::from_reader(reader)
            .map_err(|e| format!("read snapshot metadata failed:{}", e))?;
        Ok(ss)
    }

    pub fn store(&self, target: &Path) -> CResult<()> {
        let data =
            serde_json::to_string(self).map_err(|e| format!("serialize metadata failed:{}", e))?;
        let mut file = fs::File::create(target)
            .map_err(|e| format!("open {} failed:{}", target.display(), e))?;
        file.write_all(data.as_bytes())
            .map_err(|e| format!("write {} failed:{}", target.display(), e))
    }

    pub fn new(cpu: u32, memory: u64) -> Self {
        SnapshotInfo {
            vm_res: VmRes {
                cpu,
                cpu_max: cpu,
                memory,
                disks: Vec::<Disk>::new(),
                pmems: Vec::<Pmem>::new(),
            },
            ch_version: SNAPSHOT_VERSION.to_string(),
            ..Default::default()
        }
    }

    pub fn set_kernel_version(&mut self, kernel: &str) -> CResult<()> {
        self.kernel_version = Utils::get_kernel_version(kernel)?;
        Ok(())
    }

    pub fn set_image_version(&mut self) -> CResult<()> {
        self.image_version = Utils::get_image_version()?;
        Ok(())
    }

    pub fn set_image_version_for_path(&mut self, os_image_path: &str) -> CResult<()> {
        self.image_version = Utils::get_image_version_for_path(os_image_path)?;
        Ok(())
    }

    pub fn set_agent_version(&mut self, agent_path: &str) -> CResult<()> {
        self.agent_version = Some(Utils::get_agent_version(agent_path)?);
        Ok(())
    }

    pub fn set_disks(&mut self, disks: &[SbDisk]) {
        for d in disks.iter() {
            let disk = Disk {
                size: d.size,
                fs_type: d.fs_type.clone(),
            };
            self.vm_res.disks.push(disk);
        }
    }

    pub fn set_pmems(&mut self, pmems: &[SbPmem]) {
        for p in pmems.iter() {
            let pmem = Pmem {
                id: p.id.clone(),
                size: p.size,
                fs_type: p.fs_type.clone(),
                placeholder: p.placeholder,
            };
            self.vm_res.pmems.push(pmem);
        }
    }

    pub fn align_pmems(&self, pmems: &[SbPmem]) -> Vec<SbPmem> {
        let mut req_pmems: HashMap<String, SbPmem> = pmems
            .iter()
            .map(|pmem| (pmem.id.clone(), pmem.clone()))
            .collect();

        let mut res_pmems = Vec::new();
        for p in self.vm_res.pmems.iter() {
            if let Some(pmem) = req_pmems.get(&p.id) {
                res_pmems.push(pmem.clone());
                req_pmems.remove(&p.id);
            } else {
                let pmem = SbPmem {
                    placeholder: true,
                    ..Default::default()
                };
                res_pmems.push(pmem);
            }
        }

        for (_, pmem) in req_pmems {
            res_pmems.push(pmem);
        }
        res_pmems
    }

    pub fn eq(&self, o: &Self) -> CResult<()> {
        if self.ch_version != o.ch_version {
            return Err(format!(
                "ch version not eq:{} {}",
                &self.ch_version, &o.ch_version
            ));
        }

        if self.kernel_version != o.kernel_version {
            return Err(format!(
                "kernel version not eq:{} {}",
                &self.kernel_version, &o.kernel_version
            ));
        }

        if self.image_version != o.image_version {
            return Err(format!(
                "image version not eq:{} {}",
                &self.image_version, &o.image_version
            ));
        }

        // Fail only when both sides have agent_version and they differ.
        // Missing agent_version skips the check so old templates can still restore.
        if let (Some(v1), Some(v2)) = (&self.agent_version, &o.agent_version) {
            if v1 != v2 {
                return Err(format!(
                    "agent version not eq:{} {} (refuse silent agent swap on restore)",
                    v1, v2
                ));
            }
        }

        self.vm_res.eq(&o.vm_res)?;

        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn base_info() -> SnapshotInfo {
        let mut info = SnapshotInfo::new(2, 1024);
        info.kernel_version = "k1".to_string();
        info.image_version = "img1".to_string();
        info
    }

    #[test]
    fn eq_agent_version_both_some_unequal_fails() {
        let mut a = base_info();
        let mut b = base_info();
        a.agent_version = Some("v1".to_string());
        b.agent_version = Some("v2".to_string());
        let err = a.eq(&b).unwrap_err();
        assert!(err.contains("agent version not eq"), "unexpected: {err}");
    }

    #[test]
    fn eq_agent_version_both_some_equal_passes() {
        let mut a = base_info();
        let mut b = base_info();
        a.agent_version = Some("v1".to_string());
        b.agent_version = Some("v1".to_string());
        a.eq(&b).expect("equal agent_version should pass");
    }

    #[test]
    fn eq_agent_version_self_none_passes() {
        let mut a = base_info();
        let mut b = base_info();
        a.agent_version = None;
        b.agent_version = Some("v1".to_string());
        a.eq(&b).expect("None on self should skip agent check");
    }

    #[test]
    fn eq_agent_version_other_none_passes() {
        let mut a = base_info();
        let mut b = base_info();
        a.agent_version = Some("v1".to_string());
        b.agent_version = None;
        a.eq(&b).expect("None on other should skip agent check");
    }

    #[test]
    fn eq_agent_version_both_none_passes() {
        let mut a = base_info();
        let mut b = base_info();
        a.agent_version = None;
        b.agent_version = None;
        a.eq(&b).expect("both None should skip agent check");
    }

    #[test]
    fn eq_image_version_mismatch_still_fails() {
        let mut a = base_info();
        let mut b = base_info();
        a.agent_version = None;
        b.agent_version = None;
        b.image_version = "img2".to_string();
        let err = a.eq(&b).unwrap_err();
        assert!(err.contains("image version not eq"), "unexpected: {err}");
    }
}

impl Disk {
    fn eq(&self, o: &Self) -> CResult<()> {
        if self.fs_type != o.fs_type {
            return Err(format!(
                "disk fs type not eq, {} {}",
                &self.fs_type, &o.fs_type
            ));
        }
        if self.size != o.size {
            return Err(format!("disk size not eq, {} {}", &self.size, &o.size));
        }
        Ok(())
    }
}

impl VmRes {
    fn eq(&self, o: &Self) -> CResult<()> {
        if self.cpu != o.cpu {
            return Err(format!("cpu not eq: {} {}", self.cpu, o.cpu));
        }

        if self.cpu_max != o.cpu_max {
            return Err(format!("cpu not eq: {} {}", self.cpu_max, o.cpu_max));
        }

        if self.memory != o.memory {
            return Err(format!("mem size not eq: {} {}", self.memory, o.memory));
        }

        if self.disks.len() != o.disks.len() {
            return Err(format!(
                "disks len not eq: {} {}",
                self.disks.len(),
                o.disks.len()
            ));
        }

        let mut oiter = o.disks.iter();
        for d in self.disks.iter() {
            let disk = oiter.next().unwrap();
            d.eq(disk)?;
        }

        if self.pmems.len() != o.pmems.len() {
            return Err(format!(
                "the size of pmems not eq, object:{} param:{}",
                self.pmems.len(),
                o.pmems.len()
            ));
        }

        for (i, pmem) in self.pmems.iter().enumerate() {
            let req_pmem = &o.pmems[i];
            if req_pmem.placeholder {
                continue;
            }
            if pmem != req_pmem {
                return Err(format!(
                    "the pmem not match, object:{:?} param:{:?}",
                    pmem, req_pmem
                ));
            }
        }
        Ok(())
    }
}

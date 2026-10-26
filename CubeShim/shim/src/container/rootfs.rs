// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::common::{
    utils::{CPath, Utils},
    CResult, GUEST_VIRTIOFS_MNT_PATH_DEPRECATED,
};
use serde::{Deserialize, Serialize};
use serde_json;
pub const ANNOTATION_K_ROOTFS_INFO: &str = "cube.rootfs.info";
pub const ANNO_CONTAINER_CUSTOM_FILE: &str = "cube.container.custom.file";
#[derive(Serialize, Deserialize, Debug)]
pub struct OverlayInfo {
    pub virtiofs_lower_dir: Vec<String>,
}

#[derive(Serialize, Deserialize, Debug)]
pub struct MountInfo {
    #[serde(default)]
    pub virtiofs_id: String,
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
    pub pmem_file: Option<String>,
    pub overlay_info: Option<OverlayInfo>,
    pub mounts: Option<Vec<MountInfo>>,
    pub ero_image: Option<EroImage>,
}

impl RootfsInfo {
    pub fn new(s: &str) -> CResult<Self> {
        match serde_json::from_str(s).map_err(|e| format!("new RootfsInfo failed:{}", e)) {
            Ok(oi) => Ok(oi),
            Err(e) => Err(e),
        }
    }

    //convert a relative path to an absolute path in the guest directory
    pub fn fix_virtiofs(&mut self) {
        //fix mount.virtiofs_source
        if let Some(mounts) = self.mounts.as_mut() {
            for mnt in mounts {
                let mut base = GUEST_VIRTIOFS_MNT_PATH_DEPRECATED.to_string();
                if !mnt.virtiofs_id.is_empty() {
                    base = Utils::virtiofs_guest_base(mnt.virtiofs_id.clone());
                }
                let mut p = CPath::new(base.as_str());
                p.join(&mnt.virtiofs_source);
                mnt.virtiofs_source = p.to_str().unwrap_or("").to_string();
            }
        }

        //fix ovl_info.virtiofs_lower_dir
        if let Some(ovl_info) = self.overlay_info.as_mut() {
            let mut lowdirs = Vec::new();
            for lowdir in ovl_info.virtiofs_lower_dir.clone() {
                let mut p = CPath::new(GUEST_VIRTIOFS_MNT_PATH_DEPRECATED);
                p.join(lowdir.as_str());
                lowdirs.push(p.to_str().unwrap_or("").to_string());
            }
            ovl_info.virtiofs_lower_dir = lowdirs;
        }
    }
}

#[derive(Serialize, Deserialize, Debug)]
pub struct CustomFile {
    pub path: String,
    pub content: String,
}
#[cfg(test)]
mod tests {
    use crate::common::GUEST_VIRTIOFS_MNT_PATH;

    use super::*;

    #[test]
    fn test_fix_virtiofs() {
        {
            let minfo = MountInfo {
                virtiofs_id: "".to_string(),
                virtiofs_source: "123".to_string(),
                container_dest: "".to_string(),
                r#type: "".to_string(),
                options: Vec::new(),
            };

            let ovl_info = OverlayInfo {
                virtiofs_lower_dir: vec!["abc".to_string(), "qwe".to_string()],
            };

            let mut rootfs = RootfsInfo {
                pmem_file: None,
                ero_image: None,
                mounts: Some(vec![minfo]),
                overlay_info: Some(ovl_info),
            };

            rootfs.fix_virtiofs();
            assert_eq!(rootfs.mounts.is_some(), true);
            assert_eq!(rootfs.mounts.as_ref().unwrap().len(), 1);
            assert_eq!(
                rootfs.mounts.as_ref().unwrap()[0].virtiofs_source,
                format!(
                    "{}/{}",
                    GUEST_VIRTIOFS_MNT_PATH_DEPRECATED,
                    "123".to_string()
                )
            );

            assert_eq!(rootfs.overlay_info.is_some(), true);
            assert_eq!(
                rootfs
                    .overlay_info
                    .as_ref()
                    .unwrap()
                    .virtiofs_lower_dir
                    .len(),
                2
            );
            assert_eq!(
                rootfs.overlay_info.as_ref().unwrap().virtiofs_lower_dir[0],
                format!(
                    "{}/{}",
                    GUEST_VIRTIOFS_MNT_PATH_DEPRECATED,
                    "abc".to_string()
                )
            );
        }

        let minfo = MountInfo {
            virtiofs_id: "123".to_string(),
            virtiofs_source: "123".to_string(),
            container_dest: "".to_string(),
            r#type: "".to_string(),
            options: Vec::new(),
        };
        let mut rootfs = RootfsInfo {
            pmem_file: None,
            ero_image: None,
            mounts: Some(vec![minfo]),
            overlay_info: None,
        };
        rootfs.fix_virtiofs();
        assert_eq!(rootfs.mounts.is_some(), true);
        assert_eq!(rootfs.mounts.as_ref().unwrap().len(), 1);
        assert_eq!(
            rootfs.mounts.as_ref().unwrap()[0].virtiofs_source,
            format!(
                "{}/{}/{}",
                GUEST_VIRTIOFS_MNT_PATH,
                "123".to_string(),
                "123".to_string()
            )
        );
    }
}

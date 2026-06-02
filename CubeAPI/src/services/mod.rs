// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

pub mod builds;
pub mod cluster;
pub mod sandboxes;
pub mod snapshots;
pub mod templates;

use crate::{config::ServerConfig, cubemaster::CubeMasterClient};

#[derive(Clone)]
pub struct AppServices {
    pub cluster: cluster::ClusterService,
    pub sandboxes: sandboxes::SandboxService,
    pub snapshots: snapshots::SnapshotService,
    pub templates: templates::TemplateService,
    #[allow(dead_code)]
    pub builds: builds::BuildRegistry,
}

impl AppServices {
    pub fn new(config: &ServerConfig, cubemaster: CubeMasterClient) -> Self {
        let builds = builds::BuildRegistry::new();
        Self {
            cluster: cluster::ClusterService::new(cubemaster.clone()),
            sandboxes: sandboxes::SandboxService::new(
                cubemaster.clone(),
                config.instance_type.clone(),
                config.sandbox_domain.clone(),
            ),
            snapshots: snapshots::SnapshotService::new(
                cubemaster.clone(),
                config.instance_type.clone(),
            ),
            templates: templates::TemplateService::new(
                cubemaster,
                config.instance_type.clone(),
                builds.clone(),
                config.clone(),
            ),
            builds,
        }
    }
}

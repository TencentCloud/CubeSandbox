// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

pub mod cluster;
pub mod sandboxes;
pub mod templates;

use crate::{config::ServerConfig, cubemaster::CubeMasterClient};

#[derive(Clone)]
pub struct AppServices {
    pub cluster: cluster::ClusterService,
    pub sandboxes: sandboxes::SandboxService,
    pub templates: templates::TemplateService,
}

impl AppServices {
    pub fn new(config: &ServerConfig, cubemaster: CubeMasterClient) -> Self {
        Self {
            cluster: cluster::ClusterService::new(cubemaster.clone()),
            sandboxes: sandboxes::SandboxService::new(
                cubemaster.clone(),
                config.instance_type.clone(),
                config.sandbox_domain.clone(),
            ),
            templates: templates::TemplateService::new(cubemaster, config.instance_type.clone()),
        }
    }
}

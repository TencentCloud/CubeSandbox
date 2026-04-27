// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::{fs, path::Path};

use utoipa::{
    openapi::security::{ApiKey, ApiKeyValue, HttpAuthScheme, HttpBuilder, SecurityScheme},
    Modify, OpenApi,
};

use crate::{
    handlers,
    models::{
        ApiError, ClusterOverview, NodeConditionView, NodeResourcesView, NodeView, ResumedSandbox,
        Sandbox, SandboxDetail, SandboxLogEntry, SandboxLogsV2Response, SandboxState,
        SandboxVolumeMount, TemplateDetail, TemplateSummary,
    },
};

struct SecurityAddon;

impl Modify for SecurityAddon {
    fn modify(&self, openapi: &mut utoipa::openapi::OpenApi) {
        let components = openapi.components.get_or_insert_with(Default::default);
        components.add_security_scheme(
            "bearerAuth",
            SecurityScheme::Http(
                HttpBuilder::new()
                    .scheme(HttpAuthScheme::Bearer)
                    .bearer_format("JWT")
                    .build(),
            ),
        );
        components.add_security_scheme(
            "apiKeyAuth",
            SecurityScheme::ApiKey(ApiKey::Header(ApiKeyValue::new("X-API-Key"))),
        );
    }
}

#[derive(OpenApi)]
#[openapi(
    info(
        title = "CubeAPI",
        version = "0.1.0",
        description = "OpenAPI contract for the CubeSandbox dashboard surface."
    ),
    paths(
        handlers::health::health,
        handlers::cluster::cluster_overview,
        handlers::cluster::list_nodes,
        handlers::cluster::get_node,
        handlers::templates::list_templates,
        handlers::templates::get_template,
        handlers::sandboxes::list_sandboxes_v2,
        handlers::sandboxes::get_sandbox,
        handlers::sandboxes::kill_sandbox,
        handlers::sandboxes::pause_sandbox,
        handlers::sandboxes::resume_sandbox,
        handlers::sandboxes::get_sandbox_logs_v2
    ),
    components(schemas(
        ApiError,
        handlers::health::HealthResponse,
        ClusterOverview,
        NodeResourcesView,
        NodeConditionView,
        NodeView,
        TemplateSummary,
        TemplateDetail,
        SandboxState,
        SandboxVolumeMount,
        crate::models::ListedSandbox,
        SandboxDetail,
        Sandbox,
        ResumedSandbox,
        SandboxLogEntry,
        SandboxLogsV2Response
    )),
    modifiers(&SecurityAddon),
    servers(
        (url = "/cubeapi/v1", description = "CubeAPI dashboard surface")
    ),
    tags(
        (name = "health", description = "Health and liveness"),
        (name = "cluster", description = "Cluster and node inventory"),
        (name = "templates", description = "Template catalog"),
        (name = "sandboxes", description = "Sandbox lifecycle and logs")
    )
)]
struct ApiDoc;

pub fn build_openapi() -> utoipa::openapi::OpenApi {
    ApiDoc::openapi()
}

pub fn export_to_file(path: impl AsRef<Path>) -> anyhow::Result<()> {
    let path = path.as_ref();
    let yaml = build_openapi().to_yaml()?;
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    fs::write(path, yaml)?;
    Ok(())
}

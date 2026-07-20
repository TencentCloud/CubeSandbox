// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use uuid::Uuid;

use super::validate_allow_out_domains_require_deny_all;
use crate::{
    cubemaster::{
        CreateTemplateContainerOverrides, CreateTemplateCubeNetworkConfig, CreateTemplateEnv,
        CreateTemplateFromImageReq, CreateTemplateResources, CubeMasterClient, CubeMasterError,
        DnsConfig, HttpGetAction, Probe, ProbeHandler, RedoTemplateReq, TemplateCompatAdoptRequest,
        TemplateDeleteRequest, TemplateJob, TemplateJobResponse,
    },
    error::{AppError, AppResult},
    models::{
        CreateTemplateRequest, RebuildTemplateRequest, TemplateAliasLookupResponse,
        TemplateBuildJob, TemplateBuildStatus, TemplateCompatMatrixView, TemplateCompatRowView,
        TemplateCompatSummaryView, TemplateDetail, TemplateNodeCompatView, TemplateSummary,
    },
};

const TEMPLATE_PUBLIC: bool = false;

#[derive(Clone)]
pub struct TemplateService {
    cubemaster: CubeMasterClient,
    instance_type: String,
}

impl TemplateService {
    pub fn new(cubemaster: CubeMasterClient, instance_type: String) -> Self {
        Self {
            cubemaster,
            instance_type,
        }
    }

    pub async fn list_templates(&self) -> AppResult<Vec<TemplateSummary>> {
        let resp = self
            .cubemaster
            .list_templates(None, false)
            .await
            .map_err(map_err)?;

        Ok(resp
            .data
            .into_iter()
            .map(template_summary_from_cubemaster)
            .collect())
    }

    pub async fn get_template(&self, template_id: &str) -> AppResult<TemplateDetail> {
        let resp = self
            .cubemaster
            .get_template(template_id)
            .await
            .map_err(map_err)?;

        if resp.template_id.is_empty() && resp.status.is_empty() {
            return Err(AppError::NotFound(format!(
                "template {} not found",
                template_id
            )));
        }

        // Extract network fields from create_request JSON (stored by CubeMaster)
        let network_type = resp
            .create_request
            .as_ref()
            .and_then(|v| v.get("network_type"))
            .and_then(|v| v.as_str())
            .and_then(|s| {
                if s.is_empty() {
                    None
                } else {
                    Some(s.to_string())
                }
            });
        let allow_internet_access = resp
            .create_request
            .as_ref()
            .and_then(|v| v.get("cube_network_config"))
            .and_then(|v| v.get("allowInternetAccess"))
            .and_then(|v| v.as_bool());

        Ok(TemplateDetail {
            template_id: string_or(resp.template_id, template_id),
            public: TEMPLATE_PUBLIC,
            instance_type: non_empty(resp.instance_type),
            version: non_empty(resp.version),
            status: resp.status,
            last_error: non_empty(resp.last_error),
            created_at: non_empty(resp.created_at),
            replicas: resp.replicas,
            create_request: resp.create_request,
            network_type,
            allow_internet_access,
            job_id: non_empty(resp.job_id),
            aliases: alias_values_from_display_name(&resp.display_name),
        })
    }

    pub async fn get_template_by_alias(
        &self,
        alias: &str,
    ) -> AppResult<TemplateAliasLookupResponse> {
        let alias = alias.trim();
        if alias.is_empty() {
            return Err(AppError::BadRequest("alias is required".to_string()));
        }
        if !is_valid_alias(alias) {
            return Err(AppError::BadRequest(
                "alias must match ^[a-z0-9][a-z0-9-]{0,63}$ and not start with tpl-/snap-"
                    .to_string(),
            ));
        }

        let detail = self.get_template(alias).await?;
        Ok(TemplateAliasLookupResponse {
            template_id: detail.template_id,
            public: TEMPLATE_PUBLIC,
        })
    }

    pub async fn create_template(
        &self,
        body: CreateTemplateRequest,
    ) -> AppResult<TemplateBuildJob> {
        if body.image.trim().is_empty() {
            return Err(AppError::BadRequest("image is required".to_string()));
        }

        let dns_servers = validate_dns_servers(body.dns.as_deref())?;
        let container_overrides = build_template_container_overrides(&body, dns_servers.as_deref());
        let cube_network_config = build_template_cube_network_config(&body)?;
        let alias = template_alias_from_request(&body);

        let req = CreateTemplateFromImageReq {
            request_id: new_request_id(),
            instance_type: body
                .instance_type
                .unwrap_or_else(|| self.instance_type.clone()),
            // template_id is intentionally left empty — CubeMaster always
            // auto-generates it with the "tpl-" prefix via
            // normalizeTemplateImageRequest.
            template_id: String::new(),
            source_image_ref: body.image.trim().to_string(),
            writable_layer_size: body.writable_layer_size,
            alias,
            exposed_ports: body.exposed_ports,
            network_type: non_empty_option(body.network_type),
            registry_username: non_empty_option(body.registry_username),
            registry_password: non_empty_option(body.registry_password),
            distribution_scope: non_empty_vec(body.nodes),
            container_overrides,
            cube_network_config,
            enable_ivshmem: body.enable_ivshmem,
            with_cube_ca: body.with_cube_ca,
        };

        let resp = self
            .cubemaster
            .create_template_from_image(&req)
            .await
            .map_err(map_err)?;

        Ok(to_job(resp))
    }

    pub async fn rebuild_template(
        &self,
        template_id: String,
        body: RebuildTemplateRequest,
    ) -> AppResult<TemplateBuildJob> {
        let req = RedoTemplateReq {
            request_id: new_request_id(),
            template_id,
            extra: body.extra,
        };

        let resp = self.cubemaster.redo_template(&req).await.map_err(map_err)?;

        Ok(to_job(resp))
    }

    pub async fn delete_template(
        &self,
        template_id: String,
        instance_type: Option<String>,
        sync: Option<bool>,
    ) -> AppResult<()> {
        let req = TemplateDeleteRequest {
            request_id: new_request_id(),
            template_id,
            instance_type: instance_type.unwrap_or_else(|| self.instance_type.clone()),
            sync: sync.unwrap_or(false),
        };

        self.cubemaster
            .delete_template(&req)
            .await
            .map_err(map_err)?;

        Ok(())
    }

    pub async fn start_template_build(&self, template_id: String) -> AppResult<TemplateBuildJob> {
        let req = RedoTemplateReq {
            request_id: new_request_id(),
            template_id,
            extra: Default::default(),
        };

        let resp = self.cubemaster.redo_template(&req).await.map_err(map_err)?;

        Ok(to_job(resp))
    }

    pub async fn get_template_build_status(
        &self,
        template_id: &str,
        build_id: &str,
    ) -> AppResult<TemplateBuildStatus> {
        let resp = self
            .cubemaster
            .get_template_build_status(build_id)
            .await
            .map_err(map_err)?;

        Ok(TemplateBuildStatus {
            build_id: string_or(resp.build_id, build_id),
            template_id: string_or(resp.template_id, template_id),
            status: resp.status,
            progress: resp.progress,
            message: resp.message,
        })
    }

    pub async fn get_template_build_logs(&self, build_id: &str) -> AppResult<serde_json::Value> {
        let resp = self
            .cubemaster
            .get_template_build_status(build_id)
            .await
            .map_err(map_err)?;

        let line = build_log_line(&resp.status, resp.progress, &resp.message);

        Ok(serde_json::json!({
            "buildID": build_id,
            "status": resp.status,
            "progress": resp.progress,
            "lines": [line],
        }))
    }

    pub async fn compat_matrix(&self) -> AppResult<TemplateCompatMatrixView> {
        let resp = self
            .cubemaster
            .get_template_compat()
            .await
            .map_err(map_err)?;
        Ok(to_compat_matrix_view(resp.data.unwrap_or_default()))
    }

    pub async fn adopt_compat_baseline(&self, template_id: String) -> AppResult<i32> {
        let req = TemplateCompatAdoptRequest {
            action: "adopt_baseline".to_string(),
            template_id,
        };
        let resp = self
            .cubemaster
            .adopt_template_compat_baseline(&req)
            .await
            .map_err(map_err)?;
        Ok(resp.updated)
    }
}

fn map_err(e: CubeMasterError) -> AppError {
    if e.is_invalid_path_parameter() || e.is_params_error() {
        AppError::BadRequest(e.to_string())
    } else if e.is_not_found() || e.is_endpoint_missing() {
        AppError::NotFound(e.to_string())
    } else if e.is_conflict() {
        AppError::Conflict(e.to_string())
    } else {
        AppError::Internal(anyhow::anyhow!(e))
    }
}

fn new_request_id() -> String {
    Uuid::new_v4().to_string()
}

fn non_empty(s: String) -> Option<String> {
    if s.trim().is_empty() {
        None
    } else {
        Some(s)
    }
}

fn string_or(value: String, fallback: &str) -> String {
    if value.is_empty() {
        fallback.to_string()
    } else {
        value
    }
}

fn alias_values_from_display_name(display_name: &str) -> Vec<String> {
    let alias = display_name.trim();
    if alias.is_empty() {
        Vec::new()
    } else {
        vec![alias.to_string()]
    }
}

fn template_summary_from_cubemaster(s: crate::cubemaster::TemplateSummaryItem) -> TemplateSummary {
    TemplateSummary {
        template_id: s.template_id,
        public: TEMPLATE_PUBLIC,
        instance_type: non_empty(s.instance_type),
        version: non_empty(s.version),
        status: s.status,
        last_error: non_empty(s.last_error),
        created_at: non_empty(s.created_at),
        image_info: non_empty(s.image_info),
        job_id: non_empty(s.job_id),
        aliases: alias_values_from_display_name(&s.display_name),
    }
}

fn template_alias_from_request(body: &CreateTemplateRequest) -> Option<String> {
    body.name
        .as_deref()
        .and_then(alias_from_name)
        .or_else(|| body.alias.as_deref().and_then(alias_from_name))
}

fn alias_from_name(value: &str) -> Option<String> {
    let name = value.trim();
    if name.is_empty() {
        return None;
    }

    // Strip the tag suffix (after the last ':') only when the remainder
    // contains no '/' — this avoids mistaking a registry port (e.g.
    // "registry:5000/team/app:v2") for a tag delimiter. The last path
    // component after '/' is then taken as the alias, mirroring E2B's
    // flat global namespace.
    let without_tag = match name.rsplit_once(':') {
        Some((before, after)) if !after.contains('/') => before,
        _ => name,
    }
    .trim();
    let alias = without_tag.rsplit('/').next().unwrap_or(without_tag).trim();
    if alias.is_empty() {
        None
    } else if is_valid_alias(alias) {
        Some(alias.to_string())
    } else {
        None
    }
}

/// Returns true if the alias matches the same regex CubeMaster enforces:
/// ^[a-z0-9][a-z0-9-]{0,63}$ . Aliases derived from names that don't
/// conform (e.g. "a:b:c" → "a:b", "UPPER" → "UPPER") are silently
/// dropped rather than forwarded to CubeMaster, where they'd fail with
/// a less helpful error.
fn is_valid_alias(alias: &str) -> bool {
    if alias.is_empty() || alias.len() > 64 {
        return false;
    }
    // Reject canonical infrastructure-ID prefixes so a derived alias can never
    // collide with a real `tpl-*` template or `snap-*` snapshot id. This
    // mirrors the prefix guard enforced on the alias *lookup* path
    // (get_template_by_alias) and keeps the create path consistent (§1.4).
    if alias.starts_with("tpl-") || alias.starts_with("snap-") {
        return false;
    }
    alias.chars().enumerate().all(|(i, c)| {
        if i == 0 {
            c.is_ascii_lowercase() || c.is_ascii_digit()
        } else {
            c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-'
        }
    })
}

fn build_log_line(status: &str, progress: i32, message: &str) -> String {
    if message.is_empty() {
        format!("[{}] progress={}%", status, progress)
    } else {
        format!("[{}] {}", status, message)
    }
}

fn to_compat_matrix_view(src: crate::cubemaster::TemplateCompatMatrix) -> TemplateCompatMatrixView {
    TemplateCompatMatrixView {
        summary: TemplateCompatSummaryView {
            stale_templates: src.summary.stale_templates,
            stale_replicas: src.summary.stale_replicas,
            affected_nodes: src.summary.affected_nodes,
            missing_replicas: src.summary.missing_replicas,
            unknown_replicas: src.summary.unknown_replicas,
        },
        templates: src
            .templates
            .into_iter()
            .map(|row| TemplateCompatRowView {
                template_id: row.template_id,
                instance_type: non_empty(row.instance_type),
                overall: row.overall,
                nodes: row
                    .nodes
                    .into_iter()
                    .map(|node| TemplateNodeCompatView {
                        node_id: node.node_id,
                        node_ip: non_empty(node.node_ip),
                        compat_status: node.compat_status,
                        bound_guest_image_version: non_empty(node.bound_guest_image_version),
                        current_guest_image_version: non_empty(node.current_guest_image_version),
                        bound_agent_version: non_empty(node.bound_agent_version),
                        current_agent_version: non_empty(node.current_agent_version),
                        bound_kernel_version: non_empty(node.bound_kernel_version),
                        current_kernel_version: non_empty(node.current_kernel_version),
                    })
                    .collect(),
            })
            .collect(),
    }
}

fn to_job(resp: TemplateJobResponse) -> TemplateBuildJob {
    let job = resp.job.unwrap_or_else(default_template_job);
    TemplateBuildJob {
        job_id: job.job_id,
        template_id: job.template_id,
        status: job.status,
        phase: job.phase,
        progress: job.progress,
        error_message: job.error_message,
    }
}

fn default_template_job() -> TemplateJob {
    TemplateJob {
        job_id: String::new(),
        template_id: String::new(),
        status: "accepted".to_string(),
        phase: String::new(),
        progress: 0,
        error_message: String::new(),
        attempt_no: 0,
        retry_of_job_id: String::new(),
    }
}

fn non_empty_option(value: Option<String>) -> Option<String> {
    value.and_then(|s| non_empty(s))
}

fn non_empty_vec(values: Option<Vec<String>>) -> Option<Vec<String>> {
    values.and_then(|items| {
        let cleaned: Vec<String> = items
            .into_iter()
            .filter_map(|item| non_empty(item))
            .collect();
        if cleaned.is_empty() {
            None
        } else {
            Some(cleaned)
        }
    })
}

fn validate_dns_servers(servers: Option<&[String]>) -> AppResult<Option<Vec<String>>> {
    let Some(servers) = servers else {
        return Ok(None);
    };
    let mut cleaned = Vec::new();
    for server in servers {
        let server = server.trim();
        if server.is_empty() {
            continue;
        }
        if server.parse::<std::net::IpAddr>().is_err() {
            return Err(AppError::BadRequest(format!(
                "invalid dns server {server:?}"
            )));
        }
        cleaned.push(server.to_string());
    }
    if cleaned.is_empty() {
        Ok(None)
    } else {
        Ok(Some(cleaned))
    }
}

fn build_template_probe(body: &CreateTemplateRequest) -> Option<Probe> {
    body.probe_port
        .or_else(|| body.exposed_ports.as_ref().and_then(|p| p.first().copied()))
        .map(|port| Probe {
            probe_handler: ProbeHandler {
                http_get: Some(HttpGetAction {
                    path: body
                        .probe_path
                        .clone()
                        .unwrap_or_else(|| "/health".to_string()),
                    port,
                    host: None,
                    scheme: None,
                }),
                exec: None,
            },
            timeout_ms: Some(30000),
            period_ms: Some(500),
            success_threshold: Some(1),
            failure_threshold: Some(60),
        })
}

fn build_template_resources(body: &CreateTemplateRequest) -> Option<CreateTemplateResources> {
    if body.cpu.is_none() && body.memory.is_none() {
        return None;
    }
    Some(CreateTemplateResources {
        cpu: body.cpu.map(|v| format!("{v}m")),
        mem: body.memory.map(|v| format!("{v}Mi")),
    })
}

fn build_template_envs(body: &CreateTemplateRequest) -> Option<Vec<CreateTemplateEnv>> {
    body.env
        .as_ref()
        .map(|envs| {
            envs.iter()
                .filter_map(|s| {
                    let mut parts = s.splitn(2, '=');
                    let key = parts.next()?.trim().to_string();
                    let value = parts.next().unwrap_or("").to_string();
                    if key.is_empty() {
                        None
                    } else {
                        Some(CreateTemplateEnv { key, value })
                    }
                })
                .collect::<Vec<_>>()
        })
        .filter(|envs| !envs.is_empty())
}

fn build_template_container_overrides(
    body: &CreateTemplateRequest,
    dns_servers: Option<&[String]>,
) -> Option<CreateTemplateContainerOverrides> {
    let command = non_empty_vec(body.command.clone());
    let args = non_empty_vec(body.args.clone());
    let probe = build_template_probe(body);
    let resources = build_template_resources(body);
    let envs = build_template_envs(body);
    let dns_config = dns_servers.map(|servers| DnsConfig {
        servers: servers.to_vec(),
        searches: Vec::new(),
    });

    if command.is_none()
        && args.is_none()
        && probe.is_none()
        && resources.is_none()
        && envs.is_none()
        && dns_config.is_none()
    {
        return None;
    }

    Some(CreateTemplateContainerOverrides {
        command,
        args,
        probe,
        resources,
        envs,
        dns_config,
    })
}

fn build_template_cube_network_config(
    body: &CreateTemplateRequest,
) -> AppResult<Option<CreateTemplateCubeNetworkConfig>> {
    let allow_out = body.allow_out.clone().unwrap_or_default();
    let deny_out = body.deny_out.clone().unwrap_or_default();
    validate_allow_out_domains_require_deny_all(
        &allow_out,
        &deny_out,
        body.allow_internet_access == Some(false),
    )?;

    if body.allow_internet_access.is_none() && allow_out.is_empty() && deny_out.is_empty() {
        return Ok(None);
    }
    Ok(Some(CreateTemplateCubeNetworkConfig {
        allow_internet_access: body.allow_internet_access,
        allow_out,
        deny_out,
    }))
}

#[cfg(test)]
mod tests {
    use super::*;

    async fn spawn_server(app: axum::Router) -> String {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("listener should bind");
        let addr = listener.local_addr().expect("listener addr");
        tokio::spawn(async move {
            axum::serve(listener, app).await.expect("server should run");
        });
        format!("http://{}", addr)
    }

    fn sample_request() -> CreateTemplateRequest {
        CreateTemplateRequest {
            template_id: String::new(),
            name: None,
            alias: None,
            instance_type: Some("cubebox".to_string()),
            image: "python:3.11-slim".to_string(),
            writable_layer_size: Some("1G".to_string()),
            exposed_ports: Some(vec![8080]),
            probe_port: Some(8080),
            probe_path: Some("/health".to_string()),
            cpu: Some(2000),
            memory: Some(2048),
            env: Some(vec!["A=1".to_string()]),
            allow_internet_access: Some(true),
            network_type: Some("tap".to_string()),
            nodes: Some(vec!["node-1".to_string()]),
            registry_username: Some("user".to_string()),
            registry_password: Some("pass".to_string()),
            command: Some(vec!["/bin/sh".to_string(), "-c".to_string()]),
            args: Some(vec!["sleep infinity".to_string()]),
            dns: Some(vec!["8.8.8.8".to_string(), "1.1.1.1".to_string()]),
            allow_out: Some(vec!["172.67.0.0/16".to_string()]),
            deny_out: Some(vec!["10.0.0.0/8".to_string()]),
            enable_ivshmem: Some(true),
            with_cube_ca: Some(false),
        }
    }

    #[test]
    fn build_template_container_overrides_maps_cli_fields() {
        let body = sample_request();
        let overrides = build_template_container_overrides(&body, Some(&["8.8.8.8".to_string()]))
            .expect("overrides");

        assert_eq!(
            overrides.command,
            Some(vec!["/bin/sh".to_string(), "-c".to_string()])
        );
        assert_eq!(overrides.args, Some(vec!["sleep infinity".to_string()]));
        assert_eq!(
            overrides.dns_config.as_ref().map(|d| d.servers.clone()),
            Some(vec!["8.8.8.8".to_string()])
        );
        assert!(overrides.probe.is_some());
        assert!(overrides.resources.is_some());
        assert_eq!(overrides.envs.as_ref().map(|envs| envs.len()), Some(1));
    }

    #[test]
    fn alias_values_from_display_name_returns_e2b_arrays() {
        assert_eq!(
            alias_values_from_display_name(" stable-python "),
            vec!["stable-python".to_string()]
        );
        assert!(alias_values_from_display_name("   ").is_empty());
    }

    #[test]
    fn template_name_prefers_name_over_alias_and_strips_tag() {
        let mut body = sample_request();
        body.name = Some("stable-python:v1".to_string());
        body.alias = Some("legacy-python".to_string());

        assert_eq!(
            template_alias_from_request(&body).as_deref(),
            Some("stable-python")
        );
    }

    #[test]
    fn template_name_accepts_e2b_namespace_and_strips_tag() {
        let mut body = sample_request();
        body.name = Some("team-slug/my-app:v2".to_string());

        assert_eq!(
            template_alias_from_request(&body).as_deref(),
            Some("my-app")
        );
    }

    #[test]
    fn template_name_strips_tag_but_not_registry_port() {
        let mut body = sample_request();
        body.name = Some("registry:5000/team/app:v2".to_string());

        assert_eq!(template_alias_from_request(&body).as_deref(), Some("app"));
    }

    #[test]
    fn template_name_falls_back_to_deprecated_alias() {
        let mut body = sample_request();
        body.name = Some("   ".to_string());
        body.alias = Some("legacy-python".to_string());

        assert_eq!(
            template_alias_from_request(&body).as_deref(),
            Some("legacy-python")
        );
    }

    #[test]
    fn template_summary_from_cubemaster_maps_display_name_to_aliases() {
        let summary = template_summary_from_cubemaster(crate::cubemaster::TemplateSummaryItem {
            template_id: "tpl-1".to_string(),
            instance_type: "cubebox".to_string(),
            version: "v1".to_string(),
            status: "ready".to_string(),
            last_error: String::new(),
            display_name: "stable-python".to_string(),
            created_at: "2026-07-06T00:00:00Z".to_string(),
            image_info: "python:3.11".to_string(),
            job_id: "job-1".to_string(),
        });

        assert_eq!(summary.aliases, vec!["stable-python".to_string()]);
        assert!(!summary.public);
    }

    #[test]
    fn template_summary_from_cubemaster_uses_empty_aliases_without_display_name() {
        let summary = template_summary_from_cubemaster(crate::cubemaster::TemplateSummaryItem {
            template_id: "tpl-1".to_string(),
            instance_type: String::new(),
            version: String::new(),
            status: "ready".to_string(),
            last_error: String::new(),
            display_name: String::new(),
            created_at: String::new(),
            image_info: String::new(),
            job_id: String::new(),
        });

        assert!(summary.aliases.is_empty());
        assert!(!summary.public);
    }

    #[test]
    fn is_valid_alias_accepts_plain_lowercase_slug() {
        assert!(is_valid_alias("my-app"));
        assert!(is_valid_alias("stable-python-v2"));
        assert!(is_valid_alias("a"));
        assert!(is_valid_alias("a1"));
    }

    #[test]
    fn is_valid_alias_rejects_canonical_id_prefixes() {
        // §1.4: a derived alias must never collide with a real `tpl-*` /
        // `snap-*` infrastructure id — otherwise the alias-lookup path and
        // the canonical-id path could address the same resource two ways.
        assert!(!is_valid_alias("tpl-abc123"));
        assert!(!is_valid_alias("tpl-1"));
        assert!(!is_valid_alias("snap-deadbeef"));
        assert!(!is_valid_alias("tpl-"));
        assert!(!is_valid_alias("snap-"));
    }

    #[test]
    fn is_valid_alias_rejects_other_invalid_forms() {
        assert!(!is_valid_alias(""));
        assert!(!is_valid_alias("UPPER"));
        assert!(!is_valid_alias("-leading-dash"));
        assert!(!is_valid_alias("has space"));
        assert!(!is_valid_alias(&"x".repeat(65)));
        // Note: a trailing dash IS permitted by CubeMaster's regex
        // ^[a-z0-9][a-z0-9-]{0,63}$, so we don't reject it here.
        assert!(is_valid_alias("trailing-dash-"));
    }

    #[test]
    fn build_template_cube_network_config_includes_egress_rules() {
        let body = sample_request();
        let cfg = build_template_cube_network_config(&body)
            .expect("network config should be valid")
            .expect("cube_network_config");
        assert_eq!(cfg.allow_internet_access, Some(true));
        assert_eq!(cfg.allow_out, vec!["172.67.0.0/16".to_string()]);
        assert_eq!(cfg.deny_out, vec!["10.0.0.0/8".to_string()]);
    }

    #[test]
    fn build_template_cube_network_config_rejects_allow_out_domain_without_deny_all() {
        let mut body = sample_request();
        body.allow_internet_access = Some(true);
        body.allow_out = Some(vec!["api.example.com".to_string()]);
        body.deny_out = Some(vec!["203.0.113.0/24".to_string()]);

        let err = build_template_cube_network_config(&body).unwrap_err();
        assert!(err
            .to_string()
            .contains("must disable public outbound traffic or include '0.0.0.0/0' in deny_out"));
    }

    #[test]
    fn build_template_cube_network_config_accepts_domain_when_internet_disabled() {
        let mut body = sample_request();
        body.allow_internet_access = Some(false);
        body.allow_out = Some(vec!["api.example.com".to_string()]);
        body.deny_out = None;

        let cfg = build_template_cube_network_config(&body)
            .expect("network config should be valid")
            .expect("cube_network_config");
        assert_eq!(cfg.allow_internet_access, Some(false));
        assert_eq!(cfg.allow_out, vec!["api.example.com".to_string()]);
    }

    #[test]
    fn validate_dns_servers_rejects_invalid_ip() {
        let err = validate_dns_servers(Some(&["not-an-ip".to_string()])).unwrap_err();
        assert!(matches!(err, AppError::BadRequest(_)));
    }

    #[tokio::test]
    async fn create_template_forwards_name_as_cubemaster_alias() {
        use axum::{extract::State, routing::post, Json, Router};
        use serde_json::Value;
        use std::sync::Arc;
        use tokio::sync::Mutex;

        #[derive(Clone, Default)]
        struct Capture {
            body: Arc<Mutex<Option<Value>>>,
        }

        async fn create_handler(
            State(capture): State<Capture>,
            Json(body): Json<Value>,
        ) -> Json<Value> {
            *capture.body.lock().await = Some(body);
            Json(serde_json::json!({
                "RequestID": "req-1",
                "ret": { "ret_code": 0, "ret_msg": "success" },
                "job": {
                    "job_id": "job-1",
                    "template_id": "tpl-1",
                    "status": "accepted",
                    "phase": "",
                    "progress": 0,
                    "error_message": "",
                    "attempt_no": 1,
                    "retry_of_job_id": ""
                }
            }))
        }

        let capture = Capture::default();
        let cubemaster_url = spawn_server(
            Router::new()
                .route("/cube/template/from-image", post(create_handler))
                .with_state(capture.clone()),
        )
        .await;

        let service = TemplateService::new(
            CubeMasterClient::new(cubemaster_url, reqwest::Client::new()),
            "cubebox".to_string(),
        );
        let mut req = sample_request();
        req.name = Some("team-slug/stable-python:v1".to_string());
        req.alias = Some("legacy-python".to_string());

        service
            .create_template(req)
            .await
            .expect("template create should succeed");

        let body = capture
            .body
            .lock()
            .await
            .clone()
            .expect("request body should be captured");
        assert_eq!(body["alias"], "stable-python");
    }

    #[tokio::test]
    async fn get_template_maps_created_at_and_private_visibility() {
        use axum::{routing::get, Json, Router};

        async fn get_template_handler() -> Json<serde_json::Value> {
            Json(serde_json::json!({
                "RequestID": "req-1",
                "ret": { "ret_code": 0, "ret_msg": "success" },
                "template_id": "tpl-created",
                "display_name": "stable-python",
                "created_at": "2026-07-06T00:00:00Z",
                "status": "ready",
                "replicas": []
            }))
        }

        let cubemaster_url =
            spawn_server(Router::new().route("/cube/template", get(get_template_handler))).await;
        let service = TemplateService::new(
            CubeMasterClient::new(cubemaster_url, reqwest::Client::new()),
            "cubebox".to_string(),
        );

        let detail = service
            .get_template("tpl-created")
            .await
            .expect("template lookup should succeed");

        assert_eq!(detail.created_at.as_deref(), Some("2026-07-06T00:00:00Z"));
        assert!(!detail.public);
    }

    #[tokio::test]
    async fn get_template_by_alias_returns_e2b_lookup_response() {
        use axum::{extract::Query, routing::get, Json, Router};
        use std::collections::HashMap;

        async fn get_template_handler(
            Query(params): Query<HashMap<String, String>>,
        ) -> Json<serde_json::Value> {
            assert_eq!(
                params.get("template_id").map(String::as_str),
                Some("stable-python")
            );
            assert_eq!(
                params.get("include_request").map(String::as_str),
                Some("true")
            );
            Json(serde_json::json!({
                "RequestID": "req-1",
                "ret": { "ret_code": 0, "ret_msg": "success" },
                "template_id": "tpl-abc",
                "display_name": "stable-python",
                "status": "ready",
                "replicas": []
            }))
        }

        let cubemaster_url =
            spawn_server(Router::new().route("/cube/template", get(get_template_handler))).await;
        let service = TemplateService::new(
            CubeMasterClient::new(cubemaster_url, reqwest::Client::new()),
            "cubebox".to_string(),
        );

        let resp = service
            .get_template_by_alias("stable-python")
            .await
            .expect("alias lookup should succeed");

        assert_eq!(resp.template_id, "tpl-abc");
        assert!(!resp.public);
    }
}

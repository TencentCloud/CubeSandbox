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
        CreateTemplateRequest, RebuildTemplateRequest, TemplateBuildJob, TemplateBuildStatus,
        TemplateCompatMatrixView, TemplateCompatRowView, TemplateCompatSummaryView, TemplateDetail,
        TemplateNodeCompatView, TemplateSummary, UpdateTemplateRequest,
    },
};

// Resolve human-readable template names to template IDs at CubeAPI entry points.
// References that already look like `tpl-*` or `snap-*` ids bypass lookup.
// Read paths use CubeMaster's brief display-name cache; mutating paths (create
// sandbox, rename, delete, rebuild) pass fresh=true to bypass that cache.

#[derive(Clone)]
pub struct TemplateNameCache {
    cubemaster: CubeMasterClient,
}

impl TemplateNameCache {
    pub fn new(cubemaster: CubeMasterClient) -> Self {
        Self { cubemaster }
    }

    /// Returns true when the reference should be resolved via display_name lookup.
    pub fn needs_resolution(reference: &str) -> bool {
        let reference = reference.trim();
        if reference.is_empty() {
            return false;
        }
        let lower = reference.to_ascii_lowercase();
        !lower.starts_with("tpl-") && !lower.starts_with("snap-")
    }

    /// Resolve a template reference via CubeMaster name lookup.
    pub async fn resolve_template_ref(&self, reference: &str) -> AppResult<String> {
        self.resolve(reference, false).await
    }

    /// Resolve bypassing CubeMaster read cache (mutating API paths).
    pub async fn resolve_template_ref_fresh(&self, reference: &str) -> AppResult<String> {
        self.resolve(reference, true).await
    }

    async fn resolve(&self, reference: &str, fresh: bool) -> AppResult<String> {
        let reference = reference.trim();
        if reference.is_empty() {
            return Err(AppError::BadRequest(
                "template reference is empty".to_string(),
            ));
        }
        if !Self::needs_resolution(reference) {
            return Ok(reference.to_string());
        }
        if fresh {
            self.cubemaster
                .resolve_template_by_name_fresh(reference)
                .await
                .map_err(map_resolve_err(reference))
        } else {
            self.cubemaster
                .resolve_template_by_name(reference)
                .await
                .map_err(map_resolve_err(reference))
        }
    }
}

pub fn template_names(display_name: &str) -> Vec<String> {
    let display_name = display_name.trim();
    if display_name.is_empty() {
        Vec::new()
    } else {
        vec![display_name.to_string()]
    }
}

fn map_resolve_err(reference: &str) -> impl FnOnce(CubeMasterError) -> AppError {
    let reference = reference.to_string();
    move |err| match err {
        CubeMasterError::Api { ret_code, .. } if ret_code == 130404 => {
            AppError::NotFound(format!("template name {reference} not found"))
        }
        CubeMasterError::Api {
            ret_code, ret_msg, ..
        } if ret_code == 130409 => AppError::Conflict(ret_msg),
        CubeMasterError::Api {
            ret_code, ret_msg, ..
        } if ret_code == 130400 => AppError::BadRequest(ret_msg),
        CubeMasterError::Http(e) if e.is_timeout() || e.is_connect() => {
            AppError::ServiceUnavailable("CubeMaster unavailable".to_string())
        }
        CubeMasterError::Http(_) => AppError::BadGateway("CubeMaster request failed".to_string()),
        other => AppError::Internal(anyhow::anyhow!(other)),
    }
}

fn trim_display_name(raw: &str) -> String {
    raw.trim().to_string()
}

#[derive(Clone)]
pub struct TemplateService {
    cubemaster: CubeMasterClient,
    instance_type: String,
    template_names: TemplateNameCache,
}

impl TemplateService {
    pub fn new(
        cubemaster: CubeMasterClient,
        instance_type: String,
        template_names: TemplateNameCache,
    ) -> Self {
        Self {
            cubemaster,
            instance_type,
            template_names,
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
            .map(|s| TemplateSummary {
                template_id: s.template_id,
                names: template_names(&s.display_name),
                instance_type: non_empty(s.instance_type),
                version: non_empty(s.version),
                status: s.status,
                last_error: non_empty(s.last_error),
                created_at: non_empty(s.created_at),
                image_info: non_empty(s.image_info),
                job_id: non_empty(s.job_id),
            })
            .collect())
    }

    pub async fn get_template(&self, template_ref: &str) -> AppResult<TemplateDetail> {
        let template_id = self
            .template_names
            .resolve_template_ref(template_ref)
            .await?;
        let resp = self
            .fetch_resolved_template(template_ref, &template_id)
            .await?;

        Ok(template_detail_from_cm_response(
            &resp,
            &template_id,
            resp.display_name.as_str(),
        ))
    }

    /// Resolve a display name to template ID (lightweight lookup for UI hints).
    pub async fn lookup_template_name(&self, name: &str) -> AppResult<String> {
        self.template_names.resolve_template_ref(name).await
    }

    pub async fn create_template(
        &self,
        body: CreateTemplateRequest,
    ) -> AppResult<TemplateBuildJob> {
        if body.image.trim().is_empty() {
            return Err(AppError::BadRequest("image is required".to_string()));
        }

        let display_name = body
            .name
            .as_deref()
            .map(trim_display_name)
            .filter(|name| !name.is_empty());
        let dns_servers = validate_dns_servers(body.dns.as_deref())?;
        let container_overrides = build_template_container_overrides(&body, dns_servers.as_deref());
        let cube_network_config = build_template_cube_network_config(&body)?;

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
            display_name: display_name.clone(),
            writable_layer_size: body.writable_layer_size,
            exposed_ports: body.exposed_ports,
            network_type: non_empty_option(body.network_type),
            registry_username: non_empty_option(body.registry_username),
            registry_password: non_empty_option(body.registry_password),
            distribution_scope: non_empty_vec(body.nodes),
            container_overrides,
            cube_network_config,
            with_cube_ca: body.with_cube_ca,
        };

        let resp = self
            .cubemaster
            .create_template_from_image(&req)
            .await
            .map_err(map_err)?;

        Ok(to_job(resp, display_name.as_deref()))
    }

    pub async fn update_template_name(
        &self,
        template_ref: String,
        body: UpdateTemplateRequest,
    ) -> AppResult<TemplateDetail> {
        let template_id = self
            .template_names
            .resolve_template_ref_fresh(&template_ref)
            .await?;
        let name = trim_display_name(&body.name);
        if name.is_empty() {
            return Err(AppError::BadRequest("name is required".to_string()));
        }

        let existing = self
            .fetch_resolved_template(&template_ref, &template_id)
            .await?;

        self.cubemaster
            .update_template_display_name(&template_id, &name)
            .await
            .map_err(map_err)?;

        Ok(template_detail_from_cm_response(
            &existing,
            &template_id,
            &name,
        ))
    }

    pub async fn rebuild_template(
        &self,
        template_ref: String,
        body: RebuildTemplateRequest,
    ) -> AppResult<TemplateBuildJob> {
        let template_id = self
            .template_names
            .resolve_template_ref_fresh(&template_ref)
            .await?;
        let req = RedoTemplateReq {
            request_id: new_request_id(),
            template_id,
            extra: body.extra,
        };

        let resp = self.cubemaster.redo_template(&req).await.map_err(map_err)?;

        Ok(to_job(resp, None))
    }

    pub async fn delete_template(
        &self,
        template_ref: String,
        instance_type: Option<String>,
        sync: Option<bool>,
    ) -> AppResult<()> {
        let template_id = self
            .template_names
            .resolve_template_ref_fresh(&template_ref)
            .await?;
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

    pub async fn start_template_build(&self, template_ref: String) -> AppResult<TemplateBuildJob> {
        let template_id = self
            .template_names
            .resolve_template_ref_fresh(&template_ref)
            .await?;
        let req = RedoTemplateReq {
            request_id: new_request_id(),
            template_id,
            extra: Default::default(),
        };

        let resp = self.cubemaster.redo_template(&req).await.map_err(map_err)?;

        Ok(to_job(resp, None))
    }

    pub async fn get_template_build_status(
        &self,
        template_ref: &str,
        build_id: &str,
    ) -> AppResult<TemplateBuildStatus> {
        let template_id = self
            .template_names
            .resolve_template_ref(template_ref)
            .await?;
        let resp = self
            .cubemaster
            .get_template_build_status(build_id)
            .await
            .map_err(map_err)?;

        ensure_build_belongs_to_template(resp.template_id.as_str(), &template_id, build_id)?;

        Ok(TemplateBuildStatus {
            build_id: string_or(resp.build_id, build_id),
            template_id: string_or(resp.template_id, &template_id),
            status: resp.status,
            progress: resp.progress,
            message: resp.message,
        })
    }

    pub async fn get_template_build_logs(
        &self,
        template_ref: &str,
        build_id: &str,
    ) -> AppResult<serde_json::Value> {
        let template_id = self
            .template_names
            .resolve_template_ref(template_ref)
            .await?;
        let resp = self
            .cubemaster
            .get_template_build_status(build_id)
            .await
            .map_err(map_err)?;

        ensure_build_belongs_to_template(resp.template_id.as_str(), &template_id, build_id)?;

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

    pub async fn adopt_compat_baseline(&self, template_ref: String) -> AppResult<i32> {
        let template_id = self
            .template_names
            .resolve_template_ref_fresh(&template_ref)
            .await?;
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

    async fn fetch_resolved_template(
        &self,
        template_ref: &str,
        template_id: &str,
    ) -> AppResult<crate::cubemaster::TemplateResponse> {
        let resp = self
            .cubemaster
            .get_template(template_id)
            .await
            .map_err(map_err)?;

        if resp.template_id.is_empty() && resp.status.is_empty() {
            return Err(AppError::NotFound(format!(
                "template {} not found",
                template_ref
            )));
        }

        Ok(resp)
    }
}

fn ensure_build_belongs_to_template(
    build_template_id: &str,
    expected_template_id: &str,
    build_id: &str,
) -> AppResult<()> {
    let build_template_id = build_template_id.trim();
    if build_template_id.is_empty() {
        return Err(AppError::NotFound(format!(
            "build {build_id} not found for template {expected_template_id}"
        )));
    }
    if build_template_id != expected_template_id {
        return Err(AppError::NotFound(format!(
            "build {build_id} not found for template {expected_template_id}"
        )));
    }
    Ok(())
}

fn map_err(e: CubeMasterError) -> AppError {
    if e.is_invalid_path_parameter() || e.is_bad_request() {
        AppError::BadRequest(e.to_string())
    } else if e.is_not_found() || e.is_endpoint_missing() {
        AppError::NotFound(e.to_string())
    } else if e.is_conflict() {
        AppError::Conflict(e.to_string())
    } else {
        AppError::Internal(anyhow::anyhow!(e))
    }
}

fn template_detail_from_cm_response(
    resp: &crate::cubemaster::TemplateResponse,
    fallback_template_id: &str,
    display_name: &str,
) -> TemplateDetail {
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

    TemplateDetail {
        template_id: string_or(resp.template_id.clone(), fallback_template_id),
        names: template_names(display_name),
        instance_type: non_empty(resp.instance_type.clone()),
        version: non_empty(resp.version.clone()),
        status: resp.status.clone(),
        last_error: non_empty(resp.last_error.clone()),
        replicas: resp.replicas.clone(),
        create_request: resp.create_request.clone(),
        network_type,
        allow_internet_access,
        job_id: non_empty(resp.job_id.clone()),
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

fn to_job(resp: TemplateJobResponse, display_name: Option<&str>) -> TemplateBuildJob {
    let job = resp.job.unwrap_or_else(default_template_job);
    TemplateBuildJob {
        job_id: job.job_id.clone(),
        build_id: job.job_id,
        template_id: job.template_id,
        names: display_name.map(template_names).unwrap_or_default(),
        status: job.status,
        phase: job.phase,
        progress: job.progress,
        error_message: job.error_message,
    }
}

fn optional_name(name: Option<&str>) -> Option<String> {
    name.map(str::trim)
        .filter(|value| !value.is_empty())
        .map(str::to_string)
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
    use crate::cubemaster::CubeMasterClient;
    use crate::models::UpdateTemplateRequest;

    fn sample_request() -> CreateTemplateRequest {
        CreateTemplateRequest {
            name: Some("my-python-env".to_string()),
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

    #[test]
    fn template_detail_from_cm_response_overrides_display_name() {
        let resp = crate::cubemaster::TemplateResponse {
            request_id: String::new(),
            ret: crate::cubemaster::RetCode {
                ret_code: 200,
                ret_msg: "ok".to_string(),
            },
            template_id: "tpl-1".to_string(),
            display_name: "old-name".to_string(),
            job_id: String::new(),
            status: "ready".to_string(),
            instance_type: String::new(),
            version: String::new(),
            last_error: String::new(),
            replicas: Vec::new(),
            create_request: None,
        };
        let detail = template_detail_from_cm_response(&resp, "tpl-1", "new-name");
        assert_eq!(detail.names, vec!["new-name".to_string()]);
        assert_eq!(detail.template_id, "tpl-1");
    }

    #[test]
    fn needs_resolution_skips_system_ids() {
        assert!(!TemplateNameCache::needs_resolution("tpl-abc"));
        assert!(!TemplateNameCache::needs_resolution("snap-abc"));
        assert!(!TemplateNameCache::needs_resolution("TPL-abc"));
        assert!(TemplateNameCache::needs_resolution("cubesandbox-template"));
    }

    #[tokio::test]
    async fn map_resolve_err_maps_http_connect_failure_to_service_unavailable() {
        let client = reqwest::Client::builder()
            .connect_timeout(std::time::Duration::from_millis(50))
            .build()
            .expect("client");
        let err = client
            .get("http://127.0.0.1:1")
            .send()
            .await
            .expect_err("connect should fail");
        let mapped = super::map_resolve_err("my-env")(CubeMasterError::Http(err));
        match mapped {
            AppError::ServiceUnavailable(msg) => {
                assert_eq!(msg, "CubeMaster unavailable");
            }
            AppError::BadGateway(msg) => {
                assert_eq!(msg, "CubeMaster request failed");
            }
            other => panic!("unexpected error: {other:?}"),
        }
    }

    #[test]
    fn trim_display_name_strips_whitespace() {
        assert_eq!(super::trim_display_name(" my-env_1 "), "my-env_1");
        assert_eq!(super::trim_display_name("  "), "");
    }

    #[test]
    fn map_resolve_err_maps_not_found_conflict_and_bad_request() {
        let not_found = CubeMasterError::Api {
            ret_code: 130404,
            ret_msg: "template name not found".to_string(),
        };
        assert!(matches!(
            super::map_resolve_err("my-env")(not_found),
            AppError::NotFound(_)
        ));

        let conflict = CubeMasterError::Api {
            ret_code: 130409,
            ret_msg: "template name is already in use".to_string(),
        };
        assert!(matches!(
            super::map_resolve_err("my-env")(conflict),
            AppError::Conflict(_)
        ));

        let bad_request = CubeMasterError::Api {
            ret_code: 130400,
            ret_msg: "template name is invalid".to_string(),
        };
        assert!(matches!(
            super::map_resolve_err("my-env")(bad_request),
            AppError::BadRequest(_)
        ));
    }

    #[test]
    fn ensure_build_belongs_to_template_rejects_empty_build_template_id() {
        let err = super::ensure_build_belongs_to_template("", "tpl-1", "build-1").unwrap_err();
        assert!(matches!(err, AppError::NotFound(_)));
    }

    #[test]
    fn template_names_omits_empty_display_name() {
        assert_eq!(template_names("my-env"), vec!["my-env"]);
        assert!(template_names("").is_empty());
        assert!(template_names("  ").is_empty());
    }

    #[test]
    fn ensure_build_belongs_to_template_accepts_matching_ids() {
        ensure_build_belongs_to_template("tpl-1", "tpl-1", "job-1").expect("matching ids");
    }

    #[test]
    fn ensure_build_belongs_to_template_rejects_mismatch() {
        let err = ensure_build_belongs_to_template("tpl-other", "tpl-1", "job-1").unwrap_err();
        assert!(matches!(err, AppError::NotFound(_)));
    }

    #[tokio::test]
    async fn delete_template_by_display_name_uses_fresh_lookup() {
        use std::collections::HashMap;
        use std::sync::{Arc, Mutex};

        use axum::{
            extract::{Query, State},
            routing::{delete, get},
            Json, Router,
        };
        use serde_json::Value;

        #[derive(Clone, Default)]
        struct Capture {
            lookups: Arc<Mutex<Vec<(String, bool)>>>,
            delete_body: Arc<Mutex<Option<Value>>>,
        }

        async fn lookup_handler(
            Query(params): Query<HashMap<String, String>>,
            State(capture): State<Capture>,
        ) -> Json<Value> {
            let name = params.get("name").cloned().unwrap_or_default();
            let fresh = params.get("fresh").is_some_and(|v| v == "true" || v == "1");
            capture.lookups.lock().unwrap().push((name, fresh));
            Json(serde_json::json!({
                "ret": { "ret_code": 0, "ret_msg": "ok" },
                "template_id": "tpl-from-name"
            }))
        }

        async fn delete_handler(
            State(capture): State<Capture>,
            Json(body): Json<Value>,
        ) -> Json<Value> {
            *capture.delete_body.lock().unwrap() = Some(body);
            Json(serde_json::json!({
                "ret": { "ret_code": 0, "ret_msg": "ok" }
            }))
        }

        async fn spawn_server(app: Router) -> String {
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
                .await
                .expect("listener should bind");
            let addr = listener.local_addr().expect("listener addr");
            tokio::spawn(async move {
                axum::serve(listener, app).await.expect("server should run");
            });
            format!("http://{}", addr)
        }

        let capture = Capture::default();
        let cubemaster_url = spawn_server(
            Router::new()
                .route("/cube/template/lookup", get(lookup_handler))
                .route("/cube/template", delete(delete_handler))
                .with_state(capture.clone()),
        )
        .await;

        let cubemaster = CubeMasterClient::new(cubemaster_url, reqwest::Client::new());
        let service = TemplateService::new(
            cubemaster.clone(),
            "cubebox".to_string(),
            TemplateNameCache::new(cubemaster),
        );

        service
            .delete_template("my-env".to_string(), None, None)
            .await
            .expect("delete by display name should succeed");

        let lookups = capture.lookups.lock().unwrap().clone();
        assert_eq!(lookups.len(), 1);
        assert_eq!(lookups[0].0, "my-env");
        assert!(lookups[0].1, "mutating path should use fresh lookup");

        let delete_body = capture
            .delete_body
            .lock()
            .unwrap()
            .clone()
            .expect("delete body");
        assert_eq!(delete_body["template_id"], "tpl-from-name");
    }

    #[tokio::test]
    async fn delete_template_by_tpl_id_skips_lookup() {
        use std::sync::{Arc, Mutex};

        use axum::{
            extract::State,
            routing::{delete, get},
            Json, Router,
        };
        use serde_json::Value;

        #[derive(Clone, Default)]
        struct Capture {
            lookup_hits: Arc<Mutex<usize>>,
            delete_body: Arc<Mutex<Option<Value>>>,
        }

        async fn lookup_handler(State(capture): State<Capture>) -> Json<Value> {
            *capture.lookup_hits.lock().unwrap() += 1;
            Json(serde_json::json!({
                "ret": { "ret_code": 0, "ret_msg": "ok" },
                "template_id": "tpl-should-not-be-used"
            }))
        }

        async fn delete_handler(
            State(capture): State<Capture>,
            Json(body): Json<Value>,
        ) -> Json<Value> {
            *capture.delete_body.lock().unwrap() = Some(body);
            Json(serde_json::json!({
                "ret": { "ret_code": 0, "ret_msg": "ok" }
            }))
        }

        async fn spawn_server(app: Router) -> String {
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
                .await
                .expect("listener should bind");
            let addr = listener.local_addr().expect("listener addr");
            tokio::spawn(async move {
                axum::serve(listener, app).await.expect("server should run");
            });
            format!("http://{}", addr)
        }

        let capture = Capture::default();
        let cubemaster_url = spawn_server(
            Router::new()
                .route("/cube/template/lookup", get(lookup_handler))
                .route("/cube/template", delete(delete_handler))
                .with_state(capture.clone()),
        )
        .await;

        let cubemaster = CubeMasterClient::new(cubemaster_url, reqwest::Client::new());
        let service = TemplateService::new(
            cubemaster.clone(),
            "cubebox".to_string(),
            TemplateNameCache::new(cubemaster),
        );

        service
            .delete_template("tpl-direct".to_string(), None, None)
            .await
            .expect("delete by tpl id should succeed");

        assert_eq!(*capture.lookup_hits.lock().unwrap(), 0);
        let delete_body = capture
            .delete_body
            .lock()
            .unwrap()
            .clone()
            .expect("delete body");
        assert_eq!(delete_body["template_id"], "tpl-direct");
    }

    #[tokio::test]
    async fn update_template_name_by_display_name_uses_fresh_lookup_and_existing_fetch() {
        use std::collections::HashMap;
        use std::sync::{Arc, Mutex};

        use axum::{
            extract::{Query, State},
            routing::{get, post},
            Json, Router,
        };
        use serde_json::Value;

        #[derive(Clone, Default)]
        struct Capture {
            lookups: Arc<Mutex<Vec<(String, bool)>>>,
            display_name_body: Arc<Mutex<Option<Value>>>,
        }

        async fn lookup_handler(
            Query(params): Query<HashMap<String, String>>,
            State(capture): State<Capture>,
        ) -> Json<Value> {
            let name = params.get("name").cloned().unwrap_or_default();
            let fresh = params.get("fresh").is_some_and(|v| v == "true" || v == "1");
            capture.lookups.lock().unwrap().push((name, fresh));
            Json(serde_json::json!({
                "ret": { "ret_code": 0, "ret_msg": "ok" },
                "template_id": "tpl-rename"
            }))
        }

        async fn get_template_handler(
            Query(params): Query<HashMap<String, String>>,
        ) -> Json<Value> {
            assert_eq!(
                params.get("template_id").map(String::as_str),
                Some("tpl-rename")
            );
            Json(serde_json::json!({
                "ret": { "ret_code": 0, "ret_msg": "ok" },
                "template_id": "tpl-rename",
                "display_name": "old-name",
                "status": "ready"
            }))
        }

        async fn display_name_handler(
            State(capture): State<Capture>,
            Json(body): Json<Value>,
        ) -> Json<Value> {
            *capture.display_name_body.lock().unwrap() = Some(body);
            Json(serde_json::json!({
                "ret": { "ret_code": 0, "ret_msg": "ok" }
            }))
        }

        async fn spawn_server(app: Router) -> String {
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
                .await
                .expect("listener should bind");
            let addr = listener.local_addr().expect("listener addr");
            tokio::spawn(async move {
                axum::serve(listener, app).await.expect("server should run");
            });
            format!("http://{}", addr)
        }

        let capture = Capture::default();
        let cubemaster_url = spawn_server(
            Router::new()
                .route("/cube/template/lookup", get(lookup_handler))
                .route("/cube/template", get(get_template_handler))
                .route("/cube/template/display-name", post(display_name_handler))
                .with_state(capture.clone()),
        )
        .await;

        let cubemaster = CubeMasterClient::new(cubemaster_url, reqwest::Client::new());
        let service = TemplateService::new(
            cubemaster.clone(),
            "cubebox".to_string(),
            TemplateNameCache::new(cubemaster),
        );

        let detail = service
            .update_template_name(
                "old-name".to_string(),
                UpdateTemplateRequest {
                    name: "new-name".to_string(),
                },
            )
            .await
            .expect("rename by display name should succeed");

        let lookups = capture.lookups.lock().unwrap().clone();
        assert_eq!(lookups.len(), 1);
        assert_eq!(lookups[0].0, "old-name");
        assert!(lookups[0].1, "rename should use fresh lookup");
        assert_eq!(detail.template_id, "tpl-rename");
        assert_eq!(detail.names, vec!["new-name".to_string()]);

        let rename_body = capture
            .display_name_body
            .lock()
            .unwrap()
            .clone()
            .expect("display-name body");
        assert_eq!(rename_body["template_id"], "tpl-rename");
        assert_eq!(rename_body["display_name"], "new-name");
    }

    #[tokio::test]
    async fn resolve_template_ref_read_uses_cached_lookup() {
        use std::collections::HashMap;
        use std::sync::{Arc, Mutex};

        use axum::{
            extract::{Query, State},
            routing::get,
            Json, Router,
        };

        #[derive(Clone, Default)]
        struct Capture {
            lookups: Arc<Mutex<Vec<(String, bool)>>>,
        }

        async fn lookup_handler(
            Query(params): Query<HashMap<String, String>>,
            State(capture): State<Capture>,
        ) -> Json<serde_json::Value> {
            let name = params.get("name").cloned().unwrap_or_default();
            let fresh = params.get("fresh").is_some_and(|v| v == "true" || v == "1");
            capture.lookups.lock().unwrap().push((name, fresh));
            Json(serde_json::json!({
                "ret": { "ret_code": 0, "ret_msg": "ok" },
                "template_id": "tpl-read"
            }))
        }

        async fn spawn_server(app: Router) -> String {
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
                .await
                .expect("listener should bind");
            let addr = listener.local_addr().expect("listener addr");
            tokio::spawn(async move {
                axum::serve(listener, app).await.expect("server should run");
            });
            format!("http://{}", addr)
        }

        let capture = Capture::default();
        let cubemaster_url = spawn_server(
            Router::new()
                .route("/cube/template/lookup", get(lookup_handler))
                .with_state(capture.clone()),
        )
        .await;

        let cubemaster = CubeMasterClient::new(cubemaster_url, reqwest::Client::new());
        let cache = TemplateNameCache::new(cubemaster);

        let first = cache
            .resolve_template_ref("my-env")
            .await
            .expect("first read lookup");
        let second = cache
            .resolve_template_ref("my-env")
            .await
            .expect("second read lookup");

        assert_eq!(first, "tpl-read");
        assert_eq!(second, "tpl-read");
        let lookups = capture.lookups.lock().unwrap().clone();
        assert_eq!(lookups.len(), 2);
        assert!(lookups.iter().all(|(_, fresh)| !*fresh));
    }
}

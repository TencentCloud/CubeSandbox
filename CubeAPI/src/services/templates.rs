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
        TemplateNodeCompatView, TemplateSummary,
    },
};

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
            .map(|s| TemplateSummary {
                template_id: s.template_id,
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
            instance_type: non_empty(resp.instance_type),
            version: non_empty(resp.version),
            status: resp.status,
            last_error: non_empty(resp.last_error),
            replicas: resp.replicas,
            create_request: resp.create_request,
            network_type,
            allow_internet_access,
            job_id: non_empty(resp.job_id),
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
    if e.is_invalid_path_parameter() {
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

    fn sample_request() -> CreateTemplateRequest {
        CreateTemplateRequest {
            template_id: String::new(),
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
}

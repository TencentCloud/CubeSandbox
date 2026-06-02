// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::collections::HashMap;

use uuid::Uuid;

use crate::{
    config::ServerConfig,
    cubemaster::{
        CreateTemplateContainerOverrides, CreateTemplateCubeVSContext, CreateTemplateEnv,
        CreateTemplateFromImageReq, CreateTemplateResources, CubeMasterClient, CubeMasterError,
        DnsConfig, HttpGetAction, Probe, ProbeHandler, RedoTemplateReq, TemplateDeleteRequest,
        TemplateJob, TemplateJobResponse,
    },
    error::{AppError, AppResult},
    models::{
        CreateTemplateRequest, RebuildTemplateRequest, RegistryCredential, TemplateBuildJob,
        TemplateBuildStatus, TemplateDetail, TemplateSummary, V2TemplateBuildStart,
        V3BuildLogEntry, V3BuildStatusReason, V3TemplateBuildInfo, V3TemplateBuildRequest,
        V3TemplateBuildResponse,
    },
    services::builds::{BuildRegistry, BuildStage},
};

#[derive(Clone)]
pub struct TemplateService {
    cubemaster: CubeMasterClient,
    instance_type: String,
    builds: BuildRegistry,
    config: ServerConfig,
}

impl TemplateService {
    pub fn new(
        cubemaster: CubeMasterClient,
        instance_type: String,
        builds: BuildRegistry,
        config: ServerConfig,
    ) -> Self {
        Self {
            cubemaster,
            instance_type,
            builds,
            config,
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
            .and_then(|s| if s.is_empty() { None } else { Some(s.to_string()) });
        let allow_internet_access = resp
            .create_request
            .as_ref()
            .and_then(|v| v.get("cubevs_context"))
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
        })
    }

    /// Create a new template.
    ///
    /// Two paths converge here:
    ///
    ///  - **CubeSandbox-native** (`image` provided): immediately dispatches to
    ///    CubeMaster `POST /cube/template/from-image` and returns the resulting
    ///    job. No registry credential is issued.
    ///
    ///  - **E2B-standard** (`dockerfile` provided, or no `image`): allocates a
    ///    fresh `buildID`, returns the docker-push credential pointing at the
    ///    bundled OCI registry, and *does not* trigger CubeMaster yet — the
    ///    client must complete the docker push and then call
    ///    `POST /templates/{tid}/builds/{bid}` to dispatch the actual rootfs
    ///    build.
    pub async fn create_template(
        &self,
        body: CreateTemplateRequest,
    ) -> AppResult<TemplateBuildJob> {
        if body.dockerfile.is_some() || body.image.is_none() {
            return self.create_template_e2b_mode(body).await;
        }
        self.create_template_native_mode(body).await
    }

    /// Path 1: CubeSandbox-native — `image` field carries an existing OCI
    /// reference, dispatch directly.
    async fn create_template_native_mode(
        &self,
        body: CreateTemplateRequest,
    ) -> AppResult<TemplateBuildJob> {
        let image = body.image.clone().unwrap_or_default();
        if image.trim().is_empty() {
            return Err(AppError::BadRequest("image is required".to_string()));
        }
        // Validate DNS servers up-front so callers see a clear error before
        // we hand off to CubeMaster.
        validate_dns_servers(body.dns.as_deref())?;
        let req = self.build_cubemaster_request(&body, image.trim().to_string());
        let resp = self
            .cubemaster
            .create_template_from_image(&req)
            .await
            .map_err(map_err)?;
        Ok(to_job(resp, None))
    }

    /// Path 2: E2B-standard — allocate `(templateID, buildID)`, return docker
    /// push credentials. Actual build is dispatched by `start_template_build`.
    async fn create_template_e2b_mode(
        &self,
        body: CreateTemplateRequest,
    ) -> AppResult<TemplateBuildJob> {
        let upstream = self.config.registry_upstream.as_deref().unwrap_or("");
        if upstream.trim().is_empty() {
            return Err(AppError::NotImplemented(
                "registry upstream is not configured: set CUBE_API_REGISTRY_UPSTREAM \
                 to enable e2b-style template build (dockerfile push)"
                    .to_string(),
            ));
        }

        // Allocate / honour template id.
        let template_id = if body.template_id.trim().is_empty() {
            format!("tpl-{}", Uuid::new_v4().simple())
        } else {
            body.template_id.trim().to_string()
        };

        // Decide repo + tag and the public URL the CLI should push to.
        let repo_prefix = self.config.registry_repo_prefix.trim();
        let repo_prefix = if repo_prefix.is_empty() {
            "e2b"
        } else {
            repo_prefix
        };
        let public_host = self
            .config
            .registry_public_host
            .clone()
            .or_else(|| host_from_url(upstream))
            .unwrap_or_else(|| "localhost".to_string());

        let credential_url = if upstream.starts_with("https://") || upstream.starts_with("http://") {
            // strip path, keep scheme://host:port
            base_url(upstream).to_string()
        } else {
            format!("https://{}", public_host)
        };

        let credential = RegistryCredential {
            url: credential_url,
            repository: format!("{}/{}", repo_prefix, template_id),
            username: "_token".to_string(),
            password: self
                .config
                .registry_token
                .clone()
                .unwrap_or_else(|| "_anon".to_string()),
        };

        // Image ref CubeMaster will pull from once push is complete.
        let pull_host = self
            .config
            .registry_pull_host
            .clone()
            .or_else(|| host_from_url(upstream))
            .unwrap_or_else(|| public_host.clone());
        let image_ref_template = format!("{}/{}/{}", pull_host, repo_prefix, template_id);

        // Reserve the build context up-front; the buildID becomes the docker tag.
        let ctx = self.builds.create(
            template_id.clone(),
            body,
            credential.clone(),
            image_ref_template.clone(),
        );
        let image_ref_full = format!("{}:{}", image_ref_template, ctx.build_id);

        // Patch the stored ref to include the buildID-as-tag now that we know it.
        self.builds.update(&ctx.build_id, |c| {
            c.image_ref = image_ref_full.clone();
        });
        self.builds.append_log(
            &ctx.build_id,
            format!(
                "[register] templateID={} buildID={} repo={}",
                template_id, ctx.build_id, credential.repository
            ),
        );

        Ok(TemplateBuildJob {
            job_id: ctx.build_id.clone(),
            template_id: template_id.clone(),
            build_id: ctx.build_id.clone(),
            status: "accepted".to_string(),
            phase: "waiting".to_string(),
            progress: 0,
            error_message: String::new(),
            upload_url: Some(credential.url.clone()),
            registry: Some(credential),
        })
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
        Ok(to_job(resp, None))
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

    /// Dispatch the CubeMaster pipeline for a previously-registered E2B
    /// build. Falls back to a plain `redoTemplate` for builds that were not
    /// registered through `create_template_e2b_mode` (e.g. CLI invokes
    /// `start_template_build` directly on a CubeSandbox-native template).
    pub async fn start_template_build(
        &self,
        template_id: String,
        build_id: Option<String>,
    ) -> AppResult<TemplateBuildJob> {
        if let Some(bid) = build_id.as_deref() {
            if let Some(ctx) = self.builds.get_by_pair(&template_id, bid) {
                self.builds.append_log(
                    bid,
                    format!("[dispatch] image_ref={}", ctx.image_ref),
                );

                let req = self
                    .build_cubemaster_request(&ctx.create_request, ctx.image_ref.clone());
                let resp = self
                    .cubemaster
                    .create_template_from_image(&req)
                    .await
                    .map_err(map_err)?;

                let job = resp.job.clone().unwrap_or_else(default_template_job);
                let job_id = job.job_id.clone();
                self.builds.update(bid, |c| {
                    c.job_id = job_id.clone();
                    c.stage = BuildStage::Building;
                    c.message = "build dispatched to cubemaster".to_string();
                });

                return Ok(to_job(resp, Some(bid.to_string())));
            }
        }

        // Fallback for legacy `redo` semantics.
        let req = RedoTemplateReq {
            request_id: new_request_id(),
            template_id,
            extra: Default::default(),
        };
        let resp = self.cubemaster.redo_template(&req).await.map_err(map_err)?;
        Ok(to_job(resp, build_id))
    }

    pub async fn get_template_build_status(
        &self,
        template_id: &str,
        build_id: &str,
        logs_offset: i32,
    ) -> AppResult<TemplateBuildStatus> {
        // E2B mode: serve from the in-memory build registry, falling back to
        // CubeMaster for the canonical job state when it's been dispatched.
        if let Some(ctx) = self.builds.get_by_pair(template_id, build_id) {
            let mut status = ctx.stage.as_str().to_string();
            let mut progress = ctx.progress;
            let mut message = ctx.message.clone();

            if !ctx.job_id.is_empty() {
                if let Ok(remote) = self
                    .cubemaster
                    .get_template_build_status(&ctx.job_id)
                    .await
                {
                    status = remap_cubemaster_status(&remote.status);
                    progress = remote.progress;
                    message = remote.message.clone();

                    // Persist progress / terminal state into the local registry.
                    let new_stage = match status.as_str() {
                        "ready" => BuildStage::Ready,
                        "error" => BuildStage::Error,
                        _ => BuildStage::Building,
                    };
                    self.builds.update(build_id, |c| {
                        c.stage = new_stage;
                        c.progress = progress;
                        c.message = message.clone();
                    });

                    if !remote.message.is_empty() {
                        self.builds.append_log(
                            build_id,
                            format!("[{}] {}", remote.status, remote.message),
                        );
                    }
                }
            }

            // Slice logs starting at the requested offset.
            let total = ctx.logs.len() as i32;
            let offset = logs_offset.max(0).min(total);
            let lines: Vec<String> = self
                .builds
                .get(build_id)
                .map(|c| {
                    c.logs
                        .iter()
                        .skip(offset as usize)
                        .map(|l| format!("{} {}", l.timestamp.to_rfc3339(), l.line))
                        .collect()
                })
                .unwrap_or_default();
            let next_offset = offset + lines.len() as i32;

            return Ok(TemplateBuildStatus {
                build_id: build_id.to_string(),
                template_id: template_id.to_string(),
                status,
                progress,
                message,
                logs: lines,
                logs_offset: Some(next_offset),
            });
        }

        // Legacy native mode: forward to CubeMaster directly (no log buffer).
        let resp = self
            .cubemaster
            .get_template_build_status(build_id)
            .await
            .map_err(map_err)?;

        Ok(TemplateBuildStatus {
            build_id: string_or(resp.build_id, build_id),
            template_id: string_or(resp.template_id, template_id),
            status: remap_cubemaster_status(&resp.status),
            progress: resp.progress,
            message: resp.message,
            logs: Vec::new(),
            logs_offset: None,
        })
    }

    pub async fn get_template_build_logs(
        &self,
        template_id: &str,
        build_id: &str,
        offset: i32,
    ) -> AppResult<serde_json::Value> {
        let status = self
            .get_template_build_status(template_id, build_id, offset)
            .await?;

        Ok(serde_json::json!({
            "buildID": status.build_id,
            "templateID": status.template_id,
            "status": status.status,
            "progress": status.progress,
            "logs": status.logs,
            "logsOffset": status.logs_offset,
        }))
    }

    /// Mark a build as image-pushed (called by the registry handler once the
    /// manifest PUT for `repo:tag` succeeds). Idempotent.
    pub fn mark_image_pushed(&self, build_id: &str) {
        self.builds.update(build_id, |ctx| {
            ctx.append_log_inline("[push] image upload complete");
            if matches!(ctx.stage, BuildStage::WaitingPush) {
                ctx.stage = BuildStage::Building;
                ctx.message = "image uploaded, waiting for build dispatch".to_string();
            }
        });
    }

    /// Build a CubeMaster create-from-image request from the user's intent
    /// (used by both create paths so behaviour stays in lockstep).
    fn build_cubemaster_request(
        &self,
        body: &CreateTemplateRequest,
        image_ref: String,
    ) -> CreateTemplateFromImageReq {
        let probe = build_probe(body);
        let resources = build_resources(body);
        let envs = merge_envs(body);
        let command = non_empty_vec(body.command.clone());
        let args = non_empty_vec(body.args.clone());
        // We've already validated DNS servers up the call stack; here we just
        // canonicalise and drop empties.
        let dns_servers: Option<Vec<String>> = body.dns.as_ref().and_then(|servers| {
            let cleaned: Vec<String> = servers
                .iter()
                .map(|s| s.trim().to_string())
                .filter(|s| !s.is_empty())
                .collect();
            if cleaned.is_empty() {
                None
            } else {
                Some(cleaned)
            }
        });
        let dns_config = dns_servers.map(|servers| DnsConfig {
            servers,
            searches: Vec::new(),
        });

        let container_overrides = if probe.is_some()
            || resources.is_some()
            || envs.is_some()
            || command.is_some()
            || args.is_some()
            || dns_config.is_some()
        {
            Some(CreateTemplateContainerOverrides {
                command,
                args,
                probe,
                resources,
                envs,
                dns_config,
            })
        } else {
            None
        };

        let allow_out = body.allow_out.clone().unwrap_or_default();
        let deny_out = body.deny_out.clone().unwrap_or_default();
        let cubevs_context = if body.allow_internet_access.is_some()
            || !allow_out.is_empty()
            || !deny_out.is_empty()
        {
            Some(CreateTemplateCubeVSContext {
                allow_internet_access: body.allow_internet_access,
                allow_out,
                deny_out,
            })
        } else {
            None
        };

        CreateTemplateFromImageReq {
            request_id: new_request_id(),
            instance_type: body
                .instance_type
                .clone()
                .unwrap_or_else(|| self.instance_type.clone()),
            template_id: body.template_id.clone(),
            source_image_ref: image_ref,
            // CubeMaster validates `writable_layer_size` as required; fall back
            // to the configured default (env CUBE_API_DEFAULT_WRITABLE_LAYER_SIZE,
            // "1G" by default) when the caller hasn't specified one. The E2B V3
            // SDK never sends this field, so without a default the build would
            // fail with "writable_layer_size is required".
            writable_layer_size: body
                .writable_layer_size
                .clone()
                .filter(|s| !s.trim().is_empty())
                .or_else(|| Some(self.config.default_writable_layer_size.clone()))
                .filter(|s| !s.trim().is_empty()),
            exposed_ports: body.exposed_ports.clone(),
            network_type: non_empty_option(body.network_type.clone()),
            registry_username: non_empty_option(body.registry_username.clone()),
            registry_password: non_empty_option(body.registry_password.clone()),
            distribution_scope: non_empty_vec(body.nodes.clone()),
            container_overrides,
            cubevs_context,
        }
    }

    // ── V3 protocol (real e2b SDK contract) ────────────────────────────────

    /// `POST /v3/templates` — register a template + build attempt.
    ///
    /// Returns the V3 envelope shape the SDK strictly expects. We allocate
    /// `(templateID, buildID)` deterministically from `name` so subsequent
    /// builds against the same name reuse the same templateID (matching E2B's
    /// "alias is also a primary key" semantics).
    pub fn v3_create_template(
        &self,
        body: V3TemplateBuildRequest,
    ) -> AppResult<V3TemplateBuildResponse> {
        // Resolve final name + tag list (the SDK packs "name:tag" or relies on
        // the explicit `tags` array).
        let raw_name = body
            .name
            .clone()
            .or(body.alias.clone())
            .filter(|s| !s.trim().is_empty())
            .ok_or_else(|| AppError::BadRequest("template name is required".to_string()))?;
        let (name_part, name_tag) = match raw_name.split_once(':') {
            Some((n, t)) if !t.is_empty() => (n.to_string(), Some(t.to_string())),
            _ => (raw_name.clone(), None),
        };
        let mut tags = body.tags.clone().unwrap_or_default();
        if let Some(t) = name_tag.clone() {
            if !tags.contains(&t) {
                tags.insert(0, t);
            }
        }

        let template_id = stable_template_id(&name_part);

        // Build the legacy request shell so the V2 trigger step has uniform
        // metadata regardless of whether create-time fields are sparse.
        let create_req = CreateTemplateRequest {
            template_id: template_id.clone(),
            instance_type: None,
            alias: Some(name_part.clone()),
            team_id: body.team_id.clone(),
            image: None,
            dockerfile: None,
            writable_layer_size: None,
            exposed_ports: None,
            probe_port: None,
            probe_path: None,
            cpu: None,
            memory: None,
            cpu_count: body.cpu_count,
            memory_mb: body.memory_mb,
            env: None,
            env_vars: None,
            allow_internet_access: None,
            network_type: None,
            nodes: None,
            registry_username: None,
            registry_password: None,
            command: None,
            args: None,
            dns: None,
            allow_out: None,
            deny_out: None,
            start_cmd: None,
            ready_cmd: None,
        };

        // Reserve a build context. Registry credential is attached for the
        // benefit of the OCI-push flow (`/v2/...` reverse proxy); SDK V3 won't
        // actually use it — it ships a tarball through `/templates/.../files/`.
        let credential = self.issue_registry_credential(&template_id);
        let pull_host = self
            .config
            .registry_pull_host
            .clone()
            .or_else(|| self.config.registry_upstream.as_deref().and_then(host_from_url))
            .unwrap_or_else(|| {
                self.config
                    .registry_public_host
                    .clone()
                    .unwrap_or_else(|| "localhost".to_string())
            });
        let repo_prefix = if self.config.registry_repo_prefix.trim().is_empty() {
            "e2b"
        } else {
            self.config.registry_repo_prefix.trim()
        };
        let image_ref_template = format!("{}/{}/{}", pull_host, repo_prefix, template_id);

        let ctx = self.builds.create(
            template_id.clone(),
            create_req,
            credential,
            image_ref_template.clone(),
        );

        let build_id = ctx.build_id.clone();
        let cpu_count = body.cpu_count.unwrap_or(2);
        let memory_mb = body.memory_mb.unwrap_or(1024);
        self.builds.update(&build_id, |c| {
            c.image_ref = format!("{}:{}", image_ref_template, build_id);
            c.name = name_part.clone();
            c.tags = tags.clone();
            c.cpu_count = cpu_count;
            c.memory_mb = memory_mb;
            c.aliases = vec![name_part.clone()];
            c.message = "template registered, awaiting build trigger".to_string();
        });
        self.builds.append_log(
            &build_id,
            format!(
                "[register-v3] templateID={} buildID={} name={} cpu={} memMB={}",
                template_id, build_id, name_part, cpu_count, memory_mb
            ),
        );

        Ok(V3TemplateBuildResponse {
            template_id,
            build_id,
            names: vec![name_part.clone()],
            aliases: vec![name_part],
            tags,
            public: false,
        })
    }

    /// `GET /templates/{tid}/files/{hash}` — file-cache probe.
    ///
    /// Until the in-cluster builder lands we don't actually consume uploaded
    /// tarballs. We answer `present=true` so the SDK skips uploading; this is
    /// safe because `from_image`-based builds (the only flow CubeMaster
    /// currently supports) don't need the build context.
    pub fn v3_get_file_upload(&self, _template_id: &str, _files_hash: &str) -> AppResult<crate::models::V3TemplateFileUpload> {
        Ok(crate::models::V3TemplateFileUpload {
            present: true,
            url: None,
        })
    }

    /// `POST /v2/templates/{tid}/builds/{bid}` — the real "start build" call.
    ///
    /// At this point CubeMaster needs an OCI image reference. We resolve one
    /// in this priority order:
    ///
    ///   1. `body.from_image`  — the standard E2B flow, e.g.
    ///                            `python:3.11-slim`.
    ///   2. The image already pushed to the bundled registry under
    ///      `<repo>/<templateID>:<buildID>` (when the OCI Distribution path
    ///      was used).
    ///   3. `body.from_template` — copy from another known CubeSandbox
    ///      template (resolved via CubeMaster `get_template`).
    ///
    /// `start_cmd` becomes container `args`; `ready_cmd` becomes a Probe.Exec.
    pub async fn v3_trigger_build(
        &self,
        template_id: String,
        build_id: String,
        body: V2TemplateBuildStart,
    ) -> AppResult<()> {
        let ctx = self
            .builds
            .get_by_pair(&template_id, &build_id)
            .ok_or_else(|| {
                AppError::NotFound(format!(
                    "build {} of template {} is unknown — call POST /v3/templates first",
                    build_id, template_id
                ))
            })?;

        // Resolve the source image.
        let source_image = if let Some(img) = body
            .from_image
            .as_ref()
            .map(|s| s.trim().to_string())
            .filter(|s| !s.is_empty())
        {
            img
        } else if let Some(parent) = body
            .from_template
            .as_ref()
            .map(|s| s.trim().to_string())
            .filter(|s| !s.is_empty())
        {
            // Re-use an already-built CubeSandbox template as the base. We
            // synthesise a CubeMaster reference of the form `cube://<tid>`,
            // letting downstream callers resolve it. Adjust to your local
            // convention if needed.
            format!("cube://{}", parent)
        } else if !ctx.image_ref.is_empty() {
            ctx.image_ref.clone()
        } else {
            return Err(AppError::BadRequest(
                "either fromImage, fromTemplate, or a previously-pushed image is required"
                    .to_string(),
            ));
        };

        // Patch the cached create_request with the V2-time fields and dispatch.
        let mut create_req: CreateTemplateRequest = (*ctx.create_request).clone();
        if create_req.start_cmd.is_none() {
            create_req.start_cmd = body.start_cmd.clone();
        }
        if create_req.ready_cmd.is_none() {
            create_req.ready_cmd = body.ready_cmd.clone();
        }

        self.builds.append_log(
            &build_id,
            format!(
                "[dispatch-v3] from_image={} start_cmd={:?} ready_cmd={:?} steps={}",
                source_image,
                body.start_cmd.as_deref().unwrap_or(""),
                body.ready_cmd.as_deref().unwrap_or(""),
                body.steps.as_ref().map(|s| s.len()).unwrap_or(0),
            ),
        );

        // Cubelet/CubeMaster only support TcpSocket | Ping | HttpGet probes,
        // so the E2B `readyCmd` (a shell snippet) cannot be forwarded
        // verbatim. To still honour the SDK's `wait_for_url(...)` semantics
        // we attempt a best-effort parse of the well-known
        // `http://<host>:<port>/<path>` form embedded in the readyCmd. When
        // that succeeds (and the caller did not already pin probe_port/path
        // via the v3 body), we synthesise an HttpGet probe so cubelet's
        // `doProbe` blocks until the user process is actually listening
        // before sandbox creation returns.
        let ready_cmd = body
            .ready_cmd
            .as_deref()
            .map(str::trim)
            .filter(|s| !s.is_empty());
        if let Some(cmd) = ready_cmd {
            match parse_ready_url(cmd) {
                Some((port, path)) if create_req.probe_port.is_none() => {
                    create_req.probe_port = Some(port);
                    if create_req.probe_path.is_none() {
                        create_req.probe_path = Some(path.clone());
                    }
                    self.builds.append_log(
                        &build_id,
                        format!(
                            "[dispatch-v3] readyCmd parsed → HttpGet probe on \
                             port={} path={} (probe blocks sandbox creation \
                             until ready)",
                            port, path
                        ),
                    );
                }
                Some(_) => {
                    // probe_port already set by caller — keep their override
                    // but make the precedence explicit in the build log.
                    self.builds.append_log(
                        &build_id,
                        "[dispatch-v3] readyCmd parsed but probePort was \
                         supplied explicitly — keeping caller's probePort \
                         and ignoring the URL inside readyCmd",
                    );
                }
                None
                    if create_req.probe_port.is_none()
                        && create_req
                            .exposed_ports
                            .as_ref()
                            .map(|p| p.is_empty())
                            .unwrap_or(true) =>
                {
                    self.builds.append_log(
                        &build_id,
                        "[dispatch-v3] note: readyCmd is recorded but could \
                         not be parsed into an HttpGet probe (only \
                         `http://host:port/path` URLs are recognised); \
                         supply `probePort` (or build with `exposedPorts`) \
                         to enable readiness checks",
                    );
                }
                None => {
                    // Caller already supplied probe_port or exposed_ports;
                    // build_probe() will pick those up on its own.
                }
            }
        }

        let req = self.build_cubemaster_request(&create_req, source_image.clone());
        let resp = self
            .cubemaster
            .create_template_from_image(&req)
            .await
            .map_err(map_err)?;

        let job = resp.job.unwrap_or_else(default_template_job);
        let job_id = job.job_id.clone();
        self.builds.update(&build_id, |c| {
            c.job_id = job_id.clone();
            c.stage = BuildStage::Building;
            c.message = "build dispatched to cubemaster".to_string();
        });

        Ok(())
    }

    /// `GET /templates/{tid}/builds/{bid}/status` — V3 status envelope.
    pub async fn v3_get_build_status(
        &self,
        template_id: &str,
        build_id: &str,
        logs_offset: i32,
        limit: i32,
    ) -> AppResult<V3TemplateBuildInfo> {
        // Reuse the existing get_template_build_status (which already knows
        // how to refresh against CubeMaster), then convert into the V3 shape.
        let internal = self
            .get_template_build_status(template_id, build_id, logs_offset)
            .await?;

        let limit = if limit <= 0 { 100 } else { limit as usize };
        let logs: Vec<String> = internal
            .logs
            .iter()
            .take(limit)
            .cloned()
            .collect();
        let log_entries: Vec<V3BuildLogEntry> = logs
            .iter()
            .map(|line| V3BuildLogEntry {
                timestamp: chrono::Utc::now(),
                message: line.clone(),
                level: "info".to_string(),
            })
            .collect();

        let status = match internal.status.as_str() {
            "ready" => "ready",
            "error" => "error",
            "waiting" | "pending" => "waiting",
            _ => "building",
        }
        .to_string();

        let reason = if status == "error" {
            Some(V3BuildStatusReason {
                step_index: None,
                message: if internal.message.is_empty() {
                    "build failed".to_string()
                } else {
                    internal.message.clone()
                },
            })
        } else {
            None
        };

        Ok(V3TemplateBuildInfo {
            build_id: internal.build_id,
            template_id: internal.template_id,
            status,
            logs,
            log_entries,
            reason,
        })
    }

    fn issue_registry_credential(&self, template_id: &str) -> RegistryCredential {
        let upstream = self.config.registry_upstream.as_deref().unwrap_or("");
        let url = if upstream.starts_with("http://") || upstream.starts_with("https://") {
            base_url(upstream)
        } else if let Some(host) = self.config.registry_public_host.clone() {
            format!("https://{}", host)
        } else {
            "http://localhost".to_string()
        };
        let repo_prefix = if self.config.registry_repo_prefix.trim().is_empty() {
            "e2b"
        } else {
            self.config.registry_repo_prefix.trim()
        };
        RegistryCredential {
            url,
            repository: format!("{}/{}", repo_prefix, template_id),
            username: "_token".to_string(),
            password: self
                .config
                .registry_token
                .clone()
                .unwrap_or_else(|| "_anon".to_string()),
        }
    }
}

// ─── helpers ───────────────────────────────────────────────────────────────

/// Build the CubeMaster `Probe` from the user's intent.
///
/// **Important — limitations imposed by the downstream stack**:
///
///   - Cubelet (`Cubelet/services/cubebox/check.go::checkProbe`) only accepts
///     `TcpSocket | Ping | HttpGet` handlers. Anything else is rejected with
///     `invalid probe.probe_handler  param`.
///   - CubeMaster's `handleProbeHandler` (in `pkg/service/sandbox/util.go`)
///     similarly has no Exec branch — passing one yields an empty handler
///     object, which Cubelet then rejects.
///
/// As a result the E2B-style `readyCmd` (a shell snippet) **cannot** be
/// translated into a CubeMaster probe. We only synthesise a probe when the
/// caller (or template store) gives us an explicit port. `readyCmd` is
/// recorded into the build log for diagnostic purposes (see
/// `v3_trigger_build`) but never forwarded to CubeMaster as a probe.
fn build_probe(body: &CreateTemplateRequest) -> Option<Probe> {
    let port = body
        .probe_port
        .or_else(|| body.exposed_ports.as_ref().and_then(|p| p.first().copied()))?;

    Some(Probe {
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
        timeout_ms: Some(30_000),
        period_ms: Some(500),
        success_threshold: Some(1),
        failure_threshold: Some(60),
    })
}

/// Best-effort parser for the SDK's `wait_for_url(...)` ready command.
///
/// The E2B SDK ultimately sends the ready check as a free-form shell snippet
/// in `readyCmd`, e.g.
///
///   * `wait_for_url("http://localhost:49999/health")`
///   * `curl -fsS http://127.0.0.1:8080/ready`
///   * `until curl -fsS http://0.0.0.0:3000; do sleep 1; done`
///
/// Any of these collapses to "HTTP GET on `<port><path>` of the sandbox" once
/// you discard the surrounding shell. We extract `(port, path)` from the
/// first `http(s)://<host>:<port>[/path]` substring whose host is one of the
/// localhost aliases so we never accidentally point the probe at an
/// off-VM service.
///
/// Returns `None` when no recognisable URL is present — callers fall back to
/// `probe_port` / `exposedPorts` or skip the probe entirely.
fn parse_ready_url(ready_cmd: &str) -> Option<(u16, String)> {
    // Iterate over each `http(s)://` occurrence; the first parseable one
    // wins. We bound the scanning at 64 to keep this cheap.
    let mut search = ready_cmd;
    for _ in 0..64 {
        let scheme_idx = search.find("http")?;
        let after_http = &search[scheme_idx..];
        let rest = after_http
            .strip_prefix("https://")
            .or_else(|| after_http.strip_prefix("http://"));
        let rest = match rest {
            Some(r) => r,
            None => {
                // Found "http" but not as a scheme — advance one char and
                // try again.
                let next = scheme_idx + 1;
                if next >= search.len() {
                    return None;
                }
                search = &search[next..];
                continue;
            }
        };

        // `rest` now points at `<host>[:<port>][/path...][?query]...` followed
        // by whatever shell tokens come next (space, `"`, `'`, `)`, `;`, ...).
        let end = rest
            .find(|c: char| {
                matches!(
                    c,
                    ' ' | '\t' | '\n' | '"' | '\'' | ')' | ';' | '|' | '&' | '`' | '<' | '>'
                )
            })
            .unwrap_or(rest.len());
        let url_body = &rest[..end];

        // Split host[:port] / path[?query]
        let (authority, path_with_query) = match url_body.find('/') {
            Some(i) => (&url_body[..i], &url_body[i..]),
            None => (url_body, ""),
        };

        // Drop ?query — probes don't carry it.
        let path = match path_with_query.find('?') {
            Some(i) => &path_with_query[..i],
            None => path_with_query,
        };

        // Authority must contain an explicit port and resolve to a localhost
        // alias — otherwise we refuse to invent a probe target.
        let (host, port_str) = authority.rsplit_once(':')?;
        if !is_localhost_alias(host) {
            return None;
        }
        let port: u16 = port_str.parse().ok()?;
        if port == 0 {
            return None;
        }

        let path = if path.is_empty() {
            "/".to_string()
        } else {
            path.to_string()
        };
        return Some((port, path));
    }
    None
}

/// `wait_for_url` only makes sense when pointed at the sandbox itself, so we
/// limit the host whitelist to the well-known loopback aliases. Anything else
/// is almost certainly a misconfiguration we'd rather surface than silently
/// translate into a probe.
fn is_localhost_alias(host: &str) -> bool {
    matches!(
        host,
        "localhost" | "127.0.0.1" | "0.0.0.0" | "::1" | "[::1]"
    )
}

fn build_resources(body: &CreateTemplateRequest) -> Option<CreateTemplateResources> {
    // E2B `cpuCount` (cores) → `cpu * 1000` millicores; legacy `cpu` already
    // in millicores wins when both are set.
    let cpu_millicores = body.cpu.or_else(|| body.cpu_count.map(|n| n * 1000));
    let mem_mb = body.memory.or(body.memory_mb);

    if cpu_millicores.is_none() && mem_mb.is_none() {
        return None;
    }

    Some(CreateTemplateResources {
        cpu: cpu_millicores.map(|v| format!("{}m", v)),
        mem: mem_mb.map(|v| format!("{}Mi", v)),
    })
}

fn merge_envs(body: &CreateTemplateRequest) -> Option<Vec<CreateTemplateEnv>> {
    let mut out: HashMap<String, String> = HashMap::new();

    if let Some(envs) = &body.env {
        for s in envs {
            let mut parts = s.splitn(2, '=');
            if let Some(k) = parts.next() {
                let k = k.trim().to_string();
                if k.is_empty() {
                    continue;
                }
                let v = parts.next().unwrap_or("").to_string();
                out.insert(k, v);
            }
        }
    }
    if let Some(map) = &body.env_vars {
        for (k, v) in map {
            out.insert(k.clone(), v.clone());
        }
    }

    if out.is_empty() {
        None
    } else {
        Some(
            out.into_iter()
                .map(|(key, value)| CreateTemplateEnv { key, value })
                .collect(),
        )
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

fn to_job(resp: TemplateJobResponse, build_id_override: Option<String>) -> TemplateBuildJob {
    let job = resp.job.unwrap_or_else(default_template_job);
    let build_id = build_id_override
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| job.job_id.clone());
    TemplateBuildJob {
        job_id: job.job_id,
        template_id: job.template_id,
        build_id,
        status: job.status,
        phase: job.phase,
        progress: job.progress,
        error_message: job.error_message,
        upload_url: None,
        registry: None,
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

/// Translate CubeMaster-internal phase strings into E2B-style status tokens.
fn remap_cubemaster_status(raw: &str) -> String {
    match raw.trim().to_lowercase().as_str() {
        "" => "pending".to_string(),
        "ready" | "succeeded" | "success" | "completed" | "complete" => "ready".to_string(),
        "failed" | "error" | "errored" => "error".to_string(),
        // CubeMaster intermediate phases — bucket all of them into "building"
        // to match what the E2B CLI expects.
        "pending" | "queued" | "running" | "pulling" | "extracting" | "rootfs"
        | "snapshotting" | "distributing" | "uploading" | "ready_pending" => "building".to_string(),
        other => other.to_string(),
    }
}

fn host_from_url(url: &str) -> Option<String> {
    // Best-effort URL parse without pulling in a new crate.
    let after_scheme = url
        .splitn(2, "://")
        .nth(1)
        .or_else(|| Some(url))
        .unwrap_or(url);
    let host = after_scheme.split('/').next().unwrap_or("");
    if host.is_empty() {
        None
    } else {
        Some(host.to_string())
    }
}

/// Hash `name` into a stable templateID so repeated `Template.build()` calls
/// against the same name reuse the same ID. We use the first 12 hex chars of
/// a v5 UUID derived from the DNS namespace + name.
fn stable_template_id(name: &str) -> String {
    let ns = uuid::Uuid::NAMESPACE_DNS;
    let id = uuid::Uuid::new_v5(&ns, name.as_bytes());
    let simple = id.simple().to_string();
    format!("tpl-{}", &simple[..16])
}

fn base_url(url: &str) -> String {
    if let Some(rest) = url.strip_prefix("http://") {
        let host = rest.split('/').next().unwrap_or("");
        format!("http://{}", host)
    } else if let Some(rest) = url.strip_prefix("https://") {
        let host = rest.split('/').next().unwrap_or("");
        format!("https://{}", host)
    } else {
        url.to_string()
    }
}

// Adapter helper used inside dashmap update closures.
impl crate::services::builds::BuildContext {
    pub(crate) fn append_log_inline(&mut self, line: impl Into<String>) {
        self.logs.push(crate::services::builds::BuildLogLine {
            timestamp: chrono::Utc::now(),
            line: line.into(),
        });
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::models::CreateTemplateRequest;

    fn empty_request() -> CreateTemplateRequest {
        CreateTemplateRequest {
            template_id: String::new(),
            instance_type: None,
            alias: None,
            team_id: None,
            image: None,
            dockerfile: None,
            writable_layer_size: None,
            exposed_ports: None,
            probe_port: None,
            probe_path: None,
            cpu: None,
            memory: None,
            cpu_count: None,
            memory_mb: None,
            env: None,
            env_vars: None,
            allow_internet_access: None,
            network_type: None,
            nodes: None,
            registry_username: None,
            registry_password: None,
            command: None,
            args: None,
            dns: None,
            allow_out: None,
            deny_out: None,
            start_cmd: None,
            ready_cmd: None,
        }
    }

    #[allow(dead_code)]
    fn sample_request() -> CreateTemplateRequest {
        CreateTemplateRequest {
            template_id: String::new(),
            instance_type: Some("cubebox".to_string()),
            alias: None,
            team_id: None,
            image: Some("python:3.11-slim".to_string()),
            dockerfile: None,
            writable_layer_size: Some("1G".to_string()),
            exposed_ports: Some(vec![8080]),
            probe_port: Some(8080),
            probe_path: Some("/health".to_string()),
            cpu: Some(2000),
            memory: Some(2048),
            cpu_count: None,
            memory_mb: None,
            env: Some(vec!["A=1".to_string()]),
            env_vars: None,
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
            start_cmd: None,
            ready_cmd: None,
        }
    }

    #[test]
    fn validate_dns_servers_rejects_invalid_ip() {
        let err = validate_dns_servers(Some(&["not-an-ip".to_string()])).unwrap_err();
        assert!(matches!(err, AppError::BadRequest(_)));
    }

    #[test]
    fn build_resources_maps_e2b_cpu_count_to_millicores() {
        let mut req = empty_request();
        req.cpu_count = Some(2);
        req.memory_mb = Some(4096);
        let r = build_resources(&req).expect("resources should be present");
        assert_eq!(r.cpu.as_deref(), Some("2000m"));
        assert_eq!(r.mem.as_deref(), Some("4096Mi"));
    }

    #[test]
    fn build_resources_prefers_legacy_fields_when_both_supplied() {
        let mut req = empty_request();
        req.cpu = Some(500); // millicores
        req.cpu_count = Some(8);
        req.memory = Some(512);
        req.memory_mb = Some(8192);
        let r = build_resources(&req).expect("resources should be present");
        assert_eq!(r.cpu.as_deref(), Some("500m"));
        assert_eq!(r.mem.as_deref(), Some("512Mi"));
    }

    #[test]
    fn merge_envs_overrides_kv_strings_with_envvars_map() {
        let mut req = empty_request();
        req.env = Some(vec!["FOO=bar".to_string(), "EMPTY=".to_string()]);
        req.env_vars = Some({
            let mut m = HashMap::new();
            m.insert("FOO".to_string(), "baz".to_string()); // wins
            m.insert("EXTRA".to_string(), "yes".to_string());
            m
        });
        let mut envs = merge_envs(&req).expect("envs should be present");
        envs.sort_by(|a, b| a.key.cmp(&b.key));
        assert_eq!(envs.len(), 3);
        let foo = envs.iter().find(|e| e.key == "FOO").unwrap();
        assert_eq!(foo.value, "baz");
    }

    #[test]
    fn build_probe_picks_http_get_when_port_provided() {
        let mut req = empty_request();
        req.probe_port = Some(8080);
        req.probe_path = Some("/healthz".to_string());
        let probe = build_probe(&req).expect("probe should be present");
        assert!(probe.probe_handler.http_get.is_some());
        let http = probe.probe_handler.http_get.unwrap();
        assert_eq!(http.port, 8080);
        assert_eq!(http.path, "/healthz");
    }

    /// Regression: previously we synthesised an Exec probe from
    /// `readyCmd`, but neither CubeMaster nor Cubelet support Exec probes
    /// (`invalid probe.probe_handler  param`). The fix is to **not** emit a
    /// probe at all when the caller hasn't provided a port — Cubelet treats
    /// nil probes as "no readiness check", which is the right thing to do.
    #[test]
    fn build_probe_returns_none_when_only_ready_cmd_is_provided() {
        let mut req = empty_request();
        req.ready_cmd = Some("curl -fsS localhost:1234/ok".to_string());
        // No probe_port, no exposed_ports → no probe.
        assert!(build_probe(&req).is_none());
    }

    /// Regression: when the caller provides exposedPorts but no explicit
    /// probe_port, we still want an HttpGet probe on the first exposed port
    /// (matches our previous behaviour and keeps templates that listed ports
    /// working out of the box).
    #[test]
    fn build_probe_picks_http_get_from_first_exposed_port() {
        let mut req = empty_request();
        req.exposed_ports = Some(vec![3000, 8080]);
        let probe = build_probe(&req).expect("probe should be present");
        let http = probe.probe_handler.http_get.expect("http probe");
        assert_eq!(http.port, 3000);
        assert!(probe.probe_handler.exec.is_none());
    }

    #[test]
    fn parse_ready_url_extracts_port_and_path_from_localhost_url() {
        assert_eq!(
            parse_ready_url("wait_for_url(\"http://localhost:49999/health\")"),
            Some((49999, "/health".to_string()))
        );
    }

    #[test]
    fn parse_ready_url_handles_curl_with_127_0_0_1_and_query_string() {
        assert_eq!(
            parse_ready_url("curl -fsS http://127.0.0.1:8080/ready?retries=3 || exit 1"),
            Some((8080, "/ready".to_string()))
        );
    }

    #[test]
    fn parse_ready_url_defaults_path_to_root_when_omitted() {
        assert_eq!(
            parse_ready_url("until nc -z 0.0.0.0:3000; do sleep 0.2; done; \
                             curl http://0.0.0.0:3000"),
            Some((3000, "/".to_string()))
        );
    }

    #[test]
    fn parse_ready_url_rejects_non_loopback_hosts() {
        // We must not silently rewrite a probe to point at an external
        // service — that would generate noisy traffic and probably never
        // succeed against the sandbox itself.
        assert_eq!(
            parse_ready_url("curl http://api.example.com:443/healthz"),
            None
        );
    }

    #[test]
    fn parse_ready_url_returns_none_when_no_url_is_present() {
        assert_eq!(parse_ready_url("/usr/local/bin/wait-for-it.sh --quiet"), None);
        assert_eq!(parse_ready_url(""), None);
        assert_eq!(parse_ready_url("curl localhost:1234"), None); // missing http://
    }

    #[test]
    fn parse_ready_url_requires_explicit_port() {
        // Probes must target a specific port — defaulting to 80/443 here
        // would mask real misconfigurations.
        assert_eq!(parse_ready_url("curl http://localhost/health"), None);
    }

    #[test]
    fn parse_ready_url_rejects_zero_port() {
        assert_eq!(parse_ready_url("curl http://127.0.0.1:0/"), None);
    }

    #[test]
    fn host_from_url_extracts_host_with_port() {
        assert_eq!(host_from_url("http://10.0.0.1:5000"), Some("10.0.0.1:5000".to_string()));
        assert_eq!(
            host_from_url("https://registry.example.com/path"),
            Some("registry.example.com".to_string())
        );
    }

    #[test]
    fn base_url_strips_path_keeps_scheme() {
        assert_eq!(base_url("http://10.0.0.1:5000/v2/"), "http://10.0.0.1:5000");
        assert_eq!(
            base_url("https://reg.example.com/foo/bar"),
            "https://reg.example.com"
        );
    }

    #[test]
    fn remap_cubemaster_status_normalizes_phases_to_e2b_tokens() {
        assert_eq!(remap_cubemaster_status(""), "pending");
        assert_eq!(remap_cubemaster_status("Ready"), "ready");
        assert_eq!(remap_cubemaster_status("succeeded"), "ready");
        assert_eq!(remap_cubemaster_status("Failed"), "error");
        assert_eq!(remap_cubemaster_status("PULLING"), "building");
        assert_eq!(remap_cubemaster_status("distributing"), "building");
        assert_eq!(remap_cubemaster_status("custom_phase"), "custom_phase");
    }

    fn make_service(registry_upstream: Option<String>) -> TemplateService {
        let mut cfg = ServerConfig::default();
        cfg.registry_upstream = registry_upstream;
        cfg.registry_public_host = Some("cube.example.com".to_string());
        cfg.registry_repo_prefix = "e2b".to_string();
        let http = reqwest::Client::new();
        let cm = CubeMasterClient::new("http://127.0.0.1:9", http);
        TemplateService::new(cm, "cubebox".to_string(), BuildRegistry::new(), cfg)
    }

    #[tokio::test]
    async fn create_template_e2b_mode_rejects_when_registry_disabled() {
        let svc = make_service(None);
        let mut req = empty_request();
        req.dockerfile = Some("FROM ubuntu".to_string());
        let err = svc.create_template(req).await.expect_err("should fail");
        assert!(matches!(err, AppError::NotImplemented(_)));
    }

    #[tokio::test]
    async fn create_template_e2b_mode_returns_push_credential_and_registers_build() {
        let svc = make_service(Some("http://127.0.0.1:5000".to_string()));
        let mut req = empty_request();
        req.dockerfile = Some("FROM ubuntu\nCMD echo hi".to_string());
        let job = svc
            .create_template(req)
            .await
            .expect("e2b create should succeed");

        // Build identity is well-formed and emitted in both legacy & E2B fields.
        assert!(!job.template_id.is_empty());
        assert!(job.template_id.starts_with("tpl-"));
        assert!(job.build_id.starts_with("bld-"));
        assert_eq!(job.status, "accepted");
        assert_eq!(job.phase, "waiting");

        // Push credential points at the configured public host.
        let cred = job.registry.expect("registry credential");
        assert_eq!(cred.url, "http://127.0.0.1:5000");
        assert!(cred.repository.starts_with("e2b/tpl-"));
        assert_eq!(cred.username, "_token");

        // Internal BuildRegistry now knows about this build and stores the
        // image_ref CubeMaster will later pull from.
        let ctx = svc
            .builds
            .get(&job.build_id)
            .expect("build context should be registered");
        assert!(ctx.image_ref.starts_with("127.0.0.1:5000/e2b/"));
        assert!(ctx.image_ref.ends_with(&format!(":{}", job.build_id)));
    }

    /// Regression: CubeMaster validates `writable_layer_size` as required and
    /// the E2B V3 SDK never sends it. Verify the service injects the
    /// configured default so the request reaches CubeMaster non-empty.
    #[test]
    fn build_cubemaster_request_fills_default_writable_layer_size() {
        let svc = make_service(None);
        let req = empty_request();
        let cm_req = svc.build_cubemaster_request(&req, "image:tag".to_string());
        assert_eq!(cm_req.writable_layer_size.as_deref(), Some("1G"));
    }

    #[test]
    fn build_cubemaster_request_preserves_caller_writable_layer_size() {
        let svc = make_service(None);
        let mut req = empty_request();
        req.writable_layer_size = Some("4G".to_string());
        let cm_req = svc.build_cubemaster_request(&req, "image:tag".to_string());
        assert_eq!(cm_req.writable_layer_size.as_deref(), Some("4G"));
    }
}


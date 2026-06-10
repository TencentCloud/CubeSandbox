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

        let credential = mint_registry_credential(
            credential_url,
            format!("{}/{}", repo_prefix, template_id),
        );

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
    /// Advance a build from `WaitingPush` → `Building` after the registry
    /// reverse-proxy observed a successful manifest PUT.
    ///
    /// **Defence in depth**: while build IDs are 128-bit UUIDs and therefore
    /// hard to guess, we still cross-check that the manifest's repository
    /// path matches the one we minted at create time. This stops a leaked
    /// (or copy-pasted) build_id from being advanced by a manifest pushed
    /// against an unrelated repo, and surfaces config drift in the registry
    /// path-prefix as a warning rather than a silent state transition.
    ///
    /// `repo` is the path between `/v2/` and `/manifests/<tag>`, e.g.
    /// `e2b/tpl-abc123` for `PUT /v2/e2b/tpl-abc123/manifests/bld-...`.
    pub fn mark_image_pushed(&self, build_id: &str, repo: &str) {
        let Some(ctx) = self.builds.get(build_id) else {
            tracing::debug!(
                build_id = %build_id,
                repo = %repo,
                "manifest PUT received for unknown build_id; ignoring"
            );
            return;
        };

        if !manifest_repo_matches(&ctx.image_ref, repo) {
            tracing::warn!(
                build_id = %build_id,
                got_repo = %repo,
                expected_image_ref = %ctx.image_ref,
                "manifest PUT repo does not match the image_ref \
                 minted for this build; refusing to advance build state"
            );
            return;
        }

        self.builds.update(build_id, |ctx| {
            ctx.append_log_inline("[push] image upload complete");
            // `image_pushed` is the single, authoritative signal that a
            // manifest landed under our predicted `image_ref`. It survives
            // any subsequent stage mutation (e.g. by status pollers) and
            // is what `v3_trigger_build`'s OCI-distribution fallback
            // gates on — *not* `stage`, which is an indirect proxy.
            ctx.image_pushed = true;
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
    /// ## Contract (paired with `v3_trigger_build`)
    ///
    /// The E2B SDK calls this endpoint to ask "do you already have the
    /// build-context tarball identified by `<hash>`?". A `present=true`
    /// answer makes the SDK *skip* uploading the tarball, on the assumption
    /// that the server-side builder will read it from cache. CubeAPI does
    /// **not** currently run an in-cluster Dockerfile/steps builder, so
    /// strictly speaking we don't have any tarball cache at all.
    ///
    /// We still answer `present=true` here for two reasons:
    ///
    ///   1. The SDK calls this endpoint *unconditionally* before every
    ///      build, including pure `fromImage` flows that don't need a
    ///      tarball at all. Returning `present=false` would force the SDK
    ///      to PUT a (typically empty) tarball to a URL we don't have
    ///      anywhere to put.
    ///   2. We compensate by enforcing a strict fail-fast in
    ///      `v3_trigger_build`: if the dispatch body doesn't carry a
    ///      `fromImage` / `fromTemplate` / pre-pushed registry image, we
    ///      reject with `501 Not Implemented` and a message that points the
    ///      caller back to the supported flows. That means a `dockerfile`
    ///      / `steps`-driven build can never silently succeed against a
    ///      non-existent tarball — it just fails one round-trip later than
    ///      it would in upstream e2b-infra.
    ///
    /// **A `present=true` reply from this endpoint is therefore not a
    /// promise that we accepted a tarball.** It is exactly the
    /// "no-op, please proceed" hint the SDK needs to advance to the
    /// `POST /v2/.../builds/{bid}` step where the real validation lives.
    ///
    /// Until the in-cluster builder lands (Phase 4) the warning emitted
    /// here gives operators an observability hook for "someone is trying
    /// to use a context-based build against a CubeAPI that can't honour
    /// it" without having to read trigger-time logs.
    ///
    /// The handler returns `201 Created` (not `200 OK`) on purpose — see the
    /// doc comment on `handlers::templates_v3::v3_get_files_hash` for the
    /// E2B SDK compatibility rationale.
    pub fn v3_get_file_upload(
        &self,
        template_id: &str,
        files_hash: &str,
    ) -> AppResult<crate::models::V3TemplateFileUpload> {
        // Cheap heuristic: an SDK invoking the empty-context flow (pure
        // `fromImage`) typically still hashes *something*, so we can't tell
        // dockerfile vs. fromImage apart purely from `files_hash`. Emit a
        // single warn so operators can grep for it; trigger-time fail-fast
        // is the authoritative gate.
        tracing::warn!(
            template_id = %template_id,
            files_hash = %files_hash,
            "files-hash cache probe answered present=true unconditionally; \
             CubeAPI does not run an in-cluster context builder. \
             Dockerfile-/steps-based builds will be rejected at \
             POST /v2/templates/{{tid}}/builds/{{bid}} with 501. \
             Use `fromImage` (or `docker push` via the bundled OCI registry) \
             to drive the build."
        );

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
    ///      `<repo>/<templateID>:<buildID>` — only used when
    ///      `BuildContext::image_pushed` is `true`, i.e. the registry
    ///      reverse proxy has observed a successful manifest PUT and
    ///      `mark_image_pushed` cross-checked the repo. We deliberately
    ///      do **not** key off `stage` or "image_ref is non-empty"
    ///      here: `image_ref` is *predicted* at create time and would
    ///      otherwise let us dispatch CubeMaster against a registry
    ///      slot that holds nothing, with the failure surfacing later
    ///      as `manifest unknown` during pull. When `image_ref` is
    ///      non-empty but `image_pushed` is still `false`, we surface
    ///      that mismatch as **`409 Conflict`** so the SDK can retry
    ///      after `docker push` completes.
    ///   3. `body.from_template` — **rejected with 501** until a
    ///      downstream resolver for `cube://<templateID>` exists in
    ///      CubeMaster/Cubelet. Today CubeMaster feeds `SourceImageRef`
    ///      straight into `docker pull`, so a synthesised `cube://...`
    ///      ref would silently break image resolution; we fail fast at
    ///      the API layer instead. Callers who want this flow should
    ///      resolve the parent template themselves and pass the resulting
    ///      OCI reference through `from_image`.
    ///
    /// `start_cmd` becomes container `args`; `ready_cmd` is *not* forwarded
    /// as an exec probe — CubeMaster/Cubelet only accept TcpSocket / Ping /
    /// HttpGet handlers, so we instead best-effort parse an embedded
    /// `http(s)://host:port[/path]` URL out of the readyCmd and synthesise
    /// a `Probe.HttpGet`. If no URL can be parsed and no `probePort` /
    /// `exposedPorts` are supplied, no probe is emitted at all — see
    /// `parse_ready_url` and `build_probe` for the precise rules.
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
            // `fromTemplate` is **not yet wired end-to-end**: CubeMaster's
            // template_image.go feeds `SourceImageRef` straight into
            // `docker pull` / `docker image inspect`, and there is no
            // resolver for a `cube://<tid>` scheme anywhere downstream
            // (Cubelet, CubeMaster, builder). If we synthesised a
            // `cube://<parent>` ref here, the build would *look* accepted
            // at the API layer and only fail several seconds later inside
            // the build worker with an opaque `docker pull cube://...:
            // invalid reference format` error — exactly the kind of
            // "looks supported but isn't" footgun reviewers flagged.
            //
            // Until the downstream resolver lands (tracked separately),
            // surface the gap explicitly. Operators who actually want this
            // flow today can resolve `parent` themselves and pass the
            // resulting OCI ref via `fromImage`.
            self.builds.append_log(
                &build_id,
                format!(
                    "[dispatch-v3] rejecting build: fromTemplate={} is not \
                     supported by this deployment — no downstream resolver \
                     for `cube://<templateID>` exists in CubeMaster/Cubelet \
                     yet. Resolve the parent template to an OCI image \
                     reference and pass it via `fromImage` instead.",
                    parent,
                ),
            );
            return Err(AppError::NotImplemented(format!(
                "build {} of template {} requested `fromTemplate={}`, but \
                 CubeAPI cannot honour it: the downstream stack \
                 (CubeMaster/Cubelet) does not yet understand the \
                 `cube://<templateID>` source scheme and would attempt to \
                 `docker pull` it verbatim. Pass `fromImage` with the \
                 already-resolved OCI reference of the parent template, \
                 or wait for the cube:// resolver to ship.",
                build_id, template_id, parent,
            )));
        } else if ctx.image_pushed {
            // OCI Distribution path: the caller has actually completed an
            // OCI manifest PUT against `image_ref` — `mark_image_pushed`
            // verified the repo and flipped `image_pushed` to true. We
            // can safely dispatch CubeMaster against the predicted ref
            // because we *know* a manifest now lives under it.
            //
            // Note we deliberately do NOT key off `stage != WaitingPush`
            // here. `stage` is mutated by status pollers and the v2
            // dispatch path for unrelated reasons; using it as a proxy
            // for "client pushed" would re-open the very gap the
            // reviewer flagged: dispatching against `ctx.image_ref` even
            // though it's just the *predicted* path minted at create
            // time, with no manifest behind it. The CubeMaster pull
            // would then fail several seconds later with `manifest
            // unknown` — exactly the kind of late-stage error this guard
            // is meant to prevent.
            debug_assert!(
                !ctx.image_ref.is_empty(),
                "image_pushed=true must imply non-empty image_ref"
            );
            ctx.image_ref.clone()
        } else {
            // Distinguish three remaining failure modes so the error
            // message tells the operator *exactly* what to do.
            let has_steps = body
                .steps
                .as_ref()
                .map(|s| !s.is_empty())
                .unwrap_or(false);
            if has_steps {
                // (1) `steps[]` build with no fromImage — needs an
                //     in-cluster context builder we don't run.
                self.builds.append_log(
                    &build_id,
                    "[dispatch-v3] rejecting build: steps[] supplied but \
                     CubeAPI has no in-cluster context builder; supply \
                     fromImage or push a pre-built image to the bundled \
                     OCI registry instead",
                );
                return Err(AppError::NotImplemented(format!(
                    "dockerfile-/steps-based builds are not supported by \
                     this CubeAPI deployment (build {} of template {} \
                     supplied {} step(s) without a fromImage). Either set \
                     `fromImage` to a base OCI reference, or `docker push` \
                     a pre-built image to the bundled registry under \
                     `<repo_prefix>/<templateID>:<buildID>` before calling \
                     this endpoint.",
                    build_id,
                    template_id,
                    body.steps.as_ref().map(|s| s.len()).unwrap_or(0),
                )));
            }
            if !ctx.image_ref.is_empty() {
                // (2) `image_ref` is non-empty (it was predicted at
                //     create time) but the manifest never landed — the
                //     SDK skipped (or hasn't yet completed) `docker
                //     push`. Reviewer-driven guard: do NOT silently
                //     dispatch CubeMaster against an empty registry
                //     slot. Surface the mismatch *before* CubeMaster
                //     starts pulling, with an actionable hint.
                self.builds.append_log(
                    &build_id,
                    format!(
                        "[dispatch-v3] rejecting build: predicted image_ref \
                         {} exists but no successful manifest PUT has been \
                         observed by the registry reverse proxy yet \
                         (image_pushed=false). Dispatching now would only \
                         move the failure into CubeMaster's pull stage as \
                         `manifest unknown`.",
                        ctx.image_ref,
                    ),
                );
                return Err(AppError::Conflict(format!(
                    "build {} of template {} has not received the \
                     OCI manifest PUT yet: the registry reverse proxy \
                     has not observed a successful `PUT \
                     /v2/<repo>/manifests/{}` against `image_ref={}`. \
                     Note: the SDK's `GET /templates/{{tid}}/files/{{hash}}` \
                     cache probe always returns `present=true` and is \
                     *not* a commitment that any image was accepted — \
                     see `v3_get_file_upload` for the contract. Either \
                     wait for `docker push` to complete and retry, or \
                     supply `fromImage` to bypass the bundled registry \
                     path.",
                    build_id, template_id, build_id, ctx.image_ref,
                )));
            }
            // (3) Neither steps nor fromImage nor any push — the SDK
            //     probably believed the build context was already cached
            //     server-side (because `/files/{hash}` answered
            //     present=true); see `v3_get_file_upload` for the
            //     contract. We surface a 501 here so the failure mode
            //     is unambiguous.
            self.builds.append_log(
                &build_id,
                "[dispatch-v3] rejecting build: no fromImage / fromTemplate \
                 and no image was pushed to the bundled registry before \
                 dispatch; CubeAPI cannot synthesise a source image from a \
                 build-context tarball alone",
            );
            return Err(AppError::NotImplemented(format!(
                "build {} of template {} cannot be dispatched: this \
                 CubeAPI deployment does not run an in-cluster build-context \
                 builder, so a `fromImage` (or pre-pushed registry image, \
                 or `fromTemplate`) is required. The SDK's \
                 `GET /templates/{{tid}}/files/{{hash}}` cache probe \
                 returns `present=true` unconditionally and is *not* a \
                 commitment that any tarball was accepted — see the \
                 server-side docs on `v3_get_file_upload` for the contract.",
                build_id, template_id,
            )));
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

        // Reviewer-flagged bug: previously `log_entries` stamped each line
        // with `Utc::now()` at poll time, so the *same* historical line
        // would receive a fresh timestamp on every status poll — making
        // `logEntries[i].timestamp` jitter forwards in time even though
        // the line itself never changed.
        //
        // The structured timestamps already exist on
        // `BuildContext.logs[i].timestamp` (`BuildLogLine`) and were
        // stamped at log-write time by `BuildRegistry::append_log`. We
        // reach into the registry to pull those write-time timestamps
        // back out, taking care to:
        //
        //   - apply the *same* `(logs_offset, limit)` window that
        //     `get_template_build_status` used to produce
        //     `internal.logs`, so the i-th entry of `logs` lines up
        //     with the i-th entry of `log_entries`;
        //   - clamp the entry count to `logs.len()` so we never emit
        //     more `log_entries` than `logs` even if a concurrent
        //     poll appended new lines between the two reads.
        //
        // The narrow corner case where the build context has been
        // evicted between the `get_template_build_status` call above
        // and this read (e.g. terminal-state eviction firing during
        // an in-flight poll on the same build) falls through to a
        // best-effort `created_at`-style fallback — the historical
        // bug used `Utc::now()` there too, so behaviour is no worse
        // than before, and we still preserve the
        // `logs.len() == log_entries.len()` invariant the SDK relies
        // on.
        let log_entries: Vec<V3BuildLogEntry> = match self.builds.get(build_id) {
            Some(ctx) => {
                let total = ctx.logs.len();
                let start = (logs_offset.max(0) as usize).min(total);
                ctx.logs
                    .iter()
                    .skip(start)
                    .take(logs.len())
                    .map(|entry| V3BuildLogEntry {
                        timestamp: entry.timestamp,
                        message: entry.line.clone(),
                        level: "info".to_string(),
                    })
                    .collect()
            }
            None => {
                tracing::debug!(
                    template_id = %template_id,
                    build_id = %build_id,
                    "build context vanished between status poll and \
                     log-entry materialisation; falling back to \
                     poll-time timestamps for V3 logEntries"
                );
                logs.iter()
                    .map(|line| V3BuildLogEntry {
                        timestamp: chrono::Utc::now(),
                        message: line.clone(),
                        level: "info".to_string(),
                    })
                    .collect()
            }
        };
        debug_assert_eq!(
            logs.len(),
            log_entries.len(),
            "V3 logs and logEntries must be aligned 1:1"
        );

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
        // Per-build short-lived credential — see `mint_registry_credential`
        // and the matching comment in `create_template_e2b_mode` for the
        // rationale (username is the routing key into `username_index`,
        // password is verified by the registry reverse-proxy).
        mint_registry_credential(url, format!("{}/{}", repo_prefix, template_id))
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

fn mint_registry_credential(url: String, repository: String) -> RegistryCredential {
    use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine as _};
    let mut buf = [0u8; 32];
    buf[..16].copy_from_slice(Uuid::new_v4().as_bytes());
    buf[16..].copy_from_slice(Uuid::new_v4().as_bytes());
    let token = URL_SAFE_NO_PAD.encode(buf);
    RegistryCredential {
        url,
        repository,
        username: format!("bld_{}", &token[..22]),
        password: token,
    }
}

fn manifest_repo_matches(image_ref: &str, repo: &str) -> bool {
    let Some(expected) = image_ref_repo(image_ref) else {
        return false;
    };
    expected == repo
}

/// Extract the `repo` segment from an `image_ref` of the form
/// `<host>[:port]/<repo>:<tag>`. Returns `None` when the host or tag is
/// missing, or when the repo would be empty.
fn image_ref_repo(image_ref: &str) -> Option<String> {
    let without_tag = match (image_ref.rfind(':'), image_ref.rfind('/')) {
        (Some(colon), Some(slash)) if colon > slash => &image_ref[..colon],
        _ => image_ref,
    };
    let slash = without_tag.find('/')?;
    let repo = &without_tag[slash + 1..];
    if repo.is_empty() {
        None
    } else {
        Some(repo.to_string())
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
    fn image_ref_repo_extracts_repo_with_host_port_and_tag() {
        assert_eq!(
            image_ref_repo("127.0.0.1:5000/e2b/tpl-abc:bld-123").as_deref(),
            Some("e2b/tpl-abc")
        );
        assert_eq!(
            image_ref_repo("registry.example.com/team/tpl-xyz").as_deref(),
            Some("team/tpl-xyz")
        );
        assert_eq!(
            image_ref_repo("reg.local:443/x/y/z:latest").as_deref(),
            Some("x/y/z")
        );
    }

    #[test]
    fn image_ref_repo_returns_none_for_malformed_input() {
        assert_eq!(image_ref_repo("only-host").as_deref(), None);
        assert_eq!(image_ref_repo("host.example.com/").as_deref(), None);
        assert_eq!(image_ref_repo("host:5000/:tag").as_deref(), None);
    }

    #[test]
    fn manifest_repo_matches_accepts_canonical_image_ref() {
        assert!(manifest_repo_matches(
            "127.0.0.1:5000/e2b/tpl-abc:bld-123",
            "e2b/tpl-abc"
        ));
    }

    #[test]
    fn manifest_repo_matches_rejects_mismatched_repo() {
        assert!(!manifest_repo_matches(
            "127.0.0.1:5000/e2b/tpl-abc:bld-123",
            "attacker/tpl-abc"
        ));
        assert!(!manifest_repo_matches(
            "127.0.0.1:5000/e2b/tpl-abc:bld-123",
            "e2b/tpl-other"
        ));
    }

    #[test]
    fn manifest_repo_matches_rejects_malformed_image_ref() {
        assert!(!manifest_repo_matches("e2b/tpl-abc:bld-123", "e2b/tpl-abc"));
    }

    #[test]
    fn mark_image_pushed_advances_stage_when_repo_matches() {
        let svc = make_service(Some("http://127.0.0.1:5000".to_string()));
        let cred = RegistryCredential {
            url: "http://127.0.0.1:5000".to_string(),
            repository: "e2b/tpl-abc".to_string(),
            username: "_token".to_string(),
            password: "secret".to_string(),
        };
        let ctx = svc.builds.create(
            "tpl-abc".to_string(),
            empty_request(),
            cred,
            "127.0.0.1:5000/e2b/tpl-abc:bld-deadbeef".to_string(),
        );
        svc.builds.update(&ctx.build_id, |c| {
            c.image_ref = format!("127.0.0.1:5000/e2b/tpl-abc:{}", c.build_id);
        });

        svc.mark_image_pushed(&ctx.build_id, "e2b/tpl-abc");

        let after = svc.builds.get(&ctx.build_id).expect("ctx");
        assert_eq!(after.stage, BuildStage::Building);
        assert!(
            after.image_pushed,
            "mark_image_pushed must flip image_pushed=true on success — \
             this is the authoritative signal v3_trigger_build's \
             OCI fallback gates on"
        );
    }

    #[test]
    fn mark_image_pushed_refuses_when_repo_does_not_match() {
        let svc = make_service(Some("http://127.0.0.1:5000".to_string()));
        let cred = RegistryCredential {
            url: "http://127.0.0.1:5000".to_string(),
            repository: "e2b/tpl-abc".to_string(),
            username: "_token".to_string(),
            password: "secret".to_string(),
        };
        let ctx = svc.builds.create(
            "tpl-abc".to_string(),
            empty_request(),
            cred,
            "127.0.0.1:5000/e2b/tpl-abc:bld-deadbeef".to_string(),
        );
        svc.builds.update(&ctx.build_id, |c| {
            c.image_ref = format!("127.0.0.1:5000/e2b/tpl-abc:{}", c.build_id);
        });

        svc.mark_image_pushed(&ctx.build_id, "attacker/tpl-abc");

        let after = svc.builds.get(&ctx.build_id).expect("ctx");
        assert_eq!(
            after.stage,
            BuildStage::WaitingPush,
            "stage must not advance when repo mismatches"
        );
        assert!(
            !after.image_pushed,
            "image_pushed must stay false when the repo cross-check \
             fails — otherwise v3_trigger_build would later dispatch \
             against an unverified slot"
        );
    }

    #[test]
    fn mark_image_pushed_is_noop_for_unknown_build_id() {
        let svc = make_service(Some("http://127.0.0.1:5000".to_string()));
        svc.mark_image_pushed("bld-does-not-exist", "e2b/tpl-abc");
        assert!(svc.builds.get("bld-does-not-exist").is_none());
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
        // Per-build short-lived credential: username is `bld_<…>` (i.e. NOT
        // the legacy global `_token`), and password is a high-entropy
        // random string that the registry reverse-proxy validates against
        // the in-memory BuildRegistry on every push request. See
        // `mint_registry_credential` for the rationale.
        assert!(
            cred.username.starts_with("bld_"),
            "expected per-build username (bld_<…>), got {:?}",
            cred.username
        );
        assert!(
            cred.password.len() >= 32,
            "expected high-entropy random password, got {} chars",
            cred.password.len()
        );
        assert_ne!(
            cred.username, "_token",
            "the legacy shared `_token` username must not regress — \
             it would defeat per-build credential validation"
        );
        // Issuing a second build must produce a different credential pair
        // (i.e. RNG is wired up properly and we're not handing every build
        // the same secret).
        let mut req2 = empty_request();
        req2.dockerfile = Some("FROM ubuntu".to_string());
        let job2 = svc
            .create_template(req2)
            .await
            .expect("second e2b create should succeed");
        let cred2 = job2.registry.expect("second registry credential");
        assert_ne!(cred.username, cred2.username);
        assert_ne!(cred.password, cred2.password);

        // Internal BuildRegistry now knows about this build and stores the
        // image_ref CubeMaster will later pull from.
        let ctx = svc
            .builds
            .get(&job.build_id)
            .expect("build context should be registered");
        assert!(ctx.image_ref.starts_with("127.0.0.1:5000/e2b/"));
        assert!(ctx.image_ref.ends_with(&format!(":{}", job.build_id)));
    }

    #[tokio::test]
    async fn v3_trigger_build_rejects_steps_without_from_image_with_501() {
        let svc = make_service(Some("http://127.0.0.1:5000".to_string()));
        let mut req = empty_request();
        req.dockerfile = Some("FROM ubuntu".to_string());
        let job = svc
            .create_template(req)
            .await
            .expect("e2b create should succeed");

        let body = V2TemplateBuildStart {
            steps: Some(vec![serde_json::json!({"type": "RUN", "args": ["echo hi"]})]),
            ..Default::default()
        };
        let err = svc
            .v3_trigger_build(job.template_id.clone(), job.build_id.clone(), body)
            .await
            .expect_err("steps-only build must be rejected, not dispatched");

        match err {
            AppError::NotImplemented(msg) => {
                assert!(
                    msg.contains("dockerfile-/steps-based builds are not supported"),
                    "unexpected NotImplemented message: {msg}"
                );
                assert!(msg.contains(&job.build_id));
            }
            other => panic!("expected NotImplemented, got {other:?}"),
        }

        let ctx = svc
            .builds
            .get(&job.build_id)
            .expect("build context preserved on failure");
        assert_eq!(ctx.stage, BuildStage::WaitingPush);
    }

    #[tokio::test]
    async fn v3_trigger_build_does_not_use_unpushed_image_ref() {
        let svc = make_service(Some("http://127.0.0.1:5000".to_string()));
        let mut req = empty_request();
        req.dockerfile = Some("FROM ubuntu".to_string());
        let job = svc
            .create_template(req)
            .await
            .expect("e2b create should succeed");

        let ctx = svc
            .builds
            .get(&job.build_id)
            .expect("build context exists");
        assert!(!ctx.image_pushed, "fresh build must not be marked pushed");
        assert!(
            !ctx.image_ref.is_empty(),
            "image_ref is predicted at create time and should already \
             be populated — exactly the trap this guard prevents"
        );

        let body = V2TemplateBuildStart::default();
        let err = svc
            .v3_trigger_build(job.template_id.clone(), job.build_id.clone(), body)
            .await
            .expect_err("unpushed builds must not be dispatched against the predicted ref");

        match err {
            AppError::Conflict(msg) => {
                assert!(
                    msg.contains("manifest PUT"),
                    "error must name the missing operation: {msg}"
                );
                assert!(
                    msg.contains(&job.build_id),
                    "error must include the build_id: {msg}"
                );
                assert!(
                    msg.contains("fromImage"),
                    "error must point operators at the fromImage \
                     workaround: {msg}"
                );
            }
            other => panic!("expected Conflict, got {other:?}"),
        }

        let ctx = svc
            .builds
            .get(&job.build_id)
            .expect("build context preserved on failure");
        assert!(!ctx.image_pushed);
    }

    #[tokio::test]
    async fn v3_trigger_build_uses_image_ref_after_mark_image_pushed_flips_flag() {
        let svc = make_service(Some("http://127.0.0.1:5000".to_string()));
        let mut req = empty_request();
        req.dockerfile = Some("FROM ubuntu".to_string());
        let job = svc
            .create_template(req)
            .await
            .expect("e2b create should succeed");

        svc.builds.update(&job.build_id, |c| {
            c.image_ref = format!("127.0.0.1:5000/e2b/tpl-abc:{}", c.build_id);
        });
        svc.mark_image_pushed(&job.build_id, "e2b/tpl-abc");
        let ctx = svc.builds.get(&job.build_id).expect("ctx exists");
        assert!(
            ctx.image_pushed,
            "mark_image_pushed must flip image_pushed=true"
        );

        let body = V2TemplateBuildStart::default();
        let err = svc
            .v3_trigger_build(job.template_id.clone(), job.build_id.clone(), body)
            .await
            .expect_err(
                "cubemaster is unreachable in unit tests, so dispatch \
                 will fail at transport — but the source-resolution \
                 branch must already have been satisfied",
            );

        assert!(
            !matches!(err, AppError::Conflict(_)),
            "image_pushed=true must defuse the 409 guard: {err:?}"
        );
        assert!(
            !matches!(err, AppError::NotImplemented(_)),
            "image_pushed=true must defuse the 501 source-resolution \
             guard: {err:?}"
        );
    }

    #[tokio::test]
    async fn v3_get_build_status_preserves_log_write_timestamps_across_polls() {
        let svc = make_service(Some("http://127.0.0.1:5000".to_string()));
        let mut req = empty_request();
        req.dockerfile = Some("FROM ubuntu".to_string());
        let job = svc
            .create_template(req)
            .await
            .expect("e2b create should succeed");

        let baseline_len = svc
            .builds
            .get(&job.build_id)
            .expect("ctx exists")
            .logs
            .len();

        svc.builds.append_log(&job.build_id, "first line");
        svc.builds.append_log(&job.build_id, "second line");
        svc.builds.append_log(&job.build_id, "third line");

        let expected_ts: Vec<_> = svc
            .builds
            .get(&job.build_id)
            .expect("ctx exists")
            .logs
            .iter()
            .map(|l| l.timestamp)
            .collect();
        let expected_total = baseline_len + 3;
        assert_eq!(
            expected_ts.len(),
            expected_total,
            "test setup must seed exactly three additional log lines"
        );

        let first = svc
            .v3_get_build_status(&job.template_id, &job.build_id, 0, 1000)
            .await
            .expect("first status poll should succeed");
        assert_eq!(first.log_entries.len(), expected_total);
        assert_eq!(first.logs.len(), first.log_entries.len());
        for (i, entry) in first.log_entries.iter().enumerate() {
            assert_eq!(
                entry.timestamp, expected_ts[i],
                "logEntries[{i}].timestamp must match the write-time \
                 BuildLogLine.timestamp, not Utc::now() at poll time"
            );
        }
        assert_eq!(first.log_entries[baseline_len].message, "first line");
        assert_eq!(first.log_entries[baseline_len + 1].message, "second line");
        assert_eq!(first.log_entries[baseline_len + 2].message, "third line");

        tokio::time::sleep(std::time::Duration::from_millis(5)).await;

        let second = svc
            .v3_get_build_status(&job.template_id, &job.build_id, 0, 1000)
            .await
            .expect("second status poll should succeed");
        assert_eq!(second.log_entries.len(), expected_total);
        for (i, entry) in second.log_entries.iter().enumerate() {
            assert_eq!(
                entry.timestamp, first.log_entries[i].timestamp,
                "logEntries[{i}].timestamp must be stable across \
                 polls — reviewer-flagged regression: previously \
                 each poll re-stamped lines with Utc::now() so the \
                 same historical line drifted forwards in time"
            );
            assert_eq!(
                entry.message, first.log_entries[i].message,
                "log message must match across polls"
            );
        }

        svc.builds.append_log(&job.build_id, "fourth line");
        let third = svc
            .v3_get_build_status(&job.template_id, &job.build_id, 0, 1000)
            .await
            .expect("third status poll should succeed");
        assert_eq!(third.log_entries.len(), expected_total + 1);
        for i in 0..expected_total {
            assert_eq!(
                third.log_entries[i].timestamp, first.log_entries[i].timestamp,
                "appending a new line must not perturb existing \
                 logEntries[{i}].timestamp"
            );
        }
        assert_eq!(third.log_entries[expected_total].message, "fourth line");
        assert!(
            third.log_entries[expected_total].timestamp
                >= first.log_entries[expected_total - 1].timestamp,
            "newly appended line must carry a write-time timestamp \
             at or after the previous tail"
        );
    }

    #[tokio::test]
    async fn v3_get_build_status_log_entries_respect_logs_offset() {
        let svc = make_service(Some("http://127.0.0.1:5000".to_string()));
        let mut req = empty_request();
        req.dockerfile = Some("FROM ubuntu".to_string());
        let job = svc
            .create_template(req)
            .await
            .expect("e2b create should succeed");

        let baseline_len = svc
            .builds
            .get(&job.build_id)
            .expect("ctx")
            .logs
            .len();

        svc.builds.append_log(&job.build_id, "alpha");
        svc.builds.append_log(&job.build_id, "beta");
        svc.builds.append_log(&job.build_id, "gamma");
        svc.builds.append_log(&job.build_id, "delta");

        let expected_ts: Vec<_> = svc
            .builds
            .get(&job.build_id)
            .expect("ctx")
            .logs
            .iter()
            .map(|l| l.timestamp)
            .collect();

        let skip = (baseline_len + 2) as i32;
        let resp = svc
            .v3_get_build_status(&job.template_id, &job.build_id, skip, 1000)
            .await
            .expect("paged status poll should succeed");

        assert_eq!(resp.log_entries.len(), 2);
        assert_eq!(resp.logs.len(), resp.log_entries.len());
        assert_eq!(resp.log_entries[0].message, "gamma");
        assert_eq!(resp.log_entries[1].message, "delta");
        assert_eq!(
            resp.log_entries[0].timestamp,
            expected_ts[baseline_len + 2]
        );
        assert_eq!(
            resp.log_entries[1].timestamp,
            expected_ts[baseline_len + 3]
        );
    }

    #[tokio::test]
    async fn v3_trigger_build_rejects_from_template_with_501_until_resolver_lands() {
        let svc = make_service(Some("http://127.0.0.1:5000".to_string()));
        let mut req = empty_request();
        req.dockerfile = Some("FROM ubuntu".to_string());
        let job = svc
            .create_template(req)
            .await
            .expect("e2b create should succeed");

        let body = V2TemplateBuildStart {
            from_template: Some("tpl-parent-xyz".to_string()),
            ..Default::default()
        };
        let err = svc
            .v3_trigger_build(job.template_id.clone(), job.build_id.clone(), body)
            .await
            .expect_err("fromTemplate must be rejected, not silently dispatched as cube://...");

        match err {
            AppError::NotImplemented(msg) => {
                assert!(
                    msg.contains("tpl-parent-xyz"),
                    "error must echo the rejected parent: {msg}"
                );
                assert!(
                    msg.contains("fromImage"),
                    "error must point operators at the fromImage workaround: {msg}"
                );
                assert!(
                    msg.contains("cube://"),
                    "error should name the unimplemented scheme so \
                     operators can grep release notes for it: {msg}"
                );
            }
            other => panic!("expected NotImplemented, got {other:?}"),
        }

        let ctx = svc
            .builds
            .get(&job.build_id)
            .expect("build context preserved on failure");
        assert_eq!(ctx.stage, BuildStage::WaitingPush);
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


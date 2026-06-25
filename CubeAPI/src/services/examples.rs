// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
//! Example-runner service.
//!
//! Encapsulates all business logic for the three example endpoints:
//! listing available examples, fetching source, and running a script.
//! Handlers stay thin and delegate the actual work here.

use std::path::PathBuf;
use std::time::{Duration, Instant};

use tokio::process::Command;
use tokio::time::timeout;
use uuid::Uuid;

use crate::{
    db::AgentHubStore,
    error::{AppError, AppResult},
    examples::{file_languages, scenario_registry, topology_with_status, FileSpec, ScenarioSpec},
    handlers::examples::{ExampleMeta, RunExampleRequest, RunExampleResponse},
    services::templates::TemplateService,
};

// ─── Service ─────────────────────────────────────────────────────────────────

/// Holds all configuration needed to resolve templates and spawn subprocesses.
#[derive(Clone)]
pub struct ExampleService {
    /// Base URL for the CubeAPI instance example scripts call into.
    cube_api_url: Option<String>,
    /// Default template ID from server config.
    default_template_id: Option<String>,
    /// Proxy node IP for example scripts.
    cube_proxy_node_ip: Option<String>,
    /// HTTP port for the cube proxy.
    cube_proxy_port_http: Option<u16>,
    /// Sandbox domain passed to example scripts.
    sandbox_domain: String,
    /// Sandbox proxy base URL (envd / Jupyter reachability).
    sandbox_proxy_url: String,
    /// Authorization header for internal envd calls.
    envd_auth: String,
    /// Fallback API key injected into example subprocesses when the parent
    /// process does not export CUBE_API_KEY. Sourced from config/env only;
    /// never hardcoded here.
    default_api_key: Option<String>,
}

impl ExampleService {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        cube_api_url: Option<String>,
        default_template_id: Option<String>,
        cube_proxy_node_ip: Option<String>,
        cube_proxy_port_http: Option<u16>,
        sandbox_domain: String,
        sandbox_proxy_url: String,
        envd_auth: String,
        default_api_key: Option<String>,
    ) -> Self {
        Self {
            cube_api_url,
            default_template_id,
            cube_proxy_node_ip,
            cube_proxy_port_http,
            sandbox_domain,
            sandbox_proxy_url,
            envd_auth,
            default_api_key,
        }
    }

    // ─── list ─────────────────────────────────────────────────────────────

    /// Return metadata for all visible examples (hidden scenarios excluded).
    pub fn list_visible(&self) -> Vec<ExampleMeta> {
        let langs = file_languages();
        let mut out = Vec::new();
        for sc in scenario_registry() {
            if sc.hidden {
                continue;
            }
            for f in sc.files {
                let full_id = format!("{}:{}", sc.id, f.id);
                let language = langs
                    .get(full_id.as_str())
                    .copied()
                    .unwrap_or(f.language)
                    .to_string();
                out.push(ExampleMeta {
                    id: full_id,
                    scenario: sc.id.to_string(),
                    filename: f.filename.to_string(),
                    title: f.title.to_string(),
                    description: f.description.to_string(),
                    category: sc.category.to_string(),
                    language,
                    store_item_id: sc.store_item_id.map(|s| s.to_string()),
                });
            }
        }
        out
    }

    // ─── get_source ───────────────────────────────────────────────────────

    /// Read and return the source code of a single visible example.
    pub fn get_source(&self, scenario: &str, file: &str) -> AppResult<serde_json::Value> {
        let id = format!("{}:{}", scenario, file);
        let (meta, _sc, _f) = self
            .resolve_visible(&id)
            .ok_or_else(|| AppError::NotFound(format!("Example '{}' not found", id)))?;

        let base_dir = examples_root().join(&meta.scenario);
        let script_path = base_dir.join(&meta.filename);

        let source = std::fs::read_to_string(&script_path).map_err(|e| {
            AppError::Internal(anyhow::anyhow!(
                "Failed to read '{}': {}",
                script_path.display(),
                e
            ))
        })?;

        Ok(serde_json::json!({
            "id": meta.id,
            "filename": meta.filename,
            "scenario": meta.scenario,
            "language": meta.language,
            "source": source,
        }))
    }

    // ─── run ──────────────────────────────────────────────────────────────

    /// Run an example script in a subprocess and return the full result.
    pub async fn run(
        &self,
        req: RunExampleRequest,
        templates: &TemplateService,
        agenthub_store: Option<&AgentHubStore>,
    ) -> AppResult<RunExampleResponse> {
        let (meta, sc, _f) = self
            .resolve_visible(&req.id)
            .ok_or_else(|| AppError::NotFound(format!("Example '{}' not found", req.id)))?;

        let base_dir = examples_root().join(&meta.scenario);
        let script_path = base_dir.join(&meta.filename);

        // ── Template ID resolution ──────────────────────────────────────
        // Priority:
        //   1. User-explicit template_id (from frontend)
        //   2. Config default_template_id / env CUBE_TEMPLATE_ID
        //   3. store_item_id → match by image_info against catalog
        //   4. Any healthy/ready template
        let template_id = self
            .resolve_template_id(&req, sc, templates, agenthub_store)
            .await?;

        let cube_api_url = req
            .api_url
            .clone()
            .filter(|s| !s.trim().is_empty())
            .unwrap_or_else(|| {
                self.cube_api_url
                    .clone()
                    .unwrap_or_else(|| "http://127.0.0.1:3000".to_string())
            });

        tracing::info!(
            example_id = %req.id,
            scenario = %meta.scenario,
            script = %script_path.display(),
            template_id = %template_id,
            edited = req.code.is_some(),
            "running example"
        );

        let ssl_cert = std::env::var("SSL_CERT_FILE")
            .unwrap_or_else(|_| "/root/.local/share/mkcert/rootCA.pem".to_string());

        // ── Interpreter dispatch based on file extension ────────────────
        // Language-driven (not request-driven) so a malicious `language`
        // field cannot change the interpreter used for a known extension.
        let ext = script_path
            .extension()
            .and_then(|s| s.to_str())
            .unwrap_or("")
            .to_lowercase();

        let program: &str = match ext.as_str() {
            "py" => "python3",
            "go" => "go",
            "sh" | "bash" => "bash",
            "js" | "mjs" => "node",
            _ => {
                return Err(AppError::BadRequest(format!(
                    "Unsupported file extension '.{}' for example '{}'",
                    ext, req.id
                )));
            }
        };

        // ── Auto-install per-scenario Python dependencies ────────────
        if program == "python3" {
            ensure_requirements(&base_dir).await;
        }

        // ── Materialise temp file when user edited the code ──────────
        let mut tmp_path: Option<PathBuf> = None;
        let mut tmp_dir: Option<PathBuf> = None;
        let run_path: PathBuf = if let Some(user_code) = req.code.as_ref() {
            if program == "go" {
                let dir_name = format!(".tmp_run_{}", Uuid::new_v4());
                let dir = base_dir.join(&dir_name);
                std::fs::create_dir_all(&dir).map_err(|e| {
                    AppError::Internal(anyhow::anyhow!(
                        "Failed to create temp dir {}: {}",
                        dir.display(),
                        e
                    ))
                })?;
                let tmp = dir.join(&meta.filename);
                std::fs::write(&tmp, user_code).map_err(|e| {
                    let _ = std::fs::remove_dir_all(&dir);
                    AppError::Internal(anyhow::anyhow!(
                        "Failed to write edited code to {}: {}",
                        tmp.display(),
                        e
                    ))
                })?;
                for go_file in &["go.mod", "go.sum"] {
                    let src = base_dir.join(go_file);
                    if src.exists() {
                        let _ = std::fs::copy(&src, dir.join(go_file));
                    }
                }
                tmp_path = Some(tmp);
                tmp_dir = Some(dir.clone());
                dir.join(&meta.filename)
            } else {
                let tmp_name = format!(".tmp_run_{}.{}", Uuid::new_v4(), ext);
                let tmp = base_dir.join(&tmp_name);
                std::fs::write(&tmp, user_code).map_err(|e| {
                    AppError::Internal(anyhow::anyhow!(
                        "Failed to write edited code to {}: {}",
                        tmp.display(),
                        e
                    ))
                })?;
                tmp_path = Some(tmp.clone());
                tmp
            }
        } else {
            script_path.clone()
        };

        // Build argv.
        let argv: Vec<String> = match program {
            "go" => vec!["run".to_string(), ".".to_string()],
            _ => vec![run_path.to_string_lossy().to_string()],
        };

        let work_dir = if program == "go" {
            run_path.parent().unwrap_or(&base_dir).to_path_buf()
        } else {
            base_dir.clone()
        };

        let mut cmd = Command::new(program);
        for a in &argv {
            cmd.arg(a);
        }
        cmd.env("CUBE_API_URL", &cube_api_url)
            .env("CUBE_TEMPLATE_ID", &template_id)
            .env("SSL_CERT_FILE", ssl_cert)
            .env("AGENTHUB_SANDBOX_PROXY_URL", &self.sandbox_proxy_url)
            .env("CUBE_API_ENVD_AUTH", &self.envd_auth)
            .current_dir(&work_dir);

        if std::env::var("CUBE_API_KEY").is_err() {
            if let Some(ref fallback_key) = self.default_api_key {
                cmd.env("CUBE_API_KEY", fallback_key);
            }
        }

        let effective_proxy_ip = req
            .proxy_node_ip
            .clone()
            .or_else(|| self.cube_proxy_node_ip.clone());
        if let Some(ref proxy_ip) = effective_proxy_ip {
            cmd.env("CUBE_PROXY_NODE_IP", proxy_ip);
        }
        if let Some(proxy_port) = self.cube_proxy_port_http {
            cmd.env("CUBE_PROXY_PORT_HTTP", proxy_port.to_string());
        }
        cmd.env("CUBE_SANDBOX_DOMAIN", &self.sandbox_domain);

        let start = Instant::now();
        let max_secs = sc.timeout_secs.unwrap_or(120);
        let run_result = timeout(Duration::from_secs(max_secs), cmd.output()).await;
        let elapsed_ms = start.elapsed().as_millis() as u64;

        // Always remove temp file/dir, even on error paths.
        if let Some(d) = tmp_dir.take() {
            let _ = std::fs::remove_dir_all(&d);
        } else if let Some(p) = tmp_path.take() {
            let _ = std::fs::remove_file(&p);
        }

        match run_result {
            Ok(Ok(output)) => {
                let stdout = String::from_utf8_lossy(&output.stdout).to_string();
                let stderr = String::from_utf8_lossy(&output.stderr).to_string();
                let exit_code = output.status.code().unwrap_or(-1);
                let success = output.status.success();

                tracing::info!(
                    example_id = %req.id,
                    exit_code,
                    success,
                    elapsed_ms,
                    "example run complete"
                );

                Ok(RunExampleResponse {
                    stdout,
                    stderr,
                    exit_code,
                    success,
                    elapsed_ms,
                    steps: Vec::new(),
                    topology: topology_with_status(sc.topology.clone(), success),
                    ran_edited: req.code.is_some(),
                })
            }
            Ok(Err(io_err)) => Err(AppError::Internal(anyhow::anyhow!(
                "Failed to spawn process: {}",
                io_err
            ))),
            Err(_) => Err(AppError::Internal(anyhow::anyhow!(
                "Example timed out after {} seconds",
                max_secs
            ))),
        }
    }

    // ─── Private helpers ──────────────────────────────────────────────────

    fn resolve_visible(
        &self,
        id: &str,
    ) -> Option<(ExampleMeta, &'static ScenarioSpec, &'static FileSpec)> {
        let langs = file_languages();
        let (scenario_id, file_id) = id.split_once(':')?;
        for sc in scenario_registry() {
            if sc.hidden || sc.id != scenario_id {
                continue;
            }
            for f in sc.files {
                if f.id == file_id {
                    let full_id = format!("{}:{}", sc.id, f.id);
                    let language = langs
                        .get(full_id.as_str())
                        .copied()
                        .unwrap_or(f.language)
                        .to_string();
                    let meta = ExampleMeta {
                        id: full_id,
                        scenario: sc.id.to_string(),
                        filename: f.filename.to_string(),
                        title: f.title.to_string(),
                        description: f.description.to_string(),
                        category: sc.category.to_string(),
                        language,
                        store_item_id: sc.store_item_id.map(|s| s.to_string()),
                    };
                    return Some((meta, sc, f));
                }
            }
        }
        None
    }

    async fn resolve_template_id(
        &self,
        req: &RunExampleRequest,
        sc: &ScenarioSpec,
        templates: &TemplateService,
        agenthub_store: Option<&AgentHubStore>,
    ) -> AppResult<String> {
        // 1. Explicit from request / config / env
        let candidates: Vec<String> = [
            req.template_id.clone().filter(|s| !s.trim().is_empty()),
            self.default_template_id.clone(),
            std::env::var("CUBE_TEMPLATE_ID")
                .ok()
                .filter(|s| !s.is_empty()),
        ]
        .into_iter()
        .flatten()
        .collect();

        for candidate in &candidates {
            match templates.get_template(candidate).await {
                Ok(_) => return Ok(candidate.clone()),
                Err(e) => {
                    tracing::warn!(
                        candidate = %candidate,
                        error = %e,
                        "template candidate failed validation, trying next"
                    );
                }
            }
        }

        // 2. store_item_id → match by image_info
        if let Some(ref sid) = sc.store_item_id {
            let catalog_image: Option<String> = match agenthub_store {
                Some(store) => store.list_store_templates().await.ok().and_then(|catalog| {
                    catalog
                        .into_iter()
                        .find(|item| item.item_id == *sid)
                        .map(|item| item.image_cn)
                }),
                None => None,
            };
            if let Some(ref image_ref) = catalog_image {
                if let Ok(tpls) = templates.list_templates().await {
                    let matched = tpls.iter().find(|t| {
                        (t.status == "healthy" || t.status == "ready")
                            && t.image_info.as_deref() == Some(image_ref.as_str())
                    });
                    if let Some(t) = matched {
                        tracing::info!(
                            store_item_id = %sid,
                            image = %image_ref,
                            template_id = %t.template_id,
                            "matched template via store_item_id"
                        );
                        return Ok(t.template_id.clone());
                    }
                }
            }
        }

        // 3. Any healthy/ready template
        if let Ok(tpls) = templates.list_templates().await {
            let list_candidates: Vec<_> = tpls
                .iter()
                .filter(|t| t.status == "healthy" || t.status == "ready")
                .map(|t| t.template_id.as_str())
                .collect();
            for candidate in list_candidates {
                match templates.get_template(candidate).await {
                    Ok(_) => return Ok(candidate.to_string()),
                    Err(e) => {
                        tracing::warn!(
                            candidate = %candidate,
                            error = %e,
                            "listed template failed validation, skipping"
                        );
                    }
                }
            }
        }

        Err(AppError::BadRequest(
            "No template ID configured. Set CUBE_TEMPLATE_ID, configure a default template, \
             or create a template first."
                .to_string(),
        ))
    }
}

// ─── Filesystem helpers ───────────────────────────────────────────────────────

/// Resolve the examples root directory.
///
/// `CUBE_EXAMPLES_DIR` overrides the root for tests / packaged installs.
/// Default points at the in-repo `examples/` directory relative to
/// `CubeAPI/Cargo.toml`.
pub fn examples_root() -> PathBuf {
    if let Ok(v) = std::env::var("CUBE_EXAMPLES_DIR") {
        return PathBuf::from(v);
    }
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("..")
        .join("examples")
}

/// Install per-scenario Python dependencies from `requirements.txt` if present.
///
/// Uses a lightweight fingerprint file (`.requirements_installed`) to skip
/// redundant installs when the requirements have not changed since the last
/// successful install.
async fn ensure_requirements(base_dir: &PathBuf) -> bool {
    let req_file = base_dir.join("requirements.txt");
    if !req_file.exists() {
        return true;
    }

    let req_content = match std::fs::read_to_string(&req_file) {
        Ok(c) => c,
        Err(e) => {
            tracing::warn!("cannot read {}: {}", req_file.display(), e);
            return false;
        }
    };

    let stamp_file = base_dir.join(".requirements_installed");
    if let Ok(stamp) = std::fs::read_to_string(&stamp_file) {
        if stamp == req_content {
            tracing::debug!("requirements unchanged, skipping pip install");
            return true;
        }
    }

    tracing::info!(
        "installing scenario requirements from {}",
        req_file.display()
    );
    let install_result = Command::new("pip3")
        .args(["install", "--quiet", "-r"])
        .arg(&req_file)
        .output()
        .await;

    match install_result {
        Ok(output) => {
            if output.status.success() {
                let _ = std::fs::write(&stamp_file, &req_content);
                true
            } else {
                tracing::warn!(
                    stderr = %String::from_utf8_lossy(&output.stderr),
                    "pip install failed, continuing anyway"
                );
                true
            }
        }
        Err(e) => {
            tracing::warn!("failed to spawn pip3: {}", e);
            true
        }
    }
}

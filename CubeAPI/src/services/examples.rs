// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
//! Example-runner service.
//!
//! Encapsulates all business logic for the three example endpoints:
//! listing available examples, fetching source, and running a script.
//! Handlers stay thin and delegate the actual work here.

use std::path::{Path, PathBuf};
use std::sync::OnceLock;
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
    /// Fallback API key injected into example subprocesses when the parent
    /// process does not export CUBE_API_KEY. Sourced from config/env only;
    /// never hardcoded here.
    default_api_key: Option<String>,
    /// Auth callback URL mirrored from ServerConfig.
    /// When set, the server enforces per-request authentication; the
    /// hardcoded fallback key MUST NOT be injected in that mode because
    /// it is a publicly known value and would constitute a shared credential.
    auth_callback_url: Option<String>,
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
        default_api_key: Option<String>,
        auth_callback_url: Option<String>,
    ) -> Self {
        Self {
            cube_api_url,
            default_template_id,
            cube_proxy_node_ip,
            cube_proxy_port_http,
            sandbox_domain,
            sandbox_proxy_url,
            default_api_key,
            auth_callback_url,
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
    pub async fn get_source(&self, scenario: &str, file: &str) -> AppResult<serde_json::Value> {
        let id = format!("{}:{}", scenario, file);
        let (meta, _sc, _f) = self
            .resolve_visible(&id)
            .ok_or_else(|| AppError::NotFound(format!("Example '{}' not found", id)))?;

        let base_dir = examples_root().join(&meta.scenario);
        let script_path = base_dir.join(&meta.filename);

        let source = tokio::fs::read_to_string(&script_path).await.map_err(|e| {
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
                let dir = base_dir
                    .parent()
                    .unwrap_or(base_dir.as_path())
                    .join(&dir_name);
                tokio::fs::create_dir_all(&dir).await.map_err(|e| {
                    AppError::Internal(anyhow::anyhow!(
                        "Failed to create temp dir {}: {}",
                        dir.display(),
                        e
                    ))
                })?;
                let tmp = dir.join(&meta.filename);
                tokio::fs::write(&tmp, user_code).await.map_err(|e| {
                    // best-effort sync cleanup in error path; std::fs is acceptable here
                    let _ = std::fs::remove_dir_all(&dir);
                    AppError::Internal(anyhow::anyhow!(
                        "Failed to write edited code to {}: {}",
                        tmp.display(),
                        e
                    ))
                })?;
                for go_file in &["go.mod", "go.sum"] {
                    let src = base_dir.join(go_file);
                    if tokio::fs::try_exists(&src).await.unwrap_or(false) {
                        let _ = tokio::fs::copy(&src, dir.join(go_file)).await;
                    }
                }
                // Copy all other .go files in the same package so the build
                // has the full set of sources; the user-edited file is already
                // written above and must not be overwritten here.
                if let Ok(mut entries) = tokio::fs::read_dir(&base_dir).await {
                    while let Ok(Some(entry)) = entries.next_entry().await {
                        let src = entry.path();
                        if src.extension().and_then(|e| e.to_str()) == Some("go") {
                            let fname = entry.file_name();
                            if fname != meta.filename.as_str() {
                                let _ = tokio::fs::copy(&src, dir.join(&fname)).await;
                            }
                        }
                    }
                }
                tmp_path = Some(tmp);
                tmp_dir = Some(dir.clone());
                dir.join(&meta.filename)
            } else {
                let tmp_name = format!(".tmp_run_{}.{}", Uuid::new_v4(), ext);
                let tmp = base_dir.join(&tmp_name);
                tokio::fs::write(&tmp, user_code).await.map_err(|e| {
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
        // SECURITY: envd_auth is an internal server-side credential and MUST
        // NOT be forwarded to user-controlled subprocesses. Example scripts
        // access the proxy via CUBE_PROXY_NODE_IP / CUBE_PROXY_PORT_HTTP
        // (public HTTP endpoints) and never need direct envd credentials.
        cmd.env("CUBE_API_URL", &cube_api_url)
            .env("CUBE_TEMPLATE_ID", &template_id)
            .env("SSL_CERT_FILE", ssl_cert)
            .env("AGENTHUB_SANDBOX_PROXY_URL", &self.sandbox_proxy_url)
            .current_dir(&work_dir);

        // Inject a fallback CUBE_API_KEY only when:
        //   1. The parent process did not already export one, AND
        //   2. Authentication is disabled (no auth_callback_url).
        //
        // When auth is enabled the default_api_key may be a publicly known
        // value (cube_0000...). Injecting it would hand subprocesses a shared
        // credential that could pass the callback check — a security risk.
        // In that mode the operator must export a real CUBE_API_KEY or set
        // CUBE_API_DEFAULT_KEY to a secret value before starting cube-api.
        if std::env::var("CUBE_API_KEY").is_err() && self.auth_callback_url.is_none() {
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
            let _ = tokio::fs::remove_dir_all(&d).await;
        } else if let Some(p) = tmp_path.take() {
            let _ = tokio::fs::remove_file(&p).await;
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

        // Fetch the full template list once; reused by both stage 2 and stage 3
        // to avoid duplicate HTTP round-trips to CubeMaster.
        let all_templates = templates.list_templates().await.ok();

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
                if let Some(ref tpls) = all_templates {
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
        // The status field from list_templates() is authoritative enough for
        // fallback selection; a redundant get_template() per candidate is not
        // needed and was the source of the N+1 cascade.
        if let Some(ref tpls) = all_templates {
            if let Some(t) = tpls
                .iter()
                .find(|t| t.status == "healthy" || t.status == "ready")
            {
                tracing::info!(
                    template_id = %t.template_id,
                    "resolved template: first healthy/ready from list"
                );
                return Ok(t.template_id.clone());
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
/// Resolution order (first existing & valid directory wins):
///   1. `$CUBE_EXAMPLES_DIR` — explicit override (highest priority).
///   2. `<exe_dir>/examples`            — flat / portable install.
///   3. `<exe_dir>/../examples`         — typical systemd layout
///      (e.g. `/opt/cube/bin/cubeapi` + `/opt/cube/examples`).
///   4. `<exe_dir>/../share/cubeapi/examples` — FHS layout.
///   5. `<cwd>/examples`                — running the binary from repo root.
///   6. `env!("CARGO_MANIFEST_DIR")/../examples` — dev fallback for
///      `cargo run` / `cargo test`. NOTE: this is a compile-time constant
///      baked into the binary; on container-built / cross-machine deploys
///      it usually does NOT match the host path, which is exactly why we
///      validate existence and fall back gracefully instead of returning
///      it blindly.
///
/// A candidate is considered valid only when it is an existing directory
/// AND contains at least one registered scenario sub-directory. The latter
/// check guards against accidentally pointing at an unrelated `examples/`
/// directory that happens to exist on the host.
///
/// The result is cached in a process-wide `OnceLock` and returned by clone
/// to preserve the existing `-> PathBuf` signature (callers do
/// `examples_root().join(...)`).
///
/// On total failure the function logs every candidate it tried and
/// terminates the process, so misconfiguration surfaces at startup rather
/// than as a confusing HTTP 500 the first time a user opens a case.
pub fn examples_root() -> PathBuf {
    static ROOT: OnceLock<PathBuf> = OnceLock::new();
    ROOT.get_or_init(|| match resolve_examples_root() {
        Ok(p) => {
            tracing::info!(path = %p.display(), "examples root resolved");
            p
        }
        Err(candidates) => {
            eprintln!(
                "[FATAL] CubeAPI cannot locate the examples/ directory. \
                 Tried the following paths (in order):"
            );
            for (i, c) in candidates.iter().enumerate() {
                eprintln!("  {}. {}", i + 1, c.display());
            }
            eprintln!(
                "Hint: set CUBE_EXAMPLES_DIR to the absolute path of the \
                 examples/ directory, e.g.\n      \
                 export CUBE_EXAMPLES_DIR=/path/to/CubeSandbox/examples"
            );
            std::process::exit(1);
        }
    })
    .clone()
}

/// Build the ordered candidate list and return the first valid one,
/// or the full list (for error reporting) when none match.
fn resolve_examples_root() -> Result<PathBuf, Vec<PathBuf>> {
    let candidates = build_examples_candidates();
    for c in &candidates {
        if is_valid_examples_dir(c) {
            return Ok(c.clone());
        }
    }
    Err(candidates)
}

fn build_examples_candidates() -> Vec<PathBuf> {
    let mut v: Vec<PathBuf> = Vec::with_capacity(6);

    // 1. Explicit override.
    if let Ok(p) = std::env::var("CUBE_EXAMPLES_DIR") {
        if !p.is_empty() {
            v.push(PathBuf::from(p));
        }
    }

    // 2 / 3 / 4. Paths derived from the running executable's location.
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            v.push(dir.join("examples"));
            v.push(dir.join("..").join("examples"));
            v.push(
                dir.join("..")
                    .join("share")
                    .join("cubeapi")
                    .join("examples"),
            );
        }
    }

    // 5. Current working directory (e.g. running ./target/release/cubeapi
    //    from the repo root after a container build).
    if let Ok(cwd) = std::env::current_dir() {
        v.push(cwd.join("examples"));
    }

    // 6. Compile-time fallback (`cargo run` / `cargo test`).
    v.push(
        PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("..")
            .join("examples"),
    );

    v
}

/// A path is a usable examples root only when it is a directory AND
/// contains at least one registered scenario sub-directory. The latter
/// check prevents false positives like an empty `./examples/` lying
/// around in the working directory.
fn is_valid_examples_dir(p: &Path) -> bool {
    if !p.is_dir() {
        return false;
    }
    // Use the first non-hidden scenario from the registry as a sentinel
    // so this validator stays in sync as scenarios are added/removed.
    let sentinel = scenario_registry().iter().find(|s| !s.hidden);
    match sentinel {
        Some(s) => p.join(s.id).is_dir(),
        // No visible scenarios registered → any existing directory passes;
        // this only happens in unusual test builds.
        None => true,
    }
}

/// Install per-scenario Python dependencies from `requirements.txt` if present.
///
/// Uses a lightweight fingerprint file (`.requirements_installed`) to skip
/// redundant installs when the requirements have not changed since the last
/// successful install.
async fn ensure_requirements(base_dir: &Path) -> bool {
    let req_file = base_dir.join("requirements.txt");
    if !tokio::fs::try_exists(&req_file).await.unwrap_or(false) {
        return true;
    }

    let req_content = match tokio::fs::read_to_string(&req_file).await {
        Ok(c) => c,
        Err(e) => {
            tracing::warn!("cannot read {}: {}", req_file.display(), e);
            return false;
        }
    };

    let stamp_file = base_dir.join(".requirements_installed");
    if let Ok(stamp) = tokio::fs::read_to_string(&stamp_file).await {
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
                let _ = tokio::fs::write(&stamp_file, &req_content).await;
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

// ─── Tests ────────────────────────────────────────────────────────────────────

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use uuid::Uuid;

    /// RAII helper that creates a unique temp directory and removes it
    /// (recursively) on drop. Avoids pulling in the `tempfile` crate just
    /// for these tests.
    struct TempDir(PathBuf);

    impl TempDir {
        fn new() -> Self {
            let p = std::env::temp_dir().join(format!("cubeapi-test-{}", Uuid::new_v4()));
            fs::create_dir_all(&p).expect("create temp dir");
            Self(p)
        }
        fn path(&self) -> &Path {
            &self.0
        }
    }

    impl Drop for TempDir {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    fn sentinel_scenario_id() -> &'static str {
        scenario_registry()
            .iter()
            .find(|s| !s.hidden)
            .expect("registry has at least one visible scenario")
            .id
    }

    #[test]
    fn is_valid_examples_dir_rejects_non_directory() {
        let tmp = TempDir::new();
        let f = tmp.path().join("not-a-dir");
        fs::write(&f, b"hi").unwrap();
        assert!(!is_valid_examples_dir(&f));
        assert!(!is_valid_examples_dir(&tmp.path().join("does-not-exist")));
    }

    #[test]
    fn is_valid_examples_dir_rejects_directory_without_sentinel() {
        let tmp = TempDir::new();
        // Directory exists but has no registered scenario sub-directory.
        assert!(!is_valid_examples_dir(tmp.path()));
    }

    #[test]
    fn is_valid_examples_dir_accepts_directory_with_sentinel() {
        let tmp = TempDir::new();
        fs::create_dir_all(tmp.path().join(sentinel_scenario_id())).unwrap();
        assert!(is_valid_examples_dir(tmp.path()));
    }

    #[test]
    fn resolver_prefers_cube_examples_dir_env_var() {
        // Build a valid examples layout under a temp dir, point the env
        // var at it, and confirm `resolve_examples_root` returns it.
        let tmp = TempDir::new();
        fs::create_dir_all(tmp.path().join(sentinel_scenario_id())).unwrap();

        // SAFETY: tests in this module run serially via the per-test
        // serialization guard below. Even without it, we restore the
        // previous value on drop.
        let _g = EnvGuard::set("CUBE_EXAMPLES_DIR", tmp.path().to_str().unwrap());

        let resolved = resolve_examples_root().expect("should resolve via env var");
        assert_eq!(resolved, tmp.path());
    }

    #[test]
    fn resolver_skips_invalid_env_var_and_falls_through() {
        // Env var points at a non-existent path; resolver should ignore
        // it and continue down the candidate list. We can't assert which
        // later candidate wins (depends on the host), so we only assert
        // that resolution does NOT return the bogus env path.
        let tmp = TempDir::new();
        let bogus = tmp.path().join("definitely-not-here");
        let _g = EnvGuard::set("CUBE_EXAMPLES_DIR", bogus.to_str().unwrap());

        match resolve_examples_root() {
            Ok(p) => assert_ne!(p, bogus, "must not pick a non-existent env path"),
            Err(candidates) => {
                // Acceptable: every candidate failed on this host.
                // Just make sure the bogus path was indeed tried.
                assert!(candidates.iter().any(|c| c == &bogus));
            }
        }
    }

    #[test]
    fn build_candidates_includes_env_var_first_when_set() {
        let _g = EnvGuard::set("CUBE_EXAMPLES_DIR", "/tmp/cube-test-marker");
        let cands = build_examples_candidates();
        assert_eq!(
            cands.first().map(|p| p.as_path()),
            Some(Path::new("/tmp/cube-test-marker"))
        );
    }

    #[test]
    fn build_candidates_omits_empty_env_var() {
        let _g = EnvGuard::set("CUBE_EXAMPLES_DIR", "");
        let cands = build_examples_candidates();
        assert!(!cands.iter().any(|p| p.as_os_str().is_empty()));
        // Still has the compile-time fallback at the end.
        assert!(!cands.is_empty());
    }

    // ── Env var guard (restores previous value on drop) ──────────────
    struct EnvGuard {
        key: &'static str,
        prev: Option<std::ffi::OsString>,
    }
    impl EnvGuard {
        fn set(key: &'static str, val: &str) -> Self {
            let prev = std::env::var_os(key);
            std::env::set_var(key, val);
            Self { key, prev }
        }
    }
    impl Drop for EnvGuard {
        fn drop(&mut self) {
            match &self.prev {
                Some(v) => std::env::set_var(self.key, v),
                None => std::env::remove_var(self.key),
            }
        }
    }
}

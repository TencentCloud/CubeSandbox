// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// Examples handler: list available example scripts and run them via subprocess.

use axum::{extract::State, http::StatusCode, response::IntoResponse, Json};
use serde::{Deserialize, Serialize};
use std::time::Duration;
use tokio::process::Command;
use tokio::time::timeout;
use utoipa::ToSchema;

use crate::{error::AppResult, state::AppState};

// ─── Models ───────────────────────────────────────────────────────────────────

#[derive(Serialize, ToSchema)]
pub struct ExampleMeta {
    pub id: String,
    pub filename: String,
    pub title: String,
    pub description: String,
    pub category: String,
}

#[derive(Deserialize, ToSchema)]
pub struct RunExampleRequest {
    pub id: String,
    /// Optional template ID override. When provided, takes highest priority
    /// over server-configured defaults. Allows the frontend to let users
    /// pick which template to use for each example run.
    pub template_id: Option<String>,
}

#[derive(Serialize, ToSchema)]
pub struct RunExampleResponse {
    pub stdout: String,
    pub stderr: String,
    pub exit_code: i32,
    pub success: bool,
}

// ─── Example registry ─────────────────────────────────────────────────────────

fn example_list() -> Vec<ExampleMeta> {
    vec![
        ExampleMeta {
            id: "create".to_string(),
            filename: "create.py".to_string(),
            title: "Create Sandbox".to_string(),
            description: "Create a sandbox from a template and retrieve its metadata.".to_string(),
            category: "basics".to_string(),
        },
        ExampleMeta {
            id: "exec_code".to_string(),
            filename: "exec_code.py".to_string(),
            title: "Execute Code".to_string(),
            description: "Run Python code inside the sandbox using the Jupyter kernel.".to_string(),
            category: "basics".to_string(),
        },
        ExampleMeta {
            id: "cmd".to_string(),
            filename: "cmd.py".to_string(),
            title: "Run Shell Command".to_string(),
            description: "Execute shell commands inside the sandbox and capture stdout.".to_string(),
            category: "basics".to_string(),
        },
        ExampleMeta {
            id: "read".to_string(),
            filename: "read.py".to_string(),
            title: "File Read/Write".to_string(),
            description: "Read files from the sandbox filesystem.".to_string(),
            category: "filesystem".to_string(),
        },
        ExampleMeta {
            id: "pause".to_string(),
            filename: "pause.py".to_string(),
            title: "Pause & Resume".to_string(),
            description: "Pause a sandbox to save its state and resume it later.".to_string(),
            category: "lifecycle".to_string(),
        },
        ExampleMeta {
            id: "network_no_internet".to_string(),
            filename: "network_no_internet.py".to_string(),
            title: "No Internet Access".to_string(),
            description: "Create a fully isolated sandbox with all outbound traffic blocked.".to_string(),
            category: "network".to_string(),
        },
        ExampleMeta {
            id: "network_allowlist".to_string(),
            filename: "network_allowlist.py".to_string(),
            title: "Network Allowlist".to_string(),
            description: "Allow only specific IP/CIDR ranges while blocking everything else.".to_string(),
            category: "network".to_string(),
        },
        ExampleMeta {
            id: "network_denylist".to_string(),
            filename: "network_denylist.py".to_string(),
            title: "Network Denylist".to_string(),
            description: "Allow internet but block specific IP/CIDR ranges.".to_string(),
            category: "network".to_string(),
        },
    ]
}

fn example_base_dir() -> String {
    std::env::var("CUBE_EXAMPLES_DIR")
        .unwrap_or_else(|_| "/root/CubeSandbox/examples/code-sandbox-quickstart".to_string())
}

// ─── GET /cubeapi/v1/examples ────────────────────────────────────────────────

/// List all available example scripts.
pub async fn list_examples(State(_state): State<AppState>) -> AppResult<impl IntoResponse> {
    Ok(Json(example_list()))
}

// ─── GET /cubeapi/v1/examples/:id ───────────────────────────────────────────

/// Get the source code of a single example script by id.
pub async fn get_example_source(
    State(_state): State<AppState>,
    axum::extract::Path(id): axum::extract::Path<String>,
) -> AppResult<impl IntoResponse> {
    // Find example by id (only allow ids in the registry to prevent arbitrary file access)
    let examples = example_list();
    let example = match examples.iter().find(|e| e.id == id) {
        Some(e) => e,
        None => {
            return Ok((
                StatusCode::NOT_FOUND,
                Json(serde_json::json!({
                    "error": format!("Example '{}' not found", id)
                })),
            )
                .into_response());
        }
    };

    let base_dir = example_base_dir();
    let script_path = format!("{}/{}", base_dir, example.filename);

    match std::fs::read_to_string(&script_path) {
        Ok(source) => Ok((
            StatusCode::OK,
            Json(serde_json::json!({
                "id": example.id,
                "filename": example.filename,
                "source": source,
            })),
        )
            .into_response()),
        Err(io_err) => Ok((
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(serde_json::json!({
                "error": format!("Failed to read '{}': {}", script_path, io_err)
            })),
        )
            .into_response()),
    }
}

// ─── POST /cubeapi/v1/examples/run ───────────────────────────────────────────

/// Run an example script in a subprocess and return stdout/stderr.
pub async fn run_example(
    State(state): State<AppState>,
    Json(req): Json<RunExampleRequest>,
) -> AppResult<impl IntoResponse> {
    // Find example by id
    let examples = example_list();
    let example = match examples.iter().find(|e| e.id == req.id) {
        Some(e) => e,
        None => {
            return Ok((
                StatusCode::NOT_FOUND,
                Json(RunExampleResponse {
                    stdout: String::new(),
                    stderr: format!("Example '{}' not found", req.id),
                    exit_code: 1,
                    success: false,
                }),
            )
                .into_response());
        }
    };

    let base_dir = example_base_dir();
    let script_path = format!("{}/{}", base_dir, example.filename);

    // Resolve template ID with multi-level fallback.
    // Each candidate is validated with get_template before acceptance.
    let candidates: Vec<String> = [
        req.template_id.filter(|s| !s.trim().is_empty()),
        state.config.default_template_id.clone(),
        std::env::var("CUBE_TEMPLATE_ID").ok().filter(|s| !s.is_empty()),
    ]
    .into_iter()
    .flatten()
    .collect();

    let mut template_id = String::new();
    for candidate in &candidates {
        match state.services.templates.get_template(candidate).await {
            Ok(_) => {
                template_id = candidate.clone();
                break;
            }
            Err(e) => {
                tracing::warn!(
                    candidate = %candidate,
                    error = %e,
                    "template candidate failed validation, trying next"
                );
            }
        }
    }

    if template_id.is_empty() {
        // Last resort: ask CubeMaster for the first available template
        match state.services.templates.list_templates().await {
            Ok(templates) => {
                let list_candidates: Vec<_> = templates
                    .iter()
                    .filter(|t| t.status == "healthy" || t.status == "ready")
                    .chain(templates.iter())
                    .map(|t| t.template_id.as_str())
                    .collect();

                for candidate in list_candidates {
                    match state.services.templates.get_template(candidate).await {
                        Ok(_) => {
                            template_id = candidate.to_string();
                            break;
                        }
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
            Err(e) => {
                tracing::warn!(error = %e, "failed to list templates for fallback");
            }
        }
    }

    if template_id.is_empty() {
        return Ok((
            StatusCode::BAD_REQUEST,
            Json(RunExampleResponse {
                stdout: String::new(),
                stderr: "No template ID configured. Set CUBE_TEMPLATE_ID, configure a default template, or create a template first.".to_string(),
                exit_code: 1,
                success: false,
            }),
        )
            .into_response());
    }

    let cube_api_url = state
        .config
        .cube_api_url
        .clone()
        .unwrap_or_else(|| "http://127.0.0.1:3000".to_string());

    tracing::info!(
        example_id = %req.id,
        script = %script_path,
        template_id = %template_id,
        "running example"
    );

    let ssl_cert = std::env::var("SSL_CERT_FILE")
        .unwrap_or_else(|_| "/root/.local/share/mkcert/rootCA.pem".to_string());

    let mut cmd = Command::new("python3");
    cmd.arg(&script_path)
        .env("CUBE_API_URL", &cube_api_url)
        .env("CUBE_TEMPLATE_ID", &template_id)
        .env("SSL_CERT_FILE", ssl_cert)
        .current_dir(&base_dir);

    // Pass CubeProxy configuration if available
    if let Some(ref proxy_ip) = state.config.cube_proxy_node_ip {
        cmd.env("CUBE_PROXY_NODE_IP", proxy_ip);
    }
    if let Some(proxy_port) = state.config.cube_proxy_port_http {
        cmd.env("CUBE_PROXY_PORT_HTTP", proxy_port.to_string());
    }
    cmd.env("CUBE_SANDBOX_DOMAIN", &state.config.sandbox_domain);

    let run_result = timeout(
        Duration::from_secs(120),
        cmd.output(),
    )
    .await;

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
                "example run complete"
            );

            Ok(Json(RunExampleResponse {
                stdout,
                stderr,
                exit_code,
                success,
            })
            .into_response())
        }
        Ok(Err(io_err)) => Ok((
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(RunExampleResponse {
                stdout: String::new(),
                stderr: format!("Failed to spawn process: {}", io_err),
                exit_code: -1,
                success: false,
            }),
        )
            .into_response()),
        Err(_elapsed) => Ok((
            StatusCode::GATEWAY_TIMEOUT,
            Json(RunExampleResponse {
                stdout: String::new(),
                stderr: "Example timed out after 120 seconds".to_string(),
                exit_code: -1,
                success: false,
            }),
        )
            .into_response()),
    }
}

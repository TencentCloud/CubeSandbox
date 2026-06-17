// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::time::Instant;

use axum::{
    extract::{Path, Query, State},
    http::StatusCode,
    response::IntoResponse,
    Json,
};
use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use serde_json::Value;

use crate::{
    error::{AppError, AppResult},
    logging::{LogEvent, LogLevel},
    models::{
        ApiError, ConnectSandbox, ExecCodeRequest, ExecCodeResponse, ListSandboxesQuery,
        ListSandboxesV2Query, NewSandbox, RefreshRequest, ResumedSandbox, Sandbox, SandboxDetail,
        SandboxLogsQuery, SandboxLogsV2Query, SandboxLogsV2Response, SetTimeoutRequest,
    },
    state::AppState,
};

const ENVD_PORT: u16 = 49983;
const JUPYTER_PORT: u16 = 49999;
const CONNECT_JSON: &str = "application/connect+json";

// ─── GET /sandboxes ───────────────────────────────────────────────────────────

pub async fn list_sandboxes(
    State(state): State<AppState>,
    Query(params): Query<ListSandboxesQuery>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "list_sandboxes")
                .field("metadata_filter", params.metadata.as_deref().unwrap_or("")),
        )
        .await;

    match state
        .services
        .sandboxes
        .list(params.metadata.as_deref(), None, 200)
        .await
    {
        Ok(list) => {
            state
                .logger
                .log(
                    LogEvent::new(LogLevel::Info, "api.response")
                        .field("handler", "list_sandboxes")
                        .field_value("count", list.len()),
                )
                .await;
            Ok(Json(list))
        }
        Err(error) => {
            let message = error.to_string();
            tracing::error!(error = %message, "list_sandboxes: service error");
            state
                .logger
                .log(
                    LogEvent::new(LogLevel::Error, "api.error")
                        .field("handler", "list_sandboxes")
                        .field("error", &message),
                )
                .await;
            Err(error)
        }
    }
}

// ─── GET /v2/sandboxes ────────────────────────────────────────────────────────

#[utoipa::path(
    get,
    path = "/v2/sandboxes",
    params(ListSandboxesV2Query),
    responses(
        (status = 200, description = "Sandbox list", body = [crate::models::ListedSandbox]),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn list_sandboxes_v2(
    State(state): State<AppState>,
    Query(params): Query<ListSandboxesV2Query>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "list_sandboxes_v2")
                .field("state_filter", params.state.as_deref().unwrap_or(""))
                .field_value("limit", params.limit),
        )
        .await;

    let list = state
        .services
        .sandboxes
        .list(
            params.metadata.as_deref(),
            params.state.as_deref(),
            params.limit,
        )
        .await?;

    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "api.response")
                .field("handler", "list_sandboxes_v2")
                .field_value("count", list.len()),
        )
        .await;
    Ok(Json(list))
}

// ─── GET /sandboxes/:sandboxID ────────────────────────────────────────────────

#[utoipa::path(
    get,
    path = "/sandboxes/{sandboxID}",
    params(
        ("sandboxID" = String, Path, description = "Sandbox identifier")
    ),
    responses(
        (status = 200, description = "Sandbox detail", body = SandboxDetail),
        (status = 404, description = "Sandbox not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn get_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "get_sandbox")
                .field("sandbox_id", &sandbox_id),
        )
        .await;

    let detail = state.services.sandboxes.get_sandbox(&sandbox_id).await?;
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "api.response")
                .field("handler", "get_sandbox")
                .field("sandbox_id", &sandbox_id),
        )
        .await;
    Ok(Json(detail))
}

// ─── POST /sandboxes ──────────────────────────────────────────────────────────

pub async fn create_sandbox(
    State(state): State<AppState>,
    Json(body): Json<NewSandbox>,
) -> AppResult<impl IntoResponse> {
    let template_id = body.template_id.clone();
    let timeout = body.timeout;
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "create_sandbox")
                .field("template_id", &template_id)
                .field_value("timeout", timeout),
        )
        .await;

    let created = state.services.sandboxes.create_sandbox(body).await?;
    let sandbox_id = created.sandbox_id.clone();

    tracing::info!(sandbox_id = %sandbox_id, template_id = %template_id, "create_sandbox: success");
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "sandbox.created")
                .field("sandbox_id", &sandbox_id)
                .field("template_id", &template_id),
        )
        .await;

    Ok((StatusCode::CREATED, Json(created)))
}

// ─── DELETE /sandboxes/:sandboxID ─────────────────────────────────────────────

#[utoipa::path(
    delete,
    path = "/sandboxes/{sandboxID}",
    params(
        ("sandboxID" = String, Path, description = "Sandbox identifier")
    ),
    responses(
        (status = 204, description = "Sandbox deleted"),
        (status = 404, description = "Sandbox not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn kill_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "kill_sandbox")
                .field("sandbox_id", &sandbox_id),
        )
        .await;

    state.services.sandboxes.kill_sandbox(&sandbox_id).await?;

    tracing::info!(sandbox_id = %sandbox_id, "kill_sandbox: success");
    state
        .logger
        .log(LogEvent::new(LogLevel::Info, "sandbox.deleted").field("sandbox_id", &sandbox_id))
        .await;
    Ok(StatusCode::NO_CONTENT)
}

// ─── POST /sandboxes/:sandboxID/pause ─────────────────────────────────────────

#[utoipa::path(
    post,
    path = "/sandboxes/{sandboxID}/pause",
    params(
        ("sandboxID" = String, Path, description = "Sandbox identifier")
    ),
    responses(
        (status = 204, description = "Sandbox paused"),
        (status = 404, description = "Sandbox not found", body = ApiError),
        (status = 409, description = "Sandbox cannot be paused", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn pause_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "pause_sandbox")
                .field("sandbox_id", &sandbox_id),
        )
        .await;
    tracing::info!(sandbox_id = %sandbox_id, "pause sandbox request");
    state.services.sandboxes.pause_sandbox(&sandbox_id).await?;

    tracing::info!(sandbox_id = %sandbox_id, "pause_sandbox: success");
    state
        .logger
        .log(LogEvent::new(LogLevel::Info, "sandbox.paused").field("sandbox_id", &sandbox_id))
        .await;
    Ok(StatusCode::NO_CONTENT)
}

// ─── POST /sandboxes/:sandboxID/resume ────────────────────────────────────────

#[utoipa::path(
    post,
    path = "/sandboxes/{sandboxID}/resume",
    params(
        ("sandboxID" = String, Path, description = "Sandbox identifier")
    ),
    request_body = ResumedSandbox,
    responses(
        (status = 201, description = "Sandbox resumed", body = Sandbox),
        (status = 404, description = "Sandbox not found", body = ApiError),
        (status = 409, description = "Sandbox is already running", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn resume_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Json(body): Json<ResumedSandbox>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "resume_sandbox")
                .field("sandbox_id", &sandbox_id)
                .field_value("timeout", body.timeout),
        )
        .await;
    tracing::info!(sandbox_id = %sandbox_id, "resume sandbox request");
    let sandbox = state
        .services
        .sandboxes
        .resume_sandbox(&sandbox_id, body.timeout)
        .await?;

    tracing::info!(sandbox_id = %sandbox_id, "resume_sandbox: success");
    state
        .logger
        .log(LogEvent::new(LogLevel::Info, "sandbox.resumed").field("sandbox_id", &sandbox_id))
        .await;

    Ok((StatusCode::CREATED, Json(sandbox)))
}

// ─── POST /sandboxes/:sandboxID/connect ───────────────────────────────────────

pub async fn connect_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Json(body): Json<ConnectSandbox>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "connect_sandbox")
                .field("sandbox_id", &sandbox_id)
                .field_value("timeout", body.timeout),
        )
        .await;
    tracing::info!("connect request");
    let sandbox = state
        .services
        .sandboxes
        .connect_sandbox(&sandbox_id, body.timeout)
        .await?;
    Ok((StatusCode::OK, Json(sandbox)))
}

// ─── GET /sandboxes/:sandboxID/logs ───────────────────────────────────────────

pub async fn get_sandbox_logs(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Query(params): Query<SandboxLogsQuery>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "get_sandbox_logs")
                .field("sandbox_id", &sandbox_id)
                .field_value("limit", params.limit),
        )
        .await;

    let logs = state
        .services
        .sandboxes
        .get_logs(&sandbox_id, params.start, params.limit)
        .await?;
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "api.response")
                .field("handler", "get_sandbox_logs")
                .field("sandbox_id", &sandbox_id)
                .field_value("count", logs.logs.len()),
        )
        .await;
    Ok(Json(logs))
}

// ─── GET /v2/sandboxes/:sandboxID/logs ────────────────────────────────────────

#[utoipa::path(
    get,
    path = "/v2/sandboxes/{sandboxID}/logs",
    params(
        ("sandboxID" = String, Path, description = "Sandbox identifier"),
        SandboxLogsV2Query
    ),
    responses(
        (status = 200, description = "Structured sandbox logs", body = SandboxLogsV2Response),
        (status = 404, description = "Sandbox not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn get_sandbox_logs_v2(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Query(params): Query<SandboxLogsV2Query>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "get_sandbox_logs_v2")
                .field("sandbox_id", &sandbox_id)
                .field_value("limit", params.limit),
        )
        .await;

    let logs = state
        .services
        .sandboxes
        .get_logs_v2(&sandbox_id, params.cursor, params.limit)
        .await?;
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "api.response")
                .field("handler", "get_sandbox_logs_v2")
                .field("sandbox_id", &sandbox_id)
                .field_value("count", logs.logs.len()),
        )
        .await;
    Ok(Json(logs))
}

// ─── POST /sandboxes/:sandboxID/timeout ───────────────────────────────────────

pub async fn set_sandbox_timeout(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Json(body): Json<SetTimeoutRequest>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "set_sandbox_timeout")
                .field("sandbox_id", &sandbox_id)
                .field_value("timeout", body.timeout),
        )
        .await;

    state
        .services
        .sandboxes
        .set_timeout(&sandbox_id, body.timeout)
        .await?;

    tracing::info!(sandbox_id = %sandbox_id, timeout = body.timeout, "set_sandbox_timeout: success");
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "sandbox.timeout.updated")
                .field("sandbox_id", &sandbox_id)
                .field_value("timeout", body.timeout),
        )
        .await;
    Ok(StatusCode::NO_CONTENT)
}

// ─── POST /sandboxes/:sandboxID/refreshes ─────────────────────────────────────

pub async fn refresh_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Json(body): Json<RefreshRequest>,
) -> AppResult<impl IntoResponse> {
    let duration = body.duration.unwrap_or(0);
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "refresh_sandbox")
                .field("sandbox_id", &sandbox_id)
                .field_value("duration", duration),
        )
        .await;

    state
        .services
        .sandboxes
        .refresh(&sandbox_id, duration)
        .await?;

    tracing::info!(sandbox_id = %sandbox_id, duration = duration, "refresh_sandbox: success");
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "sandbox.refreshed")
                .field("sandbox_id", &sandbox_id)
                .field_value("duration", duration),
        )
        .await;
    Ok(StatusCode::NO_CONTENT)
}

// ─── POST /sandboxes/:sandboxID/exec-code ────────────────────────────────────

#[utoipa::path(
    post,
    path = "/sandboxes/{sandboxID}/exec-code",
    params(
        ("sandboxID" = String, Path, description = "Sandbox identifier")
    ),
    request_body = ExecCodeRequest,
    responses(
        (status = 200, description = "Code execution result", body = ExecCodeResponse),
        (status = 400, description = "Invalid request", body = ApiError),
        (status = 404, description = "Sandbox not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn exec_code(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Json(body): Json<ExecCodeRequest>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "exec_code")
                .field("sandbox_id", &sandbox_id)
                .field("language", &body.language),
        )
        .await;

    // Resolve sandbox to obtain domain
    let detail = state.services.sandboxes.get_sandbox(&sandbox_id).await?;
    let domain = detail
        .domain
        .unwrap_or_else(|| state.config.sandbox_domain.clone());

    let timeout_secs = body.timeout_secs.unwrap_or(30).clamp(1, 300);

    let start = Instant::now();
    let resp = match body.language.as_str() {
        "python" => {
            // Try the Jupyter kernel (port 49999) first for stateful
            // execution — variables persist across calls.  If Jupyter is
            // unavailable (e.g. 502 from the proxy because the sandbox
            // image lacks a Jupyter service), fall back to envd one-shot
            // Python execution so the endpoint still works.
            let jupyter_result =
                run_jupyter_execute(&state, &sandbox_id, &domain, &body.code).await;

            let output = match jupyter_result {
                Ok(out) => out,
                Err(jupyter_err) => {
                    tracing::warn!(
                        sandbox_id = %sandbox_id,
                        error = %jupyter_err,
                        "Jupyter execute failed, falling back to envd one-shot Python execution"
                    );
                    state
                        .logger
                        .log(
                            LogEvent::new(LogLevel::Warn, "sandbox.exec_code.jupyter_fallback")
                                .field("sandbox_id", &sandbox_id)
                                .field("error", &jupyter_err.to_string()),
                        )
                        .await;

                    // Fallback: run Python via envd Process/Start (one-shot, no
                    // state persistence across calls).
                    let req = serde_json::json!({
                        "process": {
                            "cmd": "python3",
                            "args": ["-c", &body.code],
                            "envs": {},
                            "cwd": "/root"
                        },
                        "stdin": false
                    });
                    let cmd_out = run_envd_command(&state, &sandbox_id, &domain, req).await?;
                    JupyterOutput {
                        exit_code: cmd_out.exit_code,
                        stdout: cmd_out.stdout,
                        stderr: cmd_out.stderr,
                        results: None,
                    }
                }
            };

            let elapsed_ms = start.elapsed().as_millis() as u64;

            if elapsed_ms > (timeout_secs as u64) * 1000 && output.exit_code == 0 {
                ExecCodeResponse {
                    stdout: output.stdout,
                    stderr: output.stderr,
                    exit_code: -1,
                    success: false,
                    elapsed_ms,
                    results: output.results,
                }
            } else {
                ExecCodeResponse {
                    stdout: output.stdout,
                    stderr: output.stderr,
                    exit_code: output.exit_code,
                    success: output.exit_code == 0,
                    elapsed_ms,
                    results: output.results,
                }
            }
        }
        "bash" => {
            // Route Bash through envd Process/Start (port 49983) for
            // one-shot shell command execution.
            let req = serde_json::json!({
                "process": {
                    "cmd": "bash",
                    "args": ["-c", &body.code],
                    "envs": {},
                    "cwd": "/root"
                },
                "stdin": false
            });

            let output = run_envd_command(&state, &sandbox_id, &domain, req).await?;
            let elapsed_ms = start.elapsed().as_millis() as u64;

            if elapsed_ms > (timeout_secs as u64) * 1000 && output.exit_code == 0 {
                ExecCodeResponse {
                    stdout: output.stdout,
                    stderr: output.stderr,
                    exit_code: -1,
                    success: false,
                    elapsed_ms,
                    results: None,
                }
            } else {
                ExecCodeResponse {
                    stdout: output.stdout,
                    stderr: output.stderr,
                    exit_code: output.exit_code,
                    success: output.exit_code == 0,
                    elapsed_ms,
                    results: None,
                }
            }
        }
        other => {
            return Err(AppError::BadRequest(format!(
                "unsupported language: {}",
                other
            )));
        }
    };

    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "sandbox.exec_code")
                .field("sandbox_id", &sandbox_id)
                .field("language", &body.language)
                .field_value("exit_code", resp.exit_code)
                .field_value("elapsed_ms", resp.elapsed_ms),
        )
        .await;

    Ok(Json(resp))
}

// ─── envd communication helpers ──────────────────────────────────────────────

#[derive(Default)]
struct CommandOutput {
    exit_code: i32,
    stdout: String,
    stderr: String,
}

#[derive(Default)]
struct JupyterOutput {
    exit_code: i32,
    stdout: String,
    stderr: String,
    results: Option<Vec<serde_json::Value>>,
}

async fn run_jupyter_execute(
    state: &AppState,
    sandbox_id: &str,
    domain: &str,
    code: &str,
) -> AppResult<JupyterOutput> {
    let host = format!("{}-{}.{}", JUPYTER_PORT, sandbox_id, domain);
    let base_url = std::env::var("AGENTHUB_SANDBOX_PROXY_URL")
        .unwrap_or_else(|_| "http://127.0.0.1".to_string());
    let url = format!("{}/execute", base_url.trim_end_matches('/'));

    let payload = serde_json::json!({
        "code": code,
        "language": "python"
    });

    let resp = state
        .http_client
        .post(url)
        .header("Host", host)
        .header("Content-Type", "application/json")
        .header("Authorization", "Basic cm9vdDo=")
        .json(&payload)
        .send()
        .await
        .map_err(|e| {
            AppError::Internal(anyhow::anyhow!("jupyter execute request failed: {}", e))
        })?;

    if !resp.status().is_success() {
        return Err(AppError::Internal(anyhow::anyhow!(
            "jupyter execute request returned HTTP {}",
            resp.status()
        )));
    }

    let body = resp.bytes().await.map_err(|e| {
        AppError::Internal(anyhow::anyhow!(
            "failed reading jupyter execute response: {}",
            e
        ))
    })?;

    parse_jupyter_ndjson(&body)
}

/// Parse the ndjson stream returned by the Jupyter `/execute` endpoint.
///
/// Each line is a JSON object with a `type` field:
/// - `stdout` → `{ "type": "stdout", "text": "..." }`
/// - `stderr` → `{ "type": "stderr", "text": "..." }`
/// - `result` → `{ "type": "result", "text": "...", ... }`
/// - `error`  → `{ "type": "error", "name": "...", "value": "...", "traceback": [...] }`
fn parse_jupyter_ndjson(bytes: &[u8]) -> AppResult<JupyterOutput> {
    let mut out = JupyterOutput::default();
    let text = std::str::from_utf8(bytes)
        .map_err(|e| AppError::Internal(anyhow::anyhow!("jupyter response is not UTF-8: {}", e)))?;

    for line in text.lines() {
        if line.trim().is_empty() {
            continue;
        }
        let v: serde_json::Value = serde_json::from_str(line).map_err(|e| {
            AppError::Internal(anyhow::anyhow!("invalid jupyter ndjson line: {}", e))
        })?;

        let event_type = v.get("type").and_then(|t| t.as_str()).unwrap_or("");

        match event_type {
            "stdout" => {
                if let Some(text) = v.get("text").and_then(|t| t.as_str()) {
                    out.stdout.push_str(text);
                    if !text.ends_with('\n') {
                        out.stdout.push('\n');
                    }
                }
            }
            "stderr" => {
                if let Some(text) = v.get("text").and_then(|t| t.as_str()) {
                    out.stderr.push_str(text);
                    if !text.ends_with('\n') {
                        out.stderr.push('\n');
                    }
                }
            }
            "result" => {
                if out.results.is_none() {
                    out.results = Some(Vec::new());
                }
                if let Some(results) = out.results.as_mut() {
                    results.push(v);
                }
            }
            "error" => {
                out.exit_code = 1;
                let name = v.get("name").and_then(|v| v.as_str()).unwrap_or("Error");
                let value = v.get("value").and_then(|v| v.as_str()).unwrap_or("");
                let traceback = v
                    .get("traceback")
                    .and_then(|t| t.as_array())
                    .map(|arr| {
                        arr.iter()
                            .filter_map(|v| v.as_str())
                            .collect::<Vec<_>>()
                            .join("\n")
                    })
                    .unwrap_or_default();
                if !out.stderr.is_empty() {
                    out.stderr.push('\n');
                }
                out.stderr.push_str(&format!("{}: {}", name, value));
                if !traceback.is_empty() {
                    out.stderr.push('\n');
                    out.stderr.push_str(&traceback);
                }
            }
            _ => {}
        }
    }

    // If no error event was seen, exit_code stays 0 (the default)
    Ok(out)
}

async fn run_envd_command(
    state: &AppState,
    sandbox_id: &str,
    domain: &str,
    req: Value,
) -> AppResult<CommandOutput> {
    let host = format!("{}-{}.{}", ENVD_PORT, sandbox_id, domain);
    let url = std::env::var("AGENTHUB_SANDBOX_PROXY_URL")
        .unwrap_or_else(|_| "http://127.0.0.1".to_string());
    let url = format!("{}/process.Process/Start", url.trim_end_matches('/'));

    let body = connect_envelope(&serde_json::to_vec(&req).map_err(anyhow::Error::from)?);
    let resp = state
        .http_client
        .post(url)
        .header("Host", host)
        .header("Content-Type", CONNECT_JSON)
        .header("Authorization", "Basic cm9vdDo=")
        .body(body)
        .send()
        .await
        .map_err(|e| AppError::Internal(anyhow::anyhow!("envd command request failed: {}", e)))?;

    if !resp.status().is_success() {
        return Err(AppError::Internal(anyhow::anyhow!(
            "envd command request returned HTTP {}",
            resp.status()
        )));
    }

    let bytes = resp.bytes().await.map_err(|e| {
        AppError::Internal(anyhow::anyhow!("failed reading envd command stream: {}", e))
    })?;
    parse_connect_stream(&bytes)
}

fn connect_envelope(payload: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(payload.len() + 5);
    out.push(0);
    out.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    out.extend_from_slice(payload);
    out
}

fn parse_connect_stream(bytes: &[u8]) -> AppResult<CommandOutput> {
    let mut out = CommandOutput::default();
    let mut i = 0usize;

    while i + 5 <= bytes.len() {
        let flags = bytes[i];
        let len =
            u32::from_be_bytes([bytes[i + 1], bytes[i + 2], bytes[i + 3], bytes[i + 4]]) as usize;
        i += 5;
        if i + len > bytes.len() {
            return Err(AppError::Internal(anyhow::anyhow!(
                "truncated envd command stream"
            )));
        }
        let payload = &bytes[i..i + len];
        i += len;

        let v: Value = serde_json::from_slice(payload)
            .map_err(|e| AppError::Internal(anyhow::anyhow!("invalid envd JSON event: {}", e)))?;

        if flags & 0b10 != 0 {
            if v.get("error").is_some() {
                return Err(AppError::Internal(anyhow::anyhow!(
                    "envd command error: {}",
                    v
                )));
            }
            continue;
        }

        let Some(event) = v.get("event") else {
            continue;
        };
        if let Some(data) = event.get("data") {
            if let Some(stdout) = data.get("stdout").and_then(Value::as_str) {
                out.stdout.push_str(&decode_b64_lossy(stdout));
            }
            if let Some(stderr) = data.get("stderr").and_then(Value::as_str) {
                out.stderr.push_str(&decode_b64_lossy(stderr));
            }
        }
        if let Some(end) = event.get("end") {
            out.exit_code = end
                .get("exitCode")
                .and_then(Value::as_i64)
                .or_else(|| parse_exit_status(end.get("status").and_then(Value::as_str)))
                .unwrap_or_default() as i32;
        }
    }

    Ok(out)
}

fn decode_b64_lossy(s: &str) -> String {
    BASE64
        .decode(s)
        .map(|b| String::from_utf8_lossy(&b).into_owned())
        .unwrap_or_default()
}

fn parse_exit_status(status: Option<&str>) -> Option<i64> {
    let status = status?;
    status
        .strip_prefix("exit status ")
        .and_then(|v| v.trim().parse::<i64>().ok())
}

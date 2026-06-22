// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! `POST /cube/sandboxes/{sandboxID}/exec-code`
//!
//! Cube-specific extension (NOT part of the e2b API). Executes a snippet of
//! Python or Bash inside a running sandbox. Python is routed through the
//! sandbox's Jupyter kernel (stateful) with a fallback to an envd one-shot
//! process; Bash always goes through envd `Process/Start`.

use std::time::Instant;

use axum::{
    extract::{Path, State},
    response::IntoResponse,
    Json,
};
use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use serde_json::Value;

use crate::{
    error::{AppError, AppResult},
    logging::{LogEvent, LogLevel},
    models::{ExecCodeRequest, ExecCodeResponse},
    state::AppState,
};

const ENVD_PORT: u16 = 49983;
const JUPYTER_PORT: u16 = 49999;
const CONNECT_JSON: &str = "application/connect+json";

#[utoipa::path(
    post,
    path = "/cube/sandboxes/{sandboxID}/exec-code",
    params(
        ("sandboxID" = String, Path, description = "Sandbox identifier")
    ),
    request_body = ExecCodeRequest,
    responses(
        (status = 200, description = "Code execution result", body = ExecCodeResponse),
        (status = 400, description = "Invalid request", body = crate::models::ApiError),
        (status = 404, description = "Sandbox not found", body = crate::models::ApiError),
        (status = 500, description = "Unexpected backend error", body = crate::models::ApiError)
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

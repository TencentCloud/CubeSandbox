// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Sandbox code-execution service.
//!
//! Encapsulates all communication with the in-sandbox envd / Jupyter
//! endpoints behind the Cube-specific `exec-code` extension: HTTP transport,
//! Connect-stream framing, ndjson parsing and base64 decoding. Handlers stay
//! thin and delegate the actual work here.

use std::time::Instant;

use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use serde_json::Value;
use tokio::time::{timeout, Duration};

use crate::{
    error::{AppError, AppResult},
    models::{ExecCodeRequest, ExecCodeResponse},
};

const ENVD_PORT: u16 = 49983;
const JUPYTER_PORT: u16 = 49999;
const CONNECT_JSON: &str = "application/connect+json";

/// Runs user-provided code inside a sandbox via envd / Jupyter.
///
/// Holds the shared HTTP client, the sandbox proxy base URL and the internal
/// envd `Authorization` header value (sourced from config / env, never
/// hardcoded).
#[derive(Clone)]
pub struct ExecService {
    http_client: reqwest::Client,
    proxy_url: String,
    envd_auth: String,
}

impl ExecService {
    pub fn new(http_client: reqwest::Client, proxy_url: String, envd_auth: String) -> Self {
        Self {
            http_client,
            proxy_url,
            envd_auth,
        }
    }

    /// Execute a Python or Bash snippet inside the given sandbox and return a
    /// normalised [`ExecCodeResponse`].
    ///
    /// Python is routed through the sandbox's Jupyter kernel (stateful) with a
    /// fallback to an envd one-shot process; Bash always goes through envd
    /// `Process/Start`.
    pub async fn exec_code(
        &self,
        sandbox_id: &str,
        domain: &str,
        req: &ExecCodeRequest,
    ) -> AppResult<ExecCodeResponse> {
        let timeout_secs = req.timeout_secs.unwrap_or(30).clamp(1, 300) as u64;
        let timeout_ms = timeout_secs * 1000;
        let start = Instant::now();

        let exec_future = async {
            match req.language.as_str() {
                "python" => self.exec_python(sandbox_id, domain, &req.code).await,
                "bash" => {
                    let cmd = self.exec_bash(sandbox_id, domain, &req.code).await?;
                    Ok(JupyterOutput {
                        exit_code: cmd.exit_code,
                        stdout: cmd.stdout,
                        stderr: cmd.stderr,
                        results: None,
                    })
                }
                other => Err(AppError::BadRequest(format!(
                    "unsupported language: {}",
                    other
                ))),
            }
        };

        let output = match timeout(Duration::from_secs(timeout_secs), exec_future).await {
            Ok(result) => result?,
            Err(_elapsed) => {
                return Ok(ExecCodeResponse {
                    stdout: String::new(),
                    stderr: format!("execution timed out after {}s", timeout_secs),
                    exit_code: -1,
                    success: false,
                    elapsed_ms: start.elapsed().as_millis() as u64,
                    results: None,
                });
            }
        };

        let elapsed_ms = start.elapsed().as_millis() as u64;
        Ok(build_response(output, elapsed_ms, timeout_ms))
    }

    /// Run Python through the Jupyter kernel, falling back to an envd one-shot
    /// process when Jupyter is unavailable (e.g. the sandbox image ships no
    /// Jupyter service and the proxy returns 502).
    async fn exec_python(
        &self,
        sandbox_id: &str,
        domain: &str,
        code: &str,
    ) -> AppResult<JupyterOutput> {
        match self.run_jupyter_execute(sandbox_id, domain, code).await {
            Ok(out) => Ok(out),
            Err(jupyter_err) => {
                tracing::warn!(
                    sandbox_id = %sandbox_id,
                    error = %jupyter_err,
                    "Jupyter execute failed, falling back to envd one-shot Python execution"
                );

                // Fallback: run Python via envd Process/Start (one-shot, no
                // state persistence across calls).
                let req = serde_json::json!({
                    "process": {
                        "cmd": "python3",
                        "args": ["-c", code],
                        "envs": {},
                        "cwd": "/root"
                    },
                    "stdin": false
                });
                let cmd_out = self.run_envd_command(sandbox_id, domain, req).await?;
                Ok(JupyterOutput {
                    exit_code: cmd_out.exit_code,
                    stdout: cmd_out.stdout,
                    stderr: cmd_out.stderr,
                    results: None,
                })
            }
        }
    }

    /// Run Bash through envd `Process/Start` (one-shot shell command).
    async fn exec_bash(
        &self,
        sandbox_id: &str,
        domain: &str,
        code: &str,
    ) -> AppResult<CommandOutput> {
        let req = serde_json::json!({
            "process": {
                "cmd": "bash",
                "args": ["-c", code],
                "envs": {},
                "cwd": "/root"
            },
            "stdin": false
        });
        self.run_envd_command(sandbox_id, domain, req).await
    }

    async fn run_jupyter_execute(
        &self,
        sandbox_id: &str,
        domain: &str,
        code: &str,
    ) -> AppResult<JupyterOutput> {
        let host = format!("{}-{}.{}", JUPYTER_PORT, sandbox_id, domain);
        let url = format!("{}/execute", self.proxy_url.trim_end_matches('/'));

        let payload = serde_json::json!({
            "code": code,
            "language": "python"
        });

        let resp = self
            .http_client
            .post(url)
            .header("Host", host)
            .header("Content-Type", "application/json")
            .header("Authorization", &self.envd_auth)
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

    async fn run_envd_command(
        &self,
        sandbox_id: &str,
        domain: &str,
        req: Value,
    ) -> AppResult<CommandOutput> {
        let host = format!("{}-{}.{}", ENVD_PORT, sandbox_id, domain);
        let url = format!(
            "{}/process.Process/Start",
            self.proxy_url.trim_end_matches('/')
        );

        let body = connect_envelope(&serde_json::to_vec(&req).map_err(anyhow::Error::from)?);
        let resp = self
            .http_client
            .post(url)
            .header("Host", host)
            .header("Content-Type", CONNECT_JSON)
            .header("Authorization", &self.envd_auth)
            .body(body)
            .send()
            .await
            .map_err(|e| {
                AppError::Internal(anyhow::anyhow!("envd command request failed: {}", e))
            })?;

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
}

// ─── output models ───────────────────────────────────────────────────────────

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

/// Normalise raw execution output into the public response, applying the
/// timeout rule: a wall-clock overrun with a success exit code is reported as
/// a failure with exit code `-1`.
fn build_response(output: JupyterOutput, elapsed_ms: u64, timeout_ms: u64) -> ExecCodeResponse {
    if elapsed_ms > timeout_ms && output.exit_code == 0 {
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

// ─── parsing helpers ─────────────────────────────────────────────────────────

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

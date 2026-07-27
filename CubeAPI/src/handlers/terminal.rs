// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// Interactive Web Terminal for running sandboxes.
//
// `GET /sandboxes/:sandboxID/terminal` upgrades to a WebSocket and bridges
// the browser to an interactive PTY inside the sandbox. The PTY is provided
// by envd's `process.Process` Connect-JSON RPC — the exact same data plane
// the SDKs (`sdk/node/src/pty.ts`, `sdk/python/cubesandbox/_pty.py`) and the
// AgentHub setup path (`run_envd_command`) already use — reached through the
// sandbox proxy (`AGENTHUB_SANDBOX_PROXY_URL` + `Host: 49983-<id>.<domain>`).
// No new execution mechanism is introduced.
//
// Wire protocol towards the browser (JSON text frames):
//
//   client → server: {"type":"input","data":"<base64>"}
//                    {"type":"resize","cols":120,"rows":32}
//                    {"type":"ping"}
//   server → client: {"type":"ready","pid":42}
//                    {"type":"output","data":"<base64>"}
//                    {"type":"exit","exitCode":0}
//                    {"type":"warning","code":"bad_input","message":"..."}
//                    {"type":"error","code":"idle_timeout","message":"..."}
//                    {"type":"pong"}
//
// `warning` frames are non-fatal (the session stays open); `error` frames are
// terminal. Raw binary WebSocket frames are also accepted as terminal input,
// so thin clients can skip the JSON framing for the hot path.
//
// Authentication. Browsers cannot attach headers to a WebSocket upgrade and
// anything in the WebSocket URL is logged by proxies/load balancers, so the
// WebUI first calls `POST .../terminal/ticket` (authenticated like every
// other REST route) to mint a short-lived, single-use ticket, then opens the
// socket with `?ticket=...`. Non-browser clients may instead present the
// usual `Authorization: Bearer` / `X-API-Key` / `X-Session-Token` headers
// directly on the upgrade. Both the optional auth callback and the optional
// WebUI session store are enforced. Every session start/close is written to
// the structured audit log (`terminal.session.started` /
// `terminal.session.closed`).

use std::sync::Arc;
use std::time::Duration;

use axum::{
    extract::{
        ws::{Message, WebSocket},
        OriginalUri, Path, Query, State, WebSocketUpgrade,
    },
    http::HeaderMap,
    response::Response,
    Json,
};
use base64::{engine::general_purpose::STANDARD as BASE64, Engine};
use dashmap::DashMap;
use futures::{FutureExt, StreamExt};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use tokio::sync::OwnedSemaphorePermit;
use tokio::time::Instant;

use crate::{
    error::{AppError, AppResult},
    logging::{LogEvent, LogLevel},
    models::SandboxState,
    state::AppState,
};

const ENVD_PORT: u16 = 49983;
const CONNECT_JSON: &str = "application/connect+json";
const CONNECT_END_STREAM_FLAG: u8 = 0b10;
/// SIGKILL constant of envd's `SendSignal` RPC.
const SIGNAL_SIGKILL: &str = "SIGNAL_SIGKILL";
/// How long to wait for envd's start event before giving up on the session.
const START_TIMEOUT: Duration = Duration::from_secs(15);
/// Cadence of the idle-timeout sweep.
const IDLE_SWEEP_INTERVAL: Duration = Duration::from_secs(30);
/// Bounds for the client-requested terminal geometry.
const MAX_ROWS: u16 = 512;
const MAX_COLS: u16 = 1024;
/// Largest single Connect frame the decoder will buffer from the sandbox
/// stream. The length prefix is attacker-influenceable via the proxy path, so
/// a frame claiming more than this is rejected instead of allocated.
const MAX_CONNECT_FRAME_BYTES: usize = 1 << 20; // 1 MiB
/// Generic message sent to the browser when the sandbox backend fails, so raw
/// envd/proxy error detail is logged server-side but never leaked to the UI.
const GENERIC_BACKEND_ERROR: &str = "terminal backend error";

// ─── One-time tickets ────────────────────────────────────────────────────────

/// A single-use authorization for a terminal WebSocket upgrade. Minted by the
/// authenticated `terminal_ticket` handler and consumed once by `terminal_ws`.
#[derive(Debug, Clone)]
pub struct TerminalTicket {
    sandbox_id: String,
    operator: String,
    guest_user: String,
    expires_at: Instant,
}

/// Process-wide store of outstanding terminal tickets, keyed by ticket id.
#[derive(Clone, Default)]
pub struct TerminalTickets(Arc<DashMap<String, TerminalTicket>>);

impl TerminalTickets {
    fn insert(&self, id: String, ticket: TerminalTicket) {
        self.0.insert(id, ticket);
    }

    /// Remove and return the ticket, dropping it if it has expired. Also
    /// opportunistically evicts other expired tickets so the map cannot grow
    /// without bound when sockets are never opened.
    fn take_valid(&self, id: &str) -> Option<TerminalTicket> {
        let now = Instant::now();
        self.0.retain(|_, t| t.expires_at > now);
        self.0.remove(id).map(|(_, t)| t).filter(|t| t.expires_at > now)
    }
}

// ─── Query / wire messages ──────────────────────────────────────────────────

#[derive(Debug, Default, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TerminalQuery {
    /// One-time ticket minted by `POST .../terminal/ticket`. Preferred for
    /// browsers, which cannot set auth headers on a WebSocket upgrade.
    pub ticket: Option<String>,
    /// Initial terminal size; defaults to 80x24, clamped server-side.
    pub cols: Option<u16>,
    pub rows: Option<u16>,
    /// Sandbox user the shell runs as (envd user), default `root`.
    /// Ignored when a ticket is used (the ticket carries the user).
    pub user: Option<String>,
}

/// Query for `POST .../terminal/ticket`.
#[derive(Debug, Default, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TicketQuery {
    /// Sandbox user the shell should run as (envd user), default `root`.
    pub user: Option<String>,
}

/// Response of `POST .../terminal/ticket`.
#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TicketResponse {
    pub ticket: String,
    pub expires_in_secs: u64,
}

#[derive(Debug, Deserialize, PartialEq)]
#[serde(tag = "type", rename_all = "snake_case")]
enum ClientMessage {
    /// Base64-encoded bytes for the PTY master (keystrokes, paste, …).
    Input { data: String },
    /// Terminal geometry changed (xterm fit addon / window resize).
    Resize { cols: u16, rows: u16 },
    /// Keep-alive; answered with `pong`. Does not reset the idle timer.
    Ping,
}

#[derive(Debug, Serialize, PartialEq)]
#[serde(tag = "type", rename_all = "snake_case")]
enum ServerMessage {
    /// PTY is running; `pid` identifies it inside the sandbox.
    Ready {
        pid: u32,
    },
    /// Base64-encoded PTY output.
    Output {
        data: String,
    },
    /// The shell exited on its own (e.g. `exit`, `Ctrl-D`).
    #[serde(rename_all = "camelCase")]
    Exit {
        exit_code: Option<i64>,
    },
    /// Non-fatal notice (e.g. a bad input frame was ignored). The session
    /// stays open; the client should surface it without tearing down.
    Warning {
        code: String,
        message: String,
    },
    /// Terminal session ends abnormally; `code` is machine-readable.
    Error {
        code: String,
        message: String,
    },
    Pong,
}

impl ServerMessage {
    fn to_ws(&self) -> Message {
        // These variants always serialize; fall back to a fixed error frame
        // rather than panicking so the bridge task can never abort mid-session.
        Message::Text(serde_json::to_string(self).unwrap_or_else(|_| {
            r#"{"type":"error","code":"internal","message":"terminal backend error"}"#.to_string()
        }))
    }
}

// ─── HTTP entry points ───────────────────────────────────────────────────────

/// `POST /sandboxes/:sandboxID/terminal/ticket` — authenticate the caller and
/// mint a short-lived, single-use ticket for the terminal WebSocket. This lets
/// the browser keep its real credentials in headers (a normal `fetch`) instead
/// of putting them in the WebSocket URL, which proxies and load balancers log.
pub async fn terminal_ticket(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Query(query): Query<TicketQuery>,
    OriginalUri(uri): OriginalUri,
    headers: HeaderMap,
) -> AppResult<Json<TicketResponse>> {
    validate_sandbox_id(&sandbox_id)?;
    let guest_user = validate_guest_user(query.user.as_deref())?;
    let operator = authorize(&state, &headers, uri.path()).await?;
    ensure_running(&state, &sandbox_id, &operator).await?;

    let id = uuid::Uuid::new_v4().simple().to_string();
    let ttl = state.config.terminal_ticket_ttl_secs.max(1);
    state.terminal_tickets.insert(
        id.clone(),
        TerminalTicket {
            sandbox_id,
            operator,
            guest_user,
            expires_at: Instant::now() + Duration::from_secs(ttl),
        },
    );
    Ok(Json(TicketResponse {
        ticket: id,
        expires_in_secs: ttl,
    }))
}

/// `GET /sandboxes/:sandboxID/terminal` — authorized WebSocket upgrade to an
/// interactive shell in a *running* sandbox. Authorization is either a
/// one-time `?ticket=` (browser path) or request headers (non-browser path).
pub async fn terminal_ws(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Query(query): Query<TerminalQuery>,
    OriginalUri(uri): OriginalUri,
    headers: HeaderMap,
    ws: WebSocketUpgrade,
) -> AppResult<Response> {
    validate_sandbox_id(&sandbox_id)?;

    // Resolve the operator + guest user from a ticket when present, otherwise
    // fall back to header credentials. A ticket is bound to one sandbox.
    let (operator, guest_user) = match query.ticket.as_deref() {
        Some(raw) => {
            let ticket = state
                .terminal_tickets
                .take_valid(raw.trim())
                .filter(|t| t.sandbox_id == sandbox_id)
                .ok_or_else(|| {
                    AppError::Unauthorized("invalid or expired terminal ticket".to_string())
                })?;
            (ticket.operator, ticket.guest_user)
        }
        None => {
            let operator = authorize(&state, &headers, uri.path()).await?;
            (operator, validate_guest_user(query.user.as_deref())?)
        }
    };

    let detail = ensure_running(&state, &sandbox_id, &operator).await?;
    let domain = detail
        .domain
        .filter(|d| !d.trim().is_empty())
        .unwrap_or_else(|| state.config.sandbox_domain.clone());

    // Bound the number of concurrent terminals server-wide. The permit is
    // moved into the session and released when the socket closes.
    let permit = match state.terminal_sessions.clone().try_acquire_owned() {
        Ok(permit) => permit,
        Err(_) => {
            state
                .logger
                .log(
                    LogEvent::new(LogLevel::Warn, "terminal.session.rejected")
                        .field("sandbox_id", &sandbox_id)
                        .field("operator", &operator)
                        .field("reason", "max_sessions"),
                )
                .await;
            return Err(AppError::ServiceUnavailable {
                message: "too many active terminal sessions; try again shortly".to_string(),
                retry_after: 5,
            });
        }
    };

    let size = PtySize {
        rows: query.rows.unwrap_or(24).clamp(1, MAX_ROWS),
        cols: query.cols.unwrap_or(80).clamp(1, MAX_COLS),
    };
    let session_id = uuid::Uuid::new_v4().simple().to_string();
    let idle_timeout = Duration::from_secs(state.config.terminal_idle_timeout_secs.max(30));

    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "terminal.session.started")
                .field("session_id", &session_id)
                .field("sandbox_id", &sandbox_id)
                .field("operator", &operator)
                .field("guest_user", &guest_user)
                .field_value("cols", size.cols)
                .field_value("rows", size.rows),
        )
        .await;

    let ctx = SessionContext {
        state: state.clone(),
        session_id,
        sandbox_id,
        domain,
        operator,
        guest_user,
        size,
        idle_timeout,
        _permit: permit,
    };
    Ok(ws.on_upgrade(move |socket| run_session(socket, ctx)))
}

/// Fetch the sandbox and require it to be running, auditing rejections.
async fn ensure_running(
    state: &AppState,
    sandbox_id: &str,
    operator: &str,
) -> AppResult<crate::models::SandboxDetail> {
    let detail = state.services.sandboxes.get_sandbox(sandbox_id).await?;
    if detail.state != SandboxState::Running {
        state
            .logger
            .log(
                LogEvent::new(LogLevel::Warn, "terminal.session.rejected")
                    .field("sandbox_id", sandbox_id)
                    .field("operator", operator)
                    .field_value("state", &detail.state),
            )
            .await;
        return Err(AppError::Conflict(format!(
            "sandbox {} is not running (state: {:?}); terminal login requires a running sandbox",
            sandbox_id, detail.state
        )));
    }
    Ok(detail)
}

fn validate_sandbox_id(id: &str) -> AppResult<()> {
    let ok = !id.is_empty()
        && id.len() <= 128
        && id
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_');
    if ok {
        Ok(())
    } else {
        // The id is embedded into the proxy `Host` header — reject anything
        // that could smuggle separators, before touching any backend.
        Err(AppError::BadRequest("invalid sandbox id".to_string()))
    }
}

fn validate_guest_user(user: Option<&str>) -> AppResult<String> {
    let user = user.unwrap_or("root").trim();
    let ok = !user.is_empty()
        && user.len() <= 32
        && user
            .chars()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '_' || c == '-');
    if ok {
        Ok(user.to_string())
    } else {
        Err(AppError::BadRequest(
            "invalid sandbox user; expected a lowercase unix user name".to_string(),
        ))
    }
}

// ─── Authentication ──────────────────────────────────────────────────────────

/// Enforce every configured auth mechanism and return the operator identity
/// for the audit trail. Credentials come from request *headers* only — the
/// browser path authenticates the ticket-mint call (a normal `fetch`), so no
/// credential ever needs to travel in a WebSocket URL.
///
/// - When `auth_callback_url` is configured, a Bearer token or API key
///   (`Authorization: Bearer` / `X-API-Key`) is required and validated via the
///   callback, mirroring `middleware::auth::unified_auth`.
/// - When the WebUI session store (database) is configured, a valid
///   `X-Session-Token` is required and resolves to the logged-in username.
/// - With neither configured the platform runs open (same posture as every
///   other route) and the operator is recorded as `anonymous`.
async fn authorize(state: &AppState, headers: &HeaderMap, request_path: &str) -> AppResult<String> {
    let mut operator: Option<String> = None;

    if let Some(callback_url) = state
        .config
        .auth_callback_url
        .as_deref()
        .filter(|u| !u.is_empty())
    {
        let credential = extract_api_credential(headers).ok_or_else(|| {
            AppError::Unauthorized(
                "Missing authentication: provide an 'Authorization: Bearer <token>' or \
                 'X-API-Key: <key>' header"
                    .to_string(),
            )
        })?;

        let req = state
            .http_client
            .post(callback_url)
            .header("X-Request-Path", request_path)
            .header("X-Request-Method", "GET");
        let req = match &credential {
            ApiCredential::Bearer(token) => {
                req.header("Authorization", format!("Bearer {}", token))
            }
            ApiCredential::ApiKey(key) => req.header("X-API-Key", key.as_str()),
        };
        let resp = req
            .send()
            .await
            .map_err(|e| AppError::Internal(anyhow::anyhow!("Auth callback unreachable: {}", e)))?;
        if resp.status().as_u16() != 200 {
            return Err(AppError::Unauthorized(
                "Authentication rejected by callback".to_string(),
            ));
        }
        operator = Some(
            match credential {
                ApiCredential::Bearer(_) => "bearer",
                ApiCredential::ApiKey(_) => "api-key",
            }
            .to_string(),
        );
    }

    if let Some(store) = &state.agenthub_store {
        let token = session_token(headers).ok_or_else(|| {
            AppError::Unauthorized(
                "terminal login requires a WebUI session; send 'X-Session-Token'".to_string(),
            )
        })?;
        let username = store
            .validate_session(&token)
            .await
            .map_err(|e| AppError::Internal(anyhow::anyhow!("failed to validate session: {}", e)))?
            .ok_or_else(|| {
                AppError::Unauthorized("invalid or expired WebUI session".to_string())
            })?;
        operator = Some(username);
    }

    Ok(operator.unwrap_or_else(|| "anonymous".to_string()))
}

enum ApiCredential {
    Bearer(String),
    ApiKey(String),
}

fn extract_api_credential(headers: &HeaderMap) -> Option<ApiCredential> {
    if let Some(token) = headers
        .get("Authorization")
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
        .map(str::trim)
        .filter(|v| !v.is_empty())
    {
        return Some(ApiCredential::Bearer(token.to_string()));
    }
    if let Some(key) = headers
        .get("X-API-Key")
        .and_then(|v| v.to_str().ok())
        .map(str::trim)
        .filter(|v| !v.is_empty())
    {
        return Some(ApiCredential::ApiKey(key.to_string()));
    }
    None
}

fn session_token(headers: &HeaderMap) -> Option<String> {
    headers
        .get("x-session-token")
        .and_then(|v| v.to_str().ok())
        .map(|v| v.trim().to_string())
        .filter(|v| !v.is_empty())
}

// ─── envd PTY client ─────────────────────────────────────────────────────────

#[derive(Debug, Clone, Copy)]
struct PtySize {
    rows: u16,
    cols: u16,
}

/// Minimal client for envd's `process.Process` Connect-JSON RPC, routed
/// through the sandbox proxy exactly like `run_envd_command` (AgentHub) and
/// the SDK PTY modules.
struct EnvdPtyClient {
    http: reqwest::Client,
    proxy_base: String,
    host: String,
    basic_auth: String,
}

impl EnvdPtyClient {
    fn new(http: reqwest::Client, sandbox_id: &str, domain: &str, user: &str) -> Self {
        let proxy_base = std::env::var("AGENTHUB_SANDBOX_PROXY_URL")
            .unwrap_or_else(|_| "http://127.0.0.1".to_string())
            .trim_end_matches('/')
            .to_string();
        Self {
            http,
            proxy_base,
            host: format!("{}-{}.{}", ENVD_PORT, sandbox_id, domain),
            basic_auth: format!("Basic {}", BASE64.encode(format!("{}:", user))),
        }
    }

    fn url(&self, method: &str) -> String {
        format!("{}/process.Process/{}", self.proxy_base, method)
    }

    /// Start `/bin/bash -i -l` under a PTY. Returns the PTY pid plus the
    /// still-open Connect event stream. No `Connect-Timeout-Ms` header is
    /// sent — the session lifetime is bounded by our own idle timeout.
    async fn start(
        &self,
        size: PtySize,
    ) -> anyhow::Result<(
        u32,
        impl futures::Stream<Item = reqwest::Result<bytes::Bytes>> + Unpin,
    )> {
        let payload = json!({
            "process": {
                "cmd": "/bin/bash",
                "args": ["-i", "-l"],
                "envs": {
                    "TERM": "xterm-256color",
                    "LANG": "C.UTF-8",
                    "LC_ALL": "C.UTF-8",
                },
            },
            "pty": { "size": { "rows": size.rows, "cols": size.cols } },
        });
        let resp = self
            .http
            .post(self.url("Start"))
            .header("Host", &self.host)
            .header("Content-Type", CONNECT_JSON)
            .header("Connect-Protocol-Version", "1")
            .header("Authorization", &self.basic_auth)
            .body(connect_envelope(&serde_json::to_vec(&payload)?))
            .send()
            .await?;
        if !resp.status().is_success() {
            anyhow::bail!("envd PTY start returned HTTP {}", resp.status());
        }

        let mut stream = resp.bytes_stream();
        let mut decoder = ConnectFrameDecoder::default();

        // The first event on the stream must be `{"event":{"start":{"pid":N}}}`.
        let deadline = Instant::now() + START_TIMEOUT;
        loop {
            let chunk = tokio::time::timeout_at(deadline, stream.next())
                .await
                .map_err(|_| anyhow::anyhow!("timed out waiting for PTY start event"))?;
            let Some(chunk) = chunk else {
                anyhow::bail!("PTY stream closed before start event");
            };
            let frames = decoder.push(&chunk?).map_err(|e| anyhow::anyhow!(e))?;
            let Some(frame) = frames.into_iter().next() else {
                continue;
            };
            if frame.flags & CONNECT_END_STREAM_FLAG != 0 {
                anyhow::bail!("PTY stream ended before start event: {}", frame.text());
            }
            let event: Value = serde_json::from_slice(&frame.payload)?;
            let pid = event
                .pointer("/event/start/pid")
                .and_then(Value::as_u64)
                .ok_or_else(|| {
                    anyhow::anyhow!("expected PTY start event, got: {}", frame.text())
                })?;
            return Ok((pid as u32, stream));
        }
    }

    /// Unary `process.Process` call (`SendInput` / `Update` / `SendSignal`).
    async fn unary(&self, method: &str, payload: Value) -> anyhow::Result<()> {
        let resp = self
            .http
            .post(self.url(method))
            .header("Host", &self.host)
            .header("Content-Type", "application/json")
            .header("Connect-Protocol-Version", "1")
            .header("Authorization", &self.basic_auth)
            .json(&payload)
            .send()
            .await?;
        if !resp.status().is_success() {
            anyhow::bail!("envd {} returned HTTP {}", method, resp.status());
        }
        Ok(())
    }

    async fn send_input(&self, pid: u32, data: &[u8]) -> anyhow::Result<()> {
        self.unary(
            "SendInput",
            json!({ "process": { "pid": pid }, "input": { "pty": BASE64.encode(data) } }),
        )
        .await
    }

    async fn resize(&self, pid: u32, size: PtySize) -> anyhow::Result<()> {
        self.unary(
            "Update",
            json!({
                "process": { "pid": pid },
                "pty": { "size": { "rows": size.rows, "cols": size.cols } },
            }),
        )
        .await
    }

    async fn kill(&self, pid: u32) -> anyhow::Result<()> {
        self.unary(
            "SendSignal",
            json!({ "process": { "pid": pid }, "signal": SIGNAL_SIGKILL }),
        )
        .await
    }
}

fn connect_envelope(payload: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(payload.len() + 5);
    out.push(0);
    out.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    out.extend_from_slice(payload);
    out
}

// ─── Connect stream decoding ─────────────────────────────────────────────────

#[derive(Debug, PartialEq)]
struct ConnectFrame {
    flags: u8,
    payload: Vec<u8>,
}

impl ConnectFrame {
    fn text(&self) -> String {
        String::from_utf8_lossy(&self.payload).into_owned()
    }
}

/// Incremental decoder for Connect's 5-byte-envelope framing
/// (`flags:u8 | length:u32be | payload`). HTTP chunk boundaries do not align
/// with frame boundaries, so partial input is buffered across `push` calls.
#[derive(Default)]
struct ConnectFrameDecoder {
    buf: Vec<u8>,
}

impl ConnectFrameDecoder {
    /// Feed a chunk and return any complete frames. Returns `Err` if a frame
    /// header advertises a payload larger than `MAX_CONNECT_FRAME_BYTES`, so a
    /// crafted length cannot drive unbounded allocation; the caller tears the
    /// session down on error.
    fn push(&mut self, data: &[u8]) -> Result<Vec<ConnectFrame>, String> {
        self.buf.extend_from_slice(data);
        let mut frames = Vec::new();
        loop {
            if self.buf.len() < 5 {
                break;
            }
            let len =
                u32::from_be_bytes([self.buf[1], self.buf[2], self.buf[3], self.buf[4]]) as usize;
            if len > MAX_CONNECT_FRAME_BYTES {
                self.buf.clear();
                return Err(format!(
                    "connect frame length {} exceeds {} byte cap",
                    len, MAX_CONNECT_FRAME_BYTES
                ));
            }
            if self.buf.len() < 5 + len {
                break;
            }
            let flags = self.buf[0];
            let payload = self.buf[5..5 + len].to_vec();
            self.buf.drain(..5 + len);
            frames.push(ConnectFrame { flags, payload });
        }
        Ok(frames)
    }
}

// ─── Session bridge ──────────────────────────────────────────────────────────

struct SessionContext {
    state: AppState,
    session_id: String,
    sandbox_id: String,
    domain: String,
    operator: String,
    guest_user: String,
    size: PtySize,
    idle_timeout: Duration,
    // Held for the lifetime of the session; dropping it frees a slot in the
    // server-wide terminal semaphore. Never read directly.
    _permit: OwnedSemaphorePermit,
}

/// Why a session ended — recorded verbatim in the audit log.
#[derive(Debug, Clone, Copy, PartialEq)]
enum CloseReason {
    ProcessExit,
    ClientDisconnected,
    IdleTimeout,
    EnvdError,
    EnvdStreamEnded,
    StartFailed,
    Panicked,
}

impl CloseReason {
    fn as_str(self) -> &'static str {
        match self {
            CloseReason::ProcessExit => "process_exit",
            CloseReason::ClientDisconnected => "client_disconnected",
            CloseReason::IdleTimeout => "idle_timeout",
            CloseReason::EnvdError => "envd_error",
            CloseReason::EnvdStreamEnded => "envd_stream_ended",
            CloseReason::StartFailed => "start_failed",
            CloseReason::Panicked => "panicked",
        }
    }
}

async fn run_session(mut socket: WebSocket, ctx: SessionContext) {
    let started = Instant::now();
    let envd = EnvdPtyClient::new(
        ctx.state.http_client.clone(),
        ctx.sandbox_id.as_str(),
        ctx.domain.as_str(),
        ctx.guest_user.as_str(),
    );

    let (pid, stream) = match envd.start(ctx.size).await {
        Ok(started) => started,
        Err(err) => {
            // Log the real cause; tell the client only that it failed.
            tracing::warn!(
                sandbox_id = %ctx.sandbox_id,
                error = %err,
                "web terminal: PTY start failed"
            );
            let _ = socket
                .send(
                    ServerMessage::Error {
                        code: CloseReason::StartFailed.as_str().to_string(),
                        message: "failed to start terminal".to_string(),
                    }
                    .to_ws(),
                )
                .await;
            let _ = socket.close().await;
            audit_close(&ctx, None, CloseReason::StartFailed, started).await;
            return;
        }
    };

    if socket
        .send(ServerMessage::Ready { pid }.to_ws())
        .await
        .is_err()
    {
        reap_pty(&envd, &ctx, pid).await;
        audit_close(&ctx, Some(pid), CloseReason::ClientDisconnected, started).await;
        return;
    }

    // Guard the pump against panics so the PTY is always reaped: a panic in
    // the bridge task would otherwise be swallowed by Tokio, dropping the task
    // without killing the shell.
    let reason = match std::panic::AssertUnwindSafe(bridge(
        &mut socket,
        &envd,
        pid,
        stream,
        ctx.idle_timeout,
    ))
    .catch_unwind()
    .await
    {
        Ok(reason) => reason,
        Err(_) => {
            tracing::error!(sandbox_id = %ctx.sandbox_id, pid, "web terminal: bridge task panicked");
            CloseReason::Panicked
        }
    };

    // The shell keeps running inside the sandbox unless it exited by itself;
    // reap it so closed browser tabs / crashes do not leak PTY processes.
    if reason != CloseReason::ProcessExit {
        reap_pty(&envd, &ctx, pid).await;
    }
    let _ = socket.close().await;
    audit_close(&ctx, Some(pid), reason, started).await;
}

/// SIGKILL the PTY, surfacing failures at warn level so a leaked shell is
/// visible in monitoring rather than hidden at debug.
async fn reap_pty(envd: &EnvdPtyClient, ctx: &SessionContext, pid: u32) {
    if let Err(err) = envd.kill(pid).await {
        tracing::warn!(
            sandbox_id = %ctx.sandbox_id,
            pid,
            error = %err,
            "web terminal: PTY kill on close failed; shell may be orphaned in the sandbox"
        );
    }
}

/// Pump events between the WebSocket and the envd PTY until either side
/// terminates. Returns the close reason for auditing.
async fn bridge(
    socket: &mut WebSocket,
    envd: &EnvdPtyClient,
    pid: u32,
    mut stream: impl futures::Stream<Item = reqwest::Result<bytes::Bytes>> + Unpin,
    idle_timeout: Duration,
) -> CloseReason {
    let mut decoder = ConnectFrameDecoder::default();
    let mut last_activity = Instant::now();
    let mut idle_sweep = tokio::time::interval(IDLE_SWEEP_INTERVAL);
    idle_sweep.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);

    loop {
        tokio::select! {
            chunk = stream.next() => {
                let Some(chunk) = chunk else {
                    return CloseReason::EnvdStreamEnded;
                };
                let chunk = match chunk {
                    Ok(chunk) => chunk,
                    Err(err) => {
                        tracing::warn!(pid, error = %err, "web terminal: sandbox stream failed");
                        let _ = send_error(socket, CloseReason::EnvdError,
                            GENERIC_BACKEND_ERROR.to_string()).await;
                        return CloseReason::EnvdError;
                    }
                };
                let frames = match decoder.push(&chunk) {
                    Ok(frames) => frames,
                    Err(err) => {
                        // Oversized/hostile frame length — log detail, tell the
                        // client only that the backend errored.
                        tracing::warn!(pid, error = %err, "web terminal: rejecting sandbox frame");
                        let _ = send_error(socket, CloseReason::EnvdError,
                            GENERIC_BACKEND_ERROR.to_string()).await;
                        return CloseReason::EnvdError;
                    }
                };
                for frame in frames {
                    if frame.flags & CONNECT_END_STREAM_FLAG != 0 {
                        let end: Value = serde_json::from_slice(&frame.payload).unwrap_or_default();
                        if end.get("error").is_some() {
                            tracing::warn!(pid, detail = %end, "web terminal: sandbox reported stream error");
                            let _ = send_error(socket, CloseReason::EnvdError,
                                GENERIC_BACKEND_ERROR.to_string()).await;
                            return CloseReason::EnvdError;
                        }
                        return CloseReason::EnvdStreamEnded;
                    }
                    let event: Value = match serde_json::from_slice(&frame.payload) {
                        Ok(event) => event,
                        Err(_) => continue,
                    };
                    if let Some(data) = event.pointer("/event/data/pty").and_then(Value::as_str) {
                        // envd already base64-encodes PTY bytes — forward as-is.
                        let msg = ServerMessage::Output { data: data.to_string() };
                        if socket.send(msg.to_ws()).await.is_err() {
                            return CloseReason::ClientDisconnected;
                        }
                    }
                    if let Some(end) = event.pointer("/event/end") {
                        let exit_code = end
                            .get("exitCode")
                            .and_then(Value::as_i64)
                            .or_else(|| parse_exit_status(end.get("status").and_then(Value::as_str)));
                        let _ = socket.send(ServerMessage::Exit { exit_code }.to_ws()).await;
                        return CloseReason::ProcessExit;
                    }
                }
            }

            msg = socket.recv() => {
                let Some(Ok(msg)) = msg else {
                    return CloseReason::ClientDisconnected;
                };
                match msg {
                    Message::Text(text) => {
                        let parsed: Result<ClientMessage, _> = serde_json::from_str(&text);
                        match parsed {
                            Ok(ClientMessage::Input { data }) => {
                                last_activity = Instant::now();
                                let Ok(bytes) = BASE64.decode(data.as_bytes()) else {
                                    // Client-side framing bug: warn (non-fatal),
                                    // keep the session open.
                                    let _ = send_warning(socket, "bad_input",
                                        "ignored input frame: payload is not valid base64").await;
                                    continue;
                                };
                                if let Err(err) = envd.send_input(pid, &bytes).await {
                                    tracing::warn!(pid, error = %err, "web terminal: forward input failed");
                                    let _ = send_error(socket, CloseReason::EnvdError,
                                        GENERIC_BACKEND_ERROR.to_string()).await;
                                    return CloseReason::EnvdError;
                                }
                            }
                            Ok(ClientMessage::Resize { cols, rows }) => {
                                last_activity = Instant::now();
                                let size = PtySize {
                                    rows: rows.clamp(1, MAX_ROWS),
                                    cols: cols.clamp(1, MAX_COLS),
                                };
                                if let Err(err) = envd.resize(pid, size).await {
                                    tracing::debug!(pid, error = %err,
                                        "web terminal: resize failed");
                                }
                            }
                            Ok(ClientMessage::Ping) => {
                                if socket.send(ServerMessage::Pong.to_ws()).await.is_err() {
                                    return CloseReason::ClientDisconnected;
                                }
                            }
                            Err(_) => {
                                // Unknown/bad frame: non-fatal warning, keep going.
                                let _ = send_warning(socket, "bad_message",
                                    "ignored unrecognized terminal message").await;
                            }
                        }
                    }
                    Message::Binary(bytes) => {
                        last_activity = Instant::now();
                        if let Err(err) = envd.send_input(pid, &bytes).await {
                            tracing::warn!(pid, error = %err, "web terminal: forward input failed");
                            let _ = send_error(socket, CloseReason::EnvdError,
                                GENERIC_BACKEND_ERROR.to_string()).await;
                            return CloseReason::EnvdError;
                        }
                    }
                    Message::Close(_) => return CloseReason::ClientDisconnected,
                    Message::Ping(_) | Message::Pong(_) => {}
                }
            }

            _ = idle_sweep.tick() => {
                if last_activity.elapsed() >= idle_timeout {
                    let _ = send_error(socket, CloseReason::IdleTimeout, format!(
                        "terminal closed after {}s without input",
                        idle_timeout.as_secs(),
                    )).await;
                    return CloseReason::IdleTimeout;
                }
            }
        }
    }
}

async fn send_error(
    socket: &mut WebSocket,
    code: CloseReason,
    message: String,
) -> Result<(), axum::Error> {
    socket
        .send(
            ServerMessage::Error {
                code: code.as_str().to_string(),
                message,
            }
            .to_ws(),
        )
        .await
}

async fn send_warning(
    socket: &mut WebSocket,
    code: &str,
    message: &str,
) -> Result<(), axum::Error> {
    socket
        .send(
            ServerMessage::Warning {
                code: code.to_string(),
                message: message.to_string(),
            }
            .to_ws(),
        )
        .await
}

fn parse_exit_status(status: Option<&str>) -> Option<i64> {
    status?
        .strip_prefix("exit status ")
        .and_then(|v| v.trim().parse::<i64>().ok())
}

async fn audit_close(
    ctx: &SessionContext,
    pid: Option<u32>,
    reason: CloseReason,
    started: Instant,
) {
    ctx.state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "terminal.session.closed")
                .field("session_id", &ctx.session_id)
                .field("sandbox_id", &ctx.sandbox_id)
                .field("operator", &ctx.operator)
                .field("guest_user", &ctx.guest_user)
                .field("reason", reason.as_str())
                .field_value("pid", pid)
                .field_value("duration_secs", started.elapsed().as_secs()),
        )
        .await;
}

// ─── Tests ───────────────────────────────────────────────────────────────────

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        config::ServerConfig,
        logging::{arc, noop::NoopLogger},
        routes::build_router,
        state::AppState,
    };
    use axum::{
        body::Body,
        extract::RawQuery,
        http::StatusCode,
        routing::{any, get, post},
        Json, Router,
    };
    use futures::SinkExt;
    use std::sync::{Arc, Mutex as StdMutex};
    use tokio::sync::mpsc;
    use tokio_tungstenite::tungstenite;

    // The AGENTHUB_SANDBOX_PROXY_URL env var is process-global; serialize the
    // tests that mutate it.
    static PROXY_ENV_LOCK: StdMutex<()> = StdMutex::new(());

    // ── Frame decoder ────────────────────────────────────────────────────────

    fn frame_bytes(flags: u8, payload: &[u8]) -> Vec<u8> {
        let mut out = vec![flags];
        out.extend_from_slice(&(payload.len() as u32).to_be_bytes());
        out.extend_from_slice(payload);
        out
    }

    #[test]
    fn decoder_handles_frames_split_across_chunks() {
        let mut decoder = ConnectFrameDecoder::default();
        let bytes = frame_bytes(0, br#"{"event":1}"#);
        let (a, b) = bytes.split_at(3);

        assert!(decoder.push(a).unwrap().is_empty());
        let frames = decoder.push(b).unwrap();
        assert_eq!(frames.len(), 1);
        assert_eq!(frames[0].flags, 0);
        assert_eq!(frames[0].payload, br#"{"event":1}"#);
    }

    #[test]
    fn decoder_yields_multiple_frames_from_one_chunk() {
        let mut decoder = ConnectFrameDecoder::default();
        let mut bytes = frame_bytes(0, b"one");
        bytes.extend_from_slice(&frame_bytes(CONNECT_END_STREAM_FLAG, b"{}"));

        let frames = decoder.push(&bytes).unwrap();
        assert_eq!(frames.len(), 2);
        assert_eq!(frames[0].payload, b"one");
        assert_eq!(
            frames[1].flags & CONNECT_END_STREAM_FLAG,
            CONNECT_END_STREAM_FLAG
        );
    }

    #[test]
    fn decoder_buffers_partial_header() {
        let mut decoder = ConnectFrameDecoder::default();
        assert!(decoder.push(&[0, 0]).unwrap().is_empty());
        assert!(decoder.push(&[0, 0]).unwrap().is_empty());
        let frames = decoder.push(&[1, b'x']).unwrap();
        assert_eq!(frames.len(), 1);
        assert_eq!(frames[0].payload, b"x");
    }

    #[test]
    fn decoder_rejects_oversized_frame() {
        let mut decoder = ConnectFrameDecoder::default();
        // Header claims a payload larger than the cap: reject without buffering.
        let mut header = vec![0u8];
        header.extend_from_slice(&((MAX_CONNECT_FRAME_BYTES as u32) + 1).to_be_bytes());
        let err = decoder.push(&header).unwrap_err();
        assert!(err.contains("exceeds"));
        // Buffer was cleared, so the decoder recovers for subsequent frames.
        let frames = decoder.push(&frame_bytes(0, b"ok")).unwrap();
        assert_eq!(frames.len(), 1);
        assert_eq!(frames[0].payload, b"ok");
    }

    // ── Wire messages ────────────────────────────────────────────────────────

    #[test]
    fn client_messages_parse() {
        assert_eq!(
            serde_json::from_str::<ClientMessage>(r#"{"type":"input","data":"bHM="}"#).unwrap(),
            ClientMessage::Input {
                data: "bHM=".to_string()
            }
        );
        assert_eq!(
            serde_json::from_str::<ClientMessage>(r#"{"type":"resize","cols":120,"rows":32}"#)
                .unwrap(),
            ClientMessage::Resize {
                cols: 120,
                rows: 32
            }
        );
        assert_eq!(
            serde_json::from_str::<ClientMessage>(r#"{"type":"ping"}"#).unwrap(),
            ClientMessage::Ping
        );
        assert!(serde_json::from_str::<ClientMessage>(r#"{"type":"exec","cmd":"rm"}"#).is_err());
    }

    #[test]
    fn server_messages_serialize_with_camel_case_exit_code() {
        let exit = serde_json::to_value(ServerMessage::Exit { exit_code: Some(0) }).unwrap();
        assert_eq!(exit, serde_json::json!({"type": "exit", "exitCode": 0}));
        let ready = serde_json::to_value(ServerMessage::Ready { pid: 7 }).unwrap();
        assert_eq!(ready, serde_json::json!({"type": "ready", "pid": 7}));
    }

    // ── Validation ───────────────────────────────────────────────────────────

    #[test]
    fn sandbox_id_validation_rejects_host_header_smuggling() {
        assert!(validate_sandbox_id("sb-123_ok").is_ok());
        assert!(validate_sandbox_id("").is_err());
        assert!(validate_sandbox_id("sb.evil.example").is_err());
        assert!(validate_sandbox_id("sb:443").is_err());
        assert!(validate_sandbox_id("sb/../x").is_err());
    }

    #[test]
    fn guest_user_validation() {
        assert_eq!(validate_guest_user(None).unwrap(), "root");
        assert_eq!(validate_guest_user(Some("user-1")).unwrap(), "user-1");
        assert!(validate_guest_user(Some("Root")).is_err());
        assert!(validate_guest_user(Some("a b")).is_err());
        assert!(validate_guest_user(Some("")).is_err());
    }

    #[test]
    fn session_token_read_from_header_only() {
        let mut headers = HeaderMap::new();
        headers.insert("x-session-token", "from-header".parse().unwrap());
        assert_eq!(session_token(&headers).unwrap(), "from-header");
        assert!(session_token(&HeaderMap::new()).is_none());
    }

    #[test]
    fn api_credential_read_from_header_only() {
        let mut headers = HeaderMap::new();
        headers.insert("Authorization", "Bearer tok-123".parse().unwrap());
        assert!(matches!(
            extract_api_credential(&headers),
            Some(ApiCredential::Bearer(t)) if t == "tok-123"
        ));
        let mut headers = HeaderMap::new();
        headers.insert("X-API-Key", "key-9".parse().unwrap());
        assert!(matches!(
            extract_api_credential(&headers),
            Some(ApiCredential::ApiKey(k)) if k == "key-9"
        ));
        assert!(extract_api_credential(&HeaderMap::new()).is_none());
    }

    // ── Mock backends ────────────────────────────────────────────────────────

    /// Requests recorded by the mock envd server.
    #[derive(Default)]
    struct EnvdCalls {
        inputs: Vec<Value>,
        updates: Vec<Value>,
        signals: Vec<Value>,
    }

    /// Serve a fake CubeMaster that reports one sandbox with `status`.
    async fn spawn_mock_cubemaster(status: i32) -> String {
        let app = Router::new()
            .route(
                "/cube/sandbox/info",
                get(move |RawQuery(q): RawQuery| async move {
                    let q = q.unwrap_or_default();
                    let sandbox_id = q
                        .split('&')
                        .find_map(|kv| kv.strip_prefix("sandbox_id="))
                        .unwrap_or("sb-unknown")
                        .to_string();
                    Json(serde_json::json!({
                        "requestID": "req-1",
                        "ret": {"ret_code": 0, "ret_msg": ""},
                        "data": [{
                            "sandbox_id": sandbox_id,
                            "status": status,
                            "host_id": "host-1",
                            "template_id": "tpl-1",
                            "annotations": {},
                            "labels": {},
                            "namespace": "ns",
                            "containers": [{
                                "name": "sandbox",
                                "container_id": "c-1",
                                "status": status,
                                "type": "sandbox",
                                "cpu": "1000m",
                                "mem": "512Mi",
                                "create_at": 1,
                            }],
                        }],
                    }))
                }),
            )
            .route(
                "/cube/sandbox/list",
                post(|| async {
                    Json(serde_json::json!({
                        "requestID": "req-2",
                        "ret": {"ret_code": 0, "ret_msg": ""},
                        "data": [],
                    }))
                }),
            );
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
        format!("http://{}", addr)
    }

    /// Serve a fake envd `process.Process` endpoint behind the proxy URL.
    ///
    /// `Start` emits a PTY start event plus one output chunk and then keeps
    /// the stream open until `SendSignal` is received.
    async fn spawn_mock_envd(calls: Arc<tokio::sync::Mutex<EnvdCalls>>) -> String {
        let (kill_tx, _) = tokio::sync::broadcast::channel::<()>(4);
        let kill_for_start = kill_tx.clone();

        let start = post(move || {
            let kill = kill_for_start.subscribe();
            async move {
                let (tx, rx) = mpsc::channel::<Result<bytes::Bytes, std::io::Error>>(8);
                tokio::spawn(async move {
                    let mut kill = kill;
                    let start = serde_json::json!({"event": {"start": {"pid": 4242}}});
                    let output = serde_json::json!({"event": {"data": {
                        "pty": BASE64.encode("welcome\r\n"),
                    }}});
                    let _ = tx
                        .send(Ok(bytes::Bytes::from(connect_envelope(
                            &serde_json::to_vec(&start).unwrap(),
                        ))))
                        .await;
                    let _ = tx
                        .send(Ok(bytes::Bytes::from(connect_envelope(
                            &serde_json::to_vec(&output).unwrap(),
                        ))))
                        .await;
                    // Hold the stream open until the PTY is killed.
                    let _ = kill.recv().await;
                });
                axum::response::Response::builder()
                    .header("Content-Type", CONNECT_JSON)
                    .body(Body::from_stream(
                        tokio_stream::wrappers::ReceiverStream::new(rx),
                    ))
                    .unwrap()
            }
        });

        let calls_input = calls.clone();
        let calls_update = calls.clone();
        let calls_signal = calls.clone();
        let kill_for_signal = kill_tx.clone();
        let app = Router::new()
            .route("/process.Process/Start", start)
            .route(
                "/process.Process/SendInput",
                post(move |Json(body): Json<Value>| {
                    let calls = calls_input.clone();
                    async move {
                        calls.lock().await.inputs.push(body);
                        Json(serde_json::json!({}))
                    }
                }),
            )
            .route(
                "/process.Process/Update",
                post(move |Json(body): Json<Value>| {
                    let calls = calls_update.clone();
                    async move {
                        calls.lock().await.updates.push(body);
                        Json(serde_json::json!({}))
                    }
                }),
            )
            .route(
                "/process.Process/SendSignal",
                post(move |Json(body): Json<Value>| {
                    let calls = calls_signal.clone();
                    let kill_tx = kill_for_signal.clone();
                    async move {
                        calls.lock().await.signals.push(body);
                        let _ = kill_tx.send(());
                        Json(serde_json::json!({}))
                    }
                }),
            );

        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
        format!("http://{}", addr)
    }

    async fn spawn_api(config: ServerConfig) -> String {
        let state = AppState::new(config, arc(NoopLogger)).await;
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(listener, build_router(state)).await.unwrap();
        });
        format!("127.0.0.1:{}", addr.port())
    }

    fn ws_text(msg: &tungstenite::Message) -> Value {
        match msg {
            tungstenite::Message::Text(text) => serde_json::from_str(text).unwrap(),
            other => panic!("expected text frame, got {:?}", other),
        }
    }

    async fn next_json(
        ws: &mut (impl futures::Stream<Item = Result<tungstenite::Message, tungstenite::Error>> + Unpin),
    ) -> Value {
        loop {
            let msg = tokio::time::timeout(Duration::from_secs(10), ws.next())
                .await
                .expect("timed out waiting for ws frame")
                .expect("ws stream ended")
                .expect("ws frame errored");
            if matches!(
                msg,
                tungstenite::Message::Ping(_) | tungstenite::Message::Pong(_)
            ) {
                continue;
            }
            return ws_text(&msg);
        }
    }

    // ── Integration: WS ↔ envd bridge ────────────────────────────────────────

    #[tokio::test]
    async fn terminal_bridges_browser_to_envd_pty() {
        let _guard = PROXY_ENV_LOCK.lock().unwrap_or_else(|e| e.into_inner());
        let calls = Arc::new(tokio::sync::Mutex::new(EnvdCalls::default()));
        let envd_url = spawn_mock_envd(calls.clone()).await;
        std::env::set_var("AGENTHUB_SANDBOX_PROXY_URL", &envd_url);

        let mut config = ServerConfig::default();
        config.cubemaster_url = spawn_mock_cubemaster(1).await; // running
        config.auth_callback_url = None;
        config.database_url = None;
        let addr = spawn_api(config).await;

        let url = format!(
            "ws://{}/cubeapi/v1/sandboxes/sb-term-1/terminal?cols=100&rows=30",
            addr
        );
        let (mut ws, _) = tokio_tungstenite::connect_async(&url)
            .await
            .expect("ws connect");

        let ready = next_json(&mut ws).await;
        assert_eq!(ready["type"], "ready");
        assert_eq!(ready["pid"], 4242);

        let output = next_json(&mut ws).await;
        assert_eq!(output["type"], "output");
        let text = BASE64.decode(output["data"].as_str().unwrap()).unwrap();
        assert_eq!(text, b"welcome\r\n");

        // Keystrokes are forwarded to envd SendInput with the same bytes.
        ws.send(tungstenite::Message::Text(
            serde_json::json!({"type": "input", "data": BASE64.encode("ls -la\n")}).to_string(),
        ))
        .await
        .unwrap();
        // Window geometry changes land on envd Update.
        ws.send(tungstenite::Message::Text(
            serde_json::json!({"type": "resize", "cols": 132, "rows": 43}).to_string(),
        ))
        .await
        .unwrap();
        // Ping keeps intermediaries alive and is answered.
        ws.send(tungstenite::Message::Text(
            serde_json::json!({"type": "ping"}).to_string(),
        ))
        .await
        .unwrap();
        let pong = next_json(&mut ws).await;
        assert_eq!(pong["type"], "pong");

        // Closing the browser side must SIGKILL the PTY (no process leaks).
        ws.close(None).await.unwrap();
        let deadline = Instant::now() + Duration::from_secs(10);
        loop {
            {
                let calls = calls.lock().await;
                if !calls.signals.is_empty() {
                    assert_eq!(calls.inputs.len(), 1);
                    assert_eq!(calls.inputs[0]["process"]["pid"], 4242);
                    let sent = calls.inputs[0]["input"]["pty"].as_str().unwrap();
                    assert_eq!(BASE64.decode(sent).unwrap(), b"ls -la\n");

                    assert_eq!(calls.updates.len(), 1);
                    assert_eq!(calls.updates[0]["pty"]["size"]["cols"], 132);
                    assert_eq!(calls.updates[0]["pty"]["size"]["rows"], 43);

                    assert_eq!(calls.signals[0]["signal"], SIGNAL_SIGKILL);
                    assert_eq!(calls.signals[0]["process"]["pid"], 4242);
                    break;
                }
            }
            assert!(
                Instant::now() < deadline,
                "PTY was not killed on disconnect"
            );
            tokio::time::sleep(Duration::from_millis(50)).await;
        }

        std::env::remove_var("AGENTHUB_SANDBOX_PROXY_URL");
    }

    #[tokio::test]
    async fn terminal_rejects_sandbox_that_is_not_running() {
        let _guard = PROXY_ENV_LOCK.lock().unwrap_or_else(|e| e.into_inner());
        let mut config = ServerConfig::default();
        config.cubemaster_url = spawn_mock_cubemaster(5).await; // paused
        config.database_url = None;
        let addr = spawn_api(config).await;

        let url = format!("ws://{}/cubeapi/v1/sandboxes/sb-paused/terminal", addr);
        let err = tokio_tungstenite::connect_async(&url)
            .await
            .expect_err("must reject");
        match err {
            tungstenite::Error::Http(resp) => {
                assert_eq!(resp.status(), StatusCode::CONFLICT);
            }
            other => panic!("expected HTTP 409 rejection, got {:?}", other),
        }
    }

    #[tokio::test]
    async fn terminal_ticket_flow_enforces_auth_then_authorizes_ws() {
        let _guard = PROXY_ENV_LOCK.lock().unwrap_or_else(|e| e.into_inner());

        // Callback that accepts only key "good-key" and records path+method.
        let seen = Arc::new(tokio::sync::Mutex::new(Vec::<(String, String)>::new()));
        let seen_cb = seen.clone();
        let cb_app = Router::new().route(
            "/auth",
            any(move |req: axum::http::Request<Body>| {
                let seen = seen_cb.clone();
                async move {
                    let key = req
                        .headers()
                        .get("X-API-Key")
                        .and_then(|v| v.to_str().ok())
                        .unwrap_or("")
                        .to_string();
                    let path = req
                        .headers()
                        .get("X-Request-Path")
                        .and_then(|v| v.to_str().ok())
                        .unwrap_or("")
                        .to_string();
                    seen.lock().await.push((key.clone(), path));
                    if key == "good-key" {
                        StatusCode::OK
                    } else {
                        StatusCode::FORBIDDEN
                    }
                }
            }),
        );
        let cb_listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let cb_addr = cb_listener.local_addr().unwrap();
        tokio::spawn(async move { axum::serve(cb_listener, cb_app).await.unwrap() });

        let calls = Arc::new(tokio::sync::Mutex::new(EnvdCalls::default()));
        let envd_url = spawn_mock_envd(calls).await;
        std::env::set_var("AGENTHUB_SANDBOX_PROXY_URL", &envd_url);

        let mut config = ServerConfig::default();
        config.cubemaster_url = spawn_mock_cubemaster(1).await;
        config.auth_callback_url = Some(format!("http://{}/auth", cb_addr));
        config.database_url = None;
        let addr = spawn_api(config).await;

        let http = reqwest::Client::new();
        let ticket_url = format!("http://{}/cubeapi/v1/sandboxes/sb-1/terminal/ticket", addr);

        // Ticket mint without a credential → 401 (headers carry the secret).
        let resp = http.post(&ticket_url).send().await.unwrap();
        assert_eq!(resp.status(), reqwest::StatusCode::UNAUTHORIZED);

        // Rejected credential → 401.
        let resp = http
            .post(&ticket_url)
            .header("X-API-Key", "bad-key")
            .send()
            .await
            .unwrap();
        assert_eq!(resp.status(), reqwest::StatusCode::UNAUTHORIZED);

        // A WebSocket with a bogus ticket is rejected.
        let bogus = format!(
            "ws://{}/cubeapi/v1/sandboxes/sb-1/terminal?ticket=nope",
            addr
        );
        match tokio_tungstenite::connect_async(&bogus)
            .await
            .expect_err("must reject")
        {
            tungstenite::Error::Http(resp) => assert_eq!(resp.status(), StatusCode::UNAUTHORIZED),
            other => panic!("expected 401, got {:?}", other),
        }

        // Accepted credential mints a ticket; the WS then reaches ready.
        let resp = http
            .post(&ticket_url)
            .header("X-API-Key", "good-key")
            .send()
            .await
            .unwrap();
        assert_eq!(resp.status(), reqwest::StatusCode::OK);
        let ticket = resp.json::<serde_json::Value>().await.unwrap();
        let id = ticket["ticket"].as_str().unwrap().to_string();

        let ws_url = format!(
            "ws://{}/cubeapi/v1/sandboxes/sb-1/terminal?ticket={}",
            addr, id
        );
        let (mut ws, _) = tokio_tungstenite::connect_async(&ws_url)
            .await
            .expect("ws connect");
        let ready = next_json(&mut ws).await;
        assert_eq!(ready["type"], "ready");
        ws.close(None).await.unwrap();

        // A ticket is single-use: reusing it fails.
        match tokio_tungstenite::connect_async(&ws_url)
            .await
            .expect_err("ticket must be single-use")
        {
            tungstenite::Error::Http(resp) => assert_eq!(resp.status(), StatusCode::UNAUTHORIZED),
            other => panic!("expected 401 on ticket reuse, got {:?}", other),
        }

        let seen = seen.lock().await;
        assert!(seen.iter().any(|(key, path)| {
            key == "good-key" && path == "/cubeapi/v1/sandboxes/sb-1/terminal/ticket"
        }));

        std::env::remove_var("AGENTHUB_SANDBOX_PROXY_URL");
    }

    #[tokio::test]
    async fn terminal_bad_input_is_non_fatal_warning() {
        let _guard = PROXY_ENV_LOCK.lock().unwrap_or_else(|e| e.into_inner());
        let calls = Arc::new(tokio::sync::Mutex::new(EnvdCalls::default()));
        let envd_url = spawn_mock_envd(calls).await;
        std::env::set_var("AGENTHUB_SANDBOX_PROXY_URL", &envd_url);

        let mut config = ServerConfig::default();
        config.cubemaster_url = spawn_mock_cubemaster(1).await;
        config.database_url = None;
        let addr = spawn_api(config).await;

        let url = format!("ws://{}/cubeapi/v1/sandboxes/sb-warn/terminal", addr);
        let (mut ws, _) = tokio_tungstenite::connect_async(&url)
            .await
            .expect("ws connect");
        assert_eq!(next_json(&mut ws).await["type"], "ready");
        assert_eq!(next_json(&mut ws).await["type"], "output");

        // Invalid base64 must yield a non-fatal warning, not a terminal error.
        ws.send(tungstenite::Message::Text(
            serde_json::json!({"type": "input", "data": "!!!not-base64!!!"}).to_string(),
        ))
        .await
        .unwrap();
        let warn = next_json(&mut ws).await;
        assert_eq!(warn["type"], "warning");
        assert_eq!(warn["code"], "bad_input");

        // The session is still alive: a ping is still answered.
        ws.send(tungstenite::Message::Text(
            serde_json::json!({"type": "ping"}).to_string(),
        ))
        .await
        .unwrap();
        assert_eq!(next_json(&mut ws).await["type"], "pong");

        ws.close(None).await.unwrap();
        std::env::remove_var("AGENTHUB_SANDBOX_PROXY_URL");
    }

    #[tokio::test]
    async fn terminal_enforces_max_sessions() {
        let _guard = PROXY_ENV_LOCK.lock().unwrap_or_else(|e| e.into_inner());
        let calls = Arc::new(tokio::sync::Mutex::new(EnvdCalls::default()));
        let envd_url = spawn_mock_envd(calls).await;
        std::env::set_var("AGENTHUB_SANDBOX_PROXY_URL", &envd_url);

        let mut config = ServerConfig::default();
        config.cubemaster_url = spawn_mock_cubemaster(1).await;
        config.database_url = None;
        config.terminal_max_sessions = 1; // only one concurrent terminal
        let addr = spawn_api(config).await;

        let url = format!("ws://{}/cubeapi/v1/sandboxes/sb-cap/terminal", addr);
        let (mut ws1, _) = tokio_tungstenite::connect_async(&url)
            .await
            .expect("first ws connect");
        assert_eq!(next_json(&mut ws1).await["type"], "ready");

        // Second concurrent session is rejected with 503 by the semaphore.
        match tokio_tungstenite::connect_async(&url)
            .await
            .expect_err("second session must be rejected")
        {
            tungstenite::Error::Http(resp) => {
                assert_eq!(resp.status(), StatusCode::SERVICE_UNAVAILABLE)
            }
            other => panic!("expected 503, got {:?}", other),
        }

        // After the first closes and frees its permit, a new one succeeds.
        ws1.close(None).await.unwrap();
        let deadline = Instant::now() + Duration::from_secs(10);
        loop {
            match tokio_tungstenite::connect_async(&url).await {
                Ok((mut ws2, _)) => {
                    assert_eq!(next_json(&mut ws2).await["type"], "ready");
                    ws2.close(None).await.unwrap();
                    break;
                }
                Err(_) if Instant::now() < deadline => {
                    tokio::time::sleep(Duration::from_millis(50)).await;
                }
                Err(e) => panic!("permit was not released: {:?}", e),
            }
        }

        std::env::remove_var("AGENTHUB_SANDBOX_PROXY_URL");
    }
}

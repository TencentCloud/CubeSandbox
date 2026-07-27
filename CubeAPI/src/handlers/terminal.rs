// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// Interactive terminal over WebSocket.
//
// Bridges a browser WebSocket to the envd PTY stream inside a running
// sandbox (envd listens on ENVD_PORT and speaks Connect-RPC, reached through
// CubeProxy via Host-header routing — the same wire protocol as the Go
// SDK's `pty.go`).
//
// Multi-container sandboxes: the sandbox's i-th container runs its own envd
// on port 49983 + i, reported by CubeMaster as `envd_port` per container.
// The optional `container` query parameter selects the target container by
// ID or name; without it the primary container is used. Sandboxes created
// before per-container envd ports existed carry no port on their sidecar
// containers — selecting one is rejected with 409, while the primary
// container falls back to ENVD_PORT.
//
// Sandboxes created with `allowPublicTraffic = false` require CubeProxy's
// `e2b-traffic-access-token` header. This endpoint does not send one: the
// token is only handed out at sandbox create time (CubeProxy enforces it
// against Redis directly), so terminal access to such sandboxes is rejected
// by the proxy and surfaces as an error frame.
//
// Hardening notes:
//
// - Fail closed: when no auth backend is configured (neither
//   `auth_callback_url` nor `cube_api_key`) the endpoint rejects every
//   handshake with 403 unless `terminal_allow_unauthenticated` is explicitly
//   enabled — an unauthenticated terminal is a remote shell, so a default
//   deployment must never expose one.
// - Auth token transport: browsers pass the auth token as a WebSocket
//   subprotocol (`Sec-WebSocket-Protocol: cube-terminal.<token>`) alongside
//   the token-free base protocol `cube-terminal`. Non-browser clients use
//   the standard `Authorization: Bearer <token>` header. The `token` query
//   param exists as a fallback but is DISABLED by default
//   (`terminal_token_query_param`): URLs end up in front-proxy access logs,
//   which would leak the token. Priority: subprotocol → Authorization
//   header → (when enabled) query param → X-API-Key. When the client
//   offered `cube-terminal`, the server selects exactly that base protocol
//   in the upgrade response — Chrome aborts the handshake when it offered
//   subprotocols but the server selects none — while the token-bearing
//   entry is never selected, so the token is not echoed back.
// - Origin check: when an `Origin` header is present (browsers always send
//   one on WebSocket handshakes) it must be authorized one of two ways:
//   with `terminal_allowed_origins` configured, the Origin must exactly
//   match a whitelist entry (scheme/host case-insensitive); otherwise its
//   hostname must match the request `Host` header and the effective ports
//   must agree, where a port-less side means the Origin scheme's default
//   (80/443) — so `http://example.com` matches `example.com:80` but not
//   `example.com:3000`, and a proxy cannot widen the check to arbitrary
//   same-host services by stripping or adding a port. A mismatch is
//   rejected with 403 before the upgrade. Clients that send no Origin
//   (curl, python, CLI) are unaffected.
// - Session cap: at most `terminal_max_sessions_per_sandbox` concurrent
//   sessions per sandbox (default 8) and `terminal_max_sessions_global`
//   across all sandboxes (default 128); beyond either cap → 429.
// - Frame cap: browser WebSocket messages and frames are capped at 64 KiB;
//   oversized client traffic terminates the session with a protocol error.
//   envd Connect-RPC frames are capped at 4 MiB; oversized envd frames are
//   treated as a stream error and take the normal teardown path. PTY output
//   is re-chunked to ≤64 KiB per client message so a burst of shell output
//   does not arrive as one oversized WebSocket message.
// - Write deadline: every client-bound send has a 10 s deadline so a client
//   that stops reading cannot pin the writer task.
// - Orphan reaping: the envd Start request carries a unique `tag`; if the
//   Start stream fails before the start event (request timeout, truncated
//   stream) the shell may already be running without us knowing its pid.
//   The handler then reconnects with `Connect(process.tag)` to recover the
//   pid and SIGKILLs it (best-effort, deadline-bounded).
//
// Known gaps:
//
// - Authorization scope: authentication proves *a* valid user,
//   but there is no per-sandbox ownership or tenancy check — any
//   authenticated user can open a terminal on any sandbox. CubeAPI's sandbox
//   APIs have no per-sandbox ownership/tenancy model today, so this endpoint
//   inherits the same platform-wide posture as the other sandbox actions
//   (pause/resume/kill). Proper per-sandbox authorization is future
//   cross-API multi-tenancy work.
//   Audit attribution: the audit record distinguishes identity grades via
//   `identity_source` — "auth_callback" (`user`, from the callback's
//   authoritative `X-Auth-User` response header) or "unverified_jwt_claim"
//   (`claimed_user`, a self-asserted hint parsed from the *unverified*
//   claims of the already-authorized Bearer JWT, never treated as proof of
//   identity). Simple-key and open modes have no identity, so both fields
//   stay empty.
// - Orphan reaping depends on envd's `ProcessSelector.tag` support (present
//   in e2b-dev/infra envd 2026.16, which the CubeSandbox base image builds).
//   Sandboxes running an older envd reject the tag reconnect, so a shell
//   orphaned by the failed-start window keeps running until the sandbox
//   dies.

use axum::{
    extract::{
        ws::{Message, WebSocket, WebSocketUpgrade},
        OriginalUri, Path, Query, State,
    },
    http::HeaderMap,
    response::Response,
};
use base64::{
    engine::general_purpose::{STANDARD as BASE64, URL_SAFE_NO_PAD},
    Engine as _,
};
use futures::{SinkExt, StreamExt};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::time::Duration;

use crate::{
    cubemaster::{SandboxContainer, SandboxStatus},
    error::{AppError, AppResult},
    middleware::auth::{constant_time_eq, AUTH_CALLBACK_TIMEOUT},
    state::AppState,
};

/// envd's Connect-RPC port of the primary container — also the fallback for
/// sandboxes created before per-container envd ports existed (sidecar
/// containers listen on 49983 + i; see `resolve_envd_port`).
const ENVD_PORT: u16 = 49983;
/// Connect-RPC streaming content type.
const CONNECT_JSON: &str = "application/connect+json";
/// envd's built-in root credential (`Basic base64("root:")`).
const ENVD_BASIC_AUTH: &str = "Basic cm9vdDo=";
/// Server-side deadline handed to envd for the Start stream
/// (`Connect-Timeout-Ms`). Generous on purpose: the *idle* timeout below is
/// what actually reaps inactive sessions, so an actively used terminal is
/// not cut off mid-session.
const ENVD_STREAM_TIMEOUT_MS: &str = "86400000"; // 24 h
/// How long to wait for envd's start event before giving up on the session.
const START_EVENT_TIMEOUT: Duration = Duration::from_secs(30);
/// Deadline for one envd HTTP call (Start / SendInput / Update /
/// SendSignal) — *not* the long-lived Start stream, only the request up to
/// the response headers. Without it a hung envd (or a hung CubeProxy in
/// front of it) parks the awaiting `select!` branch in `pump_loop` forever:
/// the idle timer, disconnect detection and output reads all stop, leaking
/// the session slot and the sandbox shell. Once the deadline fires the
/// caller takes the normal error path (log / `teardown_session`) and the
/// pump keeps going or exits.
const ENVD_CALL_TIMEOUT: Duration = Duration::from_secs(10);
/// Subprotocol prefix browsers use to carry the auth token
/// (`Sec-WebSocket-Protocol: cube-terminal.<token>`).
const TOKEN_SUBPROTOCOL_PREFIX: &str = "cube-terminal.";
/// Base subprotocol the server selects in the 101 response when offered (see
/// `offered_base_subprotocol` — Chrome requires a selection).
const TERMINAL_SUBPROTOCOL: &str = "cube-terminal";
/// Browser WebSocket message/frame size cap (64 KiB).
const MAX_WS_MESSAGE_SIZE: usize = 64 * 1024;
/// Response header an auth callback may set on its 200 response to name the
/// operator the request was authorized for (audit attribution only).
const AUTH_USER_HEADER: &str = "x-auth-user";
/// Defensive cap on operator identity strings before they enter tracing
/// fields — a compromised or buggy identity source must not spray
/// unbounded text into the audit log.
const MAX_IDENTITY_LEN: usize = 128;
/// envd Connect-RPC frame size cap (4 MiB). A frame header claiming more than
/// this is treated as a stream error — envd PTY chunks are 16 KiB, so
/// anything near the cap is already pathological.
const MAX_ENVD_FRAME_SIZE: usize = 4 * 1024 * 1024;
/// Raw PTY bytes per client output message: 48 KiB base64-encodes to exactly
/// the 64 KiB WebSocket cap, keeping every client message within
/// `MAX_WS_MESSAGE_SIZE` even when envd delivers a large frame.
const OUTPUT_CHUNK_SIZE: usize = 48 * 1024;
/// Deadline for every client-bound send — a client that stops reading must
/// not pin the writer.
const WS_SEND_TIMEOUT: Duration = Duration::from_secs(10);
/// PTY size bounds accepted from clients.
const MIN_PTY_SIZE: u32 = 1;
const MAX_PTY_SIZE: u32 = 500;
const DEFAULT_COLS: u32 = 80;
const DEFAULT_ROWS: u32 = 24;

#[derive(Debug, Deserialize)]
pub struct TerminalQuery {
    cols: Option<u32>,
    rows: Option<u32>,
    /// Auth credential fallback for non-browser clients, DISABLED by default
    /// (`terminal_token_query_param` / `TERMINAL_TOKEN_QUERY_PARAM`): URLs
    /// are routinely written to front-proxy access logs, which would leak
    /// the token. Non-browser clients should send `Authorization: Bearer`
    /// instead; browsers use the `cube-terminal.<token>` subprotocol (see
    /// `handshake_credential`). When the flag is off this parameter is
    /// ignored as if absent.
    token: Option<String>,
    /// Target container (ID or name) for multi-container sandboxes.
    /// Defaults to the primary container; see `resolve_envd_port`.
    container: Option<String>,
}

fn clamp_size(value: Option<u32>, default: u32) -> u32 {
    value.unwrap_or(default).clamp(MIN_PTY_SIZE, MAX_PTY_SIZE)
}

/// Pick the target container for a terminal session. Without a selector the
/// primary container (kind == "sandbox", or whose ID equals the sandbox ID)
/// wins, falling back to the first entry; with a selector the container is
/// matched by ID or name.
fn select_container<'a>(
    containers: &'a [SandboxContainer],
    sandbox_id: &str,
    selector: Option<&str>,
) -> Option<&'a SandboxContainer> {
    match selector {
        None => containers
            .iter()
            .find(|c| c.kind == "sandbox" || c.container_id == sandbox_id)
            .or_else(|| containers.first()),
        Some(s) => containers
            .iter()
            .find(|c| c.container_id == s || c.name == s),
    }
}

/// Resolve the envd port for the container selected by `selector`
/// (container ID or name; None = primary container).
///
/// - No selector: the primary container's port, falling back to ENVD_PORT
///   when it carries no `envd_port` (sandboxes created before per-container
///   envd ports existed) or when CubeMaster reports no containers at all.
/// - With a selector: 404 when no container matches, 409 when the match has
///   no terminal endpoint (e.g. a sidecar of a pre-feature sandbox).
fn resolve_envd_port(
    containers: &[SandboxContainer],
    sandbox_id: &str,
    selector: Option<&str>,
) -> AppResult<u16> {
    let container = match select_container(containers, sandbox_id, selector) {
        Some(container) => container,
        None => {
            return match selector {
                // No containers reported at all: keep the legacy
                // single-container behaviour for the default target.
                None => Ok(ENVD_PORT),
                Some(s) => Err(AppError::NotFound(format!(
                    "container {} not found in sandbox {}",
                    s, sandbox_id
                ))),
            };
        }
    };
    match container.envd_port.filter(|port| *port > 0) {
        Some(port) => Ok(port),
        None if selector.is_none() => Ok(ENVD_PORT),
        None => Err(AppError::Conflict(format!(
            "container {} does not expose a terminal endpoint",
            container.container_id
        ))),
    }
}

/// Client → server WebSocket messages.
#[derive(Debug, Deserialize)]
#[serde(tag = "type", rename_all = "lowercase")]
enum ClientMessage {
    /// Base64-encoded bytes written to the PTY.
    Input {
        data: String,
    },
    Resize {
        cols: u32,
        rows: u32,
    },
}

/// Server → client WebSocket messages.
#[derive(Debug, Serialize)]
#[serde(tag = "type", rename_all = "lowercase")]
enum ServerMessage {
    Ready {
        pid: i64,
    },
    /// Base64-encoded bytes read from the PTY.
    Output {
        data: String,
    },
    Exit {
        code: Option<i64>,
    },
    Error {
        message: String,
    },
}

impl ServerMessage {
    fn to_message(&self) -> Message {
        Message::Text(serde_json::to_string(self).unwrap_or_else(|_| "{}".to_string()))
    }
}

/// Shared live-session counters backing the concurrent-session caps (per
/// sandbox and global). Lives on `AppState` so every handler clone sees the
/// same counts. Uses Tokio's async Mutex so contended updates never block a
/// runtime worker thread.
#[derive(Clone, Default)]
pub struct TerminalSessionTracker {
    inner: std::sync::Arc<tokio::sync::Mutex<TerminalSessionCounts>>,
}

#[derive(Default)]
struct TerminalSessionCounts {
    per_sandbox: std::collections::HashMap<String, usize>,
    total: usize,
}

/// Which session cap rejected an `acquire` — used for the 429 message.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum SessionCap {
    PerSandbox,
    Global,
}

impl TerminalSessionTracker {
    /// Take a session slot for `sandbox_id`, or report which cap is full.
    /// The returned guard releases the slot exactly once on drop, covering
    /// every session exit path.
    async fn acquire(
        &self,
        sandbox_id: &str,
        max_per_sandbox: usize,
        max_global: usize,
    ) -> Result<TerminalSessionGuard, SessionCap> {
        let mut inner = self.inner.lock().await;
        if inner.total >= max_global {
            return Err(SessionCap::Global);
        }
        let count = inner.per_sandbox.entry(sandbox_id.to_string()).or_insert(0);
        if *count >= max_per_sandbox {
            return Err(SessionCap::PerSandbox);
        }
        *count += 1;
        inner.total += 1;
        Ok(TerminalSessionGuard {
            tracker: self.clone(),
            sandbox_id: sandbox_id.to_string(),
        })
    }
}

/// RAII holder for one session slot — see `TerminalSessionTracker::acquire`.
struct TerminalSessionGuard {
    tracker: TerminalSessionTracker,
    sandbox_id: String,
}

impl Drop for TerminalSessionGuard {
    fn drop(&mut self) {
        // Drop cannot await. Fast-path the uncontended update; if another
        // task owns the mutex, complete it asynchronously instead of ever
        // blocking a Tokio worker thread.
        if let Ok(mut inner) = self.tracker.inner.try_lock() {
            release_session_slot(&mut inner, &self.sandbox_id);
            return;
        }
        let tracker = self.tracker.clone();
        let sandbox_id = self.sandbox_id.clone();
        tokio::spawn(async move {
            let mut inner = tracker.inner.lock().await;
            release_session_slot(&mut inner, &sandbox_id);
        });
    }
}

fn release_session_slot(inner: &mut TerminalSessionCounts, sandbox_id: &str) {
    inner.total = inner.total.saturating_sub(1);
    if let Some(count) = inner.per_sandbox.get_mut(sandbox_id) {
        *count = count.saturating_sub(1);
        if *count == 0 {
            inner.per_sandbox.remove(sandbox_id);
        }
    }
}

/// Operator identity attached to a terminal session for the audit trail.
/// The two fields are deliberately graded: `user` is authoritative (vouched
/// for by the authorizing party), while `claimed_user` is a self-asserted
/// hint parsed from an *unverified* Bearer JWT — useful for attribution,
/// never proof of identity.
#[derive(Debug, Clone, Default)]
struct TerminalIdentity {
    /// Authoritative operator identity: the auth callback's `X-Auth-User`
    /// response header. Empty in simple-key and open modes.
    user: Option<String>,
    /// Attribution hint from the Bearer JWT's unverified claims
    /// (`username`/`sub`/...), only populated when `user` is empty.
    claimed_user: Option<String>,
}

/// Auth credential offered on the WebSocket handshake, mirroring the
/// Bearer / X-API-Key split of `middleware::auth::unified_auth`: the
/// `cube-terminal.<token>` subprotocol (then the `Authorization: Bearer`
/// header, then — when enabled — the `token` query fallback) maps to
/// Bearer; the `X-API-Key` header maps to an API key.
#[derive(Debug)]
enum TerminalCredential {
    /// `cube-terminal.<token>` subprotocol, `Authorization: Bearer` header,
    /// or (when enabled) `token` query param.
    Bearer(String),
    /// `X-API-Key: <key>` header.
    ApiKey(String),
}

impl TerminalCredential {
    /// The raw credential string, whichever transport it arrived on.
    fn secret(&self) -> &str {
        match self {
            TerminalCredential::Bearer(token) => token,
            TerminalCredential::ApiKey(key) => key,
        }
    }
}

/// Pull the auth credential out of the WebSocket handshake. Bearer
/// transports win over `X-API-Key`, exactly as in `unified_auth`; a Bearer
/// value that trims to empty falls through to the header. Bearer priority:
/// the `cube-terminal.<token>` subprotocol (browser path) first, then the
/// `Authorization: Bearer` header (non-browser clients), then — only when
/// `allow_query_token` is set — the `token` query param (disabled by
/// default because front proxies log URLs).
fn handshake_credential(
    headers: &HeaderMap,
    query_token: Option<String>,
    allow_query_token: bool,
) -> Option<TerminalCredential> {
    let bearer = subprotocol_token(headers)
        .or_else(|| authorization_bearer(headers))
        .or_else(|| query_token.filter(|_| allow_query_token));
    if let Some(token) = bearer {
        let token = token.trim();
        if !token.is_empty() {
            return Some(TerminalCredential::Bearer(token.to_string()));
        }
    }
    headers
        .get("x-api-key")
        .and_then(|v| v.to_str().ok())
        .map(str::trim)
        .filter(|k| !k.is_empty())
        .map(|k| TerminalCredential::ApiKey(k.to_string()))
}

/// The token from the `cube-terminal.<token>` subprotocol (the only
/// credential transport available to browser WebSocket clients). A bare
/// `cube-terminal.` entry carries no token and is treated as absent so the
/// lower-priority transports still apply.
fn subprotocol_token(headers: &HeaderMap) -> Option<String> {
    headers
        .get("sec-websocket-protocol")
        .and_then(|v| v.to_str().ok())
        .and_then(|v| {
            v.split(',')
                .map(str::trim)
                .find_map(|p| p.strip_prefix(TOKEN_SUBPROTOCOL_PREFIX))
                .filter(|t| !t.trim().is_empty())
        })
        .map(str::to_string)
}

/// The token from a standard `Authorization: Bearer <token>` header — the
/// recommended credential transport for non-browser clients (CLI scripts,
/// curl), which can set arbitrary handshake headers anyway.
fn authorization_bearer(headers: &HeaderMap) -> Option<String> {
    let value = headers.get("authorization").and_then(|v| v.to_str().ok())?;
    let (scheme, token) = value.split_once(' ')?;
    if !scheme.eq_ignore_ascii_case("bearer") {
        return None;
    }
    let token = token.trim();
    if token.is_empty() {
        None
    } else {
        Some(token.to_string())
    }
}

/// Whether the client offered the base `cube-terminal` subprotocol. When it
/// did, the server selects it in the 101 response — Chrome aborts the
/// handshake ("error" + close 1006, never firing `open`) when it offered
/// subprotocols but the server selects none, even though declining is legal
/// per RFC 6455. The token-bearing entry is never selected, so the token is
/// not echoed back.
fn offered_base_subprotocol(headers: &HeaderMap) -> bool {
    headers
        .get("sec-websocket-protocol")
        .and_then(|v| v.to_str().ok())
        .is_some_and(|v| {
            v.split(',')
                .map(str::trim)
                .any(|p| p == TERMINAL_SUBPROTOCOL)
        })
}

/// Whether the client offered a token-bearing `cube-terminal.<token>`
/// subprotocol.
fn offered_token_subprotocol(headers: &HeaderMap) -> bool {
    headers
        .get("sec-websocket-protocol")
        .and_then(|v| v.to_str().ok())
        .is_some_and(|v| {
            v.split(',')
                .map(str::trim)
                .any(|p| p.starts_with(TOKEN_SUBPROTOCOL_PREFIX))
        })
}

/// Entry point for the Origin guard. When `allowed_origins` is non-empty
/// the Origin must exactly equal one of the whitelist entries (compared
/// after normalization) and the Host-match fallback is not consulted;
/// otherwise the Origin must match the request Host (see
/// `origin_matches_host`). Clients that send no Origin header (curl,
/// python, CLI) are not checked either way.
fn origin_allowed(headers: &HeaderMap, allowed_origins: &[String]) -> bool {
    let Some(origin) = headers.get("origin").and_then(|v| v.to_str().ok()) else {
        return true;
    };
    if !allowed_origins.is_empty() {
        let Some(origin) = normalize_origin(origin) else {
            return false;
        };
        return allowed_origins
            .iter()
            .filter_map(|entry| normalize_origin(entry))
            .any(|entry| entry == origin);
    }
    origin_matches_host(headers)
}

/// Normalize an Origin for exact whitelist comparison: trim, lowercase the
/// scheme and authority (host comparison is case-insensitive; ports are
/// digits or bracketed IPv6 hex), and drop any path (browsers never send
/// one on an Origin header).
fn normalize_origin(origin: &str) -> Option<String> {
    let (scheme, authority) = origin_parts(origin.trim())?;
    Some(format!(
        "{}://{}",
        scheme.to_lowercase(),
        authority.to_lowercase()
    ))
}

/// CSRF guard for browser clients: when an `Origin` header is present, its
/// hostname must match the request `Host` header (compared
/// case-insensitively). Effective-port rules (a side with no explicit port
/// is read as the Origin scheme's default, 80/443; a Host without a port
/// has no scheme and therefore no default):
///
/// - both sides carry an explicit port → the ports must match;
/// - explicit Origin port vs port-less Host → the Origin port must equal
///   the Origin scheme's default, so a proxy that strips the port from
///   `Host` (nginx `proxy_set_header Host $host`) cannot widen the check
///   to arbitrary same-host services. Proxies on a non-default port must
///   forward the full authority (`proxy_set_header Host $http_host`);
/// - port-less Origin vs explicit Host port → the Host port must equal the
///   Origin scheme's default (e.g. `http://example.com` matches
///   `example.com:80` but NOT `example.com:3000`); an Origin scheme with no
///   default port never matches;
/// - neither side carries a port → the hostname match alone decides.
///
/// Clients that send no Origin header (curl, python, CLI) are not checked.
fn origin_matches_host(headers: &HeaderMap) -> bool {
    let Some(origin) = headers.get("origin").and_then(|v| v.to_str().ok()) else {
        return true;
    };
    let Some(host) = headers.get("host").and_then(|v| v.to_str().ok()) else {
        return false;
    };
    let Some((scheme, authority)) = origin_parts(origin) else {
        return false;
    };
    if authority.eq_ignore_ascii_case(host) {
        return true;
    }
    fn split_port(s: &str) -> (&str, Option<&str>) {
        // Bracketed IPv6 literal without a port (e.g. "[::1]") has no port.
        if s.ends_with(']') {
            return (s, None);
        }
        match s.rsplit_once(':') {
            Some((h, p)) => (h, Some(p)),
            None => (s, None),
        }
    }
    let (o_host, o_port) = split_port(authority);
    let (h_host, h_port) = split_port(host);
    if !o_host.eq_ignore_ascii_case(h_host) {
        return false;
    }
    match (o_port, h_port) {
        (Some(a), Some(b)) => a == b,
        // Explicit Origin port vs port-less Host: the Host then *means* the
        // scheme default port, so only that port may match (see doc
        // comment).
        (Some(port), None) => port.parse::<u16>().ok() == scheme_default_port(scheme),
        // Port-less Origin (i.e. the scheme default port) vs an explicit
        // Host port: the Host port must equal the scheme default. A
        // port-less Origin no longer matches a same-host service on an
        // arbitrary port.
        (None, Some(port)) => scheme_default_port(scheme) == port.parse::<u16>().ok(),
        // Neither side pins a port: the hostname match alone decides. The
        // terminal credential is not ambient (browsers attach it as a
        // WebSocket subprotocol, not as a cookie), so a cross-site WebSocket
        // hijack (CSWSH) already requires the attacker to know the token.
        (None, None) => true,
    }
}

/// Default port for Origin schemes that imply one.
fn scheme_default_port(scheme: &str) -> Option<u16> {
    // Browsers use http(s) in Origin even for WebSocket handshakes. The
    // ws/wss cases keep this normalization useful for non-browser clients.
    if scheme.eq_ignore_ascii_case("http") || scheme.eq_ignore_ascii_case("ws") {
        Some(80)
    } else if scheme.eq_ignore_ascii_case("https") || scheme.eq_ignore_ascii_case("wss") {
        Some(443)
    } else {
        None
    }
}

/// `scheme://authority/path` → `(scheme, authority)` (None for malformed
/// values).
fn origin_parts(origin: &str) -> Option<(&str, &str)> {
    let (scheme, rest) = origin.split_once("://")?;
    let authority = rest.split('/').next()?;
    if authority.is_empty() {
        None
    } else {
        Some((scheme, authority))
    }
}

/// GET /cubeapi/v1/sandboxes/:sandboxID/terminal/ws
///
/// Origin check, auth, sandbox validation and the per-sandbox session cap
/// all happen *before* the WebSocket upgrade so failures get proper HTTP
/// status codes (401/403/404/409/429); the browser only sees a successful
/// upgrade once the session is allowed to start.
pub async fn terminal_ws(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Query(query): Query<TerminalQuery>,
    headers: HeaderMap,
    uri: OriginalUri,
    ws: WebSocketUpgrade,
) -> AppResult<Response> {
    let cols = clamp_size(query.cols, DEFAULT_COLS);
    let rows = clamp_size(query.rows, DEFAULT_ROWS);
    let client_ip = client_ip(&headers);

    // Browser CSRF guard: an Origin that is not whitelisted (when a
    // whitelist is configured) or does not match the request host is
    // rejected before any auth work. Header-less clients skip the check.
    if !origin_allowed(&headers, &state.config.terminal_allowed_origins) {
        audit_log(
            "auth-failure",
            &sandbox_id,
            query.container.as_deref(),
            None,
            &TerminalIdentity::default(),
            client_ip.as_deref(),
            Some("origin-mismatch"),
        );
        return Err(AppError::Forbidden(
            "origin host does not match request host".to_string(),
        ));
    }

    // Fail closed: with no auth backend configured the terminal endpoint is
    // disabled unless explicitly opted in — an unauthenticated terminal is
    // a remote shell, so a default deployment must never expose one.
    let auth_configured = state
        .config
        .auth_callback_url
        .as_deref()
        .is_some_and(|u| !u.is_empty())
        || state
            .config
            .cube_api_key
            .as_deref()
            .is_some_and(|k| !k.is_empty());
    if !auth_configured && !state.config.terminal_allow_unauthenticated {
        audit_log(
            "rejected",
            &sandbox_id,
            query.container.as_deref(),
            None,
            &TerminalIdentity::default(),
            client_ip.as_deref(),
            Some("auth-disabled"),
        );
        return Err(AppError::Forbidden(
            "terminal access is disabled: no auth backend is configured (set AUTH_CALLBACK_URL \
             or CUBE_API_KEY); set TERMINAL_ALLOW_UNAUTHENTICATED=true to allow unauthenticated \
             terminal access"
                .to_string(),
        ));
    }

    let credential = handshake_credential(
        &headers,
        query.token,
        state.config.terminal_token_query_param,
    );
    let identity = match authenticate(&state, credential.as_ref(), uri.path()).await {
        Ok(identity) => identity,
        Err(err) => {
            audit_log(
                "auth-failure",
                &sandbox_id,
                query.container.as_deref(),
                None,
                &TerminalIdentity::default(),
                client_ip.as_deref(),
                None,
            );
            return Err(err);
        }
    };

    // One CubeMaster round-trip yields both the liveness-gate status and the
    // per-container envd ports used to route the session below.
    let detail = match state
        .services
        .sandboxes
        .get_sandbox_runtime_detail(&sandbox_id)
        .await
    {
        Ok(detail) => detail,
        Err(err) => {
            if matches!(err, AppError::NotFound(_)) {
                audit_log(
                    "rejected",
                    &sandbox_id,
                    query.container.as_deref(),
                    None,
                    &identity,
                    client_ip.as_deref(),
                    Some("sandbox-not-found"),
                );
            }
            return Err(err);
        }
    };
    if detail.status != SandboxStatus::Running {
        audit_log(
            "rejected",
            &sandbox_id,
            query.container.as_deref(),
            None,
            &identity,
            client_ip.as_deref(),
            Some("sandbox-not-running"),
        );
        return Err(AppError::Conflict(format!(
            "sandbox {} is not running (status: {:?})",
            sandbox_id, detail.status
        )));
    }

    // Resolve the target container's envd port before the upgrade so an
    // unknown container (404) or one without a terminal endpoint (409) gets
    // a proper HTTP status.
    let envd_port =
        match resolve_envd_port(&detail.containers, &sandbox_id, query.container.as_deref()) {
            Ok(port) => port,
            Err(err) => {
                audit_log(
                    "rejected",
                    &sandbox_id,
                    query.container.as_deref(),
                    None,
                    &identity,
                    client_ip.as_deref(),
                    Some("container-unavailable"),
                );
                return Err(err);
            }
        };
    // Audit trails record the resolved container ID, falling back to the raw
    // selector when it did not match any container.
    let audit_container =
        select_container(&detail.containers, &sandbox_id, query.container.as_deref())
            .map(|c| c.container_id.clone())
            .or_else(|| query.container.clone());

    let max_sessions = state.config.terminal_max_sessions_per_sandbox.max(1);
    let max_global = state.config.terminal_max_sessions_global.max(1);
    let session_guard = match state
        .terminal_sessions
        .acquire(&sandbox_id, max_sessions, max_global)
        .await
    {
        Ok(guard) => guard,
        Err(cap) => {
            audit_log(
                "rejected",
                &sandbox_id,
                query.container.as_deref(),
                None,
                &identity,
                client_ip.as_deref(),
                Some("session-limit"),
            );
            let message = match cap {
                SessionCap::PerSandbox => format!(
                    "sandbox {} already has {} terminal sessions",
                    sandbox_id, max_sessions
                ),
                SessionCap::Global => "global terminal session limit reached".to_string(),
            };
            return Err(AppError::TooManyRequests(message));
        }
    };

    let idle_timeout = Duration::from_secs(state.config.terminal_idle_timeout_secs.max(1));
    let domain = state.config.sandbox_domain.clone();
    // A client offering the token-bearing `cube-terminal.<token>` subprotocol
    // must also offer the token-free base protocol: the server may only
    // select an offered protocol, never selects the token one (so the token
    // is not echoed), and browsers abort the handshake when nothing they
    // offered is selected. Without the base entry no valid selection exists.
    if offered_token_subprotocol(&headers) && !offered_base_subprotocol(&headers) {
        return Err(AppError::BadRequest(
            "offering a cube-terminal.<token> subprotocol requires also offering the base \
             cube-terminal subprotocol"
                .to_string(),
        ));
    }
    // Chrome aborts the handshake when it offered subprotocols but the server
    // selects none, so select the base `cube-terminal` protocol when offered.
    // The token-bearing `cube-terminal.<token>` entry is never selected —
    // the token is read from the request, never echoed (see module header).
    let ws = ws
        .max_message_size(MAX_WS_MESSAGE_SIZE)
        .max_frame_size(MAX_WS_MESSAGE_SIZE);
    let ws = if offered_base_subprotocol(&headers) {
        ws.protocols([TERMINAL_SUBPROTOCOL])
    } else {
        ws
    };
    Ok(ws.on_upgrade(move |socket| {
        let audit_identity = identity.clone();
        let audit_ip = client_ip.clone();
        async move {
            // Held until run_session returns, releasing the session slot
            // on every exit path (disconnect, error, idle timeout).
            let _session_guard = session_guard;
            run_session(
                socket,
                state,
                sandbox_id,
                domain,
                envd_port,
                cols,
                rows,
                idle_timeout,
                audit_container,
                audit_identity,
                audit_ip,
            )
            .await;
        }
    }))
}

/// Validate the handshake credential (subprotocol, `Authorization: Bearer`
/// header, or — when enabled — query token mapped to Bearer, or the
/// `X-API-Key` header — see `handshake_credential`) against whichever auth
/// backend is configured, mirroring `unified_auth`: auth callback first
/// (forwarding the credential on the same header the client used), then the
/// simple API key (`cube_api_key`), then open mode.
///
/// Returns the operator identity for the audit trail, graded by trust (see
/// `TerminalIdentity`):
/// - callback mode: `user` comes only from the callback's `X-Auth-User`
///   response header (the callback is the authorizing party, so an identity
///   it vouches for is authoritative); when absent, `claimed_user` falls
///   back to the unverified claims of the already-authorized Bearer JWT
///   (see `jwt_identity`);
/// - simple-key mode: no identity — a shared key proves no individual;
/// - open mode: no identity — there is no credential at all.
async fn authenticate(
    state: &AppState,
    credential: Option<&TerminalCredential>,
    request_path: &str,
) -> AppResult<TerminalIdentity> {
    const MISSING_CREDENTIAL: &str = "Missing authentication token (cube-terminal subprotocol, \
         Authorization Bearer header, or X-API-Key header)";

    if let Some(callback_url) = state
        .config
        .auth_callback_url
        .as_deref()
        .filter(|u| !u.is_empty())
    {
        let credential =
            credential.ok_or_else(|| AppError::Unauthorized(MISSING_CREDENTIAL.to_string()))?;
        // The credential is forwarded verbatim into a request header below.
        // A query token carrying control characters (e.g. a percent-decoded
        // newline) would fail reqwest's header builder and surface as a 500
        // "Auth callback unreachable" — reject it as a client error instead.
        if !is_forwardable_header_value(credential.secret()) {
            return Err(AppError::Unauthorized(
                "Invalid authentication token".to_string(),
            ));
        }
        let req = state
            .http_client
            .post(callback_url)
            .header("X-Request-Path", request_path)
            .header("X-Request-Method", "GET");
        // Forward the credential on the same header the client used, like
        // `unified_auth` does, so the callback sees no behavioral difference
        // between the terminal and the plain HTTP routes.
        let req = match credential {
            TerminalCredential::Bearer(token) => {
                req.header("Authorization", format!("Bearer {}", token))
            }
            TerminalCredential::ApiKey(key) => req.header("X-API-Key", key),
        };
        // A hung callback must not park the handshake forever; the deadline
        // matches the one unified_auth applies on the HTTP routes.
        let resp = tokio::time::timeout(AUTH_CALLBACK_TIMEOUT, req.send())
            .await
            .map_err(|_| {
                tracing::error!(callback_url = %callback_url, "auth callback request timed out");
                AppError::Internal(anyhow::anyhow!(
                    "Auth callback timed out after {:?}",
                    AUTH_CALLBACK_TIMEOUT
                ))
            })?
            .map_err(|e| {
                tracing::error!(error = %e, callback_url = %callback_url, "auth callback request failed");
                AppError::Internal(anyhow::anyhow!("Auth callback unreachable: {}", e))
            })?;
        if resp.status().as_u16() == 200 {
            // The callback authorized the request; the only authoritative
            // identity is the one it names explicitly. The Bearer JWT's
            // unverified claims are a fallback attribution hint, kept out
            // of the authoritative `user` field.
            let user = callback_identity(&resp);
            let claimed_user = if user.is_none() {
                jwt_identity(credential)
            } else {
                None
            };
            return Ok(TerminalIdentity { user, claimed_user });
        }
        return Err(AppError::Unauthorized(
            "Authentication rejected by callback".to_string(),
        ));
    }

    if let Some(expected_key) = state
        .config
        .cube_api_key
        .as_deref()
        .filter(|k| !k.is_empty())
    {
        let credential =
            credential.ok_or_else(|| AppError::Unauthorized(MISSING_CREDENTIAL.to_string()))?;
        if !is_forwardable_header_value(credential.secret()) {
            return Err(AppError::Unauthorized(
                "Invalid API key or token".to_string(),
            ));
        }
        if !constant_time_eq(credential.secret(), expected_key) {
            return Err(AppError::Unauthorized(
                "Invalid API key or token".to_string(),
            ));
        }
        // Simple-key mode only proves knowledge of the shared key; the
        // caller's identity is not known to CubeAPI in this mode.
        return Ok(TerminalIdentity::default());
    }

    Ok(TerminalIdentity::default())
}

/// Trim, reject empty, and defensively truncate an operator identity
/// string before it enters a tracing field.
fn sanitize_identity(value: &str) -> Option<String> {
    let trimmed = value.trim();
    if trimmed.is_empty() {
        return None;
    }
    Some(trimmed.chars().take(MAX_IDENTITY_LEN).collect())
}

/// Operator identity named by the auth callback via the `X-Auth-User`
/// response header — the preferred source: the callback is the authorizing
/// party, so an identity it vouches for is inherently trusted.
fn callback_identity(resp: &reqwest::Response) -> Option<String> {
    resp.headers()
        .get(AUTH_USER_HEADER)?
        .to_str()
        .ok()
        .and_then(sanitize_identity)
}

/// Best-effort operator identity from a Bearer credential that looks like a
/// JWT (three `.`-separated segments): base64url-decode the payload and take
/// the first non-empty string among `username`, `sub`, `preferred_username`,
/// `name`. The signature is NOT verified — this is audit attribution only,
/// and it is safe because the callback already authorized the request based
/// on this very token: logging a claimed identity grants the caller no
/// additional privilege. A non-JWT token, an undecodable payload, or a
/// payload without usable claims all yield None — never an error, the
/// request is never rejected over identity extraction.
fn jwt_identity(credential: &TerminalCredential) -> Option<String> {
    let TerminalCredential::Bearer(token) = credential else {
        return None;
    };
    let mut segments = token.split('.');
    let (Some(_header), Some(payload), Some(_signature)) =
        (segments.next(), segments.next(), segments.next())
    else {
        return None;
    };
    let decoded = URL_SAFE_NO_PAD.decode(payload).ok()?;
    let claims: Value = serde_json::from_slice(&decoded).ok()?;
    ["username", "sub", "preferred_username", "name"]
        .iter()
        .filter_map(|key| claims.get(key).and_then(Value::as_str))
        .find_map(sanitize_identity)
}

/// Whether `value` survives being placed into an HTTP header verbatim:
/// printable ASCII (space through `~`), no control characters or DEL.
fn is_forwardable_header_value(value: &str) -> bool {
    !value.is_empty() && value.bytes().all(|b| (0x20..0x7f).contains(&b))
}

fn client_ip(headers: &HeaderMap) -> Option<String> {
    headers
        .get("x-forwarded-for")
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.split(',').next())
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .map(str::to_string)
        .or_else(|| {
            headers
                .get("x-real-ip")
                .and_then(|v| v.to_str().ok())
                .map(str::trim)
                .filter(|v| !v.is_empty())
                .map(str::to_string)
        })
}

fn audit_log(
    event: &str,
    sandbox_id: &str,
    container: Option<&str>,
    pid: Option<i64>,
    identity: &TerminalIdentity,
    client_ip: Option<&str>,
    reason: Option<&str>,
) {
    // `identity_source` states how much the logged identity is worth:
    // "auth_callback" (authoritative, from the callback's X-Auth-User),
    // "unverified_jwt_claim" (self-asserted, from an unverified JWT), or ""
    // (no identity at all).
    let identity_source = if identity.user.is_some() {
        "auth_callback"
    } else if identity.claimed_user.is_some() {
        "unverified_jwt_claim"
    } else {
        ""
    };
    tracing::info!(
        event = event,
        sandbox_id = sandbox_id,
        container = container.unwrap_or(""),
        pid = pid,
        user = identity.user.as_deref().unwrap_or(""),
        claimed_user = identity.claimed_user.as_deref().unwrap_or(""),
        identity_source = identity_source,
        client_ip = client_ip.unwrap_or(""),
        reason = reason.unwrap_or(""),
        "terminal session"
    );
}

/// One Connect-RPC envelope read from the envd response stream.
struct ConnectFrame {
    flags: u8,
    payload: Vec<u8>,
}

impl ConnectFrame {
    fn is_end_stream(&self) -> bool {
        self.flags & 0b10 != 0
    }
}

/// Incremental reader for Connect-RPC streaming responses (1 byte flags +
/// 4 bytes big-endian length + JSON payload per envelope).
struct ConnectFrameReader {
    resp: reqwest::Response,
    buf: Vec<u8>,
    eof: bool,
}

impl ConnectFrameReader {
    fn new(resp: reqwest::Response) -> Self {
        Self {
            resp,
            buf: Vec::new(),
            eof: false,
        }
    }

    async fn next_frame(&mut self) -> AppResult<Option<ConnectFrame>> {
        loop {
            if self.buf.len() >= 5 {
                let len = u32::from_be_bytes([self.buf[1], self.buf[2], self.buf[3], self.buf[4]])
                    as usize;
                // A hostile or broken envd could claim a huge frame and make
                // us buffer it unboundedly; treat oversize frames as a
                // stream error so the session takes the normal teardown path.
                if len > MAX_ENVD_FRAME_SIZE {
                    return Err(AppError::Internal(anyhow::anyhow!(
                        "envd terminal frame exceeds {} byte cap",
                        MAX_ENVD_FRAME_SIZE
                    )));
                }
                if self.buf.len() >= 5 + len {
                    let frame = ConnectFrame {
                        flags: self.buf[0],
                        payload: self.buf[5..5 + len].to_vec(),
                    };
                    self.buf.drain(..5 + len);
                    return Ok(Some(frame));
                }
            }
            if self.eof {
                if self.buf.is_empty() {
                    return Ok(None);
                }
                return Err(AppError::Internal(anyhow::anyhow!(
                    "truncated envd terminal stream"
                )));
            }
            match self.resp.chunk().await {
                Ok(Some(chunk)) => self.buf.extend_from_slice(&chunk),
                Ok(None) => self.eof = true,
                Err(e) => {
                    return Err(AppError::Internal(anyhow::anyhow!(
                        "failed reading envd terminal stream: {}",
                        e
                    )))
                }
            }
        }
    }
}

fn envd_url(state: &AppState, method: &str) -> String {
    format!(
        "{}/process.Process/{}",
        state.config.sandbox_proxy_url.trim_end_matches('/'),
        method
    )
}

/// Unary envd call (SendInput / Update / SendSignal): plain JSON, no Connect
/// envelope. Failures are best-effort by design — a 404 simply means the
/// process already exited — so callers log and carry on.
async fn envd_unary(state: &AppState, host: &str, method: &str, body: Value) -> AppResult<()> {
    let resp = tokio::time::timeout(
        ENVD_CALL_TIMEOUT,
        state
            .http_client
            .post(envd_url(state, method))
            .header("Host", host)
            .header("Content-Type", "application/json")
            .header("Connect-Protocol-Version", "1")
            .header("Authorization", ENVD_BASIC_AUTH)
            .json(&body)
            .send(),
    )
    .await
    .map_err(|_| {
        AppError::Internal(anyhow::anyhow!(
            "envd {} request timed out after {:?}",
            method,
            ENVD_CALL_TIMEOUT
        ))
    })?
    .map_err(|e| AppError::Internal(anyhow::anyhow!("envd {} request failed: {}", method, e)))?;

    if !resp.status().is_success() {
        return Err(AppError::Internal(anyhow::anyhow!(
            "envd {} returned HTTP {}",
            method,
            resp.status()
        )));
    }
    Ok(())
}

/// POST process.Process/Start and return the streaming frame reader. The
/// request carries a unique `tag` so a shell whose start event never reaches
/// us (timeout / truncated stream) can still be found and killed afterwards
/// — see `reap_shell_by_tag`.
async fn envd_start(
    state: &AppState,
    host: &str,
    cols: u32,
    rows: u32,
    tag: &str,
) -> AppResult<ConnectFrameReader> {
    let payload = json!({
        "process": {
            // `/bin/sh` is the portable entry point. It prefers an
            // interactive login bash when installed, then replaces itself
            // with an interactive POSIX shell on minimal images.
            "cmd": "/bin/sh",
            "args": ["-c", "exec /bin/bash -il 2>/dev/null || exec /bin/sh -i"],
            "envs": {
                "TERM": "xterm-256color",
                "LANG": "C.UTF-8",
                "LC_ALL": "C.UTF-8",
            },
        },
        "pty": { "size": { "rows": rows, "cols": cols } },
        "tag": tag,
    });
    let body = connect_envelope(&serde_json::to_vec(&payload).map_err(anyhow::Error::from)?);

    let resp = tokio::time::timeout(
        ENVD_CALL_TIMEOUT,
        state
            .http_client
            .post(envd_url(state, "Start"))
            .header("Host", host)
            .header("Content-Type", CONNECT_JSON)
            .header("Connect-Protocol-Version", "1")
            .header("Connect-Content-Encoding", "identity")
            .header("Connect-Timeout-Ms", ENVD_STREAM_TIMEOUT_MS)
            .header("Authorization", ENVD_BASIC_AUTH)
            .body(body)
            .send(),
    )
    .await
    .map_err(|_| {
        AppError::Internal(anyhow::anyhow!(
            "envd terminal start request timed out after {:?}",
            ENVD_CALL_TIMEOUT
        ))
    })?
    .map_err(|e| {
        AppError::Internal(anyhow::anyhow!("envd terminal start request failed: {}", e))
    })?;

    if !resp.status().is_success() {
        return Err(AppError::Internal(anyhow::anyhow!(
            "envd terminal start returned HTTP {}",
            resp.status()
        )));
    }
    Ok(ConnectFrameReader::new(resp))
}

fn connect_envelope(payload: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(payload.len() + 5);
    out.push(0);
    out.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    out.extend_from_slice(payload);
    out
}

/// What a single envd data frame means for the terminal client.
enum EnvdEvent {
    Output(String),
    Exit(Option<i64>),
}

/// Parse a non-end-stream envelope payload. Returns Ok(None) for frames that
/// carry no terminal-relevant event (keepalives, stdout/stderr data in
/// non-PTY mode, the start event once consumed).
fn parse_data_frame(payload: &[u8]) -> AppResult<Option<EnvdEvent>> {
    let v: Value = serde_json::from_slice(payload)
        .map_err(|e| AppError::Internal(anyhow::anyhow!("invalid envd JSON event: {}", e)))?;
    let Some(event) = v.get("event") else {
        return Ok(None);
    };
    if let Some(pty) = event
        .get("data")
        .and_then(|d| d.get("pty"))
        .and_then(Value::as_str)
    {
        return Ok(Some(EnvdEvent::Output(pty.to_string())));
    }
    if let Some(end) = event.get("end") {
        let code = end
            .get("exitCode")
            .and_then(Value::as_i64)
            .or_else(|| parse_exit_status(end.get("status").and_then(Value::as_str)));
        return Ok(Some(EnvdEvent::Exit(code)));
    }
    Ok(None)
}

fn parse_exit_status(status: Option<&str>) -> Option<i64> {
    status?
        .strip_prefix("exit status ")
        .and_then(|v| v.trim().parse::<i64>().ok())
}

/// Extract the end-stream error message, if the final envelope carries one.
fn end_stream_error(payload: &[u8]) -> Option<String> {
    let v: Value = serde_json::from_slice(payload).ok()?;
    v.get("error").map(|e| e.to_string())
}

/// Wait for envd's `{"event":{"start":{"pid":N}}}` frame.
async fn wait_for_start(reader: &mut ConnectFrameReader) -> AppResult<i64> {
    let wait = async {
        loop {
            let Some(frame) = reader.next_frame().await? else {
                return Err(AppError::Internal(anyhow::anyhow!(
                    "envd terminal stream ended before start event"
                )));
            };
            if frame.is_end_stream() {
                let detail = end_stream_error(&frame.payload)
                    .unwrap_or_else(|| "envd ended the terminal stream".to_string());
                return Err(AppError::Internal(anyhow::anyhow!(
                    "envd terminal start failed: {}",
                    detail
                )));
            }
            let v: Value = serde_json::from_slice(&frame.payload).map_err(|e| {
                AppError::Internal(anyhow::anyhow!("invalid envd JSON event: {}", e))
            })?;
            if let Some(pid) = v
                .get("event")
                .and_then(|e| e.get("start"))
                .and_then(|s| s.get("pid"))
                .and_then(Value::as_i64)
            {
                return Ok(pid);
            }
        }
    };
    tokio::time::timeout(START_EVENT_TIMEOUT, wait)
        .await
        .map_err(|_| {
            AppError::Internal(anyhow::anyhow!("timed out waiting for envd start event"))
        })?
}

/// Best-effort cleanup for the failure window where envd may have spawned
/// the shell but the start event (with its pid) never reached us — the Start
/// request timed out before response headers, `wait_for_start` timed out, or
/// the stream was truncated. Reconnect with `Connect(process.tag)` (envd
/// resolves the tag selector to the running process), then SIGKILL the
/// recovered pid. envd builds without `ProcessSelector.tag` reject the
/// reconnect and the shell keeps running — the known gap documented in the
/// module header. Every step is deadline-bounded (ENVD_CALL_TIMEOUT /
/// START_EVENT_TIMEOUT).
async fn reap_shell_by_tag(state: &AppState, host: &str, sandbox_id: &str, tag: &str) {
    match envd_pid_by_tag(state, host, tag).await {
        Ok(Some(pid)) => {
            if let Err(err) = envd_unary(
                state,
                host,
                "SendSignal",
                json!({"process": {"pid": pid}, "signal": "SIGNAL_SIGKILL"}),
            )
            .await
            {
                tracing::debug!(sandbox_id = %sandbox_id, pid = pid, error = %err, "terminal: orphan SIGKILL failed");
            } else {
                tracing::info!(sandbox_id = %sandbox_id, pid = pid, "terminal: reaped orphaned shell via tag reconnect");
            }
        }
        // envd has no process for the tag: the shell never started, nothing
        // to reap.
        Ok(None) => {}
        Err(err) => {
            tracing::debug!(sandbox_id = %sandbox_id, error = %err, "terminal: tag reconnect failed, shell may be orphaned");
        }
    }
}

/// `Connect(process.tag)` and read the start event to recover the pid.
/// Ok(None) when envd reports the tag unknown (HTTP 404).
async fn envd_pid_by_tag(state: &AppState, host: &str, tag: &str) -> AppResult<Option<i64>> {
    let payload = json!({"process": {"tag": tag}});
    let body = connect_envelope(&serde_json::to_vec(&payload).map_err(anyhow::Error::from)?);
    let resp = tokio::time::timeout(
        ENVD_CALL_TIMEOUT,
        state
            .http_client
            .post(envd_url(state, "Connect"))
            .header("Host", host)
            .header("Content-Type", CONNECT_JSON)
            .header("Connect-Protocol-Version", "1")
            .header("Connect-Content-Encoding", "identity")
            .header("Authorization", ENVD_BASIC_AUTH)
            .body(body)
            .send(),
    )
    .await
    .map_err(|_| {
        AppError::Internal(anyhow::anyhow!(
            "envd connect-by-tag request timed out after {:?}",
            ENVD_CALL_TIMEOUT
        ))
    })?
    .map_err(|e| {
        AppError::Internal(anyhow::anyhow!("envd connect-by-tag request failed: {}", e))
    })?;
    if resp.status() == reqwest::StatusCode::NOT_FOUND {
        return Ok(None);
    }
    if !resp.status().is_success() {
        return Err(AppError::Internal(anyhow::anyhow!(
            "envd connect-by-tag returned HTTP {}",
            resp.status()
        )));
    }
    let mut reader = ConnectFrameReader::new(resp);
    wait_for_start(&mut reader).await.map(Some)
}

/// Why a session is being torn down; recorded in the audit log.
#[derive(Clone, Copy, PartialEq, Eq)]
enum CloseReason {
    ClientDisconnect,
    EnvdExit,
    IdleTimeout,
    Error,
}

impl CloseReason {
    fn as_str(&self) -> &'static str {
        match self {
            CloseReason::ClientDisconnect => "client-disconnect",
            CloseReason::EnvdExit => "envd-exit",
            CloseReason::IdleTimeout => "idle-timeout",
            CloseReason::Error => "error",
        }
    }
}

/// Send one client-bound message with a hard deadline. Returns false when
/// the send fails or times out — the caller must end the session so a
/// client that stopped reading cannot pin the writer.
async fn ws_send(tx: &mut futures::stream::SplitSink<WebSocket, Message>, msg: Message) -> bool {
    match tokio::time::timeout(WS_SEND_TIMEOUT, tx.send(msg)).await {
        Ok(Ok(())) => true,
        Ok(Err(err)) => {
            tracing::debug!(error = %err, "terminal: client send failed");
            false
        }
        Err(_) => {
            tracing::warn!("terminal: client send timed out");
            false
        }
    }
}

/// Best-effort close handshake, deadline-bound for the same reason as
/// `ws_send`.
async fn ws_close(tx: &mut futures::stream::SplitSink<WebSocket, Message>) {
    let _ = tokio::time::timeout(WS_SEND_TIMEOUT, tx.close()).await;
}

/// Forward one envd PTY data payload (base64) to the client, re-chunked so
/// every WebSocket message stays within `MAX_WS_MESSAGE_SIZE`: decode, split
/// the raw bytes into `OUTPUT_CHUNK_SIZE` pieces, and base64-encode each
/// piece separately (splitting the base64 text itself could leave a client
/// that decodes per message with a non-aligned chunk). A payload that fails
/// to decode is forwarded as-is — envd should never send one, and the client
/// surfaces the decode error.
async fn send_pty_output(
    tx: &mut futures::stream::SplitSink<WebSocket, Message>,
    data: &str,
) -> bool {
    let Ok(raw) = BASE64.decode(data) else {
        return ws_send(
            tx,
            ServerMessage::Output {
                data: data.to_string(),
            }
            .to_message(),
        )
        .await;
    };
    for chunk in raw.chunks(OUTPUT_CHUNK_SIZE) {
        if !ws_send(
            tx,
            ServerMessage::Output {
                data: BASE64.encode(chunk),
            }
            .to_message(),
        )
        .await
        {
            return false;
        }
    }
    true
}

#[allow(clippy::too_many_arguments)]
async fn run_session(
    socket: WebSocket,
    state: AppState,
    sandbox_id: String,
    domain: String,
    envd_port: u16,
    cols: u32,
    rows: u32,
    idle_timeout: Duration,
    container: Option<String>,
    identity: TerminalIdentity,
    client_ip: Option<String>,
) {
    // CubeProxy routes by Host header: "<envd-port>-<sandbox-id>.<domain>".
    let host = format!("{}-{}.{}", envd_port, sandbox_id, domain);
    let (mut ws_tx, mut ws_rx) = socket.split();

    // Unique tag on the Start request: if envd spawns the shell but the
    // start event (with the pid) never reaches us, `reap_shell_by_tag` uses
    // it to find and kill the orphaned process.
    let tag = format!("cubeapi-terminal-{}", uuid::Uuid::new_v4());

    let mut reader = match envd_start(&state, &host, cols, rows, &tag).await {
        Ok(reader) => reader,
        Err(err) => {
            tracing::warn!(sandbox_id = %sandbox_id, error = %err, "terminal: envd start failed");
            let _ = ws_send(
                &mut ws_tx,
                ServerMessage::Error {
                    message: err.to_string(),
                }
                .to_message(),
            )
            .await;
            ws_close(&mut ws_tx).await;
            reap_shell_by_tag(&state, &host, &sandbox_id, &tag).await;
            audit_log(
                "close",
                &sandbox_id,
                container.as_deref(),
                None,
                &identity,
                client_ip.as_deref(),
                Some(CloseReason::Error.as_str()),
            );
            return;
        }
    };

    let pid = match wait_for_start(&mut reader).await {
        Ok(pid) => pid,
        Err(err) => {
            tracing::warn!(sandbox_id = %sandbox_id, error = %err, "terminal: no start event");
            drop(reader);
            let _ = ws_send(
                &mut ws_tx,
                ServerMessage::Error {
                    message: err.to_string(),
                }
                .to_message(),
            )
            .await;
            ws_close(&mut ws_tx).await;
            reap_shell_by_tag(&state, &host, &sandbox_id, &tag).await;
            audit_log(
                "close",
                &sandbox_id,
                container.as_deref(),
                None,
                &identity,
                client_ip.as_deref(),
                Some(CloseReason::Error.as_str()),
            );
            return;
        }
    };

    // The client vanished between the upgrade and the Ready frame: run the
    // same teardown as any other exit path so the shell is not left running.
    if !ws_send(&mut ws_tx, ServerMessage::Ready { pid }.to_message()).await {
        drop(reader);
        teardown_session(
            &state,
            &host,
            &sandbox_id,
            pid,
            &mut ws_tx,
            CloseReason::ClientDisconnect,
            container.as_deref(),
            &identity,
            client_ip.as_deref(),
        )
        .await;
        return;
    }
    audit_log(
        "open",
        &sandbox_id,
        container.as_deref(),
        Some(pid),
        &identity,
        client_ip.as_deref(),
        None,
    );

    let reason = pump_loop(
        &state,
        &host,
        pid,
        &mut reader,
        &mut ws_tx,
        &mut ws_rx,
        idle_timeout,
    )
    .await;

    if reason == CloseReason::IdleTimeout {
        audit_log(
            "timeout",
            &sandbox_id,
            container.as_deref(),
            Some(pid),
            &identity,
            client_ip.as_deref(),
            None,
        );
    }

    drop(reader);
    teardown_session(
        &state,
        &host,
        &sandbox_id,
        pid,
        &mut ws_tx,
        reason,
        container.as_deref(),
        &identity,
        client_ip.as_deref(),
    )
    .await;
}

/// Shared session teardown: best-effort SIGKILL of the shell (a 404 just
/// means the process already exited), close handshake, and the audit close
/// record. Runs on every exit path after the shell has started so the
/// sandbox never keeps an orphaned shell.
#[allow(clippy::too_many_arguments)]
async fn teardown_session(
    state: &AppState,
    host: &str,
    sandbox_id: &str,
    pid: i64,
    ws_tx: &mut futures::stream::SplitSink<WebSocket, Message>,
    reason: CloseReason,
    container: Option<&str>,
    identity: &TerminalIdentity,
    client_ip: Option<&str>,
) {
    if let Err(err) = envd_unary(
        state,
        host,
        "SendSignal",
        json!({"process": {"pid": pid}, "signal": "SIGNAL_SIGKILL"}),
    )
    .await
    {
        tracing::debug!(sandbox_id = %sandbox_id, pid = pid, error = %err, "terminal: SIGKILL failed");
    }
    let _ = ws_send(ws_tx, Message::Close(None)).await;
    ws_close(ws_tx).await;
    audit_log(
        "close",
        sandbox_id,
        container,
        Some(pid),
        identity,
        client_ip,
        Some(reason.as_str()),
    );
}

#[allow(clippy::too_many_arguments)]
async fn pump_loop(
    state: &AppState,
    host: &str,
    pid: i64,
    reader: &mut ConnectFrameReader,
    ws_tx: &mut futures::stream::SplitSink<WebSocket, Message>,
    ws_rx: &mut futures::stream::SplitStream<WebSocket>,
    idle_timeout: Duration,
) -> CloseReason {
    // The idle timer only reaps truly dormant sessions: any client message
    // or shell output resets it, so a session actively streaming output
    // (tail -f, a running build) is not cut off just because the user is
    // watching rather than typing.
    let idle = tokio::time::sleep(idle_timeout);
    tokio::pin!(idle);
    let reset_idle = |idle: std::pin::Pin<&mut tokio::time::Sleep>| {
        idle.reset(tokio::time::Instant::now() + idle_timeout);
    };
    loop {
        tokio::select! {
            frame = reader.next_frame() => {
                match frame {
                    Ok(Some(frame)) if frame.is_end_stream() => {
                        if let Some(detail) = end_stream_error(&frame.payload) {
                            let _ = ws_send(ws_tx, ServerMessage::Error { message: detail }.to_message()).await;
                        }
                        return CloseReason::EnvdExit;
                    }
                    Ok(Some(frame)) => match parse_data_frame(&frame.payload) {
                        Ok(Some(EnvdEvent::Output(data))) => {
                            reset_idle(idle.as_mut());
                            if !send_pty_output(ws_tx, &data).await {
                                return CloseReason::ClientDisconnect;
                            }
                        }
                        Ok(Some(EnvdEvent::Exit(code))) => {
                            let _ = ws_send(ws_tx, ServerMessage::Exit { code }.to_message()).await;
                            return CloseReason::EnvdExit;
                        }
                        Ok(None) => {}
                        Err(err) => {
                            tracing::warn!(error = %err, "terminal: bad envd frame");
                        }
                    },
                    // envd closed the stream without an end event.
                    Ok(None) => {
                        let _ = ws_send(ws_tx, ServerMessage::Exit { code: None }.to_message()).await;
                        return CloseReason::EnvdExit;
                    }
                    Err(err) => {
                        let _ = ws_send(ws_tx, ServerMessage::Error { message: err.to_string() }.to_message()).await;
                        return CloseReason::Error;
                    }
                }
            }
            msg = ws_rx.next() => {
                match msg {
                    // Client went away (close frame or dropped connection).
                    None => return CloseReason::ClientDisconnect,
                    Some(Ok(Message::Close(_))) => return CloseReason::ClientDisconnect,
                    // Read error (e.g. a message over the 64 KiB cap):
                    // best-effort error frame, then end the session.
                    Some(Err(err)) => {
                        let _ = ws_send(
                            ws_tx,
                            ServerMessage::Error {
                                message: format!("connection error: {}", err),
                            }
                            .to_message(),
                        )
                        .await;
                        return CloseReason::ClientDisconnect;
                    }
                    Some(Ok(Message::Text(text))) => {
                        reset_idle(idle.as_mut());
                        if let Err(err) = handle_client_message(state, host, pid, &text).await {
                            tracing::warn!(pid = pid, error = %err, "terminal: envd call failed");
                            let _ = ws_send(
                                ws_tx,
                                ServerMessage::Error {
                                    message: format!("terminal process is unavailable: {}", err),
                                }
                                .to_message(),
                            )
                            .await;
                            return CloseReason::Error;
                        }
                    }
                    // Binary / ping / pong frames carry no terminal meaning,
                    // but still count as client liveness.
                    Some(Ok(_)) => reset_idle(idle.as_mut()),
                }
            }
            () = &mut idle => {
                let _ = ws_send(ws_tx, ServerMessage::Error {
                    message: format!(
                        "terminal session timed out after {}s of inactivity",
                        idle_timeout.as_secs()
                    ),
                }
                .to_message())
                .await;
                return CloseReason::IdleTimeout;
            }
        }
    }
}

/// Handle one client text message. Malformed messages are ignored; envd
/// failures are returned so the client can be told the terminal is gone.
async fn handle_client_message(
    state: &AppState,
    host: &str,
    pid: i64,
    text: &str,
) -> AppResult<()> {
    let msg: ClientMessage = match serde_json::from_str(text) {
        Ok(msg) => msg,
        Err(_) => return Ok(()),
    };
    let result = match msg {
        ClientMessage::Input { data } => {
            envd_unary(
                state,
                host,
                "SendInput",
                json!({"process": {"pid": pid}, "input": {"pty": data}}),
            )
            .await
        }
        ClientMessage::Resize { cols, rows } => {
            envd_unary(
                state,
                host,
                "Update",
                json!({
                    "process": {"pid": pid},
                    "pty": {"size": {
                        "rows": clamp_size(Some(rows), DEFAULT_ROWS),
                        "cols": clamp_size(Some(cols), DEFAULT_COLS),
                    }},
                }),
            )
            .await
        }
    };
    result
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        config::ServerConfig,
        logging::{arc, noop::NoopLogger},
        routes::build_router,
    };
    use axum::{
        body::{Body, Bytes},
        extract::State as AxumState,
        http::StatusCode,
        response::IntoResponse,
        routing::{get, post},
        Json, Router,
    };
    use base64::engine::general_purpose::STANDARD as BASE64;
    use futures::channel::mpsc::{unbounded, UnboundedSender};
    use serde_json::{json, Value};
    use std::sync::Arc;
    use tokio::sync::Mutex;
    use tokio_tungstenite::{connect_async, tungstenite};

    const SANDBOX_ID: &str = "sb-terminal-test";
    const MOCK_PID: i64 = 4321;
    const STATUS_RUNNING: i32 = 1;
    const STATUS_PAUSED: i32 = 5;
    const RET_CODE_NOT_FOUND: i32 = 130404;

    // ── Mock envd (Connect-RPC over HTTP) ─────────────────────────────────

    #[derive(Default)]
    struct EnvdSpy {
        start_payload: Option<Value>,
        connect: Vec<Value>,
        send_input: Vec<Value>,
        update: Vec<Value>,
        send_signal: Vec<Value>,
    }

    type FrameTx = UnboundedSender<Result<Vec<u8>, std::io::Error>>;

    #[derive(Clone, Default)]
    struct MockEnvd {
        spy: Arc<Mutex<EnvdSpy>>,
        stream_tx: Arc<Mutex<Option<FrameTx>>>,
        /// When set, SendInput never responds, simulating a hung envd.
        hang_send_input: Arc<std::sync::atomic::AtomicBool>,
        /// When set, Start accepts the request (recording the payload, like a
        /// real envd that already spawned the process) but never responds,
        /// simulating the hung-start orphan window.
        hang_start: Arc<std::sync::atomic::AtomicBool>,
        /// When set, the Start stream ends immediately without a start event,
        /// simulating a truncated stream after the process was spawned.
        close_stream_without_start: Arc<std::sync::atomic::AtomicBool>,
    }

    impl MockEnvd {
        async fn push_frame(&self, payload: Value) {
            if let Some(tx) = &*self.stream_tx.lock().await {
                let bytes = serde_json::to_vec(&payload).expect("frame JSON");
                let _ = tx.unbounded_send(Ok(connect_envelope(&bytes)));
            }
        }

        /// Push raw bytes onto the Start stream (e.g. a malformed envelope).
        async fn push_raw(&self, bytes: Vec<u8>) {
            if let Some(tx) = &*self.stream_tx.lock().await {
                let _ = tx.unbounded_send(Ok(bytes));
            }
        }
    }

    async fn envd_start_handler(
        AxumState(mock): AxumState<MockEnvd>,
        body: Bytes,
    ) -> impl IntoResponse {
        // The request must be exactly one Connect envelope (flags=0).
        assert!(
            body.len() >= 5,
            "start request must carry a Connect envelope"
        );
        let len = u32::from_be_bytes([body[1], body[2], body[3], body[4]]) as usize;
        assert_eq!(body[0], 0, "start envelope flags must be 0");
        assert_eq!(len, body.len() - 5, "start envelope length must match body");
        let payload: Value = serde_json::from_slice(&body[5..]).expect("start payload JSON");
        mock.spy.lock().await.start_payload = Some(payload);

        if mock.hang_start.load(std::sync::atomic::Ordering::Relaxed) {
            // A hung envd never answers; the client's ENVD_CALL_TIMEOUT is
            // what unblocks the session (and triggers the tag-based orphan
            // cleanup, which this mock answers via the Connect route).
            std::future::pending::<()>().await;
        }

        let (tx, rx) = unbounded::<Result<Vec<u8>, std::io::Error>>();
        if mock
            .close_stream_without_start
            .load(std::sync::atomic::Ordering::Relaxed)
        {
            // Drop the sender: the response stream ends before any start
            // event, like a truncated envd stream.
            drop(tx);
            return (
                [(axum::http::header::CONTENT_TYPE, CONNECT_JSON)],
                Body::from_stream(rx),
            );
        }
        *mock.stream_tx.lock().await = Some(tx.clone());
        let start = serde_json::to_vec(&json!({"event": {"start": {"pid": MOCK_PID}}}))
            .expect("start event JSON");
        let _ = tx.unbounded_send(Ok(connect_envelope(&start)));

        (
            [(axum::http::header::CONTENT_TYPE, CONNECT_JSON)],
            Body::from_stream(rx),
        )
    }

    /// envd `process.Process/Connect`: resolve a tag/pid selector to the
    /// running process, answer with its start event, then end the stream.
    async fn envd_connect_handler(
        AxumState(mock): AxumState<MockEnvd>,
        body: Bytes,
    ) -> impl IntoResponse {
        assert!(
            body.len() >= 5,
            "connect request must carry a Connect envelope"
        );
        let payload: Value = serde_json::from_slice(&body[5..]).expect("connect payload JSON");
        mock.spy.lock().await.connect.push(payload);

        let (tx, rx) = unbounded::<Result<Vec<u8>, std::io::Error>>();
        let start = serde_json::to_vec(&json!({"event": {"start": {"pid": MOCK_PID}}}))
            .expect("start event JSON");
        let _ = tx.unbounded_send(Ok(connect_envelope(&start)));
        drop(tx);

        (
            [(axum::http::header::CONTENT_TYPE, CONNECT_JSON)],
            Body::from_stream(rx),
        )
    }

    async fn envd_send_input_handler(
        AxumState(mock): AxumState<MockEnvd>,
        Json(body): Json<Value>,
    ) -> Json<Value> {
        if mock
            .hang_send_input
            .load(std::sync::atomic::Ordering::Relaxed)
        {
            // A hung envd never answers; the client's ENVD_CALL_TIMEOUT is
            // what unblocks the session.
            std::future::pending::<()>().await;
        }
        mock.spy.lock().await.send_input.push(body.clone());
        // Echo the input back as PTY output, like a real shell would.
        if let Some(pty) = body.get("input").and_then(|i| i.get("pty")).cloned() {
            mock.push_frame(json!({"event": {"data": {"pty": pty}}}))
                .await;
        }
        Json(json!({}))
    }

    async fn envd_update_handler(
        AxumState(mock): AxumState<MockEnvd>,
        Json(body): Json<Value>,
    ) -> Json<Value> {
        mock.spy.lock().await.update.push(body);
        Json(json!({}))
    }

    async fn envd_send_signal_handler(
        AxumState(mock): AxumState<MockEnvd>,
        Json(body): Json<Value>,
    ) -> Json<Value> {
        mock.spy.lock().await.send_signal.push(body);
        // Killing the shell ends the process and then the stream.
        mock.push_frame(json!({"event": {"end": {"exitCode": 137}}}))
            .await;
        let _ = mock.stream_tx.lock().await.take();
        Json(json!({}))
    }

    async fn spawn_mock_envd() -> (String, MockEnvd) {
        spawn_mock_envd_with(MockEnvd::default()).await
    }

    async fn spawn_mock_envd_with(mock: MockEnvd) -> (String, MockEnvd) {
        let app = Router::new()
            .route("/process.Process/Start", post(envd_start_handler))
            .route("/process.Process/Connect", post(envd_connect_handler))
            .route("/process.Process/SendInput", post(envd_send_input_handler))
            .route("/process.Process/Update", post(envd_update_handler))
            .route(
                "/process.Process/SendSignal",
                post(envd_send_signal_handler),
            )
            .with_state(mock.clone());
        (spawn_server(app).await, mock)
    }

    // ── Mock CubeMaster ───────────────────────────────────────────────────

    async fn spawn_mock_master(status: i32) -> String {
        let handler = move || async move {
            Json(json!({
                "requestID": "req-info",
                "ret": { "ret_code": 0, "ret_msg": "ok" },
                "data": [{
                    "sandbox_id": SANDBOX_ID,
                    "status": status,
                    "host_id": "",
                    "containers": [],
                }],
            }))
        };
        spawn_server(Router::new().route("/cube/sandbox/info", get(handler))).await
    }

    async fn spawn_mock_master_not_found() -> String {
        async fn handler() -> Json<Value> {
            Json(json!({
                "requestID": "req-info",
                "ret": { "ret_code": RET_CODE_NOT_FOUND, "ret_msg": "no such sandbox" },
                "data": [],
            }))
        }
        spawn_server(Router::new().route("/cube/sandbox/info", get(handler))).await
    }

    // ── Mock auth callback ────────────────────────────────────────────────

    type CapturedHeaders = Arc<Mutex<Vec<(String, String)>>>;

    async fn spawn_auth_callback(status: StatusCode) -> (String, CapturedHeaders) {
        spawn_auth_callback_with_headers(status, &[]).await
    }

    /// Mock auth callback that also sets the given response headers (e.g.
    /// `X-Auth-User`) on its response.
    async fn spawn_auth_callback_with_headers(
        status: StatusCode,
        response_headers: &'static [(&'static str, &'static str)],
    ) -> (String, CapturedHeaders) {
        let captured: CapturedHeaders = Arc::new(Mutex::new(Vec::new()));
        let captured_clone = captured.clone();
        let handler = move |req: axum::http::Request<Body>| {
            let captured = captured_clone.clone();
            async move {
                let mut guard = captured.lock().await;
                for (k, v) in req.headers() {
                    guard.push((k.to_string(), v.to_str().unwrap_or("").to_string()));
                }
                let mut builder = axum::http::Response::builder().status(status);
                for (k, v) in response_headers {
                    builder = builder.header(*k, *v);
                }
                builder.body(Body::empty()).expect("callback response")
            }
        };
        let url = spawn_server(Router::new().route("/auth", post(handler))).await;
        (format!("{}/auth", url), captured)
    }

    /// An auth callback that never responds, simulating a hung auth service.
    async fn spawn_hanging_auth_callback() -> String {
        let handler = || async {
            std::future::pending::<()>().await;
            #[allow(unreachable_code)]
            axum::http::Response::new(Body::empty())
        };
        let url = spawn_server(Router::new().route("/auth", post(handler))).await;
        format!("{}/auth", url)
    }

    // ── Shared helpers ────────────────────────────────────────────────────

    async fn spawn_server(app: Router) -> String {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("listener should bind");
        let addr = listener.local_addr().expect("listener addr");
        tokio::spawn(async move {
            axum::serve(listener, app).await.expect("server should run");
        });
        format!("http://{}", addr)
    }

    fn test_config(proxy_url: &str, master_url: &str) -> ServerConfig {
        ServerConfig {
            cubemaster_url: master_url.to_string(),
            sandbox_proxy_url: proxy_url.to_string(),
            // Most tests predate the secure defaults: keep the legacy
            // behaviors (open-mode terminal allowed, ?token= accepted) so
            // they exercise the same paths as before. Tests for the secure
            // defaults construct their own config explicitly.
            terminal_allow_unauthenticated: true,
            terminal_token_query_param: true,
            ..Default::default()
        }
    }

    async fn spawn_app(config: ServerConfig) -> String {
        let state = AppState::new(config, arc(NoopLogger)).await;
        spawn_server(build_router(state)).await
    }

    fn ws_url(app: &str, query: &str) -> String {
        format!(
            "{}/cubeapi/v1/sandboxes/{}/terminal/ws{}",
            app.replacen("http", "ws", 1),
            SANDBOX_ID,
            query
        )
    }

    async fn recv_json<S, E>(ws: &mut S) -> Value
    where
        S: futures::Stream<Item = Result<tungstenite::Message, E>> + Unpin,
        E: std::fmt::Debug,
    {
        let msg = tokio::time::timeout(Duration::from_secs(10), ws.next())
            .await
            .expect("timed out waiting for ws message")
            .expect("ws stream ended unexpectedly")
            .expect("ws read error");
        match msg {
            tungstenite::Message::Text(text) => serde_json::from_str(&text).expect("message JSON"),
            other => panic!("expected text message, got {:?}", other),
        }
    }

    async fn wait_for_recorded<F>(fetch: F) -> Value
    where
        F: Fn() -> futures::future::BoxFuture<'static, Option<Value>>,
    {
        for _ in 0..100 {
            if let Some(v) = fetch().await {
                return v;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        panic!("expected envd call was not recorded");
    }

    fn first_of(
        spy: &Arc<Mutex<EnvdSpy>>,
        pick: fn(&EnvdSpy) -> Option<Value>,
    ) -> impl Fn() -> futures::future::BoxFuture<'static, Option<Value>> {
        let spy = spy.clone();
        move || {
            let spy = spy.clone();
            Box::pin(async move {
                let guard = spy.lock().await;
                pick(&guard)
            })
        }
    }

    async fn wait_for_connect_with_tag(spy: &Arc<Mutex<EnvdSpy>>, tag: &str) -> Value {
        let tag = tag.to_string();
        wait_for_recorded(move || {
            let spy = spy.clone();
            let tag = tag.clone();
            Box::pin(async move {
                spy.lock()
                    .await
                    .connect
                    .iter()
                    .find(|payload| payload["process"]["tag"] == tag)
                    .cloned()
            })
        })
        .await
    }

    type ConnectResult = Result<
        (
            tokio_tungstenite::WebSocketStream<
                tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>,
            >,
            axum::http::Response<Option<Vec<u8>>>,
        ),
        tungstenite::Error,
    >;

    fn expect_upgrade_error(result: ConnectResult, status: StatusCode) {
        match result {
            Err(tungstenite::Error::Http(resp)) => assert_eq!(resp.status(), status),
            other => panic!(
                "expected HTTP {} before upgrade, got {:?}",
                status,
                other.is_ok()
            ),
        }
    }

    /// Raw WebSocket handshake carrying the given `Sec-WebSocket-Protocol`
    /// header value. Returns the response head and the still-open TCP stream
    /// so callers can assert on the status line before (maybe) continuing as
    /// a WebSocket.
    async fn raw_handshake(
        app: &str,
        query: &str,
        protocols: &str,
    ) -> (String, tokio::net::TcpStream) {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};

        let url = ws_url(app, query);
        let without_scheme = url.strip_prefix("ws://").expect("ws url");
        let (authority, path) = without_scheme
            .split_once('/')
            .map(|(a, p)| (a.to_string(), format!("/{}", p)))
            .expect("ws path");

        let mut stream = tokio::net::TcpStream::connect(&authority)
            .await
            .expect("tcp connect");
        let request = format!(
            "GET {path} HTTP/1.1\r\n\
             Host: {authority}\r\n\
             Connection: Upgrade\r\n\
             Upgrade: websocket\r\n\
             Sec-WebSocket-Version: 13\r\n\
             Sec-WebSocket-Key: {key}\r\n\
             Sec-WebSocket-Protocol: {protocols}\r\n\
             \r\n",
            key = BASE64.encode(b"0123456789abcdef"),
        );
        stream
            .write_all(request.as_bytes())
            .await
            .expect("write handshake");

        // Byte-by-byte so no WebSocket frame bytes past the header
        // terminator are swallowed.
        let mut buf = Vec::new();
        let mut byte = [0u8; 1];
        while !buf.ends_with(b"\r\n\r\n") {
            let n = stream.read(&mut byte).await.expect("read handshake");
            assert_eq!(n, 1, "handshake response truncated");
            buf.push(byte[0]);
        }
        (String::from_utf8_lossy(&buf).to_string(), stream)
    }

    /// Manual WebSocket handshake carrying `Sec-WebSocket-Protocol` headers.
    /// The browser offers both the base `cube-terminal` protocol and the
    /// token-bearing `cube-terminal.<token>` entry; the server must select
    /// exactly the base protocol (Chrome aborts the handshake when offered
    /// subprotocols go unanswered) and must never echo the token one.
    async fn connect_with_subprotocol(
        app: &str,
        token_subprotocol: &str,
    ) -> tokio_tungstenite::WebSocketStream<tokio::net::TcpStream> {
        let (head, stream) = raw_handshake(
            app,
            "",
            &format!("{}, {}", TERMINAL_SUBPROTOCOL, token_subprotocol),
        )
        .await;
        assert!(
            head.starts_with("HTTP/1.1 101"),
            "expected 101 Switching Protocols, got: {}",
            head
        );
        let selected = head
            .lines()
            .find(|l| l.to_ascii_lowercase().starts_with("sec-websocket-protocol"))
            .map(|l| {
                l.split_once(':')
                    .expect("header colon")
                    .1
                    .trim()
                    .to_string()
            });
        assert_eq!(
            selected.as_deref(),
            Some("cube-terminal"),
            "server must select exactly the base subprotocol: {}",
            head
        );

        tokio_tungstenite::WebSocketStream::from_raw_socket(
            stream,
            tungstenite::protocol::Role::Client,
            None,
        )
        .await
    }

    // ── Tests ─────────────────────────────────────────────────────────────

    #[tokio::test(flavor = "multi_thread")]
    async fn happy_path_bridges_ws_and_envd_pty() {
        let (proxy_url, mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let app = spawn_app(test_config(&proxy_url, &master_url)).await;

        let (mut ws, _resp) = connect_async(ws_url(&app, "?cols=100&rows=30"))
            .await
            .expect("websocket upgrade should succeed");

        let ready = recv_json(&mut ws).await;
        assert_eq!(ready, json!({"type": "ready", "pid": MOCK_PID}));

        // The Start request went out with the requested PTY size.
        let start = mock
            .spy
            .lock()
            .await
            .start_payload
            .clone()
            .expect("start payload recorded");
        assert_eq!(start["pty"]["size"], json!({"rows": 30, "cols": 100}));
        assert_eq!(start["process"]["cmd"], json!("/bin/sh"));
        assert_eq!(
            start["process"]["args"],
            json!(["-c", "exec /bin/bash -il 2>/dev/null || exec /bin/sh -i"])
        );
        // The Start request carries a unique tag for orphan recovery.
        assert!(
            start["tag"]
                .as_str()
                .expect("start payload carries a tag")
                .starts_with("cubeapi-terminal-"),
            "unexpected start tag: {}",
            start["tag"]
        );

        // Client input is forwarded to envd SendInput, and envd PTY data
        // frames come back as output messages.
        let input_b64 = BASE64.encode("echo hi\n");
        ws.send(tungstenite::Message::Text(
            json!({"type": "input", "data": input_b64}).to_string(),
        ))
        .await
        .expect("send input");

        let output = recv_json(&mut ws).await;
        assert_eq!(output, json!({"type": "output", "data": input_b64}));

        let send_input =
            wait_for_recorded(first_of(&mock.spy, |s| s.send_input.first().cloned())).await;
        assert_eq!(
            send_input,
            json!({"process": {"pid": MOCK_PID}, "input": {"pty": input_b64}})
        );

        // Resize is forwarded to envd Update.
        ws.send(tungstenite::Message::Text(
            json!({"type": "resize", "cols": 120, "rows": 40}).to_string(),
        ))
        .await
        .expect("send resize");

        let update = wait_for_recorded(first_of(&mock.spy, |s| s.update.first().cloned())).await;
        assert_eq!(
            update,
            json!({"process": {"pid": MOCK_PID}, "pty": {"size": {"rows": 40, "cols": 120}}})
        );

        // An envd end event becomes an exit message, and teardown kills the
        // shell via SendSignal SIGKILL.
        mock.push_frame(json!({"event": {"end": {"exitCode": 0}}}))
            .await;
        let exit = recv_json(&mut ws).await;
        assert_eq!(exit, json!({"type": "exit", "code": 0}));

        let signal =
            wait_for_recorded(first_of(&mock.spy, |s| s.send_signal.first().cloned())).await;
        assert_eq!(
            signal,
            json!({"process": {"pid": MOCK_PID}, "signal": "SIGNAL_SIGKILL"})
        );
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn client_disconnect_kills_the_shell() {
        let (proxy_url, mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let app = spawn_app(test_config(&proxy_url, &master_url)).await;

        let (mut ws, _resp) = connect_async(ws_url(&app, ""))
            .await
            .expect("websocket upgrade should succeed");
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));

        ws.close(None).await.expect("close websocket");
        let signal =
            wait_for_recorded(first_of(&mock.spy, |s| s.send_signal.first().cloned())).await;
        assert_eq!(signal["signal"], json!("SIGNAL_SIGKILL"));
        assert_eq!(signal["process"]["pid"], json!(MOCK_PID));
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn idle_timeout_terminates_session() {
        let (proxy_url, mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let mut config = test_config(&proxy_url, &master_url);
        config.terminal_idle_timeout_secs = 1;
        let app = spawn_app(config).await;

        let (mut ws, _resp) = connect_async(ws_url(&app, ""))
            .await
            .expect("websocket upgrade should succeed");
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));

        // No client traffic → the server must close the session and reap the
        // shell well before the (24 h) envd stream deadline.
        let error = recv_json(&mut ws).await;
        assert_eq!(error["type"], json!("error"));
        assert!(
            error["message"]
                .as_str()
                .expect("error message")
                .contains("inactivity"),
            "unexpected idle error message: {}",
            error["message"]
        );

        let signal =
            wait_for_recorded(first_of(&mock.spy, |s| s.send_signal.first().cloned())).await;
        assert_eq!(signal["signal"], json!("SIGNAL_SIGKILL"));
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn output_activity_extends_idle_timeout() {
        let (proxy_url, mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let mut config = test_config(&proxy_url, &master_url);
        config.terminal_idle_timeout_secs = 1;
        let app = spawn_app(config).await;

        let (mut ws, _resp) = connect_async(ws_url(&app, ""))
            .await
            .expect("websocket upgrade should succeed");
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));

        // Shell output every 300 ms keeps the session alive well past the
        // 1 s idle timeout: five output frames arrive over ~1.5 s without
        // any client input.
        for _ in 0..5 {
            mock.push_frame(json!({"event": {"data": {"pty": BASE64.encode("tick")}}}))
                .await;
            let output = recv_json(&mut ws).await;
            assert_eq!(output["type"], json!("output"));
            tokio::time::sleep(Duration::from_millis(300)).await;
        }

        // Once the output stops and the client stays silent, the idle
        // timeout fires and the shell is reaped.
        let error = recv_json(&mut ws).await;
        assert_eq!(error["type"], json!("error"));
        assert!(
            error["message"]
                .as_str()
                .expect("error message")
                .contains("inactivity"),
            "unexpected idle error message: {}",
            error["message"]
        );
        let signal =
            wait_for_recorded(first_of(&mock.spy, |s| s.send_signal.first().cloned())).await;
        assert_eq!(signal["signal"], json!("SIGNAL_SIGKILL"));
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn token_subprotocol_without_base_is_rejected() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let app = spawn_app(test_config(&proxy_url, &master_url)).await;

        // Only the token-bearing subprotocol, no base `cube-terminal`: no
        // valid selection exists (the token entry is never echoed), so the
        // request is rejected with 400 before the upgrade.
        let (head, _stream) = raw_handshake(&app, "", "cube-terminal.some-token").await;
        assert!(
            head.starts_with("HTTP/1.1 400"),
            "expected 400 Bad Request, got: {}",
            head
        );
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn rejects_when_callback_auth_fails_or_token_missing() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let (callback_url, _captured) = spawn_auth_callback(StatusCode::FORBIDDEN).await;
        let mut config = test_config(&proxy_url, &master_url);
        config.auth_callback_url = Some(callback_url);
        let app = spawn_app(config).await;

        // Missing token → 401 before upgrade.
        expect_upgrade_error(
            connect_async(ws_url(&app, "")).await,
            StatusCode::UNAUTHORIZED,
        );
        // Token present but callback denies → 401 before upgrade.
        expect_upgrade_error(
            connect_async(ws_url(&app, "?token=bad-token")).await,
            StatusCode::UNAUTHORIZED,
        );
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn callback_auth_forwards_path_method_and_bearer_token() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let (callback_url, captured) = spawn_auth_callback(StatusCode::OK).await;
        let mut config = test_config(&proxy_url, &master_url);
        config.auth_callback_url = Some(callback_url);
        let app = spawn_app(config).await;

        let (mut ws, _resp) = connect_async(ws_url(&app, "?token=good-token"))
            .await
            .expect("websocket upgrade should succeed");
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));

        let guard = captured.lock().await;
        let header = |name: &str| {
            guard
                .iter()
                .find(|(k, _)| k == name)
                .map(|(_, v)| v.clone())
        };
        assert_eq!(
            header("x-request-path").as_deref(),
            Some(format!("/cubeapi/v1/sandboxes/{}/terminal/ws", SANDBOX_ID).as_str())
        );
        assert_eq!(header("x-request-method").as_deref(), Some("GET"));
        assert_eq!(
            header("authorization").as_deref(),
            Some("Bearer good-token")
        );
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn rejects_sandbox_that_is_not_running() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_PAUSED).await;
        let app = spawn_app(test_config(&proxy_url, &master_url)).await;

        expect_upgrade_error(connect_async(ws_url(&app, "")).await, StatusCode::CONFLICT);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn rejects_unknown_sandbox() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master_not_found().await;
        let app = spawn_app(test_config(&proxy_url, &master_url)).await;

        expect_upgrade_error(connect_async(ws_url(&app, "")).await, StatusCode::NOT_FOUND);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn subprotocol_token_authenticates_without_query_param() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let (callback_url, captured) = spawn_auth_callback(StatusCode::OK).await;
        let mut config = test_config(&proxy_url, &master_url);
        config.auth_callback_url = Some(callback_url);
        let app = spawn_app(config).await;

        // Browser-style handshake: the token rides Sec-WebSocket-Protocol as
        // `cube-terminal.<token>` and there is no query param at all. The
        // helper also asserts the server does not echo a subprotocol.
        let mut ws = connect_with_subprotocol(&app, "cube-terminal.good-token").await;

        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));

        // The callback received the subprotocol token as Bearer.
        let guard = captured.lock().await;
        let authz = guard
            .iter()
            .find(|(k, _)| k == "authorization")
            .map(|(_, v)| v.clone());
        assert_eq!(authz.as_deref(), Some("Bearer good-token"));
    }

    #[test]
    fn origin_host_matching_rules() {
        use axum::http::HeaderValue;

        let check = |origin: Option<&str>, host: &str| {
            let mut headers = HeaderMap::new();
            headers.insert("host", HeaderValue::from_str(host).unwrap());
            if let Some(o) = origin {
                headers.insert("origin", HeaderValue::from_str(o).unwrap());
            }
            origin_matches_host(&headers)
        };

        // Exact match, case-insensitive host.
        assert!(check(Some("http://example.com:8443"), "example.com:8443"));
        assert!(check(Some("https://EXAMPLE.com"), "example.com"));
        // Port-less Origin vs explicit Host port: the Host port must equal
        // the Origin scheme's default — `example.com:3000` is NOT matched
        // by a port-less http Origin (tightened rule), while :80/:443 are.
        assert!(!check(Some("http://example.com"), "example.com:3000"));
        assert!(check(Some("http://example.com"), "example.com:80"));
        assert!(check(Some("https://example.com"), "example.com:443"));
        assert!(!check(Some("https://example.com"), "example.com:80"));
        // An Origin scheme with no default port never matches a ported Host.
        assert!(!check(Some("ftp://example.com"), "example.com:21"));
        // An explicit port equal to the scheme default is the same as
        // port-less, even against a port-less Host.
        assert!(check(Some("http://example.com:80"), "example.com"));
        assert!(check(Some("https://example.com:443"), "example.com"));
        // An explicit non-default Origin port vs a port-less Host must NOT
        // match: a proxy that strips the port (`proxy_set_header Host
        // $host`) must not widen the check to other same-host services.
        // Such proxies must forward the full authority (`Host $http_host`).
        assert!(!check(Some("http://example.com:12088"), "example.com"));
        assert!(!check(Some("https://example.com:80"), "example.com"));
        // Both ported: ports must agree.
        assert!(!check(Some("http://example.com:12088"), "example.com:3000"));
        assert!(check(Some("http://example.com:12088"), "example.com:12088"));
        // Different hostnames never match, even port-less.
        assert!(!check(Some("http://evil.com"), "example.com"));
        assert!(!check(Some("http://example.com.evil.com"), "example.com"));
        // No Origin header: not a browser, skip the check.
        assert!(origin_allowed(&HeaderMap::new(), &[]));
        // Malformed Origin: reject.
        assert!(!check(Some("not-a-url"), "example.com"));
        // Bracketed IPv6 without a port is not misparsed as host+port.
        assert!(check(Some("http://[::1]"), "[::1]"));
    }

    #[test]
    fn origin_allowed_origins_whitelist() {
        use axum::http::HeaderValue;

        let whitelist = vec![
            "https://cube.example.com".to_string(),
            "https://admin.example.com:8443".to_string(),
        ];
        let check = |origin: Option<&str>, host: &str| {
            let mut headers = HeaderMap::new();
            headers.insert("host", HeaderValue::from_str(host).unwrap());
            if let Some(o) = origin {
                headers.insert("origin", HeaderValue::from_str(o).unwrap());
            }
            origin_allowed(&headers, &whitelist)
        };

        // Exact whitelist entries match, regardless of the request Host.
        assert!(check(Some("https://cube.example.com"), "10.0.0.1:3000"));
        assert!(check(
            Some("https://admin.example.com:8443"),
            "10.0.0.1:3000"
        ));
        // Comparison normalizes case (scheme/host) and surrounding space.
        assert!(check(Some("HTTPS://CUBE.example.COM"), "10.0.0.1:3000"));
        assert!(check(Some(" https://cube.example.com "), "10.0.0.1:3000"));
        // Same host, wrong scheme or port: no Host-match fallback.
        assert!(!check(Some("http://cube.example.com"), "cube.example.com"));
        assert!(!check(
            Some("https://cube.example.com:443"),
            "10.0.0.1:3000"
        ));
        assert!(!check(Some("https://admin.example.com"), "10.0.0.1:3000"));
        // A Host-matching Origin that is not whitelisted is rejected.
        assert!(!check(Some("http://10.0.0.1:3000"), "10.0.0.1:3000"));
        // Malformed Origin: reject; no Origin: unaffected.
        assert!(!check(Some("not-a-url"), "10.0.0.1:3000"));
        assert!(check(None, "10.0.0.1:3000"));
    }

    // ── Multi-container envd port resolution ──────────────────────────────

    fn container(name: &str, id: &str, kind: &str, envd_port: Option<u16>) -> SandboxContainer {
        SandboxContainer {
            name: name.to_string(),
            container_id: id.to_string(),
            kind: kind.to_string(),
            envd_port,
        }
    }

    fn multi_containers() -> Vec<SandboxContainer> {
        vec![
            container("sandbox", SANDBOX_ID, "sandbox", Some(ENVD_PORT)),
            container("sidecar", "cid-sidecar", "container", Some(ENVD_PORT + 1)),
        ]
    }

    #[test]
    fn resolve_envd_port_defaults_to_primary_container() {
        let containers = multi_containers();
        assert_eq!(
            resolve_envd_port(&containers, SANDBOX_ID, None).expect("port"),
            ENVD_PORT
        );
    }

    #[test]
    fn resolve_envd_port_falls_back_without_label_or_containers() {
        // Pre-feature sandbox: primary container carries no envd_port.
        let containers = vec![container("sandbox", SANDBOX_ID, "sandbox", None)];
        assert_eq!(
            resolve_envd_port(&containers, SANDBOX_ID, None).expect("port"),
            ENVD_PORT
        );
        // envd_port = 0 is normalized to "no endpoint" at deserialization;
        // a literal 0 must behave the same as None.
        let containers = vec![container("sandbox", SANDBOX_ID, "sandbox", Some(0))];
        assert_eq!(
            resolve_envd_port(&containers, SANDBOX_ID, None).expect("port"),
            ENVD_PORT
        );
        // CubeMaster reporting no containers at all keeps legacy behaviour.
        assert_eq!(
            resolve_envd_port(&[], SANDBOX_ID, None).expect("port"),
            ENVD_PORT
        );
    }

    #[test]
    fn resolve_envd_port_selects_by_id_and_name() {
        let containers = multi_containers();
        assert_eq!(
            resolve_envd_port(&containers, SANDBOX_ID, Some("cid-sidecar")).expect("port"),
            ENVD_PORT + 1
        );
        assert_eq!(
            resolve_envd_port(&containers, SANDBOX_ID, Some("sidecar")).expect("port"),
            ENVD_PORT + 1
        );
    }

    #[test]
    fn resolve_envd_port_unknown_container_is_not_found() {
        let containers = multi_containers();
        let err = resolve_envd_port(&containers, SANDBOX_ID, Some("nope"))
            .expect_err("unknown container must 404");
        assert!(matches!(err, AppError::NotFound(_)));
    }

    #[test]
    fn resolve_envd_port_sidecar_without_endpoint_is_conflict() {
        // Pre-feature sandbox: the sidecar exists but has no terminal
        // endpoint; selecting it explicitly is a 409, and a literal 0 port
        // is treated the same as a missing one.
        for envd_port in [None, Some(0)] {
            let containers = vec![
                container("sandbox", SANDBOX_ID, "sandbox", Some(ENVD_PORT)),
                container("sidecar", "cid-sidecar", "container", envd_port),
            ];
            let err = resolve_envd_port(&containers, SANDBOX_ID, Some("cid-sidecar"))
                .expect_err("sidecar without endpoint must 409");
            assert!(matches!(err, AppError::Conflict(_)));
        }
    }

    // ── Operator identity extraction (audit attribution) ─────────────────

    /// Build an AppState pointing at the given auth callback (the
    /// proxy/master URLs are irrelevant to `authenticate`).
    async fn auth_state(callback_url: &str) -> AppState {
        let mut config = test_config("http://127.0.0.1:1", "http://127.0.0.1:1");
        config.auth_callback_url = Some(callback_url.to_string());
        AppState::new(config, arc(NoopLogger)).await
    }

    /// CubeOps-style JWT assembled by hand: three base64url segments with a
    /// dummy signature — the identity extractor never verifies signatures.
    fn unsigned_jwt(payload: Value) -> String {
        format!(
            "{}.{}.signature",
            URL_SAFE_NO_PAD.encode(br#"{"alg":"HS256","typ":"JWT"}"#),
            URL_SAFE_NO_PAD.encode(payload.to_string().as_bytes())
        )
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn authenticate_uses_callback_x_auth_user_header() {
        let (url, _) =
            spawn_auth_callback_with_headers(StatusCode::OK, &[("x-auth-user", "bob")]).await;
        let state = auth_state(&url).await;
        let credential = TerminalCredential::Bearer("plain-token".to_string());
        let identity = authenticate(&state, Some(&credential), "/x")
            .await
            .expect("callback 200 authorizes");
        // The callback-named identity is authoritative…
        assert_eq!(identity.user.as_deref(), Some("bob"));
        // …and suppresses the unverified-claim fallback entirely.
        assert_eq!(identity.claimed_user, None);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn authenticate_falls_back_to_jwt_username_claim() {
        let (url, _) = spawn_auth_callback(StatusCode::OK).await;
        let state = auth_state(&url).await;
        let token = unsigned_jwt(json!({"username": "alice", "sub": "alice", "exp": 1}));
        let credential = TerminalCredential::Bearer(token);
        let identity = authenticate(&state, Some(&credential), "/x")
            .await
            .expect("callback 200 authorizes");
        // Unverified claims land in claimed_user, never in user.
        assert_eq!(identity.user, None);
        assert_eq!(identity.claimed_user.as_deref(), Some("alice"));
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn authenticate_falls_back_to_jwt_sub_claim() {
        let (url, _) = spawn_auth_callback(StatusCode::OK).await;
        let state = auth_state(&url).await;
        let token = unsigned_jwt(json!({"sub": "carol", "exp": 1}));
        let credential = TerminalCredential::Bearer(token);
        let identity = authenticate(&state, Some(&credential), "/x")
            .await
            .expect("callback 200 authorizes");
        assert_eq!(identity.user, None);
        assert_eq!(identity.claimed_user.as_deref(), Some("carol"));
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn authenticate_without_identity_source_still_allows() {
        let (url, _) = spawn_auth_callback(StatusCode::OK).await;
        let state = auth_state(&url).await;

        // Not a JWT at all.
        let credential = TerminalCredential::Bearer("not-a-jwt".to_string());
        let identity = authenticate(&state, Some(&credential), "/x")
            .await
            .expect("identity failure must not reject the request");
        assert_eq!(identity.user, None);
        assert_eq!(identity.claimed_user, None);

        // JWT-shaped, but the payload segment is not decodable base64url.
        let credential = TerminalCredential::Bearer("aaa.!!!.bbb".to_string());
        let identity = authenticate(&state, Some(&credential), "/x")
            .await
            .expect("identity failure must not reject the request");
        assert_eq!(identity.user, None);
        assert_eq!(identity.claimed_user, None);

        // API-key credentials carry no identity either.
        let credential = TerminalCredential::ApiKey("some-key".to_string());
        let identity = authenticate(&state, Some(&credential), "/x")
            .await
            .expect("identity failure must not reject the request");
        assert_eq!(identity.user, None);
        assert_eq!(identity.claimed_user, None);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn authenticate_prefers_callback_header_over_jwt_claims() {
        let (url, _) =
            spawn_auth_callback_with_headers(StatusCode::OK, &[("x-auth-user", "bob")]).await;
        let state = auth_state(&url).await;
        let token = unsigned_jwt(json!({"username": "alice"}));
        let credential = TerminalCredential::Bearer(token);
        let identity = authenticate(&state, Some(&credential), "/x")
            .await
            .expect("callback 200 authorizes");
        assert_eq!(identity.user.as_deref(), Some("bob"));
        assert_eq!(identity.claimed_user, None);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn origin_mismatch_is_forbidden_before_upgrade() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let app = spawn_app(test_config(&proxy_url, &master_url)).await;

        // The handshake Host is 127.0.0.1:<port>; a foreign Origin → 403.
        let request =
            tungstenite::ClientRequestBuilder::new(ws_url(&app, "").parse().expect("uri"))
                .with_header("Origin", "http://evil.example.com");
        expect_upgrade_error(connect_async(request).await, StatusCode::FORBIDDEN);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn matching_origin_upgrades() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let app = spawn_app(test_config(&proxy_url, &master_url)).await;

        // Exact authority match (scheme + host + port).
        let request =
            tungstenite::ClientRequestBuilder::new(ws_url(&app, "").parse().expect("uri"))
                .with_header("Origin", app.clone());
        let (mut ws, _resp) = connect_async(request)
            .await
            .expect("same-origin upgrade should succeed");
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));
        ws.close(None).await.expect("close websocket");

        // A port-less Origin against a non-default Host port no longer
        // matches (tightened port rule): http's effective port 80 != the
        // ephemeral listener port, so the handshake is rejected with 403.
        let request =
            tungstenite::ClientRequestBuilder::new(ws_url(&app, "").parse().expect("uri"))
                .with_header("Origin", "http://127.0.0.1");
        expect_upgrade_error(connect_async(request).await, StatusCode::FORBIDDEN);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn per_sandbox_session_cap_returns_429() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let mut config = test_config(&proxy_url, &master_url);
        config.terminal_max_sessions_per_sandbox = 1;
        let app = spawn_app(config).await;

        let (mut ws, _resp) = connect_async(ws_url(&app, ""))
            .await
            .expect("first session should upgrade");
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));

        // A second concurrent session on the same sandbox exceeds the cap.
        expect_upgrade_error(
            connect_async(ws_url(&app, "")).await,
            StatusCode::TOO_MANY_REQUESTS,
        );

        // Closing the first session releases the slot. Teardown is async,
        // so poll until the tracker frees it.
        ws.close(None).await.expect("close websocket");
        let mut upgraded = None;
        for _ in 0..100 {
            match connect_async(ws_url(&app, "")).await {
                Ok((ws, _resp)) => {
                    upgraded = Some(ws);
                    break;
                }
                Err(_) => tokio::time::sleep(Duration::from_millis(20)).await,
            }
        }
        let mut ws = upgraded.expect("slot should be released after close");
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn oversized_client_message_terminates_session() {
        let (proxy_url, mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let app = spawn_app(test_config(&proxy_url, &master_url)).await;

        let (mut ws, _resp) = connect_async(ws_url(&app, ""))
            .await
            .expect("websocket upgrade should succeed");
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));

        // Well over the 64 KiB server-side message cap.
        let big = json!({"type": "input", "data": "x".repeat(128 * 1024)}).to_string();
        ws.send(tungstenite::Message::Text(big))
            .await
            .expect("send oversized message");

        // The server answers with an error frame and ends the session.
        let error = recv_json(&mut ws).await;
        assert_eq!(error["type"], json!("error"));

        // The oversized payload must never reach envd.
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert!(mock.spy.lock().await.send_input.is_empty());
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn hung_envd_call_reports_an_error_and_reaps_the_session() {
        let hanging = MockEnvd::default();
        hanging
            .hang_send_input
            .store(true, std::sync::atomic::Ordering::Relaxed);
        let (proxy_url, mock) = spawn_mock_envd_with(hanging).await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let app = spawn_app(test_config(&proxy_url, &master_url)).await;

        let (mut ws, _resp) = connect_async(ws_url(&app, ""))
            .await
            .expect("websocket upgrade should succeed");
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));

        // This input hangs inside envd forever. The envd call deadline must
        // bound the stall and tell the client that its terminal is gone.
        let input_b64 = BASE64.encode("echo hi\n");
        ws.send(tungstenite::Message::Text(
            json!({"type": "input", "data": input_b64}).to_string(),
        ))
        .await
        .expect("send input");

        let msg = tokio::time::timeout(ENVD_CALL_TIMEOUT + Duration::from_secs(15), ws.next())
            .await
            .expect("client must be notified after the envd call deadline")
            .expect("ws stream ended unexpectedly")
            .expect("ws read error");
        let tungstenite::Message::Text(text) = msg else {
            panic!("expected text message, got {:?}", msg);
        };
        let error: Value = serde_json::from_str(&text).expect("message JSON");
        assert_eq!(error["type"], json!("error"));

        // The error path still reaps the shell afterwards.
        let signal =
            wait_for_recorded(first_of(&mock.spy, |s| s.send_signal.first().cloned())).await;
        assert_eq!(signal["signal"], json!("SIGNAL_SIGKILL"));
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn handshake_is_rate_limited_when_auth_is_configured() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let mut config = test_config(&proxy_url, &master_url);
        config.cube_api_key = Some("secret-key".to_string());
        // One request per second per key: the first handshake drains the
        // bucket, the second must be rejected with 429 before reaching the
        // handler — the terminal route carries the same rate limit as the
        // other sandbox routes.
        config.rate_limit_per_sec = 1;
        let app = spawn_app(config).await;

        expect_upgrade_error(
            connect_async(ws_url(&app, "?token=wrong-key")).await,
            StatusCode::UNAUTHORIZED,
        );
        expect_upgrade_error(
            connect_async(ws_url(&app, "?token=wrong-key")).await,
            StatusCode::TOO_MANY_REQUESTS,
        );
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn callback_auth_accepts_x_api_key_header() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let (callback_url, captured) = spawn_auth_callback(StatusCode::OK).await;
        let mut config = test_config(&proxy_url, &master_url);
        config.auth_callback_url = Some(callback_url);
        let app = spawn_app(config).await;

        // Non-browser client style: an X-API-Key header and no token
        // anywhere else.
        let request =
            tungstenite::ClientRequestBuilder::new(ws_url(&app, "").parse().expect("uri"))
                .with_header("X-API-Key", "good-key");
        let (mut ws, _resp) = connect_async(request)
            .await
            .expect("x-api-key handshake should succeed");
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));

        // The callback received the credential on X-API-Key (not Bearer),
        // exactly like unified_auth forwards it for the HTTP routes.
        let guard = captured.lock().await;
        let header = |name: &str| {
            guard
                .iter()
                .find(|(k, _)| k == name)
                .map(|(_, v)| v.clone())
        };
        assert_eq!(header("x-api-key").as_deref(), Some("good-key"));
        assert_eq!(header("authorization"), None);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn bearer_token_takes_priority_over_x_api_key() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let (callback_url, captured) = spawn_auth_callback(StatusCode::OK).await;
        let mut config = test_config(&proxy_url, &master_url);
        config.auth_callback_url = Some(callback_url);
        let app = spawn_app(config).await;

        // Both credential transports present: Bearer wins, mirroring
        // unified_auth's extraction order.
        let request = tungstenite::ClientRequestBuilder::new(
            ws_url(&app, "?token=good-token").parse().expect("uri"),
        )
        .with_header("X-API-Key", "other-key");
        let (mut ws, _resp) = connect_async(request)
            .await
            .expect("handshake should succeed");
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));

        let guard = captured.lock().await;
        let header = |name: &str| {
            guard
                .iter()
                .find(|(k, _)| k == name)
                .map(|(_, v)| v.clone())
        };
        assert_eq!(
            header("authorization").as_deref(),
            Some("Bearer good-token")
        );
        assert_eq!(header("x-api-key"), None);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn simple_key_auth_accepts_x_api_key_header() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let mut config = test_config(&proxy_url, &master_url);
        config.cube_api_key = Some("secret-key".to_string());
        let app = spawn_app(config).await;

        // Wrong key → 401 before upgrade.
        let request =
            tungstenite::ClientRequestBuilder::new(ws_url(&app, "").parse().expect("uri"))
                .with_header("X-API-Key", "wrong-key");
        expect_upgrade_error(connect_async(request).await, StatusCode::UNAUTHORIZED);

        // Matching key → upgrade succeeds with no token param at all.
        let request =
            tungstenite::ClientRequestBuilder::new(ws_url(&app, "").parse().expect("uri"))
                .with_header("X-API-Key", "secret-key");
        let (mut ws, _resp) = connect_async(request)
            .await
            .expect("x-api-key handshake should succeed");
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));
    }

    // ── Session tracker caps ──────────────────────────────────────────────

    #[tokio::test(flavor = "multi_thread")]
    async fn session_tracker_enforces_per_sandbox_and_global_caps() {
        let tracker = TerminalSessionTracker::default();

        // Per-sandbox cap: max 1 for sb-a, global has room.
        let a1 = tracker
            .acquire("sb-a", 1, 10)
            .await
            .expect("first sb-a session");
        assert!(
            matches!(
                tracker.acquire("sb-a", 1, 10).await,
                Err(SessionCap::PerSandbox)
            ),
            "second sb-a session must hit the per-sandbox cap"
        );
        drop(a1);
        let _a1 = tracker
            .acquire("sb-a", 1, 10)
            .await
            .expect("per-sandbox slot released on drop");

        // Global cap: with one live session and max_global = 2, one more
        // session fits anywhere; the next is rejected even on a fresh
        // sandbox.
        let _b1 = tracker
            .acquire("sb-b", 5, 2)
            .await
            .expect("second global slot");
        assert!(
            matches!(tracker.acquire("sb-c", 5, 2).await, Err(SessionCap::Global)),
            "fresh sandbox must still hit the global cap"
        );
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn global_session_cap_returns_429() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let mut config = test_config(&proxy_url, &master_url);
        // Per-sandbox cap stays at the default 8; the global cap of 1 is what
        // rejects the second handshake.
        config.terminal_max_sessions_global = 1;
        let app = spawn_app(config).await;

        let (mut ws, _resp) = connect_async(ws_url(&app, ""))
            .await
            .expect("first session should upgrade");
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));

        expect_upgrade_error(
            connect_async(ws_url(&app, "")).await,
            StatusCode::TOO_MANY_REQUESTS,
        );
    }

    // ── Orphan shell reaping via envd tag reconnect ───────────────────────

    /// envd accepts the Start request (the shell may already be running) but
    /// never responds before ENVD_CALL_TIMEOUT: the session must fail and the
    /// recovery path must reconnect by tag to learn the pid and SIGKILL it.
    #[tokio::test(flavor = "multi_thread")]
    async fn hung_start_reaps_orphaned_shell_via_tag() {
        let hanging = MockEnvd::default();
        hanging
            .hang_start
            .store(true, std::sync::atomic::Ordering::Relaxed);
        let (proxy_url, mock) = spawn_mock_envd_with(hanging).await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let app = spawn_app(test_config(&proxy_url, &master_url)).await;

        let (mut ws, _resp) = connect_async(ws_url(&app, ""))
            .await
            .expect("websocket upgrade should succeed");

        // The Start request deadline (ENVD_CALL_TIMEOUT) must fire and the
        // client must see an error frame instead of hanging forever.
        let msg = tokio::time::timeout(ENVD_CALL_TIMEOUT + Duration::from_secs(15), ws.next())
            .await
            .expect("session must fail after the envd start deadline")
            .expect("ws stream ended unexpectedly")
            .expect("ws read error");
        let tungstenite::Message::Text(text) = msg else {
            panic!("expected text message, got {:?}", msg);
        };
        let error: Value = serde_json::from_str(&text).expect("message JSON");
        assert_eq!(error["type"], json!("error"));

        // The orphan cleanup reconnects with the same tag the Start request
        // carried and kills the recovered pid.
        let start = mock
            .spy
            .lock()
            .await
            .start_payload
            .clone()
            .expect("start payload recorded");
        let connect = wait_for_connect_with_tag(
            &mock.spy,
            start["tag"].as_str().expect("start payload carries a tag"),
        )
        .await;
        assert_eq!(connect["process"]["tag"], start["tag"]);

        let signal =
            wait_for_recorded(first_of(&mock.spy, |s| s.send_signal.first().cloned())).await;
        assert_eq!(
            signal,
            json!({"process": {"pid": MOCK_PID}, "signal": "SIGNAL_SIGKILL"})
        );
    }

    /// The Start stream opens but ends before the start event: same orphan
    /// window, same tag-based reaping, without the 10 s start deadline.
    #[tokio::test(flavor = "multi_thread")]
    async fn truncated_start_stream_reaps_orphaned_shell_via_tag() {
        let truncating = MockEnvd::default();
        truncating
            .close_stream_without_start
            .store(true, std::sync::atomic::Ordering::Relaxed);
        let (proxy_url, mock) = spawn_mock_envd_with(truncating).await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let app = spawn_app(test_config(&proxy_url, &master_url)).await;

        let (mut ws, _resp) = connect_async(ws_url(&app, ""))
            .await
            .expect("websocket upgrade should succeed");
        let error = recv_json(&mut ws).await;
        assert_eq!(error["type"], json!("error"));

        let start = mock
            .spy
            .lock()
            .await
            .start_payload
            .clone()
            .expect("start payload recorded");
        let connect = wait_for_connect_with_tag(
            &mock.spy,
            start["tag"].as_str().expect("start payload carries a tag"),
        )
        .await;
        assert_eq!(connect["process"]["tag"], start["tag"]);

        let signal =
            wait_for_recorded(first_of(&mock.spy, |s| s.send_signal.first().cloned())).await;
        assert_eq!(
            signal,
            json!({"process": {"pid": MOCK_PID}, "signal": "SIGNAL_SIGKILL"})
        );
    }

    // ── envd frame cap and output chunking ────────────────────────────────

    /// A single envd frame carrying more PTY output than one client message
    /// may hold is re-chunked: every output message stays within the 64 KiB
    /// cap and decodes independently; concatenated they equal the original.
    #[tokio::test(flavor = "multi_thread")]
    async fn large_pty_output_is_chunked_within_message_cap() {
        let (proxy_url, mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let app = spawn_app(test_config(&proxy_url, &master_url)).await;

        let (mut ws, _resp) = connect_async(ws_url(&app, ""))
            .await
            .expect("websocket upgrade should succeed");
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));

        let raw: Vec<u8> = (0..OUTPUT_CHUNK_SIZE + 1000)
            .map(|i| (i % 251) as u8)
            .collect();
        mock.push_frame(json!({"event": {"data": {"pty": BASE64.encode(&raw)}}}))
            .await;

        let first = recv_json(&mut ws).await;
        let second = recv_json(&mut ws).await;
        assert_eq!(first["type"], json!("output"));
        assert_eq!(second["type"], json!("output"));
        let d1 = first["data"].as_str().expect("chunk 1 data");
        let d2 = second["data"].as_str().expect("chunk 2 data");
        assert!(
            d1.len() <= MAX_WS_MESSAGE_SIZE && d2.len() <= MAX_WS_MESSAGE_SIZE,
            "every output message must stay within the 64 KiB cap"
        );
        let decoded = [BASE64.decode(d1).unwrap(), BASE64.decode(d2).unwrap()].concat();
        assert_eq!(decoded, raw);
    }

    /// An envd frame header claiming more than the 4 MiB cap is a stream
    /// error: the session ends through the normal teardown path (error frame
    /// to the client, SIGKILL to the shell) instead of buffering unboundedly.
    #[tokio::test(flavor = "multi_thread")]
    async fn oversized_envd_frame_terminates_session() {
        let (proxy_url, mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let app = spawn_app(test_config(&proxy_url, &master_url)).await;

        let (mut ws, _resp) = connect_async(ws_url(&app, ""))
            .await
            .expect("websocket upgrade should succeed");
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));

        // Only the 5-byte envelope header arrives, claiming a frame larger
        // than MAX_ENVD_FRAME_SIZE; the reader must reject it on sight.
        let mut header = vec![0u8];
        header.extend_from_slice(&((MAX_ENVD_FRAME_SIZE as u32) + 1).to_be_bytes());
        mock.push_raw(header).await;

        let error = recv_json(&mut ws).await;
        assert_eq!(error["type"], json!("error"));

        let signal =
            wait_for_recorded(first_of(&mock.spy, |s| s.send_signal.first().cloned())).await;
        assert_eq!(signal["signal"], json!("SIGNAL_SIGKILL"));
    }

    // ── Handshake credential edge cases ───────────────────────────────────

    /// A bare `cube-terminal.` subprotocol carries no token; the handshake
    /// must fall back to the `token` query param instead of treating the
    /// empty string as the credential.
    #[tokio::test(flavor = "multi_thread")]
    async fn empty_token_subprotocol_falls_back_to_query_token() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let (callback_url, captured) = spawn_auth_callback(StatusCode::OK).await;
        let mut config = test_config(&proxy_url, &master_url);
        config.auth_callback_url = Some(callback_url);
        let app = spawn_app(config).await;

        let (head, stream) =
            raw_handshake(&app, "?token=good-token", "cube-terminal, cube-terminal.").await;
        assert!(
            head.starts_with("HTTP/1.1 101"),
            "expected 101 Switching Protocols, got: {}",
            head
        );
        let mut ws = tokio_tungstenite::WebSocketStream::from_raw_socket(
            stream,
            tungstenite::protocol::Role::Client,
            None,
        )
        .await;
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));

        // The callback received the query token, not the empty subprotocol
        // token.
        let guard = captured.lock().await;
        let authz = guard
            .iter()
            .find(|(k, _)| k == "authorization")
            .map(|(_, v)| v.clone());
        assert_eq!(authz.as_deref(), Some("Bearer good-token"));
    }

    /// A query token carrying control characters (percent-decoded newline)
    /// cannot be forwarded as a header value; it must be rejected with 401
    /// instead of surfacing as a 500 "Auth callback unreachable".
    #[tokio::test(flavor = "multi_thread")]
    async fn control_char_query_token_is_rejected_with_401() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let (callback_url, captured) = spawn_auth_callback(StatusCode::OK).await;
        let mut config = test_config(&proxy_url, &master_url);
        config.auth_callback_url = Some(callback_url);
        let app = spawn_app(config).await;

        expect_upgrade_error(
            connect_async(ws_url(&app, "?token=bad%0Atoken")).await,
            StatusCode::UNAUTHORIZED,
        );
        // The callback must never see the malformed credential.
        assert!(captured.lock().await.is_empty());
    }

    /// A hung auth callback must not park the WebSocket handshake forever:
    /// AUTH_CALLBACK_TIMEOUT bounds it and the handshake fails with a 500.
    #[tokio::test(flavor = "multi_thread")]
    async fn hanging_auth_callback_times_out_handshake() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let callback_url = spawn_hanging_auth_callback().await;
        let mut config = test_config(&proxy_url, &master_url);
        config.auth_callback_url = Some(callback_url);
        let app = spawn_app(config).await;

        expect_upgrade_error(
            connect_async(ws_url(&app, "?token=good-token")).await,
            StatusCode::INTERNAL_SERVER_ERROR,
        );
    }

    // ── Rate limiting ─────────────────────────────────────────────────────

    /// Terminal handshakes are rate-limited per client IP, not per presented
    /// token: forged or rotated tokens from one client share a single bucket
    /// and cannot mint fresh quota (which would also grow the limiter map
    /// without bound).
    #[tokio::test(flavor = "multi_thread")]
    async fn rate_limit_buckets_terminal_handshakes_per_ip() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let mut config = test_config(&proxy_url, &master_url);
        config.cube_api_key = Some("secret-key".to_string());
        config.rate_limit_per_sec = 1;
        let app = spawn_app(config).await;

        // First forged token: passes the rate limit, rejected by auth (401).
        expect_upgrade_error(
            connect_async(ws_url(&app, "?token=wrong-1")).await,
            StatusCode::UNAUTHORIZED,
        );
        // A different forged token from the same IP shares the bucket: 429.
        expect_upgrade_error(
            connect_async(ws_url(&app, "?token=wrong-2")).await,
            StatusCode::TOO_MANY_REQUESTS,
        );
    }

    // ── Secure-by-default handshake policies ──────────────────────────────

    /// Without any auth backend the terminal endpoint fails closed: the
    /// handshake is rejected with 403 unless the operator explicitly opts
    /// into unauthenticated access.
    #[tokio::test(flavor = "multi_thread")]
    async fn open_mode_terminal_is_rejected_by_default() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let mut config = test_config(&proxy_url, &master_url);
        // No auth backend and no explicit opt-in — the secure default.
        config.terminal_allow_unauthenticated = false;
        let app = spawn_app(config).await;

        expect_upgrade_error(connect_async(ws_url(&app, "")).await, StatusCode::FORBIDDEN);
    }

    /// With the explicit opt-in the open-mode terminal works as before
    /// (covered by the rest of the suite via `test_config`, restated here
    /// against the same default-reject setup).
    #[tokio::test(flavor = "multi_thread")]
    async fn open_mode_terminal_allowed_when_explicitly_enabled() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let app = spawn_app(test_config(&proxy_url, &master_url)).await;

        let (mut ws, _resp) = connect_async(ws_url(&app, ""))
            .await
            .expect("explicitly enabled open-mode terminal should upgrade");
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));
    }

    /// The `?token=` query parameter is ignored when
    /// `terminal_token_query_param` is off (the secure default): the
    /// handshake authenticates as if the parameter were absent, and the
    /// `Authorization: Bearer` header becomes the non-browser transport.
    #[tokio::test(flavor = "multi_thread")]
    async fn query_token_param_is_ignored_when_disabled() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let (callback_url, captured) = spawn_auth_callback(StatusCode::OK).await;
        let mut config = test_config(&proxy_url, &master_url);
        config.auth_callback_url = Some(callback_url);
        config.terminal_token_query_param = false;
        let app = spawn_app(config).await;

        // The query token alone no longer authenticates: 401, and the
        // callback must never see it.
        expect_upgrade_error(
            connect_async(ws_url(&app, "?token=good-token")).await,
            StatusCode::UNAUTHORIZED,
        );
        assert!(captured.lock().await.is_empty());

        // The same token via the Authorization header upgrades.
        let request =
            tungstenite::ClientRequestBuilder::new(ws_url(&app, "").parse().expect("uri"))
                .with_header("Authorization", "Bearer good-token");
        let (mut ws, _resp) = connect_async(request)
            .await
            .expect("authorization-header handshake should succeed");
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));

        // …and is forwarded to the callback as Bearer.
        let guard = captured.lock().await;
        let authz = guard
            .iter()
            .find(|(k, _)| k == "authorization")
            .map(|(_, v)| v.clone());
        assert_eq!(authz.as_deref(), Some("Bearer good-token"));
    }

    /// Credential priority: the subprotocol token beats the Authorization
    /// header, which beats the (enabled) query param.
    #[tokio::test(flavor = "multi_thread")]
    async fn credential_priority_subprotocol_then_authorization_then_query() {
        let (proxy_url, _mock) = spawn_mock_envd().await;
        let master_url = spawn_mock_master(STATUS_RUNNING).await;
        let (callback_url, captured) = spawn_auth_callback(StatusCode::OK).await;
        let mut config = test_config(&proxy_url, &master_url);
        config.auth_callback_url = Some(callback_url);
        let app = spawn_app(config).await;

        async fn last_forwarded_bearer(captured: &CapturedHeaders) -> Option<String> {
            captured
                .lock()
                .await
                .iter()
                .rev()
                .find(|(k, _)| k == "authorization")
                .map(|(_, v)| v.clone())
        }

        // Authorization header + query token: the header wins.
        let request = tungstenite::ClientRequestBuilder::new(
            ws_url(&app, "?token=query-token").parse().expect("uri"),
        )
        .with_header("Authorization", "Bearer header-token");
        let (mut ws, _resp) = connect_async(request)
            .await
            .expect("handshake should succeed");
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));
        ws.close(None).await.expect("close websocket");
        assert_eq!(
            last_forwarded_bearer(&captured).await.as_deref(),
            Some("Bearer header-token")
        );

        // Subprotocol + Authorization header: the subprotocol wins.
        let request =
            tungstenite::ClientRequestBuilder::new(ws_url(&app, "").parse().expect("uri"))
                .with_header("Authorization", "Bearer header-token")
                .with_sub_protocol("cube-terminal")
                .with_sub_protocol("cube-terminal.subprotocol-token");
        let (mut ws, _resp) = connect_async(request)
            .await
            .expect("handshake should succeed");
        let ready = recv_json(&mut ws).await;
        assert_eq!(ready["type"], json!("ready"));
        ws.close(None).await.expect("close websocket");
        assert_eq!(
            last_forwarded_bearer(&captured).await.as_deref(),
            Some("Bearer subprotocol-token")
        );
    }
}

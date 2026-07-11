// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::{
    handlers::terminal::{DEFAULT_COLS, DEFAULT_ROWS},
    logging::{ArcLogger, LogEvent, LogLevel},
    middleware::auth::{get_or_init_auth_context, AuthContext},
    state::AppState,
};
use axum::{
    extract::{ConnectInfo, Path, Request, State},
    http::{HeaderMap, StatusCode},
    middleware::Next,
    response::Response,
};
use std::{net::SocketAddr, time::Instant};

/// Terminal WebSocket access audit middleware.
///
/// Runs as the outermost layer on the terminal route so it can observe every
/// rejection (auth, rate limit, state validation, backend error) as well as the
/// successful 101 upgrade.  Successful upgrades do not emit an event here;
/// `handlers::terminal` emits `terminal.connect` once the envd PTY is actually
/// established, and `terminal.disconnect` when the socket closes.
pub async fn terminal_audit(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    connect_info: Option<ConnectInfo<SocketAddr>>,
    mut request: Request,
    next: Next,
) -> Result<Response, crate::error::AppError> {
    let remote_ip = extract_remote_ip(request.headers(), connect_info.map(|c| c.0));
    let (cols, rows) = terminal_query_dimensions(request.uri().query());
    let container = terminal_query_container(request.uri().query());

    // Initialise a shared auth context before handing the request down; inner
    // middleware will update it with the credential type and, on success, user
    // identity.  We keep a clone so we can read the final state after the
    // response has been produced.
    let auth_ctx = get_or_init_auth_context(&mut request);

    let start = Instant::now();
    let response = next.run(request).await;
    let elapsed = start.elapsed();

    let final_ctx = auth_ctx.read().await.clone();

    let status = response.status();
    if status != StatusCode::SWITCHING_PROTOCOLS {
        let reason = connect_denied_reason(status);
        let mut extra = vec![
            ("http_status", (status.as_u16() as u64).into()),
            ("reason", reason.into()),
            ("duration_ms", (elapsed.as_millis() as u64).into()),
        ];
        if let Some(container_id) = container {
            extra.push(("container", container_id.into()));
        }
        log_terminal_event(
            &state.logger,
            "terminal.connect_denied",
            &sandbox_id,
            &remote_ip,
            &final_ctx,
            cols,
            rows,
            extra,
        )
        .await;
    }

    Ok(response)
}

fn terminal_query_dimensions(query: Option<&str>) -> (u16, u16) {
    let mut cols = DEFAULT_COLS;
    let mut rows = DEFAULT_ROWS;

    if let Some(query) = query {
        for (key, value) in url::form_urlencoded::parse(query.as_bytes()) {
            match key.as_ref() {
                "cols" => {
                    if let Ok(parsed) = value.parse::<u16>() {
                        cols = parsed;
                    }
                }
                "rows" => {
                    if let Ok(parsed) = value.parse::<u16>() {
                        rows = parsed;
                    }
                }
                _ => {}
            }
        }
    }

    (cols, rows)
}

fn terminal_query_container(query: Option<&str>) -> Option<String> {
    query.and_then(|q| {
        url::form_urlencoded::parse(q.as_bytes())
            .find(|(key, _)| key == "container")
            .map(|(_, value)| value.to_string())
            .filter(|v| !v.is_empty())
    })
}

fn connect_denied_reason(status: StatusCode) -> &'static str {
    match status {
        StatusCode::UNAUTHORIZED => "auth_required",
        StatusCode::FORBIDDEN => "forbidden",
        StatusCode::NOT_FOUND => "not_found",
        StatusCode::CONFLICT => "not_running",
        StatusCode::TOO_MANY_REQUESTS => "rate_limited",
        StatusCode::INTERNAL_SERVER_ERROR => "server_error",
        StatusCode::BAD_GATEWAY | StatusCode::SERVICE_UNAVAILABLE | StatusCode::GATEWAY_TIMEOUT => {
            "backend_unavailable"
        }
        _ => "unknown",
    }
}

/// Compute the best-effort remote client IP.
///
/// Honours `X-Forwarded-For` / `X-Real-IP` first so that deployments behind a
/// reverse proxy log the original client.  Falls back to the TCP peer address
/// from `ConnectInfo`, and finally `"unknown"`.
pub(crate) fn extract_remote_ip(headers: &HeaderMap, connect_addr: Option<SocketAddr>) -> String {
    if let Some(v) = headers.get("X-Forwarded-For").and_then(|h| h.to_str().ok()) {
        if let Some(ip) = v.split(',').next().map(str::trim).filter(|s| !s.is_empty()) {
            return ip.to_string();
        }
    }
    if let Some(v) = headers
        .get("X-Real-IP")
        .and_then(|h| h.to_str().ok())
        .map(str::trim)
    {
        if !v.is_empty() {
            return v.to_string();
        }
    }
    connect_addr
        .map(|a| a.ip().to_string())
        .unwrap_or_else(|| "unknown".to_string())
}

/// Emit a structured terminal audit event.
pub(crate) async fn log_terminal_event(
    logger: &ArcLogger,
    event: &str,
    sandbox_id: &str,
    remote_ip: &str,
    auth_ctx: &AuthContext,
    cols: u16,
    rows: u16,
    extra: Vec<(&str, serde_json::Value)>,
) {
    let mut ev = LogEvent::new(LogLevel::Info, event)
        .field("sandbox_id", sandbox_id)
        .field("remote_ip", remote_ip)
        .field("auth_type", &auth_ctx.auth_type)
        .field("cols", cols.to_string())
        .field("rows", rows.to_string());

    if let Some(user_id) = &auth_ctx.user_id {
        ev = ev.field("user_id", user_id);
    }
    if let Some(user_name) = &auth_ctx.user_name {
        ev = ev.field("user_name", user_name);
    }

    for (k, v) in extra {
        if let Ok(serialised) = serde_json::to_value(v) {
            ev.fields.insert(k.to_string(), serialised);
        }
    }

    logger.log(ev).await;
}

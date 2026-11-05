// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use super::rest::{file_error, AppState};
use axum::{
    body::Body,
    extract::State,
    middleware::Next,
    response::{IntoResponse, Response},
};
use http::{Request, StatusCode};
use std::sync::Arc;
use subtle::ConstantTimeEq;

pub(super) async fn authorize(
    State(state): State<Arc<AppState>>,
    request: Request<Body>,
    next: Next,
) -> Response {
    let excluded = matches!(
        (request.method().as_str(), request.uri().path()),
        ("GET", "/health" | "/files") | ("POST", "/files" | "/init")
    );
    {
        let token = state.runtime.access_token.read().await;
        if let Some(token) = token.as_ref() {
            let provided = request
                .headers()
                .get("x-access-token")
                .map(|v| v.as_bytes())
                .unwrap_or_default();
            if !excluded && !bool::from(token.as_bytes().ct_eq(provided)) {
                return file_error(StatusCode::UNAUTHORIZED, "unauthorized access, please provide a valid access token or method signing if supported");
            }
        }
    }
    if let Some(bytes) = crate::runtime::basic_username(request.headers()) {
        let valid = match std::str::from_utf8(&bytes) {
            Ok(username) => state.users.validate_username(username).await.is_ok(),
            Err(_) => false,
        };
        if !valid {
            return username_error(request.headers(), &String::from_utf8_lossy(&bytes));
        }
    }
    next.run(request).await
}

pub(super) async fn signed_file(
    state: &AppState,
    headers: &http::HeaderMap,
    method: &http::Method,
    query: &[(String, String)],
) -> Result<(), Response> {
    use base64::Engine;
    use sha2::{Digest, Sha256};
    let value = |name: &str| {
        query
            .iter()
            .find(|(key, _)| key == name)
            .map(|(_, value)| value.as_str())
    };
    let expiration = value("signature_expiration").map(|raw| {
        raw.parse::<i64>().map_err(|_| {
            let reason = if raw.trim_start_matches(['+', '-']).bytes().all(|b| b.is_ascii_digit()) && !raw.trim_start_matches(['+', '-']).is_empty() { "value out of range" } else { "invalid syntax" };
            (StatusCode::BAD_REQUEST, [("x-content-type-options", "nosniff")], format!("Invalid format for parameter signature_expiration: error binding string parameter: strconv.ParseInt: parsing {raw:?}: {reason}\n")).into_response()
        })
    }).transpose()?;
    let token = state.runtime.access_token.read().await;
    let Some(token) = token.as_ref() else {
        return Ok(());
    };
    let provided = headers
        .get("x-access-token")
        .map(|v| v.as_bytes())
        .unwrap_or_default();
    if !provided.is_empty() {
        return if bool::from(token.as_bytes().ct_eq(provided)) {
            Ok(())
        } else {
            Err(file_error(
                StatusCode::UNAUTHORIZED,
                "access token present in header but does not match",
            ))
        };
    }
    let signature = value("signature").ok_or_else(|| {
        file_error(
            StatusCode::UNAUTHORIZED,
            "missing signature query parameter",
        )
    })?;
    let operation = if method == http::Method::GET {
        "read"
    } else {
        "write"
    };
    let mut input = zeroize::Zeroizing::new(format!(
        "{}:{operation}:{}:{}",
        value("path").unwrap_or_default(),
        value("username").unwrap_or_default(),
        token.as_str()
    ));
    if let Some(expiration) = expiration {
        use std::fmt::Write;
        write!(input, ":{expiration}").expect("signature input");
    }
    let expected = format!(
        "v1_{}",
        base64::engine::general_purpose::STANDARD_NO_PAD.encode(Sha256::digest(input.as_bytes()))
    );
    if !bool::from(expected.as_bytes().ct_eq(signature.as_bytes())) {
        return Err(file_error(StatusCode::UNAUTHORIZED, "invalid signature"));
    }
    let now = jiff::Timestamp::now().as_second();
    if expiration.is_some_and(|expiration| expiration < now) {
        return Err(file_error(
            StatusCode::UNAUTHORIZED,
            "signature is already expired",
        ));
    }
    Ok(())
}

fn username_error(headers: &http::HeaderMap, username: &str) -> Response {
    let message = format!("invalid username: '{username}'");
    let content_type = headers
        .get("content-type")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .split(';')
        .next()
        .unwrap_or("")
        .trim();
    if matches!(
        content_type,
        "application/connect+json" | "application/connect+proto"
    ) {
        let body = serde_json::to_vec(
            &serde_json::json!({"error":{"code":"unauthenticated","message":message}}),
        )
        .expect("auth end stream JSON");
        let mut frame = vec![2];
        frame.extend_from_slice(&(body.len() as u32).to_be_bytes());
        frame.extend_from_slice(&body);
        return ([("content-type", content_type.to_owned())], frame).into_response();
    }
    if matches!(
        content_type,
        "application/grpc"
            | "application/grpc+proto"
            | "application/grpc+json"
            | "application/grpc-web"
            | "application/grpc-web+proto"
            | "application/grpc-web+json"
    ) {
        return (
            [
                ("content-type", content_type.to_owned()),
                ("grpc-status", "16".into()),
                ("grpc-message", message),
            ],
            Body::empty(),
        )
            .into_response();
    }
    (
        StatusCode::UNAUTHORIZED,
        [("content-type", "application/json")],
        serde_json::to_string(&serde_json::json!({"code":"unauthenticated","message":message}))
            .expect("auth error JSON"),
    )
        .into_response()
}

// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use super::rest::{auth_error, AppState};
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
        ("GET", "/health") | ("POST", "/init")
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
                return auth_error(StatusCode::UNAUTHORIZED, "unauthorized access, please provide a valid access token or method signing if supported");
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

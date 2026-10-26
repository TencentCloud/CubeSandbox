// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::{error::AppError, state::AppState};
use axum::{
    extract::{Request, State},
    middleware::Next,
    response::Response,
};

/// Auth credential extracted from the request headers.
#[derive(Debug)]
enum AuthCredential {
    /// `Authorization: Bearer <token>`
    Bearer(String),
    /// `X-API-Key: <key>`
    ApiKey(String),
}

/// Extract the auth credential from request headers (Bearer takes priority over X-API-Key).
fn extract_credential(request: &Request) -> Option<AuthCredential> {
    let headers = request.headers();

    // Prefer Authorization: Bearer
    if let Some(auth_val) = headers.get("Authorization") {
        if let Ok(auth_str) = auth_val.to_str() {
            if let Some(token) = auth_str.strip_prefix("Bearer ") {
                let token = token.trim().to_string();
                if !token.is_empty() {
                    return Some(AuthCredential::Bearer(token));
                }
            }
        }
    }

    // Fall back to X-API-Key
    if let Some(key_val) = headers.get("X-API-Key") {
        if let Ok(key_str) = key_val.to_str() {
            let key = key_str.trim().to_string();
            if !key.is_empty() {
                return Some(AuthCredential::ApiKey(key));
            }
        }
    }

    None
}

/// Unified auth middleware.
///
/// Behavior (priority order):
///
/// 1. **Callback mode** (`auth_callback_url` set):
///    - Extract a Bearer token or X-API-Key from the request headers (Bearer takes priority).
///    - Forward a POST to the callback URL with:
///      - `Authorization: Bearer <token>`  (when the client used Bearer auth)
///      - `X-API-Key: <key>`              (when the client used API Key auth)
///      - `X-Request-Path: <original path>` (the path the client requested)
///      - `X-Request-Method: <HTTP method>` (the HTTP method the client used, e.g. GET/DELETE)
///    - HTTP 200 from callback → allow; any other status → 401 Unauthorized.
///
/// 2. **Simple key mode** (`cube_api_key` set, callback unset):
///    - Extract a Bearer token or X-API-Key from the request headers (Bearer takes priority).
///    - Compare the extracted credential string against `cube_api_key`.
///    - Match → allow; mismatch or missing → 401 Unauthorized.
///
/// 3. **No auth** (both unset): all requests are allowed through.
///
/// The two credential headers are mutually exclusive; the callback receives whichever
/// one the client sent. No extra type discriminator is needed.
///
/// # Security note
///
/// Multiple HTTP methods (e.g. GET/POST/DELETE/PATCH) are mounted on the same path
/// (e.g. `/templates/:id`). A callback that only whitelists by path cannot distinguish
/// a read from a write or delete operation. Forwarding `X-Request-Method` allows the
/// callback to enforce fine-grained (path + method) authorization.
pub async fn unified_auth(
    State(state): State<AppState>,
    request: Request,
    next: Next,
) -> Result<Response, AppError> {
    // Mode 1: callback auth — if a callback URL is configured, forward the
    // credential and let the external system decide.
    let callback_url = match state.config.auth_callback_url.as_deref() {
        Some(url) if !url.is_empty() => url.to_string(),
        _ => String::new(),
    };

    if callback_url.is_empty() {
        // Mode 2: simple key auth — if CUBE_API_KEY is configured, compare
        // the extracted credential against it locally.
        if let Some(expected_key) = state.config.cube_api_key.as_deref() {
            if !expected_key.is_empty() {
                let credential = extract_credential(&request).ok_or_else(|| {
                    AppError::Unauthorized(
                        "Missing authentication: provide 'Authorization: Bearer <token>' or 'X-API-Key: <key>'"
                            .to_string(),
                    )
                })?;
                let provided = match &credential {
                    AuthCredential::Bearer(t) => t.as_str(),
                    AuthCredential::ApiKey(k) => k.as_str(),
                };
                if provided != expected_key {
                    tracing::warn!(
                        path = %request.uri().path(),
                        method = %request.method(),
                        "simple key auth: credential mismatch"
                    );
                    return Err(AppError::Unauthorized(
                        "Invalid API key or token".to_string(),
                    ));
                }
            }
        }
        // Mode 3: no auth (both unset) or simple-key match — pass through.
        return Ok(next.run(request).await);
    }

    // ── Callback mode continuation ──────────────────────────────────────

    // Capture the request path and HTTP method to forward to the callback.
    let request_path = request.uri().path().to_string();
    let request_method = request.method().to_string();

    // Require a credential when a callback is configured.
    let credential = extract_credential(&request).ok_or_else(|| {
        AppError::Unauthorized(
            "Missing authentication: provide 'Authorization: Bearer <token>' or 'X-API-Key: <key>'"
                .to_string(),
        )
    })?;

    // Build the callback POST, forwarding the credential headers, request path, and HTTP method.
    // X-Request-Method is required for correct authz: the same path (e.g. /templates/:id)
    // serves GET/POST/DELETE/PATCH, so path alone is insufficient to distinguish read vs write.
    let req_builder = state
        .http_client
        .post(&callback_url)
        .header("X-Request-Path", &request_path)
        .header("X-Request-Method", &request_method);

    let req_builder = match &credential {
        AuthCredential::Bearer(token) => {
            req_builder.header("Authorization", format!("Bearer {}", token))
        }
        AuthCredential::ApiKey(key) => req_builder.header("X-API-Key", key.as_str()),
    };

    let callback_resp = req_builder.send().await.map_err(|e| {
        tracing::error!(error = %e, callback_url = %callback_url, "auth callback request failed");
        AppError::Internal(anyhow::anyhow!("Auth callback unreachable: {}", e))
    })?;

    let auth_type = match &credential {
        AuthCredential::Bearer(_) => "bearer",
        AuthCredential::ApiKey(_) => "api_key",
    };

    if callback_resp.status().as_u16() == 200 {
        tracing::debug!(
            path = %request_path,
            method = %request_method,
            auth_type = auth_type,
            "auth callback approved"
        );
        Ok(next.run(request).await)
    } else {
        tracing::warn!(
            status = %callback_resp.status(),
            path = %request_path,
            method = %request_method,
            auth_type = auth_type,
            "auth callback rejected request"
        );
        Err(AppError::Unauthorized(
            "Authentication rejected by callback".to_string(),
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        config::ServerConfig,
        logging::{arc, noop::NoopLogger},
        state::AppState,
    };
    use axum::{
        body::Body,
        http::{Method, StatusCode},
        routing::any,
        Router,
    };
    use axum_test::TestServer;
    use std::sync::Arc;
    use tokio::net::TcpListener;

    /// Spawn a callback server that responds with `respond_status` and records
    /// all received request headers into `captured_headers`.
    async fn spawn_callback_server(
        respond_status: StatusCode,
        captured_headers: Arc<tokio::sync::Mutex<Vec<(String, String)>>>,
    ) -> String {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();

        let app = Router::new().route(
            "/auth",
            any(move |req: axum::http::Request<Body>| {
                let headers = captured_headers.clone();
                async move {
                    let mut guard = headers.lock().await;
                    for (k, v) in req.headers() {
                        guard.push((k.to_string(), v.to_str().unwrap_or("").to_string()));
                    }
                    axum::http::Response::builder()
                        .status(respond_status)
                        .body(Body::empty())
                        .unwrap()
                }
            }),
        );

        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });

        format!("http://{}/auth", addr)
    }

    async fn build_test_server_with_callback(callback_url: &str) -> TestServer {
        let mut config = ServerConfig::default();
        config.auth_callback_url = Some(callback_url.to_string());
        let state = AppState::new(config, arc(NoopLogger)).await;
        let router = Router::new()
            .route("/templates/:id", any(|| async { "ok" }))
            .route("/sandboxes/:id", any(|| async { "ok" }))
            .layer(axum::middleware::from_fn_with_state(
                state.clone(),
                unified_auth,
            ))
            .with_state(state);
        TestServer::new(router).unwrap()
    }

    /// Core regression: GET and DELETE on the same path must produce distinct
    /// X-Request-Method values at the callback, preventing read-to-delete escalation.
    #[tokio::test]
    async fn callback_receives_distinct_method_for_same_path() {
        let captured = Arc::new(tokio::sync::Mutex::new(Vec::new()));
        let callback_url = spawn_callback_server(StatusCode::OK, captured.clone()).await;
        let server = build_test_server_with_callback(&callback_url).await;

        server
            .method(Method::GET, "/templates/demo")
            .add_header(
                axum::http::header::HeaderName::from_static("x-api-key"),
                axum::http::HeaderValue::from_static("test-key"),
            )
            .await
            .assert_status_ok();

        server
            .method(Method::DELETE, "/templates/demo")
            .add_header(
                axum::http::header::HeaderName::from_static("x-api-key"),
                axum::http::HeaderValue::from_static("test-key"),
            )
            .await
            .assert_status_ok();

        let guard = captured.lock().await;
        let methods: Vec<&str> = guard
            .iter()
            .filter(|(k, _)| k == "x-request-method")
            .map(|(_, v)| v.as_str())
            .collect();

        assert_eq!(
            methods,
            ["GET", "DELETE"],
            "callback must receive distinct X-Request-Method for GET vs DELETE on the same path"
        );
    }

    /// The callback must receive X-Request-Path.
    #[tokio::test]
    async fn callback_receives_request_path() {
        let captured = Arc::new(tokio::sync::Mutex::new(Vec::new()));
        let callback_url = spawn_callback_server(StatusCode::OK, captured.clone()).await;
        let server = build_test_server_with_callback(&callback_url).await;

        server
            .get("/templates/abc-123")
            .add_header(
                axum::http::header::HeaderName::from_static("x-api-key"),
                axum::http::HeaderValue::from_static("key"),
            )
            .await
            .assert_status_ok();

        let guard = captured.lock().await;
        let paths: Vec<&str> = guard
            .iter()
            .filter(|(k, _)| k == "x-request-path")
            .map(|(_, v)| v.as_str())
            .collect();
        assert!(paths.contains(&"/templates/abc-123"));
    }

    /// A non-200 callback response must produce 401 Unauthorized.
    #[tokio::test]
    async fn callback_rejection_returns_401() {
        let captured = Arc::new(tokio::sync::Mutex::new(Vec::new()));
        let callback_url = spawn_callback_server(StatusCode::FORBIDDEN, captured.clone()).await;
        let server = build_test_server_with_callback(&callback_url).await;

        server
            .delete("/templates/secret")
            .add_header(
                axum::http::header::HeaderName::from_static("x-api-key"),
                axum::http::HeaderValue::from_static("bad-key"),
            )
            .await
            .assert_status(StatusCode::UNAUTHORIZED);
    }

    /// When no callback is configured, requests without credentials must pass through.
    #[tokio::test]
    async fn no_callback_configured_passthrough() {
        let config = ServerConfig::default(); // auth_callback_url = None
        let state = AppState::new(config, arc(NoopLogger)).await;
        let router = Router::new()
            .route("/sandboxes/:id", any(|| async { "ok" }))
            .layer(axum::middleware::from_fn_with_state(
                state.clone(),
                unified_auth,
            ))
            .with_state(state);
        let server = TestServer::new(router).unwrap();

        server.get("/sandboxes/xyz").await.assert_status_ok();
    }

    /// When a callback is configured, a request without credentials must return 401.
    #[tokio::test]
    async fn missing_credential_returns_401() {
        let captured = Arc::new(tokio::sync::Mutex::new(Vec::new()));
        let callback_url = spawn_callback_server(StatusCode::OK, captured.clone()).await;
        let server = build_test_server_with_callback(&callback_url).await;

        server
            .get("/templates/any")
            .await
            .assert_status(StatusCode::UNAUTHORIZED);
    }

    /// POST and PATCH (write operations) must also forward the correct method to the callback.
    #[tokio::test]
    async fn callback_receives_correct_method_for_write_operations() {
        let captured = Arc::new(tokio::sync::Mutex::new(Vec::new()));
        let callback_url = spawn_callback_server(StatusCode::OK, captured.clone()).await;
        let server = build_test_server_with_callback(&callback_url).await;

        for method in [Method::POST, Method::PATCH] {
            server
                .method(method.clone(), "/templates/tmpl-01")
                .add_header(
                    axum::http::header::HeaderName::from_static("authorization"),
                    axum::http::HeaderValue::from_static("Bearer tok"),
                )
                .await
                .assert_status_ok();
        }

        let guard = captured.lock().await;
        let methods: Vec<&str> = guard
            .iter()
            .filter(|(k, _)| k == "x-request-method")
            .map(|(_, v)| v.as_str())
            .collect();
        assert!(methods.contains(&"POST"), "should see POST");
        assert!(methods.contains(&"PATCH"), "should see PATCH");
    }

    // ── Simple key mode (CUBE_API_KEY) ───────────────────────────────────

    async fn build_test_server_with_api_key(api_key: &str) -> TestServer {
        let mut config = ServerConfig::default();
        config.cube_api_key = Some(api_key.to_string());
        let state = AppState::new(config, arc(NoopLogger)).await;
        let router = Router::new()
            .route("/sandboxes/:id", any(|| async { "ok" }))
            .layer(axum::middleware::from_fn_with_state(
                state.clone(),
                unified_auth,
            ))
            .with_state(state);
        TestServer::new(router).unwrap()
    }

    /// CUBE_API_KEY mode: X-API-Key matching the configured key must pass.
    #[tokio::test]
    async fn simple_key_match_x_api_key() {
        let server = build_test_server_with_api_key("secret-123").await;
        server
            .get("/sandboxes/abc")
            .add_header(
                axum::http::header::HeaderName::from_static("x-api-key"),
                axum::http::HeaderValue::from_static("secret-123"),
            )
            .await
            .assert_status_ok();
    }

    /// CUBE_API_KEY mode: Authorization Bearer matching the configured key must pass.
    #[tokio::test]
    async fn simple_key_match_bearer() {
        let server = build_test_server_with_api_key("secret-123").await;
        server
            .get("/sandboxes/abc")
            .add_header(
                axum::http::header::HeaderName::from_static("authorization"),
                axum::http::HeaderValue::from_static("Bearer secret-123"),
            )
            .await
            .assert_status_ok();
    }

    /// CUBE_API_KEY mode: mismatched X-API-Key must return 401.
    #[tokio::test]
    async fn simple_key_mismatch() {
        let server = build_test_server_with_api_key("secret-123").await;
        server
            .get("/sandboxes/abc")
            .add_header(
                axum::http::header::HeaderName::from_static("x-api-key"),
                axum::http::HeaderValue::from_static("wrong-key"),
            )
            .await
            .assert_status(StatusCode::UNAUTHORIZED);
    }

    /// CUBE_API_KEY mode: missing credential must return 401.
    #[tokio::test]
    async fn simple_key_missing_credential() {
        let server = build_test_server_with_api_key("secret-123").await;
        server
            .get("/sandboxes/abc")
            .await
            .assert_status(StatusCode::UNAUTHORIZED);
    }

    /// CUBE_API_KEY mode: empty key string should behave as no-auth (passthrough).
    #[tokio::test]
    async fn simple_key_empty_passthrough() {
        let server = build_test_server_with_api_key("").await;
        server.get("/sandboxes/abc").await.assert_status_ok();
    }
}
